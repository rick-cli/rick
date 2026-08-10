package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"rick/internal/agent"
	"rick/internal/permission"
	"rick/internal/provider"
	"rick/internal/tools"
	"rick/internal/vision"
)

func (m *Model) syncInputHeight() bool {
	lines := m.inputVisualLines()
	if m.input.Height() == lines {
		return false
	}
	m.input.SetHeight(lines)
	return true
}

func (m *Model) resizeAfterInputEdit() {
	m.handleResize(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	m.syncInputHeight()
}

// handleKey routes a keypress through modals, picker, leader, then the input.
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Suppress the terminal's re-delivery of a paste's newlines. Windows
	// Terminal converts a native Ctrl+V into per-character events, and
	// pasted line breaks arrive as Enter-family key presses (enter / ctrl+j
	// / ctrl+m) which would otherwise submit the message. Drop them for a
	// short window after a multi-line paste; the text itself is already in
	// the input from the direct clipboard read.
	if time.Now().Before(m.pasteNewlineUntil) {
		switch key {
		case "enter", "ctrl+j", "ctrl+m":
			return m, nil
		}
	}

	// ctrl+c: interrupt a run, otherwise if there are attachments or input, clear them.
	// Second press quits.
	if key == "ctrl+c" {
		if m.running {
			m.interrupt()
			m.quitArmed = false
			return m, nil
		}
		// If there are attachments, clear the last one (remove its marker from input)
		if len(m.attachments) > 0 {
			input := m.input.Value()
			if cleaned, idx := removeLastAttachmentMarker(input); idx >= 0 {
				m.input.SetValue(cleaned)
				m.attachments = m.attachments[:len(m.attachments)-1]
				m.setStatus(fmt.Sprintf("removed [image/file #%d]", idx))
				m.quitArmed = false
				return m, nil
			}
			// Fallback: just clear all attachments
			m.attachments = nil
			m.setStatus("attachments cleared")
			m.quitArmed = false
			return m, nil
		}
		if m.input.Value() != "" {
			m.input.SetValue("") // first press clears the line, like a shell
			m.quitArmed = true
			m.quitAt = time.Now()
			m.setStatus("ctrl+c again to exit")
			return m, nil
		}
		if m.quitArmed && time.Since(m.quitAt) < 3*time.Second {
			m.quitting = true
			return m, tea.Quit
		}
		m.quitArmed = true
		m.quitAt = time.Now()
		m.setStatus("ctrl+c again to exit")
		return m, nil
	}
	// Any other key disarms the pending quit.
	m.quitArmed = false

	if m.modal == modalPermission {
		return m.handlePermissionKey(key)
	}

	// Leader sequence.
	if m.leaderActive {
		m.leaderActive = false
		return m.handleLeaderKey(key)
	}
	if key == m.leaderKey {
		m.leaderActive = true
		return m, nil
	}

	// File picker.
	if m.picker.active {
		if handled, mm, cmd := m.handlePickerKey(key); handled {
			return mm, cmd
		}
	}

	if m.web.active {
		return m.handleWebKey(msg, key)
	}

	if isChoiceMenu(m.pending.kind) && !m.pending.textInput && m.input.Value() == "" {
		switch key {
		case "up":
			m.movePendingCursor(-1)
			m.touchPendingChoice()
			m.refresh()
			return m, nil
		case "down":
			m.movePendingCursor(1)
			m.touchPendingChoice()
			m.refresh()
			return m, nil
		case "enter":
			if len(m.pending.options) == 1 || m.pending.cursorMoved {
				return m.applyPendingCursor()
			}
			mm, cmd, _ := m.handlePendingInput("")
			return mm, cmd
		case "esc", "backspace":
			return m.backPendingChoice()
		}
	}

	if m.activityFocused {
		switch key {
		case "enter":
			return m.openFocusedActivity()
		case "esc", "shift+tab":
			m.activityFocused = false
			return m, nil
		}
	}

	if key == "up" || key == "down" {
		delta := 1
		if key == "up" {
			delta = -1
		}
		// With mouse capture off (native selection on), Windows Terminal
		// delivers the scroll wheel as a rapid burst of same-direction
		// up/down key events. A burst within 150ms is a wheel — scroll the
		// transcript instead of navigating prompt history.
		if m.isWheelKeyBurst(delta) {
			m.scrollBy(delta * m.scrollStep())
			return m, nil
		}
		if m.moveSlashCursor(delta) {
			return m, nil
		}
	}

	switch key {
	case "esc":
		if m.running {
			m.interrupt()
			return m, nil
		}
		if m.input.Value() != "" {
			m.input.SetValue("")
			return m, nil
		}
		return m, nil

	case "ctrl+u":
		m.input.SetValue("")
		return m, nil

	case "shift+tab":
		if len(m.activityItems()) > 0 {
			m.activityFocused = true
			return m, nil
		}
		return m, nil

	case "tab":
		// Complete a slash command before using Tab for agent cycling.
		if m.completeSlashCommand() {
			return m, nil
		}
		m.cycleAgent()
		return m, nil

	case "alt+enter", "shift+enter", "ctrl+enter", "ctrl+j":
		// Insert a newline directly into the textarea. On Windows terminals,
		// Ctrl+Enter is commonly delivered as LF (ctrl+j), not as a modified
		// KeyEnter event.
		val := m.input.Value()
		m.input.SetValue(val + "\n")
		m.input.CursorEnd()
		m.resizeAfterInputEdit()
		return m, nil

	case "enter":
		if selected, ok := m.slashSelection(); ok {
			m.input.SetValue("")
			m.input.SetHeight(1)
			m.histIdx = -1
			m.pushHistory(selected)
			return m.submit(selected)
		}
		// If the textarea has multiple logical lines, let user submit with enter
		// (single line submits, multi-line submits on enter)
		val := m.input.Value()
		if strings.Contains(val, "\n") {
			v := strings.TrimSpace(val)
			if v == "" {
				if m.pending.kind != pendingNone {
					mm, cmd, _ := m.handlePendingInput("")
					return mm, cmd
				}
				return m, nil
			}
			if m.running {
				return m.submit(v)
			}
			m.input.SetValue("")
			m.input.SetHeight(1)
			m.histIdx = -1
			m.pushHistory(v)
			return m.submit(v)
		}
		v := strings.TrimSpace(val)
		if v == "" {
			// A bare enter cancels an armed selection; otherwise it is a no-op.
			if m.pending.kind != pendingNone {
				mm, cmd, _ := m.handlePendingInput("")
				return mm, cmd
			}
			return m, nil
		}
		if m.running {
			return m.submit(v)
		}
		m.input.SetValue("")
		m.input.SetHeight(1)
		m.histIdx = -1
		m.pushHistory(v)
		return m.submit(v)

	case "up":
		m.historyUp()
		return m, nil
	case "down":
		m.historyDown()
		return m, nil

	case "pgup":
		m.scrollBy(-(m.viewport.Height - 2))
		return m, nil
	case "pgdown":
		m.scrollBy(m.viewport.Height - 2)
		return m, nil
	case "shift+up", "ctrl+up", "shift+pgup":
		m.scrollBy(-m.scrollStep())
		return m, nil
	case "shift+down", "ctrl+down", "shift+pgdown":
		m.scrollBy(m.scrollStep())
		return m, nil
	case "alt+up":
		m.historyUp()
		return m, nil
	case "alt+down":
		m.historyDown()
		return m, nil
	case "ctrl+b":
		m.scrollBy(-(m.viewport.Height - 2))
		return m, nil
	case "ctrl+f":
		m.scrollBy(m.viewport.Height - 2)
		return m, nil
	case "ctrl+home":
		m.viewport.GotoTop()
		m.tx.userScrolled(&m.viewport)
		return m, nil
	case "ctrl+v", "ctrl+shift+v":
		if time.Since(m.lastClipboardPaste) > 250*time.Millisecond {
			m.handleClipboardPaste()
		}
		return m, nil
	case "ctrl+end", "end":
		m.tx.jumpToBottom(&m.viewport)
		return m, nil
	}

	// Suppress the terminal's re-delivery of a paste we already inserted via
	// the direct clipboard read. Windows Terminal converts its native Ctrl+V
	// into per-character key events after the fact; without this, pasting
	// would double-insert the text. While the incoming runes continue to
	// prefix-match the remembered paste they are dropped; the first rune
	// that diverges ends suppression and is processed normally.
	if m.pasteTarget != "" && time.Since(m.lastClipboardPaste) < 500*time.Millisecond {
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 0 {
			m.pasteSuppress = append(m.pasteSuppress, msg.Runes...)
			target := []rune(m.pasteTarget)
			if len(m.pasteSuppress) <= len(target) {
				match := true
				for i := range m.pasteSuppress {
					if m.pasteSuppress[i] != target[i] {
						match = false
						break
					}
				}
				if match {
					if len(m.pasteSuppress) == len(target) {
						m.pasteTarget = ""
						m.pasteSuppress = nil
					}
					return m, nil
				}
			}
			// Diverged: this is real typing. Replay the buffered runes as a
			// single insert (they were never shown) and clear suppression.
			m.pasteTarget = ""
			if len(m.pasteSuppress) > 0 {
				m.input.InsertString(string(m.pasteSuppress))
				m.pasteSuppress = nil
				m.resizeAfterInputEdit()
			}
		}
	}

	prevLines := m.input.LineCount()
	previousValue := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.input.Value() != previousValue {
		m.slashCursor = 0
		m.histIdx = -1
	}

	m.resizeAfterInputEdit()
	if m.input.LineCount() > prevLines {
		return m, cmd
	}

	// Opening the @ picker.
	v := m.input.Value()
	if strings.HasSuffix(v, "@") && !m.picker.active {
		m.openPicker()
	} else if m.picker.active {
		m.updatePickerQuery()
	}
	return m, cmd
}

// wheelKey is a single up/down key event recorded for wheel-burst detection.
type wheelKey struct {
	at  time.Time
	dir int // -1 for up, +1 for down
}

// isWheelKeyBurst reports whether the current up/down key event is part of a
// rapid same-direction burst (a scroll-wheel emulated as arrow keys by
// Windows Terminal when mouse capture is off). It records the event and
// returns true when at least three consecutive same-direction events arrive
// within a short window. Real arrow-key use (history navigation) rarely
// produces three same-direction presses that fast, so it is not misread.
func (m *Model) isWheelKeyBurst(dir int) bool {
	now := time.Now()
	cutoff := now.Add(-150 * time.Millisecond)
	kept := m.wheelKeyTimes[:0]
	for _, e := range m.wheelKeyTimes {
		if e.at.After(cutoff) {
			kept = append(kept, e)
		}
	}
	m.wheelKeyTimes = append(kept, wheelKey{at: now, dir: dir})
	if len(m.wheelKeyTimes) < 3 {
		return false
	}
	// All recorded events in the burst must go the same direction.
	for _, e := range m.wheelKeyTimes {
		if e.dir != dir {
			return false
		}
	}
	return true
}

func (m *Model) handleClipboardPaste() {
	m.lastClipboardPaste = time.Now()

	// Text paste first: read the clipboard directly and insert the whole
	// string into the input in one operation. This bypasses the terminal's
	// per-character key delivery entirely, so pasting is instant and a
	// multi-line paste becomes a multi-line input instead of submitting a
	// message per line.
	if text, err := readClipboardText(); err == nil && text != "" {
		m.input.InsertString(text)
		m.input.CursorEnd()
		m.resizeAfterInputEdit()
		m.slashCursor = 0
		m.histIdx = -1
		// Arm suppression: remember the full pasted text so a terminal that
		// re-delivers the same paste as per-character key events right after
		// (Windows Terminal's native Ctrl+V) can drop it instead of
		// double-inserting. Newline re-delivery (Enter-family keys) is
		// suppressed for a short window too, so a multi-line paste never
		// auto-submits.
		m.pasteTarget = text
		m.pasteSuppress = nil
		if strings.Contains(text, "\n") {
			m.pasteNewlineUntil = time.Now().Add(500 * time.Millisecond)
		}
		return
	}

	if path, err := readClipboardImage(); err == nil {
		if att, addErr := addAttachment(path); addErr == nil {
			m.attachments = append(m.attachments, *att)
			m.input.SetValue(m.input.Value() + fmt.Sprintf("[image #%d]", len(m.attachments)))
			m.input.CursorEnd()
			return
		}
	}

	files, err := readClipboardFiles()
	if err != nil || len(files) == 0 {
		m.setStatus("no text/image/files in clipboard")
		return
	}
	for _, path := range files {
		att, addErr := addAttachment(path)
		if addErr != nil {
			continue
		}
		m.attachments = append(m.attachments, *att)
		kind := "file"
		if att.IsImage {
			kind = "image"
		}
		m.input.SetValue(m.input.Value() + fmt.Sprintf("[%s #%d]", kind, len(m.attachments)))
	}
	m.resizeAfterInputEdit()
}

func (m *Model) handleLeaderKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "h":
		return m.cmdHelp()
	case "m":
		return m.cmdModels()
	case "t":
		return m.cmdThemes()
	case "n":
		return m.cmdNew()
	case "l":
		return m.cmdSessions()
	case "u":
		return m.cmdUndo()
	case "r":
		return m.cmdRedo()
	case "d":
		m.toolDetails = !m.toolDetails
		m.tx.invalidateAll(m.contentWidth())
		m.refresh()
		m.setStatus(fmt.Sprintf("tool details %s", onOff(m.toolDetails)))
		return m, nil
	case "c":
		return m.cmdCompact()
	case "esc":
		return m, nil
	}
	return m, nil
}

// scrollStep is the line count for one scroll increment.
func (m *Model) scrollStep() int {
	n := m.deps.Loaded.TUI.ScrollSpeed
	if n <= 0 {
		n = 3
	}
	return n
}

// scrollBy moves the viewport and updates the follow policy. All scrolling
// goes through here so "am I following the tail?" can never drift.
func (m *Model) scrollBy(lines int) {
	switch {
	case lines < 0:
		m.viewport.ScrollUp(-lines)
	case lines > 0:
		m.viewport.ScrollDown(lines)
	default:
		return
	}
	m.tx.userScrolled(&m.viewport)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (m *Model) cycleAgent() {
	if m.agentName == "build" {
		m.agentName = "plan"
	} else {
		m.agentName = "build"
	}
	m.applyAgentPermissions()
	m.setStatus("agent: " + m.agentName)
}

func (m *Model) applyAgentPermissions() {
	base := m.deps.Loaded.Config.Permission
	if m.agentName == "plan" {
		ask := "ask"
		p := *base
		p.Edit, p.Write = ask, ask
		if p.Bash == nil {
			p.Bash = map[string]string{}
		} else {
			cp := map[string]string{}
			for k, v := range p.Bash {
				cp[k] = v
			}
			p.Bash = cp
		}
		p.Bash["*"] = ask
		m.deps.Perms.SetPermission(&p)
		return
	}
	m.deps.Perms.SetPermission(base)
}

// ---------- input history ----------

func (m *Model) pushHistory(text string) {
	if text == "" {
		return
	}
	if n := len(m.inputHist); n > 0 && m.inputHist[n-1] == text {
		return
	}
	m.inputHist = append(m.inputHist, text)
	if len(m.inputHist) > 200 {
		m.inputHist = m.inputHist[len(m.inputHist)-200:]
	}
}

func (m *Model) historyUp() {
	if len(m.inputHist) == 0 {
		return
	}
	if m.histIdx == -1 {
		m.histDraft = m.input.Value()
		m.histIdx = len(m.inputHist) - 1
	} else if m.histIdx > 0 {
		m.histIdx--
	}
	m.input.SetValue(m.inputHist[m.histIdx])
	m.resizeAfterInputEdit()
}

func (m *Model) historyDown() {
	if m.histIdx == -1 {
		return
	}
	if m.histIdx < len(m.inputHist)-1 {
		m.histIdx++
		m.input.SetValue(m.inputHist[m.histIdx])
	} else {
		m.histIdx = -1
		m.input.SetValue(m.histDraft)
	}
	m.resizeAfterInputEdit()
}

// ---------- submit ----------

func (m *Model) submit(text string) (tea.Model, tea.Cmd) {
	// Slash commands remain available while an agent is running so control
	// commands such as /new and /stop can cancel or reset the active run.
	if len(text) > 0 && text[0] == '/' {
		return m.runSlash(text)
	}

	// Ordinary prompts stay blocked while the agent is running.
	if m.running {
		m.setStatus("still working — esc to interrupt")
		return m, nil
	}
	// Also block while the vision bridge is reading images — a second prompt
	// would race the pending turn.
	if m.visionPending {
		m.setStatus("vision bridge is reading the image(s) — wait a moment")
		return m, nil
	}
	if m.compactionActive {
		m.setStatus("compacting — wait for completion")
		return m, nil
	}

	// An armed inline selection gets first refusal on the input.
	if mm, cmd, handled := m.handlePendingInput(text); handled {
		return mm, cmd
	}

	// Shell escape.
	if len(text) > 0 && text[0] == '!' {
		cmdline := strings.TrimSpace(text[1:])
		if cmdline == "" {
			return m, nil
		}
		return m.runShell(cmdline)
	}

	// A leading @subagent mention becomes a task delegation.
	if expanded, ok := m.expandAgentMentions(text); ok {
		m.appendMsg(ChatMsg{Kind: MsgUser, Text: text, Time: time.Now()})
		return m, m.startAgent(expanded)
	}

	// Expand @file references into the prompt.
	prompt, attached := m.expandFileRefs(text)

	// Parse attachment markers like [image #1] or [file #2] from the prompt.
	// These are inserted when the user pastes images/files from clipboard.
	indices, cleaned := parseAttachmentMarkers(prompt)

	// Pick up image paths mentioned in the prompt (bare paths or @path) so
	// they are routed through the vision bridge instead of the agent hunting
	// for tools to read them. Runs before the marker loop below, which only
	// covers clipboard attachments.
	imagePaths := m.imagePathsInPrompt(cleaned, attached)

	// Build the user message with attachments
	userMsg := provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock(cleaned)}}
	var imageAtts []attachment
	for _, idx := range indices {
		if idx > 0 && idx <= len(m.attachments) {
			att := m.attachments[idx-1]
			if att.IsImage && att.Base64 != "" {
				userMsg.Content = append(userMsg.Content, provider.ImageBlock(att.MediaType, att.Base64))
				imageAtts = append(imageAtts, att)
			}
		}
	}
	for _, path := range imagePaths {
		if att, err := addAttachment(path); err == nil && att.IsImage && att.Base64 != "" {
			imageAtts = append(imageAtts, *att)
		}
	}

	m.appendMsg(ChatMsg{Kind: MsgUser, Text: text, Time: time.Now()})
	if len(attached) > 0 {
		m.setStatus(fmt.Sprintf("attached %d file(s)", len(attached)))
	}
	if len(indices) > 0 {
		m.setStatus(fmt.Sprintf("sending %d attachment(s)", len(indices)))
	}
	m.attachments = nil

	// Vision bridge: when enabled, route images to the vision model and
	// replace the raw image blocks with structured text evidence so a
	// text-only model (DeepSeek) can answer.
	if len(imageAtts) > 0 && m.visionEnabled() {
		return m, m.startVisionBridge(userMsg, imageAtts)
	}
	return m, m.startAgentWithMessage(userMsg)
}

// visionEnabled reports whether the vision bridge is on: the resolved config
// has it enabled AND an API key is present.
func (m *Model) visionEnabled() bool {
	if m.deps.Loaded == nil {
		return false
	}
	cfg := m.deps.Loaded.Config.Vision
	if cfg == nil || cfg.Enabled == nil || !*cfg.Enabled {
		return false
	}
	return strings.TrimSpace(cfg.APIKey) != ""
}

// imagePathsInPrompt finds image file paths mentioned in the prompt — bare
// paths or @path references — so they can be sent through the vision bridge
// rather than left for the agent to discover with tools. attachedPaths are
// the @-expanded paths from expandFileRefs, which are skipped (they were
// already inlined as text).
func (m *Model) imagePathsInPrompt(prompt string, attachedPaths []string) []string {
	attachedSet := make(map[string]bool, len(attachedPaths))
	for _, p := range attachedPaths {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(m.deps.Cwd, p)
		}
		attachedSet[strings.ToLower(filepath.Clean(abs))] = true
	}

	var out []string
	seen := make(map[string]bool)
	fields := strings.Fields(prompt)
	for _, f := range fields {
		tok := strings.TrimSuffix(f, ",")
		tok = strings.TrimSuffix(tok, ".")
		tok = strings.TrimPrefix(tok, "@")
		if tok == "" || !isImageFile(tok) {
			continue
		}
		p := tok
		if !filepath.IsAbs(p) {
			p = filepath.Join(m.deps.Cwd, tok)
		}
		clean := filepath.Clean(p)
		if attachedSet[strings.ToLower(clean)] || seen[clean] {
			continue
		}
		if _, err := os.Stat(clean); err != nil {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	return out
}

// visionConfig builds the client config for the vision bridge.
func (m *Model) visionConfig() vision.Config {
	out := vision.Config{}
	if m.deps.Loaded == nil {
		return out
	}
	cfg := m.deps.Loaded.Config.Vision
	if cfg == nil {
		return out
	}
	out.APIKey = strings.TrimSpace(cfg.APIKey)
	out.Model = cfg.Model
	out.BaseURL = cfg.BaseURL
	return out
}

// startVisionBridge sends each image to the vision model asynchronously,
// then starts the agent with the evidence in place of the raw images.
func (m *Model) startVisionBridge(userMsg provider.Message, images []attachment) tea.Cmd {
	m.visionRunID++
	runID := m.visionRunID
	m.visionPending = true
	if m.visionCancel != nil {
		m.visionCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.visionCancel = cancel
	m.setStatus(fmt.Sprintf("vision: reading %d image(s)…", len(images)))
	cfg := m.visionConfig()

	return func() tea.Msg {
		defer cancel()
		client := vision.New(cfg)
		var evidence strings.Builder
		for i, att := range images {
			if err := ctx.Err(); err != nil {
				return visionDoneMsg{runID: runID, images: i, err: ctx.Err()}
			}
			result, err := client.Analyze(ctx, att.MediaType, att.Base64, "")
			if err != nil {
				return visionDoneMsg{runID: runID, images: i, err: err}
			}
			if evidence.Len() > 0 {
				evidence.WriteString("\n\n")
			}
			fmt.Fprintf(&evidence, "### Image %d (%s)\n%s", i+1, att.Name, strings.TrimSpace(vision.Render(result)))
		}
		// Replace the user text with the prompt plus the evidence block, and
		// drop the raw image blocks entirely.
		text := ""
		for _, b := range userMsg.Content {
			if b.Type == "text" {
				text = strings.TrimSpace(b.Text)
			}
		}
		final := provider.Message{Role: provider.RoleUser,
			Content: []provider.ContentBlock{provider.TextBlock(text + "\n\n" + evidence.String())}}
		return visionDoneMsg{runID: runID, images: len(images), msg: final}
	}
}

// startAgentWithMessage kicks off a run with a pre-built user message.
func (m *Model) startAgentWithMessage(userMsg provider.Message) tea.Cmd {
	m.history = append(m.history, userMsg)
	return m.startAgent("")
}

func (m *Model) runShell(cmdline string) (tea.Model, tea.Cmd) {
	m.appendMsg(ChatMsg{Kind: MsgUser, Text: "!" + cmdline, Time: time.Now()})
	m.shellSeq++
	callID := fmt.Sprintf("shell-%d", m.shellSeq)
	m.appendMsg(ChatMsg{
		Kind: MsgTool, ToolName: "bash", ToolTitle: cmdline,
		CallID: callID, ToolRunning: true, Time: time.Now(),
	})
	idx := len(m.msgs) - 1
	if m.pendingTools == nil {
		m.pendingTools = make(map[string]int)
	}
	m.pendingTools[callID] = idx
	request := permission.Request{Tool: "bash", Title: cmdline, Command: cmdline, Body: cmdline}
	level := permission.Allow
	if m.deps.Perms != nil {
		level = m.deps.Perms.Check(request)
	}
	if level == permission.Deny {
		return m, func() tea.Msg {
			return shellDoneMsg{callID: callID, err: fmt.Errorf("permission denied by policy: %s", cmdline)}
		}
	}

	return m, func() tea.Msg {
		ctx := context.Background()
		decision := agent.DecideAccept
		if level == permission.Ask {
			decision = m.makeAsker()(ctx, request)
			if decision == agent.DecideReject {
				return shellDoneMsg{callID: callID, err: fmt.Errorf("permission denied: %s", cmdline)}
			}
			if decision == agent.DecideAlways && m.deps.Perms != nil {
				m.deps.Perms.GrantSession(permission.SessionKey(request))
			}
		}
		if m.deps.Registry == nil {
			return shellDoneMsg{callID: callID, err: fmt.Errorf("bash tool is unavailable")}
		}
		tool, ok := m.deps.Registry.Get("bash")
		if !ok {
			return shellDoneMsg{callID: callID, err: fmt.Errorf("bash tool is unavailable")}
		}
		input, _ := json.Marshal(map[string]string{"command": cmdline, "description": cmdline})
		result, err := tool.Run(ctx, tools.Context{Cwd: m.deps.Cwd, SessionID: m.sessionID(), Agent: m.agentName}, input)
		if err != nil {
			return shellDoneMsg{callID: callID, err: err}
		}
		if result.IsError {
			return shellDoneMsg{callID: callID, err: fmt.Errorf("%s", result.Output)}
		}
		return shellDoneMsg{callID: callID, output: result.Output}
	}
}

// handlePermissionKey navigates the compact approval panel.
func (m *Model) handlePermissionKey(key string) (tea.Model, tea.Cmd) {
	const optionCount = 3

	switch key {
	case "up", "ctrl+p":
		if m.permCursor > 0 {
			m.permCursor--
		}
	case "down", "ctrl+n":
		if m.permCursor < optionCount-1 {
			m.permCursor++
		}
	case "enter":
		decision := []agent.PermissionDecision{
			agent.DecideAccept,
			agent.DecideAlways,
			agent.DecideReject,
		}[m.permCursor]
		m.answerPermission(decision)
		m.modal = modalNone
		switch decision {
		case agent.DecideAccept:
			m.setStatus("permission granted once")
		case agent.DecideAlways:
			m.setStatus("always allowed: " + permission.SessionKey(m.permReq))
		default:
			m.setStatus("permission denied")
		}
	case "esc":
		m.answerPermission(agent.DecideReject)
		m.modal = modalNone
		m.setStatus("permission denied")
	}
	return m, nil
}

// answerPermission delivers the user's decision to a waiting permission prompt.
func (m *Model) answerPermission(decision agent.PermissionDecision) {
	if m.permReply != nil {
		m.permReply <- decision
		m.permReply = nil
	}
}

// permissionView renders the compact permission selector.
func (m *Model) permissionView() string {
	s := m.styles
	var b strings.Builder
	req := m.permReq
	panelWidth := m.width - 8
	if panelWidth < 44 {
		panelWidth = 44
	}
	if panelWidth > 76 {
		panelWidth = 76
	}

	title := req.Title
	if title == "" {
		title = req.Tool
	}
	b.WriteString(s.Warning.Render("? ") + s.Accent.Render("Permission required") + "\n")
	b.WriteString(s.Base.Render(truncate(title, panelWidth-4)) + "\n")

	if req.Body != "" {
		body := strings.ReplaceAll(strings.ReplaceAll(req.Body, "\r", " "), "\n", " ")
		b.WriteString(s.Muted.Render(truncate(body, panelWidth-4)) + "\n")
	}

	options := []string{"Allow once", "Always allow", "Deny"}
	for i, option := range options {
		marker := "  "
		label := s.Muted
		if i == m.permCursor {
			marker = s.Primary.Render("❯ ")
			label = s.Base
		}
		b.WriteString(marker + label.Render(option) + "\n")
	}
	b.WriteString("\n" + s.Faint.Render("↑↓ select · enter confirm · esc deny"))
	return s.OverlayWarn.Width(panelWidth).Render(b.String())
}

// shellDoneMsg is delivered when a shell command finishes.
type shellDoneMsg struct {
	callID string
	idx    int // legacy fallback for messages produced by older callers
	output string
	err    error
}
