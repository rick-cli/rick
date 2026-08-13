package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"rick/internal/config"
)

// TestPasteSuppressionDropsTerminalRedelivery pins the anti-double-paste
// guard: after a direct clipboard text paste, Windows Terminal re-delivers
// the same text as per-character key events. Those must be dropped while they
// prefix-match the pasted text, so pasting never double-inserts.
func TestPasteSuppressionDropsTerminalRedelivery(t *testing.T) {
	m := newModelChoiceTestModel()
	m.input = textarea.New()
	m.input.SetValue("prefix ")
	m.input.CursorEnd()

	pasted := "multi\nline paste"
	m.pasteTarget = pasted
	m.pasteSuppress = nil
	m.lastClipboardPaste = time.Now()

	// Terminal re-delivers the paste as one rune per KeyMsg (Windows
	// coninput path). Each must be swallowed.
	for _, r := range []rune(pasted) {
		_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := m.input.Value(); got != "prefix " {
		t.Fatalf("terminal re-delivery was not suppressed: value %q", got)
	}
	if m.pasteTarget != "" {
		t.Fatal("paste suppression should be disarmed after the full match")
	}

	// Real typing after the paste window is unaffected.
	m.pasteTarget = "abc"
	m.pasteSuppress = nil
	m.lastClipboardPaste = time.Now()
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}) // diverges from "abc"
	if got := m.input.Value(); !strings.HasSuffix(got, "x") {
		t.Fatalf("diverged rune was lost: value %q", got)
	}
	if m.pasteTarget != "" {
		t.Fatal("divergence must disarm suppression")
	}
}

// TestPasteSuppressionReplaysDivergedBuffer ensures that when the buffered
// runes stop matching the remembered paste, the buffered content is inserted
// (it was typed by the user, not pasted) instead of being dropped.
func TestPasteSuppressionReplaysDivergedBuffer(t *testing.T) {
	m := newModelChoiceTestModel()
	m.input = textarea.New()
	m.input.SetValue("")
	m.pasteTarget = "hello"
	m.pasteSuppress = nil
	m.lastClipboardPaste = time.Now()

	// First two runes match the paste target.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h', 'e'}})
	if got := m.input.Value(); got != "" {
		t.Fatalf("matching prefix should be buffered, got %q", got)
	}
	// Third rune diverges: "heX" != "hello".
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'X'}})
	if got := m.input.Value(); got != "heX" {
		t.Fatalf("diverged buffer not replayed, got %q", got)
	}
}

// TestWheelKeyBurstRequiresSameDirection pins that a rapid burst of
// same-direction up/down keys is treated as a wheel (scroll), while mixed or
// sparse arrow keys (history navigation) are not.
func TestWheelKeyBurstRequiresSameDirection(t *testing.T) {
	// Isolated Up press: not a burst, so it browses history.
	m := newModelChoiceTestModel()
	m.input = textarea.New()
	m.inputHist = []string{"first prompt", "second prompt"}
	m.histIdx = -1
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.input.Value(); got != "second prompt" {
		t.Fatalf("isolated Up should browse history, got %q", got)
	}

	// Three same-direction keys inside the window = a wheel burst; the input
	// is left untouched (it scrolls instead of browsing history).
	m2 := newModelChoiceTestModel()
	m2.input = textarea.New()
	m2.inputHist = []string{"first prompt", "second prompt"}
	m2.histIdx = -1
	for i := 0; i < 3; i++ {
		m2.isWheelKey(-1)
	}
	m2.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := m2.input.Value(); got != "" {
		t.Fatalf("wheel burst Up should scroll (input untouched), got %q", got)
	}

	// A direction change breaks the burst.
	m3 := newModelChoiceTestModel()
	m3.isWheelKey(-1)
	m3.isWheelKey(-1)
	if m3.isWheelKey(1) {
		t.Fatal("direction change must not be a wheel burst")
	}
}

// TestWheelGestureContinuationScrollsThroughSlowdown pins that once a wheel is
// confirmed, same-direction keys keep scrolling even when the user slows to
// ~2 notches/sec — the 3-event burst rule alone would have fallen through to
// history navigation between the spaced-out notches.
func TestWheelGestureContinuationScrollsThroughSlowdown(t *testing.T) {
	m := newModelChoiceTestModel()
	m.deps = Deps{Loaded: &config.Loaded{TUI: config.TUI{ScrollSpeed: 1}}}
	m.input = textarea.New()
	m.ready = false
	for i := 0; i < 60; i++ {
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "chat line " + strings.Repeat("x", i%20), Time: nowFn()})
	}
	m.handleResize(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.viewport.GotoBottom()
	m.inputHist = []string{"first prompt", "second prompt"}
	m.histIdx = -1
	m.input.SetValue("draft")
	m.histDraft = "draft"

	// Fast start confirms the wheel gesture.
	for i := 0; i < 3; i++ {
		m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
		time.Sleep(30 * time.Millisecond)
	}
	before := m.viewport.YOffset
	// Pause, then slow notches (500ms apart): the gesture must keep scrolling.
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < 2; i++ {
		m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
		time.Sleep(500 * time.Millisecond)
	}
	if got := m.input.Value(); got != "draft" {
		t.Fatalf("slow tail of wheel corrupted input to %q", got)
	}
	if m.viewport.YOffset >= before {
		t.Fatalf("slow tail did not keep scrolling (offset %d)", m.viewport.YOffset)
	}
}

// TestPasteNewlineRedeliveryDoesNotSubmit pins that Enter-family keys
// delivered by the terminal after a multi-line paste are dropped instead of
// submitting the message.
func TestPasteNewlineRedeliveryDoesNotSubmit(t *testing.T) {
	m := newModelChoiceTestModel()
	m.input = textarea.New()
	m.pasteNewlineUntil = time.Now().Add(500 * time.Millisecond)

	// Terminal re-delivers the pasted newlines as enter / ctrl+j / ctrl+m.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlJ})
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlM})

	// The input must not have been submitted — value stays as-is, and no
	// newline was inserted by these keys either.
	if m.input.Value() != "" {
		t.Fatalf("Enter-family keys during paste window must be dropped, input=%q", m.input.Value())
	}

	// After the window expires, Enter submits normally.
	m.pasteNewlineUntil = time.Time{}
	m.input.SetValue("hello")
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.input.Value() != "" {
		t.Fatalf("Enter after paste window should submit (clear input), got %q", m.input.Value())
	}
}
