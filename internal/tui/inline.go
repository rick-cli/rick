package tui

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"rick/internal/config"
	"rick/internal/goal"
	"rick/internal/provider"
	"rick/internal/provider/catalog"
)

func nowFn() time.Time { return time.Now() }

func config_SplitModel(id string) (string, string) { return config.SplitModel(id) }

// modelsFor lists a provider's models, preferring what /auth fetched.
func (m *Model) modelsFor(id string) []provider.ModelInfo {
	if cred, ok := m.creds.Providers[id]; ok && len(cred.Models) > 0 {
		out := make([]provider.ModelInfo, 0, len(cred.Models))
		for _, mid := range cred.Models {
			contextWindow := cred.ContextWindows[mid]
			contextSource := cred.ContextSources[mid]
			if contextWindow > 0 && contextSource == provider.ContextSourceUnknown {
				// User-configured or written by an older rick: a deliberate
				// constraint, so it outranks catalogs and id inference.
				contextSource = provider.ContextSourceConfigured
			}
			if override, ok := provider.ProviderContextWindow(id, mid); ok {
				// The hardcoded override is weaker than an API-reported or
				// user-configured value; only fill in a missing/weak one.
				if contextSource != provider.ContextSourceConfigured &&
					contextSource != provider.ContextSourceAPI {
					contextWindow = override
					contextSource = provider.ContextSourceCatalog
				}
			}
			out = append(out, provider.ModelInfo{
				ID: mid, Name: mid, ContextWindow: contextWindow,
				ContextSource:  contextSource,
				SupportsImages: stringSliceContains(cred.VisionModels, mid),
			})
		}
		return provider.FilterChatModels(out)
	}
	if p, ok := m.deps.Providers[id]; ok && p != nil {
		return provider.FilterChatModels(p.Models())
	}
	return nil
}

// Inline command output.
//
// Commands render into the conversation rather than opening an overlay
// window. A command that needs a choice prints a numbered list and arms a
// "pending selection": the next thing you type, if it is a number, answers
// it. Anything else is treated as a normal prompt and the selection lapses.
//
// This keeps the transcript as the single surface — no modal state to trap
// the cursor, and the output stays scrollable in the history afterwards.

// pendingKind identifies what a numbered list is waiting for.
type pendingKind int

const (
	pendingNone pendingKind = iota
	pendingProvider
	pendingModel
	pendingTheme
	pendingSession
	pendingAgent
	pendingReasoning

	// theme add flow
	pendingThemeAdd    // chose "Add theme" from theme list
	pendingThemeSource // choosing file vs URL for theme
	pendingThemeURL    // waiting for URL input

	// session management
	pendingSessionMenu      // session management menu
	pendingSessionSearch    // waiting for search query
	pendingSessionRename    // waiting for new title
	pendingSessionDelete    // waiting for delete confirmation
	pendingSessionPage      // paginated session browse
	pendingSessionFavToggle // toggle favorite on a session

	// skill management
	pendingSkillAdd    // chose "Add skill"
	pendingSkillSource // choosing file vs URL vs create
	pendingSkillURL    // waiting for URL input
	pendingSkillName   // waiting for new skill name

	// tool toggle
	pendingToolToggle // tool list (toggle on select)

	// goal management
	pendingGoalMenu   // goal management menu
	pendingGoalTitle  // waiting for goal title
	pendingGoalBudget // waiting for budget number
	pendingGoalStep   // waiting for step description

	// plugin add flow
	pendingPluginAdd    // chose "Add plugin"
	pendingPluginSource // choosing file vs URL
	pendingPluginURL    // waiting for URL input

	// open skill / plugin source in explorer, or plugin URL
	pendingSkillOpen  // skill chosen — open source
	pendingPluginOpen // plugin chosen — open source or URL

	// MCP management
	pendingMCPMenu    // MCP management menu
	pendingMCPAddName // adding a custom MCP server: name
	pendingMCPAddType // adding: local vs remote
	pendingMCPAddCmd  // adding local: command
	pendingMCPAddURL  // adding remote: URL
	pendingMCPToggle  // toggle an MCP server on/off
	pendingMCPRemove  // remove an MCP server

	// permissions
	pendingPermission // permission mode/profile choice

	// multi-key management
	pendingKeyManage // key management menu
	pendingKeyAdd    // adding new keys
	pendingKeyRemove // removing a key
	pendingKeyMode   // setting rotation mode

	// maintenance
	pendingMaintenance

	// agent and background-job management
	pendingAgentManage
	pendingAgentAction
	pendingAgentChat
	pendingAgentSteer
	pendingJobManage

	// design mode
	pendingDesignPrompt // waiting for what to design
)

// pendingChoice is an armed numbered selection.
type pendingChoice struct {
	kind        pendingKind
	title       string // rendered heading of the armed menu
	options     []choiceOption
	context     string // e.g. the provider id while choosing a model
	edit        *editModal
	textInput   bool // true when waiting for free text, not a number
	choiceID    uint64
	cursor      int
	cursorMoved bool
	livePinned  bool // menu is re-rendered at the live tail while pending
}

type choiceOption struct {
	value  string
	label  string
	detail string
	active bool
}

type choiceButtonZone struct {
	id    string
	x     int
	y     int
	width int
}

const (
	choiceButtonBack   = "choice-back"
	choiceButtonSelect = "choice-select"
)

func isChoiceMenu(kind pendingKind) bool {
	return kind != pendingNone
}

func (m *Model) clearPending() { m.pending = pendingChoice{} }

// armChoice prints a numbered list into the chat and waits for a number.
func (m *Model) armChoice(title string, kind pendingKind, ctx string, opts []choiceOption) {
	m.choiceSeq++
	cursor := 0
	for i, option := range opts {
		if option.active {
			cursor = i
			break
		}
	}
	m.pending = pendingChoice{kind: kind, title: title, options: opts, context: ctx, choiceID: m.choiceSeq, cursor: cursor, livePinned: true}
	m.appendMsg(ChatMsg{Kind: MsgChoice, Text: title, Choices: opts, choiceID: m.choiceSeq, Time: nowFn()})
	m.refresh()
}

func (m *Model) movePendingCursor(delta int) {
	if !isChoiceMenu(m.pending.kind) || m.pending.textInput || len(m.pending.options) == 0 {
		return
	}
	m.pending.cursor = (m.pending.cursor + delta + len(m.pending.options)) % len(m.pending.options)
	m.pending.cursorMoved = true
}

func (m *Model) applyPendingCursor() (tea.Model, tea.Cmd) {
	if !isChoiceMenu(m.pending.kind) || m.pending.textInput || len(m.pending.options) == 0 {
		return m, nil
	}
	return m.applyChoice(m.pending.options[m.pending.cursor])
}

func (m *Model) backPendingChoice() (tea.Model, tea.Cmd) {
	if m.pending.kind == pendingModel {
		m.clearPending()
		return m.cmdModels()
	}
	m.clearPending()
	m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "cancelled", Time: nowFn()})
	return m, nil
}

// armInput prints a prompt and waits for free-form text input.
func (m *Model) armInput(prompt string, kind pendingKind, ctx string) {
	m.pending = pendingChoice{kind: kind, context: ctx, textInput: true}
	m.appendMsg(ChatMsg{Kind: MsgSystem, Text: prompt, Time: nowFn()})
}

// handlePendingInput answers an armed selection. It reports whether the input
// was consumed.
func (m *Model) handlePendingInput(text string) (tea.Model, tea.Cmd, bool) {
	if m.pending.kind == pendingNone {
		return m, nil, false
	}
	t := strings.TrimSpace(strings.ToLower(text))

	if t == "" || t == "q" || t == "cancel" || t == "esc" {
		m.clearPending()
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "cancelled", Time: nowFn()})
		return m, nil, true
	}
	if t == "b" || t == "back" {
		mm, cmd := m.backPendingChoice()
		return mm, cmd, true
	}

	// Text-input kinds accept any non-cancel text as the answer.
	if m.pending.textInput {
		kind, ctx := m.pending.kind, m.pending.context
		m.clearPending()
		mm, cmd := m.applyTextInput(kind, ctx, strings.TrimSpace(text))
		return mm, cmd, true
	}

	// Only digits answer the prompt; anything else is a normal message and
	// the selection quietly lapses so the user is never stuck.
	n, err := strconv.Atoi(t)
	if err != nil {
		// A bare name is also accepted when it matches exactly.
		for _, o := range m.pending.options {
			if strings.EqualFold(o.value, t) || strings.EqualFold(o.label, t) {
				mm, cmd := m.applyChoice(o)
				return mm, cmd, true
			}
		}
		m.clearPending()
		return m, nil, false
	}
	if n < 1 || n > len(m.pending.options) {
		m.appendMsg(ChatMsg{Kind: MsgError,
			Text: fmt.Sprintf("pick 1–%d, or press enter to select", len(m.pending.options)),
			Time: nowFn()})
		return m, nil, true
	}
	mm, cmd := m.applyChoice(m.pending.options[n-1])
	return mm, cmd, true
}

func (m *Model) applyChoice(o choiceOption) (tea.Model, tea.Cmd) {
	kind, ctx := m.pending.kind, m.pending.context
	m.clearPending()

	switch kind {
	case pendingProvider:
		return m.cmdModelsFor(o.value)
	case pendingModel:
		if o.value == "__back__" {
			return m.cmdModels()
		}
		id := o.value
		if ctx != "" {
			id = ctx + "/" + id
		}
		m.setModel(id)
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "model: " + id, Time: nowFn()})
		m.setStatus("model: " + shortModel(id))
	case pendingTheme:
		if o.value == "__add__" {
			return m.cmdThemeSource()
		}
		return m.applyTheme(o.value)
	case pendingThemeSource:
		switch o.value {
		case "file":
			path, err := openFileDialog("*.rick *.json")
			if err != nil {
				m.appendMsg(ChatMsg{Kind: MsgError, Text: "file dialog: " + err.Error(), Time: nowFn()})
				return m, nil
			}
			return m.addThemeFromSource(path)
		case "url":
			m.armInput("enter theme URL:", pendingThemeURL, "")
			return m, nil
		}
	case pendingSession:
		m.doResume(o.value)
	case pendingSessionFavToggle:
		// Toggle favorite on the selected session.
		sess, err := m.deps.Store.Load(o.value)
		if err != nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "favorite: " + err.Error(), Time: nowFn()})
			return m, nil
		}
		newFav := !sess.Favorite
		if err := m.deps.Store.SetFavorite(o.value, newFav); err != nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "favorite: " + err.Error(), Time: nowFn()})
			return m, nil
		}
		state := "unfavorited"
		if newFav {
			state = "favorited ★"
		}
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: state + ": " + truncate(o.label, 30), Time: nowFn()})
		return m, nil
	case pendingSessionMenu:
		return m.applySessionMenu(o.value)
	case pendingSessionPage:
		if o.value == "__next__" {
			return m.cmdSessionPage(ctx)
		}
		m.doResume(o.value)
	case pendingSessionDelete:
		if o.value == "__confirm__" {
			// ctx holds the session id to delete
			if err := m.deps.Store.Delete(ctx); err != nil {
				m.appendMsg(ChatMsg{Kind: MsgError, Text: "delete: " + err.Error(), Time: nowFn()})
			} else {
				m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "session deleted", Time: nowFn()})
			}
			return m, nil
		}
		// o.value is the session id; arm confirmation
		m.armChoice("delete session "+truncate(o.label, 30)+"? type 1 to confirm",
			pendingSessionDelete, o.value,
			[]choiceOption{{value: "__confirm__", label: "yes, delete"}})
		return m, nil
	case pendingAgent:
		m.agentName = o.value
		m.applyAgentPermissions()
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "agent: " + o.value, Time: nowFn()})
	case pendingAgentManage:
		return m.applyAgentManage(o.value)
	case pendingAgentAction:
		return m.applyAgentAction(o.value, ctx)
	case pendingReasoning:
		if lvl, ok := provider.ParseEffort(o.value); ok {
			return m.applyReasoning(lvl)
		}
	case pendingSkillAdd:
		if o.value == "__add__" {
			return m.cmdSkillSource()
		}
		return m.showSkillContent(o.value)
	case pendingSkillSource:
		switch o.value {
		case "file":
			path, err := openFileDialog("*.md")
			if err != nil {
				m.appendMsg(ChatMsg{Kind: MsgError, Text: "file dialog: " + err.Error(), Time: nowFn()})
				return m, nil
			}
			return m.addSkillFromFile(path)
		case "url":
			m.armInput("enter skill URL:", pendingSkillURL, "")
			return m, nil
		case "create":
			m.armInput("enter new skill name:", pendingSkillName, "")
			return m, nil
		}
	case pendingToolToggle:
		switch o.value {
		case "__enable_all__":
			m.disabledTools = map[string]bool{}
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "all tools enabled", Time: nowFn()})
			return m, nil
		case "__disable_all__":
			for _, name := range m.deps.Registry.Names() {
				m.disabledTools[name] = true
			}
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "all tools disabled", Time: nowFn()})
			return m, nil
		default:
			if m.disabledTools[o.value] {
				delete(m.disabledTools, o.value)
				m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "enabled: " + o.value, Time: nowFn()})
			} else {
				m.disabledTools[o.value] = true
				m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "disabled: " + o.value, Time: nowFn()})
			}
			return m.cmdToolsMenu()
		}
	case pendingGoalMenu:
		return m.applyGoalMenu(o.value)
	case pendingPluginAdd:
		if o.value == "__add__" {
			return m.cmdPluginSource()
		}
		// Toggle plugin on/off
		name := o.value
		if m.deps.Plugins.IsEnabled(name) {
			m.deps.Plugins.SetEnabled(name, false)
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "disabled: " + name, Time: nowFn()})
		} else {
			m.deps.Plugins.SetEnabled(name, true)
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "enabled: " + name, Time: nowFn()})
		}
		return m.cmdPluginsMenu()
	case pendingPluginSource:
		switch o.value {
		case "file":
			path, err := openFileDialog("*.json *.rick-plugin")
			if err != nil {
				m.appendMsg(ChatMsg{Kind: MsgError, Text: "file dialog: " + err.Error(), Time: nowFn()})
				return m, nil
			}
			return m.addPluginFromSource(path)
		case "url":
			m.armInput("enter plugin URL:", pendingPluginURL, "")
			return m, nil
		}
	case pendingSkillOpen:
		if o.value == "__add__" {
			return m.cmdSkillSource()
		}
		return m.openSkillSource(o.value)
	case pendingPluginOpen:
		if o.value == "__add__" {
			return m.cmdPluginSource()
		}
		return m.openPluginSource(o.value)
	case pendingMCPMenu:
		return m.cmdMcpApplyMenu(o.value)
	case pendingMCPToggle:
		if o.value == "toggle" {
			return m.cmdMcpToggleServer(ctx)
		}
		return m.cmdMcpRemoveServer(ctx)
	case pendingPermission:
		return m.applyPermissionMenu(o.value)
	case pendingKeyManage:
		return m.applyKeyManage(o.value)
	case pendingKeyMode:
		return m.applyKeyMode(o.value)
	case pendingJobManage:
		return m.showJobDetail(o.value)
	case pendingMaintenance:
		return m, runUninstall(o.value)
	}
	return m, nil
}

// applyTextInput handles free-form text answers for text-input pending kinds.
func (m *Model) applyTextInput(kind pendingKind, ctx, text string) (tea.Model, tea.Cmd) {
	switch kind {
	case pendingThemeURL:
		return m.addThemeFromSource(text)
	case pendingSessionSearch:
		metas, err := m.deps.Store.Search(text)
		if err != nil || len(metas) == 0 {
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "no sessions match: " + text, Time: nowFn()})
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
		m.armChoice("search results: "+text, pendingSession, "", opts)
		return m, nil
	case pendingSessionRename:
		if m.sess == nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "no active session to rename", Time: nowFn()})
			return m, nil
		}
		if err := m.deps.Store.Rename(m.sess.ID, text); err != nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "rename: " + err.Error(), Time: nowFn()})
			return m, nil
		}
		m.sess.Title = text
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "session renamed: " + text, Time: nowFn()})
		return m, nil
	case pendingSkillURL:
		return m.addSkillFromURL(text)
	case pendingSkillName:
		// Open the edit modal for a new skill.
		path := filepath.Join(config.GlobalDir(), "skills", text+".md")
		return m.showEditModal("skill", text, path)
	case pendingGoalTitle:
		return m.createAndStartGoal(text)
	case pendingGoalBudget:
		var n int
		if _, err := fmt.Sscanf(text, "%d", &n); err != nil || n <= 0 {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "budget must be a positive integer (thousands)", Time: nowFn()})
			return m, nil
		}
		g, err := m.deps.Goals.GetActive()
		if err != nil || g == nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "no active goal", Time: nowFn()})
			return m, nil
		}
		g.TokenBudget = n * 1000
		if err := m.deps.Goals.Save(g); err != nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "goal: " + err.Error(), Time: nowFn()})
			return m, nil
		}
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: fmt.Sprintf("budget set: %dk tokens", n), Time: nowFn()})
		return m, nil
	case pendingGoalStep:
		g, err := m.deps.Goals.GetActive()
		if err != nil || g == nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "no active goal", Time: nowFn()})
			return m, nil
		}
		g.Steps = append(g.Steps, goal.Step{
			ID:      fmt.Sprintf("s%d", len(g.Steps)+1),
			Content: text,
			Status:  "pending",
		})
		if err := m.deps.Goals.Save(g); err != nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "goal: " + err.Error(), Time: nowFn()})
			return m, nil
		}
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "step added: " + text, Time: nowFn()})
		return m, nil
	case pendingPluginURL:
		return m.addPluginFromSource(text)
	case pendingMCPAddName:
		return m.cmdMcpAddType(text)
	case pendingMCPAddType:
		if text == "remote" {
			m.armInput("remote URL:", pendingMCPAddURL, ctx)
			return m, nil
		}
		m.armInput("command (space-separated):", pendingMCPAddCmd, ctx)
		return m, nil
	case pendingMCPAddURL:
		srv := config.MCPServer{Type: "remote", URL: text}
		return m.cmdMcpSaveAndConnect(ctx, srv)
	case pendingMCPAddCmd:
		fields := strings.Fields(text)
		if len(fields) == 0 {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "a command is required", Time: nowFn()})
			return m, nil
		}
		srv := config.MCPServer{Type: "local", Command: fields}
		return m.cmdMcpSaveAndConnect(ctx, srv)
	case pendingAgentChat:
		if err := m.deps.AgentRegistry.Send(ctx, m.agentID, text); err != nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "chat: " + err.Error(), Time: nowFn()})
		} else {
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "message sent to " + ctx, Time: nowFn()})
		}
		return m, nil
	case pendingAgentSteer:
		if err := m.deps.AgentRegistry.Steer(ctx, m.agentID, text); err != nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "steer: " + err.Error(), Time: nowFn()})
		} else {
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "steering instruction sent to " + ctx, Time: nowFn()})
		}
		return m, nil
	case pendingKeyAdd:
		return m.applyKeyAdd(text)
	case pendingDesignPrompt:
		return m.startDesignRun(text)
	}
	return m, nil
}

// ---------- /models: provider first, then model ----------

// cmdModels lists providers with their model counts.
func (m *Model) cmdModels() (tea.Model, tea.Cmd) {
	if len(m.deps.Providers) == 0 {
		// Nothing to choose from: go straight to the setup flow rather than
		// telling the user to type another command.
		m.setStatus("no providers yet — connect one")
		return m.openAuth()
	}

	activeProv, _ := config_SplitModel(m.modelID)
	ids := make([]string, 0, len(m.deps.Providers))
	for id := range m.deps.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	opts := make([]choiceOption, 0, len(ids))
	for _, id := range ids {
		n := len(m.modelsFor(id))
		label := id
		if e, ok := catalog.Get(id); ok {
			label = e.Name
		}
		if cred, ok := m.creds.Providers[id]; ok && cred.Label != "" {
			label = cred.Label
		}
		opts = append(opts, choiceOption{
			value: id, label: label, active: id == activeProv,
			detail: fmt.Sprintf("%d model%s", n, plural(n)),
		})
	}
	m.armChoice("select a provider", pendingProvider, "", opts)
	return m, nil
}

// cmdModelsFor lists the models of one provider, honoring OnlyFree.
func (m *Model) cmdModelsFor(providerID string) (tea.Model, tea.Cmd) {
	models := m.modelsFor(providerID)
	cred, hasCred := m.creds.Providers[providerID]
	if hasCred && cred.OnlyFree {
		var filtered []provider.ModelInfo
		for _, mi := range models {
			if isFreeModel(mi.ID) {
				filtered = append(filtered, mi)
			}
		}
		models = filtered
	}
	if len(models) == 0 {
		m.appendMsg(ChatMsg{Kind: MsgSystem,
			Text: providerID + " reports no matching models — try /auth to change the filter",
			Time: nowFn()})
		return m, nil
	}

	_, activeModel := config_SplitModel(m.modelID)
	opts := make([]choiceOption, 0, len(models))
	for _, mi := range models {
		detail := ""
		if mi.ContextWindow > 0 {
			detail = humanTokens(mi.ContextWindow) + " ctx"
		}
		if mi.SupportsImages {
			if detail != "" {
				detail += " · "
			}
			detail += "vision"
		}
		opts = append(opts, choiceOption{
			value: mi.ID, label: mi.ID, detail: detail, active: mi.ID == activeModel,
		})
	}
	opts = append(opts, choiceOption{value: "__back__", label: "b back"})
	label := providerID
	if e, ok := catalog.Get(providerID); ok {
		label = e.Name
	}
	m.armChoice("select a model · "+label, pendingModel, providerID, opts)
	return m, nil
}

// isFreeModel reports whether a model id indicates a free-tier model.
func isFreeModel(id string) bool {
	return strings.HasSuffix(id, ":free") || strings.HasSuffix(strings.ToLower(id), "-free")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
