package tui

import (
	"errors"
	"testing"
	"time"

	"rick/internal/config"
	"rick/internal/goal"
)

func TestParseLoopArgs(t *testing.T) {
	tests := []struct {
		in       string
		wantDur  time.Duration
		wantTask string
	}{
		{"10m fix all failing tests", 10 * time.Minute, "fix all failing tests"},
		{"1h30m build the login system", 90 * time.Minute, "build the login system"},
		{"90s polish the UI", 90 * time.Second, "polish the UI"},
		{"10m", 0, ""},            // missing task
		{"", 0, ""},               // empty
		{"abc fix things", 0, ""}, // bad duration
		{"-5m fix things", 0, ""}, // non-positive duration
	}
	for _, tt := range tests {
		gotDur, gotTask := parseLoopArgs(tt.in)
		if gotDur != tt.wantDur {
			t.Errorf("parseLoopArgs(%q) duration = %v, want %v", tt.in, gotDur, tt.wantDur)
		}
		if gotTask != tt.wantTask {
			t.Errorf("parseLoopArgs(%q) task = %q, want %q", tt.in, gotTask, tt.wantTask)
		}
	}
}

func newLoopTestModel(t *testing.T) (*Model, *goal.Store) {
	t.Helper()
	store, err := goal.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("goal store: %v", err)
	}
	m := &Model{
		deps: Deps{Loaded: &config.Loaded{}, Goals: store},
		tx:   newTranscript(),
	}
	return m, store
}

func TestLoopAdvanceAfterMinRunCompletesGoal(t *testing.T) {
	m, store := newLoopTestModel(t)
	g := &goal.Goal{Title: "task", Status: "active", LoopRun: &goal.LoopRun{MinRunSeconds: 1, MaxRetries: 100}}
	if err := store.Save(g); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.SetActive(g.ID); err != nil {
		t.Fatalf("set active: %v", err)
	}
	m.loop = &loopState{goalID: g.ID, task: "task", minRun: 1 * time.Millisecond, maxRetries: 100, start: time.Now().Add(-time.Second)}

	cmd, stop := m.loopAdvance()
	if !stop {
		t.Fatal("loopAdvance did not signal stop")
	}
	if cmd != nil {
		t.Fatalf("expected nil command after completing, got %v", cmd)
	}
	if m.loop != nil {
		t.Fatal("loop state not cleared after completion")
	}
	active, err := store.GetActive()
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if active != nil {
		t.Fatalf("active goal not cleared, got %q", active.Title)
	}
	loaded, err := store.Load(g.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Status != "completed" {
		t.Fatalf("goal status = %q, want completed", loaded.Status)
	}
}

func TestLoopAdvanceBeforeMinRunContinues(t *testing.T) {
	m, _ := newLoopTestModel(t)
	m.loop = &loopState{
		goalID: "g", task: "task",
		minRun: time.Hour, maxRetries: 100, start: time.Now(),
		tools: 2,
	}
	// No provider configured: startAgent fails fast but the loop must survive.
	cmd, stop := m.loopAdvance()
	if !stop {
		t.Fatal("loopAdvance did not signal stop")
	}
	if cmd != nil {
		t.Fatalf("expected nil command (no provider), got %v", cmd)
	}
	if m.loop == nil {
		t.Fatal("loop state cleared while minimum run time remains")
	}
	if m.loop.tools != 0 {
		t.Fatalf("iteration tool counter not reset, got %d", m.loop.tools)
	}
}

func TestLoopAdvanceNoToolsStillContinues(t *testing.T) {
	m, _ := newLoopTestModel(t)
	m.loop = &loopState{
		goalID: "g", task: "task",
		minRun: time.Hour, maxRetries: 100, start: time.Now(),
		tools: 0,
	}
	cmd, _ := m.loopAdvance()
	if cmd != nil {
		t.Fatalf("expected nil command, got %v", cmd)
	}
	if m.loop == nil {
		t.Fatal("loop ended before minimum run time")
	}
}

func TestLoopRetrySwallowsErrorAndContinues(t *testing.T) {
	m, _ := newLoopTestModel(t)
	m.loop = &loopState{
		goalID: "g", task: "task",
		minRun: time.Hour, maxRetries: 100, start: time.Now(),
	}
	cmd, stop := m.loopRetry(errors.New("provider exploded"))
	if !stop {
		t.Fatal("loopRetry did not signal stop")
	}
	if cmd != nil {
		t.Fatalf("expected nil command (no provider), got %v", cmd)
	}
	if m.loop == nil {
		t.Fatal("loop ended after a single error")
	}
	if m.loop.retries != 1 {
		t.Fatalf("retries = %d, want 1", m.loop.retries)
	}
}

func TestLoopRetryExhaustionAbortsGoal(t *testing.T) {
	m, store := newLoopTestModel(t)
	g := &goal.Goal{Title: "task", Status: "active", LoopRun: &goal.LoopRun{MinRunSeconds: 60, MaxRetries: 100}}
	if err := store.Save(g); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.SetActive(g.ID); err != nil {
		t.Fatalf("set active: %v", err)
	}
	m.loop = &loopState{goalID: g.ID, task: "task", minRun: time.Hour, maxRetries: 1, start: time.Now()}

	cmd, stop := m.loopRetry(errors.New("boom"))
	if !stop {
		t.Fatal("loopRetry did not signal stop")
	}
	if cmd != nil {
		t.Fatalf("expected nil command, got %v", cmd)
	}
	if m.loop != nil {
		t.Fatal("loop state not cleared after retry exhaustion")
	}
	loaded, err := store.Load(g.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Status != "aborted" {
		t.Fatalf("goal status = %q, want aborted", loaded.Status)
	}
	if loaded.LoopRun == nil || loaded.LoopRun.Retries != 1 {
		t.Fatalf("loop retries not persisted, LoopRun = %+v", loaded.LoopRun)
	}
}

func TestLoopHandlersWithoutLoopFallThrough(t *testing.T) {
	m, _ := newLoopTestModel(t)
	cmd, stop := m.loopAdvance()
	if !stop || cmd != nil {
		t.Fatalf("loopAdvance without loop: cmd=%v stop=%v, want nil/true", cmd, stop)
	}
	cmd, stop = m.loopRetry(errors.New("x"))
	if !stop || cmd != nil {
		t.Fatalf("loopRetry without loop: cmd=%v stop=%v, want nil/true", cmd, stop)
	}
}
