package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"rick/internal/config"
)

// TestAuthListSortedWithAddProviderFirst pins the list ordering: "+ Add
// Provider" is always row 0, connected providers sort A-Z after it, and the
// remaining catalog entries sort A-Z too.
func TestAuthListSortedWithAddProviderFirst(t *testing.T) {
	m := newModelChoiceTestModel()
	m.creds = &config.Credentials{Providers: map[string]config.Credential{
		"zz-last":  {BaseURL: "https://zz.example.com"},
		"aa-first": {BaseURL: "https://aa.example.com"},
	}}
	m.deps.Loaded = &config.Loaded{Config: config.Config{Providers: map[string]config.Provider{}}}
	m.rebuildAuthRows()

	if len(m.auth.rows) == 0 {
		t.Fatal("auth rows empty")
	}
	if !m.auth.rows[0].addProvider {
		t.Fatalf("row 0 must be + Add Provider, got %q", m.auth.rows[0].label)
	}
	// The two connected providers must come right after the sentinel, A-Z.
	if len(m.auth.rows) < 3 || m.auth.rows[1].id != "aa-first" || m.auth.rows[2].id != "zz-last" {
		t.Fatalf("connected providers not A-Z after sentinel: %+v", m.auth.rows[:min(3, len(m.auth.rows))])
	}
	// Every remaining run must itself be sorted A-Z.
	for i := 1; i < len(m.auth.rows); i++ {
		// A new run starts where a connected provider is followed by an
		// available one (or the reverse) — only compare within runs.
		if i >= 2 && m.auth.rows[i].connected != m.auth.rows[i-1].connected {
			continue
		}
		if m.auth.rows[i].label < m.auth.rows[i-1].label {
			t.Fatalf("rows not sorted within run at %d: %q < %q", i, m.auth.rows[i].label, m.auth.rows[i-1].label)
		}
	}
}

// TestAuthSearchFiltersRows pins the as-you-type filter: typing narrows the
// list by label/id/detail, and the + Add Provider sentinel always stays.
func TestAuthSearchFiltersRows(t *testing.T) {
	m := newModelChoiceTestModel()
	m.auth.rows = []authRow{
		{addProvider: true, label: "+ Add Provider"},
		{id: "openai", label: "OpenAI", detail: "api.openai.com"},
		{id: "anthropic", label: "Anthropic", detail: "api.anthropic.com"},
		{id: "deepseek", label: "DeepSeek", detail: "api.deepseek.com"},
	}
	m.auth.query = "deep"
	filtered := m.authFilteredRows()
	if len(filtered) != 2 {
		t.Fatalf("expected 2 matches (sentinel + deepseek), got %d: %+v", len(filtered), filtered)
	}
	if !filtered[0].addProvider {
		t.Fatal("sentinel must stay first while searching")
	}
	if filtered[1].id != "deepseek" {
		t.Fatalf("expected deepseek match, got %q", filtered[1].id)
	}
}

// TestAuthTypingFiltersAndEnterSelectsMatch pins the end-to-end search flow:
// typing a query narrows the list, and Enter on the filtered list selects the
// matching provider (not an unrelated row that a numeric shortcut could hit).
func TestAuthTypingFiltersAndEnterSelectsMatch(t *testing.T) {
	m := newModelChoiceTestModel()
	m.creds = &config.Credentials{Providers: map[string]config.Credential{
		"openai":   {BaseURL: "https://api.openai.com"},
		"deepseek": {BaseURL: "https://api.deepseek.com"},
	}}
	m.deps.Loaded = &config.Loaded{Config: config.Config{Providers: map[string]config.Provider{}}}
	m.auth = authState{active: true, stage: authList, inputBuf: ""}
	m.rebuildAuthRows()

	// Type "deep" -> only deepseek (+ sentinel) remain.
	for _, r := range []rune("deep") {
		m.handleAuthKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, string(r))
	}
	filtered := m.authFilteredRows()
	if len(filtered) != 2 || filtered[1].id != "deepseek" {
		t.Fatalf("search should narrow to deepseek, got %+v", filtered)
	}

	// Enter with an empty input selects the highlighted filtered row.
	m.handleAuthKey(tea.KeyMsg{Type: tea.KeyEnter}, "enter")
	if m.auth.draftID != "deepseek" {
		t.Fatalf("Enter selected %q, want deepseek", m.auth.draftID)
	}
}

// TestAuthEscClearsSearchFirst pins that esc with an active search clears
// the query instead of backing out of /auth.
func TestAuthEscClearsSearchFirst(t *testing.T) {
	m := newModelChoiceTestModel()
	m.auth = authState{active: true, stage: authList, inputBuf: "deep", query: "deep"}
	_, _ = m.handleAuthKey(tea.KeyMsg{Type: tea.KeyEscape}, "esc")
	if !m.auth.active || m.auth.stage != authList {
		t.Fatalf("esc should not exit auth while searching: active=%v stage=%d", m.auth.active, m.auth.stage)
	}
	if m.auth.query != "" || m.auth.inputBuf != "" {
		t.Fatalf("esc should clear search: query=%q input=%q", m.auth.query, m.auth.inputBuf)
	}
}

// TestAuthSelectingAddProviderStartsAddFlow pins that selecting the sentinel
// row (cursor 0 + enter) begins the custom-provider flow.
func TestAuthSelectingAddProviderStartsAddFlow(t *testing.T) {
	m := newModelChoiceTestModel()
	m.auth = authState{active: true, stage: authList, rows: []authRow{
		{addProvider: true, label: "+ Add Provider"},
		{id: "openai", label: "OpenAI"},
	}, cursor: 0}
	_, _ = m.handleAuthKey(tea.KeyMsg{Type: tea.KeyEnter}, "enter")
	if m.auth.stage != authAddName || !m.auth.custom {
		t.Fatalf("selecting + Add Provider should start add flow: stage=%d custom=%v", m.auth.stage, m.auth.custom)
	}
}

// TestAuthAddProviderRendersInBody ensures the sentinel row appears in the
// rendered list body.
func TestAuthAddProviderRendersInBody(t *testing.T) {
	m := newModelChoiceTestModel()
	m.auth = authState{active: true, stage: authList, rows: []authRow{
		{addProvider: true, label: "+ Add Provider"},
		{id: "openai", label: "OpenAI"},
	}}
	body := m.authListBody(80)
	if !strings.Contains(body, "+ Add Provider") {
		t.Fatalf("+ Add Provider missing from list body:\n%s", body)
	}
	if !strings.Contains(body, "OpenAI") {
		t.Fatalf("OpenAI missing from list body:\n%s", body)
	}
}

// TestAuthBackspaceEditsSearch pins that backspace shrinks the search query
// (and its effect on the filtered rows) rather than leaving the flow.
func TestAuthBackspaceEditsSearch(t *testing.T) {
	m := newModelChoiceTestModel()
	m.auth = authState{active: true, stage: authList, inputBuf: "dee", query: "dee"}
	_, _ = m.handleAuthKey(tea.KeyMsg{Type: tea.KeyBackspace}, "backspace")
	if m.auth.query != "de" || m.auth.inputBuf != "de" {
		t.Fatalf("backspace should trim search: query=%q input=%q", m.auth.query, m.auth.inputBuf)
	}
	if !m.auth.active || m.auth.stage != authList {
		t.Fatal("backspace with a search active must not exit auth")
	}
}
