package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestDeviceFlowSendsUserAgent pins the RFC 8628 device flow (GitHub/Copilot):
// requests carry a real User-Agent so Cloudflare-fronted auth endpoints do
// not reject Go's default UA with a 403.
func TestDeviceFlowSendsUserAgent(t *testing.T) {
	var deviceUA string
	device := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		deviceUA = r.Header.Get("User-Agent")
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://example.com/device","interval":1,"expires_in":60}`))
	}))
	defer device.Close()

	var tokenUA string
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		tokenUA = r.Header.Get("User-Agent")
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"access_token":"tok-1","token_type":"Bearer","expires_in":3600}`))
	}))
	defer token.Close()

	flow := DeviceFlow{
		DeviceAuthURL: device.URL,
		TokenURL:      token.URL,
		ClientID:      "Iv1.b507a08c87ecfe98",
		Scope:         "read:user",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := flow.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if deviceUA == "" || deviceUA == "Go-http-client/1.1" {
		t.Errorf("device User-Agent = %q, want a real UA", deviceUA)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	tok, err := flow.Poll(ctx2, resp.DeviceCode, 1)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if tok.AccessToken != "tok-1" {
		t.Errorf("access token = %q, want tok-1", tok.AccessToken)
	}
	if tokenUA == "" || tokenUA == "Go-http-client/1.1" {
		t.Errorf("token User-Agent = %q, want a real UA", tokenUA)
	}
}

// TestChatGPTCatalogEntryUsesCodexFlow guards the registry entry: the
// ChatGPT / Codex entry must use the browser-login flow (localhost OAuth
// callback + PKCE), not the retired device-code endpoints that mint tokens
// the chatgpt.com backend rejects with "missing_end_user_auth".
func TestChatGPTCatalogEntryUsesCodexFlow(t *testing.T) {
	entry, ok := Get("chatgpt")
	if !ok {
		t.Fatal("chatgpt entry missing from registry")
	}
	if entry.OAuth == nil {
		t.Fatal("chatgpt entry has no OAuth flow")
	}
	flow, ok := entry.OAuth.(*CodexBrowserFlow)
	if !ok {
		t.Fatalf("chatgpt OAuth flow = %T, want *CodexBrowserFlow", entry.OAuth)
	}
	if flow.Issuer != "https://auth.openai.com" {
		t.Errorf("issuer = %q, want https://auth.openai.com", flow.Issuer)
	}
	if flow.ClientID == "" || flow.ClientID == "codex" {
		t.Errorf("client id = %q, want the registered codex public client", flow.ClientID)
	}
	// Must NOT point at the retired RFC 8628 device endpoint.
	if df, isRFC := entry.OAuth.(*DeviceFlow); isRFC && df.DeviceAuthURL != "" {
		t.Errorf("chatgpt still uses RFC 8628 device URL %q", df.DeviceAuthURL)
	}
}

// TestCodexDeviceFlowEndToEnd drives the full current Codex device protocol:
// usercode request, pending poll, authorized poll returning a PKCE code, and
// the /oauth/token exchange.
func TestCodexDeviceFlowEndToEnd(t *testing.T) {
	var mux http.ServeMux
	issuer := httptest.NewServer(&mux)
	defer issuer.Close()

	// /api/accounts/deviceauth/usercode
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ClientID string `json:"client_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ClientID != "app_EMoamEEZ73f0CkXaXp7hrann" {
			t.Errorf("usercode client_id = %q, want the codex client", req.ClientID)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"device_auth_id":"deviceauth_abc","user_code":"ABCD-1234","interval":"5"}`))
	})

	polled := 0
	// /api/accounts/deviceauth/token
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		polled++
		var req struct {
			DeviceAuthID string `json:"device_auth_id"`
			UserCode     string `json:"user_code"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.DeviceAuthID != "deviceauth_abc" || req.UserCode != "ABCD-1234" {
			t.Errorf("poll body = %+v", req)
		}
		w.Header().Set("content-type", "application/json")
		if polled < 2 {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"message":"Device authorization is pending. Please try again.","code":"deviceauth_authorization_pending"}}`))
			return
		}
		w.Write([]byte(`{"authorization_code":"authcode-1","code_challenge":"challenge","code_verifier":"verifier"}`))
	})

	// /oauth/token
	var tokenForm url.Values
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		tokenForm = r.PostForm
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id_token":"id-1","access_token":"tok-codex","refresh_token":"refresh-1"}`))
	})

	flow := CodexDeviceFlow{Issuer: issuer.URL, ClientID: "app_EMoamEEZ73f0CkXaXp7hrann"}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := flow.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if resp.UserCode != "ABCD-1234" {
		t.Errorf("user code = %q, want ABCD-1234", resp.UserCode)
	}
	if resp.VerificationURI == "" || !strings.Contains(resp.VerificationURI, "/codex/device") {
		t.Errorf("verification URI = %q, want .../codex/device", resp.VerificationURI)
	}
	if !strings.Contains(resp.DeviceCode, "\t") {
		t.Fatalf("DeviceCode %q must carry device_auth_id\\tuser_code", resp.DeviceCode)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	tok, err := flow.Poll(ctx2, resp.DeviceCode, 1)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if polled < 2 {
		t.Errorf("poll called %d times, want >= 2 (pending then authorized)", polled)
	}
	if tok.AccessToken != "tok-codex" {
		t.Errorf("access token = %q, want tok-codex", tok.AccessToken)
	}
	if tok.RefreshToken != "refresh-1" {
		t.Errorf("refresh token = %q, want refresh-1", tok.RefreshToken)
	}
	// The exchange must be the standard authorization-code grant with PKCE.
	if tokenForm.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q, want authorization_code", tokenForm.Get("grant_type"))
	}
	if tokenForm.Get("code") != "authcode-1" {
		t.Errorf("code = %q, want authcode-1", tokenForm.Get("code"))
	}
	if tokenForm.Get("code_verifier") != "verifier" {
		t.Errorf("code_verifier = %q, want verifier", tokenForm.Get("code_verifier"))
	}
	if !strings.Contains(tokenForm.Get("redirect_uri"), "/deviceauth/callback") {
		t.Errorf("redirect_uri = %q, want .../deviceauth/callback", tokenForm.Get("redirect_uri"))
	}
}

// TestCodexDeviceFlowRejectsBrokenDeviceCode guards Poll against a
// non-codex device code (e.g. one minted by the old RFC 8628 flow).
func TestCodexDeviceFlowRejectsBrokenDeviceCode(t *testing.T) {
	flow := CodexDeviceFlow{Issuer: "https://auth.openai.com"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := flow.Poll(ctx, "no-tab-here", 1); err == nil {
		t.Fatal("Poll accepted a malformed device code")
	}
}

// TestCodexDeviceFlowFailsFastOnExpired pins the terminal-error handling:
// OpenAI returns 403 for BOTH "still pending" and "expired" (and 404 for
// "not found"), distinguished only by the error code. The poll must stop
// immediately on a terminal code instead of looping for the full TUI
// timeout, which the user experienced as an infinite spinner.
func TestCodexDeviceFlowFailsFastOnExpired(t *testing.T) {
	var mux http.ServeMux
	issuer := httptest.NewServer(&mux)
	defer issuer.Close()

	// /oauth/token should never be reached.
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		t.Error("token exchange called for an expired device code")
	})

	pollCount := 0
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		pollCount++
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"message":"Device authorization code has expired. Please request a new code.","code":"deviceauth_user_code_expired"}}`))
	})

	flow := CodexDeviceFlow{Issuer: issuer.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := flow.Poll(ctx, "deviceauth_x\tCODE-1", 1)
	if err == nil {
		t.Fatal("Poll succeeded for an expired device code")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error = %q, want an expired-code message", err.Error())
	}
	if pollCount != 1 {
		t.Errorf("polled %d times, want exactly 1 (fail fast)", pollCount)
	}
}

// TestCodexDeviceFlowFailsFastOnNotFound pins the 404 + deviceauth_not_found
// terminal case the same way.
func TestCodexDeviceFlowFailsFastOnNotFound(t *testing.T) {
	var mux http.ServeMux
	issuer := httptest.NewServer(&mux)
	defer issuer.Close()

	pollCount := 0
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		pollCount++
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"Resource not found","code":"deviceauth_not_found"}}`))
	})

	flow := CodexDeviceFlow{Issuer: issuer.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := flow.Poll(ctx, "deviceauth_x\tCODE-1", 1)
	if err == nil {
		t.Fatal("Poll succeeded for a not-found device code")
	}
	if pollCount != 1 {
		t.Errorf("polled %d times, want exactly 1 (fail fast)", pollCount)
	}
}

// TestCodexBrowserFlowEndToEnd drives the browser-login flow: Start binds a
// localhost callback server and builds the authorize URL; simulating the
// browser hitting /auth/callback with a code + matching state; Poll then
// exchanges that code at /oauth/token.
func TestCodexBrowserFlowEndToEnd(t *testing.T) {
	var exchanged url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		exchanged = r.PostForm
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id_token":"id-b","access_token":"tok-browser","refresh_token":"refresh-b","expires_in":3600}`))
	})
	issuer := httptest.NewServer(mux)
	defer issuer.Close()

	flow := CodexBrowserFlow{Issuer: issuer.URL, ClientID: "app_EMoamEEZ73f0CkXaXp7hrann"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := flow.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.Contains(resp.VerificationURI, "/oauth/authorize?") {
		t.Fatalf("authorize URL = %q, want /oauth/authorize?...", resp.VerificationURI)
	}
	if !strings.HasPrefix(resp.DeviceCode, "port:") {
		t.Fatalf("DeviceCode = %q, want port:N marker", resp.DeviceCode)
	}
	port := strings.TrimPrefix(resp.DeviceCode, "port:")
	redirectURI := "http://localhost:" + port + "/auth/callback"

	// Pull the state + code_challenge out of the authorize URL so the fake
	// browser can respond with the matching state.
	u, err := url.Parse(resp.VerificationURI)
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatal("authorize URL has no state")
	}
	if u.Query().Get("code_challenge") == "" || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize URL missing PKCE: %s", resp.VerificationURI)
	}

	// Fire the callback as the browser would after the user signs in.
	cbURL := redirectURI + "?code=oauth-code-1&state=" + url.QueryEscape(state)
	cbReq, err := http.NewRequestWithContext(ctx, http.MethodGet, cbURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	cbResp, err := (&http.Client{Timeout: 5 * time.Second}).Do(cbReq)
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", cbResp.StatusCode)
	}

	tok, err := flow.Poll(ctx, resp.DeviceCode, 0)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if tok.AccessToken != "tok-browser" {
		t.Errorf("access token = %q, want tok-browser", tok.AccessToken)
	}
	if tok.RefreshToken != "refresh-b" {
		t.Errorf("refresh token = %q, want refresh-b", tok.RefreshToken)
	}
	// The exchange must be the standard authorization-code grant with PKCE.
	if exchanged.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q, want authorization_code", exchanged.Get("grant_type"))
	}
	if exchanged.Get("code") != "oauth-code-1" {
		t.Errorf("code = %q, want oauth-code-1", exchanged.Get("code"))
	}
	if exchanged.Get("code_verifier") == "" {
		t.Error("code_verifier is empty")
	}
	if exchanged.Get("redirect_uri") != redirectURI {
		t.Errorf("redirect_uri = %q, want %q", exchanged.Get("redirect_uri"), redirectURI)
	}
}

// TestCodexBrowserFlowSurvivesStartContext pins the regression where the
// callback server was tied to Start's context: the TUI cancels that context
// as soon as Start returns, so the port died before the browser hit it and
// the callback produced "connection failed". The server must live until Poll.
func TestCodexBrowserFlowSurvivesStartContext(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id_token":"id-x","access_token":"tok-x","refresh_token":"refresh-x"}`))
	})
	issuer := httptest.NewServer(mux)
	defer issuer.Close()

	flow := CodexBrowserFlow{Issuer: issuer.URL, ClientID: "app_EMoamEEZ73f0CkXaXp7hrann"}

	// Start with a context that is cancelled immediately afterwards, exactly
	// how the TUI drives the flow.
	startCtx, cancelStart := context.WithTimeout(context.Background(), 2*time.Second)
	resp, err := flow.Start(startCtx)
	cancelStart()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Give the cancelled context a moment to propagate; the server must stay
	// up regardless.
	time.Sleep(100 * time.Millisecond)

	u, err := url.Parse(resp.VerificationURI)
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	port := strings.TrimPrefix(resp.DeviceCode, "port:")

	cbURL := "http://localhost:" + port + "/auth/callback?code=code-survives&state=" + url.QueryEscape(state)
	cbResp, err := (&http.Client{Timeout: 5 * time.Second}).Get(cbURL)
	if err != nil {
		t.Fatalf("callback after Start ctx cancelled: %v (server died)", err)
	}
	cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", cbResp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tok, err := flow.Poll(ctx, resp.DeviceCode, 0)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if tok.AccessToken != "tok-x" {
		t.Errorf("access token = %q, want tok-x", tok.AccessToken)
	}
}
