// Package provider defines the LLM provider abstraction used by rick.
//
// Every backend (Anthropic, OpenAI, OpenRouter, ...) implements Provider.
// The agent loop only ever talks to this interface, so adding a backend never
// touches the agent or the TUI.
package provider

import (
	"context"
	"encoding/json"
	"sort"
)

// Role constants for Message.Role.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
)

// ContentBlock is one piece of a message. A message is a list of blocks so a
// single assistant turn can hold text plus several tool calls, and a single
// user turn can hold several tool results.
type ContentBlock struct {
	Type string `json:"type"` // "text" | "tool_use" | "tool_result" | "thinking" | "image"

	// Type == "text" or "thinking"
	Text string `json:"text,omitempty"`

	// Type == "thinking" (provider-signed, replayed verbatim)
	Signature string `json:"signature,omitempty"`

	// Type == "tool_use"
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// Type == "tool_result"
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	// Type == "image" — base64-encoded image for vision models
	Source    string `json:"source,omitempty"`     // "base64" or "url"
	MediaType string `json:"media_type,omitempty"` // "image/png", "image/jpeg", etc.
	Data      string `json:"data,omitempty"`       // base64-encoded image data or URL
}

// TextBlock is a convenience constructor.
func TextBlock(s string) ContentBlock { return ContentBlock{Type: "text", Text: s} }

// ImageBlock is a convenience constructor for base64-encoded images.
func ImageBlock(mediaType, base64Data string) ContentBlock {
	return ContentBlock{Type: "image", Source: "base64", MediaType: mediaType, Data: base64Data}
}

// ToolResultBlock is a convenience constructor.
func ToolResultBlock(id, content string, isErr bool) ContentBlock {
	return ContentBlock{Type: "tool_result", ToolUseID: id, Content: content, IsError: isErr}
}

// Message is one conversational turn.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// UserText builds a plain user message.
func UserText(s string) Message {
	return Message{Role: RoleUser, Content: []ContentBlock{TextBlock(s)}}
}

// AssistantText builds a plain assistant message.
func AssistantText(s string) Message {
	return Message{Role: RoleAssistant, Content: []ContentBlock{TextBlock(s)}}
}

// Text flattens all text blocks of a message.
func (m Message) Text() string {
	out := ""
	for _, b := range m.Content {
		if b.Type == "text" {
			out += b.Text
		}
	}
	return out
}

// ToolSchema describes a tool to the model.
type ToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// CanonicalToolSchemas returns a byte-stable copy of tools for the wire:
// ordered by name, with each schema's JSON canonicalized. The wire tools
// block is part of the provider-cached prefix — a registry that iterates a
// map, or a schema whose key order flips between turns, would re-bill the
// whole tools block mid-session. Sorts are stable so identical inputs always
// serialize identically (matching reasonix's sorted Schemas()).
func CanonicalToolSchemas(tools []ToolSchema) []ToolSchema {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ToolSchema, len(tools))
	copy(out, tools)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	for i := range out {
		out[i].InputSchema = canonicalizeSchemaMap(out[i].InputSchema).(map[string]any)
	}
	return out
}

// canonicalSchemaMap deep-normalizes a JSON-Schema map so key order and map
// iteration can never change the marshaled bytes of the wire tools block.
// encoding/json sorts map keys at marshal time, but nested objects created
// by tools may arrive as map[string]any while slices of objects need
// element-wise normalization; recursing here makes the snapshot canonical
// regardless of how each tool built its schema.
func canonicalizeSchemaMap(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = canonicalizeSchemaMap(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = canonicalizeSchemaMap(val)
		}
		return out
	default:
		return v
	}
}

// CacheRetention controls provider prompt-cache behaviour for a request.
type CacheRetention string

const (
	// CacheRetentionAuto uses the provider default TTL (Anthropic ~5 min,
	// OpenAI implicit caching) — "short".
	CacheRetentionAuto CacheRetention = ""
	// CacheRetentionLong requests extended retention (Anthropic
	// cache_control ttl:"1h", OpenAI prompt_cache_retention:"24h") so the
	// cache survives long turns, idle gaps, and resumed sessions.
	CacheRetentionLong CacheRetention = "long"
	// CacheRetentionNone disables prompt caching for this request: no
	// breakpoints, no cache keys, no cache writes. Used for one-off calls
	// (distillation, compaction) that would pollute the session cache.
	CacheRetentionNone CacheRetention = "none"
)

// Request is a single completion request.
type Request struct {
	Model  string
	System string
	// SystemStable is an optional prefix of System that remains reusable
	// across requests. Providers that support prompt caching may mark it.
	SystemStable string
	Messages     []Message
	Tools        []ToolSchema
	MaxTokens    int
	Temperature  *float64
	// Reasoning is the requested thinking level. Providers translate it into
	// their own dialect and ignore it when the model does not reason.
	Reasoning ReasoningEffort
	// CacheBoundaries marks message indices that delimit a stable history
	// prefix. Providers that support explicit cache breakpoints (Anthropic)
	// place a cache_control marker on the message at each index; other
	// providers ignore the field.
	CacheBoundaries map[int]bool
	// CacheRetention is the prompt-cache retention policy for this request.
	// Empty uses the provider default; "long" extends the TTL; "none"
	// disables caching entirely.
	CacheRetention CacheRetention
	// MaxReasoningTurns caps how many prior turns' reasoning blocks are
	// echoed back to a DeepSeek-line provider as reasoning_content. 0 (the
	// default) keeps every turn's reasoning so the serialized prefix is
	// byte-identical and the automatic prefix cache stays warm. A positive
	// value keeps only the most recent turns and strips older blocks to
	// empty — a deliberate one-time prompt rewrite that shrinks the prompt
	// but costs one cache invalidation. Tune with the per-request telemetry
	// (session "requests") to find the local optimum.
	MaxReasoningTurns int
	// SessionID names the session for session-keyed prompt caches and
	// session-affinity routing hints. Stable across resume so a restarted
	// session keeps hitting the same cache.
	SessionID string
}

// Usage reports token accounting for a turn.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

// EventKind enumerates stream event types.
type EventKind int

const (
	EventText          EventKind = iota // incremental assistant text
	EventThinking                       // incremental reasoning text
	EventToolCallStart                  // model began emitting a tool call
	EventToolCall                       // a complete tool call is ready
	EventUsage                          // token accounting
	EventDone                           // turn finished (StopReason set)
	EventError                          // fatal stream error
)

// ToolCall is a fully-parsed tool invocation from the model.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// Event is one item in the provider stream.
type Event struct {
	Kind       EventKind
	Text       string
	ToolCall   *ToolCall
	Usage      *Usage
	StopReason string
	Err        error
}

// ContextSource describes where a model context window came from.
type ContextSource string

const (
	ContextSourceUnknown  ContextSource = ""
	ContextSourceAPI      ContextSource = "api"
	ContextSourceCatalog  ContextSource = "catalog"
	ContextSourceInferred ContextSource = "inferred"
	ContextSourceBuiltin  ContextSource = "builtin"
)

// ModelInfo is a model advertised by a provider.
type ModelInfo struct {
	ID             string
	Name           string
	ContextWindow  int
	ContextSource  ContextSource
	MaxOutput      int
	SupportsImages bool
	// CapabilitiesKnown is true when the provider gave explicit modality/task
	// metadata. When false, ChatCapable is only a hint and the model id is used
	// as a conservative fallback for non-chat model families.
	CapabilitiesKnown bool
	ChatCapable       bool
	// ReasoningEfforts is the provider's advertised effort vocabulary. A nil
	// slice means no explicit vocabulary was supplied; use the model/provider
	// fallback in that case.
	ReasoningEfforts             []ReasoningEffort
	ReasoningEffortsKnown        bool
	ReasoningEffortsAll          bool
	ReasoningDefault             ReasoningEffort
	ReasoningDefaultEnabled      bool
	ReasoningDefaultEnabledKnown bool
	ReasoningMandatory           bool
	ReasoningSupportsMaxTokens   bool
	ReasoningKnown               bool
}

// Provider is the single abstraction every backend implements.
//
// Stream owns ch: it must close ch exactly once before returning. Callers must
// never close it (see the double-close pitfall). EventDone and EventError are
// terminal events; callers may cancel ctx immediately after receiving either.
// Providers should stop work promptly when ctx is cancelled.
type Provider interface {
	Name() string
	Models() []ModelInfo
	Stream(ctx context.Context, req Request, ch chan<- Event)
}

// ModelLister is optionally implemented by providers that can enumerate models
// from a live endpoint.
type ModelLister interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// CacheWarmber is optionally implemented by providers that can submit a small,
// non-streaming "warm" request that populates the provider's prompt cache with
// the request's prefix (stable system + tools + prior transcript) before the
// first real turn. The caller (agent layer) invokes this once at session start
// when prompt-cache warming is enabled, then proceeds to Stream. Errors are
// best-effort: the agent logs and continues.
type CacheWarmber interface {
	Warm(ctx context.Context, req Request) error
}
