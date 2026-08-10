package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"rick/internal/agent"
	"rick/internal/config"
	"rick/internal/goal"
	"rick/internal/plugin"
	"rick/internal/provider"
	"rick/internal/session"
	"rick/internal/tools"
)

func (m *Model) closeModal() {
	m.modal = modalNone
	m.listFilter = ""
	m.input.Focus()
}

// ---------- specific modals ----------

// cmdThemes lists themes inline with an "Add theme" option.
func (m *Model) cmdThemes() (tea.Model, tea.Cmd) {
	var opts []choiceOption
	for _, name := range m.deps.Themes.SortedNames() {
		detail := ""
		switch name {
		case "pickle-rick":
			detail = "dark · light green"
		case "rick-black":
			detail = "pure black · neon green"
		case "evil-rick":
			detail = "blood red · dark romance"
		case "rick-neon":
			detail = "cyberpunk · hot pink"
		case "synthwave":
			detail = "retrowave · neon sunset"
		}
		opts = append(opts, choiceOption{
			value: name, label: name, detail: detail, active: name == m.themeName,
		})
	}
	opts = append(opts, choiceOption{value: "__add__", label: "＋ Add theme"})
	m.armChoice("select a theme", pendingTheme, "", opts)
	return m, nil
}

// cmdThemeSource shows the file-vs-URL sub-menu for adding a theme.
func (m *Model) cmdThemeSource() (tea.Model, tea.Cmd) {
	m.armChoice("add theme from…", pendingThemeSource, "", []choiceOption{
		{value: "file", label: "Select file"},
		{value: "url", label: "Enter URL"},
	})
	return m, nil
}

// addThemeFromSource loads a theme from a file path or URL and auto-selects it.
func (m *Model) addThemeFromSource(src string) (tea.Model, tea.Cmd) {
	var addErr error
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		addErr = m.deps.Themes.AddFromURL(src)
	} else {
		addErr = m.deps.Themes.AddFromFile(src)
	}
	if addErr != nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "theme add failed: " + addErr.Error(), Time: time.Now()})
		return m, nil
	}
	name := themeNameFromSource(src)
	m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "theme added: " + name, Time: time.Now()})
	return m.applyTheme(name)
}

// ---------- session management ----------

// cmdSessionsMenu shows the top-level session management menu.
func (m *Model) cmdSessionsMenu() (tea.Model, tea.Cmd) {
	m.armChoice("session management", pendingSessionMenu, "", []choiceOption{
		{value: "browse", label: "Browse & resume"},
		{value: "search", label: "Search"},
		{value: "fork", label: "Fork current"},
		{value: "rename", label: "Rename current"},
		{value: "delete", label: "Delete"},
		{value: "favorite", label: "Favorite"},
	})
	return m, nil
}

// applySessionMenu routes a session management menu choice.
func (m *Model) applySessionMenu(action string) (tea.Model, tea.Cmd) {
	switch action {
	case "browse":
		return m.cmdSessions()
	case "search":
		m.armInput("search sessions:", pendingSessionSearch, "")
		return m, nil
	case "fork":
		if m.sess == nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "no active session to fork", Time: time.Now()})
			return m, nil
		}
		forked, err := m.deps.Store.Fork(m.sess.ID)
		if err != nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "fork: " + err.Error(), Time: time.Now()})
			return m, nil
		}
		m.appendMsg(ChatMsg{Kind: MsgSystem,
			Text: fmt.Sprintf("forked session %s → %s", m.sess.ID, forked.ID), Time: time.Now()})
		return m, nil
	case "rename":
		if m.sess == nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "no active session to rename", Time: time.Now()})
			return m, nil
		}
		m.armInput("new title for session:", pendingSessionRename, "")
		return m, nil
	case "delete":
		return m.cmdSessionDeleteList()
	case "favorite":
		return m.cmdSessionFavoriteList()
	}
	return m, nil
}

// cmdSessionPage shows a paginated session list (10 at a time).
// ctx is the page offset as a string.
func (m *Model) cmdSessionPage(ctx string) (tea.Model, tea.Cmd) {
	offset, _ := strconv.Atoi(ctx)
	metas, err := m.deps.Store.List(m.deps.Cwd)
	if err != nil || len(metas) == 0 {
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "no saved sessions here yet", Time: time.Now()})
		return m, nil
	}
	pageSize := 10
	end := offset + pageSize
	if end > len(metas) {
		end = len(metas)
	}
	page := metas[offset:end]

	var opts []choiceOption
	for _, meta := range page {
		title := meta.Title
		if title == "" {
			title = "(untitled)"
		}
		fav := ""
		if meta.Favorite {
			fav = "★ "
		}
		opts = append(opts, choiceOption{
			value: meta.ID, label: fav + truncate(title, 38),
			detail: humanAge(meta.Updated),
			active: m.sess != nil && m.sess.ID == meta.ID,
		})
	}
	if end < len(metas) {
		opts = append(opts, choiceOption{
			value: "__next__", label: "→ next page",
			detail: fmt.Sprintf("%d–%d of %d", offset+1, end, len(metas)),
		})
	}
	nextCtx := strconv.Itoa(end)
	m.armChoice(fmt.Sprintf("sessions (%d–%d of %d)", offset+1, end, len(metas)),
		pendingSessionPage, nextCtx, opts)
	return m, nil
}

// cmdSessionDeleteList shows sessions to pick for deletion.
func (m *Model) cmdSessionDeleteList() (tea.Model, tea.Cmd) {
	metas, err := m.deps.Store.List(m.deps.Cwd)
	if err != nil || len(metas) == 0 {
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "no sessions to delete", Time: time.Now()})
		return m, nil
	}
	if len(metas) > 20 {
		metas = metas[:20]
	}
	var opts []choiceOption
	for _, meta := range metas {
		title := meta.Title
		if title == "" {
			title = "(untitled)"
		}
		opts = append(opts, choiceOption{
			value: meta.ID, label: truncate(title, 40), detail: humanAge(meta.Updated),
		})
	}
	m.armChoice("pick a session to delete", pendingSessionDelete, "", opts)
	return m, nil
}

// cmdSessionFavoriteList shows sessions to toggle favorite.
func (m *Model) cmdSessionFavoriteList() (tea.Model, tea.Cmd) {
	metas, err := m.deps.Store.List(m.deps.Cwd)
	if err != nil || len(metas) == 0 {
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "no sessions", Time: time.Now()})
		return m, nil
	}
	if len(metas) > 20 {
		metas = metas[:20]
	}
	var opts []choiceOption
	for _, meta := range metas {
		title := meta.Title
		if title == "" {
			title = "(untitled)"
		}
		fav := "☆"
		if meta.Favorite {
			fav = "★"
		}
		opts = append(opts, choiceOption{
			value: meta.ID, label: fav + " " + truncate(title, 38), detail: humanAge(meta.Updated),
		})
	}
	m.armChoice("toggle favorite (pick a session)", pendingSessionFavToggle, "", opts)
	return m, nil
}

// cmdSessionsArgs handles /sessions with subcommands (backward compat).
func (m *Model) cmdSessionsArgs(args string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return m.cmdSessionsMenu()
	}
	switch strings.ToLower(fields[0]) {
	case "search":
		query := strings.TrimSpace(strings.TrimPrefix(args, fields[0]))
		if query == "" {
			m.armInput("search sessions:", pendingSessionSearch, "")
			return m, nil
		}
		metas, err := m.deps.Store.Search(query)
		if err != nil || len(metas) == 0 {
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "no sessions match: " + query, Time: time.Now()})
			return m, nil
		}
		if len(metas) > 20 {
			metas = metas[:20]
		}
		var opts []choiceOption
		for _, meta := range metas {
			title := meta.Title
			if title == "" {
				title = "(untitled)"
			}
			opts = append(opts, choiceOption{
				value: meta.ID, label: truncate(title, 40),
				detail: humanAge(meta.Updated),
				active: m.sess != nil && m.sess.ID == meta.ID,
			})
		}
		m.armChoice("search results: "+query, pendingSession, "", opts)
		return m, nil
	case "fork":
		if m.sess == nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "no active session to fork", Time: time.Now()})
			return m, nil
		}
		forked, err := m.deps.Store.Fork(m.sess.ID)
		if err != nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "fork: " + err.Error(), Time: time.Now()})
			return m, nil
		}
		m.appendMsg(ChatMsg{Kind: MsgSystem,
			Text: fmt.Sprintf("forked session %s → %s", m.sess.ID, forked.ID), Time: time.Now()})
		return m, nil
	case "rename":
		title := strings.TrimSpace(strings.TrimPrefix(args, fields[0]))
		if title == "" {
			m.armInput("new title for session:", pendingSessionRename, "")
			return m, nil
		}
		if m.sess == nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "no active session to rename", Time: time.Now()})
			return m, nil
		}
		if err := m.deps.Store.Rename(m.sess.ID, title); err != nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "rename: " + err.Error(), Time: time.Now()})
			return m, nil
		}
		m.sess.Title = title
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "session renamed: " + title, Time: time.Now()})
		return m, nil
	default:
		return m.cmdSessionsMenu()
	}
}

// cmdSessions opens the same full-screen browser used by `rick resume`.
func (m *Model) cmdSessions() (tea.Model, tea.Cmd) {
	if m.running || m.agentCh != nil {
		m.interrupt()
	}
	browser, err := newResumeModel(m.styles)
	if err != nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "sessions: " + err.Error(), Time: time.Now()})
		return m, nil
	}
	m.resumeBrowser = browser
	if m.width > 0 && m.height > 0 {
		browser.width, browser.height = m.width, m.height
		browser.recalculateViewport()
	}
	return m, nil
}

// cmdAgents lists registered agents and falls back to the primary agent picker
// when no live hierarchy exists.
func (m *Model) cmdAgents() (tea.Model, tea.Cmd) {
	if m.deps.AgentRegistry != nil {
		entries := m.deps.AgentRegistry.List()
		if len(entries) > 0 {
			opts := make([]choiceOption, 0, len(entries))
			for _, entry := range entries {
				indent := strings.Repeat("  ", entry.Depth)
				label := fmt.Sprintf("%s[%d] %s", indent, entry.Depth, entry.Name)
				if entry.Description != "" {
					label += " \"" + truncate(entry.Description, 42) + "\""
				}
				opts = append(opts, choiceOption{value: entry.ID, label: label, detail: string(entry.Status) + " · " + entry.ID})
			}
			m.armChoice("active agents — choose one", pendingAgentManage, "", opts)
			return m, nil
		}
	}
	opts := []choiceOption{
		{value: "build", label: "build", detail: "all tools allowed", active: m.agentName == "build"},
		{value: "plan", label: "plan", detail: "edits and bash ask first", active: m.agentName == "plan"},
	}
	m.armChoice("select an agent", pendingAgent, "", opts)
	return m, nil
}

func (m *Model) applyTheme(name string) (tea.Model, tea.Cmd) {
	th := m.deps.Themes.Get(name)
	if th == nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "unknown theme: " + name, Time: time.Now()})
		return m, nil
	}
	m.themeName = name
	m.styles = NewStyles(th)
	m.rebuildMarkdown(m.contentWidth())
	m.tx.invalidateAll(m.contentWidth())
	m.refresh()
	if err := config.SaveThemeChoice(name); err != nil {
		m.setStatus("theme: " + name + " (not saved: " + err.Error() + ")")
	} else {
		m.setStatus("theme: " + name)
	}
	return m, nil
}

// themeNameFromSource derives a theme registry name from a file path or URL,
// matching the logic in theme.themeBaseName.
func themeNameFromSource(src string) string {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		trimmed := strings.TrimRight(src, "/")
		if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
			trimmed = trimmed[idx+1:]
		}
		src = trimmed
	} else {
		src = filepath.Base(src)
	}
	lower := strings.ToLower(src)
	if strings.HasSuffix(lower, ".rick") {
		return src[:len(src)-5]
	}
	if strings.HasSuffix(lower, ".json") {
		return src[:len(src)-5]
	}
	return src
}

func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// ---------- session commands ----------

func (m *Model) cmdNew() (tea.Model, tea.Cmd) {
	if m.running || m.agentCh != nil {
		m.interrupt()
	}
	m.cancelCompaction()
	m.compactionRunID++
	m.compactionActive = false
	m.autoCompactPending = false
	m.lastAutoCompact = time.Time{}
	m.agentCh = nil
	m.agentCancel = nil
	m.resetSwarmRuntime()
	m.pendingTools = map[string]int{}
	m.childActive = nil
	m.closeModal()
	m.auth.active = false

	m.msgs = nil
	m.history = nil
	m.sess = nil
	m.streamBuf.Reset()
	m.thinkBuf.Reset()
	m.tx.reset()
	m.resetStats()
	if m.deps.Todos != nil {
		m.deps.Todos.Clear()
	}
	tools.ResetFileState()
	m.refresh()
	m.setStatus("new session")
	return m, nil
}

func (m *Model) doResume(id string) {
	if m.running || m.agentCh != nil {
		m.interrupt()
	}
	m.cancelCompaction()
	m.compactionRunID++
	m.compactionActive = false
	m.autoCompactPending = false
	m.lastAutoCompact = time.Time{}
	m.compactIneffectiveStrikes = 0
	m.resetSwarmRuntime()
	sess, err := m.deps.Store.Load(id)
	if err != nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "resume: " + err.Error(), Time: time.Now()})
		return
	}
	m.sess = sess
	if len(sess.SentTranscript) > 0 {
		// Replay the exact bytes last sent so the provider cache prefix
		// survives the restart; seed the budget so the first turn emits
		// boundaries instead of starting cold.
		m.history = append([]provider.Message(nil), sess.SentTranscript...)
	} else {
		m.history = m.capHistoryCacheAware(compactHistory(sess.Messages))
	}
	if m.deps.Budget != nil {
		m.deps.Budget.SeedStability(m.history)
	}
	m.toolOutputs = toolOutputsFromHistory(sess.Messages)
	m.resetStats()
	m.restoreRunError(sess)
	m.usage = sess.Usage
	m.optimization = sess.Optimization
	if sess.Model != "" {
		m.modelID = sess.Model
		m.updateContextWindow()
	}
	if sess.Agent != "" {
		m.agentName = sess.Agent
		m.applyAgentPermissions()
	}
	if m.deps.Snapshots.Enabled() {
		m.deps.Snapshots.LoadHistory(sess.Snapshots)
	}
	m.msgs = messagesToChat(sess.Messages)
	m.tx.invalidateAll(m.contentWidth())
	m.refresh()
	m.setStatus(fmt.Sprintf("resumed %q (%d messages)", sess.Title, len(sess.Messages)))
}

func messagesToChat(msgs []provider.Message) []ChatMsg {
	var out []ChatMsg
	toolMsgIndex := map[string]int{}
	for _, msg := range msgs {
		turnBoundaryIndex := -1
		for _, b := range msg.Content {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				kind := MsgAssistant
				if msg.Role == provider.RoleUser {
					kind = MsgUser
				}
				out = append(out, ChatMsg{Kind: kind, Text: b.Text})
			case "thinking":
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				out = append(out, ChatMsg{Kind: MsgThinking, Text: b.Text})
			case "tool_use":
				toolMsgIndex[b.ID] = len(out)
				out = append(out, ChatMsg{
					Kind: MsgTool, CallID: b.ID, ToolName: b.Name,
					ToolTitle: b.Name, ToolInput: b.Input,
				})
			case "tool_result":
				if index, ok := toolMsgIndex[b.ToolUseID]; ok && index < len(out) {
					out[index].ToolOutput = truncate(b.Content, toolOutputPreviewChars)
					out[index].ToolErr = b.IsError
					turnBoundaryIndex = index
				}
			}
		}
		if turnBoundaryIndex >= 0 {
			out[turnBoundaryIndex].TurnBoundary = true
		}
	}
	return out
}

func compactHistory(history []provider.Message) []provider.Message {
	lastThinkingMessage := -1
	for i := len(history) - 1; i >= 0; i-- {
		for _, block := range history[i].Content {
			if block.Type == "thinking" {
				lastThinkingMessage = i
				break
			}
		}
		if lastThinkingMessage >= 0 {
			break
		}
	}

	out := make([]provider.Message, 0, len(history))
	for i, msg := range history {
		compacted := msg
		compacted.Content = make([]provider.ContentBlock, 0, len(msg.Content))
		for _, block := range msg.Content {
			if block.Type == "thinking" && i != lastThinkingMessage {
				continue
			}
			if block.Type == "tool_result" {
				block.Content = compactToolOutput(block.Content, historyToolOutputChars)
			}
			compacted.Content = append(compacted.Content, block)
		}
		if len(compacted.Content) > 0 {
			out = append(out, compacted)
		}
	}
	return out
}

func toolOutputsFromHistory(history []provider.Message) map[string]string {
	outputs := make(map[string]string)
	for _, msg := range history {
		for _, block := range msg.Content {
			if block.Type == "tool_result" && block.ToolUseID != "" {
				outputs[block.ToolUseID] = block.Content
			}
		}
	}
	return outputs
}

func (m *Model) saveSession() error {
	if len(m.history) == 0 {
		return nil
	}
	if m.deps.Store == nil {
		return fmt.Errorf("session store is unavailable")
	}
	if m.sess == nil {
		m.sess = &session.Session{
			ID:      session.NewID(),
			Cwd:     m.deps.Cwd,
			Created: time.Now(),
		}
	}
	// Keep the complete canonical transcript on disk for local expansion and
	// export. Only the provider-facing m.history is bounded by rebuildHistory.
	m.sess.Messages = m.buildHistory(0)
	// Persist the exact bounded view last sent so resume can replay it
	// byte-identically and warm the provider prompt cache.
	m.sess.SentTranscript = append([]provider.Message(nil), m.history...)
	m.sess.Model = m.modelID
	m.sess.Agent = m.agentName
	m.sess.RunError = m.lastRunError
	m.sess.Usage = m.usage
	m.sess.Optimization = m.optimization
	if m.deps.Snapshots != nil && m.deps.Snapshots.Enabled() {
		m.sess.Snapshots = m.deps.Snapshots.History()
	}
	if m.sess.Title == "" {
		m.sess.Title = session.Title(m.sess.Messages)
	}
	if err := m.deps.Store.Save(m.sess); err != nil {
		return err
	}
	// Publish the current pointer only after the referenced session exists.
	return m.deps.Store.SetCurrent(m.deps.Cwd, m.sess.ID)
}

func (m *Model) cmdUndo() (tea.Model, tea.Cmd) {
	if !m.deps.Snapshots.Enabled() {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "snapshots unavailable (git not found)", Time: time.Now()})
		return m, nil
	}
	snap, err := m.deps.Snapshots.Undo()
	if err != nil {
		m.setStatus(err.Error())
		return m, nil
	}
	m.appendMsg(ChatMsg{Kind: MsgSystem,
		Text: fmt.Sprintf("undid changes back to snapshot %s (%s)", snap.ID[:8], snap.Label), Time: time.Now()})
	return m, nil
}

func (m *Model) cmdRedo() (tea.Model, tea.Cmd) {
	if !m.deps.Snapshots.Enabled() {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "snapshots unavailable (git not found)", Time: time.Now()})
		return m, nil
	}
	snap, err := m.deps.Snapshots.Redo()
	if err != nil {
		m.setStatus(err.Error())
		return m, nil
	}
	m.appendMsg(ChatMsg{Kind: MsgSystem,
		Text: fmt.Sprintf("redid changes to snapshot %s (%s)", snap.ID[:8], snap.Label), Time: time.Now()})
	return m, nil
}

// ---------- compaction ----------

const autoCompactCooldown = 2 * time.Minute
const compactionMaxTokens = 2048
const compactionTimeout = 120 * time.Second

func addProviderUsage(total *provider.Usage, delta provider.Usage) {
	total.InputTokens += delta.InputTokens
	total.OutputTokens += delta.OutputTokens
	total.CacheReadTokens += delta.CacheReadTokens
	total.CacheWriteTokens += delta.CacheWriteTokens
}

func (m *Model) maybeAutoCompact() {
	cfg := m.deps.Loaded.Config
	if cfg.AutoCompact != nil && !*cfg.AutoCompact {
		return
	}
	if m.ctxWindow <= 0 || len(m.history) <= 6 || m.autoCompactPending ||
		(!m.lastAutoCompact.IsZero() && time.Since(m.lastAutoCompact) < autoCompactCooldown) {
		return
	}
	// Anti-thrash: after two compactions that each saved <10% of the window,
	// stop auto-compacting — repeated ineffective folds only burn aux tokens.
	if m.compactIneffectiveStrikes >= 2 {
		return
	}
	used := m.usage.Input + m.usage.CacheRead + m.usage.CacheWrite + m.usage.Output
	threshold := contextCompactionThreshold(m.ctxWindow, cfg.ContextReserve)
	if used > threshold {
		m.autoCompactPending = true
		m.setStatus("context reserve reached — compacting after this turn")
	}
}

func contextCompactionThreshold(contextWindow, reserve int) int {
	if contextWindow <= 0 {
		return 0
	}
	if reserve <= 0 {
		return contextWindow * 70 / 100
	}
	if reserve >= contextWindow {
		return 0
	}
	return contextWindow - reserve
}

func compactionTokenLimit(configured int) int {
	if configured > 0 && configured < compactionMaxTokens {
		return configured
	}
	return compactionMaxTokens
}

// cancelCompaction invalidates the current compaction and stops its provider
// request. The run ID prevents a late command result from changing a new
// session's history or re-enabling the wrong state.
func (m *Model) cancelCompaction() {
	if m.compactionCancel != nil {
		m.compactionCancel()
		m.compactionCancel = nil
	}
	if m.compactionActive {
		m.compactionRunID++
		m.compactionActive = false
	}
	m.autoCompactPending = false
}

func (m *Model) cmdCompact() (tea.Model, tea.Cmd) {
	if m.running {
		m.setStatus("cannot compact while working")
		return m, nil
	}
	if m.compactionActive {
		m.setStatus("compaction already in progress")
		return m, nil
	}
	if len(m.history) < 4 {
		m.setStatus("nothing to compact")
		return m, nil
	}

	prov, modelID, err := m.resolveProvider()
	if err != nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: err.Error(), Time: time.Now()})
		return m, nil
	}
	small := m.deps.Loaded.Config.SmallModel
	if small != "" {
		if pid, mid := config.SplitModel(small); m.deps.Providers[pid] != nil {
			prov = m.deps.Providers[pid]
			modelID = mid
		}
	}

	keep := 4
	if len(m.history) <= keep {
		return m, nil
	}
	head := append([]provider.Message(nil), m.history[:len(m.history)-keep]...)
	tail := append([]provider.Message(nil), m.history[len(m.history)-keep:]...)
	// Bound + redact the compaction input: the summary persists and re-enters
	// the prompt on every later turn, so secrets must never reach it, and the
	// aux call stays small even for a long session.
	head = agent.CompactBoundMessages(head)
	summaryMaxTokens := compactionTokenLimit(m.deps.Loaded.Config.MaxTokens)

	m.setStatus("compacting…")
	m.compactionActive = true
	m.compactionRunID++
	runID := m.compactionRunID
	ctx, cancel := context.WithTimeout(context.Background(), compactionTimeout)
	m.compactionCancel = cancel

	return m, func() tea.Msg {
		defer cancel()

		req := provider.Request{
			Model:     modelID,
			System:    agent.CompactPrompt,
			Messages:  append(head, provider.UserText("Summarise the conversation above now.")),
			MaxTokens: summaryMaxTokens,
		}
		ch := make(chan provider.Event, 128)
		go prov.Stream(ctx, req, ch)

		var (
			sb    strings.Builder
			usage provider.Usage
		)
		for {
			var ev provider.Event
			var ok bool
			select {
			case <-ctx.Done():
				return compactDoneMsg{runID: runID, err: ctx.Err(), modelID: modelID, usage: usage}
			case ev, ok = <-ch:
				if !ok {
					return compactDoneMsg{
						runID: runID, err: fmt.Errorf("compaction provider stream ended without a completion event"),
						modelID: modelID, usage: usage,
					}
				}
			}
			switch ev.Kind {
			case provider.EventText:
				sb.WriteString(ev.Text)
			case provider.EventUsage:
				if ev.Usage != nil {
					addProviderUsage(&usage, *ev.Usage)
				}
			case provider.EventError:
				if ev.Err == nil {
					ev.Err = fmt.Errorf("compaction provider returned an unspecified error")
				}
				return compactDoneMsg{runID: runID, err: ev.Err, modelID: modelID, usage: usage}
			case provider.EventDone:
				return compactDoneMsg{runID: runID, summary: sb.String(), tail: tail, modelID: modelID, usage: usage}
			}
		}
	}
}

type compactDoneMsg struct {
	runID   uint64
	summary string
	tail    []provider.Message
	modelID string
	usage   provider.Usage
	err     error
}

// ---------- goal commands ----------

// loopMaxRetries bounds how many times a /loop goal retries after an error
// before giving up.
const loopMaxRetries = 100

// cmdGoal handles /goal — shows an interactive menu, or handles backward-compat
// subcommands when args are provided.
func (m *Model) cmdGoal(args string) (tea.Model, tea.Cmd) {
	if m.deps.Goals == nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "goals not available", Time: time.Now()})
		return m, nil
	}
	args = strings.TrimSpace(args)

	// Bare /goal: show active goal status or prompt for a task.
	if args == "" {
		g, _ := m.deps.Goals.GetActive()
		if g != nil && g.Status == "active" {
			// Show progress + management menu.
			title := fmt.Sprintf("goal: %s · %s", g.Title, goal.Progress(g))
			m.armChoice(title, pendingGoalMenu, "active", []choiceOption{
				{value: "done", label: "Mark done"},
				{value: "abort", label: "Abort"},
				{value: "steps", label: "View steps"},
			})
			return m, nil
		}
		m.armInput("what should I work on?", pendingGoalTitle, "")
		return m, nil
	}

	// /goal <task> — create and immediately start working on it.
	return m.createAndStartGoal(args)
}

// createAndStartGoal creates a goal from the task text and kicks off the agent.
func (m *Model) createAndStartGoal(task string) (tea.Model, tea.Cmd) {
	g := &goal.Goal{
		Title:  task,
		Status: "active",
	}
	if err := m.deps.Goals.Save(g); err != nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "goal: " + err.Error(), Time: time.Now()})
		return m, nil
	}
	_ = m.deps.Goals.SetActive(g.ID)
	m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "goal set: " + task, Time: time.Now()})
	m.setStatus("goal: " + truncate(task, 40))
	// Start the agent with the goal as the prompt.
	m.appendMsg(ChatMsg{Kind: MsgUser, Text: task, Time: time.Now()})
	return m, m.startAgent(task)
}

// cmdLoop handles /loop <duration> <task> — like /goal but the agent keeps
// working (ignoring errors, retrying up to loopMaxRetries times) until at
// least <duration> of wall time has elapsed. The goal has an unlimited token
// budget; only the run time and the retry ceiling bound it.
func (m *Model) cmdLoop(args string) (tea.Model, tea.Cmd) {
	if m.deps.Goals == nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "goals not available", Time: time.Now()})
		return m, nil
	}
	args = strings.TrimSpace(args)
	if args == "" {
		m.appendMsg(ChatMsg{Kind: MsgError,
			Text: "usage: /loop <duration> <task> — e.g. /loop 30m fix all failing tests", Time: time.Now()})
		return m, nil
	}
	minRun, task := parseLoopArgs(args)
	if minRun <= 0 {
		m.appendMsg(ChatMsg{Kind: MsgError,
			Text: "usage: /loop <duration> <task> — duration like 10m, 1h, 90s", Time: time.Now()})
		return m, nil
	}
	if task == "" {
		m.appendMsg(ChatMsg{Kind: MsgError,
			Text: "usage: /loop <duration> <task> — give me something to work on", Time: time.Now()})
		return m, nil
	}
	if m.running || m.agentCh != nil {
		m.interrupt()
	}
	now := time.Now()
	g := &goal.Goal{
		Title:  task,
		Status: "active",
		LoopRun: &goal.LoopRun{
			MinRunSeconds: int(minRun.Seconds()),
			MaxRetries:    loopMaxRetries,
			StartedAt:     now,
		},
	}
	if err := m.deps.Goals.Save(g); err != nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "loop: " + err.Error(), Time: time.Now()})
		return m, nil
	}
	_ = m.deps.Goals.SetActive(g.ID)
	m.loop = &loopState{
		goalID:     g.ID,
		task:       task,
		minRun:     minRun,
		maxRetries: loopMaxRetries,
		start:      now,
	}
	m.appendMsg(ChatMsg{Kind: MsgSystem, Text: fmt.Sprintf(
		"loop set: %s — working for at least %s, retrying errors up to %d times, unlimited token budget",
		task, humanDuration(minRun), loopMaxRetries), Time: time.Now()})
	m.setStatus("loop: " + truncate(task, 40))
	m.appendMsg(ChatMsg{Kind: MsgUser, Text: task, Time: time.Now()})
	return m, m.startAgent(task)
}

// parseLoopArgs splits "/loop <duration> <task>" into the minimum run duration
// and the task text. The duration must be the first field and parseable by
// time.ParseDuration (e.g. "10m", "1h30m", "90s").
func parseLoopArgs(args string) (time.Duration, string) {
	fields := strings.Fields(args)
	if len(fields) < 2 {
		return 0, ""
	}
	d, err := time.ParseDuration(fields[0])
	if err != nil || d <= 0 {
		return 0, ""
	}
	return d, strings.TrimSpace(strings.TrimPrefix(args, fields[0]))
}

// loopState tracks one active /loop run in memory. The goal store holds the
// persistent metadata; this carries the runtime wall-clock and per-iteration
// counters that only matter while the loop is live.
type loopState struct {
	goalID     string
	task       string
	minRun     time.Duration
	maxRetries int
	retries    int
	start      time.Time
	tools      int // tool calls executed during the current iteration
}

// loopAdvance handles a clean finish of one loop iteration. Before the minimum
// run time has elapsed the agent is restarted to keep doing real work; after it
// the loop completes.
func (m *Model) loopAdvance() (tea.Cmd, bool) {
	if m.loop == nil {
		return m.finishRun(nil), true
	}
	if time.Since(m.loop.start) >= m.loop.minRun {
		m.appendMsg(ChatMsg{Kind: MsgSystem,
			Text: fmt.Sprintf("loop: minimum run time %s reached — task complete", humanDuration(m.loop.minRun)),
			Time: time.Now()})
		return m.finishLoop(nil), true
	}
	remaining := m.loop.minRun - time.Since(m.loop.start)
	didWork := m.loop.tools > 0
	m.loop.tools = 0
	note := fmt.Sprintf("Loop continuing: %s of the %s minimum run time remain. Keep doing real work on the task: %s",
		humanDuration(remaining), humanDuration(m.loop.minRun), m.loop.task)
	if !didWork {
		note = fmt.Sprintf("Loop continuing: %s of the %s minimum run time remain, and your last answer used no tools — that isn't real work. Keep actively using tools to make progress on: %s",
			humanDuration(remaining), humanDuration(m.loop.minRun), m.loop.task)
	}
	return m.continueLoop(note)
}

// loopRetry handles an errored iteration of a loop goal: the error is
// swallowed and the agent restarted, up to maxRetries times.
func (m *Model) loopRetry(err error) (tea.Cmd, bool) {
	if m.loop == nil {
		return m.finishRun(err), true
	}
	m.loop.retries++
	if m.loop.retries >= m.loop.maxRetries {
		m.appendMsg(ChatMsg{Kind: MsgError,
			Text: fmt.Sprintf("loop: %d retries exhausted — stopping", m.loop.maxRetries), Time: time.Now()})
		return m.finishLoop(fmt.Errorf("loop: %d retries exhausted (last error: %v)", m.loop.maxRetries, err)), true
	}
	m.loop.tools = 0
	note := fmt.Sprintf("Loop retry %d/%d: the previous attempt failed with: %v\nIgnore that error and keep working on the task: %s",
		m.loop.retries, m.loop.maxRetries, err, m.loop.task)
	return m.continueLoop(note)
}

// continueLoop cleans up the finished iteration without ending the loop and
// restarts the agent with the given continuation note.
func (m *Model) continueLoop(note string) (tea.Cmd, bool) {
	if m.agentCancel != nil {
		m.agentCancel()
		m.agentCancel = nil
	}
	m.agentCh = nil
	m.running = false
	m.resizeForActivity()
	// The note below is re-added to history by startAgent; rebuild first so it
	// is not duplicated by the MsgUser appended for the transcript.
	m.rebuildHistory()
	m.recordRunError(nil) // errors are swallowed by the loop
	if saveErr := m.saveSession(); saveErr != nil {
		m.reportSessionSaveError(saveErr)
	}
	m.refresh()
	m.appendMsg(ChatMsg{Kind: MsgUser, Text: note, Time: time.Now()})
	m.setStatus("loop: " + truncate(note, 40))
	return m.startAgent(note), true
}

// finishLoop ends a loop run: the goal is marked completed (nil err) or
// aborted (non-nil err) and the run finishes normally.
func (m *Model) finishLoop(err error) tea.Cmd {
	if m.loop != nil && m.deps.Goals != nil {
		if g, gerr := m.deps.Goals.GetActive(); gerr == nil && g != nil && g.ID == m.loop.goalID {
			if err != nil {
				g.Status = "aborted"
			} else {
				g.Status = "completed"
			}
			if g.LoopRun != nil {
				g.LoopRun.Retries = m.loop.retries
			}
			_ = m.deps.Goals.Save(g)
			_ = m.deps.Goals.ClearActive()
		}
	}
	m.loop = nil
	return m.finishRun(err)
}

// abortLoop stops an active loop without completing its goal, recording the
// goal as aborted. Used when a run ends outside the loop handlers (user
// interrupt, unexpected stream end).
func (m *Model) abortLoop() {
	if m.loop == nil {
		return
	}
	if m.deps.Goals != nil {
		if g, gerr := m.deps.Goals.GetActive(); gerr == nil && g != nil && g.ID == m.loop.goalID {
			g.Status = "aborted"
			if g.LoopRun != nil {
				g.LoopRun.Retries = m.loop.retries
			}
			_ = m.deps.Goals.Save(g)
			_ = m.deps.Goals.ClearActive()
		}
	}
	m.loop = nil
}

// cmdGoalMenu shows the interactive goal menu.
func (m *Model) cmdGoalMenu() (tea.Model, tea.Cmd) {
	g, err := m.deps.Goals.GetActive()
	if err != nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "goal: " + err.Error(), Time: time.Now()})
		return m, nil
	}
	if g == nil {
		m.armChoice("no active goal", pendingGoalMenu, "none", []choiceOption{
			{value: "create", label: "Create goal"},
			{value: "history", label: "View history"},
		})
		return m, nil
	}
	// Show progress in the title.
	title := fmt.Sprintf("goal: %s · %s", g.Title, goal.Progress(g))
	m.armChoice(title, pendingGoalMenu, "active", []choiceOption{
		{value: "budget", label: "Set budget"},
		{value: "done", label: "Mark done"},
		{value: "abort", label: "Abort"},
		{value: "step", label: "Add step"},
		{value: "steps", label: "View steps"},
	})
	return m, nil
}

// applyGoalMenu routes a goal menu choice.
func (m *Model) applyGoalMenu(action string) (tea.Model, tea.Cmd) {
	switch action {
	case "create":
		m.armInput("goal title:", pendingGoalTitle, "")
		return m, nil
	case "history":
		goals, err := m.deps.Goals.List()
		if err != nil || len(goals) == 0 {
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "no goals yet", Time: time.Now()})
			return m, nil
		}
		var b strings.Builder
		for _, g := range goals {
			fmt.Fprintf(&b, "  [%s] %s — %s\n", g.Status, g.Title, goal.Progress(&g))
		}
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: strings.TrimRight(b.String(), "\n"), Time: time.Now()})
		return m, nil
	case "budget":
		m.armInput("budget in k tokens:", pendingGoalBudget, "")
		return m, nil
	case "done":
		return m.goalDone()
	case "abort":
		return m.goalAbort()
	case "step":
		m.armInput("step description:", pendingGoalStep, "")
		return m, nil
	case "steps":
		g, err := m.deps.Goals.GetActive()
		if err != nil || g == nil {
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "no active goal", Time: time.Now()})
			return m, nil
		}
		if len(g.Steps) == 0 {
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "no steps yet", Time: time.Now()})
			return m, nil
		}
		var b strings.Builder
		for _, st := range g.Steps {
			marker := "○"
			switch st.Status {
			case "done":
				marker = "✓"
			case "in_progress":
				marker = "▶"
			case "skipped":
				marker = "⊘"
			}
			fmt.Fprintf(&b, "  %s [%s] %s\n", marker, st.ID, st.Content)
		}
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: strings.TrimRight(b.String(), "\n"), Time: time.Now()})
		return m, nil
	}
	return m, nil
}

func (m *Model) goalDone() (tea.Model, tea.Cmd) {
	g, err := m.deps.Goals.GetActive()
	if err != nil || g == nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "no active goal", Time: time.Now()})
		return m, nil
	}
	g.Status = "completed"
	if err := m.deps.Goals.Save(g); err != nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "goal: " + err.Error(), Time: time.Now()})
		return m, nil
	}
	_ = m.deps.Goals.ClearActive()
	m.loop = nil
	m.appendMsg(ChatMsg{Kind: MsgSystem, Text: fmt.Sprintf("goal completed: %s (%s)", g.Title, goal.Progress(g)), Time: time.Now()})
	m.setStatus("goal done")
	return m, nil
}

func (m *Model) goalAbort() (tea.Model, tea.Cmd) {
	g, err := m.deps.Goals.GetActive()
	if err != nil || g == nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "no active goal", Time: time.Now()})
		return m, nil
	}
	g.Status = "aborted"
	if err := m.deps.Goals.Save(g); err != nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "goal: " + err.Error(), Time: time.Now()})
		return m, nil
	}
	_ = m.deps.Goals.ClearActive()
	m.loop = nil
	m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "goal aborted: " + g.Title, Time: time.Now()})
	m.setStatus("goal aborted")
	return m, nil
}

// ---------- tools menu ----------

// cmdToolsMenu shows all tools with on/off status for interactive toggling.
func (m *Model) cmdToolsMenu() (tea.Model, tea.Cmd) {
	names := m.deps.Registry.Names()
	if len(names) == 0 {
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "no tools registered", Time: time.Now()})
		return m, nil
	}
	var opts []choiceOption
	for _, name := range names {
		status := "[on]"
		if m.disabledTools[name] {
			status = "[off]"
		}
		opts = append(opts, choiceOption{
			value: name, label: status + " " + name,
		})
	}
	opts = append(opts,
		choiceOption{value: "__enable_all__", label: "Enable all"},
		choiceOption{value: "__disable_all__", label: "Disable all"},
	)
	m.armChoice("toggle tools", pendingToolToggle, "", opts)
	return m, nil
}

// ---------- skill helpers ----------

// showSkillContent displays a skill's content inline.
func (m *Model) showSkillContent(name string) (tea.Model, tea.Cmd) {
	for _, s := range m.deps.Skills {
		if strings.EqualFold(s.Name, name) {
			var b strings.Builder
			fmt.Fprintf(&b, "## %s\n", s.Name)
			if s.Description != "" {
				fmt.Fprintf(&b, "%s\n\n", s.Description)
			}
			if len(s.Trigger) > 0 {
				fmt.Fprintf(&b, "Triggers: %s\n\n", strings.Join(s.Trigger, ", "))
			}
			b.WriteString(s.Body)
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: b.String(), Time: time.Now()})
			return m, nil
		}
	}
	m.appendMsg(ChatMsg{Kind: MsgError, Text: "skill not found: " + name, Time: time.Now()})
	return m, nil
}

// cmdSkillSource shows the add-skill sub-menu.
func (m *Model) cmdSkillSource() (tea.Model, tea.Cmd) {
	m.armChoice("add skill from…", pendingSkillSource, "", []choiceOption{
		{value: "file", label: "From file"},
		{value: "url", label: "From URL"},
		{value: "create", label: "Create new"},
	})
	return m, nil
}

// addSkillFromFile loads a skill from a .md file path.
func (m *Model) addSkillFromFile(path string) (tea.Model, tea.Cmd) {
	skills := plugin.LoadSkills(filepath.Dir(path))
	// Find the skill that matches the file.
	base := strings.TrimSuffix(filepath.Base(path), ".md")
	for _, s := range skills {
		if strings.EqualFold(s.Name, base) || s.Source == path {
			m.deps.Skills = append(m.deps.Skills, s)
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "skill added: " + s.Name, Time: time.Now()})
			return m, nil
		}
	}
	// If LoadSkills didn't find it (maybe no frontmatter), just note it.
	m.appendMsg(ChatMsg{Kind: MsgError, Text: "could not parse skill from: " + path, Time: time.Now()})
	return m, nil
}

// addSkillFromURL fetches a skill from a URL.
func (m *Model) addSkillFromURL(rawURL string) (tea.Model, tea.Cmd) {
	// Download to a temp file and parse.
	m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "fetching skill from: " + rawURL, Time: time.Now()})
	// For now, just note that URL skill loading requires network.
	// We'll use a simple HTTP GET approach.
	m.appendMsg(ChatMsg{Kind: MsgError, Text: "URL skill loading not yet implemented — use file or create", Time: time.Now()})
	return m, nil
}

// ---------- plugin helpers ----------

// cmdPluginsMenu shows plugins as an interactive toggle list with "Add plugin".
func (m *Model) cmdPluginsMenu() (tea.Model, tea.Cmd) {
	if m.deps.Plugins == nil || m.deps.Plugins.Len() == 0 {
		m.armChoice("no plugins loaded", pendingPluginAdd, "", []choiceOption{
			{value: "__add__", label: "＋ Add plugin"},
		})
		return m, nil
	}
	infos := m.deps.Plugins.List()
	var opts []choiceOption
	for _, info := range infos {
		status := "●"
		if !info.Enabled {
			status = "○"
		}
		desc := ""
		if info.Description != "" {
			desc = " — " + info.Description
		}
		opts = append(opts, choiceOption{
			value: info.Name, label: status + " " + info.Name, detail: desc,
		})
	}
	opts = append(opts, choiceOption{value: "__add__", label: "＋ Add plugin"})
	m.armChoice("plugins (pick to toggle, or open source)", pendingPluginOpen, "", opts)
	return m, nil
}

// openPluginSource opens a plugin's source file in the explorer, or its URL in
// the browser when the plugin is remote.
func (m *Model) openPluginSource(name string) (tea.Model, tea.Cmd) {
	man := m.pluginManifest(name)
	if man == nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "plugin not found: " + name, Time: time.Now()})
		return m, nil
	}
	if man.Source != "" {
		if err := openInExplorer(man.Source); err != nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "open: " + err.Error(), Time: time.Now()})
			return m, nil
		}
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "opened: " + man.Source, Time: time.Now()})
		return m, nil
	}
	m.appendMsg(ChatMsg{Kind: MsgError, Text: "plugin has no source path: " + name, Time: time.Now()})
	return m, nil
}

// pluginManifest finds a loaded plugin's manifest by name.
func (m *Model) pluginManifest(name string) *plugin.Manifest {
	for _, dir := range []string{
		filepath.Join(config.GlobalDir(), "plugins"),
		filepath.Join(m.deps.Loaded.ProjectRoot, ".rick", "plugins"),
	} {
		if ms, err := plugin.LoadDir(dir); err == nil {
			for i := range ms {
				if ms[i].Name == name {
					return &ms[i]
				}
			}
		}
	}
	return nil
}

// openSkillSource opens a skill's source file in the OS file explorer.
func (m *Model) openSkillSource(name string) (tea.Model, tea.Cmd) {
	for _, s := range m.deps.Skills {
		if strings.EqualFold(s.Name, name) {
			if s.Source == "" {
				m.appendMsg(ChatMsg{Kind: MsgError, Text: "skill has no source path: " + name, Time: time.Now()})
				return m, nil
			}
			if err := openInExplorer(s.Source); err != nil {
				m.appendMsg(ChatMsg{Kind: MsgError, Text: "open: " + err.Error(), Time: time.Now()})
				return m, nil
			}
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "opened: " + s.Source, Time: time.Now()})
			return m, nil
		}
	}
	m.appendMsg(ChatMsg{Kind: MsgError, Text: "skill not found: " + name, Time: time.Now()})
	return m, nil
}

// cmdPluginSource shows the add-plugin sub-menu.
func (m *Model) cmdPluginSource() (tea.Model, tea.Cmd) {
	m.armChoice("add plugin from…", pendingPluginSource, "", []choiceOption{
		{value: "file", label: "From file"},
		{value: "url", label: "From URL"},
	})
	return m, nil
}

// addPluginFromSource loads a plugin from a file path or URL.
func (m *Model) addPluginFromSource(src string) (tea.Model, tea.Cmd) {
	var manifests []plugin.Manifest
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		man, err := plugin.LoadURL(src)
		if err != nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "plugin add: " + err.Error(), Time: time.Now()})
			return m, nil
		}
		manifests = append(manifests, man)
	} else {
		ms, err := plugin.LoadDir(src)
		if err != nil {
			ms = nil
		}
		if len(ms) == 0 {
			man, ok := plugin.LoadFile(src)
			if !ok {
				m.appendMsg(ChatMsg{Kind: MsgError, Text: "no plugin manifests found at: " + src, Time: time.Now()})
				return m, nil
			}
			manifests = append(manifests, man)
		} else {
			manifests = ms
		}
	}
	for _, man := range manifests {
		hooks := plugin.ManifestToHooks(man)
		m.deps.Plugins.Register(hooks)
		m.deps.Plugins.SetEnabled(man.Name, man.IsEnabled())
	}
	names := make([]string, len(manifests))
	for i, man := range manifests {
		names[i] = man.Name
	}
	m.appendMsg(ChatMsg{Kind: MsgSystem,
		Text: fmt.Sprintf("added %d plugin(s): %s", len(manifests), strings.Join(names, ", ")), Time: time.Now()})
	return m, nil
}
