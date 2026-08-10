package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"rick/internal/provider"
)

// rewriteTransport points requests for a fake gateway host at the httptest
// server, so the client's BaseURL is not a loopback address (which the
// provider treats as a local endpoint without prompt caching).
type rewriteTransport struct {
	base   http.RoundTripper
	target string
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	u, err := url.Parse(rt.target)
	if err != nil {
		return nil, err
	}
	r.URL.Scheme = u.Scheme
	r.URL.Host = u.Host
	return rt.base.RoundTrip(r)
}

func kaClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	c := New("deepseek", "test-key", "https://gateway.test/v1")
	c.HTTP = &http.Client{Transport: rewriteTransport{
		base:   server.Client().Transport,
		target: server.URL,
	}}
	return c
}

// sseHandler serves the minimal SSE shape the adapter's readSSE consumes.
func kaSSEHandler(posts *[]map[string]any, mu *sync.Mutex, block <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		mu.Lock()
		*posts = append(*posts, m)
		mu.Unlock()
		if block != nil {
			<-block
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"x"},"usage":{"input_tokens":1,"cache_read_input_tokens":0}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: [DONE]`)
	}
}

// TestKeepaliveRefreshesIdleSessionPrefix verifies that an idle session's
// last stream body is re-sent after the keep-alive interval, shaped like the
// stream (stream=true) with a tiny output budget and the same messages, so it
// rides the same prompt-cache entry the next real stream will extend.
func TestKeepaliveRefreshesIdleSessionPrefix(t *testing.T) {
	var mu sync.Mutex
	var posts []map[string]any
	server := httptest.NewServer(kaSSEHandler(&posts, &mu, nil))
	defer server.Close()

	client := kaClient(t, server)
	client.SetKeepalive(150 * time.Millisecond)
	defer client.stopKeepalive()

	req := provider.Request{
		Model:          "test-model",
		System:         "stable system",
		Messages:       []provider.Message{provider.UserText("hello")},
		SessionID:      "sess-1",
		CacheRetention: provider.CacheRetentionLong,
		MaxTokens:      2048,
	}
	ch := make(chan provider.Event, 8)
	go client.Stream(context.Background(), req, ch)
	for range ch {
	}

	// Wait for the keep-alive POST (interval 150 ms; loop ticks every 75 ms).
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(posts)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("keep-alive POST never arrived (posts=%d)", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	ka := posts[1]
	mu.Unlock()
	stream, _ := ka["stream"].(bool)
	if !stream {
		t.Errorf("keep-alive body stream=%v, want true", ka["stream"])
	}
	if mt, ok := ka["max_tokens"].(float64); !ok || int(mt) != 1 {
		t.Errorf("keep-alive max_tokens=%v, want 1 (tiny output budget)", ka["max_tokens"])
	}
	msgs, _ := ka["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("keep-alive messages=%v, want system+user from the stream", ka["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	second, _ := msgs[1].(map[string]any)
	if first["role"] != "system" || second["role"] != "user" {
		t.Errorf("keep-alive message roles=%v/%v, want system/user", first["role"], second["role"])
	}
	if got := ka["model"]; got != "test-model" {
		t.Errorf("keep-alive model=%v, want test-model", got)
	}
}

// TestKeepaliveSkipsActiveStream verifies the in-flight guard: a long-running
// stream must never race a keep-alive POST with a stale mid-turn body, and
// the keep-alive only fires a full interval after the stream completes.
func TestKeepaliveSkipsActiveStream(t *testing.T) {
	var mu sync.Mutex
	var posts []map[string]any
	release := make(chan struct{})
	server := httptest.NewServer(kaSSEHandler(&posts, &mu, release))
	defer server.Close()

	client := kaClient(t, server)
	client.SetKeepalive(120 * time.Millisecond)
	defer client.stopKeepalive()

	req := provider.Request{
		Model:          "test-model",
		System:         "sys",
		Messages:       []provider.Message{provider.UserText("hello")},
		SessionID:      "sess-2",
		CacheRetention: provider.CacheRetentionLong,
	}
	ch := make(chan provider.Event, 8)
	done := make(chan struct{})
	go func() {
		client.Stream(context.Background(), req, ch)
		close(done)
	}()
	// Let the stream start and the loop tick several times while it is stuck.
	time.Sleep(400 * time.Millisecond)
	mu.Lock()
	n := len(posts)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("keep-alive fired during an active stream (posts=%d, want 1)", n)
	}
	close(release)
	<-done

	// After completion the session is idle again: a keep-alive may fire, but
	// only after a full interval has elapsed since the stream finished.
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n = len(posts)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("keep-alive POST never arrived after idle (posts=%d)", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestKeepaliveRequiresSessionAndRetention pins the opt-outs: no session id
// and retention "none" never register a session, so the loop stays idle.
func TestKeepaliveRequiresSessionAndRetention(t *testing.T) {
	client := New("deepseek", "test-key", "https://gateway.test/v1")
	client.SetKeepalive(50 * time.Millisecond)

	client.noteKeepalive(provider.Request{
		SessionID:      "",
		CacheRetention: provider.CacheRetentionLong,
	}, wireRequest{}, nil)
	client.noteKeepalive(provider.Request{
		SessionID:      "sess-x",
		CacheRetention: provider.CacheRetentionNone,
	}, wireRequest{}, nil)

	client.kaMu.Lock()
	n := len(client.kaSessions)
	client.kaMu.Unlock()
	if n != 0 {
		t.Fatalf("keep-alive sessions registered without session/retention (got %d)", n)
	}
	client.stopKeepalive()
}

// TestKeepalivePrunesIdleSessions verifies the 24h prune runs even when the
// keep-alive interval is far shorter — the bug where the else-if branch was
// unreachable because every idle > 24h already matched the interval branch.
func TestKeepalivePrunesIdleSessions(t *testing.T) {
	client := New("deepseek", "test-key", "https://gateway.test/v1")
	client.SetKeepalive(10 * time.Millisecond)
	client.kaMu.Lock()
	client.kaSessions = map[string]*kaSession{
		"dead-1": {last: time.Now().Add(-48 * time.Hour)},
		"live-1": {last: time.Now()},
	}
	client.kaMu.Unlock()

	// The tick prunes dead-1 (idle > 24h) and sends a keep-alive for live-1
	// only if it is idle past the interval — it is not, so nothing is sent.
	client.keepaliveTick()

	client.kaMu.Lock()
	defer client.kaMu.Unlock()
	if _, ok := client.kaSessions["dead-1"]; ok {
		t.Errorf("dead-1 not pruned: keep-alive map still holds it")
	}
	if _, ok := client.kaSessions["live-1"]; !ok {
		t.Errorf("live-1 was pruned despite being fresh")
	}
}

// TestKeepaliveMapSizeCap verifies the map cannot grow past kaMaxSessions:
// the oldest idle entries beyond the bound are dropped on a tick.
func TestKeepaliveMapSizeCap(t *testing.T) {
	client := New("deepseek", "test-key", "https://gateway.test/v1")
	client.SetKeepalive(10 * time.Millisecond)
	client.kaMu.Lock()
	client.kaSessions = make(map[string]*kaSession, kaMaxSessions+10)
	for i := 0; i < kaMaxSessions+10; i++ {
		client.kaSessions[fmt.Sprintf("sess-%d", i)] = &kaSession{last: time.Now().Add(-time.Hour)}
	}
	client.kaMu.Unlock()

	client.keepaliveTick()

	client.kaMu.Lock()
	defer client.kaMu.Unlock()
	if len(client.kaSessions) > kaMaxSessions {
		t.Fatalf("keep-alive map grew past cap: %d entries (cap %d)", len(client.kaSessions), kaMaxSessions)
	}
}
