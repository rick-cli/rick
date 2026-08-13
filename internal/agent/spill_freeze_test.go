package agent

import (
	"context"
	"encoding/json"
	"testing"

	"rick/internal/provider"
	"rick/internal/tools"
	"rick/pkg/contextbudget"
)

// schemaChangingTool returns a different schema each call, simulating a
// mid-run registry mutation that must not change the frozen wire prefix.
type schemaChangingTool struct{ call int }

func (t *schemaChangingTool) Name() string        { return "schema_changer" }
func (t *schemaChangingTool) ReadOnly() bool      { return true }
func (t *schemaChangingTool) Description() string { return "changes schema" }
func (t *schemaChangingTool) Schema() map[string]any {
	t.call++
	return map[string]any{"type": "object", "properties": map[string]any{
		"v": map[string]any{"type": "integer", "description": "call " + string(rune('0'+t.call))},
	}}
}
func (t *schemaChangingTool) Run(_ context.Context, _ tools.Context, _ json.RawMessage) (tools.Result, error) {
	return tools.Result{Output: "ok"}, nil
}

// TestFreezeSchemasPinsToolList verifies the structural freeze: after the
// first buildRequest, the provider-facing tool schema list is byte-identical
// for every later turn even if the underlying registry mutates.
func TestFreezeSchemasPinsToolList(t *testing.T) {
	reg := tools.NewRegistry()
	changer := &schemaChangingTool{}
	reg.Register(changer)

	runner := New(Config{
		Tools:  reg,
		Budget: contextbudget.New(contextbudget.Options{}),
	})
	schemas := runner.freezeSchemas(reg.Schemas(nil))
	first := marshalBytes(schemas)

	// Simulate registry churn: a new schema each build.
	for turn := 0; turn < 3; turn++ {
		cur := runner.freezeSchemas(reg.Schemas(nil))
		if got := marshalBytes(cur); string(got) != string(first) {
			t.Fatalf("turn %d: frozen schemas changed bytes:\n%s\nvs\n%s", turn, got, first)
		}
	}
}

// boundaryTool is a tool whose result is large enough to trigger the spill
// path so the runner records a boundary.
type boundaryTool struct{}

func (boundaryTool) Name() string        { return "big_output" }
func (boundaryTool) ReadOnly() bool      { return true }
func (boundaryTool) Description() string { return "emits a big output" }
func (boundaryTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (boundaryTool) Run(_ context.Context, _ tools.Context, _ json.RawMessage) (tools.Result, error) {
	return tools.Result{Output: "x"}, nil
}

// TestCapToolOutputSpillsOversizedResult verifies the item-3 spill: an
// oversized tool result is persisted to the content-addressed store and the
// model sees a bounded preview plus a retrieval locator, not the raw bytes.
func TestCapToolOutputSpillsOversizedResult(t *testing.T) {
	store := contextbudget.New(contextbudget.Options{})
	runner := New(Config{
		SpillBytes:       4096,
		MaxToolResultBytes: 1024,
		Budget:           store,
	})
	output := repeatString("spill-payload-", 5000) // > 4 KiB
	modelOutput, stats := runner.capToolOutput(provider.ToolCall{ID: "c1", Name: "bash"}, output, false)
	if stats == nil || stats.Stage != "spill" {
		t.Fatalf("want spill stage, got %+v", stats)
	}
	if len(modelOutput) >= len(output) {
		t.Fatal("spill preview must be smaller than the raw output")
	}
	// The locator must be present and the full payload retrievable.
	key := ""
	for i := 0; i < len(modelOutput); i++ {
		if prefix := "key="; i+len(prefix) <= len(modelOutput) && modelOutput[i:i+len(prefix)] == prefix {
			rest := modelOutput[i+len(prefix):]
			for j := 0; j < len(rest); j++ {
				if rest[j] == ']' || rest[j] == '\n' || rest[j] == ' ' {
					key = rest[:j]
					break
				}
			}
			break
		}
	}
	if key == "" {
		t.Fatalf("spill preview missing retrieval locator: %q", modelOutput)
	}
	if got, ok := store.StoredPayload(key); !ok || got != output {
		t.Fatal("spilled payload not retrievable via content address")
	}
}
