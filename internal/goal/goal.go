// Package goal implements persistent goals with progress tracking and token
// budget enforcement for rick's agent loop.
package goal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Goal is a tracked objective with optional steps and a token budget.
type Goal struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"` // active | completed | aborted
	TokenBudget int    `json:"token_budget,omitempty"`
	TokensUsed  int    `json:"tokens_used"`
	Steps       []Step `json:"steps,omitempty"`
	// LoopRun, when set, makes this a loop goal: the agent keeps working on
	// the title, swallowing errors and retrying, until at least MinRunSeconds
	// of wall time have elapsed. MaxRetries bounds error retries.
	LoopRun *LoopRun  `json:"loop_run,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// LoopRun configures an autonomous loop goal (/loop). TokenBudget stays 0
// (unlimited) for loop goals — the loop is bounded by time, not tokens.
type LoopRun struct {
	MinRunSeconds int       `json:"min_run_seconds"`
	MaxRetries    int       `json:"max_retries"`
	Retries       int       `json:"retries"`
	StartedAt     time.Time `json:"started_at,omitempty"`
}

// Step is one unit of work within a goal.
type Step struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | done | skipped
}

// Store persists goals as JSON files in a directory.
type Store struct {
	mu  sync.Mutex
	dir string
}

// NewStore opens (and creates) a goal directory.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func validID(id string) bool {
	return id != "" && id != "." && id != ".." &&
		!strings.ContainsAny(id, "/\\\x00")
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".json") }

// NewID mints a sortable, filesystem-safe goal id.
func NewID() string {
	now := time.Now()
	return fmt.Sprintf("goal_%s_%04x", now.Format("2006-01-02T15-04-05"), now.Nanosecond()&0xFFFF)
}

// Save atomically writes a goal to disk.
func (s *Store) Save(g *Goal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(g)
}

func (s *Store) saveLocked(g *Goal) error {
	if g.ID == "" {
		g.ID = NewID()
	}
	if !validID(g.ID) {
		return fmt.Errorf("invalid goal id")
	}
	g.Updated = time.Now()
	if g.Created.IsZero() {
		g.Created = g.Updated
	}
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	final := s.path(g.ID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// Load reads a goal by id.
func (s *Store) Load(id string) (*Goal, error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid goal id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(id)
}

func (s *Store) loadLocked(id string) (*Goal, error) {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var g Goal
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// List returns all goals, newest first.
func (s *Store) List() ([]Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Goal
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var g Goal
		if json.Unmarshal(data, &g) != nil {
			continue
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// Delete removes a goal file.
func (s *Store) Delete(id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid goal id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, err := os.ReadFile(filepath.Join(s.dir, "active.json")); err == nil {
		var active map[string]string
		if json.Unmarshal(data, &active) == nil && active["id"] == id {
			if err := os.Remove(filepath.Join(s.dir, "active.json")); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return os.Remove(s.path(id))
}

// SetActive marks a goal as the active one by writing an active.json pointer.
func (s *Store) SetActive(id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid goal id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Join(s.dir, "active.json")
	data, err := json.Marshal(map[string]string{"id": id})
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ClearActive removes the active goal pointer.
func (s *Store) ClearActive() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Join(s.dir, "active.json")
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// GetActive returns the currently active goal, or nil if none is set.
func (s *Store) GetActive() (*Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(s.dir, "active.json"))
	if err != nil {
		return nil, nil //nolint:nilerr — no active goal is not an error
	}
	var m map[string]string
	if json.Unmarshal(data, &m) != nil {
		return nil, nil
	}
	id := m["id"]
	if !validID(id) {
		return nil, nil
	}
	return s.loadLocked(id)
}

// CheckBudget reports whether a goal still has token budget remaining.
// It returns false when TokensUsed >= TokenBudget (and TokenBudget > 0).
func CheckBudget(g *Goal) (ok bool, remaining int) {
	if g.TokenBudget <= 0 {
		return true, -1 // unlimited
	}
	remaining = g.TokenBudget - g.TokensUsed
	return remaining > 0, remaining
}

// AddTokens accumulates token usage on a goal and persists the change.
func (s *Store) AddTokens(id string, n int) error {
	if !validID(id) {
		return fmt.Errorf("invalid goal id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.loadLocked(id)
	if err != nil {
		return err
	}
	g.TokensUsed += n
	return s.saveLocked(g)
}

// UpdateStep changes a step's status within a goal.
func (s *Store) UpdateStep(goalID, stepID, status string) error {
	if !validID(goalID) {
		return fmt.Errorf("invalid goal id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.loadLocked(goalID)
	if err != nil {
		return err
	}
	for i := range g.Steps {
		if g.Steps[i].ID == stepID {
			g.Steps[i].Status = status
			return s.saveLocked(g)
		}
	}
	return fmt.Errorf("step %q not found in goal %q", stepID, goalID)
}

// Progress renders a human-readable progress string for a goal.
func Progress(g *Goal) string {
	done := 0
	total := len(g.Steps)
	for _, st := range g.Steps {
		if st.Status == "done" || st.Status == "skipped" {
			done++
		}
	}
	var b strings.Builder
	if total > 0 {
		fmt.Fprintf(&b, "%d/%d steps", done, total)
	} else {
		b.WriteString("no steps")
	}
	if g.TokenBudget > 0 {
		fmt.Fprintf(&b, " · %dk/%dk tokens", g.TokensUsed/1000, g.TokenBudget/1000)
	} else if g.TokensUsed > 0 {
		fmt.Fprintf(&b, " · %dk tokens used", g.TokensUsed/1000)
	}
	if g.LoopRun != nil {
		remaining := g.LoopRun.MinRunSeconds - int(time.Since(g.LoopRun.StartedAt).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		fmt.Fprintf(&b, " · loop %d/%d retries · %s/%s", g.LoopRun.Retries, g.LoopRun.MaxRetries,
			formatDuration(remaining), formatDuration(g.LoopRun.MinRunSeconds))
	}
	return b.String()
}

// formatDuration renders a seconds count compactly (e.g. "25m30s").
func formatDuration(totalSeconds int) string {
	if totalSeconds < 0 {
		totalSeconds = 0
	}
	h := totalSeconds / 3600
	m := (totalSeconds % 3600) / 60
	s := totalSeconds % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
