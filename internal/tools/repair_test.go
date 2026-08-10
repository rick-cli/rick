package tools

import (
	"context"
	"strings"
	"testing"
)

// The todowrite schema exercises the array-field repairs (the "tasks"-style
// shape the harness-engineering guidance targets).
var todowriteSchema = func() map[string]any {
	return (TodoWriteTool{}).Schema()
}()

func TestRepairDecodeLeavesValidInputUntouched(t *testing.T) {
	var a todoArgs
	var note string
	opts := &RepairOpts{Note: &note, Family: "deepseek"}
	in := jsonArgs(map[string]any{"todos": []any{
		map[string]any{"id": "t1", "content": "do it", "status": "pending"},
	}})
	if err := RepairDecode(in, &a, todowriteSchema, opts); err != nil {
		t.Fatalf("RepairDecode errored on valid input: %v", err)
	}
	if note != "" {
		t.Fatalf("valid input must not be flagged as repaired, got note %q", note)
	}
	if len(a.Todos) != 1 || a.Todos[0].ID != "t1" {
		t.Fatalf("decoded wrong todos: %+v", a.Todos)
	}
}

func TestRepairDecodeStringifiedArray(t *testing.T) {
	// DeepSeek-family quirk: "[\"a\",\"b\"]" as a JSON string instead of an
	// array. Must parse to a real array, never double-wrap.
	var a todoArgs
	var note string
	opts := &RepairOpts{Note: &note}
	in := jsonArgs(map[string]any{"todos": `[{"id":"t1","content":"x","status":"pending"}]`})
	if err := RepairDecode(in, &a, todowriteSchema, opts); err != nil {
		t.Fatalf("RepairDecode failed on stringified array: %v", err)
	}
	if len(a.Todos) != 1 || a.Todos[0].ID != "t1" {
		t.Fatalf("stringified array not parsed: %+v", a.Todos)
	}
	if !strings.Contains(note, "parsed") {
		t.Fatalf("note should mention the parse, got %q", note)
	}
}

func TestRepairDecodeBareStringWrappedInArray(t *testing.T) {
	// A bare string for a []string array field is wrapped. (For arrays of
	// structs the wrap cannot re-decode, so the original error is returned —
	// see TestRepairDecodeUnrepairableArrayReturnsError.)
	var a struct {
		Items []string `json:"items"`
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{"type": "array"},
		},
	}
	var note string
	opts := &RepairOpts{Note: &note}
	in := jsonArgs(map[string]any{"items": "one"})
	if err := RepairDecode(in, &a, schema, opts); err != nil {
		t.Fatalf("RepairDecode failed on bare string: %v", err)
	}
	if len(a.Items) != 1 || a.Items[0] != "one" {
		t.Fatalf("bare string not wrapped: %+v", a.Items)
	}
	if !strings.Contains(note, "wrapped") {
		t.Fatalf("note should mention the wrap, got %q", note)
	}
}

func TestRepairDecodeUnrepairableArrayReturnsError(t *testing.T) {
	// A bare string wrapped into an array of structs cannot re-decode (the
	// item is a string, not an object). The repair must give up and surface
	// the original strict error rather than silently corrupt.
	var a todoArgs
	var note string
	opts := &RepairOpts{Note: &note}
	in := jsonArgs(map[string]any{"todos": "t1"})
	if err := RepairDecode(in, &a, todowriteSchema, opts); err == nil {
		t.Fatalf("unrepairable array must still error, note=%q", note)
	}
	if note != "" {
		t.Fatalf("no note on a failed repair, got %q", note)
	}
}

func TestRepairDecodeOrderingParseBeforeWrap(t *testing.T) {
	// The corruption the ordering rule prevents: `["a","b"]` (stringified
	// array) must become a real array, NOT `["[\"a\",\"b\"]"]` (a wrapped
	// string containing JSON). The todo item ids prove the inner parse ran.
	var a todoArgs
	var note string
	opts := &RepairOpts{Note: &note}
	in := jsonArgs(map[string]any{"todos": `[{"id":"t1","content":"a","status":"pending"}]`})
	if err := RepairDecode(in, &a, todowriteSchema, opts); err != nil {
		t.Fatalf("RepairDecode failed: %v", err)
	}
	if len(a.Todos) != 1 || a.Todos[0].ID != "t1" {
		t.Fatalf("stringified array must be parsed, not wrapped: %+v", a.Todos)
	}
}

func TestRepairDecodeEmptyObjectPlaceholder(t *testing.T) {
	var a todoArgs
	var note string
	opts := &RepairOpts{Note: &note}
	in := jsonArgs(map[string]any{"todos": map[string]any{}})
	if err := RepairDecode(in, &a, todowriteSchema, opts); err != nil {
		t.Fatalf("RepairDecode failed on empty placeholder: %v", err)
	}
	if len(a.Todos) != 0 {
		t.Fatalf("empty object placeholder should become an empty array, got %+v", a.Todos)
	}
}

func TestRepairDecodeSingleObjectWrappedInArray(t *testing.T) {
	var a todoArgs
	var note string
	opts := &RepairOpts{Note: &note}
	in := jsonArgs(map[string]any{"todos": map[string]any{"id": "t1", "content": "x", "status": "pending"}})
	if err := RepairDecode(in, &a, todowriteSchema, opts); err != nil {
		t.Fatalf("RepairDecode failed on single object: %v", err)
	}
	if len(a.Todos) != 1 || a.Todos[0].ID != "t1" {
		t.Fatalf("single object should be wrapped in array: %+v", a.Todos)
	}
}

func TestRepairDecodeNullOptionalFieldOmitted(t *testing.T) {
	// Go's encoding/json tolerates null for every value type (zero value,
	// no error), so a null optional field never reaches the repair path —
	// the call just decodes. This pins that behavior: nulls are not a
	// failure class in rick.
	var a readArgs
	var note string
	opts := &RepairOpts{Note: &note}
	in := jsonArgs(map[string]any{"path": "x.go", "offset": nil, "limit": nil})
	if err := RepairDecode(in, &a, (ReadTool{}).Schema(), opts); err != nil {
		t.Fatalf("RepairDecode failed on null optionals: %v", err)
	}
	if a.Offset != 0 || a.Limit != 0 {
		t.Fatalf("null fields should decode to zero: %+v", a)
	}
}

func TestRepairDecodeNullRequiredFieldZeroValue(t *testing.T) {
	// Same story for a required array field: null decodes to nil without
	// error, so no repair is needed — the tool then reports "no tasks".
	var a todoArgs
	var note string
	opts := &RepairOpts{Note: &note}
	in := jsonArgs(map[string]any{"todos": nil})
	if err := RepairDecode(in, &a, todowriteSchema, opts); err != nil {
		t.Fatalf("RepairDecode failed on null required: %v", err)
	}
	if a.Todos != nil {
		t.Fatalf("null required field should stay nil, got %+v", a.Todos)
	}
}

func TestRepairDecodeUnknownFieldStillRejected(t *testing.T) {
	// Strictness is preserved: an unknown (typo'd) field is never repaired
	// away. The typo "commmand" must still fail.
	var a bashArgs
	var note string
	opts := &RepairOpts{Note: &note}
	in := jsonArgs(map[string]any{"command": "echo ok", "commmand": "typo"})
	if err := RepairDecode(in, &a, (BashTool{}).Schema(), opts); err == nil {
		t.Fatalf("unknown field must still be rejected; note=%q", note)
	}
}

func TestRepairDecodeStringFieldWithJSONContentUntouched(t *testing.T) {
	// A string-typed field that merely looks like JSON must never be
	// unwrapped: old_string "[\"a\",\"b\"]" is a literal search string.
	var a editArgs
	var note string
	opts := &RepairOpts{Note: &note}
	in := jsonArgs(map[string]any{
		"path": "x.go", "old_string": `["a","b"]`, "new_string": "c",
	})
	if err := RepairDecode(in, &a, (EditTool{}).Schema(), opts); err != nil {
		t.Fatalf("RepairDecode failed: %v", err)
	}
	if a.OldString != `["a","b"]` {
		t.Fatalf("string field was mutated: %q", a.OldString)
	}
	if note != "" {
		t.Fatalf("no repair should be reported for a valid string field, got %q", note)
	}
}

func TestRepairDecodeNumberStringCoercionFamilyGated(t *testing.T) {
	// "5" as a string for a number field: repaired only for families that
	// have the quirk.
	readSchema := (ReadTool{}).Schema()

	var a readArgs
	var note string
	in := jsonArgs(map[string]any{"path": "x.go", "limit": "5"})

	// Unknown family: no number-string quirk, strict failure.
	opts := &RepairOpts{Note: &note}
	if err := RepairDecode(in, &a, readSchema, opts); err == nil {
		t.Fatalf("number-string coercion must not apply outside known families")
	}
	if a.Limit != 0 {
		t.Fatalf("limit should stay unset: %d", a.Limit)
	}

	// DeepSeek family: coerced.
	opts = &RepairOpts{Note: &note, Family: "deepseek"}
	if err := RepairDecode(in, &a, readSchema, opts); err != nil {
		t.Fatalf("RepairDecode failed with family quirk: %v", err)
	}
	if a.Limit != 5 {
		t.Fatalf("limit should be coerced to 5, got %d", a.Limit)
	}
	if !strings.Contains(note, "coerced") {
		t.Fatalf("note should mention coercion, got %q", note)
	}
}

func TestFamilyForModel(t *testing.T) {
	cases := map[string]string{
		"deepseek/deepseek-v4-flash": "deepseek",
		"openai/gpt-5":               "",
		"glm/glm-4.5":                "glm",
		"qwen/qwen3":                 "qwen",
		"anthropic/claude-sonnet":    "",
		"":                           "",
	}
	for in, want := range cases {
		if got := FamilyForModel(in); got != want {
			t.Errorf("FamilyForModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnwrapMarkdownLink(t *testing.T) {
	cases := map[string]string{
		"[notes.md](http://notes.md)":  "notes.md",
		"[src/main.go](https://x.dev)": "src/main.go",
		"src/main.go":                  "src/main.go",
		"  [a.go](http://x)  ":         "a.go",
		"[not a link](relative/path)":  "[not a link](relative/path)", // non-http target
		"plain text with (parens)":     "plain text with (parens)",
		"[double](http://a)(http://b)": "[double](http://a)(http://b)",
	}
	for in, want := range cases {
		if got := unwrapMarkdownLink(in); got != want {
			t.Errorf("unwrapMarkdownLink(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepairNoteSurfacesToModelAndMeta(t *testing.T) {
	res := repairNote(Result{Output: "done"}, "parsed todos from string to array")
	if !strings.Contains(res.Output, "<repaired: parsed todos") {
		t.Fatalf("repair note not surfaced to model: %q", res.Output)
	}
	if res.IsError {
		t.Fatal("a repaired call must never be an error")
	}
	if res.Meta["repaired"] != "parsed todos from string to array" {
		t.Fatalf("meta missing repair note: %+v", res.Meta)
	}
}

func TestTodoWriteRepairsEndToEnd(t *testing.T) {
	// The full pipeline: a malformed todowrite call is repaired, runs, and
	// the note rides in Meta + output.
	store := NewTodoStore()
	tool := TodoWriteTool{Store: store}
	in := jsonArgs(map[string]any{"todos": `[{"id":"t1","content":"a","status":"pending"}]`})
	var note string
	tc := Context{Repair: &RepairOpts{Note: &note}}
	res, err := tool.Run(context.Background(), tc, in)
	if err != nil {
		t.Fatalf("todowrite errored: %v", err)
	}
	if res.IsError {
		t.Fatalf("repaired todowrite must succeed, got %q", res.Output)
	}
	if note == "" {
		t.Fatal("expected a repair note")
	}
	items := store.Items()
	if len(items) != 1 || items[0].ID != "t1" {
		t.Fatalf("todos not stored: %+v", items)
	}
}
