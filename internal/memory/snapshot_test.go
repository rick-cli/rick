package memory

import "testing"

// messages builds a small transcript with a user goal, an assistant finding,
// an error, and a tool-result-looking user message that must never leak into
// the Goal section.
func messages() []MessageLike {
	return []MessageLike{
		{Role: "user", Text: "add a cache-hit benchmark harness"},
		{Role: "assistant", Text: "I will add cmd/cachehit with a runner-mode pass."},
		{Role: "user", Text: `{"json": true, "large": "blob"}`},
		{Role: "assistant", Text: "found: provider prefix cache is a bounded LRU"},
		{Role: "user", Text: `[duplicate payload sha256:abc; identical to an earlier tool result — retrieve via retrieve_uncompressed_context key abc]`},
		{Role: "assistant", IsError: false, Text: ""},
		{Role: "user", Text: "run the benchmark with -runs 2"},
	}
}

func TestDeriveDeterministic(t *testing.T) {
	a := Derive(messages(), Options{})
	b := Derive(messages(), Options{})
	if a.ID != b.ID || a.Text != b.Text {
		t.Fatal("same transcript derived different snapshots")
	}
	if a.ID == "" {
		t.Fatal("empty snapshot id")
	}
}

func TestDeriveFixedSections(t *testing.T) {
	s := Derive(messages(), Options{})
	for _, section := range []string{SectionGoal, SectionFacts, SectionErrors, SectionNext} {
		if !contains(s.Text, "## "+section) {
			t.Fatalf("missing section %q in:\n%s", section, s.Text)
		}
	}
}

func TestDeriveSkipsToolResultShapes(t *testing.T) {
	s := Derive(messages(), Options{})
	for _, line := range []string{"blob", "duplicate payload sha256"} {
		if contains(s.Text, line) {
			t.Fatalf("tool-result-shaped text leaked into snapshot: %q\n%s", line, s.Text)
		}
	}
	if s.LastUserText != "run the benchmark with -runs 2" {
		t.Fatalf("last user text = %q", s.LastUserText)
	}
}

func TestDeriveBounded(t *testing.T) {
	long := make([]MessageLike, 0, 40)
	for i := 0; i < 40; i++ {
		long = append(long, MessageLike{Role: "user", Text: "step " + string(rune('a'+i%26)) + " with some detail to fill the section"})
	}
	s := Derive(long, Options{})
	if s.Sections[SectionGoal] > DefaultMaxSections {
		t.Fatalf("goal section overflowed: %d > %d", s.Sections[SectionGoal], DefaultMaxSections)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
