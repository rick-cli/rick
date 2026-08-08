package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rick/internal/provider"
	"rick/internal/tokens"
	"rick/internal/tools"
)

// TestBootComposesByteStableSystemPrompt is the boot-level twin of reasonix's
// TestBuildComposesByteStableSystemPrompt: two full compositions over the same
// project context, environment and tool registry must produce byte-identical
// system prompts. This is the provider-cached prefix of every request in every
// session — any byte of nondeterminism (probe flaps, unsorted iteration,
// time-dependent content, an Environment block that rolls over) cold-starts
// the whole session's prefix cache.
func TestBootComposesByteStableSystemPrompt(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "RICK.md"), []byte("Project rule: keep the prompt prefix stable."), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	registry.Register(staticTestTool{name: "aa_first"})
	registry.Register(staticTestTool{name: "bb_second"})
	registry.Register(staticTestTool{name: "cc_third"})

	compose := func() (string, []provider.ToolSchema) {
		base := BuildPrompt
		project := ProjectContext(root, nil)
		stable := base + project
		volatile := Environment(root, "deepseek-model", "open", "") +
			"\n" + toolManifest(registry.Schemas(nil))
		return stable + volatile, registry.Schemas(nil)
	}

	firstPrompt, firstSchemas := compose()
	secondPrompt, secondSchemas := compose()
	if firstPrompt != secondPrompt {
		t.Fatalf("composed system prompt not byte-stable across identical builds:\nfirst  (%d bytes)\nsecond (%d bytes)\nfirst divergence at %d",
			len(firstPrompt), len(secondPrompt), firstDivergence(firstPrompt, secondPrompt))
	}
	if len(firstSchemas) != len(secondSchemas) {
		t.Fatalf("schema count drifted: %d vs %d", len(firstSchemas), len(secondSchemas))
	}
	for i := range firstSchemas {
		if firstSchemas[i].Name != secondSchemas[i].Name {
			t.Fatalf("schema order drifted at %d: %q vs %q", i, firstSchemas[i].Name, secondSchemas[i].Name)
		}
	}
	if got, want := firstSchemas[0].Name, "aa_first"; got != want {
		t.Fatalf("schemas not alphabetically sorted: first = %q, want %q", got, want)
	}
}

// firstDivergence returns the byte offset of the first difference between two
// strings, or -1 when they are identical.
func firstDivergence(a, b string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return min(len(a), len(b))
	}
	return -1
}

// TestSystemPromptByteStable locks the provider-shape invariant: two identical
// runners over the same config must produce byte-identical system prompts and
// tool-schema JSON, and the environment block is free of rollover dates. Any
// nondeterminism here (time-dependent text, unstable iteration, probe flaps)
// cold-starts the provider prefix cache for the whole session.
func TestSystemPromptByteStable(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(staticTestTool{name: "aa_first"})
	registry.Register(staticTestTool{name: "bb_second"})

	build := func() (string, string) {
		runner := New(Config{System: "base system prompt", Tools: registry})
		sys := runner.systemBlock(nil, registry.Schemas(nil))
		schemaJSON, err := json.Marshal(registry.Schemas(nil))
		if err != nil {
			t.Fatal(err)
		}
		return sys, string(schemaJSON)
	}

	sys1, tools1 := build()
	sys2, tools2 := build()
	if sys1 != sys2 {
		t.Fatalf("system prompt drifted between identical builds:\n%q\nvs\n%q", sys1, sys2)
	}
	if tools1 != tools2 {
		t.Fatalf("tool schema JSON drifted between identical builds:\n%q\nvs\n%q", tools1, tools2)
	}
}

// TestEnvironmentBlockHasNoRolloverDate guards the prompt builder: the
// environment block must not embed a wall-clock date (a midnight rollover
// would invalidate the cached prefix) and must be byte-stable across calls.
func TestEnvironmentBlockHasNoRolloverDate(t *testing.T) {
	dir := t.TempDir()
	block := Environment(dir, "deepseek-model", "open", "")
	if strings.Contains(block, "Today") || strings.Contains(block, "Date:") {
		t.Fatalf("environment block must not contain a rollover date: %q", block)
	}
	again := Environment(dir, "deepseek-model", "open", "")
	if block != again {
		t.Fatalf("environment block not byte-stable:\n%q\nvs\n%q", block, again)
	}
}

// TestFirstTurnPinnedAcrossHeadTrim verifies the first (small) user turn is
// pinned verbatim beside the trim sentinel so the original task stays visible
// after the head is folded, and that growth afterwards remains append-only.
func TestFirstTurnPinnedAcrossHeadTrim(t *testing.T) {
	r := New(Config{})
	enc := tokens.EncodingCl100kBase
	first := provider.UserText("the goal: fix the URL fetch bug")
	filler := provider.UserText(strings.Repeat("old context ", 4000))

	view1 := r.retainStable([]provider.Message{first, filler}, 5000, enc)
	if !r.trimEngaged {
		t.Fatal("expected trimming to engage on the first over-budget call")
	}
	if len(view1) < 2 {
		t.Fatalf("pinned view too short: %d", len(view1))
	}
	if !sameMessage(view1[0], first) {
		t.Fatalf("first user turn not pinned at view[0]: %#v", view1[0])
	}
	if !strings.Contains(view1[1].Text(), "trimmed") {
		t.Fatalf("view[1] is not the trim sentinel: %#v", view1[1])
	}

	// Once the pin has engaged, further growth must remain append-only.
	grown := append([]provider.Message{first, filler}, provider.UserText("check two"), provider.UserText("check three"))
	view2 := r.retainStable(grown, 5000, enc)
	for i := range view1 {
		if i >= len(view2) || !sameMessage(view1[i], view2[i]) {
			t.Fatalf("view1[%d] rewritten after pinned growth: %#v vs %#v", i, view1[i], view2[i])
		}
	}

	// A giant first paste is never pinned: it would push the pinned prefix
	// far past the budget it was folded under.
	big := provider.UserText(strings.Repeat("a-noise ", 4000))
	r2 := New(Config{})
	view := r2.retainStable([]provider.Message{big, filler}, 300, enc)
	if !r2.trimEngaged {
		t.Fatal("expected trimming to engage for the big-paste case")
	}
	if sameMessage(view[0], big) {
		t.Fatal("oversized first paste must not be pinned verbatim")
	}
}

// TestArchiveTrimmedWritesJSONL verifies the head-trim archive: with
// ArchiveDir set, dropped originals land in a timestamped JSONL (one record
// per message); without ArchiveDir the trim runs unchanged and writes nothing.
func TestArchiveTrimmedWritesJSONL(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{ArchiveDir: filepath.Join(dir, "archive"), SessionID: "sess-1"})
	enc := tokens.EncodingCl100kBase
	first := provider.UserText("hello, world")
	filler := provider.UserText(strings.Repeat("noise ", 600))

	r.retainStable([]provider.Message{first, filler}, 400, enc)
	entries, err := os.ReadDir(filepath.Join(dir, "archive"))
	if err != nil {
		t.Fatalf("archive dir missing: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected an archive file after a head-trim")
	}
	lines := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "archive", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		lines += strings.Count(string(data), "\n")
	}
	if lines == 0 {
		t.Fatal("archive file is empty")
	}

	// No archive configured: nothing written, no failure.
	r2 := New(Config{})
	r2.retainStable([]provider.Message{first, filler}, 400, enc)
}

// TestSummarizeToolArgsDeterministic guards the distill transcript: large
// tool inputs fold to a stable key+size summary (never re-broadcast verbatim),
// and identical inputs render byte-identical output.
func TestSummarizeToolArgsDeterministic(t *testing.T) {
	input := json.RawMessage(`{"prompt":"` + strings.Repeat("delegate-work ", 200) + `","cwd":"/tmp"}`)
	a := summarizeToolArgs(input)
	b := summarizeToolArgs(input)
	if a != b {
		t.Fatalf("summarizeToolArgs not deterministic: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "[") || !strings.Contains(a, "bytes; keys:") {
		t.Fatalf("expected a size summary but got: %q", a)
	}
	small := summarizeToolArgs(json.RawMessage(`{"cwd":"/tmp"}`))
	if !strings.HasPrefix(small, "{") || strings.HasPrefix(small, "[") {
		t.Fatalf("small inputs must pass through verbatim: %q", small)
	}
}

// staticTestTool is a minimal Tool double that keeps schema ordering stable.
type staticTestTool struct {
	name string
}

func (t staticTestTool) Name() string        { return t.name }
func (t staticTestTool) Description() string { return "a static test tool" }
func (t staticTestTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t staticTestTool) ReadOnly() bool { return true }
func (t staticTestTool) Run(_ context.Context, _ tools.Context, _ json.RawMessage) (tools.Result, error) {
	return tools.Result{Output: "ok"}, nil
}
