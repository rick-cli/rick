// Package tools implements rick's built-in tool set and the registry the agent
// loop uses to dispatch model tool calls.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"rick/internal/provider"
)

// Context carries per-call environment for a tool.
type Context struct {
	Cwd       string // working directory (project root)
	SessionID string
	Agent     string
	AgentID   string
	CallID    string
	Elicit    func(prompt string) (string, error) // optional interactive hook
	Progress  func(string)                        // optional progress reporting
	Depth     int                                 // subagent recursion depth
	// Repair carries the per-call tool-call-repair configuration. The agent
	// fills it; tools pass it to RepairDecode so repaired args and the
	// description of what was fixed flow back to the agent for surfacing and
	// per-model telemetry. Nil is fine (tests, direct callers).
	Repair *RepairOpts
}

// Result is a tool's outcome.
type Result struct {
	Output  string         // text fed back to the model
	Title   string         // one-line summary for the TUI
	Meta    map[string]any // structured extras (diffs, paths, ...)
	IsError bool
}

// Errf builds an error result.
func Errf(format string, a ...any) Result {
	return Result{Output: fmt.Sprintf(format, a...), IsError: true, Title: "error"}
}

// Tool is one callable capability.
type Tool interface {
	Name() string
	Description() string
	Schema() map[string]any
	// ReadOnly tools may be executed concurrently within a turn.
	ReadOnly() bool
	Run(ctx context.Context, tc Context, input json.RawMessage) (Result, error)
}

// ToolSet is the interface for looking up tools during an agent run.
type ToolSet interface {
	Get(name string) (Tool, bool)
	Names() []string
	Schemas(enabled func(string) bool) []provider.ToolSchema
}

// Registry holds the active tool set.
type Registry struct {
	mu     sync.RWMutex
	tools  map[string]Tool
	order  []string
	sorted []string
}

// Ensure Registry implements ToolSet.
var _ ToolSet = (*Registry)(nil)

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register adds or replaces a tool.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[t.Name()]; !exists {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
	r.sorted = append([]string(nil), r.order...)
	sort.Strings(r.sorted)
}

// Unregister removes a tool.
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	r.sorted = append([]string(nil), r.order...)
	sort.Strings(r.sorted)
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names lists registered tool names in registration order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// Schemas renders the provider-facing schema list, filtered by an enable map
// (nil means "everything enabled"). Keys may be globs handled by the caller.
func (r *Registry) Schemas(enabled func(string) bool) []provider.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := append([]string(nil), r.sorted...)
	out := make([]provider.ToolSchema, 0, len(names))
	for _, n := range names {
		if enabled != nil && !enabled(n) {
			continue
		}
		t := r.tools[n]
		out = append(out, provider.ToolSchema{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.Schema(),
		})
	}
	return out
}

// obj is a small helper for building JSON schemas.
func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// pathProp is strProp for filesystem-path fields. The "format" hint tells
// schema-aware consumers the value is a path (never markdown), and the
// runtime unwrapMarkdownLink guard in resolvePath fixes auto-linked paths
// like `[notes.md](http://notes.md)` regardless.
func pathProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "format": "path"}
}

func numProp(desc string) map[string]any {
	return map[string]any{"type": "number", "description": desc}
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func enumProp(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "enum": vals, "description": desc}
}
