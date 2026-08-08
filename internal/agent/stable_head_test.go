package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"rick/internal/provider"
	"rick/internal/tokens"
)

// TestRetainStableTrimsTheHeadOnceThenGrowsAppendOnly guards the P2 invariant:
// the first time the conversation overflows the budget the oldest groups are
// dropped behind a byte-stable sentinel; subsequent calls re-send that exact
// sentinel and only ever append new content at the tail. A mid-prompt rewrite
// (dropping more from the front) would invalidate the provider prefix cache.
func TestRetainStableTrimsTheHeadOnceThenGrowsAppendOnly(t *testing.T) {
	r := New(Config{})
	enc := tokens.EncodingCl100kBase

	old := provider.UserText(strings.Repeat("old context ", 200))
	messages := []provider.Message{old, provider.UserText("question one")}

	view1 := r.retainStable(messages, 40, enc)
	if !r.trimEngaged {
		t.Fatal("expected trimming to engage on the first over-budget call")
	}
	if len(view1) == 0 || view1[0].Role != provider.RoleUser ||
		!strings.Contains(view1[0].Text(), "trimmed") {
		t.Fatalf("view does not start with the trim sentinel: %#v", view1[0])
	}

	// Grow the conversation and call again: the second view must be
	// append-only with respect to the first — the sentinel and the retained
	// tail keep their exact bytes at the same positions.
	growing1 := append(messages, provider.UserText("question two"))
	view2 := r.retainStable(growing1, 40, enc)
	if len(view2) <= len(view1) {
		t.Fatalf("second view did not grow: len1=%d len2=%d", len(view1), len(view2))
	}
	for i, m := range view1 {
		if i >= len(view2) || !sameMessage(m, view2[i]) {
			t.Fatalf("view1[%d] changed or dropped: %#v vs %#v", i, m, view2[i])
		}
	}

	// A third call with even more content must remain append-only too: the
	// stable head is never trimmed again once engaged.
	growing2 := append(growing1, provider.UserText("answer 2"), provider.UserText("answer 3"))
	view3 := r.retainStable(growing2, 100, enc)
	for i, m := range view2 {
		if i >= len(view3) || !sameMessage(m, view3[i]) {
			t.Fatalf("view2[%d] changed after further growth: %#v vs %#v", i, m, view3[i])
		}
	}
}

// TestRetainStableUnchangedBelowBudget confirms trimming and the sentinel never
// appear while the conversation fits comfortably in the budget.
func TestRetainStableUnchangedBelowBudget(t *testing.T) {
	r := New(Config{})
	enc := tokens.EncodingCl100kBase
	messages := []provider.Message{provider.UserText("hello"), provider.UserText("world")}
	view := r.retainStable(messages, 100000, enc)
	if r.trimEngaged {
		t.Fatal("trim engaged despite a generous budget")
	}
	if len(view) != len(messages) {
		t.Fatalf("view = %d messages, want %d", len(view), len(messages))
	}
}

func sameMessage(a, b provider.Message) bool {
	if a.Role != b.Role || len(a.Content) != len(b.Content) {
		return false
	}
	for i := range a.Content {
		ab, _ := json.Marshal(a.Content[i])
		bb, _ := json.Marshal(b.Content[i])
		if string(ab) != string(bb) {
			return false
		}
	}
	return true
}
