package tui

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"
	"golang.org/x/term"

	"rick/internal/agent"
	"rick/internal/cache"
	"rick/internal/config"
	"rick/internal/goal"
	"rick/internal/mcp"
	"rick/internal/permission"
	"rick/internal/plugin"
	"rick/internal/provider"
	"rick/internal/sandbox"
	"rick/internal/session"
	"rick/internal/swarm"
	"rick/internal/theme"
	"rick/internal/tools"
	"rick/internal/usage"
	"rick/pkg/contextbudget"
)

// Deps is everything the TUI needs from the outside world.
type Deps struct {
	Loaded    *config.Loaded
	Themes    *theme.Registry
	ThemeDirs *theme.Watcher
	Registry  *tools.Registry
	Todos     *tools.TodoStore
	Perms     *permission.Engine
	Sandbox   *sandbox.Holder
	Store     *session.Store
	Snapshots *session.Snapshotter
	Providers map[string]provider.Provider
	MCP       *mcp.Manager
	Plugins   *plugin.Registry
	Skills    []plugin.Skill
	// Budget is the shared session context manager: content-addressed dedup,
	// cache boundaries, and reversible live-zone compression.
	Budget       *contextbudget.Budget
	SwarmManager *swarm.SwarmManager
	Goals        *goal.Store
	Agent        string
	Cwd          string
	Version      string
	ResumeID     string
	InitialMsg   string
	// Credentials is already-loaded auth data (avoids double-load at startup).
	Credentials *config.Credentials
	// Usage persists cumulative token usage per model per day.
	Usage         *usage.Tracker
	AgentRegistry *agent.Registry
}

// modal identifies the active overlay, if any.
type modalKind int

const (
	modalNone modalKind = iota
	modalPermission
)

// cacheMissNoiseFloor is the per-turn miss threshold below which a shortfall
// is cache-breakpoint granularity noise, not a real miss (pi uses 1024).
const cacheMissNoiseFloor = 1024

// Model is the root bubbletea model.
type Model struct {
	deps      Deps
	styles    *Styles
	themeName string

	width, height int
	ready         bool

	viewport   viewport.Model
	tx         *transcript
	input      textarea.Model
	mdRenderer *glamour.TermRenderer

	msgs        []ChatMsg
	chatContent string

	// conversation state
	history   []provider.Message
	sess      *session.Session
	agentName string
	agentID   string
	modelID   string
	designMode         bool
	wheelPreScroll     int
	wheelActiveUntil   time.Time
	wheelActiveDir     int
	pasteSuppressSeed  int
	lastRuneAt         time.Time
	pasteBurstRunes    []rune
	pasteBurstInserted int
	pasteBurstAt       time.Time
	// pendingDivergence carries the prefix-divergence diagnostics from an
	// agent event until the next usage row arrives (they precede EvUsage).
	pendingDivergence string

	// repoMapOnce/repoMapBlock build the RepoMap once per session so every
	// turn sends a byte-identical system suffix (provider cache stays warm).
	repoMapOnce  sync.Once
	repoMapBlock string
	// repoDiskOnce/repoDiskDir open the content-addressed disk cache used to
	// reuse RepoMap blocks across sessions with an unchanged git tree.
	repoDiskOnce sync.Once
	repoDiskDir  *cache.Dir

	// sysPartsKey/sysPartsStable/sysPartsVolatile freeze the volatile system
	// prompt bytes (skills match + environment: git state) once per
	// (session, model, agent) so the provider cache prefix stays
	// byte-identical. The session id in the key keeps a brand-new session
	// from reusing the previous one's frozen bytes.
	sysPartsKey      string
	sysPartsStable   string
	sysPartsVolatile string

	// toolSchemasKey/toolSchemasPinned freeze the provider-facing tool list
	// per (session, model, agent, tool-toggles) so mid-session plugin churn
	// never changes the cached prefix bytes.
	toolSchemasKey    string
	toolSchemasPinned []provider.ToolSchema

	// cachePrevPrompt tracks the previous request's prompt footprint so
	// per-turn cache misses can be detected and surfaced (noise floor 1024
	// tokens, like pi's cache-stats.ts). A miss is only measured when the
	// turn reports cache tokens at all.
	cachePrevPrompt int
	cacheMissTokens int
	cacheMissCount  int
	cacheMissStreak int
	cacheLastUsage  time.Time
	// cacheLastMissReason is the classified cause of the most recent miss
	// ("idle gap (cache expired)", "provider eviction (session prefix
	// expired)", ...) so /stats and the MCP UI can surface it later.
	cacheLastMissReason string
	// requestSeq counts provider requests in the primary session so the
	// persisted per-request telemetry has stable chronological indices.
	requestSeq int

	// streaming
	running     bool
	agentCh     chan agent.Event
	agentCancel context.CancelFunc
	streamBuf   strings.Builder
	thinkBuf    strings.Builder
	// Armed by EvTurnEnd and committed after that turn's tool results,
	// immediately before the next model turn.
	turnBoundaryPending bool
	pendingTools        map[string]int // callID -> index into msgs
	shellSeq            uint64
	toolOutputs         map[string]string // full tool output, kept out of render entries
	spinnerTick         int

	// permission prompt
	permReq    permission.Request
	permReply  chan agent.PermissionDecision
	permGate   chan struct{}
	permCursor int

	// generic list modal (models, themes, sessions, help)
	modal modalKind
	// resumeBrowser is the shared full-screen session browser used by /sessions.
	resumeBrowser *resumeModel

	listFilter string

	// file picker
	picker filePicker

	// presentation flags
	showThinking  bool
	toolDetails   bool
	rawMode       bool
	diffMode      DiffMode
	diffThreshold int

	// leader key
	leaderActive bool
	leaderKey    string

	// active team tracking; all view state is owned by the Bubble Tea loop.
	activeSwarms int
	teamViews    map[string]*SwarmView

	// input history
	inputHist []string
	histIdx   int
	histDraft string

	// status
	status             string
	statusTime         time.Time
	lastRunError       string
	usage              session.Usage
	optimization       session.OptimizationUsage
	ctxWindow          int
	lastAutoCompact    time.Time
	autoCompactPending bool
	compactionActive   bool
	compactionRunID    uint64
	compactionCancel   context.CancelFunc
	// compactIneffectiveStrikes counts consecutive compactions that saved
	// less than 10% of the context. After two strikes automatic compaction
	// is skipped and the reason surfaced, so a dead-end session stops
	// burning aux tokens on compactions that cannot shrink it.
	compactIneffectiveStrikes int
	quitting                  bool

	// provider auth flow
	auth  authState
	web   webSearchState
	creds *config.Credentials
	// providers declared by rick.json/env, which /auth must not delete
	pinnedProviders map[string]bool

	// consecutive quiet theme polls, used to back the interval off
	themeIdle int

	// startup tip, chosen once per launch
	tip string
	// title of the most recent session here, if any
	resumable   string
	slashCursor int

	// reasoning effort for the active model and its model-specific controls
	reasoning             provider.ReasoningEffort
	reasoningStyle        provider.ReasoningStyle
	reasoningCapabilities provider.ReasoningCapabilities

	// billed totals across the session (usage tracks context occupancy)
	billed session.Usage

	// timing for the current/last turn
	turnStart   time.Time
	turnElapsed time.Duration

	// armed inline numbered selection
	pending       pendingChoice
	choiceSeq     uint64
	choiceButtons []choiceButtonZone

	// ctrl+c must be pressed twice to quit
	quitArmed bool
	quitAt    time.Time

	// theme hot-reload
	themeWatch *theme.Watcher

	// cumulative entry renders, for verifying the render cache
	renderCount int

	// active subagent labels
	childActive []string

	// job tracking for background processes
	jobs *JobTracker

	// active swarms panel
	swarmPanel string

	// program handle, needed so background goroutines can Send messages
	program *tea.Program

	// attachments for the pending prompt (images, files, etc.)
	attachments              []attachment
	clipboardShortcutWasDown bool
	lastClipboardPaste       time.Time
	// pasteSuppress buffers terminal re-delivery of a paste we already
	// inserted via the direct clipboard read. Windows Terminal converts its
	// native Ctrl+V into per-character key events after the fact; without
	// this, pasting would double-insert the text. The buffered runes are
	// compared against the pasted text and dropped while they match.
	pasteSuppress []rune
	pasteTarget   string
	// pasteNewlineUntil is set when a paste with newlines is inserted; the
	// terminal's re-delivery of those newlines as Enter-family keys must be
	// dropped until this time, even after the rune match cleared pasteTarget.
	pasteNewlineUntil time.Time
	focused           bool

	// wheelKeyTimes tracks recent up/down key timestamps and directions.
	// With mouse capture off (so the terminal owns selection), Windows
	// Terminal delivers the scroll wheel as a rapid burst of same-direction
	// up/down key events; a same-direction burst within a short window is
	// treated as wheel scrolling instead of prompt history.
	wheelKeyTimes []wheelKey

	// per-tool expand/collapse (mouse click toggles when toolDetails is off)
	expandedTools map[string]bool

	// disabledTools tracks tools the user has toggled off via /tools
	disabledTools map[string]bool

	// toolRowMap maps content rows to tool call IDs for mouse click handling
	toolRowMap []toolRowEntry

	// double-click detection
	lastClickTime time.Time
	lastClickY    int

	// always-visible activity browser state
	activityCursor  int
	activityFocused bool

	// mouseEnabled is true only while an interactive activity/control surface is
	// present. Keeping it false for the ordinary view preserves terminal text
	// selection and copy behavior.
	mouseEnabled bool

	// agentRunID prevents stale drain ticks from consuming a later run's events.
	agentRunID uint64

	// visionRunID tracks the active vision bridge run so stale completions
	// (from an interrupted run) are dropped.
	visionRunID uint64
	// visionPending is true while image attachments are being sent to the
	// vision model before the agent turn starts.
	visionPending bool
	// visionCancel cancels the in-flight vision bridge HTTP call.
	visionCancel context.CancelFunc

	// loop tracks an active /loop goal run; nil when not looping.
	loop *loopState
}

// toolRowEntry records the content-row span of one rendered tool block.
type toolRowEntry struct {
	callID   string
	startRow int
	endRow   int
}

// SetProgram wires the running tea.Program so async work can deliver messages.
func (m *Model) SetProgram(p *tea.Program) { m.program = p }

// tick messages
type spinnerTickMsg time.Time
type themePollMsg time.Time
type readAgentMsg struct{ runID uint64 }
type visionDoneMsg struct {
	runID  uint64
	images int
	err    error
	msg    provider.Message
}
type statusMsg struct {
	text string
	quit bool
}
type errMsg struct{ err error }
type permAskMsg struct {
	req   permission.Request
	reply chan agent.PermissionDecision
}
type todosChangedMsg struct{ items []tools.TodoItem }

// New builds the root model.
func New(d Deps) *Model {
	cfg := d.Loaded.Config
	tuiCfg := d.Loaded.TUI
	registry := d.AgentRegistry
	if registry == nil {
		depth := 1
		if cfg.SubagentDepth != nil {
			depth = *cfg.SubagentDepth
		}
		registry = agent.NewRegistry(depth, cfg.MaxBackground)
	}
	_, rootCancel := context.WithCancel(context.Background())
	rootID, err := registry.Register(&agent.AgentEntry{
		ID: "orchestrator", Name: "orchestrator", Depth: 0,
		Status: agent.AgentIdle, Cancel: rootCancel,
	})
	if err != nil {
		rootID = "orchestrator"
	}

	themeName := tuiCfg.Theme
	th := d.Themes.Get(themeName)
	if th == nil {
		themeName = "pickle-rick"
		th = d.Themes.Get(themeName)
	}
	if th == nil {
		names := d.Themes.Names()
		if len(names) > 0 {
			themeName = names[0]
			th = d.Themes.Get(themeName)
		}
	}

	ta := textarea.New()
	ta.Placeholder = "ask anything · / commands · @ files · ! shell"
	ta.Prompt = ""
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.SetHeight(1)
	ta.Focus()
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.BlurredStyle.Base = lipgloss.NewStyle()
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("alt+enter", "shift+enter", "ctrl+enter"),
		key.WithHelp("alt+enter/shift+enter/ctrl+enter", "newline"),
	)
	ta.KeyMap.DeleteWordBackward = key.NewBinding(key.WithKeys("ctrl+backspace", "alt+backspace", "ctrl+w"), key.WithHelp("ctrl+backspace", "delete word"))

	m := &Model{
		deps:          d,
		styles:        NewStyles(th),
		themeName:     themeName,
		input:         ta,
		agentName:     "build",
		agentID:       rootID,
		modelID:       cfg.Model,
		tx:            newTranscript(),
		tip:           pickTip(),
		resumable:     latestSessionTitle(d),
		themeWatch:    d.ThemeDirs,
		pendingTools:  map[string]int{},
		permGate:      make(chan struct{}, 1),
		showThinking:  tuiCfg.ShowThinking == nil || *tuiCfg.ShowThinking,
		toolDetails:   tuiCfg.ToolDetails != nil && *tuiCfg.ToolDetails,
		diffMode:      DiffMode(orString(tuiCfg.DiffMode, "auto")),
		diffThreshold: orInt(tuiCfg.DiffThreshold, 120),
		leaderKey:     orString(tuiCfg.Keybinds.Leader, "ctrl+x"),
		histIdx:       -1,
		teamViews:     map[string]*SwarmView{},
		ctxWindow:     200000,
		jobs:          NewJobTracker(50),
		focused:       true,
		expandedTools: map[string]bool{},
		disabledTools: map[string]bool{},
	}
	m.permGate <- struct{}{}

	// Credentials are already loaded in buildDeps — reuse them.
	if d.Credentials != nil {
		m.creds = d.Credentials
	} else {
		m.creds = &config.Credentials{Providers: map[string]config.Credential{}}
	}
	// Anything already configured at startup that /auth did not save is
	// owned by rick.json or the environment; never delete those.
	m.pinnedProviders = map[string]bool{}
	for id := range cfg.Providers {
		if _, ours := m.creds.Providers[id]; !ours {
			m.pinnedProviders[id] = true
		}
	}
	if d.Agent == "plan" || d.Agent == "build" {
		m.agentName = d.Agent
	}
	m.applyAgentPermissions()
	m.registerTaskTool()
	m.rebuildMarkdown(80)
	m.updateContextWindow()
	return m
}

// updateContextWindow syncs the status gauge and reasoning support with the
// active model.
func (m *Model) updateContextWindow() {
	provID, modelID := config.SplitModel(m.modelID)

	// Reasoning support is a property of the model, so re-detect on every
	// switch and keep the user's level only when the new model supports it.
	previousStyle := m.reasoningStyle
	var advertised *provider.ModelInfo
	if p, ok := m.deps.Providers[provID]; ok {
		for _, mi := range p.Models() {
			if mi.ID == modelID {
				modelInfo := mi
				advertised = &modelInfo
				break
			}
		}
	}
	caps := provider.ReasoningCapabilitiesForProvider(provID, modelID, advertised)
	m.reasoningCapabilities = caps
	m.reasoningStyle = caps.Style
	switch {
	case caps.Style == provider.ReasoningStyleNone:
		m.reasoning = provider.ReasoningOff
	case caps.Style == provider.ReasoningStyleAlways:
		m.reasoning = caps.Default
	case m.reasoning == "" || previousStyle != caps.Style || !containsReasoningEffort(caps.Efforts, m.reasoning):
		m.reasoning = caps.Default
	}

	// Prefer live API metadata over catalogs and id heuristics. The source is
	// persisted alongside the value so an inferred value never silently wins
	// over a context limit returned by the provider.
	known := provider.KnownProviderContextWindow(provID, modelID)
	bestValue, bestSource := known, provider.ContextSourceInferred
	var stored int
	var storedSource provider.ContextSource
	if m.creds != nil {
		if cred, ok := m.creds.Providers[provID]; ok {
			stored = cred.ContextWindows[modelID]
			storedSource = cred.ContextSources[modelID]
		}
	}
	bestValue, bestSource = betterContextCandidate(bestValue, bestSource, stored, storedSource)
	if advertised != nil {
		bestValue, bestSource = betterContextCandidate(
			bestValue, bestSource, advertised.ContextWindow, advertised.ContextSource)
	}
	if bestValue > 0 {
		m.ctxWindow = bestValue
	} else {
		m.ctxWindow = 200000
	}
}

func betterContextCandidate(current int, currentSource provider.ContextSource, candidate int, candidateSource provider.ContextSource) (int, provider.ContextSource) {
	if candidate <= 0 {
		return current, currentSource
	}
	candidateRank := contextSourceRank(candidateSource)
	currentRank := contextSourceRank(currentSource)
	if candidateRank > currentRank || (candidateRank == currentRank && candidate > current) {
		return candidate, candidateSource
	}
	return current, currentSource
}

func contextSourceRank(source provider.ContextSource) int {
	switch source {
	case provider.ContextSourceAPI:
		return 4
	case provider.ContextSourceBuiltin, provider.ContextSourceCatalog:
		return 3
	case provider.ContextSourceInferred:
		return 1
	default:
		// Missing provenance is weaker than even an inferred model value. Old
		// credential files may contain a stale endpoint default, and it must not
		// replace reliable model-id inference.
		return 0
	}
}

func orString(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func orInt(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.input.Focus(), tea.EnterAltScreen}
	// Only capture the mouse when an interactive surface needs it; the
	// ordinary chat view must leave mouse tracking off so the terminal owns
	// drag selection and copy. m.mouseEnabled tracks the actual mode and is
	// synced here so the Update toggle knows the starting state.
	if m.wantsMouseCapture() {
		cmds = append(cmds, tea.EnableMouseCellMotion)
		m.mouseEnabled = true
	}
	// Some terminals (and piped/CI invocations) never deliver a
	// WindowSizeMsg. Without a fallback the UI would sit on "starting rick…"
	// forever, so seed a sane size that a real WindowSizeMsg overrides.
	cmds = append(cmds, func() tea.Msg { return ensureSizeMsg{} }, m.themePollCmd())
	if clipboardShortcutSupported() {
		cmds = append(cmds, clipboardShortcutTick())
	}
	if m.deps.ResumeID != "" {
		resumeID := m.deps.ResumeID
		initialMsg := m.deps.InitialMsg
		cmds = append(cmds, func() tea.Msg { return resumeMsg{id: resumeID, prompt: initialMsg} })
	} else if m.deps.InitialMsg != "" {
		msg := m.deps.InitialMsg
		cmds = append(cmds, func() tea.Msg { return submitMsg{text: msg} })
	}
	if m.deps.Store != nil && m.deps.ResumeID == "" {
		d := m.deps
		cmds = append(cmds, func() tea.Msg {
			return sessionTitleMsg{title: latestSessionTitle(d)}
		})
	}
	return tea.Batch(cmds...)
}

type resumeMsg struct {
	id     string
	prompt string
}
type submitMsg struct{ text string }
type ensureSizeMsg struct{}
type sessionTitleMsg struct{ title string }

// Update implements tea.Model and keeps terminal mouse capture scoped to
// interactive controls. The ordinary transcript/input view deliberately leaves
// mouse tracking disabled so the terminal owns drag selection and copy.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	model, cmd := m.update(msg)
	updated, ok := model.(*Model)
	if !ok {
		return model, cmd
	}

	wantsMouse := updated.wantsMouseCapture()
	if wantsMouse == updated.mouseEnabled {
		return updated, cmd
	}
	updated.mouseEnabled = wantsMouse
	if wantsMouse {
		return updated, tea.Batch(cmd, tea.EnableMouseCellMotion)
	}
	return updated, tea.Batch(cmd, tea.DisableMouse)
}

// wantsMouseCapture reports whether the terminal should hand mouse events to
// rick. The ordinary chat + input view deliberately returns false so the
// terminal owns drag selection and copy there — selecting and copying text
// works exactly like a normal terminal. Mouse capture is enabled only for
// interactive surfaces that genuinely need clicks (auth, web, permission
// prompt, choice menus, activity panel, resume browser). Setting
// tui.mouse: true overrides this and keeps full mouse capture everywhere.
func (m *Model) wantsMouseCapture() bool {
	if m.deps.Loaded != nil && m.deps.Loaded.TUI.Mouse {
		return true
	}
	if m.web.active || m.auth.active || m.resumeBrowser != nil {
		return true
	}
	if m.modal != modalNone {
		return true
	}
	if isChoiceMenu(m.pending.kind) {
		return true
	}
	if m.activityFocused {
		return true
	}
	return false
}

func (m *Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.resumeBrowser != nil {
		_, cmd := m.resumeBrowser.Update(msg)
		if m.resumeBrowser.quit {
			resumeID := m.resumeBrowser.resumeID
			m.resumeBrowser = nil
			m.input.Focus()
			if resumeID != "" {
				m.doResume(resumeID)
			}
			return m, nil
		}
		return m, cmd
	}

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		if msg.Width <= 0 || msg.Height <= 0 {
			return m, nil // ignore degenerate sizes from some terminals
		}
		return m, m.handleResize(msg)

	case tea.FocusMsg:
		m.focused = true
		return m, nil

	case tea.BlurMsg:
		m.focused = false
		return m, nil

	case clipboardShortcutTickMsg:
		down := clipboardShortcutDown()
		pressed := down && !m.clipboardShortcutWasDown
		m.clipboardShortcutWasDown = down
		if pressed && m.focused && time.Since(m.lastClipboardPaste) > 250*time.Millisecond {
			m.handleClipboardPaste()
		}
		return m, clipboardShortcutTick()

	case ensureSizeMsg:
		if !m.ready {
			w, h := terminalSize()
			return m, m.handleResize(tea.WindowSizeMsg{Width: w, Height: h})
		}
		return m, nil

	case tea.MouseMsg:
		if m.web.active {
			return m.handleWebMouse(msg)
		}
		if m.auth.active {
			if msg.Button == tea.MouseButtonWheelUp {
				m.authScroll(-m.scrollStep())
				return m, nil
			}
			if msg.Button == tea.MouseButtonWheelDown {
				m.authScroll(m.scrollStep())
				return m, nil
			}
			if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress {
				if button, ok := m.authButtonAt(msg.X, msg.Y); ok {
					return m.handleAuthButton(button)
				}
			}
			return m, nil
		}
		if m.modal != modalNone {
			return m, nil
		}
		// Shift+click/drag is reserved for the terminal's own selection:
		// Windows Terminal lets the user select text with Shift even while
		// the app captures the mouse, so rick must not consume those events.
		// This is what makes "select and copy like a normal terminal" work
		// while rick keeps its click features for unmodified clicks.
		if msg.Shift && msg.Button != tea.MouseButtonWheelUp && msg.Button != tea.MouseButtonWheelDown {
			return m, nil
		}
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			if m.activityContainsY(msg.Y) {
				m.activityFocused = true
				m.moveActivityCursor(-1)
			} else {
				m.scrollBy(-m.scrollStep())
			}
		case tea.MouseButtonWheelDown:
			if m.activityContainsY(msg.Y) {
				m.activityFocused = true
				m.moveActivityCursor(1)
			} else {
				m.scrollBy(m.scrollStep())
			}
		case tea.MouseButtonLeft:
			if msg.Action == tea.MouseActionPress {
				m.handleMouseClick(msg)
			}
		case tea.MouseButtonRight:
			// ignore gracefully
		}
		return m, nil

	case tea.KeyMsg:
		if m.auth.active {
			return m.handleAuthKey(msg, msg.String())
		}
		return m.handleKey(msg)

	case SubmitText:
		return m.submit(string(msg))

	case authProbeMsg:
		m.applyAuthProbe(msg)
		return m, nil

	case oauthStartMsg:
		return m, m.applyOAuthStart(msg)

	case oauthDoneMsg:
		return m.applyOAuthDone(msg)

	case themePollMsg:
		if m.themeWatch != nil && m.themeWatch.Changed() {
			m.deps.Themes = theme.Load(m.themeWatch.Dirs()...)
			if th := m.deps.Themes.Get(m.themeName); th != nil {
				m.styles = NewStyles(th)
				m.rebuildMarkdown(m.contentWidth())
				m.tx.invalidateAll(m.contentWidth())
				m.refresh()
				m.setStatus("theme reloaded: " + m.themeName)
			}
			m.themeIdle = 0
		} else if m.themeIdle < 8 {
			m.themeIdle++
		}
		return m, m.themePollCmd()

	case spinnerTickMsg:
		m.spinnerTick++
		if m.running || m.auth.busy || m.activeSwarms > 0 {
			for i, msg := range m.msgs {
				if msg.Kind == MsgTool && msg.ToolRunning {
					m.touch(i)
				}
				if msg.Kind == MsgSwarm && m.activeSwarms > 0 {
					m.touch(i)
				}
			}
			m.refresh()
			return m, m.spinnerCmd()
		}
		return m, nil

	case readAgentMsg:
		return m.drainAgent(msg.runID)

	case visionDoneMsg:
		if !m.visionPending || msg.runID != m.visionRunID {
			return m, nil
		}
		m.visionPending = false
		if msg.err != nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "vision: " + msg.err.Error(), Time: time.Now()})
			m.setStatus("vision failed")
			return m, nil
		}
		plural := "image"
		if msg.images != 1 {
			plural = "images"
		}
		m.setStatus(fmt.Sprintf("read %d %s via vision model", msg.images, plural))
		return m, m.startAgentWithMessage(msg.msg)

	case permAskMsg:
		m.permReq = msg.req
		m.permReply = msg.reply
		m.permCursor = 0
		m.modal = modalPermission
		return m, nil

	case todosChangedMsg:
		if m.ready {
			// The checklist lives above the input in the footer. Its height can
			// change without a terminal resize, so reserve the new rows before
			// rebuilding the transcript view.
			m.handleResize(tea.WindowSizeMsg{Width: m.width, Height: m.height})
		}
		m.refresh()
		return m, nil

	case statusMsg:
		m.setStatus(msg.text)
		if msg.quit {
			return m, tea.Quit
		}
		return m, nil

	case errMsg:
		m.appendMsg(ChatMsg{Kind: MsgError, Text: msg.err.Error(), Time: time.Now()})
		return m, nil

	case resumeMsg:
		m.doResume(msg.id)
		if msg.prompt != "" {
			return m.submit(msg.prompt)
		}
		return m, nil

	case sessionTitleMsg:
		m.resumable = msg.title
		return m, nil

	case submitMsg:
		return m.submit(msg.text)

	case shellDoneMsg:
		idx := msg.idx
		matched := msg.callID == ""
		if msg.callID != "" {
			mapped, ok := m.pendingTools[msg.callID]
			if ok {
				idx, matched = mapped, true
			}
			delete(m.pendingTools, msg.callID)
		}
		if !matched || idx < 0 || idx >= len(m.msgs) {
			m.refresh()
			return m, nil
		}
		out := msg.output
		isErr := msg.err != nil
		if isErr {
			out += "\n" + msg.err.Error()
		}
		m.msgs[idx].ToolRunning = false
		m.msgs[idx].ToolOutput = out
		m.msgs[idx].ToolErr = isErr
		// Feed the result into the conversation so the model can see it.
		m.history = append(m.history,
			provider.UserText(fmt.Sprintf("I ran this shell command:\n```\n%s\n```\nOutput:\n```\n%s\n```",
				m.msgs[idx].ToolTitle, truncate(out, 8000))))
		m.refresh()
		return m, nil

	case subagentEventMsg:
		m.applySubagentEvent(msg)
		return m, nil

	case subagentResultMsg:
		m.applySubagentResult(msg)
		return m, nil

	case childUsageMsg:
		m.billed.Input += msg.usage.InputTokens
		m.billed.Output += msg.usage.OutputTokens
		m.billed.CacheRead += msg.usage.CacheReadTokens
		m.billed.CacheWrite += msg.usage.CacheWriteTokens
		return m, nil

	case swarmStartMsg:
		plan, err := m.beginSwarm(msg)
		if err != nil {
			msg.reply <- swarmStartReply{err: err}
			return m, nil
		}
		m.resizeForActivity()
		msg.reply <- swarmStartReply{text: fmt.Sprintf("Team %q started with %d teammates.", msg.name, len(msg.agents))}
		return m, func() tea.Msg { m.runSwarmPlan(plan); return nil }

	case swarmWorkerMsg:
		m.applySwarmWorker(msg)
		return m, nil

	case swarmCompleteMsg:
		m.applySwarmComplete(msg)
		return m, nil

	case compactDoneMsg:
		if !m.compactionActive || msg.runID != m.compactionRunID {
			return m, nil
		}
		m.compactionActive = false
		if m.compactionCancel != nil {
			m.compactionCancel()
			m.compactionCancel = nil
		}
		m.billed.Input += msg.usage.InputTokens
		m.billed.Output += msg.usage.OutputTokens
		m.billed.CacheRead += msg.usage.CacheReadTokens
		m.billed.CacheWrite += msg.usage.CacheWriteTokens
		if m.deps.Usage != nil && msg.modelID != "" {
			_ = m.deps.Usage.Record(msg.modelID, msg.usage.InputTokens, msg.usage.OutputTokens,
				msg.usage.CacheReadTokens, msg.usage.CacheWriteTokens)
		}
		if msg.err != nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "compact: " + msg.err.Error(), Time: time.Now()})
			return m, nil
		}
		summary := strings.TrimSpace(msg.summary)
		if summary == "" {
			m.setStatus("compact produced no summary")
			return m, nil
		}
		// Keep the stable cached prefix intact: insert the summary right
		// after the last cache boundary, dropping only the volatile head, so
		// the bytes the provider still has cached stay byte-identical.
		removed := len(m.history) - len(msg.tail)
		if removed < 0 {
			removed = 0
		}
		insert := 0
		if m.deps.Budget != nil {
			insert = lastBoundaryBefore(m.history, removed, m.deps.Budget.ChooseBoundaries(m.history))
		}
		// Reuse the previous summary pair's position so repeated compactions
		// keep the summary at the same bytes; a moving summary would rewrite
		// the canonical prefix and invalidate the provider prefix cache.
		if existing := summaryPairAt(m.history); existing >= 0 && existing < removed {
			insert = existing
		}
		newHistory := append([]provider.Message{}, m.history[:insert]...)
		newHistory = append(newHistory,
			provider.UserText("Summary of the conversation so far:\n\n"+summary),
			provider.AssistantText("Understood. Continuing from that state."),
		)
		newHistory = append(newHistory, msg.tail...)
		// Anti-thrash effectiveness: a compaction that shrank the history by
		// less than 10% is ineffective — the session can't be meaningfully
		// compressed. Two strikes in a row stop automatic compaction so it
		// stops burning aux tokens on dead-end folds.
		beforeBytes := historyByteSize(m.history)
		afterBytes := historyByteSize(newHistory)
		if beforeBytes > 0 && beforeBytes-afterBytes < beforeBytes/10 {
			m.compactIneffectiveStrikes++
			if m.compactIneffectiveStrikes == 2 {
				m.appendMsg(ChatMsg{Kind: MsgSystem,
					Text: "automatic compaction paused: recent compactions saved <10% of context — run /compact manually if the session is still too large",
					Time: time.Now()})
			}
		} else {
			m.compactIneffectiveStrikes = 0
		}
		m.history = newHistory
		m.msgs = append([]ChatMsg{{Kind: MsgSystem,
			Text: "context compacted\n\n" + summary, Time: time.Now()}},
			messagesToChat(msg.tail)...)
		m.tx.invalidateAll(m.contentWidth())
		// Occupancy only: compaction shrinks the context but the tokens
		// already spent this session were still spent.
		m.usage = session.Usage{}
		m.refresh()
		m.setStatus("context compacted")
		if err := m.saveSession(); err != nil {
			m.reportSessionSaveError(err)
		}
		return m, nil

	case refreshDoneMsg:
		m.applyRefreshDone()
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *Model) handleResize(msg tea.WindowSizeMsg) tea.Cmd {
	widthChanged := msg.Width != m.width
	m.width, m.height = msg.Width, msg.Height

	inputH := m.inputHeight()
	vpH := m.height - inputH - 4 - m.todoPanelHeight() - m.activityPanelHeight() // header + status + padding
	if vpH < 3 {
		vpH = 3
	}

	if !m.ready {
		m.viewport = viewport.New(m.width, vpH)
		m.viewport.YPosition = 0
		m.ready = true
		m.input.SetWidth(m.width - 4)
		m.rebuildMarkdown(m.contentWidth())
		m.seedWelcome()
		return nil
	}

	// Remember where the user was proportionally: after re-wrapping, the
	// absolute line offset is meaningless but the fraction still is.
	frac := relativePos(&m.viewport)
	following := m.tx.following()

	m.viewport.Width = m.width
	m.viewport.Height = vpH
	m.input.SetWidth(m.width - 4)

	if widthChanged {
		// Wrapping changed, so every cached block is stale.
		m.rebuildMarkdown(m.contentWidth())
		m.tx.invalidateAll(m.contentWidth())
	}
	m.refresh()

	if !following {
		restorePos(&m.viewport, frac)
	}
	return nil
}

func (m *Model) contentWidth() int {
	w := m.width - 2
	if w < 1 {
		w = 1
	}
	if w > 160 {
		w = 160
	}
	return w
}

func (m *Model) inputVisualLines() int {
	width := m.width - 4
	if width < 1 {
		width = 1
	}

	lines := 0
	for _, logicalLine := range strings.Split(m.input.Value(), "\n") {
		lines += wrappedInputLineCount([]rune(logicalLine), width)
	}
	if lines < 1 {
		return 1
	}
	if lines > 8 {
		return 8
	}
	return lines
}

// wrappedInputLineCount mirrors bubbles/textarea's word-wrap behavior so the
// outer layout reserves the same number of rows that the textarea renders.
func wrappedInputLineCount(runes []rune, width int) int {
	lines := [][]rune{{}}
	word := []rune{}
	row := 0
	spaces := 0

	for _, r := range runes {
		if unicode.IsSpace(r) {
			spaces++
		} else {
			word = append(word, r)
		}
		if spaces > 0 {
			if uniseg.StringWidth(string(lines[row]))+uniseg.StringWidth(string(word))+spaces > width {
				row++
				lines = append(lines, append(append([]rune{}, word...), []rune(strings.Repeat(" ", spaces))...))
			} else {
				lines[row] = append(lines[row], word...)
				lines[row] = append(lines[row], []rune(strings.Repeat(" ", spaces))...)
			}
			spaces = 0
			word = nil
		} else if len(word) > 0 {
			lastCharWidth := uniseg.StringWidth(string(word[len(word)-1]))
			if uniseg.StringWidth(string(word))+lastCharWidth > width {
				if len(lines[row]) > 0 {
					row++
					lines = append(lines, []rune{})
				}
				lines[row] = append(lines[row], word...)
				word = nil
			}
		}
	}

	if uniseg.StringWidth(string(lines[row]))+uniseg.StringWidth(string(word))+spaces >= width {
		row++
	}
	return row + 1
}

func (m *Model) inputHeight() int {
	lines := m.inputVisualLines()
	if lines < 1 {
		lines = 1
	}
	if lines > 8 {
		lines = 8
	}
	h := lines + 2 // border
	if m.picker.active {
		h += m.picker.height() + 1
	} else if strings.HasPrefix(m.input.Value(), "/") {
		h += m.autocompleteHeight() + 1
	}
	return h
}

// todoPanelHeight returns the checklist rows plus the separator appended by
// footer. The panel is omitted while a swarm owns the footer.
func (m *Model) todoPanelHeight() int {
	if m.deps.Todos == nil || m.activeSwarms != 0 {
		return 0
	}
	items := m.deps.Todos.Items()
	if len(items) == 0 {
		return 0
	}
	panel := m.renderTodos(items, m.contentWidth())
	if panel == "" {
		return 0
	}
	return strings.Count(panel, "\n") + 2
}

func (m *Model) rebuildMarkdown(width int) {
	if width < 1 {
		width = 1
	}
	style := "dark"
	if m.themeName == "light" {
		style = "light"
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(style),
		glamour.WithWordWrap(width),
		glamour.WithEmoji(),
	)
	if err == nil {
		m.mdRenderer = r
	}
}

// themePollCmd re-checks the theme directories once a second so edits to a
// theme file show up without a restart.
func (m *Model) themePollCmd() tea.Cmd {
	// Hot-reload matters only while someone is editing a theme file, so back
	// off from 1s to 8s once nothing has changed for a while. An edit is then
	// picked up within 8s and the idle UI stops touching the disk every
	// second for the entire session.
	d := time.Duration(1+m.themeIdle) * time.Second
	return tea.Tick(d, func(t time.Time) tea.Msg { return themePollMsg(t) })
}

func (m *Model) spinnerCmd() tea.Cmd {
	return tea.Tick(90*time.Millisecond, func(t time.Time) tea.Msg { return spinnerTickMsg(t) })
}

// trimHeight clips a block to at most n rows.
func trimHeight(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

// padHeight pads a block with blank lines so it occupies exactly n rows,
// trimming it if it is already taller.
func padHeight(s string, n int) string {
	if n <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		return strings.Join(lines[:n], "\n")
	}
	return s + strings.Repeat("\n", n-len(lines))
}

func (m *Model) setStatus(s string) {
	m.status = s
	m.statusTime = time.Now()
}

const maxTranscriptMessages = 500

func (m *Model) trimTranscript() {
	if len(m.msgs) <= maxTranscriptMessages {
		return
	}
	// Omit enough older messages that the replacement (system note + tail)
	// lands at or under the cap. Trimming only down to 501 would leave the
	// transcript permanently over budget: every refresh — the 25 Hz stream
	// drain and the 90 ms spinner — would re-enter trimTranscript and
	// invalidate the whole render cache, forcing a full re-render of every
	// block per frame as the session grows.
	remove := len(m.msgs) - maxTranscriptMessages + 1
	m.msgs = append([]ChatMsg{{Kind: MsgSystem, Text: fmt.Sprintf("... %d earlier messages omitted to fit the transcript", remove), Time: time.Now()}}, m.msgs[remove:]...)
	m.pendingTools = make(map[string]int)
	for i, msg := range m.msgs {
		if msg.Kind == MsgTool && msg.CallID != "" && msg.ToolRunning {
			m.pendingTools[msg.CallID] = i
		}
	}
	kept := make(map[string]struct{}, len(m.msgs))
	for _, msg := range m.msgs {
		if msg.CallID != "" {
			kept[msg.CallID] = struct{}{}
		}
	}
	for callID := range m.toolOutputs {
		if _, ok := kept[callID]; !ok {
			delete(m.toolOutputs, callID)
		}
	}
	m.tx.invalidateAll(m.contentWidth())
}

func (m *Model) appendMsg(msg ChatMsg) {
	m.msgs = append(m.msgs, msg)
	m.tx.noteAppend()
	m.refresh()
}

// refresh re-renders only what changed and re-applies the scroll policy.
//
// Entries are cached by the transcript, so a streaming chunk repaints one
// line instead of the whole history — that full re-layout was the cause of
// the flicker and the scroll snapping.
func (m *Model) refresh() {
	m.trimTranscript()
	if !m.ready {
		return
	}
	w := m.contentWidth()

	// The streaming tail is volatile; keep it outside the cache.
	var live strings.Builder
	if m.thinkBuf.Len() > 0 && m.showThinking {
		live.WriteString(m.styles.Thinking.Render(wrapIndent(m.thinkBuf.String(), w-2, "  ")))
	}
	if m.streamBuf.Len() > 0 {
		if live.Len() > 0 {
			live.WriteString("\n")
		}
		live.WriteString(wrapIndent(m.streamBuf.String(), w, ""))
	}
	// While a choice menu is pending, re-render its option block at the tail
	// so streaming after it cannot push the menu out of the viewport. The
	// settled MsgChoice message stays in history for scrollback; the live
	// tail re-pins the interactive menu to the bottom.
	if isChoiceMenu(m.pending.kind) && !m.pending.textInput {
		if blk := m.renderPendingMenu(w); blk != "" {
			if live.Len() > 0 {
				live.WriteString("\n")
			}
			live.WriteString(blk)
		}
	}
	m.tx.live = live.String()

	m.tx.render(len(m.msgs), w, func(i int) string {
		m.renderCount++
		return m.renderMsg(m.msgs[i], w)
	})
	m.chatContent = m.tx.content
	m.tx.apply(&m.viewport)
	m.rebuildToolRowMap()
	m.rebuildChoiceButtonMap()
}

// touch marks one entry as needing a re-render.
func (m *Model) touch(i int) { m.tx.invalidate(i) }

// rebuildToolRowMap scans the rendered content and records which rows belong
// to tool blocks, so mouse clicks can toggle expand/collapse.
func (m *Model) rebuildToolRowMap() {
	m.toolRowMap = m.toolRowMap[:0]
	row := 0
	for i := range m.msgs {
		msg := &m.msgs[i]
		if msg.Kind != MsgTool || msg.CallID == "" {
			// Skip non-tool blocks: count their lines to keep row in sync.
			if i < len(m.tx.blocks) && m.tx.blocks[i] != "" {
				row += strings.Count(m.tx.blocks[i], "\n") + 1
				row++ // inter-block separator
			}
			continue
		}
		block := ""
		if i < len(m.tx.blocks) {
			block = m.tx.blocks[i]
		}
		if block == "" {
			continue
		}
		nLines := strings.Count(block, "\n") + 1
		m.toolRowMap = append(m.toolRowMap, toolRowEntry{
			callID:   msg.CallID,
			startRow: row,
			endRow:   row + nLines - 1,
		})
		row += nLines
		row++ // inter-block separator
	}
}

func (m *Model) rebuildChoiceButtonMap() {
	m.choiceButtons = m.choiceButtons[:0]
	if !isChoiceMenu(m.pending.kind) || m.pending.textInput {
		return
	}
	// The pending menu is pinned as the live tail, so its button row is the
	// last rendered line of the content.
	backWidth, selectWidth := m.choiceButtonWidths()
	lines := strings.Split(strings.TrimRight(m.chatContent, "\n"), "\n")
	buttonY := len(lines) - 1
	if buttonY < 0 {
		return
	}
	backX := 2
	m.choiceButtons = append(m.choiceButtons,
		choiceButtonZone{id: choiceButtonBack, x: backX, y: buttonY, width: backWidth},
		choiceButtonZone{id: choiceButtonSelect, x: backX + backWidth + 1, y: buttonY, width: selectWidth},
	)
}

// handleMouseClick processes a left-button press in the transcript area.
func (m *Model) handleMouseClick(msg tea.MouseMsg) {
	if item, ok := m.activityAt(msg.Y); ok {
		items := m.activityItems()
		for index := range items {
			if items[index].id == item.id {
				m.activityCursor = index
				break
			}
		}
		m.activityFocused = true
		now := time.Now()
		if msg.Y == m.lastClickY && now.Sub(m.lastClickTime) < 400*time.Millisecond {
			m.lastClickTime = time.Time{}
			_, _ = m.openActivity(item)
			m.refresh()
			return
		}
		m.lastClickTime = now
		m.lastClickY = msg.Y
		m.refresh()
		return
	}

	// Ignore clicks outside the viewport (status bar, input area).
	if msg.Y < m.viewport.YPosition || msg.Y >= m.viewport.YPosition+m.viewport.Height {
		return
	}

	contentRow := m.viewport.YOffset + (msg.Y - m.viewport.YPosition)
	if button, ok := m.choiceButtonAt(msg.X, contentRow); ok {
		switch button.id {
		case choiceButtonBack:
			_, _ = m.backPendingChoice()
		case choiceButtonSelect:
			_, _ = m.applyPendingCursor()
		}
		m.refresh()
		return
	}

	// Double-click detection: same row within 400ms copies a file path.
	now := time.Now()
	if msg.Y == m.lastClickY && now.Sub(m.lastClickTime) < 400*time.Millisecond {
		m.lastClickTime = time.Time{}
		m.handleDoubleClick(msg)
		return
	}
	m.lastClickTime = now
	m.lastClickY = msg.Y

	// Find the tool block at this row and toggle it.
	for _, entry := range m.toolRowMap {
		if contentRow >= entry.startRow && contentRow <= entry.endRow {
			m.expandedTools[entry.callID] = !m.expandedTools[entry.callID]
			// Invalidate the tool's cached render so it re-renders expanded.
			for i := range m.msgs {
				if m.msgs[i].CallID == entry.callID {
					m.touch(i)
					break
				}
			}
			m.refresh()
			return
		}
	}
}

func (m *Model) touchPendingChoice() {
	for i, msg := range m.msgs {
		if msg.choiceID == m.pending.choiceID {
			m.touch(i)
			return
		}
	}
}

func (m *Model) choiceButtonAt(x, contentRow int) (choiceButtonZone, bool) {
	if !isChoiceMenu(m.pending.kind) || m.pending.textInput || contentRow < 0 {
		return choiceButtonZone{}, false
	}
	for _, button := range m.choiceButtons {
		if button.y == contentRow && x >= button.x && x < button.x+button.width {
			return button, true
		}
	}
	return choiceButtonZone{}, false
}

func (m *Model) authButtonZones() []authButtonZone {
	if !m.auth.active {
		return nil
	}
	panel := m.authView()
	panelWidth := lipgloss.Width(panel)
	panelLeft := (m.width - panelWidth) / 2
	panelTop := (m.height - lipgloss.Height(panel)) / 2
	buttonRow := -1
	panelLine := ""
	for row, line := range strings.Split(panel, "\n") {
		if strings.Contains(line, "← Back") && strings.Contains(line, m.authPrimaryLabel()) {
			buttonRow = row
			panelLine = line
			break
		}
	}
	if buttonRow < 0 {
		return nil
	}

	backLabel := "← Back"
	primaryLabel := m.authPrimaryLabel()
	backLabelIndex := strings.Index(panelLine, backLabel)
	primaryLabelIndex := strings.Index(panelLine, primaryLabel)
	if backLabelIndex < 0 || primaryLabelIndex < 0 {
		return nil
	}
	backRendered := m.choiceButtonStyle(false).Render(backLabel)
	primaryRendered := m.choiceButtonStyle(true).Render(primaryLabel)
	backWidth := lipgloss.Width(backRendered)
	primaryWidth := lipgloss.Width(primaryRendered)
	backPadding := (backWidth - lipgloss.Width(backLabel)) / 2
	primaryPadding := (primaryWidth - lipgloss.Width(primaryLabel)) / 2
	backX := panelLeft + lipgloss.Width(panelLine[:backLabelIndex]) - backPadding
	primaryX := panelLeft + lipgloss.Width(panelLine[:primaryLabelIndex]) - primaryPadding
	return []authButtonZone{
		{id: authButtonBack, x: backX, y: panelTop + buttonRow, width: backWidth},
		{id: authButtonPrimary, x: primaryX, y: panelTop + buttonRow, width: primaryWidth},
	}
}

func (m *Model) authButtonAt(x, y int) (authButtonZone, bool) {
	for _, zone := range m.authButtonZones() {
		if x >= zone.x && x < zone.x+zone.width && y == zone.y {
			return zone, true
		}
	}
	return authButtonZone{}, false
}

func (m *Model) handleAuthButton(zone authButtonZone) (tea.Model, tea.Cmd) {
	if zone.id == authButtonBack || m.auth.stage == authProbing || m.auth.stage == authOAuthWaiting {
		return m.authBack()
	}
	return m.handleAuthKey(tea.KeyMsg{Type: tea.KeyEnter}, "enter")
}

// handleDoubleClick copies a file path from the clicked line to clipboard.
func (m *Model) handleDoubleClick(msg tea.MouseMsg) {
	contentRow := m.viewport.YOffset + (msg.Y - m.viewport.YPosition)
	lines := strings.Split(m.chatContent, "\n")
	if contentRow < 0 || contentRow >= len(lines) {
		return
	}
	line := lines[contentRow]
	if match := linkRe.FindString(line); match != "" {
		// Prefer the native Windows clipboard (works everywhere); fall back
		// to OSC52 for terminals that only support the escape sequence.
		if err := writeClipboardText(match); err != nil {
			copyToClipboardOSC52(match)
		}
		m.setStatus("copied: " + match)
	}
}

// seedWelcome primes the transcript. The splash itself is rendered by View
// while the conversation is empty, so nothing is written into msgs — that way
// it disappears the moment real content arrives, without a special case.
func (m *Model) seedWelcome() { m.refresh() }

// View implements tea.Model.
func (m *Model) View() string {
	if m.resumeBrowser != nil {
		return m.resumeBrowser.View()
	}
	if !m.ready {
		return "starting rick…"
	}
	if m.quitting {
		return ""
	}

	var main string
	if len(m.msgs) == 0 && m.streamBuf.Len() == 0 {
		// Pre-conversation: banner instead of an empty box. It must still
		// fill the viewport's height — a short frame leaves the previous,
		// taller frame's lines on screen in alt-screen mode, which is what
		// made /new look like it did nothing.
		main = padHeight(m.splash(), m.viewport.Height)
	} else {
		main = m.viewport.View()
	}
	body := main + "\n" + m.footer()

	if m.web.active {
		return m.overlay(body, m.webView())
	}
	if m.auth.active {
		return m.overlay(body, m.authView())
	}

	switch m.modal {
	case modalPermission:
		return m.overlay(body, m.permissionView())
	}

	// Pad to exactly m.height lines so alt-screen mode doesn't leave
	// ghost content from a previous taller frame.
	body = padHeight(body, m.height)
	return body
}

func (m *Model) overlay(body, panel string) string {
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		panel, lipgloss.WithWhitespaceChars(" "))
}

func shortModel(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	id = strings.TrimPrefix(id, "claude-")
	// strip a trailing date stamp
	parts := strings.Split(id, "-")
	if len(parts) > 1 {
		last := parts[len(parts)-1]
		if len(last) == 8 && isDigits(last) {
			id = strings.Join(parts[:len(parts)-1], "-")
		}
	}
	return id
}

// displayModel returns the model label shown in the UI. A stale model
// reference must not look like an active selection after its provider is gone.
func (m *Model) displayModel() string {
	providerID, modelID := config.SplitModel(strings.TrimSpace(m.modelID))
	if modelID == "" {
		return "None selected"
	}
	if providerID != "" {
		if _, ok := m.deps.Providers[providerID]; !ok {
			return "None selected"
		}
	}
	return shortModel(m.modelID)
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

func (m *Model) footer() string {
	var b strings.Builder
	b.WriteString(m.activityPrefix())

	if m.picker.active {
		b.WriteString(m.pickerView() + "\n")
	} else if strings.HasPrefix(m.input.Value(), "/") {
		if ac := m.autocompleteView(); ac != "" {
			b.WriteString(ac + "\n")
		}
	}

	if sb := m.statusBar(); sb != "" {
		b.WriteString(sb)
	}

	// Show active jobs and swarms
	if jobs := m.jobs.Render(m.contentWidth(), m.styles); jobs != "" {
		b.WriteString(jobs)
	}
	if m.swarmPanel != "" {
		b.WriteString(m.swarmPanel)
	}

	return b.String()
}

func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		s := fmt.Sprintf("%.1fM", float64(n)/1e6)
		return strings.Replace(s, ".0M", "M", 1) // 1.0M reads worse than 1M
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// ---------- exported test helpers ----------

// InputSetValue sets the input text (test helper).
func (m *Model) InputSetValue(s string) { m.input.SetValue(s) }

// InputValue returns the input text (test helper).
func (m *Model) InputValue() string { return m.input.Value() }

// ChatContent returns the rendered transcript buffer (test helper).
func (m *Model) ChatContent() string { return m.chatContent }

// Msgs returns the chat entries (test helper).
func (m *Model) Msgs() []ChatMsg { return m.msgs }

// ThemeName returns the active theme name (test helper).
func (m *Model) ThemeName() string { return m.themeName }

// AgentName returns the active agent (test helper).
func (m *Model) AgentName() string { return m.agentName }

// RunSlash dispatches a slash command and returns the resulting transcript
// text (test helper).
func (m *Model) RunSlash(text string) string {
	m.runSlash(text)
	if len(m.msgs) == 0 {
		return ""
	}
	return m.msgs[len(m.msgs)-1].Text
}

// ModelID returns the active model id (test helper).
func (m *Model) ModelID() string { return m.modelID }

// ModalOpen reports whether an overlay is showing (test helper).
func (m *Model) ModalOpen() bool { return m.modal != modalNone }

// PickerActive reports whether the @ picker is open (test helper).
func (m *Model) PickerActive() bool { return m.picker.active }

// PickerResults returns filtered picker entries (test helper).
func (m *Model) PickerResults() int { return len(m.picker.results) }

// RenderAutocomplete exposes the slash autocomplete (test helper).
func (m *Model) RenderAutocomplete() string { return m.autocompleteView() }

// Cwd returns the working directory (test helper).
func (m *Model) Cwd() string { return m.deps.Cwd }

// StatusLine returns the status bar (test helper).
func (m *Model) StatusLine() string { return m.statusBar() }

// AuthActive reports whether the /auth flow is open (test helper).
func (m *Model) AuthActive() bool { return m.auth.active }

// AuthStageName names the current /auth stage (test helper).
func (m *Model) AuthStageName() string {
	switch m.auth.stage {
	case authList:
		return "list"
	case authEnterKey:
		return "key"
	case authEditMenu:
		return "edit"
	case authAddName:
		return "add-name"
	case authAddURL:
		return "add-url"
	case authAddKey:
		return "add-key"
	case authProbing:
		return "probing"
	case authPickModel:
		return "pick-model"
	case authEnterModel:
		return "enter-model"
	case authDeviceCode:
		return "device-code"
	}
	return "unknown"
}

// AuthRowCount is the number of providers listed (test helper).
func (m *Model) AuthRowCount() int { return len(m.auth.rows) }

// AuthModelCount is the number of models offered (test helper).
func (m *Model) AuthModelCount() int { return len(m.auth.models) }

// Following reports whether the viewport tracks the tail (test helper).
func (m *Model) Following() bool { return m.tx.following() }

// Pending is the unseen-entry count (test helper).
func (m *Model) Pending() int { return m.tx.pending() }

// ScrollOffset is the viewport's line offset (test helper).
func (m *Model) ScrollOffset() int { return m.viewport.YOffset }

// ScrollFraction is the scroll position as 0..1 (test helper).
func (m *Model) ScrollFraction() float64 { return relativePos(&m.viewport) }

// RenderCount is the cumulative number of entry renders (test helper). It
// stays flat while streaming if the render cache is working.
func (m *Model) RenderCount() int { return m.renderCount }

// StreamLen is the streaming buffer size (test helper).
func (m *Model) StreamLen() int { return m.streamBuf.Len() }

// Ready reports whether the viewport is initialised (test helper).
func (m *Model) Ready() bool { return m.ready }

// ForceRefresh re-renders (test helper).
func (m *Model) ForceRefresh() { m.refresh() }

// HistoryLen is the provider-message count (test helper).
func (m *Model) HistoryLen() int { return len(m.history) }

// SetUsage seeds token counters (test helper).
func (m *Model) SetUsage(in, out int) {
	m.usage.Input, m.usage.Output = in, out
}

// StatusBar renders just the status line (test helper).
func (m *Model) StatusBar() string { return m.StatusLine() }

// resetStats clears every per-session counter shown in the status bar.
//
// These are session-scoped, not process-scoped: leaving them behind made a
// brand-new conversation report the previous one's tokens and turn time.
func (m *Model) resetStats() {
	m.lastRunError = ""
	m.usage = session.Usage{}
	m.optimization = session.OptimizationUsage{}
	m.billed = session.Usage{}
	m.turnStart = time.Time{}
	m.turnElapsed = 0
	m.cachePrevPrompt = 0
	m.cacheMissTokens = 0
	m.cacheMissCount = 0
	m.cacheMissStreak = 0
	m.cacheLastUsage = time.Time{}
	m.cacheLastMissReason = ""
}

// SetTurnElapsed fakes a completed turn duration (test helper).
func (m *Model) SetTurnElapsed(d time.Duration) {
	m.turnStart = time.Now().Add(-d)
	m.turnElapsed = d
}

// CompactUsage clears occupancy as /compact does (test helper).
func (m *Model) CompactUsage() { m.usage = session.Usage{} }

// Refresh rebuilds the transcript (test helper).
func (m *Model) Refresh() { m.refresh() }

// HumanTokens formats a token count (test helper).
func HumanTokens(n int) string { return humanTokens(n) }

// SetModelID switches model and re-detects its capabilities (test helper).
func (m *Model) SetModelID(id string) { m.setModel(id) }

// setModel switches the active model, re-detects its context window and
// reasoning support, and remembers the choice for the next launch.
//
// Every model change goes through here. Doing it inline at each call site is
// how the previous bug crept in: five places set modelID and each had to
// remember to call updateContextWindow, so a sixth would silently skip it.
func (m *Model) setModel(id string) {
	if id == "" || id == m.modelID {
		return
	}
	m.modelID = id
	m.updateContextWindow()
	// A model switch re-bills the whole prompt; do not count the next turn's
	// small cache-read as a miss against the old model's footprint.
	m.cachePrevPrompt = 0
	m.cacheMissStreak = 0
	// The switch itself always succeeds; only remembering it can fail, so
	// say so in the status line rather than silently forgetting — matching
	// how /theme reports a failed save.
	if err := config.SaveModelChoice(id); err != nil {
		m.setStatus("model: " + shortModel(id) + " (not saved: " + err.Error() + ")")
	}
}

// ContextWindow is the active model's window (test helper).
func (m *Model) ContextWindow() int { return m.ctxWindow }

// ContextPct is the gauge percentage (test helper).
func (m *Model) ContextPct() int { return m.contextPct() }

// UsageInput is the current context occupancy (test helper).
func (m *Model) UsageInput() int { return m.usage.Input }

// BilledTotal is the cumulative billed token count (test helper).
func (m *Model) BilledTotal() int { return m.billed.Input + m.billed.Output }

// Reasoning is the active effort level (test helper).
func (m *Model) Reasoning() string { return string(m.reasoning) }

// ApplyUsage feeds a usage event, as the agent would (test helper).
func (m *Model) ApplyUsage(input, output, cache int) {
	m.usage.Input = input
	m.usage.Output = output
	m.usage.CacheRead = cache
	m.billed.Input += input
	m.billed.Output += output
	m.billed.CacheRead += cache
}

// SubmitText is a message that submits text, as if typed (test helper).
type SubmitText string

// PendingKind is the armed inline selection type, 0 when none (test helper).
func (m *Model) PendingKind() int { return int(m.pending.kind) }

// PendingCount is the number of armed options (test helper).
func (m *Model) PendingCount() int { return len(m.pending.options) }

// MsgCount is the transcript entry count (test helper).
func (m *Model) MsgCount() int { return len(m.msgs) }

// CacheLen is the render cache size (test helper).
func (m *Model) CacheLen() int { return len(m.tx.blocks) }

// ForceRunning flips the running flag (test helper).
func (m *Model) ForceRunning(v bool) { m.running = v }

// Drain runs one agent-drain cycle (test helper).
func (m *Model) Drain() *Model {
	nm, _ := m.drainAgent(m.agentRunID)
	return nm.(*Model)
}

// PushSystem appends a system line (test helper).
func (m *Model) PushSystem(text string) {
	m.appendMsg(ChatMsg{Kind: MsgSystem, Text: text, Time: time.Now()})
}

// PushStreamChunk simulates one streamed token (test helper).
func (m *Model) PushStreamChunk(s string) {
	m.streamBuf.WriteString(s)
	m.refresh()
}

// AuthInputBuf is the /auth prompt buffer (test helper).
func (m *Model) AuthInputBuf() string { return m.auth.inputBuf }

// AuthClearInput empties the /auth prompt buffer (test helper).
func (m *Model) AuthClearInput() { m.auth.inputBuf = "" }

// AuthDraftURL is the normalised URL under construction (test helper).
func (m *Model) AuthDraftURL() string { return m.auth.draftURL }

// AuthStatus is the flow's status line, stripped of styling (test helper).
func (m *Model) AuthStatus() string { return stripANSI(m.auth.statusLine) }

// AuthReset returns the flow to a fresh provider list (test helper).
func (m *Model) AuthReset() {
	m.auth = authState{active: true, stage: authList}
	m.rebuildAuthRows()
}

// ProviderCount is the number of live providers (test helper).
func (m *Model) ProviderCount() int { return len(m.deps.Providers) }

// stripANSI removes escape sequences so tests can assert on plain text.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// terminalSize resolves a usable size when the terminal never reports one.
func terminalSize() (int, int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err == nil && w > 0 && h > 0 {
		return w, h
	}
	w, h = 100, 30
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			w = n
		}
	}
	if v := os.Getenv("LINES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			h = n
		}
	}
	return w, h
}
