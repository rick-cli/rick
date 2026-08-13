package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"rick/internal/agent"
	"rick/internal/tools"
)

// MsgKind classifies a rendered chat entry.
type MsgKind int

// Chat entry kinds.
const (
	MsgUser MsgKind = iota
	MsgAssistant
	MsgThinking
	MsgTool
	MsgSystem
	MsgError
	MsgDiff
	MsgChoice
	MsgSwarm
)

// ChatMsg is one renderable entry in the transcript.
type ChatMsg struct {
	Kind MsgKind
	Text string
	Time time.Time

	// Tool entries
	ToolName    string
	ToolTitle   string
	ToolInput   json.RawMessage
	ToolOutput  string
	ToolErr     bool
	ToolRunning bool
	ToolElapsed time.Duration
	Choices     []choiceOption
	choiceID    uint64
	CallID      string
	// TurnBoundary marks the final tool result in a provider turn so streamed
	// reasoning from the next turn cannot be regrouped with the prior call.
	TurnBoundary bool

	// Diff entries
	DiffPath string
	DiffOld  string
	DiffNew  string

	// Swarm entries
	SwarmID string
}

const (
	toolOutputPreviewChars = 200
	historyToolOutputChars = 4000
)

// Render turns a chat entry into styled lines.
func (m *Model) renderMsg(msg ChatMsg, width int) string {
	s := m.styles
	switch msg.Kind {
	case MsgUser:
		// A dim accent bar down the left, no box: colour and indent do the
		// work, which keeps the transcript calm when it gets long.
		body := wrapIndent(msg.Text, width-2, "")
		var b strings.Builder
		for _, line := range strings.Split(body, "\n") {
			b.WriteString(s.Accent.Render("│ ") + s.Base.Render(line) + "\n")
		}
		return strings.TrimRight(b.String(), "\n")

	case MsgAssistant:
		txt := msg.Text
		if m.mdRenderer != nil && !m.rawMode {
			if out, err := m.mdRenderer.Render(txt); err == nil {
				return strings.TrimRight(out, "\n")
			}
		}
		return wrapIndent(txt, width, "")

	case MsgThinking:
		if !m.showThinking {
			return ""
		}
		body := wrapIndent(msg.Text, width-2, "  ")
		return s.Thinking.Render(body)

	case MsgChoice:
		// The currently pending menu is re-pinned at the live tail; suppress
		// this settled copy so it does not render twice. Once the menu is
		// answered or cancelled, pending clears and this renders normally.
		if m.pending.livePinned && m.pending.choiceID == msg.choiceID {
			return ""
		}
		var b strings.Builder
		b.WriteString(s.Accent.Render(msg.Text) + "\n")
		interactive := isChoiceMenu(m.pending.kind) && !m.pending.textInput &&
			m.pending.choiceID != 0 && m.pending.choiceID == msg.choiceID
		for i, o := range msg.Choices {
			marker := "  "
			if interactive && i == m.pending.cursor {
				marker = "▸ "
			}
			line := fmt.Sprintf("%s%2d %s", marker, i+1, padRight(o.label, 34))
			if o.detail != "" {
				line += o.detail
			}
			if o.active {
				line += "  ← current"
			}
			if interactive && i == m.pending.cursor {
				line = s.Accent.Bold(true).Render(line)
			} else if o.active {
				line = s.Accent.Render(line)
			} else {
				line = s.Base.Render(line)
			}
			b.WriteString(strings.TrimRight(line, " ") + "\n")
		}
		if interactive {
			b.WriteString(s.Faint.Render("  ↑/↓ select · enter confirm · esc/backspace back · type a number") + "\n")
			b.WriteString(m.renderChoiceButtons())
			return b.String()
		}
		hint := "type a number"
		if msg.Text != "" && strings.Contains(msg.Text, "·") {
			hint += " · b back"
		}
		b.WriteString(s.Faint.Render("  " + hint + " · enter to cancel"))
		return b.String()

	case MsgSwarm:
		return m.renderSwarmMessage(msg, width)

	case MsgSystem:
		return s.Muted.Render(wrapIndent(msg.Text, width, ""))

	case MsgError:
		body := wrapIndent(msg.Text, width-2, "")
		var b strings.Builder
		for i, line := range strings.Split(body, "\n") {
			mark := "  "
			if i == 0 {
				mark = s.Error.Render("✗ ")
			}
			b.WriteString(mark + s.Error.Render(line) + "\n")
		}
		return strings.TrimRight(b.String(), "\n")

	case MsgDiff:
		return s.RenderDiff(msg.DiffPath, msg.DiffOld, msg.DiffNew, width, m.diffMode, m.diffThreshold, m.linksEnabled())

	case MsgTool:
		return m.renderTool(msg, width)
	}
	return ""
}

// renderPendingMenu renders the currently armed choice menu as a pinned tail,
// keeping it visible at the bottom of the viewport even while a long stream
// arrives after it. It returns "" when there is no pending menu.
func (m *Model) renderPendingMenu(width int) string {
	p := m.pending
	if p.kind == pendingNone || p.textInput || len(p.options) == 0 {
		return ""
	}
	s := m.styles
	var b strings.Builder
	b.WriteString(s.Accent.Render(p.title) + "\n")
	for i, o := range p.options {
		marker := "  "
		if i == p.cursor {
			marker = "▸ "
		}
		line := fmt.Sprintf("%s%2d %s", marker, i+1, padRight(o.label, 34))
		if o.detail != "" {
			line += o.detail
		}
		if o.active {
			line += "  ← current"
		}
		if i == p.cursor {
			line = s.Accent.Bold(true).Render(line)
		} else if o.active {
			line = s.Accent.Render(line)
		} else {
			line = s.Base.Render(line)
		}
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	b.WriteString(s.Faint.Render("  ↑/↓ select · enter confirm · esc/backspace back · type a number") + "\n")
	b.WriteString(m.renderChoiceButtons())
	return b.String()
}

func (m *Model) choiceButtonStyle(active bool) lipgloss.Style {
	c := m.styles.Theme().Color
	foreground, background := c("text"), c("border")
	if active {
		foreground, background = c("background"), c("borderActive")
	}
	return lipgloss.NewStyle().Foreground(foreground).Background(background).Bold(true).Padding(0, 1)
}

func (m *Model) renderChoiceButtons() string {
	back := m.choiceButtonStyle(false).Render("← Back")
	selectButton := m.choiceButtonStyle(true).Render("↵ Select")
	return "  " + back + " " + selectButton
}

func (m *Model) choiceButtonWidths() (int, int) {
	return lipgloss.Width(m.choiceButtonStyle(false).Render("← Back")),
		lipgloss.Width(m.choiceButtonStyle(true).Render("↵ Select"))
}

func (m *Model) fullToolOutput(msg ChatMsg) string {
	if m.toolOutputs != nil {
		if output, ok := m.toolOutputs[msg.CallID]; ok {
			return output
		}
	}
	return msg.ToolOutput
}

func compactToolOutput(output string, limit int) string {
	if limit <= 0 || len(output) <= limit {
		return output
	}
	runes := []rune(output)
	if len(runes) <= limit {
		return output
	}
	marker := fmt.Sprintf("\n… [tool output truncated; original %d chars] …\n", len(runes))
	markerRunes := []rune(marker)
	available := limit - len(markerRunes)
	if available <= 1 {
		return string(runes[:limit])
	}
	head := available * 3 / 4
	tail := available - head
	return string(runes[:head]) + marker + string(runes[len(runes)-tail:])
}

func (m *Model) renderTool(msg ChatMsg, width int) string {
	s := m.styles

	icon := "→"
	iconStyle := s.Faint
	switch {
	case msg.ToolRunning:
		icon = m.spinnerFrame()
		iconStyle = s.Secondary
	case msg.ToolErr:
		icon = "✗"
		iconStyle = s.ToolErr
	}

	title := msg.ToolTitle
	if title == "" {
		title = msg.ToolName
	}
	head := iconStyle.Render(icon) + " " +
		s.ToolOutput.Render(msg.ToolName+": ") +
		s.Faint.Render(truncate(title, max(10, width-len(msg.ToolName)-16)))
	if msg.ToolElapsed > 250*time.Millisecond {
		head += s.Faint.Render(fmt.Sprintf("  %s", msg.ToolElapsed.Round(10*time.Millisecond)))
	}

	if msg.ToolRunning {
		return head
	}

	// Compact mode: header only, unless the tool failed or was click-expanded.
	expanded := m.expandedTools[msg.CallID]
	if !m.toolDetails && !msg.ToolErr && !expanded {
		if n := outputLineCount(msg.ToolOutput); n > 0 {
			head += s.Faint.Render(fmt.Sprintf("  (%d lines)", n))
		}
		return head
	}

	out := strings.TrimRight(m.fullToolOutput(msg), "\n")
	if out == "" {
		return head
	}
	lines := strings.Split(out, "\n")
	limit := 16
	if msg.ToolErr {
		limit = 30
	}
	truncatedBy := 0
	if len(lines) > limit {
		truncatedBy = len(lines) - limit
		lines = lines[:limit]
	}
	style := s.ToolOutput
	if msg.ToolErr {
		style = s.ToolErr
	}
	var b strings.Builder
	b.WriteString(head + "\n")
	linksOn := m.linksEnabled()
	for _, l := range lines {
		b.WriteString(s.Faint.Render("  │ ") + style.Render(truncate(linkifyLine(expandTabs(l), linksOn), width-4)) + "\n")
	}
	if truncatedBy > 0 {
		b.WriteString(s.Faint.Render(fmt.Sprintf("  │ … %d more lines", truncatedBy)) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func outputLineCount(s string) int {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// renderSwarmMessage renders a live swarm status card.
func (m *Model) renderSwarmMessage(msg ChatMsg, width int) string {
	if msg.SwarmID == "" {
		return ""
	}
	return m.FormatSwarmCard(msg.SwarmID, width)
}
func (m *Model) spinnerFrame() string {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	return frames[m.spinnerTick%len(frames)]
}

// wrapIndent hard-wraps text at the exact terminal width.
func wrapIndent(text string, width int, indent string) string {
	if width < 1 {
		return ""
	}
	var out []string
	for _, source := range strings.Split(text, "\n") {
		line := indent + source
		if line == "" {
			out = append(out, "")
			continue
		}
		var chunk strings.Builder
		cells := 0
		for _, r := range []rune(line) {
			rw := lipgloss.Width(string(r))
			if cells > 0 && cells+rw > width {
				out = append(out, chunk.String())
				chunk.Reset()
				cells = 0
			}
			if rw > width {
				continue
			}
			chunk.WriteRune(r)
			cells += rw
		}
		out = append(out, chunk.String())
	}
	return strings.Join(out, "\n")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// toolMsgFromEvent builds a chat entry from an agent tool event.
func toolMsgFromEvent(ev *agent.ToolEvent, running bool) ChatMsg {
	return ChatMsg{
		Kind:        MsgTool,
		Time:        time.Now(),
		CallID:      ev.CallID,
		ToolName:    ev.Name,
		ToolTitle:   ev.Title,
		ToolInput:   ev.Input,
		ToolOutput:  truncate(ev.Output, toolOutputPreviewChars),
		ToolErr:     ev.IsError,
		ToolRunning: running,
		ToolElapsed: ev.Elapsed,
	}
}

// diffMsgFromMeta extracts a diff entry from tool metadata, if present.
func diffMsgFromMeta(meta map[string]any) (ChatMsg, bool) {
	if meta == nil {
		return ChatMsg{}, false
	}
	path, _ := meta["path"].(string)
	oldS, okOld := meta["old"].(string)
	newS, okNew := meta["new"].(string)
	if path == "" || (!okOld && !okNew) {
		return ChatMsg{}, false
	}
	if oldS == newS {
		return ChatMsg{}, false
	}
	return ChatMsg{
		Kind: MsgDiff, Time: time.Now(),
		DiffPath: shortPath(path), DiffOld: oldS, DiffNew: newS,
	}, true
}

func shortPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	parts := strings.Split(p, "/")
	if len(parts) <= 3 {
		return p
	}
	return ".../" + strings.Join(parts[len(parts)-3:], "/")
}

// renderTodos renders the checklist panel.
func (m *Model) renderTodos(items []tools.TodoItem, width int) string {
	if len(items) == 0 {
		return ""
	}
	s := m.styles
	done := 0
	for _, it := range items {
		if it.Status == "completed" || it.Status == "cancelled" {
			done++
		}
	}
	var b strings.Builder
	b.WriteString(s.Muted.Render(fmt.Sprintf("tasks %d/%d", done, len(items))) + "\n")
	for _, it := range items {
		var mark, text string
		switch it.Status {
		case "completed":
			mark = s.Success.Render("✓")
			text = s.Faint.Render(it.Content)
		case "in_progress":
			mark = s.Warning.Render("▸")
			text = s.Base.Render(it.Content)
		case "cancelled":
			mark = s.Faint.Render("−")
			text = s.Faint.Render(it.Content)
		default:
			mark = s.Faint.Render("○")
			text = s.Muted.Render(it.Content)
		}
		b.WriteString("  " + mark + " " + truncate(text, width-6) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
