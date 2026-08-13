// Package plugin provides rick's hook system. v1 supports compiled-in Go
// plugins registered by the host; the dispatcher is designed so a script
// runtime can be added later without changing call sites.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// ToolBeforeEvent is dispatched before a tool executes. Handlers may mutate
// Input, or set Skip/Reason to prevent execution.
type ToolBeforeEvent struct {
	SessionID string
	Agent     string
	Tool      string
	CallID    string
	Input     json.RawMessage

	// Set by a handler to block the call.
	Skip   bool
	Reason string
}

// ToolAfterEvent is dispatched after a tool executes. Handlers may rewrite
// Output.
type ToolAfterEvent struct {
	SessionID string
	Agent     string
	Tool      string
	CallID    string
	Input     json.RawMessage
	Output    string
	IsError   bool
}

// SessionEvent is dispatched on idle / error.
type SessionEvent struct {
	SessionID string
	Agent     string
	Err       error
}

// TurnStartEvent is dispatched at the beginning of each agent turn.
type TurnStartEvent struct {
	SessionID  string
	Agent      string
	TurnNumber int
}

// TurnEndEvent is dispatched at the end of each agent turn.
type TurnEndEvent struct {
	SessionID  string
	Agent      string
	TurnNumber int
	StopReason string
}

// SubagentStartEvent is dispatched when a subagent is spawned.
type SubagentStartEvent struct {
	SessionID    string
	Agent        string
	SubagentName string
	Task         string
}

// SubagentEndEvent is dispatched when a subagent finishes.
type SubagentEndEvent struct {
	SessionID    string
	Agent        string
	SubagentName string
	Result       string
}

// SessionStartEvent is dispatched when a session begins.
type SessionStartEvent struct {
	SessionID string
	Agent     string
}

// SessionEndEvent is dispatched when a session ends.
type SessionEndEvent struct {
	SessionID string
	Agent     string
}

// Hooks is the set of callbacks a plugin may implement. All fields optional.
type Hooks struct {
	Name string

	ToolExecuteBefore func(ctx context.Context, ev *ToolBeforeEvent) error
	ToolExecuteAfter  func(ctx context.Context, ev *ToolAfterEvent) error
	SessionIdle       func(ctx context.Context, ev *SessionEvent) error
	SessionError      func(ctx context.Context, ev *SessionEvent) error

	// Lifecycle hooks added in v2.
	TurnStart     func(ctx context.Context, ev *TurnStartEvent) error
	TurnEnd       func(ctx context.Context, ev *TurnEndEvent) error
	SubagentStart func(ctx context.Context, ev *SubagentStartEvent) error
	SubagentEnd   func(ctx context.Context, ev *SubagentEndEvent) error
	SessionStart  func(ctx context.Context, ev *SessionStartEvent) error
	SessionEnd    func(ctx context.Context, ev *SessionEndEvent) error

	// CacheStrategyHook, when set, returns a named prompt-cache strategy for
	// a provider/model route, or nil to fall through to the config/default.
	// This is the plugin seam for cache strategy: a plugin can tune the
	// cache profile of a backend it knows without touching the agent loop.
	// The returned value must implement provider.CacheStrategy.
	CacheStrategyHook func(providerID, modelID string) any
}

// Registry holds every loaded plugin.
type Registry struct {
	mu      sync.RWMutex
	plugins []Hooks
	active  []Hooks
	enabled map[string]bool
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{enabled: map[string]bool{}}
}

// Register adds a plugin. Plugins are enabled by default.
func (r *Registry) Register(h Hooks) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.plugins {
		if existing.Name == h.Name {
			r.plugins[i] = h
			r.refreshActiveLocked()
			return
		}
	}
	r.plugins = append(r.plugins, h)
	if _, exists := r.enabled[h.Name]; !exists {
		r.enabled[h.Name] = true
	}
	r.refreshActiveLocked()
}

// Names lists loaded plugin names.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.plugins))
	for _, p := range r.plugins {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

// Len returns the plugin count.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.plugins)
}

// SetEnabled enables or disables a plugin by name.
func (r *Registry) SetEnabled(name string, enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled[name] = enabled
	r.refreshActiveLocked()
}

// IsEnabled reports whether a plugin is enabled.
func (r *Registry) IsEnabled(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.enabled[name]
	return !ok || v // default to enabled
}

// Toggle flips a plugin's enabled state and returns the new state.
func (r *Registry) Toggle(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.enabled[name]
	if !ok {
		cur = true
	}
	r.enabled[name] = !cur
	r.refreshActiveLocked()
	return !cur
}

// PluginInfo describes a loaded plugin for listing.
type PluginInfo struct {
	Name        string
	Description string
	Enabled     bool
	Source      string
}

// List returns info about every registered plugin.
func (r *Registry) List() []PluginInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]PluginInfo, 0, len(r.plugins))
	for _, p := range r.plugins {
		enabled, ok := r.enabled[p.Name]
		if !ok {
			enabled = true
		}
		out = append(out, PluginInfo{
			Name:    p.Name,
			Enabled: enabled,
		})
	}
	return out
}

// Remove deletes a plugin by name. Returns true if found.
func (r *Registry) Remove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, p := range r.plugins {
		if p.Name == name {
			r.plugins = append(r.plugins[:i], r.plugins[i+1:]...)
			delete(r.enabled, name)
			r.refreshActiveLocked()
			return true
		}
	}
	return false
}

// refreshActiveLocked rebuilds the immutable dispatch snapshot. Caller holds r.mu.
func (r *Registry) refreshActiveLocked() {
	active := make([]Hooks, 0, len(r.plugins))
	for _, p := range r.plugins {
		if enabled, ok := r.enabled[p.Name]; !ok || enabled {
			active = append(active, p)
		}
	}
	r.active = active
}

// activePlugins returns the current immutable snapshot of enabled plugins.
func (r *Registry) activePlugins() []Hooks {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active
}

// DispatchToolBefore runs every before-hook in registration order. The first
// handler to set Skip stops the chain.
func (r *Registry) DispatchToolBefore(ctx context.Context, ev *ToolBeforeEvent) error {
	for _, p := range r.activePlugins() {
		if p.ToolExecuteBefore == nil {
			continue
		}
		if err := p.ToolExecuteBefore(ctx, ev); err != nil {
			return err
		}
		if ev.Skip {
			return nil
		}
	}
	return nil
}

// DispatchToolAfter runs every after-hook.
func (r *Registry) DispatchToolAfter(ctx context.Context, ev *ToolAfterEvent) error {
	for _, p := range r.activePlugins() {
		if p.ToolExecuteAfter == nil {
			continue
		}
		if err := p.ToolExecuteAfter(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

// dispatchLifecycle runs every lifecycle hook and retains failures so callers
// can surface them without silently swallowing plugin errors.
func (r *Registry) dispatchLifecycle(run func(Hooks) error) []error {
	var errs []error
	for _, p := range r.activePlugins() {
		if err := run(p); err != nil {
			errs = append(errs, fmt.Errorf("plugin %q: %w", p.Name, err))
		}
	}
	return errs
}

// DispatchSessionIdle runs every idle hook.
func (r *Registry) DispatchSessionIdle(ctx context.Context, ev *SessionEvent) []error {
	return r.dispatchLifecycle(func(p Hooks) error {
		if p.SessionIdle == nil {
			return nil
		}
		return p.SessionIdle(ctx, ev)
	})
}

// DispatchSessionError runs every error hook.
func (r *Registry) DispatchSessionError(ctx context.Context, ev *SessionEvent) []error {
	return r.dispatchLifecycle(func(p Hooks) error {
		if p.SessionError == nil {
			return nil
		}
		return p.SessionError(ctx, ev)
	})
}

// DispatchTurnStart runs every turn-start hook.
func (r *Registry) DispatchTurnStart(ctx context.Context, ev *TurnStartEvent) []error {
	return r.dispatchLifecycle(func(p Hooks) error {
		if p.TurnStart == nil {
			return nil
		}
		return p.TurnStart(ctx, ev)
	})
}

// DispatchTurnEnd runs every turn-end hook.
func (r *Registry) DispatchTurnEnd(ctx context.Context, ev *TurnEndEvent) []error {
	return r.dispatchLifecycle(func(p Hooks) error {
		if p.TurnEnd == nil {
			return nil
		}
		return p.TurnEnd(ctx, ev)
	})
}

// DispatchSubagentStart runs every subagent-start hook.
func (r *Registry) DispatchSubagentStart(ctx context.Context, ev *SubagentStartEvent) []error {
	return r.dispatchLifecycle(func(p Hooks) error {
		if p.SubagentStart == nil {
			return nil
		}
		return p.SubagentStart(ctx, ev)
	})
}

// DispatchSubagentEnd runs every subagent-end hook.
func (r *Registry) DispatchSubagentEnd(ctx context.Context, ev *SubagentEndEvent) []error {
	return r.dispatchLifecycle(func(p Hooks) error {
		if p.SubagentEnd == nil {
			return nil
		}
		return p.SubagentEnd(ctx, ev)
	})
}

// DispatchSessionStart runs every session-start hook.
func (r *Registry) DispatchSessionStart(ctx context.Context, ev *SessionStartEvent) []error {
	return r.dispatchLifecycle(func(p Hooks) error {
		if p.SessionStart == nil {
			return nil
		}
		return p.SessionStart(ctx, ev)
	})
}

// DispatchSessionEnd runs every session-end hook.
func (r *Registry) DispatchSessionEnd(ctx context.Context, ev *SessionEndEvent) []error {
	return r.dispatchLifecycle(func(p Hooks) error {
		if p.SessionEnd == nil {
			return nil
		}
		return p.SessionEnd(ctx, ev)
	})
}

// CacheStrategyHooks returns the first non-nil strategy from the enabled
// plugins' CacheStrategyHook callbacks for a provider/model route. Plugins
// run in registration order; the first hit wins. A nil result means no
// plugin overrides this route.
func (r *Registry) CacheStrategyHooks(providerID, modelID string) any {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	active := append([]Hooks(nil), r.active...)
	r.mu.RUnlock()
	for _, hooks := range active {
		if hooks.CacheStrategyHook == nil {
			continue
		}
		if strategy := hooks.CacheStrategyHook(providerID, modelID); strategy != nil {
			return strategy
		}
	}
	return nil
}

// knownHookNames enumerates valid hook keys in a manifest.
var knownHookNames = map[string]bool{
	"tool_before":    true,
	"tool_after":     true,
	"session_start":  true,
	"session_end":    true,
	"session_idle":   true,
	"session_error":  true,
	"turn_start":     true,
	"turn_end":       true,
	"subagent_start": true,
	"subagent_end":   true,
}

// Manifest is the JSON shape of a plugin file (.rick-plugin or .json).
type Manifest struct {
	Name        string            `json:"name"`
	Version     string            `json:"version,omitempty"`
	Description string            `json:"description,omitempty"`
	Hooks       map[string]string `json:"hooks,omitempty"`
	Enabled     *bool             `json:"enabled,omitempty"`
	Source      string            `json:"-"` // file path or URL, not serialized
}

// Validate checks a manifest for structural correctness.
func (m *Manifest) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("plugin manifest: name is required")
	}
	for hook := range m.Hooks {
		if !knownHookNames[hook] {
			return fmt.Errorf("plugin %q: unknown hook %q", m.Name, hook)
		}
	}
	return nil
}

// IsEnabled returns the manifest's enabled state (default true).
func (m *Manifest) IsEnabled() bool {
	return m.Enabled == nil || *m.Enabled
}
