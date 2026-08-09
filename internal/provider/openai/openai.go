// Package openai implements provider.Provider against the OpenAI
// chat-completions wire format, which OpenRouter, Groq, Together, LM Studio,
// Ollama and most gateways also speak.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"rick/internal/provider"
	"rick/internal/provider/catalog"
)

// Client is an OpenAI-compatible provider.
type Client struct {
	ID      string // registry name: "openai", "openrouter", ...
	APIKey  string
	BaseURL string
	Headers map[string]string
	HTTP    *http.Client

	models []provider.ModelInfo

	// Keep-alive state (SetKeepalive): per-session record of the last stream
	// body so an idle gap can refresh the provider's prompt cache with the
	// exact prompt bytes the next real request will extend. Zero interval
	// leaves all of this inert.
	kaMu       sync.Mutex
	kaSessions map[string]*kaSession
	kaStop     chan struct{}
	kaOnce     sync.Once
	kaInterval time.Duration
}

// kaSession remembers the last stream-shaped wire body per session so the
// keep-alive loop can re-send the same prompt prefix while the user idles.
type kaSession struct {
	body    wireRequest
	headers http.Header
	last    time.Time
	// inFlight is true while a real stream (or a keep-alive POST) for this
	// session is on the wire, so the loop never double-fires or races a
	// mid-turn stale body against the live stream.
	inFlight bool
}

// New builds a client. baseURL defaults to the public OpenAI API.
func New(id, apiKey, baseURL string) *Client {
	if baseURL == "" {
		switch id {
		case "openrouter":
			baseURL = "https://openrouter.ai/api/v1"
		case "groq":
			baseURL = "https://api.groq.com/openai/v1"
		default:
			baseURL = "https://api.openai.com/v1"
		}
	}
	c := &Client{
		ID:      id,
		APIKey:  catalog.CleanSecret(apiKey),
		BaseURL: strings.TrimRight(strings.ReplaceAll(baseURL, "\x00", ""), "/"),
		HTTP:    &http.Client{Timeout: 15 * time.Minute},
		Headers: map[string]string{},
	}
	if id == "openrouter" {
		c.Headers["HTTP-Referer"] = "https://github.com/rick-agent/rick"
		c.Headers["X-Title"] = "rick"
	}
	c.models = defaultModels(id)
	return c
}

// SetKeepalive enables the prompt-cache keep-alive loop with the given idle
// interval. After a session has been silent for the interval, the exact last
// stream body is re-sent as a minimal stream-shaped request so the provider
// keeps the session's prefix cached across idle gaps. Zero disables it; the
// loop is best-effort and never surfaces errors (same contract as Warm).
// Must be called before the first request for the interval to take effect.
func (c *Client) SetKeepalive(interval time.Duration) {
	if interval <= 0 {
		return
	}
	c.kaInterval = interval
}

// noteKeepalive records the stream body that just went out so the keep-alive
// loop can re-send the same prefix later. Local endpoints have no provider
// cache, and retention "none" explicitly opted out of caching. The session is
// marked in-flight; finishKeepalive clears it when the stream completes.
func (c *Client) noteKeepalive(req provider.Request, body wireRequest, headers http.Header) {
	if c.kaInterval <= 0 || req.SessionID == "" ||
		req.CacheRetention == provider.CacheRetentionNone || isLocal(c.BaseURL) {
		return
	}
	c.ensureKeepalive()
	c.kaMu.Lock()
	defer c.kaMu.Unlock()
	s, ok := c.kaSessions[req.SessionID]
	if !ok {
		s = &kaSession{}
		c.kaSessions[req.SessionID] = s
	}
	s.body = body
	s.headers = headers.Clone()
	s.inFlight = true
}

// finishKeepalive marks the session idle again after a stream completes and
// records the completion time, so the next keep-alive waits a full interval
// from the turn that just primed the cache.
func (c *Client) finishKeepalive(sessionID string) {
	if c.kaInterval <= 0 || sessionID == "" {
		return
	}
	c.kaMu.Lock()
	if s, ok := c.kaSessions[sessionID]; ok {
		s.inFlight = false
		s.last = time.Now()
	}
	c.kaMu.Unlock()
}

// finishKeepaliveFor is the keep-alive-send variant: it only clears the
// in-flight flag when the map still holds the captured session, so a stale
// keep-alive completing after a newer stream replaced the entry cannot clear
// the live stream's in-flight marker.
func (c *Client) finishKeepaliveFor(sessionID string, s *kaSession) {
	c.kaMu.Lock()
	if cur, ok := c.kaSessions[sessionID]; ok && cur == s {
		s.inFlight = false
		s.last = time.Now()
	}
	c.kaMu.Unlock()
}

// touchKeepalive bumps the session's last-activity time without replacing the
// recorded body (used after a successful pre-turn warm, which already primed
// the cache).
func (c *Client) touchKeepalive(sessionID string) {
	if c.kaInterval <= 0 || sessionID == "" {
		return
	}
	c.kaMu.Lock()
	if s, ok := c.kaSessions[sessionID]; ok {
		s.last = time.Now()
	}
	c.kaMu.Unlock()
}

func (c *Client) ensureKeepalive() {
	c.kaOnce.Do(func() {
		c.kaMu.Lock()
		c.kaSessions = map[string]*kaSession{}
		c.kaMu.Unlock()
		c.kaStop = make(chan struct{})
		go c.keepaliveLoop()
	})
}

// stopKeepalive halts the loop. Production relies on process lifetime; tests
// call this to avoid leaking the ticker goroutine.
func (c *Client) stopKeepalive() {
	c.kaOnce.Do(func() {}) // mark the once so no new loop starts
	if c.kaStop != nil {
		close(c.kaStop)
	}
}

func (c *Client) keepaliveLoop() {
	period := c.kaInterval / 2
	if period < time.Second {
		period = time.Second
	}
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-c.kaStop:
			return
		case <-t.C:
			c.keepaliveTick()
		}
	}
}

// keepaliveTick re-sends stale session bodies. Only sessions idle past the
// interval with no stream in flight are touched, so an active turn never
// races a keep-alive. Sessions idle over a day are pruned.
func (c *Client) keepaliveTick() {
	c.kaMu.Lock()
	now := time.Now()
	type due struct {
		sessionID string
		s         *kaSession
	}
	var toSend []due
	for sid, s := range c.kaSessions {
		if idle := now.Sub(s.last); idle > c.kaInterval {
			if !s.inFlight {
				s.inFlight = true // held until the POST below completes
				toSend = append(toSend, due{sid, s})
			}
		} else if idle > 24*time.Hour {
			// Prune long-dead sessions so the map cannot grow unbounded.
			delete(c.kaSessions, sid)
		}
	}
	c.kaMu.Unlock()
	for _, d := range toSend {
		c.keepaliveSend(d.sessionID, d.s)
	}
}

// keepaliveSend re-POSTs a session's last stream body reshaped to a minimal
// stream-shaped request: same messages/tools/model, tiny output budget, so it
// rides (and refreshes) the exact prompt-cache entry the next real stream
// will extend. Success bumps the session's last-activity time; any failure
// clears in-flight so the next tick retries.
func (c *Client) keepaliveSend(sessionID string, s *kaSession) {
	body := s.body
	body.Stream = true
	body.StreamOpts = nil
	if deepseekWire(c.ID) {
		body.MaxTokens = 0
		body.MaxTokensLegacy = 1
	} else {
		body.MaxTokens = 1
	}
	raw, err := json.Marshal(body)
	if err != nil {
		c.finishKeepaliveFor(sessionID, s)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := c.doCompletions(ctx, raw, s.headers)
	if err != nil {
		c.finishKeepaliveFor(sessionID, s)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	resp.Body.Close()
	c.finishKeepaliveFor(sessionID, s)
}

func defaultModels(id string) []provider.ModelInfo {
	switch id {
	case "openrouter":
		return []provider.ModelInfo{
			{ID: "anthropic/claude-sonnet-4.5", Name: "Claude Sonnet 4.5", ContextWindow: 200000, MaxOutput: 64000},
			{ID: "openai/gpt-5", Name: "GPT-5", ContextWindow: 400000, MaxOutput: 128000},
			{ID: "google/gemini-2.5-pro", Name: "Gemini 2.5 Pro", ContextWindow: 1000000, MaxOutput: 65000},
			{ID: "deepseek/deepseek-chat", Name: "DeepSeek Chat", ContextWindow: 128000, MaxOutput: 8192},
			{ID: "qwen/qwen3-coder", Name: "Qwen3 Coder", ContextWindow: 256000, MaxOutput: 32000},
		}
	case "deepseek":
		return []provider.ModelInfo{
			{ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash", ContextWindow: 1_000_000, MaxOutput: 384_000},
			{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", ContextWindow: 1_000_000, MaxOutput: 384_000},
			{ID: "deepseek-chat", Name: "DeepSeek Chat", ContextWindow: 128000, MaxOutput: 8192},
			{ID: "deepseek-reasoner", Name: "DeepSeek Reasoner", ContextWindow: 128000, MaxOutput: 8192},
		}
	case "opencode-zen", "opencode-go":
		return []provider.ModelInfo{
			{ID: "deepseek-v4-flash-free", Name: "DeepSeek V4 Flash Free", ContextWindow: 200_000, MaxOutput: 32_000},
			{ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash", ContextWindow: 1_000_000, MaxOutput: 384_000},
		}
	default:
		// No static catalogue for arbitrary/custom providers. A provider that
		// has not been probed yet has no known models — advertising OpenAI
		// placeholders here (gpt-5, gpt-4.1, o4-mini) would list models the
		// endpoint does not serve. The real list comes from the /models probe
		// (auth.json -> SetModels); until then an empty list is honest and
		// triggers the lazy reload on the next start.
		return nil
	}
}

// Name implements provider.Provider.
func (c *Client) Name() string { return c.ID }

// Models implements provider.Provider.
func (c *Client) Models() []provider.ModelInfo { return c.models }

// SetModels overrides the advertised catalogue.
func (c *Client) SetModels(m []provider.ModelInfo) {
	if len(m) > 0 {
		c.models = m
	}
}

func (c *Client) modelInfo(id string) *provider.ModelInfo {
	for _, model := range c.models {
		if model.ID == id {
			copy := model
			return &copy
		}
	}
	return nil
}

// SetAPIKey updates the API key for this client.
// deepseekWire reports whether the endpoint speaks DeepSeek's wire dialect,
// which reads max_tokens (and thinking switches) rather than OpenAI's
// max_completion_tokens. Zen's free-tier engines are DeepSeek-line.
func deepseekWire(id string) bool {
	return id == "opencode-zen" || id == "opencode-go" || id == "deepseek"
}

func (c *Client) SetAPIKey(key string) {
	c.APIKey = key
}

// ---------- wire types ----------

type wireToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type wireImageContent struct {
	Type     string       `json:"type"`
	ImageURL wireImageURL `json:"image_url"`
}

type wireImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type wireMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content,omitempty"`
	ReasoningContent *string        `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type wireRequest struct {
	Model      string        `json:"model"`
	Messages   []wireMessage `json:"messages"`
	Tools      []wireTool    `json:"tools,omitempty"`
	Stream     bool          `json:"stream"`
	StreamOpts *streamOpts   `json:"stream_options,omitempty"`
	MaxTokens  int           `json:"max_completion_tokens,omitempty"`
	// MaxTokensLegacy is the output budget for endpoints that read
	// DeepSeek's max_tokens instead of OpenAI's max_completion_tokens.
	MaxTokensLegacy int      `json:"max_tokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	PromptCacheKey  string   `json:"prompt_cache_key,omitempty"`
	// PromptCacheRetention is the OpenAI chat-completions cache-retention
	// hint ("24h" for long retention). Gateway providers ignore it.
	PromptCacheRetention string `json:"prompt_cache_retention,omitempty"`
	// Effort is OpenAI's reasoning control; Qwen-style endpoints use the
	// boolean instead. Only one is ever set.
	Effort         string         `json:"reasoning_effort,omitempty"`
	EnableThinking *bool          `json:"enable_thinking,omitempty"`
	Thinking       *wireThinking  `json:"thinking,omitempty"`
	Reasoning      *wireReasoning `json:"reasoning,omitempty"`
}

// promptCacheKey derives a session-scoped cache key. The session id is stable
// across resume, so a restarted session keeps hitting the same cache even if
// the volatile system tail drifts between sessions.
func promptCacheKey(model, sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(model + "\x00" + sessionID))
	return hex.EncodeToString(digest[:])
}

// wireThinking is GLM's native OpenAI-compatible thinking switch.
type wireThinking struct {
	Type          string `json:"type"`
	ClearThinking *bool  `json:"clear_thinking,omitempty"`
}

type wireReasoning struct {
	Effort    string `json:"effort,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	Enabled   *bool  `json:"enabled,omitempty"`
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

// toWire flattens rick's block model onto OpenAI's message model.
func toWire(system string, msgs []provider.Message) []wireMessage {
	return toWireWithReasoning(system, msgs, false, false)
}

// toWireWithReasoning preserves reasoning_content for providers such as GLM
// and DeepSeek that require it when a tool call is followed by another turn.
// retainAllReasoning keeps reasoning for every assistant turn (append-only) so
// the serialized prompt stays a strict prefix of the next request — DeepSeek's
// automatic prefix cache then hits the whole stable history every turn.
func toWireWithReasoning(system string, msgs []provider.Message, includeReasoning, retainAllReasoning bool) []wireMessage {
	return toWireWithStable(system, "", msgs, includeReasoning, retainAllReasoning)
}

// hasThinkingBlocks reports whether any prior turn produced reasoning that a
// DeepSeek-style endpoint will demand be echoed back with reasoning_content.
func hasThinkingBlocks(msgs []provider.Message) bool {
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == "thinking" {
				return true
			}
		}
	}
	return false
}

// wireReasoning decides whether the wire serializer preserves and retains
// reasoning_content for the given request. Stream and Warm must use the same
// decision: a warm request that strips reasoning primes different bytes than
// the real append-only stream, so the provider's automatic prefix cache would
// never hit on the reasoning turns that follow.
func (c *Client) wireReasoning(req provider.Request) (style provider.ReasoningStyle, preserveReasoning, retainAllReasoning bool) {
	style, _ = provider.DetectReasoningForProvider(c.ID, req.Model)
	advertised := c.modelInfo(req.Model)
	preserve := style == provider.ReasoningStyleGLM || style == provider.ReasoningStyleDeepSeek ||
		style == provider.ReasoningStyleAlways || style == provider.ReasoningStyleQwen ||
		style == provider.ReasoningStyleUnknown
	if c.ID == "openrouter" && advertised != nil && advertised.ReasoningKnown {
		preserve = true
	}
	// OpenCode Zen/Go serve OpenAI-compatible reasoning models built on
	// DeepSeek-style thinking, which require the provider to echo the prior
	// turn's reasoning_content back in the next request. A model name here
	// rarely maps to a DeepSeek dialect (names cluster around gpt/gemini), so
	// force preservation so the endpoint never rejects the exchange with a
	// "reasoning_content must be passed back" 400.
	if c.ID == "opencode-zen" || c.ID == "opencode-go" {
		preserve = true
	}
	// Deciding to strip reasoning from the wire must never depend on guessing
	// the provider or model dialect. If any prior turn produced thinking, the
	// DeepSeek-style endpoint requires that reasoning_content be echoed back
	// verbatim on the next request — otherwise it rejects the exchange with a
	// "reasoning_content must be passed back" 400. Check the actual history so
	// we only ever preserve what genuinely exists.
	if hasThinkingBlocks(req.Messages) {
		preserve = true
	}
	// DeepSeek-line endpoints (Zen/Go gateways build on DeepSeek-style thinking)
	// get an append-only prompt: every turn keeps all of its reasoning instead
	// of stripping to the most-recent window. That keeps the serialized prefix
	// byte-identical across turns so the provider's automatic prefix cache hits,
	// instead of re-billing the whole tail every turn. Other reasoning
	// providers keep the token-saving strip.
	retainAll := req.MaxReasoningTurns <= 0 &&
		(c.ID == "opencode-zen" || c.ID == "opencode-go" ||
			style == provider.ReasoningStyleDeepSeek ||
			(hasThinkingBlocks(req.Messages) && (c.ID == "deepseek" || c.ID == "openrouter")))
	return style, preserve, retainAll
}

// toWireWithStable keeps the stable prompt in an earlier message than the
// per-turn tail. Direct OpenAI caching can then retain the stable prefix while
// the volatile environment and skill instructions continue to be sent.
func toWireWithStable(system, stable string, msgs []provider.Message, includeReasoning, retainAllReasoning bool) []wireMessage {
	var out []wireMessage
	if strings.TrimSpace(stable) != "" && strings.HasPrefix(system, stable) {
		out = append(out, wireMessage{Role: "system", Content: stable})
		if tail := strings.TrimPrefix(system, stable); strings.TrimSpace(tail) != "" {
			out = append(out, wireMessage{Role: "system", Content: tail})
		}
	} else if strings.TrimSpace(system) != "" {
		out = append(out, wireMessage{Role: "system", Content: system})
	}

	// DeepSeek/GLM require the *immediately previous* turn's reasoning echoed
	// back to continue a tool exchange. Reasoning from older turns is normally
	// dead weight: it multiplies the prompt and spikes CPU when compaction
	// resends the whole head, so by default only the most recent thinking
	// assistant message keeps its value (older turns carry an empty
	// reasoning_content so the endpoint never rejects them).
	//
	// For DeepSeek-line providers that preserve the reasoning, that stripping
	// also *breaks the provider cache*: the "previous" assistant message flips
	// from a full reasoning value to an empty one as the window moves, so the
	// serialized prefix changes in the middle of the prompt and every turn
	// re-bills the whole tail (DeepSeek's automatic cache is prefix-based and
	// only hits a byte-identical prefix). Retaining every reasoning block is
	// append-only — the prompt only grows at the end — so the cache stays hot
	// and cached reasoning is billed at DeepSeek's discounted 0.1x rate.
	lastThinking := -1
	if includeReasoning && !retainAllReasoning {
		for i := len(msgs) - 1; i >= 0; i-- {
			for _, b := range msgs[i].Content {
				if b.Type == "thinking" && strings.TrimSpace(b.Text) != "" {
					lastThinking = i
					break
				}
			}
			if lastThinking >= 0 {
				break
			}
		}
	}

	for i, m := range msgs {
		var text strings.Builder
		var reasoning strings.Builder
		var calls []wireToolCall
		var results []wireMessage
		var imageBlocks []wireImageContent

		keeper := includeReasoning && (retainAllReasoning || i == lastThinking)

		for _, b := range m.Content {
			switch b.Type {
			case "text":
				text.WriteString(b.Text)
			case "thinking":
				if keeper {
					reasoning.WriteString(b.Text)
				}
			case "tool_use":
				var tc wireToolCall
				tc.ID = b.ID
				tc.Type = "function"
				tc.Function.Name = b.Name
				args := string(b.Input)
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				tc.Function.Arguments = args
				calls = append(calls, tc)
			case "tool_result":
				results = append(results, wireMessage{
					Role: "tool", ToolCallID: b.ToolUseID, Content: b.Content,
				})
			case "image":
				if b.Source == "base64" && b.Data != "" {
					imageBlocks = append(imageBlocks, wireImageContent{
						Type: "image_url",
						ImageURL: wireImageURL{
							URL:    "data:" + b.MediaType + ";base64," + b.Data,
							Detail: "low",
						},
					})
				}
			}
		}

		if m.Role == provider.RoleAssistant && (text.Len() > 0 || reasoning.Len() > 0 || len(calls) > 0) {
			wm := wireMessage{Role: "assistant", ToolCalls: calls}
			if text.Len() > 0 {
				wm.Content = text.String()
			}
			if reasoning.Len() > 0 || includeReasoning && len(calls) > 0 {
				value := reasoning.String()
				wm.ReasoningContent = &value
			}
			// OpenAI-compatible endpoints (OpenCode/Zen, GLM, DeepSeek) reject
			// an assistant message with neither "content" nor "tool_calls"
			// set. A turn that produced only thinking (e.g. a truncated
			// "continue" exchange) would otherwise serialize to an empty
			// assistant block and fail the whole request with a 400, forcing
			// an uncached retry. If we have nothing else, fall back to the
			// echoed reasoning as content so the message is always sendable.
			if len(calls) == 0 && wm.Content == nil && wm.ReasoningContent != nil {
				wm.Content = *wm.ReasoningContent
			}
			out = append(out, wm)
		} else if m.Role == provider.RoleUser && (text.Len() > 0 || len(imageBlocks) > 0) {
			wm := wireMessage{Role: "user", Content: text.String()}
			if len(imageBlocks) > 0 {
				// OpenAI vision: content is an array of text + image_url blocks
				var contentArray []map[string]interface{}
				if text.Len() > 0 {
					contentArray = append(contentArray, map[string]interface{}{
						"type": "text",
						"text": text.String(),
					})
				}
				for _, img := range imageBlocks {
					contentArray = append(contentArray, map[string]interface{}{
						"type":      img.Type,
						"image_url": img.ImageURL,
					})
				}
				wm.Content = contentArray
			}
			out = append(out, wm)
		}
		out = append(out, results...)
	}
	return out
}

func toWireTools(ts []provider.ToolSchema) []wireTool {
	ts = provider.CanonicalToolSchemas(ts)
	out := make([]wireTool, 0, len(ts))
	for _, t := range ts {
		var wt wireTool
		wt.Type = "function"
		wt.Function.Name = t.Name
		wt.Function.Description = t.Description
		wt.Function.Parameters = t.InputSchema
		out = append(out, wt)
	}
	return out
}

// completionAttempts bounds how many times a request is retried before the
// transport error is surfaced. Gateways such as OpenCode Zen/DeepSeek
// intermittently drop keep-alive connections mid-request ("unexpected EOF")
// under load; a fresh attempt usually succeeds and avoids aborting a turn.
const completionAttempts = 3

// retryableTransportError reports whether a request failed in a way that is
// safe to retry. TLS/authorization failures, cancellations and deadline
// exceedances are never retried.
func retryableTransportError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	for _, target := range []error{
		syscall.ECONNRESET, syscall.EPIPE, syscall.ECONNREFUSED,
		syscall.ETIMEDOUT, syscall.ENETDOWN, syscall.EHOSTUNREACH,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	// Some wrappers lose the error identity; fall back on the message.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "server closed idle connection")
}

func retryBackoff(attempt int) time.Duration {
	// 250ms then 1s: short enough not to stall turns, long enough to let a
	// flaky gateway settle.
	if attempt == 1 {
		return 250 * time.Millisecond
	}
	return time.Second
}

// doCompletions POSTs raw to the chat/completions endpoint, rebuilding the
// request on every attempt (a dropped connection may have consumed the body)
// and retrying transient failures before the response starts.
func (c *Client) doCompletions(ctx context.Context, raw []byte, extraHeaders http.Header) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < completionAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(retryBackoff(attempt)):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.BaseURL+"/chat/completions", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("content-type", "application/json")
		if c.APIKey != "" {
			httpReq.Header.Set("authorization", "Bearer "+c.APIKey)
		}
		for k, v := range c.Headers {
			httpReq.Header.Set(k, v)
		}
		for k, vs := range extraHeaders {
			for _, v := range vs {
				httpReq.Header.Add(k, v)
			}
		}
		resp, err := c.HTTP.Do(httpReq)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryableTransportError(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

// Warm implements provider.CacheWarmber. It submits a tiny non-streaming
// request carrying the same stable system + tools + prior transcript so the
// provider populates its automatic prefix cache before the first real turn.
// The body is built by the same builder as Stream (including the reasoning
// dialect and cache-control fields) so the warm primes byte-identical prefix
// bytes; only the output budget differs (1 token). The response is discarded.
// Any error is returned but treated as best-effort by the caller — a failed
// warm simply means the first turn stays cold.
func (c *Client) Warm(ctx context.Context, req provider.Request) error {
	if c.APIKey == "" && !isLocal(c.BaseURL) {
		return fmt.Errorf("%s: no API key configured", c.ID)
	}
	msgs := req.Messages
	if len(msgs) == 0 {
		msgs = []provider.Message{provider.UserText("ack")}
	}
	// Prime the exact bytes the real stream will send: the same reasoning
	// retention decision as Stream, so the warm's prefix matches the
	// append-only prompt that follows and the automatic cache actually hits.
	// Cache-routing hints and keys come from the shared builder unchanged —
	// a warm that sends a retention hint the stream would not send primes a
	// different cache entry and misses.
	req.Messages = msgs
	body := c.buildWireBody(req, false, false)
	if deepseekWire(c.ID) {
		body.MaxTokens = 0
		body.MaxTokensLegacy = 1
	} else {
		body.MaxTokens = 1
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	extraHeaders := http.Header{}
	if req.SessionID != "" {
		switch {
		case c.ID == "openrouter" || strings.Contains(c.BaseURL, "openrouter.ai"):
			extraHeaders.Set("x-session-id", req.SessionID)
		case c.ID == "openai" || strings.Contains(c.BaseURL, "api.openai.com"):
			extraHeaders.Set("session_id", req.SessionID)
			extraHeaders.Set("x-client-request-id", req.SessionID)
			extraHeaders.Set("x-session-affinity", req.SessionID)
		}
	}
	resp, err := c.doCompletions(ctx, raw, extraHeaders)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("%s: warm http %d: %s", c.ID, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	// A successful warm just primed this session's prefix; let the keep-alive
	// loop know so it does not also fire during the same idle gap.
	c.touchKeepalive(req.SessionID)
	return nil
}

// Stream implements provider.Provider. It owns ch and closes it exactly once.
// buildWireBody assembles the provider request body. Warm and Stream must
// produce byte-identical prefixes — a warm that primes a different request
// than the stream sends cannot serve the automatic prefix cache, so every
// turn start would re-bill cold. The only permitted differences are the
// streaming flag, the output budget, and the usage callback flag.
func (c *Client) buildWireBody(req provider.Request, streaming, includeUsage bool) wireRequest {
	style, preserveReasoning, retainAllReasoning := c.wireReasoning(req)
	body := wireRequest{
		Model:          req.Model,
		Messages:       toWireWithReasoning(req.System, req.Messages, preserveReasoning, retainAllReasoning),
		Tools:          toWireTools(req.Tools),
		Stream:         streaming,
		Temperature:    req.Temperature,
		PromptCacheKey: promptCacheKey(req.Model, req.SessionID),
	}
	if streaming {
		body.StreamOpts = &streamOpts{IncludeUsage: includeUsage}
	}
	if deepseekWire(c.ID) {
		// DeepSeek-line endpoints read max_tokens, not max_completion_tokens.
		body.MaxTokens = 0
		body.MaxTokensLegacy = req.MaxTokens
	} else {
		body.MaxTokens = req.MaxTokens
	}
	if c.ID == "openai" || deepseekWire(c.ID) {
		body.Messages = toWireWithStable(req.System, req.SystemStable, req.Messages, preserveReasoning, retainAllReasoning)
	}
	if c.ID != "openai" {
		// OpenAI-compatible gateways do not all accept OpenAI's cache-routing
		// hint. The stable system prefix is still sent to every provider.
		body.PromptCacheKey = ""
	}
	switch req.CacheRetention {
	case provider.CacheRetentionNone:
		// "none" omits the cache key and retention hint so the one-off call
		// (distillation, compaction) never reads or writes the session cache.
		body.PromptCacheKey = ""
	case provider.CacheRetentionLong:
		if c.ID == "openai" {
			body.PromptCacheRetention = "24h"
		}
	}

	// OpenRouter has one normalized reasoning object. Use it for every
	// explicitly selected effort so model-specific metadata is not translated
	// into a potentially unsupported root reasoning_effort field.
	if c.ID == "openrouter" && style != provider.ReasoningStyleAlways && req.Reasoning != "" &&
		(req.Reasoning != provider.ReasoningOff || style != provider.ReasoningStyleNone && style != provider.ReasoningStyleUnknown) {
		body.Reasoning = &wireReasoning{}
		if req.Reasoning == provider.ReasoningOn {
			on := true
			body.Reasoning.Enabled = &on
		} else {
			body.Reasoning.Effort = map[provider.ReasoningEffort]string{
				provider.ReasoningOff: "none",
			}[req.Reasoning]
			if body.Reasoning.Effort == "" {
				body.Reasoning.Effort = string(req.Reasoning)
			}
		}
		body.Temperature = nil
	} else if req.Reasoning != "" && req.Reasoning != provider.ReasoningOff {
		// Direct providers use their native reasoning dialect.
		switch style {
		case provider.ReasoningStyleOpenAI:
			effort := req.Reasoning
			if effort == provider.ReasoningOn {
				// Unknown/gateway-normalized models use the common medium
				// effort as their explicit opt-in.
				effort = provider.ReasoningMedium
			}
			body.Effort = string(effort)
			// Reasoning models reject a custom temperature.
			body.Temperature = nil
		case provider.ReasoningStyleQwen:
			on := true
			body.EnableThinking = &on
		case provider.ReasoningStyleGLM:
			clearThinking := false
			body.Thinking = &wireThinking{Type: "enabled", ClearThinking: &clearThinking}
			if req.Reasoning != provider.ReasoningOn && strings.Contains(strings.ToLower(req.Model), "glm-5.2") {
				body.Effort = string(req.Reasoning)
			}
			// Reasoning models reject a custom temperature.
			body.Temperature = nil
		case provider.ReasoningStyleDeepSeek:
			body.Effort = string(req.Reasoning)
			body.Thinking = &wireThinking{Type: "enabled"}
			// Reasoning models reject a custom temperature.
			body.Temperature = nil
		case provider.ReasoningStyleUnknown:
			// Custom gateways often omit capability metadata. Only use the
			// generic OpenAI field after the user explicitly enables thinking;
			// the default off path never sends an unsupported parameter.
			effort := req.Reasoning
			if effort == provider.ReasoningOn {
				effort = provider.ReasoningMedium
			}
			body.Effort = string(effort)
			body.Temperature = nil
		}
	}
	return body
}

func (c *Client) Stream(ctx context.Context, req provider.Request, ch chan<- provider.Event) {
	defer close(ch)

	emit := func(ev provider.Event) bool {
		select {
		case ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	if c.APIKey == "" && !isLocal(c.BaseURL) {
		emit(provider.Event{Kind: provider.EventError,
			Err: fmt.Errorf("%s: no API key configured", c.ID)})
		return
	}

	body := c.buildWireBody(req, true, true)

	raw, err := json.Marshal(body)
	if err != nil {
		emit(provider.Event{Kind: provider.EventError, Err: err})
		return
	}

	extraHeaders := http.Header{"accept": {"text/event-stream"}}
	// Session-affinity hints keep the prompt cache warm on the provider's
	// router: direct OpenAI uses session_id/x-client-request-id (plus the
	// legacy x-session-affinity), OpenRouter uses x-session-id. One-off calls
	// (retention none) skip the hints so they never ride a session's cache.
	if req.CacheRetention != provider.CacheRetentionNone && req.SessionID != "" {
		switch {
		case c.ID == "openrouter" || strings.Contains(c.BaseURL, "openrouter.ai"):
			extraHeaders.Set("x-session-id", req.SessionID)
		case c.ID == "openai" || strings.Contains(c.BaseURL, "api.openai.com") ||
			c.ID == "opencode-zen" || c.ID == "opencode-go" ||
			strings.Contains(c.BaseURL, "opencode.ai"):
			extraHeaders.Set("session_id", req.SessionID)
			extraHeaders.Set("x-client-request-id", req.SessionID)
			extraHeaders.Set("x-session-affinity", req.SessionID)
		}
	}

	// Record this stream body so the keep-alive loop can re-send the same
	// prefix while the user idles instead of paying a full cold re-bill.
	c.noteKeepalive(req, body, extraHeaders)
	defer c.finishKeepalive(req.SessionID)

	resp, err := c.doCompletions(ctx, raw, extraHeaders)
	if err != nil {
		emit(provider.Event{Kind: provider.EventError, Err: err})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		emit(provider.Event{Kind: provider.EventError,
			Err: fmt.Errorf("%s: http %d: %s", c.ID, resp.StatusCode, strings.TrimSpace(string(b)))})
		return
	}

	c.readSSE(ctx, resp.Body, emit)
}

func isLocal(u string) bool {
	return strings.Contains(u, "localhost") || strings.Contains(u, "127.0.0.1")
}

type callAccum struct {
	id   string
	name string
	args strings.Builder
}

func (c *Client) readSSE(ctx context.Context, r io.Reader, emit func(provider.Event) bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)

	calls := map[int]*callAccum{}
	usage := provider.Usage{}
	stopReason := ""
	sawOutput := false
	completed := false

	flushCalls := func() bool {
		// Validate the complete batch before emitting any call. This preserves
		// all-or-nothing semantics when a later call in the batch is malformed.
		indices := make([]int, 0, len(calls))
		for index := range calls {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		validatedCalls := make([]provider.ToolCall, 0, len(indices))
		seenIDs := make(map[string]struct{}, len(indices))
		for _, index := range indices {
			acc := calls[index]
			args := strings.TrimSpace(acc.args.String())
			if args == "" {
				args = "{}"
			}
			if strings.TrimSpace(acc.name) == "" {
				emit(provider.Event{Kind: provider.EventError,
					Err: fmt.Errorf("%s: malformed tool call at index %d: missing function name", c.ID, index)})
				return false
			}
			if !json.Valid([]byte(args)) {
				emit(provider.Event{Kind: provider.EventError,
					Err: fmt.Errorf("%s: malformed arguments for tool %q at index %d", c.ID, acc.name, index)})
				return false
			}
			if args[0] != '{' {
				emit(provider.Event{Kind: provider.EventError,
					Err: fmt.Errorf("%s: arguments for tool %q at index %d must be a JSON object", c.ID, acc.name, index)})
				return false
			}
			if acc.id != "" {
				if _, duplicate := seenIDs[acc.id]; duplicate {
					emit(provider.Event{Kind: provider.EventError,
						Err: fmt.Errorf("%s: duplicate tool call ID %q", c.ID, acc.id)})
					return false
				}
				seenIDs[acc.id] = struct{}{}
			}
			validatedCalls = append(validatedCalls,
				provider.ToolCall{ID: acc.id, Name: acc.name, Input: json.RawMessage(args)})
		}
		for _, index := range indices {
			delete(calls, index)
		}
		for index := range validatedCalls {
			if !emit(provider.Event{Kind: provider.EventToolCall, ToolCall: &validatedCalls[index]}) {
				return false
			}
		}
		return true
	}
	emitAssistantText := func(text string) bool {
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
			Choices []struct {
				Delta struct {
					Content          string         `json:"content"`
					Refusal          string         `json:"refusal"`
					Reasoning        string         `json:"reasoning"`
					ReasoningContent string         `json:"reasoning_content"`
					ToolCalls        []wireToolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				PromptTokensDetails struct {
					CachedTokens     int `json:"cached_tokens"`
					CacheWriteTokens int `json:"cache_write_tokens"`
				} `json:"prompt_tokens_details"`
				PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			emit(provider.Event{Kind: provider.EventError,
				Err: fmt.Errorf("%s: malformed SSE data: %w", c.ID, err)})
			return
		}
		if chunk.Error != nil {
			emit(provider.Event{Kind: provider.EventError,
				Err: fmt.Errorf("%s: %s", c.ID, chunk.Error.Message)})
			return
		}
		if chunk.Usage != nil {
			// cached_tokens is the cache-read count; OpenRouter-compatible
			// providers may additionally report cache_write_tokens. OpenAI
			// does not document writes, so never subtract reads from the
			// write bucket (pi/OpenRouter contract).
			cacheRead := chunk.Usage.PromptTokensDetails.CachedTokens
			if cacheRead == 0 {
				cacheRead = chunk.Usage.PromptCacheHitTokens
			}
			cacheWrite := chunk.Usage.PromptTokensDetails.CacheWriteTokens
			input := chunk.Usage.PromptTokens - cacheRead - cacheWrite
			if input < 0 {
				input = 0
			}
			usage.InputTokens = input
			usage.OutputTokens = chunk.Usage.CompletionTokens
			usage.CacheReadTokens = cacheRead
			usage.CacheWriteTokens = cacheWrite
		}
		for _, choice := range chunk.Choices {
			if !emitAssistantText(choice.Delta.Content) {
				return
			}
			if !emitAssistantText(choice.Delta.Refusal) {
				return
			}
			if t := choice.Delta.Reasoning + choice.Delta.ReasoningContent; t != "" {
				if strings.TrimSpace(t) != "" {
					sawOutput = true
				}
				if !emit(provider.Event{Kind: provider.EventThinking, Text: t}) {
					return
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				sawOutput = true
				if existing, ok := calls[tc.Index]; ok && tc.ID != "" && existing.id != "" && existing.id != tc.ID {
					if !flushCalls() {
						return
					}
				}
				acc, ok := calls[tc.Index]
				if !ok {
					acc = &callAccum{}
					calls[tc.Index] = acc
					if tc.ID != "" {
						acc.id = tc.ID
					}
					if tc.Function.Name != "" {
						acc.name = tc.Function.Name
						emit(provider.Event{Kind: provider.EventToolCallStart,
							ToolCall: &provider.ToolCall{ID: tc.ID, Name: tc.Function.Name}})
					}
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" && acc.name == "" {
					acc.name = tc.Function.Name
				}
				acc.args.WriteString(tc.Function.Arguments)
			}
			if choice.FinishReason != "" {
				stopReason = choice.FinishReason
				completed = true
			}
		}
	}

	if err := sc.Err(); err != nil && ctx.Err() == nil {
		emit(provider.Event{Kind: provider.EventError, Err: err})
		return
	}
	if !completed {
		emit(provider.Event{Kind: provider.EventError,
			Err: fmt.Errorf("%s: stream ended without a completion marker", c.ID)})
		return
	}
	if !flushCalls() {
		return
	}
	emit(provider.Event{Kind: provider.EventUsage, Usage: &usage})
	if !sawOutput {
		emit(provider.Event{Kind: provider.EventError,
			Err: fmt.Errorf("%s: empty completion: provider returned no text, reasoning, or tool calls", c.ID)})
		return
	}
	emit(provider.Event{Kind: provider.EventDone, StopReason: stopReason})
}

// ListModels queries the /models endpoint.
func (c *Client) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if c.APIKey != "" {
		req.Header.Set("authorization", "Bearer "+c.APIKey)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: models http %d", c.ID, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	models, _, err := catalog.ParseModels(body)
	if err != nil {
		return nil, err
	}
	infos := make([]provider.ModelInfo, 0, len(models))
	for _, model := range catalog.FilterChatModels(models) {
		contextWindow := model.Context
		contextSource := model.ContextSource
		if override, ok := provider.ProviderContextWindow(c.ID, model.ID); ok {
			contextWindow = override
			contextSource = provider.ContextSourceCatalog
		}
		infos = append(infos, provider.ModelInfo{
			ID: model.ID, Name: model.Name, ContextWindow: contextWindow,
			ContextSource: contextSource, SupportsImages: model.SupportsImages,
			CapabilitiesKnown: model.CapabilitiesKnown, ChatCapable: model.ChatCapable,
			ReasoningEfforts:      append([]provider.ReasoningEffort(nil), model.ReasoningEfforts...),
			ReasoningEffortsKnown: model.ReasoningEffortsKnown, ReasoningEffortsAll: model.ReasoningEffortsAll,
			ReasoningDefault: model.ReasoningDefault, ReasoningDefaultEnabled: model.ReasoningDefaultEnabled,
			ReasoningDefaultEnabledKnown: model.ReasoningDefaultEnabledKnown, ReasoningMandatory: model.ReasoningMandatory,
			ReasoningSupportsMaxTokens: model.ReasoningSupportsMaxTokens,
			ReasoningKnown:             model.ReasoningKnown,
		})
	}
	return infos, nil
}
