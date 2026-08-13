package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// cmdHelp prints the command reference into the conversation.
//
// Commands are the thing people actually look up, so they lead. Keybindings
// are a short appendix rather than half the screen.
func (m *Model) cmdHelp() (tea.Model, tea.Cmd) {
	s := m.styles
	w := m.contentWidth()
	var b strings.Builder

	group := func(name string, rows [][2]string) {
		b.WriteString(s.Accent.Render(name) + "\n")
		for _, r := range rows {
			b.WriteString("  " + s.Base.Render(padRight(r[0], 22)) +
				s.Faint.Render(truncate(r[1], max(20, w-26))) + "\n")
		}
		b.WriteString("\n")
	}

	group("session", [][2]string{
		{"/new", "start a fresh conversation"},
		{"/sessions", "resume an earlier session"},
		{"/undo  /redo", "step file changes back and forward"},
		{"/compact", "summarise the history to free context"},
		{"/ref", "load context from another session (query or id)"},
		{"/export", "write the transcript to a file"},
		{"/exit", "quit rick"},
	})
	group("model & providers", [][2]string{
		{"/models", "pick a provider, then a model"},
		{"/model <id>", "switch directly, e.g. openai/gpt-5"},
		{"/auth", "connect or edit a provider"},
		{"/webproviders", "configure web-search providers and routing"},
		{"/visionds", "toggle the vision bridge for text-only models (DeepSeek)"},
		{"/visionapi", "set/clear the free Google AI Studio vision key"},
		{"/update", "update Rick to the latest GitHub release"},
		{"/uninstall", "choose FULL or PART removal"},
	})
	group("appearance", [][2]string{
		{"/theme", "switch theme"},
		{"/details", "toggle full tool output"},
		{"/think", "toggle thinking sections (on/off)"},
		{"/thinking", "reasoning effort: off/low/medium/high"},
		{"/design <task>", "design engineering brief — audit & fix UI"},
		{"/design off", "remove the design brief from the prompt"},
		{"/config", "show the resolved settings"},
	})
	group("inspect", [][2]string{
		{"/tools", "list available tools"},
		{"/permissions", "show the permission policy"},
		{"/mcp", "manage MCP servers"},
		{"/plugins", "loaded plugins"},
		{"/stats", "token usage summary"},
		{"/agents", "list agents; view, chat, steer, attach, or kill"},
		{"/jobs", "show tracked background jobs"},
		{"/compact", "summarise history to free context"},
		{"/ram", "current Rick terminal RAM usage"},
		{"/goal <task>", "start an autonomous tracked goal"},
		{"/loop <dur> <task>", "work on a task for at least <dur>, retrying errors"},
		{"/refreshmodellist", "refresh model list from providers"},
		{"/init", "write a project brief"},
	})
	group("in the prompt", [][2]string{
		{"@path", "reference a file"},
		{"!command", "run a shell command"},
		{"tab", "switch build / plan mode"},
	})

	b.WriteString(s.Accent.Render("keys") + "\n")
	keys := [][2]string{
		{"enter", "send"},
		{"esc", "interrupt a run"},
		{"ctrl+c ×2", "quit"},
		{"ctrl+v", "paste (instant, multi-line safe)"},
		{"pgup/pgdn", "scroll a page"},
		{"shift+↑/↓", "scroll a line"},
		{"end", "jump to the newest output"},
		{"↑/↓", "previous prompts"},
		{"ctrl+x", "leader — then h m t n l u r d"},
	}
	for _, r := range keys {
		b.WriteString("  " + s.Base.Render(padRight(r[0], 22)) +
			s.Faint.Render(r[1]) + "\n")
	}
	b.WriteString("\n" + s.Accent.Render("mouse") + "\n")
	b.WriteString("  " + s.Base.Render(padRight("scroll wheel", 22)) + s.Faint.Render("scroll the transcript") + "\n")
	b.WriteString("  " + s.Base.Render(padRight("drag / shift+drag", 22)) + s.Faint.Render("select text — copy with ctrl+shift+c") + "\n")
	b.WriteString("  " + s.Base.Render(padRight("click tool line", 22)) + s.Faint.Render("expand / collapse tool output") + "\n")
	b.WriteString("  " + s.Base.Render(padRight("double-click path", 22)) + s.Faint.Render("copy file path to clipboard") + "\n")

	m.appendMsg(ChatMsg{Kind: MsgSystem, Text: strings.TrimRight(b.String(), "\n"), Time: time.Now()})
	return m, nil
}

// cmdConfig prints the resolved settings into the conversation.
func (m *Model) cmdConfig() (tea.Model, tea.Cmd) {
	s := m.styles
	var b strings.Builder
	b.WriteString(s.Accent.Render("settings") + "\n")

	row := func(k, v string) {
		b.WriteString("  " + s.Faint.Render(padRight(k, 16)) + s.Base.Render(v) + "\n")
	}
	row("model", m.displayModel())
	row("agent", m.agentName)
	row("theme", m.themeName)
	row("cwd", prettyPath(m.deps.Cwd))
	if root := m.deps.Loaded.ProjectRoot; root != "" {
		row("project root", prettyPath(root))
	}
	row("tool details", onOff(m.toolDetails))
	row("thinking", onOff(m.showThinking))
	row("mouse capture", onOff(m.deps.Loaded.TUI.Mouse))
	row("vision bridge", onOff(m.visionEnabled()))

	provs := make([]string, 0, len(m.deps.Providers))
	for id := range m.deps.Providers {
		provs = append(provs, id)
	}
	sort.Strings(provs)
	if len(provs) == 0 {
		row("providers", "none — run /auth")
	} else {
		row("providers", fmt.Sprintf("%d · %s", len(provs), strings.Join(provs, ", ")))
	}

	b.WriteString("\n" + s.Faint.Render("  /theme  /models  /auth  /visionds  /details to change these"))
	m.appendMsg(ChatMsg{Kind: MsgSystem, Text: b.String(), Time: time.Now()})
	return m, nil
}
