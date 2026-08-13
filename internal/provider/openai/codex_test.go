package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"rick/internal/provider"
)

// codexTestClient builds a codex client pointed at a local test server. The
// package-level codexBaseURL var is swapped so requests hit the test server.
func codexTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	old := codexBaseURL
	codexBaseURL = server.URL
	t.Cleanup(func() { codexBaseURL = old })
	c := New("chatgpt", "access-token", server.URL)
	c.SetCodex("refresh-token", "", time.Now().Add(time.Hour).Unix(), nil)
	return c
}

// TestCodexStreamEmitsText verifies the Responses-API SSE parser turns
// output_text deltas and the completed event into text + usage + done.
func TestCodexStreamEmitsText(t *testing.T) {
	var mu sync.Mutex
	var gotBody map[string]any
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		gotBody = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"Hel"}` + "\n\n"))
		w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"lo"}` + "\n\n"))
		w.Write([]byte(`data: {"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":2}},"end_turn":true}}` + "\n\n"))
	}))
	defer server.Close()

	c := codexTestClient(t, server)
	ch := make(chan provider.Event, 16)
	go c.Stream(context.Background(), provider.Request{
		Model: "chatgpt/gpt-5.5", System: "be brief",
		Messages: []provider.Message{provider.UserText("hi")},
	}, ch)

	var text strings.Builder
	var usage *provider.Usage
	done := false
	for ev := range ch {
		switch ev.Kind {
		case provider.EventText:
			text.WriteString(ev.Text)
		case provider.EventUsage:
			usage = ev.Usage
		case provider.EventDone:
			done = true
		case provider.EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if text.String() != "Hello" {
		t.Errorf("text = %q, want Hello", text.String())
	}
	if !done {
		t.Error("no done event")
	}
	if usage == nil || usage.OutputTokens != 5 || usage.InputTokens != 8 || usage.CacheReadTokens != 2 {
		t.Errorf("usage = %+v, want output 5 input 8 cache-read 2", usage)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer access-token" {
		t.Errorf("Authorization = %q, want Bearer access-token", gotAuth)
	}
	if gotBody["model"] != "gpt-5.5" {
		t.Errorf("model = %v, want gpt-5.5 (provider prefix stripped)", gotBody["model"])
	}
	if gotBody["store"] != false {
		t.Errorf("store = %v, want false", gotBody["store"])
	}
	if gotBody["stream"] != true {
		t.Errorf("stream = %v, want true", gotBody["stream"])
	}
	if gotBody["instructions"] != "be brief" {
		t.Errorf("instructions = %v, want be brief", gotBody["instructions"])
	}
	input, ok := gotBody["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %#v, want one user item", gotBody["input"])
	}
	user, _ := input[0].(map[string]any)
	if user["role"] != "user" {
		t.Errorf("input[0] role = %v, want user", user["role"])
	}
}

// TestCodexStreamEmitsToolCall verifies output_item.done with a function_call
// item produces a tool-call event.
func TestCodexStreamEmitsToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"a\":1}"}}` + "\n\n"))
		w.Write([]byte(`data: {"type":"response.completed","response":{"id":"r1"}}` + "\n\n"))
	}))
	defer server.Close()

	c := codexTestClient(t, server)
	ch := make(chan provider.Event, 16)
	go c.Stream(context.Background(), provider.Request{
		Model:    "gpt-5.5",
		Messages: []provider.Message{provider.UserText("run it")},
		Tools: []provider.ToolSchema{{
			Name: "bash", Description: "run a command",
			InputSchema: map[string]any{"type": "object"},
		}},
	}, ch)

	var calls []provider.ToolCall
	done := false
	for ev := range ch {
		switch ev.Kind {
		case provider.EventToolCall:
			calls = append(calls, *ev.ToolCall)
		case provider.EventDone:
			done = true
		case provider.EventError:
			t.Fatalf("unexpected error: %v", ev.Err)
		}
	}
	if !done {
		t.Fatal("no done event")
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	if calls[0].Name != "bash" {
		t.Errorf("tool name = %q, want bash", calls[0].Name)
	}
	if strings.TrimSpace(string(calls[0].Input)) != `{"a":1}` {
		t.Errorf("tool input = %s, want {\"a\":1}", calls[0].Input)
	}
}

// TestCodexStreamRefresh verifies an expired access token triggers a refresh
// via the /oauth/token refresh grant before the API call.
func TestCodexStreamRefresh(t *testing.T) {
	oldBase, oldToken := codexBaseURL, codexTokenURL
	t.Cleanup(func() { codexBaseURL, codexTokenURL = oldBase, oldToken })

	var mu sync.Mutex
	refreshed := false
	var apiAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			mu.Lock()
			refreshed = true
			mu.Unlock()
			_ = r.ParseForm()
			if r.PostForm.Get("grant_type") != "refresh_token" {
				t.Errorf("grant_type = %q, want refresh_token", r.PostForm.Get("grant_type"))
			}
			if r.PostForm.Get("client_id") != codexClientID {
				t.Errorf("client_id = %q, want %q", r.PostForm.Get("client_id"), codexClientID)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"fresh-token","refresh_token":"new-refresh","expires_in":3600}`))
		case "/responses":
			mu.Lock()
			apiAuth = r.Header.Get("Authorization")
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"ok"}` + "\n\n"))
			w.Write([]byte(`data: {"type":"response.completed","response":{"id":"r1"}}` + "\n\n"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	codexBaseURL = server.URL
	codexTokenURL = server.URL + "/oauth/token"

	// Access token already expired; the client must refresh first.
	c := New("chatgpt", "stale-token", server.URL)
	c.SetCodex("refresh-token", "", time.Now().Add(-time.Minute).Unix(), nil)

	ch := make(chan provider.Event, 8)
	go c.Stream(context.Background(), provider.Request{
		Model:    "gpt-5.5",
		Messages: []provider.Message{provider.UserText("hi")},
	}, ch)
	for ev := range ch {
		if ev.Kind == provider.EventError {
			t.Fatalf("unexpected error: %v", ev.Err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if !refreshed {
		t.Error("token refresh was not attempted")
	}
	if apiAuth != "Bearer fresh-token" {
		t.Errorf("API Authorization = %q, want Bearer fresh-token", apiAuth)
	}
}

// TestCodexModelsParsesSlug verifies the /models envelope with slug ids.
func TestCodexModelsParsesSlug(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %s, want /models", r.URL.Path)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acc-1" {
			t.Errorf("ChatGPT-Account-ID = %q, want acc-1", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[{"slug":"gpt-5.5","display_name":"GPT-5.5","context_window":400000},{"slug":"gpt-5.4","display_name":"GPT-5.4"}]}`))
	}))
	defer server.Close()

	c := codexTestClient(t, server)
	c.SetCodex("refresh-token", "acc-1", time.Now().Add(time.Hour).Unix(), nil)
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2", len(models))
	}
	if models[0].ID != "gpt-5.5" || models[0].Name != "GPT-5.5" {
		t.Errorf("models[0] = %+v", models[0])
	}
	if models[0].ContextWindow != 400000 {
		t.Errorf("models[0] context = %d, want 400000", models[0].ContextWindow)
	}
	// gpt-5.4 has no context_window; it falls back to the id catalog.
	if models[1].ContextWindow != 400000 {
		t.Errorf("models[1] context = %d, want inferred 400000", models[1].ContextWindow)
	}
}

// TestCodexBodyMapsToolResults verifies assistant tool calls and user tool
// results serialize to function_call + function_call_output input items.
func TestCodexBodyMapsToolResults(t *testing.T) {
	c := New("chatgpt", "t", "https://chatgpt.com/backend-api/codex")
	raw, err := codexBody(c, provider.Request{
		Model: "gpt-5.5",
		Messages: []provider.Message{
			{Role: "assistant", Content: []provider.ContentBlock{
				{Type: "text", Text: "calling"},
				{Type: "tool_use", ID: "call_1", Name: "bash", Input: json.RawMessage(`{"x":1}`)},
			}},
			{Role: "user", Content: []provider.ContentBlock{
				{Type: "tool_result", ToolUseID: "call_1", Content: "ok"},
			}},
		},
	}, true)
	if err != nil {
		t.Fatalf("codexBody: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	input, _ := body["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input len = %d, want 3 (assistant msg, function_call, function_call_output)", len(input))
	}
	fc, _ := input[1].(map[string]any)
	if fc["type"] != "function_call" || fc["name"] != "bash" || fc["call_id"] != "call_1" {
		t.Errorf("function_call item = %#v", fc)
	}
	fo, _ := input[2].(map[string]any)
	if fo["type"] != "function_call_output" || fo["call_id"] != "call_1" || fo["output"] != "ok" {
		t.Errorf("function_call_output item = %#v", fo)
	}
}

// TestCodexStreamDoesNotDuplicateText pins the doubled-reply regression: the
// parser was emitting text from both response.output_text.delta AND the
// completed message item inside response.output_item.done, so every GPT reply
// appeared twice. Text must come only from the deltas; the done item only
// finalizes the turn.
func TestCodexStreamDoesNotDuplicateText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"one "}` + "\n\n"))
		w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"two"}` + "\n\n"))
		// The completed message item carries the full text; emitting it again
		// would double the reply.
		w.Write([]byte(`data: {"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"one two"}]}}` + "\n\n"))
		w.Write([]byte(`data: {"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":2}},"end_turn":true}}` + "\n\n"))
	}))
	defer server.Close()

	c := codexTestClient(t, server)
	ch := make(chan provider.Event, 16)
	go c.Stream(context.Background(), provider.Request{
		Model:    "chatgpt/gpt-5.5",
		Messages: []provider.Message{provider.UserText("hi")},
	}, ch)

	var text strings.Builder
	for ev := range ch {
		if ev.Kind == provider.EventText {
			text.WriteString(ev.Text)
		}
	}
	if got := text.String(); got != "one two" {
		t.Fatalf("streamed text = %q, want %q (the full item text was re-emitted)", got, "one two")
	}
}

// TestGenerateImageStreamsImageResult verifies the image_generation_call
// result is parsed from the Responses-API SSE stream and returned as base64.
func TestGenerateImageStreamsImageResult(t *testing.T) {
	var mu sync.Mutex
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotBody = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"type":"response.output_item.done","item":{"type":"image_generation_call","result":"data:image/png;base64,aGVsbG8="}}` + "\n\n"))
		w.Write([]byte(`data: {"type":"response.completed","response":{"id":"r1"}}` + "\n\n"))
	}))
	defer server.Close()

	c := codexTestClient(t, server)
	results, err := c.GenerateImage(context.Background(), CodexImageRequest{Prompt: "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Base64 != "aGVsbG8=" {
		t.Errorf("base64 = %q, want aGVsbG8= (data URI prefix stripped)", results[0].Base64)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotBody["model"] != codexImageModel {
		t.Errorf("model = %v, want %s", gotBody["model"], codexImageModel)
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v, want one image_generation tool", gotBody["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "image_generation" || tool["model"] != "gpt-image-2" {
		t.Errorf("tool = %#v, want image_generation/gpt-image-2", tool)
	}
}

// TestGenerateImageRequiresCodex ensures the tool errors clearly when the
// client is not the Codex backend.
func TestGenerateImageRequiresCodex(t *testing.T) {
	c := New("openai", "sk-x", "")
	if _, err := c.GenerateImage(context.Background(), CodexImageRequest{Prompt: "x"}); err == nil {
		t.Fatal("expected an error for a non-codex client")
	}
}
