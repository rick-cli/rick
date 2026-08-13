package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

// TestPasteBurstCoalescesTypedRunes pins the fix for the "paste slowly types
// in, then double-inserts" bug: Windows Terminal consumes Ctrl+V and replays
// the text as per-character key events, and the polling clipboard read used to
// insert the whole text again on top. The burst coalescer must insert the
// clipboard text exactly once.
func TestPasteBurstCoalescesTypedRunes(t *testing.T) {
	clipboardTextReader = func() (string, error) { return "hello world", nil }
	defer func() { clipboardTextReader = readClipboardText }()

	m := newModelChoiceTestModel()
	m.input = textarea.New()
	m.input.Focus()

	// The terminal replays the paste one rune per KeyMsg. The first two land
	// in the textarea, the third confirms the paste and coalesces the rest.
	for _, r := range []rune("hel") {
		_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := m.input.Value(); got != "hello world" {
		t.Fatalf("input after coalesce = %q, want %q", got, "hello world")
	}

	// The rest of the terminal's replay must be dropped, not double-inserted.
	for _, r := range []rune("lo world") {
		_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := m.input.Value(); got != "hello world" {
		t.Fatalf("input after full replay = %q, want %q", got, "hello world")
	}
}

// TestPasteBurstMultiLineCoalescesAtNewline covers a paste with line breaks:
// the newline arrives as an Enter-family key while the burst is hot, and must
// be coalesced (and dropped) instead of submitting the partial line.
func TestPasteBurstMultiLineCoalescesAtNewline(t *testing.T) {
	clipboardTextReader = func() (string, error) { return "ab\ncd", nil }
	defer func() { clipboardTextReader = readClipboardText }()

	m := newModelChoiceTestModel()
	m.input = textarea.New()
	m.input.Focus()

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter}) // paste's newline
	if got := m.input.Value(); got != "ab\ncd" {
		t.Fatalf("input after newline coalesce = %q, want %q", got, "ab\ncd")
	}
	// The remaining replay runes are dropped by suppression.
	for _, r := range []rune("cd") {
		_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := m.input.Value(); got != "ab\ncd" {
		t.Fatalf("input after replay = %q", got)
	}
}

// TestPasteBurstFastTypingIsNotCoalesced guards against false positives: a
// fast typing flurry (or key repeat) whose runes do not prefix the clipboard
// must type normally, never be replaced by clipboard content.
func TestPasteBurstFastTypingIsNotCoalesced(t *testing.T) {
	clipboardTextReader = func() (string, error) { return "unrelated clipboard content", nil }
	defer func() { clipboardTextReader = readClipboardText }()

	m := newModelChoiceTestModel()
	m.input = textarea.New()
	m.input.Focus()

	for _, r := range []rune("ddd") {
		_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := m.input.Value(); got != "ddd" {
		t.Fatalf("fast typing was coalesced into clipboard: got %q, want %q", got, "ddd")
	}
}

// TestPasteBurstSeededSuppressionNeverReplaysSeed verifies that a keystroke
// which diverges from a coalesced paste's target after the full paste is
// already in the input does not replay the seeded (already-inserted) prefix.
func TestPasteBurstSeededSuppressionNeverReplaysSeed(t *testing.T) {
	clipboardTextReader = func() (string, error) { return "abc", nil }
	defer func() { clipboardTextReader = readClipboardText }()

	m := newModelChoiceTestModel()
	m.input = textarea.New()
	m.input.Focus()

	// Three runes confirm the paste and consume the whole replay; the paste is
	// fully inserted by the coalesce.
	for _, r := range []rune("abc") {
		_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := m.input.Value(); got != "abc" {
		t.Fatalf("input = %q, want %q", got, "abc")
	}

	// A keystroke within the suppression window must not replay the seeded
	// prefix "abc" — only the new rune.
	m.pasteTarget = "abc"
	m.pasteSuppress = []rune("abc")
	m.pasteSuppressSeed = 3
	m.lastClipboardPaste = time.Now()
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if got := m.input.Value(); got != "abcx" {
		t.Fatalf("post-paste keystroke = %q, want %q", got, "abcx")
	}
}

// TestPasteBurstShortPasteTypesNormally verifies a two-character paste (below
// the trigger) simply types in — no coalesce, no double insert.
func TestPasteBurstShortPasteTypesNormally(t *testing.T) {
	clipboardTextReader = func() (string, error) { return "ab", nil }
	defer func() { clipboardTextReader = readClipboardText }()

	m := newModelChoiceTestModel()
	m.input = textarea.New()
	m.input.Focus()

	for _, r := range []rune("ab") {
		_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := m.input.Value(); got != "ab" {
		t.Fatalf("short paste input = %q, want %q", got, "ab")
	}
	if !strings.HasSuffix(m.input.Value(), "ab") {
		t.Fatalf("short paste lost: %q", m.input.Value())
	}
}
