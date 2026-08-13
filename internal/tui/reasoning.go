package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"rick/internal/config"
	"rick/internal/provider"
)

// cmdReasoning shows or sets the reasoning effort for the active model. The
// selector is populated from the model's advertised vocabulary when available,
// with provider/model fallbacks for endpoints that do not publish metadata.
func (m *Model) cmdReasoning(args string) (tea.Model, tea.Cmd) {
	_, modelID := config.SplitModel(m.modelID)

	if m.reasoningStyle == provider.ReasoningStyleNone {
		m.appendMsg(ChatMsg{Kind: MsgSystem,
			Text: m.displayModel() + " is not a reasoning model — nothing to set",
			Time: time.Now()})
		return m, nil
	}
	if m.reasoningStyle == provider.ReasoningStyleAlways {
		m.appendMsg(ChatMsg{Kind: MsgSystem,
			Text: m.displayModel() + " always reasons; the level cannot be changed",
			Time: time.Now()})
		return m, nil
	}

	// An argument sets the level directly.
	if strings.TrimSpace(args) != "" {
		lvl, ok := provider.ParseEffort(args)
		if !ok {
			m.appendMsg(ChatMsg{Kind: MsgError,
				Text: "unknown level " + strconvQuote(args) + " — available: " + formatReasoningEfforts(m.reasoningCapabilities.Efforts),
				Time: time.Now()})
			return m, nil
		}
		return m.applyReasoning(lvl)
	}

	var opts []choiceOption
	for _, lvl := range m.reasoningCapabilities.Efforts {
		opts = append(opts, choiceOption{
			value:  string(lvl),
			label:  string(lvl),
			detail: effortDetail(lvl, m.reasoningStyle, m.maxTokens()),
			active: lvl == m.reasoning,
		})
	}
	m.armChoice("reasoning effort · "+modelID, pendingReasoning, "", opts)
	return m, nil
}

// cmdThink toggles visibility of the thinking sections in the transcript.
// An optional "on"/"off" argument sets the state explicitly instead.
func (m *Model) cmdThink(args string) (tea.Model, tea.Cmd) {
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "on":
		m.showThinking = true
	case "off":
		m.showThinking = false
	case "":
		m.showThinking = !m.showThinking
	default:
		m.appendMsg(ChatMsg{Kind: MsgError,
			Text: "usage: /think [on|off] — toggle thinking section visibility",
			Time: time.Now()})
		return m, nil
	}
	m.tx.invalidateAll(m.contentWidth())
	m.refresh()
	m.appendMsg(ChatMsg{Kind: MsgSystem,
		Text: "thinking: " + onOff(m.showThinking), Time: time.Now()})
	m.setStatus("thinking " + onOff(m.showThinking))
	return m, nil
}

func (m *Model) applyReasoning(lvl provider.ReasoningEffort) (tea.Model, tea.Cmd) {
	if len(m.reasoningCapabilities.Efforts) > 0 && !containsReasoningEffort(m.reasoningCapabilities.Efforts, lvl) {
		m.appendMsg(ChatMsg{Kind: MsgError,
			Text: "reasoning level " + strconvQuote(string(lvl)) + " is not supported by " + m.displayModel() + " — available: " + formatReasoningEfforts(m.reasoningCapabilities.Efforts),
			Time: time.Now()})
		return m, nil
	}
	m.reasoning = lvl
	// Showing the reasoning stream only makes sense when there is one.
	m.showThinking = lvl != provider.ReasoningOff
	m.tx.invalidateAll(m.contentWidth())
	m.refresh()
	m.appendMsg(ChatMsg{Kind: MsgSystem,
		Text: "reasoning: " + string(lvl), Time: time.Now()})
	m.setStatus("reasoning: " + string(lvl))
	return m, nil
}

func containsReasoningEffort(efforts []provider.ReasoningEffort, wanted provider.ReasoningEffort) bool {
	for _, effort := range efforts {
		if effort == wanted {
			return true
		}
	}
	return false
}

func formatReasoningEfforts(efforts []provider.ReasoningEffort) string {
	values := make([]string, 0, len(efforts))
	for _, effort := range efforts {
		values = append(values, string(effort))
	}
	return strings.Join(values, ", ")
}

// effortDetail explains what a level costs in the model's own terms.
func effortDetail(lvl provider.ReasoningEffort, style provider.ReasoningStyle, maxTok int) string {
	switch lvl {
	case provider.ReasoningOff:
		return "no thinking, fastest"
	case provider.ReasoningOn:
		return "thinking enabled; model controls depth"
	}
	if style == provider.ReasoningStyleAnthropic {
		if b := lvl.Budget(maxTok); b > 0 {
			return fmt.Sprintf("%s thinking budget", humanTokens(b))
		}
		return "unavailable at this max_tokens"
	}
	switch lvl {
	case provider.ReasoningMinimal:
		return "barely thinks, fastest"
	case provider.ReasoningLow:
		return "quick reasoning"
	case provider.ReasoningMedium:
		return "balanced"
	case provider.ReasoningHigh:
		return "deep reasoning"
	case provider.ReasoningXHigh:
		return "extra-deep reasoning"
	case provider.ReasoningMax:
		return "maximum reasoning"
	}
	if style == provider.ReasoningStyleGLM {
		return "thinking enabled; model controls depth"
	}
	return ""
}

// maxTokens is the response limit for the active model.
func (m *Model) maxTokens() int {
	if n := m.deps.Loaded.Config.MaxTokens; n > 0 {
		return n
	}
	return 8192
}

func strconvQuote(s string) string { return "\"" + strings.TrimSpace(s) + "\"" }

// reasoningSegment renders the level for the status line, or "".
func (m *Model) reasoningSegment() string {
	switch m.reasoningStyle {
	case provider.ReasoningStyleNone:
		return ""
	case provider.ReasoningStyleAlways:
		return "reasoning"
	}
	if m.reasoning == provider.ReasoningOff || m.reasoning == "" {
		return ""
	}
	return "reasoning:" + string(m.reasoning)
}
