package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// TodoItem is one entry in the model's task list.
type TodoItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | completed | cancelled
}

// TodoStore holds the current list for a session. It is safe for concurrent
// use and exposes a change callback so the TUI can re-render the panel.
type TodoStore struct {
	mu       sync.RWMutex
	items    []TodoItem
	OnChange func([]TodoItem)
}

// NewTodoStore builds an empty store.
func NewTodoStore() *TodoStore { return &TodoStore{} }

// Items returns a copy of the list.
func (s *TodoStore) Items() []TodoItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]TodoItem(nil), s.items...)
}

// Set replaces the list.
func (s *TodoStore) Set(items []TodoItem) {
	s.mu.Lock()
	s.items = append([]TodoItem(nil), items...)
	cb := s.OnChange
	snapshot := append([]TodoItem(nil), s.items...)
	s.mu.Unlock()
	if cb != nil {
		cb(snapshot)
	}
}

// Clear empties the list.
func (s *TodoStore) Clear() { s.Set(nil) }

// Pending counts unfinished items.
func (s *TodoStore) Pending() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, it := range s.items {
		if it.Status == "pending" || it.Status == "in_progress" {
			n++
		}
	}
	return n
}

// TodoWriteTool lets the model maintain its own task list.
type TodoWriteTool struct{ Store *TodoStore }

// Name implements Tool.
func (TodoWriteTool) Name() string { return "todowrite" }

// ReadOnly implements Tool.
func (TodoWriteTool) ReadOnly() bool { return true }

// Description implements Tool.
func (TodoWriteTool) Description() string {
	return "Maintain a structured task list for the current work. Use it for any " +
		"task needing 3+ steps: write the full plan up front, mark exactly one " +
		"item 'in_progress' at a time, and mark items 'completed' immediately " +
		"when finished. Always send the COMPLETE list — it replaces the previous one."
}

// Schema implements Tool.
func (TodoWriteTool) Schema() map[string]any {
	return obj(map[string]any{
		"todos": map[string]any{
			"type":        "array",
			"description": "The complete task list, in priority order.",
			"items": obj(map[string]any{
				"id":      strProp("Stable identifier for the item."),
				"content": strProp("What needs to be done."),
				"status": map[string]any{
					"type": "string",
					"enum": []string{"pending", "in_progress", "completed", "cancelled"},
				},
			}, "id", "content", "status"),
		},
	}, "todos")
}

type todoArgs struct {
	Todos []TodoItem `json:"todos"`
}

// Run implements Tool.
func (t TodoWriteTool) Run(_ context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a todoArgs
	if err := RepairDecode(in, &a, t.Schema(), tc.Repair); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	inProgress := 0
	for i := range a.Todos {
		if a.Todos[i].Status == "" {
			a.Todos[i].Status = "pending"
		}
		if a.Todos[i].ID == "" {
			a.Todos[i].ID = fmt.Sprintf("t%d", i+1)
		}
		if a.Todos[i].Status == "in_progress" {
			inProgress++
		}
	}
	if t.Store != nil {
		t.Store.Set(a.Todos)
	}

	var b strings.Builder
	done, total := 0, len(a.Todos)
	for _, it := range a.Todos {
		mark := "[ ]"
		switch it.Status {
		case "completed":
			mark = "[x]"
			done++
		case "in_progress":
			mark = "[~]"
		case "cancelled":
			mark = "[-]"
			done++
		}
		fmt.Fprintf(&b, "%s %s\n", mark, it.Content)
	}
	note := ""
	if inProgress > 1 {
		note = "\nnote: more than one item is in_progress; keep it to one."
	}
	return repairNote(Result{
		Output: fmt.Sprintf("task list updated (%d/%d done)\n%s%s", done, total, b.String(), note),
		Title:  fmt.Sprintf("todos %d/%d", done, total),
		Meta:   map[string]any{"todos": a.Todos},
	}, noteOf(tc)), nil
}

// TodoReadTool returns the current list.
type TodoReadTool struct{ Store *TodoStore }

// Name implements Tool.
func (TodoReadTool) Name() string { return "todoread" }

// ReadOnly implements Tool.
func (TodoReadTool) ReadOnly() bool { return true }

// Description implements Tool.
func (TodoReadTool) Description() string {
	return "Read the current task list. Call this when you have lost track of " +
		"which step you are on."
}

// Schema implements Tool.
func (TodoReadTool) Schema() map[string]any { return obj(map[string]any{}) }

// Run implements Tool.
func (t TodoReadTool) Run(_ context.Context, _ Context, _ json.RawMessage) (Result, error) {
	if t.Store == nil {
		return Result{Output: "task list is empty", Title: "todos"}, nil
	}
	items := t.Store.Items()
	if len(items) == 0 {
		return Result{Output: "task list is empty", Title: "todos"}, nil
	}
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "- [%s] %s (%s)\n", it.Status, it.Content, it.ID)
	}
	return Result{Output: b.String(), Title: fmt.Sprintf("todos (%d)", len(items))}, nil
}
