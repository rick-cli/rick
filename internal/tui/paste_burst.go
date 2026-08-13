package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	// pasteBurstWindow is the max gap between events of a paste re-delivery
	// burst. Windows Terminal/conhost replay a paste as one key event per
	// character with sub-millisecond gaps; a human typing never produces
	// events this close together.
	pasteBurstWindow = 60 * time.Millisecond
	// pasteBurstTrigger is how many burst events confirm a paste. Three runes
	// within the window is beyond any human typing burst (and beyond Windows
	// key repeat), so a false trigger is not possible; the clipboard prefix
	// check below guards the remainder.
	pasteBurstTrigger = 3
)

// clipboardTextReader is the clipboard read used by the paste-burst coalescer.
// It is a var so tests can substitute a fake without touching the OS
// clipboard.
var clipboardTextReader = readClipboardText

// trackPasteBurst watches the terminal's per-character re-delivery of a paste
// and coalesces it into a single atomic clipboard insert. On Windows,
// Terminal/conhost consume Ctrl+V and replay the pasted text as one key event
// per character — no Paste flag, no bracketed-paste marker on the coninput
// path. If the runes simply flowed into the textarea they would type in
// one-by-one ("slow paste") and the old polling clipboard read would then
// insert the whole text again on top (double paste). Once a burst of three
// events confirms a paste, the clipboard text is verified as a prefix of the
// burst, the not-yet-typed remainder is inserted, and the rest of the replay
// is suppressed. Returns true when msg was consumed as part of the paste.
func (m *Model) trackPasteBurst(msg tea.KeyMsg) bool {
	if msg.Type == tea.KeyRunes && len(msg.Runes) > 0 {
		m.lastRuneAt = time.Now()
	}
	if m.pasteTarget != "" {
		// A direct clipboard insert (Ctrl+V forwarded by the terminal) already
		// owns this paste; the suppression logic drops the replay.
		return false
	}

	now := time.Now()
	hot := m.pasteBurstActive()
	key := msg.String()

	switch {
	case msg.Type == tea.KeyRunes && len(msg.Runes) > 1:
		// A multi-rune key (bracketed paste, IME) is already atomic.
		m.resetPasteBurst()
		return false
	case msg.Type == tea.KeyRunes && len(msg.Runes) == 1:
		if !hot {
			m.pasteBurstRunes = nil
			m.pasteBurstInserted = 0
		}
		m.pasteBurstAt = now
		m.pasteBurstRunes = append(m.pasteBurstRunes, msg.Runes...)
		if len(m.pasteBurstRunes) >= pasteBurstTrigger {
			if coalesced, transient := m.coalescePasteBurst(); coalesced {
				return true
			} else if !transient {
				m.resetPasteBurst()
			}
		}
		m.pasteBurstInserted += len(msg.Runes)
		return false
	case key == "tab":
		if !hot || len(m.pasteBurstRunes) == 0 {
			return false
		}
		m.pasteBurstAt = now
		m.pasteBurstRunes = append(m.pasteBurstRunes, '\t')
		if len(m.pasteBurstRunes) >= pasteBurstTrigger {
			if coalesced, _ := m.coalescePasteBurst(); coalesced {
				return true
			}
			m.resetPasteBurst()
			return false
		}
		return true // tab inside a growing paste burst: drop it (it is content)
	case key == "enter" || key == "ctrl+j" || key == "ctrl+m":
		if !hot || len(m.pasteBurstRunes) == 0 {
			return false
		}
		// A newline inside a hot burst is either the paste's line break
		// (coalesce now, drop the key) or a real Enter after a fast typing
		// flurry (submit). Confirm against the clipboard before dropping.
		if text, err := clipboardTextReader(); err == nil && text != "" {
			burst := string(m.pasteBurstRunes)
			if strings.HasPrefix(text, burst+"\n") || strings.HasPrefix(text, burst+"\r\n") {
				m.pasteBurstAt = now
				m.pasteBurstRunes = append(m.pasteBurstRunes, '\n')
				m.coalescePasteBurst()
				return true
			}
		}
		m.resetPasteBurst()
		return false
	default:
		if hot {
			m.resetPasteBurst()
		}
		return false
	}
}

// coalescePasteBurst completes a confirmed paste atomically. The burst runes
// already inserted into the input are the paste's prefix, so only the
// remainder of the clipboard text is inserted; pasteSuppress is seeded with
// the consumed prefix so the rest of the terminal's replay is dropped instead
// of double-inserted. Returns (coalesced, transient); transient=true means the
// clipboard was temporarily unreadable and the burst should be retried.
func (m *Model) coalescePasteBurst() (bool, bool) {
	text, err := clipboardTextReader()
	if err != nil {
		return false, true
	}
	if text == "" {
		return false, false
	}
	if !strings.HasPrefix(text, string(m.pasteBurstRunes)) {
		return false, false // fast typing / key repeat, not a paste
	}
	remaining := []rune(text)[m.pasteBurstInserted:]
	if len(remaining) > 0 {
		m.input.InsertString(string(remaining))
		m.resizeAfterInputEdit()
	}
	m.slashCursor = 0
	m.histIdx = -1
	m.lastClipboardPaste = time.Now()
	m.pasteTarget = text
	m.pasteSuppress = append([]rune(nil), m.pasteBurstRunes...)
	m.pasteSuppressSeed = len(m.pasteSuppress)
	if strings.Contains(text, "\n") {
		m.pasteNewlineUntil = time.Now().Add(500 * time.Millisecond)
	}
	m.pasteBurstAt = time.Time{}
	m.pasteBurstRunes = nil
	m.pasteBurstInserted = 0
	return true, false
}

func (m *Model) resetPasteBurst() {
	m.pasteBurstRunes = nil
	m.pasteBurstInserted = 0
	m.pasteBurstAt = time.Time{}
}

func (m *Model) pasteBurstActive() bool {
	return !m.pasteBurstAt.IsZero() && time.Since(m.pasteBurstAt) <= pasteBurstWindow
}
