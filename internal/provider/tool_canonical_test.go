package provider

import (
	"encoding/json"
	"testing"
)

// TestCanonicalToolSchemasStable locks the wire-tools byte-stability
// invariant: reordered or key-shuffled inputs must canonicalize to
// identical schemas and names in alphabetical order, so the provider's
// cached tools block can never flip mid-session.
func TestCanonicalToolSchemasStable(t *testing.T) {
	insertion := []ToolSchema{
		{Name: "write"},
		{Name: "bash"},
		{Name: "read", InputSchema: map[string]any{
			"type": "object", "required": []any{"path"},
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
		}},
	}
	shuffledKeyOrder := []ToolSchema{
		{Name: "bash"},
		{Name: "write"},
		{Name: "read", InputSchema: map[string]any{
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []any{"path"},
			"type":       "object",
		}},
	}

	first := CanonicalToolSchemas(insertion)
	second := CanonicalToolSchemas(shuffledKeyOrder)
	for i := range 3 {
		if first[i].Name != second[i].Name {
			t.Fatalf("canonical order drifted at %d: %q vs %q", i, first[i].Name, second[i].Name)
		}
	}
	if first[0].Name != "bash" {
		t.Fatalf("first canonical tool = %q, want bash", first[0].Name)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatalf("canonical schemas not byte-identical:\n%s\nvs\n%s", a, b)
	}
}

// TestCanonicalToolSchemasPreservesOrderedSlices checks that nested arrays
// of objects survive canonicalization element-wise.
func TestCanonicalToolSchemasPreservesOrderedSlices(t *testing.T) {
	in := []ToolSchema{{
		Name: "zoom",
		InputSchema: map[string]any{
			"type":  "object",
			"items": []any{map[string]any{"b": 1, "a": 2}},
		},
	}}
	out := CanonicalToolSchemas(in)
	got := out[0].InputSchema["items"].([]any)
	if got[0].(map[string]any)["a"] != 2 {
		t.Fatalf("nested slice element lost: %#v", got)
	}
}
