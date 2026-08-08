package contextbudget

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"rick/internal/provider"
)

func toolUse(id string) provider.Message {
	return provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.ContentBlock{
			{Type: "tool_use", ID: id, Name: "bash"},
		},
	}
}

func toolResult(id, content string) provider.Message {
	return provider.Message{
		Role: provider.RoleUser,
		Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: id, Content: content},
		},
	}
}

func TestVerifyPairSafetyAcceptsValidTranscript(t *testing.T) {
	messages := []provider.Message{
		provider.UserText("run it"),
		toolUse("c1"),
		toolResult("c1", "ok"),
		toolUse("c2"),
		toolResult("c2", "done"),
	}
	if err := VerifyPairSafety(messages); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVerifyPairSafetyRejectsOrphanedResult(t *testing.T) {
	messages := []provider.Message{
		provider.UserText("run it"),
		toolResult("orphan", "no tool_use here"),
	}
	if err := VerifyPairSafety(messages); err == nil {
		t.Fatal("expected orphaned tool_result to be rejected")
	}
}

func TestVerifyPairSafetyRejectsUnpairedToolUse(t *testing.T) {
	messages := []provider.Message{
		provider.UserText("run it"),
		toolUse("c1"),
	}
	if err := VerifyPairSafety(messages); err == nil {
		t.Fatal("expected dangling tool_use to be rejected")
	}
}

func TestApplyDedupReplacesRepeatedPayloads(t *testing.T) {
	payload := strings.Repeat(`{"data":"`+"abcdefgh"+`"}`, 300) // > MinDedupBytes
	messages := []provider.Message{
		provider.UserText("first"),
		toolUse("c1"),
		toolResult("c1", payload),
		provider.UserText("again"),
		toolUse("c2"),
		toolResult("c2", payload),
	}
	budget := New(Options{})
	result := budget.ApplyDedup(messages)

	if result.Replaced != 1 {
		t.Fatalf("Replaced = %d, want 1", result.Replaced)
	}
	if result.SavedBytes <= 0 {
		t.Fatalf("SavedBytes = %d, want > 0", result.SavedBytes)
	}

	// First occurrence is untouched; second is a self-contained reference and
	// the original is retrievable via the content address.
	first := result.View[2].Content[0].Content
	second := result.View[5].Content[0].Content
	if first != payload {
		t.Fatal("first occurrence was unexpectedly replaced")
	}
	if !strings.Contains(second, "duplicate payload sha256:") {
		t.Fatalf("second occurrence not deduplicated: %s", second[:60])
	}
	hash := Hash(payload)
	original, ok := budget.StoredPayload(hash)
	if !ok || original != payload {
		t.Fatal("content-addressed original is not retrievable")
	}
}

func TestApplyDedupSupersedesTrimmedOriginal(t *testing.T) {
	budget := New(Options{})
	payload := strings.Repeat("identical large payload ", 200)

	// Turn 1: payload appears twice; only the second is collapsed.
	turn1 := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "tool_use", ID: "c1", Name: "bash"}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "tool_result", ToolUseID: "c1", Content: payload}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "tool_use", ID: "c2", Name: "bash"}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "tool_result", ToolUseID: "c2", Content: payload}}},
	}
	first := budget.ApplyDedup(turn1).View
	if !strings.Contains(first[1].Content[0].Content, payload) {
		t.Fatal("first occurrence was collapsed while its original was present")
	}
	if !strings.Contains(first[3].Content[0].Content, "duplicate payload sha256:") {
		t.Fatal("second occurrence was not collapsed")
	}

	// Turn 2: the original occurrence is trimmed away; the surviving
	// occurrence must stay collapsed so the provider-facing bytes are stable.
	turn2 := turn1[2:]
	second := budget.ApplyDedup(turn2).View
	if !strings.Contains(second[1].Content[0].Content, "duplicate payload sha256:") {
		t.Fatalf("surviving occurrence flipped back to full payload: %s", second[1].Content[0].Content[:60])
	}
	// The original payload stays retrievable by its content address.
	if original, ok := budget.StoredPayload(Hash(payload)); !ok || original != payload {
		t.Fatal("content-addressed original is not retrievable")
	}
}

func TestChooseBoundariesRequiresStability(t *testing.T) {
	budget := New(Options{MinStableTurns: 2, LiveZoneTurns: 2, MaxStableBytes: 64, MinCacheTokens: 1})
	history := []provider.Message{
		provider.UserText(strings.Repeat("stable old context ", 20)),
		provider.UserText(strings.Repeat("more stable context ", 20)),
		provider.UserText(strings.Repeat("even more stable ", 20)),
		provider.UserText("current request"),
	}
	// First observation: no boundary yet.
	first := budget.ChooseBoundaries(history)
	if len(first) != 0 {
		t.Fatalf("first observation produced boundaries: %v", first)
	}
	// Identical second observation: the stable prefix now qualifies.
	second := budget.ChooseBoundaries(history)
	if len(second) == 0 {
		t.Fatal("second identical observation produced no boundary")
	}
	// The live zone (newest turns) must never be a boundary.
	if second[2] || second[3] {
		t.Fatalf("boundary placed on a live-zone message: %v", second)
	}
}

func TestChooseBoundariesNeverSplitsToolPair(t *testing.T) {
	budget := New(Options{MinStableTurns: 2, LiveZoneTurns: 1, MaxStableBytes: 64, MinCacheTokens: 1})
	history := []provider.Message{
		provider.UserText(strings.Repeat("stable intro ", 20)),
		provider.UserText(strings.Repeat("stable second ", 20)),
		toolUse("c1"),
		toolResult("c1", strings.Repeat("result ", 20)),
		provider.UserText(strings.Repeat("current ", 20)),
	}
	budget.ChooseBoundaries(history)
	boundaries := budget.ChooseBoundaries(history)

	for index := range boundaries {
		if index == 3 {
			t.Fatalf("boundary at %d lands inside a tool_use/tool_result pair", index)
		}
	}
	// With a one-turn live zone the boundary may sit at the pair's start
	// (the pair stays intact, after the boundary), warming the cache sooner.
	if !boundaries[2] {
		t.Fatalf("expected a boundary at the tool pair start, got %v", boundaries)
	}
}

// TestChooseBoundariesInvalidatesSharedPrefixOnHeadChange pins the incremental
// prefix reuse (C3): when the view's head changes, the cached per-message
// analysis must be discarded from the divergence point on, so the stability
// counters reset instead of reusing stale prefix bytes.
func TestChooseBoundariesInvalidatesSharedPrefixOnHeadChange(t *testing.T) {
	budget := New(Options{MinStableTurns: 2, LiveZoneTurns: 1, MaxStableBytes: 64, MinCacheTokens: 1})
	history := []provider.Message{
		provider.UserText(strings.Repeat("stable old context ", 20)),
		provider.UserText(strings.Repeat("more stable context ", 20)),
		provider.UserText(strings.Repeat("even more stable ", 20)),
		provider.UserText("current request"),
	}
	budget.ChooseBoundaries(history) // observation 1
	stable := budget.ChooseBoundaries(history)
	if len(stable) == 0 {
		t.Fatal("expected boundaries after two identical observations")
	}

	// Change the head: the old prefix bytes are gone, so no boundary may
	// survive until the new head has stabilized on its own.
	changed := append([]provider.Message(nil), history...)
	changed[0] = provider.UserText(strings.Repeat("rewritten head context ", 20))
	first := budget.ChooseBoundaries(changed)
	if len(first) != 0 {
		t.Fatalf("head change kept stale boundaries: %v", first)
	}
	second := budget.ChooseBoundaries(changed)
	if len(second) == 0 {
		t.Fatal("new head never stabilized after the rewrite")
	}
}

func TestCompressLiveIsReversible(t *testing.T) {
	budget := New(Options{})
	payload := `{"items":[` + strings.Repeat(`"value",`, 50) + `"last"],"note":"x"}`
	compressed, changed := budget.CompressLive("call-42", payload)
	if !changed {
		t.Fatal("expected compression to change the payload")
	}
	if len(compressed) >= len(payload) {
		t.Fatalf("compression did not shrink: %d -> %d", len(payload), len(compressed))
	}
	original, ok := budget.LiveOriginal("call-42")
	if !ok || original != payload {
		t.Fatal("live-zone original is not retrievable")
	}
}

func TestCompressLiveStoresNonJSONViaCap(t *testing.T) {
	budget := New(Options{LiveZoneCapBytes: 200})
	payload := strings.Repeat("line of text\n", 100)
	compressed, changed := budget.CompressLive("call-7", payload)
	if !changed {
		t.Fatal("expected capping to change the payload")
	}
	if len(compressed) > 200+64 {
		t.Fatalf("capped output too large: %d", len(compressed))
	}
	if !strings.Contains(compressed, "retrieve_uncompressed_context") {
		t.Fatal("cap marker missing from output")
	}
	// The marker must name the retrieval key so the model can round-trip the
	// payload through retrieve_uncompressed_context instead of guessing it.
	if !strings.Contains(compressed, "key call-7") {
		t.Fatalf("cap marker omits the retrieval key: %q", compressed)
	}
	if original, ok := budget.LiveOriginal("call-7"); !ok || original != payload {
		t.Fatal("capped original not retrievable")
	}
}

// TestMaskJSONTruncationStaysRuneAligned locks the live-zone JSON mask: a
// long string value must be cut on a rune boundary. Byte-slicing mid-rune
// used to make json.Marshal emit a literal U+FFFD replacement char, so the
// model received corrupted JSON instead of a clean truncation.
func TestMaskJSONTruncationStaysRuneAligned(t *testing.T) {
	budget := New(Options{})
	// 155 ASCII bytes followed by two 3-byte runes: byte 157 lands inside the
	// first "€", which previously produced a "\ufffd" escape in the mask.
	prefix := strings.Repeat("a", 155)
	payload := `{"msg":"` + prefix + "€€" + `"}`
	compressed, changed := budget.CompressLive("call-rune", payload)
	if !changed {
		t.Fatal("expected JSON mask to change the payload")
	}
	if strings.Contains(compressed, "ufffd") {
		t.Fatalf("mask truncated mid-rune (replacement char present): %q", compressed)
	}
	if !utf8.ValidString(compressed) {
		t.Fatalf("mask output is not valid UTF-8: %q", compressed)
	}
	// The truncation point must still be a rune boundary.
	if cut := strings.Index(compressed, "…"); cut >= 0 {
		if !utf8.ValidString(compressed[:cut]) {
			t.Fatalf("mask cut is not rune-aligned: %q", compressed[:cut])
		}
	}
}

// TestCutRunesNeverSplitsARune is the direct helper-level check: cutting any
// multibyte string at a byte limit must not produce invalid UTF-8.
func TestCutRunesNeverSplitsARune(t *testing.T) {
	text := strings.Repeat("a", 155) + "€€"
	for limit := 0; limit <= len(text); limit++ {
		cut := cutRunes(text, limit)
		if !utf8.ValidString(cut) {
			t.Fatalf("cutRunes(%d) produced invalid UTF-8: %q", limit, cut)
		}
		if len(cut) > limit {
			t.Fatalf("cutRunes(%d) exceeded limit: %d bytes", limit, len(cut))
		}
	}
}

// TestDedupDecisionIsPerResultAndNeverFlipped locks the B-phase invariant:
// a tool result's bytes are decided once, by tool_use_id, and never change —
// even when duplication trimming moves the payload's copy out of the view and
// a brand-new occurrence of the same payload appears later.
func TestDedupDecisionIsPermanentPerToolUseID(t *testing.T) {
	budget := New(Options{})
	payload := strings.Repeat("identical large payload ", 200)

	toolPair := func(id string) []provider.Message {
		return []provider.Message{
			{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "tool_use", ID: id, Name: "bash"}}},
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "tool_result", ToolUseID: id, Content: payload}}},
		}
	}

	// Turn 1: c1 verbatim, c2 (same payload) collapsed to a pointer.
	turn1 := append([]provider.Message{provider.UserText("go")}, toolPair("c1")...)
	turn1 = append(turn1, toolPair("c2")...)
	view1 := budget.ApplyDedup(turn1).View
	// view1 = [user, c1 use, c1 result, c2 use, c2 result]
	if !strings.Contains(view1[2].Content[0].Content, payload) {
		t.Fatal("first result was collapsed while its original was present")
	}
	if !strings.Contains(view1[4].Content[0].Content, "duplicate payload sha256:") {
		t.Fatal("second result was not collapsed")
	}

	// Turn 2: the head (the verbatim copy) is trimmed; the surviving pointer
	// stays a pointer (bytes must not change).
	turn2 := turn1[3:] // drop the user message and the c1 tool pair
	view2 := budget.ApplyDedup(turn2).View
	// view2 = [c2 use, c2 result]
	if !strings.Contains(view2[1].Content[0].Content, "duplicate payload sha256:") {
		t.Fatal("surviving result flipped back to the full payload")
	}

	// Turn 3: the same payload recurs under a new id; the new result must be
	// collapsed too (the payload has already been sent to the provider), and
	// the previously sent result's bytes must be untouched.
	turn3 := append(append([]provider.Message{}, turn2...), toolPair("c3")...)
	view3 := budget.ApplyDedup(turn3).View
	if view3[1].Content[0].Content != view2[1].Content[0].Content {
		t.Fatal("previously sent result changed bytes on a later turn")
	}
	if !strings.Contains(view3[3].Content[0].Content, "duplicate payload sha256:") {
		t.Fatal("new occurrence of a seen payload was not collapsed")
	}
}

// TestCompressLiveIsDeterministicAcrossRounds ensures the live-zone pass
// yields byte-identical output on every application, so a payload captured in
// one turn serializes the same on save/resume and re-stream.
func TestCompressLiveIsDeterministicAcrossRounds(t *testing.T) {
	budget := New(Options{})
	long := strings.Repeat("some very long field value ", 20) // > 160 chars
	row := func(i int) string { return fmt.Sprintf("  {\"id\": %d, \"note\": \"%s\"}", i, long) }
	rows := make([]string, 12)
	for i := range rows {
		rows[i] = row(i)
	}
	payload := "{\n \"rows\": [\n" + strings.Join(rows, ",\n") + "\n ]\n}"
	first, changed1 := budget.CompressLive("call-1", payload)
	if !changed1 {
		t.Fatal("expected compression to change the payload")
	}
	if second, _ := budget.CompressLive("call-1", payload); second != first {
		t.Fatal("CompressLive produced different bytes on a second pass")
	}
	// A fresh budget must give identical bytes for identical input (no
	// per-instance state affects the output).
	fresh := New(Options{})
	if again, _ := fresh.CompressLive("call-1", payload); again != first {
		t.Fatal("CompressLive output differs between budget instances")
	}
}
