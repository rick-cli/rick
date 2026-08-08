// Package anthropic implements provider.Provider against the Anthropic
// Messages API using raw HTTP + SSE (no SDK dependency).
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"rick/internal/provider"
	"rick/internal/provider/catalog"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
	// extendedCacheTTLBeta opt-in for cache_control ttl:"1h". Anthropic
	// rejects a 1h TTL with a 400 unless this beta header is present, so
	// "long" retention must not be sent without it or it silently degrades
	// to the default 5-minute TTL and costs a cache re-write every turn.
	extendedCacheTTLBeta = "extended-cache-ttl-2025-04-11"
)

// cacheBetaHeader returns the anthropic-beta header needed for the request's
// retention policy, or "". "long" asks for a 1h cache TTL, which Anthropic
// only honours when the extended-cache-ttl beta is opted in per request
// (mirrors oh-my-pi: the header travels exactly when ttl:"1h" is emitted).
func cacheBetaHeader(retention provider.CacheRetention) string {
	if retention == provider.CacheRetentionLong {
		return extendedCacheTTLBeta
	}
	return ""
}

// Client is an Anthropic provider.
type Client struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
}

// New builds a client. baseURL may be empty for the public API.
func New(apiKey, baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		APIKey:  catalog.CleanSecret(apiKey),
		BaseURL: strings.TrimRight(strings.ReplaceAll(baseURL, "\x00", ""), "/"),
		HTTP:    &http.Client{Timeout: 15 * time.Minute},
	}
}

// Name implements provider.Provider.
func (c *Client) Name() string { return "anthropic" }

// SetAPIKey updates the API key for this client.
func (c *Client) SetAPIKey(key string) {
	c.APIKey = key
}
func (c *Client) Models() []provider.ModelInfo {
	return []provider.ModelInfo{
		{ID: "claude-sonnet-4-5-20250929", Name: "Claude Sonnet 4.5", ContextWindow: 200000, MaxOutput: 64000},
		{ID: "claude-opus-4-5-20251101", Name: "Claude Opus 4.5", ContextWindow: 200000, MaxOutput: 64000},
		{ID: "claude-haiku-4-5-20251001", Name: "Claude Haiku 4.5", ContextWindow: 200000, MaxOutput: 32000},
		{ID: "claude-3-5-haiku-20241022", Name: "Claude 3.5 Haiku", ContextWindow: 200000, MaxOutput: 8192},
	}
}

// ---------- wire types ----------

type wireBlock struct {
	Type         string           `json:"type"`
	Text         string           `json:"text,omitempty"`
	Thinking     string           `json:"thinking,omitempty"`
	Signature    string           `json:"signature,omitempty"`
	ID           string           `json:"id,omitempty"`
	Name         string           `json:"name,omitempty"`
	Input        json.RawMessage  `json:"input,omitempty"`
	ToolUseID    string           `json:"tool_use_id,omitempty"`
	Content      any              `json:"content,omitempty"`
	IsError      bool             `json:"is_error,omitempty"`
	Source       *wireImageSource `json:"source,omitempty"`
	CacheControl *cacheControl    `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// cacheControlFor renders the cache_control marker for a retention policy.
// "none" disables prompt caching entirely: no breakpoints are emitted, so a
// one-off call (distillation, compaction) never reads or writes the session
// cache. "long" extends the provider's default 5-minute TTL to an hour.
func cacheControlFor(retention provider.CacheRetention) *cacheControl {
	switch retention {
	case provider.CacheRetentionNone:
		return nil
	case provider.CacheRetentionLong:
		return &cacheControl{Type: "ephemeral", TTL: "1h"}
	default:
		return &cacheControl{Type: "ephemeral"}
	}
}

type wireTool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	CacheControl *cacheControl  `json:"cache_control,omitempty"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

type wireRequest struct {
	Model        string        `json:"model"`
	MaxTokens    int           `json:"max_tokens"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
	System       []wireBlock   `json:"system,omitempty"`
	Messages     []wireMessage `json:"messages"`
	Tools        []wireTool    `json:"tools,omitempty"`
	Stream       bool          `json:"stream"`
	Temperature  *float64      `json:"temperature,omitempty"`
	Thinking     *wireThinking `json:"thinking,omitempty"`
}

func wireSystem(system, stable string, retention provider.CacheRetention) []wireBlock {
	if strings.TrimSpace(system) == "" {
		return nil
	}
	cc := cacheControlFor(retention)
	if stable != "" && strings.HasPrefix(system, stable) && strings.TrimSpace(stable) != "" {
		blocks := []wireBlock{{
			Type: "text", Text: stable,
			CacheControl: cc,
		}}
		if suffix := strings.TrimPrefix(system, stable); strings.TrimSpace(suffix) != "" {
			blocks = append(blocks, wireBlock{Type: "text", Text: suffix})
		}
		return blocks
	}
	return []wireBlock{{
		Type: "text", Text: system,
		CacheControl: cc,
	}}
}

func wireTools(tools []provider.ToolSchema, retention provider.CacheRetention) []wireTool {
	tools = provider.CanonicalToolSchemas(tools)
	if len(tools) == 0 {
		return nil
	}
	cc := cacheControlFor(retention)
	out := make([]wireTool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, wireTool{
			Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema,
		})
	}
	if cc != nil {
		out[len(out)-1].CacheControl = cc
	}
	return out
}

// wireThinking is Anthropic's extended-thinking block.
type wireThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

// wireImageSource is Anthropic's image source for vision.
type wireImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// toWire flattens rick's block model onto Anthropic's wire model. Marked
// boundary messages get a cache_control on their first non-tool_result block
// (Anthropic rejects cache_control on tool_result blocks). When retention is
// "none" no breakpoints are emitted at all.
//
// Besides the requested boundaries, the newest message that can carry a
// breakpoint always gets one (pi's "cache the whole history from turn 1"):
// everything before the live turn is cached, so only the newest tool results
// and the reply pay full price each turn.
func toWire(msgs []provider.Message, boundaries map[int]bool, retention provider.CacheRetention) []wireMessage {
	cc := cacheControlFor(retention)
	out := make([]wireMessage, 0, len(msgs))
	for index, m := range msgs {
		wm := wireMessage{Role: m.Role}
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				wm.Content = append(wm.Content, wireBlock{Type: "text", Text: b.Text})
			case "thinking":
				if b.Signature == "" {
					continue // unsigned thinking cannot be replayed
				}
				wm.Content = append(wm.Content, wireBlock{Type: "thinking", Thinking: b.Text, Signature: b.Signature})
			case "tool_use":
				in := b.Input
				if len(in) == 0 {
					in = json.RawMessage(`{}`)
				}
				wm.Content = append(wm.Content, wireBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: in})
			case "tool_result":
				wm.Content = append(wm.Content, wireBlock{
					Type: "tool_result", ToolUseID: b.ToolUseID, Content: b.Content, IsError: b.IsError,
				})
			case "image":
				if b.Source == "base64" && b.Data != "" {
					wm.Content = append(wm.Content, wireBlock{
						Type: "image",
						Source: &wireImageSource{
							Type:      "base64",
							MediaType: b.MediaType,
							Data:      b.Data,
						},
					})
				}
			}
		}
		if len(wm.Content) == 0 {
			continue // Anthropic rejects empty content arrays
		}
		// A stable cache boundary marks the message that ends a reusable
		// prefix; everything before it is cached by the provider. The marker
		// must sit on the first block that can carry cache_control
		// (Anthropic rejects it on tool_result and thinking blocks).
		if boundaries != nil && boundaries[index] {
			placeBoundary(wm.Content, cc)
		}
		out = append(out, wm)
	}

	// Eager last-message boundary: walk from the newest message backwards to
	// the first that can carry a cache breakpoint and mark it. A boundary on
	// the newest message means the entire prior history is cacheable.
	if cc != nil && len(out) > 0 {
		for index := len(out) - 1; index >= 0; index-- {
			if hasMarkableBlock(out[index].Content) {
				placeBoundary(out[index].Content, cc)
				break
			}
		}
	}
	return out
}

// placeBoundary puts cache_control on the first block that can carry it.
// Anthropic rejects cache_control on tool_result and thinking blocks.
func placeBoundary(content []wireBlock, cc *cacheControl) {
	for blockIndex, block := range content {
		if !boundaryMarkable(block.Type) {
			continue
		}
		content[blockIndex].CacheControl = cc
		return
	}
}

// hasMarkableBlock reports whether a wire message can carry cache_control.
func hasMarkableBlock(content []wireBlock) bool {
	for _, block := range content {
		if boundaryMarkable(block.Type) {
			return true
		}
	}
	return false
}

func boundaryMarkable(blockType string) bool {
	return blockType != "tool_result" && blockType != "thinking"
}

// Stream implements provider.Provider. It owns ch and closes it exactly once.
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

	if c.APIKey == "" {
		emit(provider.Event{Kind: provider.EventError, Err: fmt.Errorf("anthropic: no API key (set ANTHROPIC_API_KEY)")})
		return
	}

	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = 8192
	}
	// Anthropic allows 4 cache breakpoints per request: system, tools, and
	// at most 2 message boundaries. toWire also adds an eager boundary on the
	// newest markable message, so keep at most one budget boundary here (the
	// oldest, which anchors the distillation/compaction summary cut).
	boundaries := req.CacheBoundaries
	if len(boundaries) > 1 {
		oldest := -1
		for index := range boundaries {
			if oldest < 0 || index < oldest {
				oldest = index
			}
		}
		boundaries = map[int]bool{oldest: true}
	}
	body := wireRequest{
		Model:        req.Model,
		MaxTokens:    maxTok,
		CacheControl: cacheControlFor(req.CacheRetention),
		System:       wireSystem(req.System, req.SystemStable, req.CacheRetention),
		Messages:     toWire(req.Messages, boundaries, req.CacheRetention),
		Tools:        wireTools(req.Tools, req.CacheRetention),
		Stream:       true,
		Temperature:  req.Temperature,
	}

	// Extended thinking, when the model supports it and a level is asked for.
	if style, _ := provider.DetectReasoningForProvider("anthropic", req.Model); style == provider.ReasoningStyleAnthropic ||
		(style == provider.ReasoningStyleUnknown && req.Reasoning != provider.ReasoningOff) {
		if budget := req.Reasoning.Budget(maxTok); budget > 0 {
			body.Thinking = &wireThinking{Type: "enabled", BudgetTokens: budget}
			// Anthropic rejects a temperature alongside thinking.
			body.Temperature = nil
		}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		emit(provider.Event{Kind: provider.EventError, Err: err})
		return
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		emit(provider.Event{Kind: provider.EventError, Err: err})
		return
	}
	httpReq.Header.Set("x-api-key", c.APIKey)
	httpReq.Header.Set("anthropic-version", apiVersion)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "text/event-stream")
	if beta := cacheBetaHeader(req.CacheRetention); beta != "" {
		httpReq.Header.Set("anthropic-beta", beta)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		emit(provider.Event{Kind: provider.EventError, Err: err})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		emit(provider.Event{Kind: provider.EventError,
			Err: fmt.Errorf("anthropic: http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))})
		return
	}

	c.readSSE(ctx, resp.Body, emit)
}

// Warm implements provider.CacheWarmber. It submits a tiny non-streaming
// request carrying the same stable system + tools + prior transcript, with
// cache_control breakpoints and a 1h TTL, so the provider populates its prompt
// cache before the first real turn. The 1-token output budget keeps the call
// near-free; the response is discarded. Errors are best-effort (the caller
// proceeds to Stream regardless).
func (c *Client) Warm(ctx context.Context, req provider.Request) error {
	if c.APIKey == "" {
		return fmt.Errorf("anthropic: no API key set")
	}
	msgs := req.Messages
	if len(msgs) == 0 {
		msgs = []provider.Message{provider.UserText("ack")}
	}
	body := wireRequest{
		Model:     req.Model,
		MaxTokens: 1,
		Stream:    false,
		System:    wireSystem(req.System, req.SystemStable, provider.CacheRetentionLong),
		Messages:  toWire(msgs, nil, provider.CacheRetentionLong),
		Tools:     wireTools(req.Tools, provider.CacheRetentionLong),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	httpReq.Header.Set("x-api-key", c.APIKey)
	httpReq.Header.Set("anthropic-version", apiVersion)
	httpReq.Header.Set("content-type", "application/json")
	if beta := cacheBetaHeader(provider.CacheRetentionLong); beta != "" {
		httpReq.Header.Set("anthropic-beta", beta)
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("anthropic: warm http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// toolAccum accumulates streamed partial JSON for one tool_use block.
type toolAccum struct {
	id   string
	name string
	json strings.Builder
}

func (c *Client) readSSE(ctx context.Context, r io.Reader, emit func(provider.Event) bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)

	var event string
	var data strings.Builder
	blocks := map[int]*toolAccum{}
	usage := provider.Usage{}
	stopReason := ""

	flush := func() bool {
		if event == "" && data.Len() == 0 {
			return true
		}
		payload := data.String()
		ev, dat := event, payload
		event = ""
		data.Reset()
		return c.handleEvent(ev, dat, blocks, &usage, &stopReason, emit)
	}

	for sc.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			if !flush() {
				return
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(line[len("data:"):]))
		}
	}
	if !flush() {
		return
	}

	if err := sc.Err(); err != nil && ctx.Err() == nil {
		emit(provider.Event{Kind: provider.EventError, Err: err})
		return
	}
	indices := make([]int, 0, len(blocks))
	for index := range blocks {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		b := blocks[index]
		input := strings.TrimSpace(b.json.String())
		if input == "" {
			input = "{}"
		}
		if !emit(provider.Event{Kind: provider.EventToolCall,
			ToolCall: &provider.ToolCall{ID: b.id, Name: b.name, Input: json.RawMessage(input)}}) {
			return
		}
	}
	emit(provider.Event{Kind: provider.EventUsage, Usage: &usage})
	emit(provider.Event{Kind: provider.EventDone, StopReason: stopReason})
}

func (c *Client) handleEvent(event, data string, blocks map[int]*toolAccum,
	usage *provider.Usage, stopReason *string, emit func(provider.Event) bool) bool {

	if data == "" {
		return true
	}
	switch event {
	case "message_start":
		var d struct {
			Message struct {
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(data), &d) == nil {
			// Anthropic's input_tokens includes cache_creation tokens; the
			// Usage contract is InputTokens = uncached input, with cache
			// reads and writes disjoint. Normalize here so goal budgets,
			// hit-rate math and the TUI split agree across providers.
			creation := d.Message.Usage.CacheCreationInputTokens
			input := d.Message.Usage.InputTokens - creation
			if input < 0 {
				input = 0
			}
			usage.InputTokens = input
			usage.CacheReadTokens = d.Message.Usage.CacheReadInputTokens
			usage.CacheWriteTokens = creation
		}

	case "content_block_start":
		var d struct {
			Index        int       `json:"index"`
			ContentBlock wireBlock `json:"content_block"`
		}
		if json.Unmarshal([]byte(data), &d) != nil {
			return true
		}
		if d.ContentBlock.Type == "tool_use" {
			blocks[d.Index] = &toolAccum{id: d.ContentBlock.ID, name: d.ContentBlock.Name}
			return emit(provider.Event{Kind: provider.EventToolCallStart,
				ToolCall: &provider.ToolCall{ID: d.ContentBlock.ID, Name: d.ContentBlock.Name}})
		}

	case "content_block_delta":
		var d struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &d) != nil {
			return true
		}
		switch d.Delta.Type {
		case "text_delta":
			return emit(provider.Event{Kind: provider.EventText, Text: d.Delta.Text})
		case "thinking_delta":
			return emit(provider.Event{Kind: provider.EventThinking, Text: d.Delta.Thinking})
		case "input_json_delta", "input_json_partial":
			if b := blocks[d.Index]; b != nil {
				b.json.WriteString(d.Delta.PartialJSON)
			}
		}

	case "content_block_stop":
		var d struct {
			Index int `json:"index"`
		}
		if json.Unmarshal([]byte(data), &d) != nil {
			return true
		}
		if b := blocks[d.Index]; b != nil {
			in := strings.TrimSpace(b.json.String())
			if in == "" {
				in = "{}"
			}
			delete(blocks, d.Index)
			return emit(provider.Event{Kind: provider.EventToolCall,
				ToolCall: &provider.ToolCall{ID: b.id, Name: b.name, Input: json.RawMessage(in)}})
		}

	case "message_delta":
		var d struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &d) == nil {
			if d.Delta.StopReason != "" {
				*stopReason = d.Delta.StopReason
			}
			if d.Usage.OutputTokens > 0 {
				usage.OutputTokens = d.Usage.OutputTokens
			}
		}

	case "error":
		var d struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &d) == nil {
			return emit(provider.Event{Kind: provider.EventError,
				Err: fmt.Errorf("anthropic: %s: %s", d.Error.Type, d.Error.Message)})
		}
	}
	return true
}

// ListModels queries the live model catalogue.
func (c *Client) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/models?limit=100", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", apiVersion)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic: models http %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	models := make([]provider.ModelInfo, 0, len(out.Data))
	for _, m := range out.Data {
		models = append(models, provider.ModelInfo{
			ID: m.ID, Name: m.DisplayName, ContextWindow: 200000, MaxOutput: 32000,
		})
	}
	return models, nil
}
