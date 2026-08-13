package agent

import "testing"

// TestCacheScopeKeyPartitionsParallelAgents pins the OpenAI per-key rate
// guidance: parallel agents (subagents, swarm workers) that share a parent
// SessionID must derive distinct, stable prompt-cache routing scopes, so a
// concurrent burst does not pile every agent onto one hot prompt_cache_key.
func TestCacheScopeKeyPartitionsParallelAgents(t *testing.T) {
	// Explicit CacheScopeKey (non-interactive runs) always wins.
	explicit := (&Runner{cfg: Config{CacheScopeKey: "fixed"}}).cacheScopeKey()
	if explicit != "fixed" {
		t.Fatalf("explicit scope = %q, want fixed", explicit)
	}

	// Parallel agents with distinct identities partition the scope, but the
	// same agent keeps a stable scope across its own turns.
	agentA := (&Runner{cfg: Config{Parallel: true, SessionID: "sess-1", AgentID: "a1", AgentName: "explore"}}).cacheScopeKey()
	agentA2 := (&Runner{cfg: Config{Parallel: true, SessionID: "sess-1", AgentID: "a1", AgentName: "explore"}}).cacheScopeKey()
	agentB := (&Runner{cfg: Config{Parallel: true, SessionID: "sess-1", AgentID: "b2", AgentName: "general"}}).cacheScopeKey()
	if agentA == "" {
		t.Fatal("parallel agent scope is empty")
	}
	if agentA != agentA2 {
		t.Fatalf("same agent produced different scopes: %q vs %q", agentA, agentA2)
	}
	if agentA == agentB {
		t.Fatal("distinct parallel agents shared a cache scope")
	}

	// A non-parallel main session keeps the session-derived scope (empty
	// override, so the provider falls back to the stable-head/session scope).
	if got := (&Runner{cfg: Config{SessionID: "sess-1"}}).cacheScopeKey(); got != "" {
		t.Fatalf("non-parallel session scope = %q, want empty", got)
	}

	// AgentName is the fallback identity when AgentID is absent.
	byName := (&Runner{cfg: Config{Parallel: true, SessionID: "sess-1", AgentName: "worker"}}).cacheScopeKey()
	if byName == "" {
		t.Fatal("parallel agent without AgentID produced an empty scope")
	}
	if byName == agentA {
		t.Fatal("name fallback collided with an id-derived scope")
	}

	// A parallel agent without any session still derives no scope (the
	// provider falls back to the byte-stable head).
	if got := (&Runner{cfg: Config{Parallel: true, AgentName: "worker"}}).cacheScopeKey(); got != "" {
		t.Fatalf("parallel agent without session scope = %q, want empty", got)
	}
}
