package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"rick/internal/agent"
	"rick/internal/swarm"
)

const activityVisibleRows = 6

type activityKind string

const (
	activityAgent activityKind = "agent"
	activityJob   activityKind = "job"
	activitySwarm activityKind = "swarm"
)

type activityItem struct {
	id          string
	kind        activityKind
	label       string
	detail      string
	started     time.Time
	agentID     string
	interactive bool
}

func (m *Model) activityItems() []activityItem {
	items := make([]activityItem, 0)
	if m.deps.AgentRegistry != nil {
		for _, entry := range m.deps.AgentRegistry.List() {
			if entry.Status != agent.AgentRunning && entry.Status != agent.AgentIdle {
				continue
			}
			detail := entry.Description
			if detail == "" {
				detail = fmt.Sprintf("agent · depth %d", entry.Depth)
			}
			items = append(items, activityItem{
				id: entry.ID, kind: activityAgent, label: entry.Name,
				detail: truncate(detail, 54), started: entry.Started,
				interactive: entry.Status == agent.AgentRunning,
			})
		}
	}

	for _, view := range m.teamViews {
		if !view.Active {
			continue
		}
		for _, name := range view.AgentOrd {
			agentView := view.Agents[name]
			if agentView == nil || agentView.Status == swarm.StatusDone || agentView.Status == swarm.StatusFailed {
				continue
			}
			items = append(items, activityItem{
				id: view.SwarmID + ":" + name, kind: activitySwarm,
				label: agentView.Name, detail: view.Name + " · " + name,
				started: view.Started, agentID: name,
				interactive: agentView.Status == swarm.StatusWorking,
			})
		}
	}

	// Tool calls already have a transcript row with their own lifecycle icon.
	// Do not add the same call to the footer activity panel: doing so renders
	// one command twice, especially noticeable for parallel tool batches.
	if m.jobs != nil {
		for _, job := range m.jobs.Recent(0) {
			job.mu.RLock()
			if job.Status == JobRunning {
				label := job.Label
				if label == "" {
					label = job.Kind
				}
				items = append(items, activityItem{
					id: job.ID, kind: activityJob, label: label,
					detail: job.Kind, started: job.Started,
					interactive: true,
				})
			}
			job.mu.RUnlock()
		}
	}
	return items
}

func (m *Model) activityPanel() string {
	items := m.activityItems()
	if len(items) == 0 {
		return ""
	}
	if m.activityCursor >= len(items) {
		m.activityCursor = len(items) - 1
	}
	if m.activityCursor < 0 {
		m.activityCursor = 0
	}

	start := 0
	if m.activityCursor >= activityVisibleRows {
		start = m.activityCursor - activityVisibleRows + 1
	}
	end := start + activityVisibleRows
	if end > len(items) {
		end = len(items)
	}

	var b strings.Builder
	b.WriteString(m.styles.Accent.Render(fmt.Sprintf("activity · %d running", len(items))))
	b.WriteByte('\n')
	for index := start; index < end; index++ {
		item := items[index]
		marker := "  "
		if m.activityFocused && index == m.activityCursor {
			marker = "▸ "
		}
		icon := "●"
		iconStyle := m.styles.Secondary
		if item.kind == activityJob {
			icon = "◆"
		}
		line := fmt.Sprintf("%s%s %-10s %s", marker, iconStyle.Render(icon), item.kind, item.label)
		if item.detail != "" {
			line += "  " + m.styles.Faint.Render(truncate(item.detail, max(12, m.contentWidth()-38)))
		}
		if m.activityFocused && index == m.activityCursor {
			line = m.styles.Accent.Bold(true).Render(line)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if m.running {
		if len(items) > activityVisibleRows {
			b.WriteString(m.styles.Faint.Render("  ↑/↓ scroll · enter view · shift+tab close · double-click open"))
		} else {
			b.WriteString(m.styles.Faint.Render("  shift+tab focus · enter view · double-click open"))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *Model) activityPanelHeight() int {
	panel := m.activityPanel()
	if panel == "" {
		return 0
	}
	return strings.Count(panel, "\n") + 2
}

func (m *Model) activityPrefix() string {
	var b strings.Builder
	b.WriteString(m.activityTodoPrefix())
	if panel := m.activityPanel(); panel != "" {
		b.WriteString(panel)
		b.WriteString("\n")
	}

	border := m.styles.PromptBorder
	if m.agentName == "plan" {
		border = m.styles.PlanBorder
	}
	prompt := m.styles.Accent.Render("› ")
	switch {
	case m.running:
		prompt = m.styles.Accent.Render(m.spinnerFrame() + " ")
	case strings.HasPrefix(m.input.Value(), "!"):
		prompt = m.styles.Warning.Render("! ")
	}
	inner := prompt + m.input.View()
	b.WriteString(border.Width(m.width - 2).Render(inner))
	b.WriteString("\n")
	return b.String()
}

func (m *Model) activityTodoPrefix() string {
	if m.deps.Todos == nil || m.activeSwarms != 0 {
		return ""
	}
	items := m.deps.Todos.Items()
	if len(items) == 0 {
		return ""
	}
	return m.renderTodos(items, m.contentWidth()) + "\n"
}

func (m *Model) activityPanelTop() int {
	return m.viewport.Height + strings.Count(m.activityTodoPrefix(), "\n")
}

// activityPanelBounds returns only the rows occupied by the activity panel.
// The prompt follows this range; including it here makes a mouse wheel over
// the input bar move the activity cursor instead of the chat viewport.
func (m *Model) activityPanelBounds() (int, int) {
	panel := m.activityPanel()
	if panel == "" {
		return 0, 0
	}
	top := m.activityPanelTop()
	return top, top + strings.Count(panel, "\n") + 1
}

func (m *Model) activityContainsY(y int) bool {
	top, bottom := m.activityPanelBounds()
	return bottom > top && y >= top && y < bottom
}

func (m *Model) activityAt(y int) (activityItem, bool) {
	items := m.activityItems()
	if len(items) == 0 {
		return activityItem{}, false
	}
	top, bottom := m.activityPanelBounds()
	itemTop := top + 1
	itemBottom := min(itemTop+activityVisibleRows, bottom)
	if y < itemTop || y >= itemBottom {
		return activityItem{}, false
	}
	start := 0
	if m.activityCursor >= activityVisibleRows {
		start = m.activityCursor - activityVisibleRows + 1
	}
	index := start + y - itemTop
	if index < 0 || index >= len(items) {
		return activityItem{}, false
	}
	return items[index], true
}

func (m *Model) openActivity(item activityItem) (tea.Model, tea.Cmd) {
	m.activityFocused = true
	switch item.kind {
	case activityAgent:
		return m.applyAgentManage(item.id)
	case activityJob:
		return m.showJobDetail(item.id)
	case activitySwarm:
		parts := strings.SplitN(item.id, ":", 2)
		text := fmt.Sprintf("swarm agent %s", item.agentID)
		if len(parts) == 2 {
			text += "\nrun: " + parts[0]
		}
		text += "\nstatus: running\nrole: " + item.detail
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: text, Time: time.Now()})
	}
	return m, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
