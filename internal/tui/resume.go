package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"rick/internal/config"
	"rick/internal/session"
)

// sortMode controls how sessions are ordered. Favorites always stay above
// regular sessions; the selected mode controls the order inside each group.
type sortMode int

const (
	sortDate sortMode = iota
	sortMessages
	sortTitle
	sortCategory
)

func (s sortMode) String() string {
	switch s {
	case sortDate:
		return "recent"
	case sortMessages:
		return "messages"
	case sortTitle:
		return "title"
	case sortCategory:
		return "category"
	default:
		return "?"
	}
}

type resumeEditMode int

const (
	resumeEditNone resumeEditMode = iota
	resumeEditSearch
	resumeEditCategoryFilter
	resumeEditRename
	resumeEditCategory
)

// searchDebounceInterval paces live search: filtering runs only after the
// user pauses typing, so a fast typist pays for one message-text pass instead
// of one per keystroke (the first pass may parse sidecar-less sessions).
const searchDebounceInterval = 150 * time.Millisecond

type searchDebounceMsg time.Time

type resumeButtonSpec struct {
	id    string
	label string
}

type resumeButtonZone struct {
	id string
	x  int
	y  int
	w  int
}

const (
	resumeButtonUp        = "up"
	resumeButtonDown      = "down"
	resumeButtonResume    = "resume"
	resumeButtonSearch    = "search"
	resumeButtonFilter    = "filter"
	resumeButtonCategory  = "category"
	resumeButtonRename    = "rename"
	resumeButtonFavorite  = "favorite"
	resumeButtonSort      = "sort"
	resumeButtonFavorites = "favorites"
	resumeButtonDelete    = "delete"
	resumeButtonHelp      = "help"
	resumeButtonQuit      = "quit"
)

// resumeModel is the full-screen session browser used by `rick resume`.
// It deliberately owns only browser state; session persistence remains in
// internal/session so the main chat UI and the browser share one data model.
type resumeModel struct {
	store    *session.Store
	metas    []session.Meta
	filtered []session.Meta

	messageSearchCache      map[string]string
	messageSearchCacheOrder []string
	messageSearchCacheBytes int
	searchDebounce         time.Time

	cursor       int
	width        int
	height       int
	visibleStart int
	listTop      int
	listHeight   int
	styles       *Styles
	selected     string
	resumeID     string
	quit         bool
	currentID    string

	search         textinput.Model
	editInput      textinput.Model
	editMode       resumeEditMode
	categoryFilter string
	favoriteOnly   bool

	gotoMode  bool
	gotoInput textinput.Model
	sortMode  sortMode

	// favorites.json was used by an earlier browser build. Keep reading it so
	// existing bookmarks survive the migration to the session metadata field.
	legacyFavs map[string]bool
	favPath    string

	deleteConfirm bool
	showHelp      bool
	statusMsg     string
	statusTime    time.Time
	lastClickAt   time.Time
	lastClickRow  int

	bottomButtons []resumeButtonZone
	rightButtons  []resumeButtonZone
	rightPanelX   int
	rightActionsY int
}

// newResumeModel builds the shared interactive session browser used by both
// the standalone `rick resume` command and the in-app `/sessions` command.
func newResumeModel(styles *Styles) (*resumeModel, error) {
	store, err := session.NewStore(filepath.Join(config.DataDir(), "sessions"))
	if err != nil {
		return nil, err
	}

	metas, err := store.List("")
	if err != nil {
		return nil, err
	}

	favPath := filepath.Join(config.DataDir(), "favorites.json")
	legacyFavs := loadFavs(favPath)
	search := textinput.New()
	search.Placeholder = "search title, project, model, category, or message"
	search.CharLimit = 160
	search.Prompt = ""
	search.Width = 40

	edit := textinput.New()
	edit.CharLimit = 160
	edit.Prompt = ""
	edit.Width = 44

	gotoInput := textinput.New()
	gotoInput.Placeholder = "session #"
	gotoInput.CharLimit = 8
	gotoInput.Prompt = ""
	gotoInput.Width = 12

	cwd, _ := os.Getwd()
	m := &resumeModel{
		store:      store,
		metas:      metas,
		filtered:   append([]session.Meta(nil), metas...),
		styles:     styles,
		search:     search,
		editInput:  edit,
		gotoInput:  gotoInput,
		legacyFavs: legacyFavs,
		favPath:    favPath,
		currentID:  store.GetCurrent(cwd),
		sortMode:   sortDate,
	}
	m.sortAndFilter()

	// Backfill search sidecars for sessions saved before the feature, off the
	// UI goroutine: the first message search would otherwise load each of
	// those full JSON files inline. The browser reads sidecars when present,
	// so results converge as files land.
	go backfillSearchSidecars(store, metas)

	return m, nil
}

// ResumeSessions launches the interactive session browser and returns the id
// chosen for resuming. An empty id means the user cancelled or quit.
func ResumeSessions(styles *Styles) (string, error) {
	m, err := newResumeModel(styles)
	if err != nil {
		return "", err
	}

	// Resume is explicitly a mouse-capable screen, independent of the main
	// chat's mouse preference. WithMouseCellMotion also works in Windows
	// Terminal and lets the browser receive wheel and click events.
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		return "", err
	}
	return m.resumeID, nil
}

func (m *resumeModel) Init() tea.Cmd { return nil }

func (m *resumeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.recalculateViewport()
		return m, nil
	case searchDebounceMsg:
		// Only run the filter if the query has not changed since the tick
		// was scheduled — later keystrokes supersede this one.
		if m.editMode == resumeEditSearch && !time.Time(msg).IsZero() && !m.searchDebounce.IsZero() && !time.Now().Before(m.searchDebounce) {
			m.searchDebounce = time.Time{}
			m.sortAndFilter()
		}
		return m, nil
	case tea.MouseMsg:
		return m, m.handleMouse(msg)
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.quit = true
			return m, tea.Quit
		}
		if time.Since(m.statusTime) > 4*time.Second {
			m.statusMsg = ""
		}
		if m.deleteConfirm {
			return m.handleDeleteKey(msg)
		}
		if m.showHelp {
			if msg.String() == "esc" || msg.String() == "q" || msg.String() == "?" || msg.String() == "enter" {
				m.showHelp = false
			}
			return m, nil
		}
		if m.gotoMode {
			return m.handleGotoKey(msg)
		}
		if m.editMode != resumeEditNone {
			return m.handleEditKey(msg)
		}
		return m.handleNormalKey(msg)
	}
	return m, nil
}

func (m *resumeModel) handleNormalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc":
		if m.search.Value() != "" || m.categoryFilter != "" || m.favoriteOnly {
			m.search.SetValue("")
			m.categoryFilter = ""
			m.favoriteOnly = false
			m.sortAndFilter()
			m.setStatus("filters cleared")
			return m, nil
		}
		m.quit = true
		return m, tea.Quit
	case "enter":
		return m.resumeSelected()
	case "down", "j":
		m.moveCursor(1)
	case "up", "k":
		m.moveCursor(-1)
	case "pgdown", "ctrl+f":
		m.moveCursor(m.pageStep())
	case "pgup", "ctrl+b":
		m.moveCursor(-m.pageStep())
	case "home":
		m.cursor = 0
		m.refreshSelection()
	case "end":
		m.cursor = len(m.filtered) - 1
		m.refreshSelection()
	case "/":
		m.beginEdit(resumeEditSearch, "filter sessions")
	case "c":
		m.beginEdit(resumeEditCategoryFilter, "category filter (blank = all)")
	case "C":
		if len(m.filtered) > 0 {
			m.beginEdit(resumeEditCategory, "set category (blank = uncategorized)")
		}
	case "r":
		if len(m.filtered) > 0 {
			m.beginEdit(resumeEditRename, "rename session")
			m.editInput.SetValue(m.selectedMeta().Title)
			m.editInput.CursorEnd()
		}
	case "g":
		m.gotoMode = true
		m.gotoInput.SetValue("")
		return m, m.gotoInput.Focus()
	case "s":
		m.sortMode = (m.sortMode + 1) % 4
		m.sortAndFilter()
		m.setStatus("sort: " + m.sortMode.String())
	case "f":
		m.toggleFavorite()
	case "v":
		m.favoriteOnly = !m.favoriteOnly
		m.sortAndFilter()
		m.setStatus(filterLabel(m.favoriteOnly, m.categoryFilter))
	case "d", "x":
		if len(m.filtered) > 0 {
			m.deleteConfirm = true
		}
	case "?", "h":
		m.showHelp = true
	}
	return m, nil
}

func (m *resumeModel) handleEditKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "esc":
		m.editMode = resumeEditNone
		m.editInput.Blur()
		m.editInput.SetValue("")
		return m, nil
	case "enter":
		value := strings.TrimSpace(m.editInput.Value())
		mode := m.editMode
		m.editMode = resumeEditNone
		m.editInput.Blur()
		m.editInput.SetValue("")
		switch mode {
		case resumeEditSearch:
			m.search.SetValue(value)
			m.sortAndFilter()
		case resumeEditCategoryFilter:
			m.categoryFilter = value
			m.sortAndFilter()
		case resumeEditRename:
			if value == "" {
				m.setStatus("title cannot be empty")
				return m, nil
			}
			if err := m.store.Rename(m.selected, value); err != nil {
				m.setStatus("rename failed: " + err.Error())
				return m, nil
			}
			m.reload("renamed session")
		case resumeEditCategory:
			if err := m.store.SetCategory(m.selected, value); err != nil {
				m.setStatus("category failed: " + err.Error())
				return m, nil
			}
			if value == "" {
				value = "uncategorized"
			}
			m.reload("category: " + value)
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.editInput, cmd = m.editInput.Update(msg)
	if m.editMode == resumeEditSearch {
		m.search.SetValue(m.editInput.Value())
		// Debounce live search so each keystroke does not re-scan every
		// session's message text. The result still renders once the pause
		// elapses; enter runs it immediately.
		if len(strings.TrimSpace(m.search.Value())) < 2 {
			m.sortAndFilter()
		} else {
			m.searchDebounce = time.Now()
			cmd = tea.Batch(cmd, tea.Tick(searchDebounceInterval, func(t time.Time) tea.Msg {
				return searchDebounceMsg(t)
			}))
		}
	}
	return m, cmd
}

func (m *resumeModel) beginEdit(mode resumeEditMode, placeholder string) {
	m.editMode = mode
	m.editInput.Placeholder = placeholder
	m.editInput.SetValue("")
	m.editInput.Focus()
	if mode == resumeEditSearch {
		m.editInput.SetValue(m.search.Value())
		m.editInput.CursorEnd()
	}
}

func (m *resumeModel) handleGotoKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.gotoMode = false
		m.gotoInput.Blur()
		m.gotoInput.SetValue("")
	case "enter":
		raw := m.gotoInput.Value()
		m.gotoMode = false
		m.gotoInput.Blur()
		m.gotoInput.SetValue("")
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || n < 1 || n > len(m.filtered) {
			m.setStatus("invalid session number")
			return m, nil
		}
		m.cursor = n - 1
		m.refreshSelection()
		return m.resumeSelected()
	default:
		var cmd tea.Cmd
		m.gotoInput, cmd = m.gotoInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *resumeModel) handleDeleteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch strings.ToLower(msg.String()) {
	case "y", "enter":
		id := m.selected
		if err := m.store.Delete(id); err != nil {
			m.setStatus("delete failed: " + err.Error())
		} else {
			delete(m.legacyFavs, id)
			saveFavs(m.favPath, m.legacyFavs)
			m.reload("session deleted")
		}
		m.deleteConfirm = false
	case "n", "esc", "q":
		m.deleteConfirm = false
	}
	return m, nil
}

func (m *resumeModel) activateButton(id string) (tea.Model, tea.Cmd) {
	switch id {
	case resumeButtonUp:
		m.moveCursor(-1)
	case resumeButtonDown:
		m.moveCursor(1)
	case resumeButtonResume:
		return m.resumeSelected()
	case resumeButtonSearch:
		m.beginEdit(resumeEditSearch, "filter sessions")
	case resumeButtonFilter:
		m.beginEdit(resumeEditCategoryFilter, "category filter (blank = all)")
	case resumeButtonCategory:
		if len(m.filtered) > 0 {
			m.beginEdit(resumeEditCategory, "set category (blank = uncategorized)")
		}
	case resumeButtonRename:
		if len(m.filtered) > 0 {
			m.beginEdit(resumeEditRename, "rename session")
			m.editInput.SetValue(m.selectedMeta().Title)
			m.editInput.CursorEnd()
		}
	case resumeButtonFavorite:
		m.toggleFavorite()
	case resumeButtonSort:
		m.sortMode = (m.sortMode + 1) % 4
		m.sortAndFilter()
		m.setStatus("sort: " + m.sortMode.String())
	case resumeButtonFavorites:
		m.favoriteOnly = !m.favoriteOnly
		m.sortAndFilter()
		m.setStatus(filterLabel(m.favoriteOnly, m.categoryFilter))
	case resumeButtonDelete:
		if len(m.filtered) > 0 {
			m.deleteConfirm = true
		}
	case resumeButtonHelp:
		m.showHelp = true
	case resumeButtonQuit:
		m.quit = true
		return m, tea.Quit
	}
	return m, nil
}

func (m *resumeModel) buttonAt(x, y int) (string, bool) {
	for _, button := range m.bottomButtons {
		if y == button.y && x >= button.x && x < button.x+button.w {
			return button.id, true
		}
	}
	for _, button := range m.rightButtons {
		if y == button.y && x >= button.x && x < button.x+button.w {
			return button.id, true
		}
	}
	return "", false
}

func (m *resumeModel) buttonStyle(id string) lipgloss.Style {
	c := m.styles.Theme().Color
	background := c("border")
	foreground := c("text")
	if id == resumeButtonResume || id == resumeButtonHelp {
		background = c("borderActive")
		foreground = c("background")
	}
	return lipgloss.NewStyle().Foreground(foreground).Background(background).Bold(true).Padding(0, 1)
}

func (m *resumeModel) renderButtonRow(specs []resumeButtonSpec, y, startX, maxWidth int, rightPane bool) string {
	if rightPane {
		m.rightButtons = nil
	} else if y == m.height-2 {
		m.bottomButtons = nil
	}
	var rendered []string
	x := startX
	for _, spec := range specs {
		button := m.buttonStyle(spec.id).Render(spec.label)
		buttonWidth := lipgloss.Width(button)
		if x > startX && x+buttonWidth > startX+maxWidth {
			break
		}
		zone := resumeButtonZone{id: spec.id, x: x, y: y, w: buttonWidth}
		if rightPane {
			m.rightButtons = append(m.rightButtons, zone)
		} else {
			m.bottomButtons = append(m.bottomButtons, zone)
		}
		rendered = append(rendered, button)
		x += buttonWidth + 1
	}
	return strings.Join(rendered, " ")
}

func (m *resumeModel) handleMouse(msg tea.MouseMsg) tea.Cmd {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.moveCursor(-3)
	case tea.MouseButtonWheelDown:
		m.moveCursor(3)
	case tea.MouseButtonLeft:
		if msg.Action != tea.MouseActionPress || m.showHelp || m.deleteConfirm {
			return nil
		}
		if id, ok := m.buttonAt(msg.X, msg.Y); ok {
			_, cmd := m.activateButton(id)
			return cmd
		}
		if m.editMode != resumeEditNone {
			return nil
		}
		if msg.Y == 1 {
			m.beginEdit(resumeEditSearch, "filter sessions")
			return nil
		}
		row := msg.Y - m.listTop
		if row < 0 || row >= m.listHeight {
			return nil
		}
		index := m.visibleStart + row
		if index < 0 || index >= len(m.filtered) {
			return nil
		}
		now := time.Now()
		doubleClick := index == m.lastClickRow && now.Sub(m.lastClickAt) < 450*time.Millisecond
		m.lastClickAt, m.lastClickRow = now, index
		m.cursor = index
		m.refreshSelection()
		if doubleClick {
			_, cmd := m.resumeSelected()
			return cmd
		}
	}
	return nil
}

func (m *resumeModel) moveCursor(delta int) {
	if len(m.filtered) == 0 {
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	m.refreshSelection()
}

func (m *resumeModel) pageStep() int {
	step := m.listHeight - 2
	if step < 3 {
		step = 3
	}
	return step
}

func (m *resumeModel) resumeSelected() (tea.Model, tea.Cmd) {
	if len(m.filtered) == 0 || m.cursor < 0 || m.cursor >= len(m.filtered) {
		return m, nil
	}
	m.selected = m.filtered[m.cursor].ID
	m.resumeID = m.selected
	m.quit = true
	return m, tea.Quit
}

func (m *resumeModel) toggleFavorite() {
	if len(m.filtered) == 0 {
		return
	}
	meta := m.selectedMeta()
	next := !m.isFavorite(meta)
	if err := m.store.SetFavorite(meta.ID, next); err != nil {
		m.setStatus("bookmark failed: " + err.Error())
		return
	}
	if next {
		m.legacyFavs[meta.ID] = true
	} else {
		delete(m.legacyFavs, meta.ID)
	}
	saveFavs(m.favPath, m.legacyFavs)
	m.reload(map[bool]string{true: "bookmarked ★", false: "bookmark removed"}[next])
}

func (m *resumeModel) reload(status string) {
	metas, err := m.store.List("")
	if err != nil {
		m.setStatus("reload failed: " + err.Error())
		return
	}
	m.metas = metas
	m.sortAndFilter()
	m.setStatus(status)
}

func (m *resumeModel) refreshSelection() {
	if len(m.filtered) == 0 {
		m.selected = ""
		return
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	m.selected = m.filtered[m.cursor].ID
}

func (m *resumeModel) selectedMeta() session.Meta {
	if len(m.filtered) == 0 || m.cursor < 0 || m.cursor >= len(m.filtered) {
		return session.Meta{}
	}
	return m.filtered[m.cursor]
}

func (m *resumeModel) isFavorite(meta session.Meta) bool {
	return meta.Favorite || m.legacyFavs[meta.ID]
}

func (m *resumeModel) sortAndFilter() {
	sort.SliceStable(m.metas, func(i, j int) bool {
		left, right := m.metas[i], m.metas[j]
		lf, rf := m.isFavorite(left), m.isFavorite(right)
		if lf != rf {
			return lf
		}
		switch m.sortMode {
		case sortMessages:
			if left.Messages != right.Messages {
				return left.Messages > right.Messages
			}
		case sortTitle:
			if strings.ToLower(left.Title) != strings.ToLower(right.Title) {
				return strings.ToLower(left.Title) < strings.ToLower(right.Title)
			}
		case sortCategory:
			if strings.ToLower(left.Category) != strings.ToLower(right.Category) {
				return strings.ToLower(left.Category) < strings.ToLower(right.Category)
			}
		}
		return left.Updated.After(right.Updated)
	})

	query := strings.ToLower(strings.TrimSpace(m.search.Value()))
	category := strings.ToLower(strings.TrimSpace(m.categoryFilter))
	results := make([]session.Meta, 0, len(m.metas))
	for _, meta := range m.metas {
		if m.favoriteOnly && !m.isFavorite(meta) {
			continue
		}
		if category != "" && !strings.EqualFold(strings.TrimSpace(meta.Category), category) {
			continue
		}
		if query != "" && !m.matchesQuery(meta, query) {
			continue
		}
		results = append(results, meta)
	}
	m.filtered = results
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.refreshSelection()
}

func (m *resumeModel) matchesQuery(meta session.Meta, query string) bool {
	fields := []string{meta.Title, meta.Cwd, meta.Model, meta.Category, meta.ID, meta.LastPrompt}
	for _, field := range fields {
		lower := strings.ToLower(field)
		if strings.Contains(lower, query) {
			return true
		}
		if _, ok := fuzzyMatch(query, lower); ok {
			return true
		}
	}
	// Metadata search is cheap for every keystroke. Only open a transcript
	// when its metadata did not match, so message search remains complete
	// without making the common case expensive.
	return strings.Contains(m.cachedMessageSearchText(meta.ID), query)
}

const (
	maxResumeMessageCacheEntries = 64
	maxResumeMessageCacheBytes   = 4 << 20 // 4 MiB total; big transcripts must not thrash the cache
)

func (m *resumeModel) cachedMessageSearchText(id string) string {
	if m.messageSearchCache != nil {
		if text, ok := m.messageSearchCache[id]; ok {
			return text
		}
	}

	text := m.messageSearchSidecar(id)
	if text == "" {
		// No sidecar (legacy session): fall back to loading the full JSON.
		sess, err := m.store.Load(id)
		if err != nil {
			return ""
		}
		var builder strings.Builder
		for _, message := range sess.Messages {
			for _, block := range message.Content {
				if block.Type == "text" {
					builder.WriteString(block.Text)
				}
			}
		}
		text = strings.ToLower(builder.String())
	}
	m.cacheMessageSearchText(id, text)
	return text
}

// cacheMessageSearchText stores a session's search text, evicting by total
// bytes (not just entry count) so a few multi-hundred-KB transcripts cannot
// evict everything else and force reloads on the next keystroke.
func (m *resumeModel) cacheMessageSearchText(id, text string) {
	if len(text) > maxResumeMessageCacheBytes {
		return // never cache a single transcript this large
	}
	if m.messageSearchCache == nil {
		m.messageSearchCache = make(map[string]string)
	}
	if _, exists := m.messageSearchCache[id]; exists {
		return
	}
	if len(m.messageSearchCacheOrder) >= maxResumeMessageCacheEntries {
		oldest := m.messageSearchCacheOrder[0]
		m.messageSearchCacheOrder = m.messageSearchCacheOrder[1:]
		if dropped, ok := m.messageSearchCache[oldest]; ok {
			m.messageSearchCacheBytes -= len(dropped)
		}
		delete(m.messageSearchCache, oldest)
	}
	// Evict cached entries until this one fits the byte budget.
	for m.messageSearchCacheBytes+len(text) > maxResumeMessageCacheBytes && len(m.messageSearchCacheOrder) > 0 {
		oldest := m.messageSearchCacheOrder[0]
		m.messageSearchCacheOrder = m.messageSearchCacheOrder[1:]
		if dropped, ok := m.messageSearchCache[oldest]; ok {
			m.messageSearchCacheBytes -= len(dropped)
		}
		delete(m.messageSearchCache, oldest)
	}
	m.messageSearchCache[id] = text
	m.messageSearchCacheBytes += len(text)
	m.messageSearchCacheOrder = append(m.messageSearchCacheOrder, id)
}

// messageSearchSidecar reads the store's lightweight .search.txt sidecar for
// a session, avoiding a full-session JSON unmarshal on every search keystroke.
func (m *resumeModel) messageSearchSidecar(id string) string {
	data, err := os.ReadFile(m.store.SearchPath(id))
	if err != nil {
		return ""
	}
	return string(data)
}

// backfillSearchSidecars writes .search.txt sidecars for every session that
// lacks one, loading each session's JSON once. Running in the background keeps
// the first interactive search keystroke from paying for the full backfill.
func backfillSearchSidecars(store *session.Store, metas []session.Meta) {
	for _, meta := range metas {
		if store.HasSearchText(meta.ID) {
			continue
		}
		sess, err := store.Load(meta.ID)
		if err != nil {
			continue
		}
		_ = store.WriteSearchText(meta.ID, session.SearchTextOf(sess))
	}
}

func (m *resumeModel) recalculateViewport() {
	if m.width < 1 || m.height < 1 {
		return
	}
	// Two header rows and three footer rows leave the split panels the rest.
	panelHeight := m.height - 5
	if panelHeight < 5 {
		panelHeight = 5
	}
	m.listHeight = panelHeight - 2 // rounded border top/bottom
	if m.listHeight < 1 {
		m.listHeight = 1
	}
	m.listTop = 3 // header, filter row, panel border
	m.visibleStart = 0
	if len(m.filtered) > m.listHeight {
		m.visibleStart = m.cursor - m.listHeight/2
		if m.visibleStart < 0 {
			m.visibleStart = 0
		}
		if m.visibleStart+m.listHeight > len(m.filtered) {
			m.visibleStart = len(m.filtered) - m.listHeight
		}
	}
}

func (m *resumeModel) setStatus(text string) {
	m.statusMsg = text
	m.statusTime = time.Now()
}

func (m *resumeModel) View() string {
	if m.quit || m.height == 0 {
		return ""
	}
	m.recalculateViewport()
	s := m.styles
	if s == nil {
		s = NewStyles(nil)
	}

	var out strings.Builder
	title := s.Accent.Bold(true).Render(" RICK ") + s.Base.Render("/ resume")
	count := s.Muted.Render(fmt.Sprintf("%d shown · %d total", len(m.filtered), len(m.metas)))
	right := s.Faint.Render("sort: " + m.sortMode.String())
	topGap := m.width - lipgloss.Width(title) - lipgloss.Width(count) - lipgloss.Width(right) - 4
	if topGap < 1 {
		topGap = 1
	}
	out.WriteString(title + strings.Repeat(" ", topGap) + count + "  " + right + "\n")

	filterText := m.filterSummary()
	if m.editMode == resumeEditSearch || m.editMode == resumeEditCategoryFilter || m.editMode == resumeEditRename || m.editMode == resumeEditCategory {
		label := editLabel(m.editMode)
		out.WriteString("  " + s.Primary.Render("▸ ") + s.Muted.Render(label+": ") + m.editInput.View() + "\n")
	} else if filterText != "" {
		out.WriteString("  " + s.Faint.Render("filters: ") + s.Muted.Render(filterText) + "\n")
	} else {
		out.WriteString("  " + s.Faint.Render("all sessions · press / to search") + "\n")
	}

	if m.showHelp {
		out.WriteString(m.helpView())
	} else {
		out.WriteString(m.panelsView())
	}

	status := ""
	if m.deleteConfirm {
		status = s.Warning.Render("Delete " + truncate(m.selectedMeta().Title, 40) + "? [y]es / [n]o")
	} else if m.statusMsg != "" && time.Since(m.statusTime) < 4*time.Second {
		status = s.Warning.Render(m.statusMsg) + "  " + s.Faint.Render("click a button or use its shortcut")
	} else {
		status = s.Faint.Render("Click a button below, or use the matching keyboard shortcut")
	}
	m.bottomButtons = nil
	out.WriteString(truncate(status, m.width) + "\n")
	out.WriteString(m.renderButtonRow([]resumeButtonSpec{
		{id: resumeButtonUp, label: "↑ Up"},
		{id: resumeButtonDown, label: "↓ Down"},
		{id: resumeButtonResume, label: "↵ Resume"},
		{id: resumeButtonSearch, label: "/ Search"},
		{id: resumeButtonFilter, label: "c Filter"},
		{id: resumeButtonCategory, label: "C Category"},
	}, m.height-2, 1, m.width-2, false) + "\n")
	out.WriteString(m.renderButtonRow([]resumeButtonSpec{
		{id: resumeButtonRename, label: "r Rename"},
		{id: resumeButtonFavorite, label: "f Bookmark"},
		{id: resumeButtonSort, label: "s Sort"},
		{id: resumeButtonFavorites, label: "v Saved"},
		{id: resumeButtonDelete, label: "d Delete"},
		{id: resumeButtonHelp, label: "? Help"},
		{id: resumeButtonQuit, label: "q Quit"},
	}, m.height-1, 1, m.width-2, false))
	return out.String()
}

func (m *resumeModel) panelsView() string {
	s := m.styles
	panelHeight := m.height - 5
	if panelHeight < 5 {
		panelHeight = 5
	}
	leftWidth := m.width * 58 / 100
	if leftWidth < 34 {
		leftWidth = 34
	}
	if leftWidth > m.width-24 {
		leftWidth = m.width - 24
	}
	if leftWidth < 1 {
		leftWidth = m.width
	}
	rightWidth := m.width - leftWidth
	if rightWidth < 24 {
		rightWidth = 24
		leftWidth = m.width - rightWidth
	}
	if leftWidth < 1 {
		leftWidth = 1
	}

	leftBody := m.listView(leftWidth - 4)
	leftPanel := s.Panel.Width(leftWidth-2).Height(panelHeight-2).Border(lipgloss.RoundedBorder()).BorderForeground(s.Theme().Color("border")).Padding(0, 1).Render(leftBody)
	m.rightPanelX = lipgloss.Width(leftPanel)
	rightBody := m.detailView(rightWidth - 4)
	rightPanel := s.Panel.Width(rightWidth-2).Height(panelHeight-2).Border(lipgloss.RoundedBorder()).BorderForeground(s.Theme().Color("borderActive")).Padding(0, 1).Render(rightBody)
	return lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, rightPanel) + "\n"
}

func (m *resumeModel) listView(width int) string {
	s := m.styles
	if width < 10 {
		width = 10
	}
	var b strings.Builder
	if len(m.filtered) == 0 {
		b.WriteString("\n")
		b.WriteString(s.Muted.Render("No sessions match."))
		b.WriteString("\n\n")
		b.WriteString(s.Faint.Render("Try clearing the filters with esc."))
		return b.String()
	}

	numberWidth := len(strconv.Itoa(len(m.filtered)))
	if numberWidth < 2 {
		numberWidth = 2
	}
	end := m.visibleStart + m.listHeight
	if end > len(m.filtered) {
		end = len(m.filtered)
	}
	for i := m.visibleStart; i < end; i++ {
		meta := m.filtered[i]
		marker := "  "
		if i == m.cursor {
			marker = "▸ "
		}
		bookmark := "  "
		if m.isFavorite(meta) {
			bookmark = "★ "
		}
		title := meta.Title
		if strings.TrimSpace(title) == "" {
			title = "(untitled)"
		}
		category := strings.TrimSpace(meta.Category)
		if category == "" {
			category = "uncategorized"
		}
		titleWidth := width - numberWidth - 13
		if titleWidth < 10 {
			titleWidth = 10
		}
		title = truncate(title, titleWidth)
		info := fmt.Sprintf("%d  %s", meta.Messages, humanAge(meta.Updated))
		plain := fmt.Sprintf("%*d %s%s%-*s %s", numberWidth, i+1, marker, bookmark, max(1, titleWidth), title, info)
		if lipgloss.Width(plain) > width {
			plain = truncate(plain, width)
		}
		if i == m.cursor {
			b.WriteString(s.Base.Bold(true).Render(plain))
		} else {
			b.WriteString(plain)
		}
		b.WriteString("\n")
		if category != "uncategorized" && i == m.cursor {
			b.WriteString("   " + s.Faint.Render("· "+truncate(category, width-5)) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *resumeModel) detailView(width int) string {
	s := m.styles
	m.rightButtons = nil
	if width < 12 {
		width = 12
	}
	if len(m.filtered) == 0 {
		return s.Muted.Render("Select a session to inspect it.")
	}
	meta := m.selectedMeta()
	title := meta.Title
	if title == "" {
		title = "(untitled)"
	}
	category := meta.Category
	if category == "" {
		category = "uncategorized"
	}
	var b strings.Builder
	b.WriteString(s.Accent.Bold(true).Render(truncate(title, width)))
	b.WriteString("\n")
	b.WriteString(s.Faint.Render(strings.Repeat("─", max(1, width))) + "\n")
	b.WriteString(s.Muted.Render("category ") + s.Base.Render(truncate(category, width-9)) + "\n")
	b.WriteString(s.Muted.Render("messages ") + s.Base.Render(strconv.Itoa(meta.Messages)) + "\n")
	b.WriteString(s.Muted.Render("model    ") + s.Base.Render(truncate(meta.Model, width-11)) + "\n")
	b.WriteString(s.Muted.Render("project  ") + s.Base.Render(truncate(meta.Cwd, width-11)) + "\n")
	b.WriteString(s.Muted.Render("updated  ") + s.Base.Render(meta.Updated.Format("2006-01-02 15:04")) + "\n")
	b.WriteString(s.Muted.Render("created  ") + s.Base.Render(meta.Created.Format("2006-01-02 15:04")) + "\n")
	if m.isFavorite(meta) {
		b.WriteString(s.Warning.Render("★ bookmarked") + "\n")
	}
	if meta.ID == m.currentID {
		b.WriteString(s.Success.Render("● last active here") + "\n")
	}
	if meta.LastPrompt != "" {
		b.WriteString("\n" + s.Faint.Render("last prompt") + "\n")
		b.WriteString(s.Base.Render(wrapPreview(meta.LastPrompt, width, 5)))
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	lines = append(lines, "")
	m.rightActionsY = 3 + len(lines)
	actions := m.renderButtonRow([]resumeButtonSpec{
		{id: resumeButtonResume, label: "↵ Resume"},
		{id: resumeButtonRename, label: "r Rename"},
		{id: resumeButtonCategory, label: "C Cat."},
	}, m.rightActionsY, m.rightPanelX+2, width, true)
	if actions != "" {
		lines = append(lines, actions)
	}
	return strings.Join(lines, "\n")
}

func (m *resumeModel) helpView() string {
	s := m.styles
	body := []string{
		s.Accent.Bold(true).Render("Session browser help"),
		"",
		"↑/↓ j/k       move through sessions",
		"enter         resume selected session",
		"mouse click   select · double-click resume",
		"mouse wheel   scroll the list",
		"/             search titles, metadata, and messages",
		"c             filter by category",
		"C             categorize selected session",
		"r             rename selected session",
		"f             bookmark / unbookmark",
		"v             show bookmarks only",
		"s             cycle sort order",
		"d / x         delete (with confirmation)",
		"g             resume by visible number",
		"esc           clear filters / close help",
		"",
		s.Faint.Render("Press any listed key, or esc to return."),
	}
	panelWidth := m.width - 8
	if panelWidth < 30 {
		panelWidth = 30
	}
	return s.Overlay.Width(panelWidth).Render(strings.Join(body, "\n")) + "\n"
}

func (m *resumeModel) filterSummary() string {
	var parts []string
	if value := strings.TrimSpace(m.search.Value()); value != "" {
		parts = append(parts, "search="+value)
	}
	if m.categoryFilter != "" {
		parts = append(parts, "category="+m.categoryFilter)
	}
	if m.favoriteOnly {
		parts = append(parts, "bookmarks")
	}
	return strings.Join(parts, " · ")
}

func filterLabel(favoriteOnly bool, category string) string {
	if favoriteOnly {
		return "showing bookmarks"
	}
	if category != "" {
		return "category: " + category
	}
	return "showing all sessions"
}

func editLabel(mode resumeEditMode) string {
	switch mode {
	case resumeEditSearch:
		return "search"
	case resumeEditCategoryFilter:
		return "category"
	case resumeEditRename:
		return "rename"
	case resumeEditCategory:
		return "set category"
	default:
		return "input"
	}
}

func wrapPreview(text string, width, lines int) string {
	text = strings.Join(strings.Fields(text), " ")
	if width < 10 {
		width = 10
	}
	limit := width * lines
	if len([]rune(text)) > limit {
		text = string([]rune(text)[:limit]) + "…"
	}
	var out []string
	runes := []rune(text)
	for len(runes) > 0 {
		n := width
		if n > len(runes) {
			n = len(runes)
		}
		out = append(out, string(runes[:n]))
		runes = runes[n:]
		if len(out) == lines {
			break
		}
	}
	return strings.Join(out, "\n")
}

func loadFavs(path string) map[string]bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]bool{}
	}
	var favs map[string]bool
	_ = json.Unmarshal(data, &favs)
	if favs == nil {
		favs = map[string]bool{}
	}
	return favs
}

func saveFavs(path string, favs map[string]bool) {
	data, err := json.MarshalIndent(favs, "", "  ")
	if err == nil {
		_ = os.WriteFile(path, data, 0o644)
	}
}
