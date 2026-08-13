package catalog

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DeviceFlow describes an RFC 8628 OAuth 2.0 Device Authorization Grant.
// Public clients (CLI tools) use this to let users authenticate in a browser
// without embedding a client secret. GitHub (and the Copilot entry) use this;
// ChatGPT / Codex uses the newer CodexDeviceFlow below.
type DeviceFlow struct {
	DeviceAuthURL string // POST here to request a device code
	TokenURL      string // POST here to poll for a token
	ClientID      string // public client identifier
	Scope         string // space-separated scopes
	// Audience is the OAuth audience claim. OpenAI's auth endpoints require
	// it (audience=https://api.openai.com/v1 for the codex client); other
	// providers (GitHub) ignore it.
	Audience string
}

// DeviceAuth is the subset of the device flows the TUI drives. Both the
// RFC 8628 DeviceFlow and the Codex device flow implement it, so the auth
// UI stays provider-agnostic.
type DeviceAuth interface {
	// Start requests a device code and returns the user-facing prompt data.
	Start(ctx context.Context) (*DeviceCodeResponse, error)
	// Poll blocks until the user authorizes (or the context expires) and
	// returns the final token response.
	Poll(ctx context.Context, deviceCode string, interval int) (*TokenResponse, error)
}

// CodexDeviceFlow implements the current ChatGPT / Codex device-authorization
// protocol (auth.openai.com). It is NOT RFC 8628: the endpoints are
// /api/accounts/deviceauth/usercode and /api/accounts/deviceauth/token, and
// the poll returns a short-lived authorization_code with PKCE parameters that
// must then be exchanged at /oauth/token for the real tokens.
type CodexDeviceFlow struct {
	Issuer   string // e.g. https://auth.openai.com
	ClientID string // e.g. app_EMoamEEZ73f0CkXaXp7hrann
}

// DeviceCodeResponse is what the device authorization endpoint returns.
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	// Some providers (GitHub) use a different field name.
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

// TokenResponse is a successful token endpoint reply.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	// IDToken is the OIDC id_token the ChatGPT / Codex exchange returns. Its
	// claims carry the account id the backend requires as ChatGPT-Account-ID.
	IDToken string `json:"id_token"`
}

// tokenError is the error shape the token endpoint returns while the user
// has not yet authorized the device (or has denied it).
type tokenError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Start initiates the device flow: it POSTs to the device authorization
// endpoint and returns the codes the user needs.
func (f DeviceFlow) Start(ctx context.Context) (*DeviceCodeResponse, error) {
	form := url.Values{
		"client_id": {f.ClientID},
	}
	if f.Scope != "" {
		form.Set("scope", f.Scope)
	}
	if f.Audience != "" {
		form.Set("audience", f.Audience)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.DeviceAuthURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("device flow: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// OpenAI's auth endpoints sit behind Cloudflare, which rejects Go's
	// default User-Agent with a 403. Send a browser-like UA so the first
	// request of the flow is not blocked before it starts.
	req.Header.Set("User-Agent", "rick-cli")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("device flow: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device flow: http %d: %s", resp.StatusCode, snippet(body))
	}

	var dc DeviceCodeResponse
	if err := json.Unmarshal(body, &dc); err != nil {
		return nil, fmt.Errorf("device flow: bad response: %w", err)
	}
	if dc.DeviceCode == "" || dc.UserCode == "" {
		return nil, fmt.Errorf("device flow: server returned no device_code/user_code")
	}
	if dc.VerificationURI == "" {
		dc.VerificationURI = dc.VerificationURIComplete
	}
	if dc.Interval <= 0 {
		dc.Interval = 5 // RFC 8628 default
	}
	if dc.ExpiresIn <= 0 {
		dc.ExpiresIn = 900 // 15 minutes
	}
	return &dc, nil
}

// Poll polls the token endpoint until the user authorizes the device, the
// code expires, or the context is cancelled. It handles "authorization_pending"
// and "slow_down" per RFC 8628 §3.5.
func (f DeviceFlow) Poll(ctx context.Context, deviceCode string, interval int) (*TokenResponse, error) {
	if interval <= 0 {
		interval = 5
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(interval) * time.Second):
		}

		form := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {deviceCode},
			"client_id":   {f.ClientID},
		}
		if f.Audience != "" {
			form.Set("audience", f.Audience)
		}

		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, f.TokenURL,
			strings.NewReader(form.Encode()))
		if err != nil {
			cancel()
			return nil, fmt.Errorf("device flow poll: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "rick-cli")

		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			cancel()
			// Transient network error — keep polling.
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()

		// Success.
		if resp.StatusCode == http.StatusOK {
			var tok TokenResponse
			if err := json.Unmarshal(body, &tok); err != nil {
				return nil, fmt.Errorf("device flow: bad token response: %w", err)
			}
			if tok.AccessToken != "" {
				return &tok, nil
			}
		}

		// Error — decide whether to retry or give up.
		var te tokenError
		if err := json.Unmarshal(body, &te); err != nil {
			return nil, fmt.Errorf("device flow: http %d: %s", resp.StatusCode, snippet(body))
		}
		switch te.Error {
		case "authorization_pending":
			// User hasn't authorized yet — keep polling.
			continue
		case "slow_down":
			interval += 5
			continue
		case "expired_token":
			return nil, fmt.Errorf("device code expired — restart the sign-in")
		case "access_denied":
			return nil, fmt.Errorf("authorization denied by user")
		default:
			desc := te.ErrorDescription
			if desc == "" {
				desc = te.Error
			}
			return nil, fmt.Errorf("device flow: %s", desc)
		}
	}
}

// CodexDeviceFlow ---------------------------------------------------------

// codexUserCodeResponse is the /api/accounts/deviceauth/usercode reply.
type codexUserCodeResponse struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
	// interval arrives as a string on the wire ("5"), hence json.Number.
	Interval  json.Number `json:"interval"`
	ExpiresAt string      `json:"expires_at"`
}

// codexTokenPollResponse is the /api/accounts/deviceauth/token reply once the
// user has authorized. It carries the one-time authorization_code plus the
// PKCE verifier/challenge to exchange at /oauth/token.
type codexTokenPollResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeChallenge     string `json:"code_challenge"`
	CodeVerifier      string `json:"code_verifier"`
}

// codexExchangeResponse is the /oauth/token reply for the authorization-code
// grant.
type codexExchangeResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// CodexAccountID extracts the ChatGPT account id the backend requires in the
// ChatGPT-Account-ID header. It reads the JWT claims of the id_token first,
// falling back to the access token. The backend rejects a bearer token with
// 401 {"detail":"Unauthorized"} when the account header is missing.
func CodexAccountID(idToken, accessToken string) string {
	for _, tok := range []string{idToken, accessToken} {
		if id := accountIDFromJWT(tok); id != "" {
			return id
		}
	}
	return ""
}

func accountIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		AccountID  string `json:"chatgpt_account_id"`
		OpenAIAuth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
		Organizations []struct {
			ID string `json:"id"`
		} `json:"organizations"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	if claims.AccountID != "" {
		return claims.AccountID
	}
	if claims.OpenAIAuth.AccountID != "" {
		return claims.OpenAIAuth.AccountID
	}
	if len(claims.Organizations) > 0 && claims.Organizations[0].ID != "" {
		return claims.Organizations[0].ID
	}
	return ""
}

// codexPendingError is the shape of the "not yet authorized" reply the poll
// endpoint returns (HTTP 403 with code deviceauth_authorization_pending).
type codexPendingError struct {
	Error struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

func (f CodexDeviceFlow) baseURL() string {
	return strings.TrimRight(f.Issuer, "/")
}

func (f CodexDeviceFlow) clientID() string {
	if f.ClientID != "" {
		return f.ClientID
	}
	// The public client registered with auth.openai.com for the Codex CLI.
	return "app_EMoamEEZ73f0CkXaXp7hrann"
}

// Start implements DeviceAuth. It calls the codex-specific usercode endpoint
// (not the RFC 8628 device-authorization endpoint, which OpenAI retired) and
// returns the prompt data in the generic shape the TUI renders.
func (f CodexDeviceFlow) Start(ctx context.Context) (*DeviceCodeResponse, error) {
	body, err := json.Marshal(map[string]string{"client_id": f.clientID()})
	if err != nil {
		return nil, fmt.Errorf("codex device flow: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		f.baseURL()+"/api/accounts/deviceauth/usercode", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("codex device flow: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "rick-cli")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex device flow: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex device flow: http %d: %s", resp.StatusCode, snippet(respBody))
	}

	var uc codexUserCodeResponse
	if err := json.Unmarshal(respBody, &uc); err != nil {
		return nil, fmt.Errorf("codex device flow: bad response: %w", err)
	}
	if uc.DeviceAuthID == "" || uc.UserCode == "" {
		return nil, fmt.Errorf("codex device flow: server returned no device_auth_id/user_code")
	}

	interval, _ := uc.Interval.Int64()
	if interval <= 0 {
		interval = 5
	}
	// The codex poll returns the device_auth_id + user_code pair; carry both
	// in the DeviceCode field (tab-separated) so Poll can round-trip them.
	return &DeviceCodeResponse{
		DeviceCode:      uc.DeviceAuthID + "\t" + uc.UserCode,
		UserCode:        uc.UserCode,
		VerificationURI: f.baseURL() + "/codex/device",
		Interval:        int(interval),
		ExpiresIn:       15 * 60, // codex device codes expire after 15 minutes
	}, nil
}

// Poll implements DeviceAuth. It polls the codex token endpoint until the
// user authorizes, then exchanges the returned authorization_code for real
// tokens at /oauth/token.
func (f CodexDeviceFlow) Poll(ctx context.Context, deviceCode string, interval int) (*TokenResponse, error) {
	if interval <= 0 {
		interval = 5
	}
	deviceAuthID, userCode, _ := strings.Cut(deviceCode, "\t")
	if deviceAuthID == "" || userCode == "" {
		return nil, fmt.Errorf("codex device flow: malformed device code")
	}

	code, err := f.pollForAuthorizationCode(ctx, deviceAuthID, userCode, interval)
	if err != nil {
		return nil, err
	}

	// Exchange the authorization_code (with PKCE) for the real tokens.
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code.AuthorizationCode},
		"redirect_uri":  {f.baseURL() + "/deviceauth/callback"},
		"client_id":     {f.clientID()},
		"code_verifier": {code.CodeVerifier},
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		f.baseURL()+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("codex device flow: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "rick-cli")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex device flow: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex device flow exchange: http %d: %s", resp.StatusCode, snippet(respBody))
	}
	var tok codexExchangeResponse
	if err := json.Unmarshal(respBody, &tok); err != nil {
		return nil, fmt.Errorf("codex device flow: bad token response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("codex device flow: exchange returned no access_token")
	}
	return &TokenResponse{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    tok.ExpiresIn,
		IDToken:      tok.IDToken,
	}, nil
}

// pollForAuthorizationCode polls /api/accounts/deviceauth/token until the
// user authorizes the device. The endpoint returns HTTP 403 with
// code deviceauth_authorization_pending while waiting, and 200 with the
// authorization_code once approved.
func (f CodexDeviceFlow) pollForAuthorizationCode(ctx context.Context, deviceAuthID, userCode string, interval int) (*codexTokenPollResponse, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(interval) * time.Second):
		}

		body, err := json.Marshal(map[string]string{
			"device_auth_id": deviceAuthID,
			"user_code":      userCode,
		})
		if err != nil {
			return nil, fmt.Errorf("codex device flow: %w", err)
		}

		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
			f.baseURL()+"/api/accounts/deviceauth/token", bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, fmt.Errorf("codex device flow: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "rick-cli")

		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			cancel()
			// Transient network error — keep polling.
			continue
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()

		if resp.StatusCode == http.StatusOK {
			var code codexTokenPollResponse
			if err := json.Unmarshal(respBody, &code); err != nil {
				return nil, fmt.Errorf("codex device flow: bad poll response: %w", err)
			}
			if code.AuthorizationCode != "" {
				return &code, nil
			}
			// Authorized but no code yet — keep polling.
			continue
		}

		var pe codexPendingError
		if err := json.Unmarshal(respBody, &pe); err == nil && pe.Error.Code != "" {
			// OpenAI returns 403 for BOTH "still waiting" and "expired", and
			// 404 for "not found" — only the code distinguishes them. Retry
			// only while the user is still expected to authorize; any other
			// code (expired, denied, not found) is terminal, so fail fast
			// instead of polling for the full 15-minute TUI timeout.
			switch pe.Error.Code {
			case "deviceauth_authorization_pending":
				continue
			case "deviceauth_user_code_expired":
				return nil, fmt.Errorf("codex device flow: authorization code expired — restart the sign-in")
			case "deviceauth_not_found":
				return nil, fmt.Errorf("codex device flow: device authorization not found — restart the sign-in")
			case "access_denied", "deviceauth_denied":
				return nil, fmt.Errorf("codex device flow: authorization denied")
			default:
				return nil, fmt.Errorf("codex device flow: %s", pe.Error.Message)
			}
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			// No recognizable error code — the codex client still treats
			// bare 403/404 as pending. Keep polling.
			continue
		}
		return nil, fmt.Errorf("codex device flow: http %d: %s", resp.StatusCode, snippet(respBody))
	}
}

// CodexBrowserFlow is the ChatGPT / Codex browser-login flow used by the
// official Codex CLI (and opencode). The device-code flow OpenAI once offered
// mints tokens the chatgpt.com backend rejects with "missing_end_user_auth",
// so browser login (localhost OAuth callback + PKCE) is the working path.
type CodexBrowserFlow struct {
	Issuer   string // e.g. https://auth.openai.com
	ClientID string // e.g. app_EMoamEEZ73f0CkXaXp7hrann
}

// The Codex CLI's redirect-URI allow-list: 1455 primary, 1457 fallback.
const (
	codexLoginPort         = 1455
	codexLoginFallbackPort = 1457
)

// codexPKCE is a PKCE S256 verifier/challenge pair.
type codexPKCE struct {
	verifier  string
	challenge string
}

// newCodexPKCE generates a 64-byte URL-safe base64 verifier and its S256
// challenge, matching the Codex CLI.
func newCodexPKCE() (codexPKCE, error) {
	buf := make([]byte, 64)
	if _, err := rand.Read(buf); err != nil {
		return codexPKCE{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return codexPKCE{
		verifier:  verifier,
		challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// codexSession is one in-flight browser login.
type codexSession struct {
	port     int
	verifier string
	server   *http.Server
	// result receives the OAuth code (or the error) once /auth/callback fires.
	result chan codexCallback
}

type codexCallback struct {
	code string
	err  error
}

// codexSessions tracks in-flight browser logins by callback port.
var codexSessions = struct {
	mu sync.Mutex
	m  map[int]*codexSession
}{m: map[int]*codexSession{}}

// Start binds a localhost callback server, opens the browser on the OAuth
// authorize URL, and returns a DeviceCodeResponse whose VerificationURI is
// that URL. DeviceCode carries "port:N" so Poll can find the session. The
// browser flow has no user code, so UserCode stays empty.
func (f CodexBrowserFlow) Start(ctx context.Context) (*DeviceCodeResponse, error) {
	pkce, err := newCodexPKCE()
	if err != nil {
		return nil, fmt.Errorf("codex browser flow: %w", err)
	}
	port, ln, err := bindCodexCallback()
	if err != nil {
		return nil, fmt.Errorf("codex browser flow: %w", err)
	}
	sess := &codexSession{port: port, verifier: pkce.verifier, result: make(chan codexCallback, 1)}
	codexSessions.mu.Lock()
	codexSessions.m[port] = sess
	codexSessions.mu.Unlock()

	state := randomCodexState()
	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", port)
	authURL := codexAuthorizeURL(strings.TrimRight(f.Issuer, "/"), f.clientID(), redirectURI, pkce, state)

	// The server must outlive Start: the TUI calls Start in a goroutine with
	// its own context that is cancelled as soon as Start returns, so tying
	// the listener to ctx would close the callback port before the browser
	// ever hits it. Poll shuts the server down when the login completes.
	sess.server = &http.Server{Handler: codexCallbackHandler(sess, state)}
	go func() {
		if err := sess.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			sess.result <- codexCallback{err: fmt.Errorf("codex browser flow: callback server: %w", err)}
		}
	}()

	return &DeviceCodeResponse{
		DeviceCode:      fmt.Sprintf("port:%d", port),
		VerificationURI: authURL,
		ExpiresIn:       15 * 60,
	}, nil
}

// Poll waits for the browser callback and exchanges the authorization code.
// deviceCode is the "port:N" marker Start produced; interval is unused.
func (f CodexBrowserFlow) Poll(ctx context.Context, deviceCode string, _ int) (*TokenResponse, error) {
	port := codexLoginPort
	if rest, ok := strings.CutPrefix(deviceCode, "port:"); ok {
		if n, err := strconv.Atoi(rest); err == nil {
			port = n
		}
	}
	codexSessions.mu.Lock()
	sess := codexSessions.m[port]
	codexSessions.mu.Unlock()
	if sess == nil {
		return nil, fmt.Errorf("codex browser flow: no login session on port %d", port)
	}
	// Shut the callback server down and forget the session no matter how the
	// wait ends, so a cancelled or failed login does not leak the port.
	defer func() {
		_ = sess.server.Close()
		codexSessions.mu.Lock()
		delete(codexSessions.m, port)
		codexSessions.mu.Unlock()
	}()

	var cb codexCallback
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case cb = <-sess.result:
	}
	if cb.err != nil {
		return nil, cb.err
	}
	return f.exchange(ctx, cb.code, sess.verifier, port)
}

// exchange performs the authorization-code grant with PKCE at the issuer.
func (f CodexBrowserFlow) exchange(ctx context.Context, code, verifier string, port int) (*TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {fmt.Sprintf("http://localhost:%d/auth/callback", port)},
		"client_id":     {f.clientID()},
		"code_verifier": {verifier},
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		strings.TrimRight(f.Issuer, "/")+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("codex browser flow: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "rick-cli")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex browser flow: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex browser flow exchange: http %d: %s", resp.StatusCode, snippet(body))
	}
	var tok codexExchangeResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("codex browser flow: bad token response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("codex browser flow: exchange returned no access_token")
	}
	return &TokenResponse{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    tok.ExpiresIn,
		IDToken:      tok.IDToken,
	}, nil
}

func (f CodexBrowserFlow) clientID() string {
	if f.ClientID != "" {
		return f.ClientID
	}
	return "app_EMoamEEZ73f0CkXaXp7hrann"
}

// bindCodexCallback binds 127.0.0.1:1455, falling back to 1457, mirroring the
// Codex CLI's redirect-URI allow-list.
func bindCodexCallback() (int, net.Listener, error) {
	for _, port := range []int{codexLoginPort, codexLoginFallbackPort} {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return port, ln, nil
		}
	}
	return 0, nil, fmt.Errorf("callback ports %d and %d are in use", codexLoginPort, codexLoginFallbackPort)
}

// codexCallbackHandler serves the /auth/callback path and completes the
// session's result channel.
func codexCallbackHandler(sess *codexSession, state string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("state") != state {
			codexWriteSimple(w, http.StatusBadRequest, "State mismatch")
			return
		}
		if oauthErr := q.Get("error"); oauthErr != "" {
			codexCallbackDone(sess, codexCallback{err: fmt.Errorf("codex browser flow: %s", oauthErr)})
			codexWriteSimple(w, http.StatusBadRequest, oauthErr)
			return
		}
		code := q.Get("code")
		if code == "" {
			codexCallbackDone(sess, codexCallback{err: fmt.Errorf("codex browser flow: missing authorization code")})
			codexWriteSimple(w, http.StatusBadRequest, "Missing authorization code")
			return
		}
		codexCallbackDone(sess, codexCallback{code: code})
		codexWriteSimple(w, http.StatusOK, "Signed in — you can close this tab and return to rick.")
	})
}

// codexCallbackDone delivers a callback result without blocking: the result
// channel is buffered for one, and duplicate callbacks (a user retrying the
// URL) must not hang the HTTP handler.
func codexCallbackDone(sess *codexSession, cb codexCallback) {
	select {
	case sess.result <- cb:
	default:
	}
}

func codexWriteSimple(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg))
}

// randomCodexState returns a URL-safe random state string.
func randomCodexState() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "rick-state"
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// codexAuthorizeURL builds the /oauth/authorize URL the Codex CLI uses,
// including the scopes and simplified-flow flags the backend expects.
func codexAuthorizeURL(baseURL, clientID, redirectURI string, pkce codexPKCE, state string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "openid profile email offline_access api.connectors.read api.connectors.invoke")
	q.Set("code_challenge", pkce.challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("state", state)
	q.Set("originator", "rick-cli")
	return baseURL + "/oauth/authorize?" + q.Encode()
}

// CopilotTokenExchange exchanges a GitHub OAuth access token for a short-lived
// GitHub Copilot API token. The Copilot API (api.githubcopilot.com) requires
// its own token, obtained from the internal endpoint.
func CopilotTokenExchange(ctx context.Context, githubToken string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/copilot_internal/v2/token", nil)
	if err != nil {
		return "", fmt.Errorf("copilot exchange: %w", err)
	}
	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "rick-cli")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("copilot exchange: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("copilot exchange: http %d: %s", resp.StatusCode, snippet(body))
	}

	var result struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("copilot exchange: bad response: %w", err)
	}
	if result.Token == "" {
		return "", fmt.Errorf("copilot exchange: no token in response — does this account have Copilot?")
	}
	return result.Token, nil
}
