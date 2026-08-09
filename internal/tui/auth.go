package tui

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"rick/internal/config"
	"rick/internal/provider"
	"rick/internal/provider/anthropic"
	"rick/internal/provider/catalog"
	"rick/internal/provider/openai"
)

// authStage is the position in the /auth state machine.
type authStage int

const (
	authList         authStage = iota // the provider list, awaiting a number / "add" / id
	authEnterKey                      // pasting an API key
	authEditMenu                      // chose a connected provider: what to change?
	authAddName                       // custom: provider name
	authAddURL                        // custom: base URL
	authAddKey                        // custom: API key
	authProbing                       // contacting the endpoint
	authPickModel                     // choose the default model
	authEnterModel                    // type a model id (endpoint has no list)
	authDeviceCode                    // OAuth device-code instructions (fallback)
	authOAuthWaiting                  // OAuth device-code flow in progress
	authKeyMenu                       // multi-key management menu
	authKeyAdd                        // pasting new keys
	authKeyMode                       // choosing rotation mode
)

// authState holds everything the /auth flow needs.
type authState struct {
	active bool
	stage  authStage

	rows   []authRow // what the list currently displays
	cursor int
	scroll int // first visible row in the provider list

	// entry being configured
	returnTo authStage // stage to fall back to when a probe fails
	target   catalog.Entry
	custom   bool
	inputBuf string
	draftID  string
	draftURL string
	draftKey string

	// probe results
	models        []catalog.Model
	probeErr      error
	statusLine    string
	busy          bool
	confirmRemove bool
	probeCancel   context.CancelFunc
	probeGen      int // generation counter to ignore stale probe results

	// OAuth device-code flow state
	oauthCancel   context.CancelFunc
	oauthUserCode string
	oauthVerifURI string
	oauthGen      int // generation counter to ignore stale async results
}

// authRow is one line in the provider list.
type authRow struct {
	id        string
	label     string
	detail    string
	connected bool
	envOnly   bool // credential came from the environment, not our store
	custom    bool
}

type authButtonZone struct {
	id    string
	x     int
	y     int
	width int
}

const (
	authButtonBack    = "auth-back"
	authButtonPrimary = "auth-primary"
)

// openAuth enters the /auth flow.
func (m *Model) openAuth() (tea.Model, tea.Cmd) {
	m.auth = authState{active: true, stage: authList}
	m.rebuildAuthRows()
	m.refresh()
	return m, nil
}

// rebuildAuthRows recomputes the list: configured providers first (green
// check), then the rest of the catalog.
func (m *Model) rebuildAuthRows() {
	creds := m.creds
	var connected, available []authRow

	seen := map[string]bool{}
	add := func(r authRow) {
		if seen[r.id] {
			return
		}
		seen[r.id] = true
		if r.connected {
			connected = append(connected, r)
		} else {
			available = append(available, r)
		}
	}

	// Saved credentials (including custom providers).
	for _, id := range creds.IDs() {
		cred, _ := creds.Get(id)
		label := cred.Label
		if label == "" {
			if e, ok := catalog.Get(id); ok {
				label = e.Name
			} else {
				label = id
			}
		}
		detail := hostOf(cred.BaseURL)
		if n := len(cred.Models); n > 0 {
			detail += fmt.Sprintf(" · %d models", n)
		}
		add(authRow{id: id, label: label, detail: detail, connected: true, custom: cred.Custom})
	}

	// Catalog entries, marking any satisfied purely by an environment variable.
	for _, e := range catalog.Registry {
		if seen[e.ID] {
			continue
		}
		if key, envName := e.EnvKey(); key != "" {
			add(authRow{id: e.ID, label: e.Name, connected: true, envOnly: true,
				detail: "from $" + envName})
			continue
		}
		detail := e.Note
		if detail == "" {
			detail = hostOf(e.BaseURL)
		}
		if e.Auth == catalog.AuthDeviceCode {
			detail = "browser sign-in · " + detail
		}
		add(authRow{id: e.ID, label: e.Name, detail: detail})
	}

	sort.SliceStable(connected, func(i, j int) bool { return connected[i].label < connected[j].label })
	m.auth.rows = append(connected, available...)
	if m.auth.cursor >= len(m.auth.rows) {
		m.auth.cursor = len(m.auth.rows) - 1
	}
	if m.auth.cursor < 0 {
		m.auth.cursor = 0
	}
}

func hostOf(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i > 0 {
		s = s[:i]
	}
	return s
}

func (m *Model) authListPageSize() int {
	// Reserve room for the title, input, overflow marker, buttons, hint,
	// border, and padding. The extra row keeps the first menu inside the
	// terminal when the catalog is longer than the viewport.
	per := m.height - 15
	if per < 3 {
		per = 3
	}
	return per
}

// authVisibleRows returns the slice of providers that fits on screen, plus
// the window bounds so the caller can number rows correctly.
func (m *Model) authVisibleRows() ([]authRow, int, int) {
	// Budget: 2 border + 2 padding + title + blank + add line + blank +
	// input + blank + hint, plus the two overflow markers.
	per := m.authListPageSize()
	if per < 3 {
		per = 3
	}
	if per >= len(m.auth.rows) {
		return m.auth.rows, 0, len(m.auth.rows)
	}
	from := m.auth.scroll
	if from > len(m.auth.rows)-per {
		from = len(m.auth.rows) - per
	}
	if from < 0 {
		from = 0
	}
	return m.auth.rows[from : from+per], from, from + per
}

// authScroll moves the provider list window.
func (m *Model) authScroll(delta int) {
	per := m.authListPageSize()
	if per < 3 {
		per = 3
	}
	maxScroll := len(m.auth.rows) - per
	if maxScroll < 0 {
		maxScroll = 0
	}
	m.auth.scroll += delta
	if m.auth.scroll > maxScroll {
		m.auth.scroll = maxScroll
	}
	if m.auth.scroll < 0 {
		m.auth.scroll = 0
	}
}

// authView renders the whole flow.
func (m *Model) authView() string {
	s := m.styles
	w := m.width - 4
	if w > 92 {
		w = 92
	}
	if w < 46 {
		w = 46
	}

	var b strings.Builder
	title := "providers"
	switch m.auth.stage {
	case authEnterKey, authAddKey:
		title = "providers · api key"
	case authEditMenu:
		title = "providers · edit"
	case authAddName, authAddURL:
		title = "providers · add"
	case authProbing:
		title = "providers · connecting"
	case authPickModel, authEnterModel:
		title = "providers · default model"
	case authDeviceCode:
		title = "providers · browser sign-in"
	case authOAuthWaiting:
		title = "providers · waiting for sign-in"
	}
	b.WriteString(s.Primary.Render(title) + "\n\n")

	switch m.auth.stage {
	case authList:
		b.WriteString(m.authListBody(w))
	case authEnterKey, authAddKey:
		b.WriteString(m.authKeyBody(w))
	case authEditMenu:
		b.WriteString(m.authEditBody(w))
	case authAddName:
		b.WriteString(s.Muted.Render("Name for this provider (used in model ids):") + "\n\n")
		b.WriteString("  " + s.Base.Render(m.auth.inputBuf+"█") + "\n")
	case authAddURL:
		b.WriteString(s.Muted.Render("Base URL for "+m.auth.draftID+":") + "\n")
		b.WriteString(s.Faint.Render("  e.g. https://api.example.com/v1 — rick detects the protocol") + "\n\n")
		b.WriteString("  " + s.Base.Render(m.auth.inputBuf+"█") + "\n")
	case authProbing:
		b.WriteString("  " + s.Warning.Render(m.spinnerFrame()+" contacting "+hostOf(m.auth.draftURL)) + "\n")
		b.WriteString(s.Faint.Render("  detecting protocol and fetching models…") + "\n")
	case authPickModel:
		b.WriteString(m.authModelBody(w))
	case authEnterModel:
		b.WriteString(s.Muted.Render("This endpoint does not publish a model list.") + "\n")
		b.WriteString(s.Faint.Render("  Type the model id you want to use, e.g. gpt-4o or claude-sonnet-4-5.") + "\n\n")
		b.WriteString("  " + s.Base.Render(m.auth.inputBuf+"█") + "\n")
	case authDeviceCode:
		b.WriteString(m.authDeviceBody(w))
	case authOAuthWaiting:
		b.WriteString(m.authOAuthBody(w))
	case authKeyMenu:
		b.WriteString(m.authKeyMenuBody(w))
	case authKeyAdd:
		b.WriteString(s.Muted.Render("Paste API key(s), semicolon-separated:") + "\n\n")
		b.WriteString("  " + s.Base.Render(m.auth.inputBuf+"█") + "\n")
	case authKeyMode:
		b.WriteString(m.authKeyModeBody(w))
	}

	if m.auth.statusLine != "" {
		b.WriteString("\n" + m.auth.statusLine + "\n")
	}
	b.WriteString("\n" + m.renderAuthButtons() + "\n")
	b.WriteString(s.Faint.Render(m.authHint()))

	// Below ~14 rows the border and padding cost more than they are worth,
	// and a framed box would overflow the screen. Render bare instead.
	if m.height < 14 {
		return padHeight(trimHeight(b.String(), m.height-1), m.height-1)
	}
	return s.Overlay.Width(w).Render(b.String())
}

func (m *Model) authHint() string {
	switch m.auth.stage {
	case authList:
		return "↑↓ select · enter configure · number/add type shortcut · esc/backspace back"
	case authEnterKey, authAddKey, authAddName, authAddURL:
		return "enter confirm · backspace edit · esc back"
	case authEditMenu:
		return "↑↓ select · enter choose · 1–6 shortcuts · esc/backspace back"
	case authPickModel:
		return "↑↓ select · enter choose · esc/backspace back"
	case authEnterModel:
		return "enter save · backspace edit · esc back"
	case authDeviceCode:
		return "enter continue · esc/backspace back"
	case authOAuthWaiting:
		return "esc/backspace cancel"
	}
	return "esc/backspace back"
}

func (m *Model) authPrimaryLabel() string {
	switch m.auth.stage {
	case authList:
		return "↵ Configure"
	case authEnterKey, authAddName, authAddURL, authAddKey:
		return "↵ Continue"
	case authEnterModel:
		return "↵ Save"
	case authDeviceCode:
		return "↵ Continue"
	case authProbing, authOAuthWaiting:
		return "× Cancel"
	default:
		return "↵ Select"
	}
}

func (m *Model) renderAuthButtons() string {
	back := m.choiceButtonStyle(false).Render("← Back")
	primary := m.choiceButtonStyle(true).Render(m.authPrimaryLabel())
	return "  " + back + " " + primary
}

func (m *Model) authListBody(w int) string {
	s := m.styles
	var b strings.Builder
	active, _ := config.SplitModel(m.modelID)

	// The catalog is longer than most terminals are tall, so page it rather
	// than rendering a list whose top is unreachable.
	rows, from, to := m.authVisibleRows()
	if from > 0 {
		b.WriteString(s.Faint.Render(fmt.Sprintf("  ↑ %d more above", from)) + "\n")
	}
	for i, r := range rows {
		num := s.Faint.Render(fmt.Sprintf("%2d ", from+i+1))
		mark := "  "
		label := s.Muted.Render(r.label)
		if from+i == m.auth.cursor {
			mark = s.Primary.Render("❯ ")
			label = s.Base.Render(r.label)
		} else if r.connected {
			mark = s.Success.Render("✓ ")
			label = s.Base.Render(r.label)
		}
		line := num + mark + padRight(truncate(label, 30), 30)
		detail := r.detail
		if r.id == active {
			detail = "in use · " + detail
		}
		line += s.Faint.Render(truncate(detail, w-40))
		b.WriteString(line + "\n")
	}
	if to < len(m.auth.rows) {
		b.WriteString(s.Faint.Render(fmt.Sprintf("  ↓ %d more below", len(m.auth.rows)-to)) + "\n")
	}

	b.WriteString("\n")
	b.WriteString(s.Faint.Render(" add ") + s.Muted.Render("add a custom provider (any OpenAI/Anthropic endpoint)") + "\n")
	b.WriteString("\n  " + s.Base.Render(m.auth.inputBuf+"█") + "\n")
	return b.String()
}

func (m *Model) authKeyBody(w int) string {
	s := m.styles
	var b strings.Builder
	name := m.auth.target.Name
	if name == "" {
		name = m.auth.draftID
	}
	b.WriteString(s.Muted.Render("API key for "+name+":") + "\n")
	if h := m.auth.target.KeyHint; h != "" {
		b.WriteString(s.Faint.Render("  get one at "+h) + "\n")
	}
	if len(m.auth.target.KeyEnv) > 0 {
		b.WriteString(s.Faint.Render("  or set $"+m.auth.target.KeyEnv[0]) + "\n")
	}
	b.WriteString("\n  " + s.Base.Render(maskKey(m.auth.inputBuf)+"█") + "\n")
	return b.String()
}

func maskKey(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("•", len(s))
	}
	return s[:4] + strings.Repeat("•", len(s)-8) + s[len(s)-4:]
}

func (m *Model) authEditBody(w int) string {
	s := m.styles
	cred, _ := m.creds.Get(m.auth.draftID)
	var b strings.Builder

	b.WriteString(s.Base.Render(m.auth.draftID) + s.Success.Render("  ✓ connected") + "\n\n")
	row := func(k, v string) {
		b.WriteString("  " + s.Faint.Render(padRight(k, 12)) + s.Muted.Render(v) + "\n")
	}
	row("protocol", cred.Type)
	row("endpoint", cred.BaseURL)
	keys := m.creds.AllKeys(m.auth.draftID)
	row("keys", fmt.Sprintf("%d configured", len(keys)))
	mode := cred.APIKeyMode
	if mode == "" {
		mode = "single"
	}
	row("mode", mode)
	row("models", fmt.Sprintf("%d fetched", len(cred.Models)))
	if cred.OnlyFree {
		row("filter", "free only")
	}
	if cred.Default != "" {
		row("default", cred.Default)
	}

	b.WriteString("\n")
	opts := []string{
		"manage keys", "change base URL", "refresh model list",
		"set default model",
	}
	if cred.OnlyFree {
		opts = append(opts, "show all models", "remove this provider")
	} else {
		opts = append(opts, "only free models", "remove this provider")
	}
	for i, opt := range opts {
		prefix := s.Faint.Render(fmt.Sprintf("  %d ", i+1))
		label := s.Muted.Render(opt)
		if i == m.auth.cursor {
			prefix = s.Primary.Render("❯ ")
			label = s.Base.Render(opt)
		}
		b.WriteString(prefix + label + "\n")
	}
	return b.String()
}

func (m *Model) authSelectableModels() []catalog.Model {
	models := catalog.FilterChatModels(m.auth.models)
	cred, _ := m.creds.Get(m.auth.draftID)
	if !cred.OnlyFree {
		return models
	}

	filtered := make([]catalog.Model, 0, len(models))
	for _, model := range models {
		if model.Free {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func (m *Model) authModelBody(w int) string {
	s := m.styles
	cred, _ := m.creds.Get(m.auth.draftID)
	models := m.authSelectableModels()
	var b strings.Builder
	b.WriteString(s.Muted.Render(fmt.Sprintf("%d models available from %s:", len(models), m.auth.draftID)))
	if cred.OnlyFree {
		b.WriteString(s.Faint.Render(" (free only)"))
	}
	b.WriteString("\n\n")

	const page = 12
	start := m.auth.cursor - m.auth.cursor%page
	end := start + page
	if end > len(models) {
		end = len(models)
	}
	for i := start; i < end; i++ {
		mk := "  "
		st := s.Muted
		if i == m.auth.cursor {
			mk = s.Primary.Render("❯ ")
			st = s.Base
		}
		mm := models[i]
		line := mk + st.Render(truncate(mm.ID, w-30))
		ctxLen := mm.Context
		if override, ok := provider.ProviderContextWindow(m.auth.draftID, mm.ID); ok {
			ctxLen = override
		}
		if ctxLen <= 0 {
			ctxLen = provider.KnownProviderContextWindow(m.auth.draftID, mm.ID)
		}
		if ctxLen > 0 {
			line += s.Faint.Render("  " + humanTokens(ctxLen))
		}
		if style, _ := provider.DetectReasoningForProvider(m.auth.draftID, mm.ID); style != provider.ReasoningStyleNone && style != provider.ReasoningStyleUnknown {
			line += s.Secondary.Render("  reasoning")
		}
		if mm.Free {
			line += s.Success.Render("  free")
		}
		b.WriteString(line + "\n")
	}
	if len(models) > page {
		b.WriteString(s.Faint.Render(fmt.Sprintf("\n  %d of %d", m.auth.cursor+1, len(models))) + "\n")
	}
	return b.String()
}

func (m *Model) authDeviceBody(w int) string {
	s := m.styles
	var b strings.Builder
	if m.auth.target.OAuth != nil {
		// OAuth is available — this screen is the fallback if it fails.
		b.WriteString(s.Muted.Render("  "+m.auth.target.Name+" uses an OAuth device-code flow.") + "\n")
		b.WriteString(s.Faint.Render("  Press enter to start browser sign-in, or paste a token manually:") + "\n\n")
		b.WriteString(s.Faint.Render("  1. sign in at "+m.auth.target.KeyHint) + "\n")
		b.WriteString(s.Faint.Render("  2. copy an API key / access token") + "\n")
		b.WriteString(s.Faint.Render("  3. press enter here and paste it") + "\n")
	} else {
		b.WriteString(s.Warning.Render("  browser sign-in is not wired up for this provider") + "\n\n")
		b.WriteString(s.Muted.Render("  "+m.auth.target.Name+" uses an OAuth device-code flow.") + "\n")
		b.WriteString(s.Muted.Render("  rick can still use it with a bearer token:") + "\n\n")
		b.WriteString(s.Faint.Render("  1. sign in at "+m.auth.target.KeyHint) + "\n")
		b.WriteString(s.Faint.Render("  2. copy an API key / access token") + "\n")
		b.WriteString(s.Faint.Render("  3. press enter here and paste it") + "\n")
	}
	return b.String()
}

// authOAuthBody renders the "waiting for authorization" screen.
func (m *Model) authOAuthBody(w int) string {
	s := m.styles
	var b strings.Builder
	b.WriteString(s.Muted.Render("  Open this URL in your browser:") + "\n")
	b.WriteString("  " + s.Primary.Render(m.auth.oauthVerifURI) + "\n\n")
	b.WriteString(s.Muted.Render("  Enter code:") + "\n")
	b.WriteString("  " + s.Warning.Render("  "+m.auth.oauthUserCode) + "\n\n")
	b.WriteString("  " + s.Faint.Render(m.spinnerFrame()+" waiting for authorization…") + "\n")
	return b.String()
}

// handleAuthKey routes keys while /auth is open.
func (m *Model) handleAuthKey(msg tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	a := &m.auth

	if key == "esc" || (key == "backspace" && !authBackspaceEdits(a.stage, a.inputBuf)) {
		return m.authBack()
	}
	if a.busy {
		return m, nil // ignore input while probing
	}

	switch a.stage {
	case authList:
		return m.authListKey(msg, key)
	case authEditMenu:
		return m.authEditKey(key)
	case authPickModel:
		return m.authModelKey(key)
	case authDeviceCode:
		if key == "enter" {
			// If this provider has a real OAuth flow, start it.
			if a.target.OAuth != nil {
				return m.authStartOAuth()
			}
			// Otherwise fall back to manual token paste.
			a.stage = authEnterKey
			a.inputBuf = ""
			if a.draftURL == "" {
				a.draftURL = catalog.FirstNonEmpty(a.target.EnvBaseURL(), a.target.BaseURL)
			}
		}
		return m, nil
	case authOAuthWaiting:
		// Only esc is handled (above); ignore everything else.
		return m, nil
	case authEnterKey, authAddKey, authAddName, authAddURL, authEnterModel:
		return m.authInputKey(msg, key)
	case authKeyMenu:
		return m.authKeyMenuKey(key)
	case authKeyAdd:
		return m.authKeyAddKey(msg, key)
	case authKeyMode:
		return m.authKeyModeKey(key)
	}
	return m, nil
}

func authBackspaceEdits(stage authStage, input string) bool {
	if stage == authList {
		return input != ""
	}
	switch stage {
	case authEnterKey, authAddName, authAddURL, authAddKey, authEnterModel, authKeyAdd:
		return true
	default:
		return false
	}
}

func (m *Model) authBack() (tea.Model, tea.Cmd) {
	a := &m.auth
	a.statusLine = ""
	if a.probeCancel != nil {
		a.probeCancel()
		a.probeCancel = nil
	}
	if a.stage == authProbing {
		a.probeGen++
	}
	a.busy = false
	switch a.stage {
	case authList:
		*a = authState{}
		m.input.Focus()
		m.refresh()
	case authEditMenu, authAddName, authProbing, authDeviceCode, authEnterKey, authEnterModel:
		a.stage = authList
		a.inputBuf = ""
		m.rebuildAuthRows()
	case authAddURL:
		if a.returnTo == authEditMenu {
			a.stage = authEditMenu
		} else {
			a.stage = authAddName
		}
		a.inputBuf = ""
	case authAddKey:
		a.stage = authAddURL
		a.inputBuf = ""
	case authPickModel:
		if a.returnTo == authEditMenu {
			a.stage = authEditMenu
		} else {
			a.stage = authList
			m.rebuildAuthRows()
		}
	case authOAuthWaiting:
		a.oauthGen++
		if a.oauthCancel != nil {
			a.oauthCancel()
			a.oauthCancel = nil
		}
		a.oauthUserCode = ""
		a.oauthVerifURI = ""
		a.stage = authList
	case authKeyMenu, authKeyMode:
		a.stage = authEditMenu
	case authKeyAdd:
		a.stage = authKeyMenu
	}
	return m, nil
}

func (m *Model) authListKey(msg tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	a := &m.auth
	switch key {
	case "enter":
		in := strings.TrimSpace(strings.ToLower(a.inputBuf))
		a.inputBuf = ""
		if in == "" {
			if len(a.rows) == 0 {
				return m, nil
			}
			return m.authSelectRow(a.rows[a.cursor])
		}
		if in == "add" || in == "a" || in == "+" {
			a.stage = authAddName
			a.custom = true
			a.returnTo = authAddName
			a.draftID, a.draftURL, a.draftKey = "", "", ""
			a.inputBuf = ""
			return m, nil
		}
		// A number selects a row; a bare name also works.
		idx := -1
		if n, err := strconv.Atoi(in); err == nil && n >= 1 && n <= len(a.rows) {
			idx = n - 1
		} else {
			for i, r := range a.rows {
				if strings.EqualFold(r.id, in) || strings.EqualFold(r.label, in) {
					idx = i
					break
				}
			}
		}
		if idx < 0 {
			a.statusLine = m.styles.Error.Render("  no provider matches " + strconv.Quote(in))
			return m, nil
		}
		return m.authSelectRow(a.rows[idx])

	case "backspace":
		if len(a.inputBuf) > 0 {
			a.inputBuf = a.inputBuf[:len(a.inputBuf)-1]
		}
		return m, nil

	case "up", "ctrl+p":
		if a.cursor > 0 {
			a.cursor--
		}
		m.authRevealRow(a.cursor)
		return m, nil
	case "down", "ctrl+n":
		if a.cursor < len(a.rows)-1 {
			a.cursor++
		}
		m.authRevealRow(a.cursor)
		return m, nil
	case "pgup":
		m.authScroll(-(m.height - 14))
		return m, nil
	case "pgdown":
		m.authScroll(m.height - 14)
		return m, nil
	case "home":
		a.scroll = 0
		return m, nil
	case "end":
		m.authScroll(len(a.rows))
		return m, nil
	}
	if len(msg.Runes) == 1 {
		a.inputBuf += string(msg.Runes)
		// Typing a number should reveal the row it refers to.
		if n, err := strconv.Atoi(strings.TrimSpace(a.inputBuf)); err == nil && n >= 1 {
			m.authRevealRow(n - 1)
		}
	}
	return m, nil
}

// authRevealRow scrolls a row into view.
func (m *Model) authRevealRow(idx int) {
	per := m.authListPageSize()
	if per < 3 {
		per = 3
	}
	switch {
	case idx < m.auth.scroll:
		m.auth.scroll = idx
	case idx >= m.auth.scroll+per:
		m.auth.scroll = idx - per + 1
	}
	m.authScroll(0)
}

// authSelectRow opens the right sub-flow for a chosen provider.
func (m *Model) authSelectRow(r authRow) (tea.Model, tea.Cmd) {
	a := &m.auth
	a.statusLine = ""
	a.draftID = r.id
	a.custom = r.custom
	a.confirmRemove = false
	a.target, _ = catalog.Get(r.id)

	// Already saved: offer the edit menu.
	if _, ok := m.creds.Get(r.id); ok {
		a.stage = authEditMenu
		a.cursor = 0
		return m, nil
	}
	// Satisfied by an env var: adopt it and verify immediately.
	if r.envOnly {
		key, _ := a.target.EnvKey()
		a.draftKey = key
		a.draftURL = catalog.FirstNonEmpty(a.target.EnvBaseURL(), a.target.BaseURL)
		return m.authStartProbe()
	}
	switch a.target.Auth {
	case catalog.AuthNone:
		a.draftKey = ""
		a.draftURL = catalog.FirstNonEmpty(a.target.EnvBaseURL(), a.target.BaseURL)
		return m.authStartProbe()
	case catalog.AuthDeviceCode, catalog.AuthExternal:
		a.stage = authDeviceCode
		return m, nil
	default:
		a.stage = authEnterKey
		a.inputBuf = ""
		a.draftURL = catalog.FirstNonEmpty(a.target.EnvBaseURL(), a.target.BaseURL)
		return m, nil
	}
}

func (m *Model) authInputKey(msg tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	a := &m.auth
	switch key {
	case "enter":
		val := strings.TrimSpace(a.inputBuf)
		a.inputBuf = ""
		// Any submission supersedes the previous complaint; a handler below
		// sets a fresh one if this input is also bad. Without this an old
		// error trails the user onto the next screen.
		a.statusLine = ""
		switch a.stage {
		case authAddName:
			if val == "" {
				a.statusLine = m.styles.Error.Render("  a name is required")
				return m, nil
			}
			a.draftID = sanitizeID(val)
			a.stage = authAddURL
			return m, nil
		case authAddURL:
			url, err := normalizeURL(val)
			if err != nil {
				a.statusLine = m.styles.Error.Render("  " + err.Error())
				a.inputBuf = val // keep it so the user can fix, not retype
				return m, nil
			}
			if url != val {
				a.statusLine = m.styles.Faint.Render("  using " + url)
			}
			a.draftURL = url
			a.stage = authAddKey
			return m, nil
		case authEnterModel:
			if val == "" {
				a.stage = authList
				m.rebuildAuthRows()
				return m, nil
			}
			cred, _ := m.creds.Get(a.draftID)
			cred.Default = val
			if len(cred.Models) == 0 {
				cred.Models = []string{val}
			}
			m.creds.Set(a.draftID, cred)
			_ = m.creds.Save()
			m.reloadProviders()
			m.setModel(a.draftID + "/" + val)
			a.stage = authList
			m.rebuildAuthRows()
			a.statusLine = m.styles.Success.Render("  active model: " + m.modelID)
			m.setStatus("model: " + m.displayModel())
			return m, nil
		case authAddKey, authEnterKey:
			// A pasted key routinely carries a trailing newline or a
			// bracketed-paste wrapper, which makes net/http refuse to send
			// the Authorization header at all.
			clean := catalog.CleanSecret(val)
			if clean == "" && a.target.NeedsKey() && a.stage == authEnterKey {
				a.statusLine = m.styles.Error.Render("  a key is required for this provider")
				return m, nil
			}
			if clean != val && clean != "" {
				a.statusLine = m.styles.Faint.Render("  cleaned up pasted whitespace")
			}
			a.draftKey = clean
			return m.authStartProbe()
		}
		return m, nil

	case "backspace":
		if len(a.inputBuf) > 0 {
			a.inputBuf = a.inputBuf[:len(a.inputBuf)-1]
		}
		return m, nil

	case "ctrl+u":
		a.inputBuf = ""
		return m, nil

	case "ctrl+v":
		return m, nil // terminals paste as runes; nothing to do
	}
	if len(msg.Runes) >= 1 {
		// Paste arrives as a rune burst; strip characters that can never be
		// part of a URL or a credential so they cannot corrupt the value.
		for _, r := range msg.Runes {
			if r == '\r' || r == '\n' || r == 0 {
				continue
			}
			a.inputBuf += string(r)
		}
		a.statusLine = "" // the complaint referred to what was there before
	}
	return m, nil
}

func (m *Model) authKeyMenuKey(key string) (tea.Model, tea.Cmd) {
	a := &m.auth
	a.statusLine = ""
	if key == "up" || key == "ctrl+p" {
		if a.cursor > 0 {
			a.cursor--
		}
		return m, nil
	}
	if key == "down" || key == "ctrl+n" {
		if a.cursor < 2 {
			a.cursor++
		}
		return m, nil
	}
	if key == "enter" {
		key = strconv.Itoa(a.cursor + 1)
	}

	switch key {
	case "1":
		a.inputBuf = ""
		a.stage = authKeyAdd
	case "2":
		a.cursor = 0
		a.stage = authKeyMode
	case "3":
		a.cursor = 0
		a.stage = authEditMenu
	}
	return m, nil
}

func (m *Model) authKeyAddKey(msg tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	a := &m.auth
	a.statusLine = ""

	switch key {
	case "enter":
		return m.applyKeyAdd(a.inputBuf)
	case "backspace":
		if len(a.inputBuf) > 0 {
			a.inputBuf = a.inputBuf[:len(a.inputBuf)-1]
		}
	case "ctrl+u":
		a.inputBuf = ""
	case "ctrl+v":
		// terminals paste as runes; nothing to do
	default:
		if len(msg.Runes) >= 1 {
			for _, r := range msg.Runes {
				if r == '\r' || r == '\n' || r == 0 {
					continue
				}
				a.inputBuf += string(r)
			}
		}
	}
	return m, nil
}

func (m *Model) authKeyModeKey(key string) (tea.Model, tea.Cmd) {
	a := &m.auth
	a.statusLine = ""
	if key == "up" || key == "ctrl+p" {
		if a.cursor > 0 {
			a.cursor--
		}
		return m, nil
	}
	if key == "down" || key == "ctrl+n" {
		if a.cursor < 2 {
			a.cursor++
		}
		return m, nil
	}
	if key == "enter" {
		key = strconv.Itoa(a.cursor + 1)
	}

	switch key {
	case "1":
		return m.applyKeyMode("single")
	case "2":
		return m.applyKeyMode("round-robin")
	case "3":
		return m.applyKeyMode("failover")
	}
	return m, nil
}

// scheme typo, a full endpoint path — and returns something dialable, or an
// error explaining precisely what is wrong.
func normalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "\"'")            // quotes from a copy-paste
	s = strings.ReplaceAll(s, "\x00", "") // Windows null-byte corruption
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("a URL is required")
	}
	if strings.ContainsAny(s, " \t") {
		return "", fmt.Errorf("URL must not contain spaces")
	}

	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		// good as-is
	case strings.HasPrefix(lower, "http:/") || strings.HasPrefix(lower, "https:/"):
		// a single slash: repair rather than scold
		s = strings.Replace(s, ":/", "://", 1)
	case strings.Contains(lower, "://"):
		scheme := lower[:strings.Index(lower, "://")]
		return "", fmt.Errorf("unsupported scheme %q — use http:// or https://", scheme)
	default:
		// A bare host is the most common input; assume TLS, except locally.
		if strings.HasPrefix(lower, "localhost") || strings.HasPrefix(lower, "127.0.0.1") {
			s = "http://" + s
		} else {
			s = "https://" + s
		}
	}

	rest := s[strings.Index(s, "://")+3:]
	if rest == "" || strings.HasPrefix(rest, "/") {
		return "", fmt.Errorf("URL is missing a host")
	}
	host := rest
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if h, _, found := strings.Cut(host, ":"); found {
		host = h // strip the port before validating
	}
	// A host must look like one: a dotted name, localhost, or a literal IP.
	// Without this "not-a-url" is treated as a hostname and we spend a DNS
	// timeout discovering the obvious.
	if !strings.Contains(host, ".") &&
		!strings.EqualFold(host, "localhost") &&
		!strings.Contains(host, "::") {
		return "", fmt.Errorf("%q is not a valid host — did you mean https://%s.com?", host, host)
	}
	return strings.TrimRight(s, "/"), nil
}

func sanitizeID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "custom"
	}
	return out
}

// oauthStartMsg carries the device-code response back into Update.
type oauthStartMsg struct {
	gen  int
	resp *catalog.DeviceCodeResponse
	err  error
}

// oauthDoneMsg carries the final token (or error) back into Update.
type oauthDoneMsg struct {
	gen   int
	token *catalog.TokenResponse
	err   error
}

// authStartOAuth kicks off the RFC 8628 device-code flow for the current
// target provider. It runs Start in a goroutine; on success the TUI shows
// the user code and begins polling.
func (m *Model) authStartOAuth() (tea.Model, tea.Cmd) {
	a := &m.auth
	flow := a.target.OAuth
	if flow == nil {
		// Shouldn't happen, but fall back gracefully.
		a.stage = authEnterKey
		a.inputBuf = ""
		return m, nil
	}

	a.oauthGen++
	gen := a.oauthGen
	a.busy = true
	a.statusLine = ""

	return m, tea.Batch(m.spinnerCmd(), func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := flow.Start(ctx)
		return oauthStartMsg{gen: gen, resp: resp, err: err}
	})
}

// applyOAuthStart handles the device-code response: show the code, open the
// browser, and start polling. Returns a Cmd that blocks until polling completes.
func (m *Model) applyOAuthStart(msg oauthStartMsg) tea.Cmd {
	a := &m.auth
	if msg.gen != a.oauthGen {
		return nil // stale
	}
	a.busy = false

	if msg.err != nil {
		// OAuth start failed — fall back to manual paste.
		a.stage = authDeviceCode
		a.statusLine = m.styles.Error.Render("  OAuth failed: "+truncate(msg.err.Error(), m.width-16)) +
			"\n" + m.styles.Faint.Render("  press enter to paste a token manually")
		return nil
	}

	a.oauthUserCode = msg.resp.UserCode
	a.oauthVerifURI = msg.resp.VerificationURI
	a.stage = authOAuthWaiting

	// Open the browser.
	openBrowser(msg.resp.VerificationURI)

	// Return a Cmd that blocks until the poll completes.
	flow := a.target.OAuth
	deviceCode := msg.resp.DeviceCode
	interval := msg.resp.Interval
	expiresIn := msg.resp.ExpiresIn
	gen := a.oauthGen

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(expiresIn)*time.Second)
	a.oauthCancel = cancel

	return tea.Batch(m.spinnerCmd(), func() tea.Msg {
		defer cancel()
		tok, err := flow.Poll(ctx, deviceCode, interval)
		return oauthDoneMsg{gen: gen, token: tok, err: err}
	})
}

// applyOAuthDone handles the final token response.
func (m *Model) applyOAuthDone(msg oauthDoneMsg) (tea.Model, tea.Cmd) {
	a := &m.auth
	if msg.gen != a.oauthGen {
		return m, nil // stale
	}
	if a.oauthCancel != nil {
		a.oauthCancel()
		a.oauthCancel = nil
	}

	if msg.err != nil {
		// Poll failed — fall back to manual paste.
		a.stage = authDeviceCode
		a.oauthUserCode = ""
		a.oauthVerifURI = ""
		a.statusLine = m.styles.Error.Render("  sign-in failed: "+truncate(msg.err.Error(), m.width-16)) +
			"\n" + m.styles.Faint.Render("  press enter to paste a token manually")
		return m, nil
	}

	apiKey := msg.token.AccessToken

	// GitHub Copilot needs a token exchange.
	if a.target.CopilotExchange {
		a.statusLine = m.styles.Faint.Render("  exchanging for Copilot token…")
		exchCtx, exchCancel := context.WithTimeout(context.Background(), 30*time.Second)
		copilotToken, err := catalog.CopilotTokenExchange(exchCtx, apiKey)
		exchCancel()
		if err != nil {
			a.stage = authDeviceCode
			a.oauthUserCode = ""
			a.oauthVerifURI = ""
			a.statusLine = m.styles.Error.Render("  Copilot exchange failed: "+truncate(err.Error(), m.width-20)) +
				"\n" + m.styles.Faint.Render("  press enter to paste a token manually")
			return m, nil
		}
		apiKey = copilotToken
	}

	a.oauthUserCode = ""
	a.oauthVerifURI = ""
	a.draftKey = apiKey
	a.draftURL = catalog.FirstNonEmpty(a.target.EnvBaseURL(), a.target.BaseURL)
	return m.authStartProbe()
}

// openBrowser opens a URL in the user's default browser.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// authStartProbe contacts the endpoint to detect its protocol and models.
func (m *Model) authStartProbe() (tea.Model, tea.Cmd) {
	a := &m.auth
	if a.probeCancel != nil {
		a.probeCancel()
	}
	a.probeGen++
	gen := a.probeGen
	if a.stage != authProbing {
		a.returnTo = a.stage
	}
	a.stage = authProbing
	a.busy = true
	a.probeErr = nil
	a.statusLine = ""

	id, url, key := a.draftID, a.draftURL, a.draftKey
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	a.probeCancel = cancel
	return m, tea.Batch(m.spinnerCmd(), func() tea.Msg {
		defer cancel()
		res := catalog.Probe(ctx, url, key)
		return authProbeMsg{gen: gen, id: id, key: key, res: res}
	})
}

// authProbeMsg carries a completed probe back into Update.
type authProbeMsg struct {
	gen int
	id  string
	key string
	res catalog.ProbeResult
}

func (m *Model) applyAuthProbe(msg authProbeMsg) {
	a := &m.auth
	if msg.gen != a.probeGen {
		return
	}
	a.probeCancel = nil
	a.busy = false

	if msg.res.Err != nil {
		a.probeErr = msg.res.Err
		a.stage = a.returnTo // the stage that launched the probe
		if a.stage == authProbing || a.stage == 0 {
			a.stage = authEnterKey
		}
		a.statusLine = m.styles.Error.Render("  " + truncate(msg.res.Err.Error(), m.width-12))
		return
	}

	// Persist the credential.
	label := a.target.Name
	if label == "" {
		label = msg.id
	}
	// Persist only models Rick can use for text/tool conversations. This is
	// deliberately done before caching so /models and future sessions agree.
	msg.res.Models = catalog.FilterChatModels(msg.res.Models)
	ids := make([]string, 0, len(msg.res.Models))
	visionModels := make([]string, 0)
	windows := make(map[string]int, len(msg.res.Models))
	sources := make(map[string]provider.ContextSource, len(msg.res.Models))
	modalitiesKnown := false
	for _, mm := range msg.res.Models {
		ids = append(ids, mm.ID)
		if mm.SupportsImages {
			visionModels = append(visionModels, mm.ID)
		}
		modalitiesKnown = modalitiesKnown || mm.ModalitiesKnown
		// Prefer the provider-specific deployment override, then what the
		// endpoint reported, then what the model id implies.
		n := mm.Context
		contextSource := mm.ContextSource
		if override, ok := provider.ProviderContextWindow(msg.id, mm.ID); ok {
			n = override
			contextSource = provider.ContextSourceCatalog
		}
		if n <= 0 {
			n = provider.KnownProviderContextWindow(msg.id, mm.ID)
		}
		if n > 0 {
			windows[mm.ID] = n
			if contextSource != provider.ContextSourceUnknown {
				sources[mm.ID] = contextSource
			}
		}
	}
	// Start from the existing record. A connectivity probe refreshes endpoint
	// metadata; it must not erase APIKeys, rotation mode, the selected model,
	// disabled state, or user labels/custom fields.
	cred, _ := m.creds.Get(msg.id)
	oldCred := cred
	cred.Type = msg.res.Flavor
	if len(cred.APIKeys) == 0 {
		cred.APIKey = msg.key
	}
	cred.BaseURL = msg.res.BaseURL
	if cred.Label == "" {
		cred.Label = label
	}
	if len(ids) > 0 {
		cred.Models = ids
		cred.ContextWindows = windows
		cred.ContextSources = sources
		cred.VisionModels = visionModels
		cred.ModalitiesKnown = modalitiesKnown
	}
	cred.Custom = a.custom || cred.Custom
	m.creds.Set(msg.id, cred)
	if err := m.creds.Save(); err != nil {
		m.creds.Set(msg.id, oldCred)
		a.statusLine = m.styles.Error.Render("  could not save: " + err.Error())
		return
	}

	m.reloadProviders()
	a.models = msg.res.Models
	a.cursor = 0

	if msg.res.Partial || len(msg.res.Models) == 0 {
		// The endpoint works but does not publish a model list. Ask for a
		// model id instead of dead-ending on an empty picker.
		a.stage = authEnterModel
		a.inputBuf = ""
		a.statusLine = m.styles.Warning.Render(fmt.Sprintf(
			"  connected · %s protocol · endpoint has no model list", msg.res.Flavor))
		return
	}

	a.stage = authPickModel
	a.statusLine = m.styles.Success.Render(fmt.Sprintf(
		"  connected · %s protocol · %d models", msg.res.Flavor, len(msg.res.Models)))
}

func (m *Model) authModelKey(key string) (tea.Model, tea.Cmd) {
	a := &m.auth
	models := m.authSelectableModels()
	switch key {
	case "up", "ctrl+p":
		if a.cursor > 0 {
			a.cursor--
		}
	case "down", "ctrl+n":
		if a.cursor < len(models)-1 {
			a.cursor++
		}
	case "pgup":
		a.cursor -= 12
		if a.cursor < 0 {
			a.cursor = 0
		}
	case "pgdown":
		a.cursor += 12
		if a.cursor >= len(models) {
			a.cursor = len(models) - 1
		}
	case "enter":
		if len(models) == 0 {
			a.stage = authList
			return m, nil
		}
		chosen := models[a.cursor].ID
		cred, _ := m.creds.Get(a.draftID)
		cred.Default = chosen
		m.creds.Set(a.draftID, cred)
		_ = m.creds.Save()

		m.setModel(a.draftID + "/" + chosen)
		a.stage = authList
		a.inputBuf = ""
		m.rebuildAuthRows()
		a.statusLine = m.styles.Success.Render("  active model: " + m.modelID)
		m.setStatus("model: " + m.displayModel())
	}
	return m, nil
}

func (m *Model) authEditKey(key string) (tea.Model, tea.Cmd) {
	a := &m.auth
	cred, _ := m.creds.Get(a.draftID)
	a.statusLine = ""
	if key == "up" || key == "ctrl+p" {
		if a.cursor > 0 {
			a.cursor--
		}
		return m, nil
	}
	if key == "down" || key == "ctrl+n" {
		if a.cursor < 5 {
			a.cursor++
		}
		return m, nil
	}
	if key == "enter" {
		key = strconv.Itoa(a.cursor + 1)
	}

	switch key {
	case "1":
		return m.cmdManageKeys()
	case "2":
		a.returnTo = authEditMenu
		a.stage = authAddURL
		a.inputBuf = cred.BaseURL
		a.custom = cred.Custom
	case "3":
		a.draftURL, a.draftKey = cred.BaseURL, m.creds.CurrentKey(a.draftID)
		return m.authStartProbe()
	case "4":
		if len(cred.Models) == 0 {
			a.draftURL, a.draftKey = cred.BaseURL, m.creds.CurrentKey(a.draftID)
			return m.authStartProbe()
		}
		a.models = a.models[:0]
		for _, id := range cred.Models {
			a.models = append(a.models, catalog.Model{ID: id})
		}
		a.cursor = 0
		a.returnTo = authEditMenu
		a.stage = authPickModel
	case "5":
		cred.OnlyFree = !cred.OnlyFree
		m.creds.Set(a.draftID, cred)
		_ = m.creds.Save()
	case "6":
		if !a.confirmRemove {
			a.confirmRemove = true
			a.statusLine = m.styles.Warning.Render("  press 6 again to permanently remove this provider")
			return m, nil
		}
		a.confirmRemove = false
		m.creds.Remove(a.draftID)
		if err := m.creds.Save(); err != nil {
			m.creds.Set(a.draftID, cred)
			a.statusLine = m.styles.Error.Render("  provider removal was not saved: " + err.Error())
			return m, nil
		}
		m.reloadProviders()
		a.stage = authList
		m.rebuildAuthRows()
		a.statusLine = ""
	}
	return m, nil
}

// reloadProviders rebuilds the live provider set from saved credentials so a
// new login is usable immediately, without restarting rick.
//
// Credential removal changes the live provider set but never deletes entries
// from the loaded project configuration. The project configuration is not an
// auth-store scratch buffer, and mutating it here used to make unrelated
// provider settings disappear on refresh/remove flows.
func (m *Model) reloadProviders() {
	if m.deps.Loaded == nil {
		return
	}
	cfg := m.deps.Loaded.Config
	if cfg.Providers == nil {
		cfg.Providers = map[string]config.Provider{}
	}
	creds := m.creds.Snapshot()
	for id, cred := range creds {
		if cred.Disabled {
			continue
		}
		cfg.Providers[id] = config.Provider{
			Type:    cred.Type,
			APIKey:  m.creds.CurrentKey(id),
			BaseURL: cred.BaseURL,
		}
	}
	m.deps.Loaded.Config = cfg

	provs := map[string]provider.Provider{}
	for id, p := range cfg.Providers {
		if cred, hasCredential := creds[id]; hasCredential && cred.Disabled {
			continue
		}
		if _, pinned := m.pinnedProviders[id]; !pinned {
			if _, hasCredential := creds[id]; !hasCredential {
				continue
			}
		}
		if p.Enabled != nil && !*p.Enabled {
			continue
		}
		kind := p.Type
		if kind == "" {
			if e, ok := catalog.Get(id); ok {
				kind = e.Flavor
			} else {
				kind = catalog.FlavorOpenAI
			}
		}
		switch kind {
		case catalog.FlavorAnthropic:
			if p.APIKey == "" && p.BaseURL == "" {
				continue
			}
			provs[id] = anthropic.New(p.APIKey, p.BaseURL)
		default:
			if p.APIKey == "" && p.BaseURL == "" {
				continue
			}
			c := openai.New(id, p.APIKey, p.BaseURL)
			c.SetKeepalive(time.Duration(cfg.CacheKeepaliveSeconds) * time.Second)
			if cred, ok := creds[id]; ok && len(cred.Models) > 0 {
				infos := make([]provider.ModelInfo, 0, len(cred.Models))
				for _, mid := range cred.Models {
					contextWindow := cred.ContextWindows[mid]
					contextSource := cred.ContextSources[mid]
					if override, ok := provider.ProviderContextWindow(id, mid); ok {
						contextWindow = override
						contextSource = provider.ContextSourceCatalog
					}
					infos = append(infos, provider.ModelInfo{
						ID: mid, Name: mid, ContextWindow: contextWindow,
						ContextSource:  contextSource,
						SupportsImages: stringSliceContains(cred.VisionModels, mid),
					})
				}
				c.SetModels(provider.FilterChatModels(infos))
			}
			provs[id] = c
		}
	}
	m.deps.Providers = provs
	if m.modelID != "" {
		providerID, _ := config.SplitModel(m.modelID)
		if _, stillConfigured := provs[providerID]; !stillConfigured {
			m.modelID = ""
			m.updateContextWindow()
			m.status = ""
		}
	}
}

// ---------- multi-key management ----------

func (m *Model) cmdManageKeys() (tea.Model, tea.Cmd) {
	m.auth.stage = authKeyMenu
	m.auth.cursor = 0
	return m, nil
}

func (m *Model) authKeyMenuBody(w int) string {
	s := m.styles
	cred, _ := m.creds.Get(m.auth.draftID)
	keys := m.creds.AllKeys(m.auth.draftID)

	var b strings.Builder
	b.WriteString(s.Muted.Render(fmt.Sprintf("Keys for %s:", m.auth.draftID)) + "\n\n")
	for i, k := range keys {
		b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, s.Muted.Render(maskKey(k))))
	}
	mode := cred.APIKeyMode
	if mode == "" {
		mode = "single"
	}
	b.WriteString(s.Faint.Render(fmt.Sprintf("\nMode: %s", mode)) + "\n\n")
	options := []string{"＋ Add key(s)", "Change mode", "← Back"}
	for i, option := range options {
		prefix := s.Faint.Render(fmt.Sprintf("  %d ", i+1))
		label := s.Muted.Render(option)
		if i == m.auth.cursor {
			prefix = s.Primary.Render("❯ ")
			label = s.Base.Render(option)
		}
		b.WriteString(prefix + label + "\n")
	}
	return b.String()
}

func (m *Model) authKeyModeBody(w int) string {
	s := m.styles
	var b strings.Builder
	b.WriteString(s.Muted.Render("Key rotation mode:") + "\n\n")
	options := []string{"Single — use first key", "Round-robin — rotate each request", "Failover — rotate on rate-limit"}
	for i, option := range options {
		prefix := s.Faint.Render(fmt.Sprintf("  %d ", i+1))
		label := s.Muted.Render(option)
		if i == m.auth.cursor {
			prefix = s.Primary.Render("❯ ")
			label = s.Base.Render(option)
		}
		b.WriteString(prefix + label + "\n")
	}
	return b.String()
}

func (m *Model) applyKeyManage(value string) (tea.Model, tea.Cmd) {
	m.auth.statusLine = ""
	switch value {
	case "1":
		m.auth.inputBuf = ""
		m.auth.stage = authKeyAdd
	case "2":
		m.auth.stage = authKeyMode
	case "3":
		m.auth.stage = authEditMenu
	}
	return m, nil
}

func (m *Model) applyKeyMode(value string) (tea.Model, tea.Cmd) {
	cred, _ := m.creds.Get(m.auth.draftID)
	mode := "single"
	switch value {
	case "1":
		mode = "single"
	case "2":
		mode = "round-robin"
	case "3":
		mode = "failover"
	}
	cred.APIKeyMode = mode
	m.creds.Set(m.auth.draftID, cred)
	_ = m.creds.Save()
	m.reloadProviders()
	m.auth.statusLine = m.styles.Success.Render("  key mode: " + mode)
	m.auth.stage = authKeyMenu
	return m, nil
}

func (m *Model) applyKeyAdd(text string) (tea.Model, tea.Cmd) {
	text = strings.TrimSpace(text)
	m.auth.stage = authKeyMenu
	if text == "" {
		return m, nil
	}
	parts := strings.Split(text, ";")
	var newKeys []string
	for _, p := range parts {
		p = catalog.CleanSecret(strings.TrimSpace(p))
		if p != "" {
			newKeys = append(newKeys, p)
		}
	}
	cred, _ := m.creds.Get(m.auth.draftID)
	existing := m.creds.AllKeys(m.auth.draftID)
	existing = append(existing, newKeys...)
	if len(existing) == 1 {
		cred.APIKey = existing[0]
		cred.APIKeys = nil
	} else {
		cred.APIKeys = existing
	}
	m.creds.Set(m.auth.draftID, cred)
	_ = m.creds.Save()
	m.reloadProviders()
	m.auth.statusLine = m.styles.Success.Render(fmt.Sprintf("  added %d key(s) for %s", len(newKeys), m.auth.draftID))
	return m, nil
}
