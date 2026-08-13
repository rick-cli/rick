package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"rick/internal/provider"
	"rick/internal/session"
)

func TestResumeBrowserFiltersMessagesCategoriesAndBookmarks(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sessions := []*session.Session{
		{
			ID: "roblox", Title: "Mining simulator", Category: "Roblox", Cwd: "G:/games",
			Updated: now, Messages: []provider.Message{provider.UserText("repair the ore respawn loop")},
		},
		{
			ID: "docs", Title: "Documentation", Category: "Writing", Cwd: "G:/docs",
			Updated: now.Add(-time.Hour), Messages: []provider.Message{provider.UserText("write the release notes")},
		},
	}
	for _, sess := range sessions {
		if err := store.Save(sess); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetFavorite("roblox", true); err != nil {
		t.Fatal(err)
	}

	metas, err := store.List("")
	if err != nil {
		t.Fatal(err)
	}
	m := &resumeModel{
		store:      store,
		metas:      metas,
		styles:     NewStyles(nil),
		legacyFavs: map[string]bool{},
		search:     newResumeInput(),
		editInput:  newResumeInput(),
	}

	m.search.SetValue("respawn")
	m.sortAndFilter()
	if len(m.filtered) != 1 || m.filtered[0].ID != "roblox" {
		t.Fatalf("message search returned %+v", m.filtered)
	}

	m.search.SetValue("")
	m.categoryFilter = "Writing"
	m.sortAndFilter()
	if len(m.filtered) != 1 || m.filtered[0].ID != "docs" {
		t.Fatalf("category filter returned %+v", m.filtered)
	}

	m.categoryFilter = ""
	m.favoriteOnly = true
	m.sortAndFilter()
	if len(m.filtered) != 1 || m.filtered[0].ID != "roblox" {
		t.Fatalf("bookmark filter returned %+v", m.filtered)
	}
}

// TestResumeSearchCacheByteBudget verifies the message-search cache evicts by
// total bytes, so one oversized transcript cannot evict every other entry and
// force a reload on the next keystroke.
func TestResumeSearchCacheByteBudget(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&session.Session{
		ID: "small", Title: "Small", Cwd: "G:/x",
		Messages: []provider.Message{provider.UserText("small needle")},
	}); err != nil {
		t.Fatal(err)
	}
	bigText := strings.Repeat("big haystack ", 2000) // ~24KB
	if err := store.Save(&session.Session{
		ID: "big", Title: "Big", Cwd: "G:/x",
		Messages: []provider.Message{provider.UserText(bigText)},
	}); err != nil {
		t.Fatal(err)
	}

	m := &resumeModel{store: store}
	m.cacheMessageSearchText("small", strings.Repeat("s", 100))
	m.cacheMessageSearchText("big", strings.Repeat("b", 24*1024))
	// Both must survive; a byte-agnostic entry-limit cache would drop small.
	if _, ok := m.messageSearchCache["small"]; !ok {
		t.Fatal("small entry evicted by a single large entry; byte budget broken")
	}
	if _, ok := m.messageSearchCache["big"]; !ok {
		t.Fatal("big entry missing")
	}
	if m.messageSearchCacheBytes <= 0 || m.messageSearchCacheBytes != len(m.messageSearchCache["small"])+len(m.messageSearchCache["big"]) {
		t.Fatalf("cache byte count out of sync: %d", m.messageSearchCacheBytes)
	}

	// Entry-limit eviction must also keep the byte count accurate: fill past
	// maxResumeMessageCacheEntries and confirm the byte counter tracks the
	// surviving entries.
	for i := 0; i < maxResumeMessageCacheEntries+8; i++ {
		id := fmt.Sprintf("filler-%d", i)
		m.cacheMessageSearchText(id, strings.Repeat("x", 32))
	}
	want := 0
	for _, text := range m.messageSearchCache {
		want += len(text)
	}
	if m.messageSearchCacheBytes != want {
		t.Fatalf("byte count after entry-limit evictions: got %d want %d", m.messageSearchCacheBytes, want)
	}
	if len(m.messageSearchCache) != maxResumeMessageCacheEntries {
		t.Fatalf("entry count after fill: got %d want %d", len(m.messageSearchCache), maxResumeMessageCacheEntries)
	}
}

// TestResumeSearchDebounce verifies live search defers message scans: a
// single keystroke schedules a tick instead of filtering inline, and the tick
// fires the filter once the debounce window has elapsed.
func TestResumeSearchDebounce(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&session.Session{
		ID: "a", Title: "Alpha", Cwd: "G:/x",
		Messages: []provider.Message{provider.UserText("needle alpha")},
	}); err != nil {
		t.Fatal(err)
	}
	metas, err := store.List("")
	if err != nil {
		t.Fatal(err)
	}
	m := &resumeModel{
		store:      store,
		metas:      metas,
		styles:     NewStyles(nil),
		editMode:   resumeEditSearch,
		search:     newResumeInput(),
		editInput:  newResumeInput(),
		legacyFavs: map[string]bool{},
	}

	// Typing a real query must not filter synchronously; it returns a tick cmd.
	m.editInput.SetValue("needl")
	m.editInput.CursorEnd()
	m.editInput.Focus()
	_, cmd := m.handleEditKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if m.search.Value() != "needle" {
		t.Fatalf("search value not updated: %q", m.search.Value())
	}
	if cmd == nil {
		t.Fatal("debounce: expected a tick cmd after typing")
	}
	// Force the debounce window to have elapsed, then deliver the tick.
	m.searchDebounce = time.Now().Add(-time.Second)
	next, _ := m.Update(searchDebounceMsg(time.Now()))
	rm := next.(*resumeModel)
	if len(rm.filtered) != 1 || rm.filtered[0].ID != "a" {
		t.Fatalf("debounced search filtered %+v, want session a", rm.filtered)
	}
}

func TestResumeBrowserKeyboardEditAndMouseSelection(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, sess := range []*session.Session{
		{ID: "first", Title: "First", Cwd: "G:/one", Updated: time.Now(), Messages: []provider.Message{provider.UserText("one")}},
		{ID: "second", Title: "Second", Cwd: "G:/two", Updated: time.Now().Add(-time.Minute), Messages: []provider.Message{provider.UserText("two")}},
	} {
		if err := store.Save(sess); err != nil {
			t.Fatal(err)
		}
	}
	metas, err := store.List("")
	if err != nil {
		t.Fatal(err)
	}
	m := &resumeModel{
		store:      store,
		metas:      metas,
		styles:     NewStyles(nil),
		legacyFavs: map[string]bool{},
		search:     newResumeInput(),
		editInput:  newResumeInput(),
	}
	m.sortAndFilter()
	m.width, m.height = 100, 24
	m.recalculateViewport()

	m.handleNormalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	m.editInput.SetValue("Renamed")
	m.handleEditKey(tea.KeyMsg{Type: tea.KeyEnter})
	renamed, err := store.Load(m.selected)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Title != "Renamed" {
		t.Fatalf("rename did not persist: %q", renamed.Title)
	}

	m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, Y: m.listTop + 1})
	if m.selected != "first" {
		t.Fatalf("mouse selected %q, want first (y=%d top=%d visible=%d list=%+v)", m.selected, m.listTop+1, m.listTop, m.visibleStart, m.filtered)
	}
	m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, Y: m.listTop})
	view := m.View()
	if view == "" || !containsAll(view, "RICK", "Renamed", "messages") {
		t.Fatalf("browser view is missing expected content:\n%s", view)
	}
}

func TestResumeBrowserCancelDoesNotResumeHighlightedSession(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&session.Session{ID: "highlighted", Title: "Highlighted", Updated: time.Now()}); err != nil {
		t.Fatal(err)
	}
	metas, err := store.List("")
	if err != nil {
		t.Fatal(err)
	}
	m := &resumeModel{
		store:      store,
		metas:      metas,
		styles:     NewStyles(nil),
		legacyFavs: map[string]bool{},
		search:     newResumeInput(),
		editInput:  newResumeInput(),
	}
	m.sortAndFilter()
	m.handleNormalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if m.selected != "highlighted" || m.resumeID != "" || !m.quit {
		t.Fatalf("cancel state is wrong: selected=%q resumeID=%q quit=%v", m.selected, m.resumeID, m.quit)
	}
}

func TestResumeBrowserButtonsAreClickable(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, sess := range []*session.Session{
		{ID: "first", Title: "First", Updated: time.Now()},
		{ID: "second", Title: "Second", Updated: time.Now().Add(-time.Minute)},
	} {
		if err := store.Save(sess); err != nil {
			t.Fatal(err)
		}
	}
	metas, err := store.List("")
	if err != nil {
		t.Fatal(err)
	}
	m := &resumeModel{
		store:      store,
		metas:      metas,
		styles:     NewStyles(nil),
		legacyFavs: map[string]bool{},
		search:     newResumeInput(),
		editInput:  newResumeInput(),
	}
	m.sortAndFilter()
	m.width, m.height = 100, 24
	m.recalculateViewport()
	m.View()

	button, ok := findResumeButton(m.bottomButtons, resumeButtonDown)
	if !ok {
		t.Fatal("down button was not rendered")
	}
	m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: button.x + 1, Y: button.y})
	if m.cursor != 1 {
		t.Fatalf("down button moved cursor to %d, want 1", m.cursor)
	}

	button, ok = findResumeButton(m.bottomButtons, resumeButtonSearch)
	if !ok {
		t.Fatal("search button was not rendered")
	}
	m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: button.x + 1, Y: button.y})
	if m.editMode != resumeEditSearch {
		t.Fatalf("search button set edit mode %d, want search", m.editMode)
	}

	m.editMode = resumeEditNone
	m.View()
	button, ok = findResumeButton(m.rightButtons, resumeButtonRename)
	if !ok {
		t.Fatal("right-pane rename button was not rendered")
	}
	m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: button.x + 1, Y: button.y})
	if m.editMode != resumeEditRename {
		t.Fatalf("right-pane rename button set edit mode %d, want rename", m.editMode)
	}
}

func findResumeButton(buttons []resumeButtonZone, id string) (resumeButtonZone, bool) {
	for _, button := range buttons {
		if button.id == id {
			return button, true
		}
	}
	return resumeButtonZone{}, false
}

func newResumeInput() textinput.Model {
	input := textinput.New()
	input.Prompt = ""
	return input
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
