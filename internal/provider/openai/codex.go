package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"rick/internal/provider"
	"rick/internal/provider/catalog"
)

// codexBaseURL is the ChatGPT backend that serves the Codex Responses API to
// ChatGPT Plus/Pro subscribers. It is not the platform API: the OAuth access
// token from auth.openai.com is only accepted here (api.openai.com/v1 returns
// 401 "Incorrect API key" for it). A package-level var so tests can redirect
// it to a local httptest server.
var codexBaseURL = "https://chatgpt.com/backend-api/codex"

// codexClientID is the public OAuth client registered for the Codex CLI.
const codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

// codexTokenURL is the auth server's token endpoint, used for both the device
// flow's authorization-code exchange and refresh-token renewal. A
// package-level var so tests can redirect it to a local httptest server.
var codexTokenURL = "https://auth.openai.com/oauth/token"

// CodexConfig switches an openai.Client to the ChatGPT Codex backend: requests
// go to chatgpt.com/backend-api/codex/responses in Responses-API wire format,
// and access tokens are refreshed with the stored refresh token before they
// expire.
type CodexConfig struct {
	// RefreshToken is the OAuth refresh token issued alongside the access
	// token. Access tokens expire quickly; the client refreshes when the
	// stored expiry is near and persists the new pair via OnTokenRefresh.
	RefreshToken string
	// TokenExpiresAt is the unix-epoch second when the current access token
	// expires. 0 means unknown — treat as already expired so the first
	// request refreshes.
	TokenExpiresAt int64
	// AccountID is the ChatGPT account id the backend requires in the
	// ChatGPT-Account-ID header. It may be empty until derived from a token.
	AccountID string
	// OnTokenRefresh persists a refreshed token pair (access + refresh +
	// expiry). May be nil; the client still uses the refreshed token in
	// memory for the rest of the process.
	OnTokenRefresh func(accessToken, refreshToken string, expiresAt int64)

	mu         sync.Mutex
	refreshing bool
}

// codexRefreshResponse is the /oauth/token refresh reply.
type codexRefreshResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// CodexImageRequest asks the ChatGPT Codex backend to generate an image with
// the image_generation tool (the same tool the chat UI's picture mode uses).
// The backend requires the mainline chat model (gpt-5.5) plus the tool; it
// bills the generation through the account's ChatGPT plan, not the API.
type CodexImageRequest struct {
	// Prompt is the natural-language image prompt.
	Prompt string
	// Size is one of the image_generation tool sizes ("1024x1024", "1536x1024",
	// ...). Empty uses the backend default.
	Size string
	// Quality is "auto" (default), "low", "medium", or "high".
	Quality string
}

// CodexImageResult is one generated image: the raw base64 payload (either a
// bare base64 blob or a "data:image/...;base64,..." data URI) plus the
// revised prompt when the backend rewrote the user's wording.
type CodexImageResult struct {
	Base64        string
	RevisedPrompt string
}

// codexImageModel is the mainline chat model that hosts the image_generation
// tool. The tool picks the actual GPT-Image model ("gpt-image-2").
const codexImageModel = "gpt-5.5"

// codexImageInstructions keeps the chat model on the image task; without it
// the model may answer in text instead of invoking the tool.
const codexImageInstructions = "Use the image_generation tool to create exactly one image for the user's request. Return the generated image result."

// GenerateImage generates one or more images through the ChatGPT backend and
// returns the base64 payloads. It streams the Responses-API request so the
// tool result arrives in an image_generation_call item.
func (c *Client) GenerateImage(ctx context.Context, req CodexImageRequest) ([]CodexImageResult, error) {
	if c.Codex == nil {
		return nil, fmt.Errorf("image generation requires the ChatGPT OAuth login (provider \"chatgpt\")")
	}
	token, err := c.codexToken(ctx)
	if err != nil {
		return nil, err
	}
	quality := req.Quality
	if quality == "" {
		quality = "auto"
	}
	size := req.Size
	if size == "" {
		size = "1024x1024"
	}
	payload := map[string]any{
		"model":        codexImageModel,
		"instructions": codexImageInstructions,
		"store":        false,
		"stream":       true,
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": req.Prompt},
			},
		}},
		"tools": []any{map[string]any{
			"type":          "image_generation",
			"model":         "gpt-image-2",
			"action":        "generate",
			"size":          size,
			"quality":       quality,
			"output_format": "png",
		}},
		"tool_choice": map[string]any{"type": "image_generation"},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		codexBaseURL+"/responses", strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	c.codexHeaders(httpReq, token)
	resp, err := (&http.Client{Timeout: 15 * time.Minute}).Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		return nil, fmt.Errorf("codex image generation: http %d: %s", resp.StatusCode, codexSnippet(body))
	}
	results, err := c.readCodexImageSSE(ctx, resp.Body)
	if err != nil {
		return nil, fmt.Errorf("codex image generation: %w", err)
	}
	return results, nil
}

// readCodexImageSSE scans the Responses-API stream for image_generation_call
// items and collects their base64 results.
func (c *Client) readCodexImageSSE(ctx context.Context, r io.Reader) ([]CodexImageResult, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var out []CodexImageResult
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Type  string          `json:"type"`
			Item  json.RawMessage `json:"item"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		switch chunk.Type {
		case "response.output_item.done":
			var item struct {
				Type   string `json:"type"`
				Result string `json:"result"`
			}
			if len(chunk.Item) == 0 || json.Unmarshal(chunk.Item, &item) != nil || item.Type != "image_generation_call" {
				continue
			}
			result := strings.TrimSpace(item.Result)
			if result != "" {
				out = append(out, CodexImageResult{Base64: stripDataURIPrefix(result)})
			}
		case "response.failed":
			if chunk.Error != nil && chunk.Error.Message != "" {
				return nil, fmt.Errorf("%s", chunk.Error.Message)
			}
			return nil, fmt.Errorf("codex response failed")
		case "response.completed":
			if len(out) == 0 {
				return nil, fmt.Errorf("no image result in response")
			}
			return out, nil
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("stream ended without an image result")
	}
	return out, nil
}

// stripDataURIPrefix turns "data:image/png;base64,<b64>" into just <b64>.
func stripDataURIPrefix(s string) string {
	if idx := strings.Index(s, ";base64,"); idx >= 0 {
		return s[idx+len(";base64,"):]
	}
	if idx := strings.Index(s, ","); idx >= 0 && strings.HasPrefix(s, "data:") {
		return s[idx+1:]
	}
	return s
}

// refreshAccessToken exchanges the refresh token for a fresh access token at
// the OpenAI auth server (the same endpoint the device flow uses to exchange
// the authorization code).
func refreshAccessToken(refreshToken string) (*codexRefreshResponse, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {codexClientID},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		codexTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("codex token refresh: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "rick-cli")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex token refresh: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex token refresh: http %d: %s", resp.StatusCode, codexSnippet(body))
	}
	var tok codexRefreshResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("codex token refresh: bad response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("codex token refresh: no access_token in response")
	}
	return &tok, nil
}

// refreshIfNeeded returns a valid access token, refreshing via the stored
// refresh token when the current one is missing, expired, or near expiry.
func (c *CodexConfig) refreshIfNeeded(accessToken string) (string, error) {
	if accessToken != "" && c.TokenExpiresAt > 0 && time.Now().Unix() < c.TokenExpiresAt-60 {
		return accessToken, nil
	}
	if c.RefreshToken == "" {
		return accessToken, nil
	}

	// Serialize refreshes: one request refreshes, the others wait for it.
	c.mu.Lock()
	if c.refreshing {
		c.mu.Unlock()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
			c.mu.Lock()
			done := !c.refreshing
			c.mu.Unlock()
			if done {
				return accessToken, nil
			}
		}
		return accessToken, nil
	}
	c.refreshing = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.refreshing = false
		c.mu.Unlock()
	}()

	tok, err := refreshAccessToken(c.RefreshToken)
	if err != nil {
		return "", err
	}
	expiresAt := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	if tok.ExpiresIn <= 0 {
		expiresAt = time.Now().Add(time.Hour).Unix()
	}
	if tok.RefreshToken != "" {
		c.RefreshToken = tok.RefreshToken
	}
	if id := catalog.CodexAccountID(tok.IDToken, tok.AccessToken); id != "" {
		c.AccountID = id
	}
	c.TokenExpiresAt = expiresAt
	if c.OnTokenRefresh != nil {
		c.OnTokenRefresh(tok.AccessToken, c.RefreshToken, expiresAt)
	}
	return tok.AccessToken, nil
}

// codexModels returns the model list from the ChatGPT backend
// (GET /codex/models → {"models":[{"slug":"gpt-5.5",...}]}).
func (c *Client) codexModels(ctx context.Context) ([]provider.ModelInfo, error) {
	token, err := c.codexToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexBaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	c.codexHeaders(req, token)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex models: http %d: %s", resp.StatusCode, codexSnippet(body))
	}
	var envelope struct {
		Models []struct {
			Slug          string `json:"slug"`
			DisplayName   string `json:"display_name"`
			ContextWindow int64  `json:"context_window"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("codex models: bad response: %w", err)
	}
	infos := make([]provider.ModelInfo, 0, len(envelope.Models))
	for _, m := range envelope.Models {
		if strings.TrimSpace(m.Slug) == "" {
			continue
		}
		ctxLen := int(m.ContextWindow)
		source := provider.ContextSourceUnknown
		if ctxLen > 0 {
			source = provider.ContextSourceAPI
		} else if known := provider.KnownContextWindow(m.Slug); known > 0 {
			ctxLen = known
			source = provider.ContextSourceInferred
		}
		infos = append(infos, provider.ModelInfo{
			ID: m.Slug, Name: catalog.FirstNonEmpty(m.DisplayName, m.Slug),
			ContextWindow: ctxLen, ContextSource: source, ChatCapable: true,
			CapabilitiesKnown: true,
		})
	}
	if len(infos) == 0 {
		return nil, fmt.Errorf("codex models: no models in response")
	}
	return infos, nil
}

// codexToken resolves the access token through the codex refresh path.
func (c *Client) codexToken(ctx context.Context) (string, error) {
	if c.Codex == nil {
		return c.APIKey, nil
	}
	token, err := c.Codex.refreshIfNeeded(c.APIKey)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", fmt.Errorf("codex: no access token")
	}
	return token, nil
}

// codexHeaders sets the headers the ChatGPT backend requires: the OAuth bearer
// plus the account id, the Responses-API beta marker and originator
// identification. The backend rejects requests missing the account header.
func (c *Client) codexHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("originator", "rick-cli")
	req.Header.Set("User-Agent", "rick-cli")
	// The backend keys first-party routing on this sku; the official Codex
	// client sends OAI-Product-Sku: codex to the chatgpt backend. Raw map
	// assignment preserves the exact casing (Header.Set would emit
	// "Oai-Product-Sku").
	req.Header["OAI-Product-Sku"] = []string{"codex"}
	accountID := ""
	if c.Codex != nil {
		accountID = c.Codex.AccountID
		if accountID == "" {
			accountID = catalog.CodexAccountID("", token)
		}
	}
	if accountID != "" {
		// Preserve the exact casing the ChatGPT backend expects. The header
		// is "ChatGPT-Account-ID" (capital "ID"); Go's Header.Set would
		// canonicalize it to "Chatgpt-Account-Id", so assign the raw map key.
		req.Header["ChatGPT-Account-ID"] = []string{accountID}
	}
}

// codexBody converts rick's request into the Responses-API wire body the
// ChatGPT backend expects. The chat-completions model (messages + tools) maps
// to Responses items: developer instructions, user/assistant messages with
// content parts, function_call + function_call_output items for tools.
func codexBody(c *Client, req provider.Request, streaming bool) ([]byte, error) {
	model := req.Model
	// Codex model ids in rick may be bare slugs already ("gpt-5.5"); strip
	// any provider prefix so the backend accepts them.
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	body := map[string]any{
		"model":               model,
		"store":               false,
		"stream":              streaming,
		"parallel_tool_calls": true,
	}
	// Explicit prompt-cache routing: a stable key (session or system scope)
	// pins every request in a conversation to the same cache bucket on the
	// ChatGPT router, exactly like the direct-OpenAI path. Without it the
	// backend derives a bucket from the volatile session headers and cache
	// hits drop well below the usual 99%+.
	if req.CacheRetention != provider.CacheRetentionNone {
		if key := promptCacheKey(req.Model, promptCacheScope(req), ""); key != "" {
			body["prompt_cache_key"] = key
		}
	}
	if strings.TrimSpace(req.System) != "" {
		body["instructions"] = req.System
	}
	if len(req.Tools) > 0 {
		body["tools"] = codexTools(req.Tools)
	}
	// Reasoning effort: map rick's level to the Codex dialect. Codex models
	// reject "none"; the official client maps "none"→"low" and "minimal"→"low".
	if req.Reasoning != "" {
		effort := string(req.Reasoning)
		switch effort {
		case "none", "minimal":
			effort = "low"
		}
		body["reasoning"] = map[string]any{"effort": effort}
	}
	// The ChatGPT Codex backend rejects max_output_tokens and
	// max_completion_tokens with "Unsupported parameter" (verified live); the
	// official Codex CLI sends no output-token limit and the model's
	// truncation policy governs.
	items, err := codexInput(req)
	if err != nil {
		return nil, err
	}
	body["input"] = items
	return json.Marshal(body)
}

// codexInput flattens the conversation onto Responses-API input items. Tool
// results from a prior turn are function_call_output items keyed by the
// matching function call's call_id.
func codexInput(req provider.Request) ([]any, error) {
	var items []any
	for _, m := range req.Messages {
		switch m.Role {
		case provider.RoleUser:
			// A user message may be a plain turn or a batch of tool results
			// from the previous assistant turn. Responses-API expects tool
			// results as function_call_output items, not inside user messages.
			if isToolResults(m) {
				for _, b := range m.Content {
					if b.Type != "tool_result" {
						continue
					}
					if b.ToolUseID == "" {
						continue
					}
					items = append(items, map[string]any{
						"type":    "function_call_output",
						"call_id": b.ToolUseID,
						"output":  b.Content,
					})
				}
				continue
			}
			content := codexContentParts(m)
			if len(content) == 0 {
				continue
			}
			items = append(items, map[string]any{
				"role":    "user",
				"content": content,
			})
		case provider.RoleAssistant:
			var content []map[string]any
			var toolCalls []map[string]any
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					content = append(content, map[string]any{"type": "output_text", "text": b.Text})
				case "thinking":
					content = append(content, map[string]any{"type": "reasoning", "text": b.Text})
				case "tool_use":
					args := strings.TrimSpace(string(b.Input))
					if args == "" {
						args = "{}"
					}
					toolCalls = append(toolCalls, map[string]any{
						"type": "function_call", "name": b.Name, "arguments": args,
						"call_id": b.ID,
					})
				}
			}
			if len(toolCalls) > 0 {
				if len(content) > 0 {
					items = append(items, map[string]any{"role": "assistant", "content": content})
				}
				for _, tc := range toolCalls {
					items = append(items, tc)
				}
			} else if len(content) > 0 {
				items = append(items, map[string]any{"role": "assistant", "content": content})
			}
		}
	}
	return items, nil
}

// isToolResults reports whether a user message is purely tool outputs from the
// previous assistant turn (the Responses API wants these as
// function_call_output items, not user content).
func isToolResults(m provider.Message) bool {
	if len(m.Content) == 0 {
		return false
	}
	for _, b := range m.Content {
		if b.Type != "tool_result" {
			return false
		}
	}
	return true
}

// codexContentParts renders a user message's content as Responses-API content
// parts (input_text + input_image).
func codexContentParts(m provider.Message) []map[string]any {
	var parts []map[string]any
	var text strings.Builder
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "image":
			if b.Source == "base64" && b.Data != "" {
				if text.Len() > 0 {
					parts = append(parts, map[string]any{"type": "input_text", "text": text.String()})
					text.Reset()
				}
				parts = append(parts, map[string]any{
					"type":      "input_image",
					"image_url": "data:" + b.MediaType + ";base64," + b.Data,
				})
			}
		}
	}
	if text.Len() > 0 {
		parts = append(parts, map[string]any{"type": "input_text", "text": text.String()})
	}
	return parts
}

// codexTools converts rick's tool schemas to Responses-API function tools.
func codexTools(ts []provider.ToolSchema) []map[string]any {
	ts = provider.CanonicalToolSchemas(ts)
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, map[string]any{
			"type":        "function",
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.InputSchema,
			"strict":      false,
		})
	}
	return out
}

// codexStream issues a streaming Responses-API request to the ChatGPT backend
// and returns the response for SSE parsing.
func (c *Client) codexStream(ctx context.Context, req provider.Request) (*http.Response, error) {
	token, err := c.codexToken(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := codexBody(c, req, true)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		codexBaseURL+"/responses", strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	c.codexHeaders(httpReq, token)
	// Session-affinity hints keep the prompt cache warm on the ChatGPT
	// router. Without session_id/x-client-request-id every request lands in a
	// fresh cache bucket and the <96% cache-hit regression appears.
	if req.CacheRetention != provider.CacheRetentionNone && req.SessionID != "" {
		httpReq.Header.Set("session_id", req.SessionID)
		httpReq.Header.Set("x-client-request-id", req.SessionID)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Minute}).Do(httpReq)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// codexItem is a Responses-API output item (message or function_call) carried
// by response.output_item.done events.
type codexItem struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	CallID    string `json:"call_id"`
}

// readCodexSSE parses the Responses-API SSE stream and emits provider events.
// Event types:
//
//	response.output_text.delta             → incremental text
//	response.reasoning_text.delta          → thinking
//	response.output_item.done              → final text / completed function call
//	response.completed                     → terminal, carries usage
//	response.failed                        → error
//
// Function calls are emitted from response.output_item.done, which carries
// the complete name + arguments (the official Codex client does the same and
// ignores the intermediate function_call_arguments deltas).
func (c *Client) readCodexSSE(ctx context.Context, r io.Reader, emit func(provider.Event) bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)

	usage := provider.Usage{}
	sawOutput := false
	completed := false

	// Emits one validated tool call. Malformed calls (empty name, invalid
	// JSON arguments) fail the stream rather than silently dropping them.
	emitToolCall := func(id, name, args string) bool {
		args = strings.TrimSpace(args)
		if args == "" {
			args = "{}"
		}
		if strings.TrimSpace(name) == "" {
			emit(provider.Event{Kind: provider.EventError,
				Err: fmt.Errorf("%s: codex tool call missing name", c.ID)})
			return false
		}
		if !json.Valid([]byte(args)) {
			emit(provider.Event{Kind: provider.EventError,
				Err: fmt.Errorf("%s: codex malformed arguments for tool %q", c.ID, name)})
			return false
		}
		sawOutput = true
		return emit(provider.Event{Kind: provider.EventToolCall, ToolCall: &provider.ToolCall{
			ID: id, Name: name, Input: json.RawMessage(args),
		}})
	}

	emitText := func(text string) bool {
		if text == "" {
			return true
		}
		if strings.TrimSpace(text) != "" {
			sawOutput = true
		}
		return emit(provider.Event{Kind: provider.EventText, Text: text})
	}

	for sc.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			completed = true
			break
		}
		var chunk struct {
			Type     string          `json:"type"`
			Delta    string          `json:"delta"`
			Item     json.RawMessage `json:"item"`
			Response json.RawMessage `json:"response"`
			Error    *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // non-JSON keep-alive or metadata line
		}
		switch chunk.Type {
		case "response.output_text.delta":
			if !emitText(chunk.Delta) {
				return
			}
		case "response.reasoning_text.delta":
			if chunk.Delta != "" {
				if strings.TrimSpace(chunk.Delta) != "" {
					sawOutput = true
				}
				if !emit(provider.Event{Kind: provider.EventThinking, Text: chunk.Delta}) {
					return
				}
			}
		case "response.output_item.done":
			var item codexItem
			if len(chunk.Item) == 0 || json.Unmarshal(chunk.Item, &item) != nil {
				continue
			}
			switch item.Type {
			case "message":
				// Text is already streamed incrementally via
				// response.output_text.delta; emitting the complete item text
				// here would duplicate every reply. The done event only
				// finalizes the item — tool calls are handled below.
			case "function_call":
				if !emitToolCall(item.CallID, item.Name, item.Arguments) {
					return
				}
			}
		case "response.completed":
			completed = true
			if len(chunk.Response) > 0 {
				var done struct {
					Usage *struct {
						InputTokens  int `json:"input_tokens"`
						OutputTokens int `json:"output_tokens"`
						InputDetails struct {
							CachedTokens     int `json:"cached_tokens"`
							CacheWriteTokens int `json:"cache_write_tokens"`
						} `json:"input_tokens_details"`
					} `json:"usage"`
				}
				_ = json.Unmarshal(chunk.Response, &done)
				if done.Usage != nil {
					usage.InputTokens = done.Usage.InputTokens - done.Usage.InputDetails.CachedTokens - done.Usage.InputDetails.CacheWriteTokens
					if usage.InputTokens < 0 {
						usage.InputTokens = 0
					}
					usage.OutputTokens = done.Usage.OutputTokens
					usage.CacheReadTokens = done.Usage.InputDetails.CachedTokens
					usage.CacheWriteTokens = done.Usage.InputDetails.CacheWriteTokens
				}
			}
		case "response.failed":
			if chunk.Error != nil && chunk.Error.Message != "" {
				emit(provider.Event{Kind: provider.EventError,
					Err: fmt.Errorf("%s: %s", c.ID, chunk.Error.Message)})
			} else {
				emit(provider.Event{Kind: provider.EventError,
					Err: fmt.Errorf("%s: codex response failed", c.ID)})
			}
			return
		}
	}

	if err := sc.Err(); err != nil && ctx.Err() == nil {
		emit(provider.Event{Kind: provider.EventError, Err: err})
		return
	}
	if !completed {
		emit(provider.Event{Kind: provider.EventError,
			Err: fmt.Errorf("%s: codex stream ended without completion", c.ID)})
		return
	}
	emit(provider.Event{Kind: provider.EventUsage, Usage: &usage})
	if !sawOutput {
		emit(provider.Event{Kind: provider.EventError,
			Err: fmt.Errorf("%s: codex empty completion: no text, reasoning, or tool calls", c.ID)})
		return
	}
	emit(provider.Event{Kind: provider.EventDone})
}

// codexSnippet truncates a response body for error messages.
func codexSnippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
