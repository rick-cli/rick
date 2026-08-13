package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"rick/internal/config"
	"rick/internal/provider"
	"rick/internal/session"
)

func TestAutoCompactCooldownExpires(t *testing.T) {
	model := &Model{
		deps:      Deps{Loaded: &config.Loaded{}},
		ctxWindow: 100,
		history:   make([]provider.Message, 7),
		usage:     session.Usage{Input: 80},
	}

	model.lastAutoCompact = time.Now()
	model.maybeAutoCompact()
	if model.autoCompactPending {
		t.Fatal("auto-compaction re-triggered during cooldown")
	}

	model.lastAutoCompact = time.Now().Add(-autoCompactCooldown - time.Second)
	model.maybeAutoCompact()
	if !model.autoCompactPending {
		t.Fatal("auto-compaction did not trigger after cooldown")
	}
}

func TestContextCompactionThresholdUsesConfiguredReserve(t *testing.T) {
	if got := contextCompactionThreshold(200000, 24000); got != 176000 {
		t.Fatalf("threshold = %d, want 176000", got)
	}
	if got := contextCompactionThreshold(100, 0); got != 70 {
		t.Fatalf("default threshold = %d, want 70", got)
	}
	if got := contextCompactionThreshold(100, 100); got != 0 {
		t.Fatalf("full reserve threshold = %d, want 0", got)
	}
	if got := compactionTokenLimit(16384); got != compactionMaxTokens {
		t.Fatalf("compaction token limit = %d, want %d", got, compactionMaxTokens)
	}
	if got := compactionTokenLimit(512); got != 512 {
		t.Fatalf("configured small token limit = %d, want 512", got)
	}
}

func TestFailedAutoCompactProviderResolutionDoesNotStartCompaction(t *testing.T) {
	model := &Model{
		deps:               Deps{Loaded: &config.Loaded{}},
		tx:                 newTranscript(),
		history:            make([]provider.Message, 7),
		ctxWindow:          100,
		autoCompactPending: true,
	}

	_, cmd := model.cmdCompact()
	if cmd != nil {
		t.Fatal("compaction command created without a provider")
	}
	if model.compactionActive {
		t.Fatal("failed provider resolution left compaction marked active")
	}
}

func TestOverlappingCompactionIsRejected(t *testing.T) {
	model := &Model{compactionActive: true}
	_, cmd := model.cmdCompact()
	if cmd != nil {
		t.Fatal("overlapping compaction unexpectedly created a command")
	}
}

func TestStaleCompactionResultIsIgnored(t *testing.T) {
	model := &Model{compactionActive: true, compactionRunID: 2}
	model.Update(compactDoneMsg{runID: 1, summary: "stale"})
	if !model.compactionActive {
		t.Fatal("stale compaction result changed active state")
	}
}

func TestAddProviderUsagePreservesCacheAccounting(t *testing.T) {
	var total provider.Usage
	addProviderUsage(&total, provider.Usage{
		InputTokens: 11, OutputTokens: 7, CacheReadTokens: 13, CacheWriteTokens: 17,
	})
	addProviderUsage(&total, provider.Usage{
		InputTokens: 19, OutputTokens: 23, CacheReadTokens: 29, CacheWriteTokens: 31,
	})

	want := provider.Usage{InputTokens: 30, OutputTokens: 30, CacheReadTokens: 42, CacheWriteTokens: 48}
	if total != want {
		t.Fatalf("usage = %#v, want %#v", total, want)
	}
}

func TestSystemPromptPlacesProjectContextBeforeVolatileEnvironment(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "RICK.md"), []byte("project conventions"), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := buildSystemPrompt("build", "test-model", root, root, config.Config{}, nil, "")
	projectIndex := strings.Index(prompt, "## Project instructions")
	environmentIndex := strings.Index(prompt, "## Environment")
	if projectIndex < 0 || environmentIndex < 0 {
		t.Fatalf("prompt missing stable or volatile section: %q", prompt)
	}
	if projectIndex > environmentIndex {
		t.Fatalf("project context follows environment: project=%d environment=%d", projectIndex, environmentIndex)
	}
}

func TestCompactHistoryKeepsOnlyLatestThinkingMessage(t *testing.T) {
	history := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "thinking", Text: "old"}, {Type: "text", Text: "answer"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "thinking", Text: "latest"}, {Type: "text", Text: "final"}}},
	}
	compacted := compactHistory(history)
	if len(compacted[0].Content) != 1 || compacted[0].Content[0].Type != "text" {
		t.Fatalf("old thinking was retained: %#v", compacted[0].Content)
	}
	if len(compacted[1].Content) != 2 || compacted[1].Content[0].Text != "latest" {
		t.Fatalf("latest thinking was not retained: %#v", compacted[1].Content)
	}
}

type compactionTestProvider struct {
	events []provider.Event
	close  bool
}

func (p *compactionTestProvider) Name() string { return "fake" }

func (p *compactionTestProvider) Models() []provider.ModelInfo {
	return []provider.ModelInfo{{ID: "model", Name: "model"}}
}

func (p *compactionTestProvider) Stream(ctx context.Context, req provider.Request, ch chan<- provider.Event) {
	for _, event := range p.events {
		select {
		case ch <- event:
		case <-ctx.Done():
			return
		}
	}
	if p.close {
		close(ch)
	}
}

func newCompactionTestModel(prov provider.Provider) *Model {
	return &Model{
		deps: Deps{
			Loaded:    &config.Loaded{},
			Providers: map[string]provider.Provider{"fake": prov},
		},
		modelID: "fake/model",
		history: make([]provider.Message, 5),
		tx:      newTranscript(),
	}
}

func runCompactionCommand(t *testing.T, cmd tea.Cmd) compactDoneMsg {
	t.Helper()
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()
	select {
	case msg := <-result:
		compact, ok := msg.(compactDoneMsg)
		if !ok {
			t.Fatalf("compaction returned %T, want compactDoneMsg", msg)
		}
		return compact
	case <-time.After(2 * time.Second):
		t.Fatal("compaction waited for channel close after receiving a terminal event")
		return compactDoneMsg{}
	}
}

func TestCompactionStopsAtDoneWithoutWaitingForChannelClose(t *testing.T) {
	prov := &compactionTestProvider{events: []provider.Event{
		{Kind: provider.EventText, Text: "summary"},
		{Kind: provider.EventDone},
	}}
	model := newCompactionTestModel(prov)
	_, cmd := model.cmdCompact()
	if cmd == nil {
		t.Fatal("compaction command was not created")
	}
	result := runCompactionCommand(t, cmd)
	if result.err != nil || result.summary != "summary" {
		t.Fatalf("compaction result = %#v, want a successful summary", result)
	}
	model.compactionCancel = nil
}

func TestCompactionReportsProviderStreamEndingWithoutDone(t *testing.T) {
	prov := &compactionTestProvider{events: nil, close: true}
	model := newCompactionTestModel(prov)
	_, cmd := model.cmdCompact()
	if cmd == nil {
		t.Fatal("compaction command was not created")
	}
	result := runCompactionCommand(t, cmd)
	if result.err == nil || !strings.Contains(result.err.Error(), "without a completion event") {
		t.Fatalf("compaction error = %v, want missing completion error", result.err)
	}
}

func TestCompactionErrorReleasesChatGate(t *testing.T) {
	model := &Model{
		compactionActive: true,
		compactionRunID:  7,
		tx:               newTranscript(),
	}
	model.Update(compactDoneMsg{runID: 7, err: errors.New("provider failed")})
	if model.compactionActive {
		t.Fatal("provider failure left compaction active")
	}
}

func TestCancelCompactionInvalidatesLateResult(t *testing.T) {
	model := &Model{compactionActive: true, compactionRunID: 3}
	cancelled := false
	model.compactionCancel = func() { cancelled = true }
	model.cancelCompaction()
	if !cancelled || model.compactionActive || model.compactionRunID != 4 {
		t.Fatalf("cancelCompaction state = active:%v run:%d cancelled:%v", model.compactionActive, model.compactionRunID, cancelled)
	}
}

// TestCompactSurfaceStabilityPinsHarnessRecheck pins the harness-style
// surface-stability re-check: the compact snapshot matches the live head when
// nothing changed, and mismatches when the conversation grew while the
// summarizer ran — so the commit is rejected against a stale span.
func TestCompactSurfaceStabilityPinsHarnessRecheck(t *testing.T) {
	history := []provider.Message{
		provider.UserText("turn 1"),
		provider.AssistantText("reply 1"),
		provider.UserText("turn 2"),
		provider.AssistantText("reply 2"),
		provider.UserText("turn 3"),
		provider.AssistantText("reply 3"),
		provider.UserText("current"),
	}
	snapshot := compactSurfaceSnapshot(history)
	if snapshot == "" {
		t.Fatal("compact snapshot is empty for a multi-message history")
	}
	if !compactSurfaceStable(snapshot, history) {
		t.Fatal("identical history reported unstable")
	}
	// The conversation grew while the summarizer ran: the head (oldest
	// len-keep messages) is unchanged, so the snapshot still matches — only
	// the tail grew, which is append-only and cache-safe.
	grown := append(append([]provider.Message(nil), history...), provider.UserText("new question"))
	if !compactSurfaceStable(snapshot, grown) {
		t.Fatal("append-only growth should keep the compaction span stable")
	}
	// The head changed (a message was inserted mid-history while the
	// summarizer ran): the snapshot no longer matches, rejecting the commit.
	changed := append([]provider.Message(nil), history...)
	changed[0] = provider.UserText("rewritten turn 1")
	if compactSurfaceStable(snapshot, changed) {
		t.Fatal("rewritten head reported stable; the compaction span moved")
	}
	// Tiny history: no snapshot, always stable (nothing to verify).
	if !compactSurfaceStable("", history[:3]) {
		t.Fatal("empty snapshot should be trivially stable")
	}
}
