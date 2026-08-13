package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// truncateWidthOK asserts the result fits w cells and is a prefix of the
// input (plus an ellipsis) whenever truncation happened.
func truncateWidthOK(t *testing.T, s string, w int) {
	t.Helper()
	got := truncate(s, w)
	if lipgloss.Width(got) > w {
		t.Fatalf("truncate(%q, %d) = %q exceeds %d cells", s, w, got, w)
	}
	if !strings.HasSuffix(got, "…") {
		return // no truncation needed
	}
	body := strings.TrimSuffix(got, "…")
	if !strings.HasPrefix(s, body) {
		t.Fatalf("truncate(%q, %d) = %q is not a prefix of the input", s, w, got)
	}
}

func TestTruncateFitsWidth(t *testing.T) {
	cases := []string{
		"hello world this is a long line that must be cut",
		"short",
		"",
		"0123456789",
		"ünïcödé wörds with accents éàè",
		"a\nb\tc",
		strings.Repeat("x", 5000),
		"https://example.com/averylongpath/here for details",
		linkifyLine("see https://example.com/averylongpath/here for details", true),
		strings.Repeat("🦀", 3000), // wide runes
	}
	for _, s := range cases {
		for _, w := range []int{1, 5, 10, 20, 100} {
			truncateWidthOK(t, s, w)
		}
	}
}

// TestTruncateLargeStringFast guards the O(n^2) regression: truncating a
// 150KB string used to take ~a minute because every dropped rune re-measured
// the whole remaining tail.
func TestTruncateLargeStringFast(t *testing.T) {
	s := strings.Repeat("0123456789", 15000) // 150k chars, like a big tool result
	got := truncate(s, 100)
	if lipgloss.Width(got) > 100 {
		t.Fatalf("truncate width %d > 100", lipgloss.Width(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("large string was not truncated")
	}
}
