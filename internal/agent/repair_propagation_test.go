package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"rick/internal/provider"
	"rick/internal/tools"
)

// repairTool exercises the stringified-array repair: it takes a []string
// field and would hard-fail on a string value without the repair pass.
type repairTool struct{}

func (repairTool) Name() string        { return "repair_me" }
func (repairTool) Description() string { return "takes an items array" }
func (repairTool) ReadOnly() bool      { return true }
func (repairTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{"type": "array"},
		},
	}
}
func (repairTool) Run(_ context.Context, tc tools.Context, in json.RawMessage) (tools.Result, error) {
	var a struct {
		Items []string `json:"items"`
	}
	if err := tools.RepairDecode(in, &a, repairTool{}.Schema(), tc.Repair); err != nil {
		return tools.Errf("invalid arguments: %v", err), nil
	}
	return tools.RepairNoteResult(tools.Result{
		Output: "items=" + strings.Join(a.Items, ","),
		Title:  "repair_me",
	}, tools.NoteOf(tc)), nil
}

// repairProvider emits a tool call with a stringified array (a DeepSeek-
// family shape error) on the first turn, then finishes on the second.
type repairProvider struct {
	calls int
}

func (p *repairProvider) Name() string                 { return "repair-provider" }
func (p *repairProvider) Models() []provider.ModelInfo { return nil }
func (p *repairProvider) Stream(_ context.Context, _ provider.Request, ch chan<- provider.Event) {
	defer close(ch)
	p.calls++
	if p.calls == 1 {
		ch <- provider.Event{Kind: provider.EventToolCall, ToolCall: &provider.ToolCall{
			ID: "call-1", Name: "repair_me", Input: json.RawMessage(`{"items":"[\"a\",\"b\"]"}`),
		}}
		ch <- provider.Event{Kind: provider.EventDone, StopReason: "tool_use"}
		return
	}
	ch <- provider.Event{Kind: provider.EventText, Text: "done"}
	ch <- provider.Event{Kind: provider.EventDone, StopReason: "end_turn"}
}

func TestRunnerSurfacesRepairOnToolEvent(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(repairTool{})
	runner := New(Config{
		Provider: &repairProvider{},
		Model:    "deepseek/deepseek-v4-flash",
		Tools:    registry,
		MaxTurns: 3,
	})

	out := make(chan Event, 64)
	_, err := runner.Run(context.Background(), []provider.Message{provider.UserText("run")}, out)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	repaired := ""
	var toolOutput string
	for ev := range out {
		if ev.Tool != nil && ev.Tool.Name == "repair_me" {
			if ev.Tool.Repaired != "" {
				repaired = ev.Tool.Repaired
			}
			toolOutput = ev.Tool.Output
		}
	}
	if repaired == "" {
		t.Fatal("expected ToolEvent.Repaired to be set")
	}
	if !strings.Contains(toolOutput, "<repaired:") {
		t.Fatalf("tool output should surface the repair note, got %q", toolOutput)
	}
	if !strings.Contains(toolOutput, "items=a,b") {
		t.Fatalf("stringified array should have been parsed, got %q", toolOutput)
	}
}
