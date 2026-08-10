package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"rick/internal/permission"
	"rick/internal/provider"
	"rick/internal/tools"
)

// SubagentKind identifies a built-in subagent type.
const (
	SubagentGeneral = "general"
	SubagentExplore = "explore"
)

func sortedSubagentNames(specs map[string]SubagentSpec) []string {
	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// SubagentSpec describes a spawnable subagent.
type SubagentSpec struct {
	Name        string
	Description string
	Prompt      string
	ReadOnly    bool
	Model       string
}

// BuiltinSubagents returns the shipped subagent types.
func BuiltinSubagents() map[string]SubagentSpec {
	return map[string]SubagentSpec{
		SubagentGeneral: {
			Name:        SubagentGeneral,
			Description: "General-purpose agent with full tool access (except task delegation). Use for multi-step work you want handled autonomously.",
			Prompt:      GeneralSubagentPrompt,
		},
		SubagentExplore: {
			Name:        SubagentExplore,
			Description: "Fast read-only agent for searching and understanding a codebase. Cannot modify anything. Use for 'where is X', 'how does Y work' questions.",
			Prompt:      ExploreSubagentPrompt,
			ReadOnly:    true,
		},
	}
}

// TaskTool lets a primary agent spawn a subagent.
type TaskTool struct {
	// Spawn runs a subagent and returns its final text. Injected by the host so
	// the tools package never depends on the agent runner.
	Spawn func(ctx context.Context, kind, description, prompt string, depth int) (string, error)
	// SpawnBackground starts a child and returns its registry ID immediately.
	SpawnBackground func(ctx context.Context, parentID, kind, description, prompt string, depth int) (string, error)

	Specs    map[string]SubagentSpec
	MaxDepth int
}

// Name implements tools.Tool.
func (TaskTool) Name() string { return "task" }

// ReadOnly implements tools.Tool. Subagents may write, so this is false.
func (TaskTool) ReadOnly() bool { return false }

// Description implements tools.Tool.
func (t TaskTool) Description() string {
	var b strings.Builder
	b.WriteString("Delegate work to a subagent running in its own context.\n\n")
	b.WriteString("USE THIS for multi-step tasks that can be done autonomously. Examples:\n")
	b.WriteString("- Analyze 3 files and summarize the architecture\n")
	b.WriteString("- Refactor a function and update all callers\n")
	b.WriteString("- Investigate a bug across multiple files\n\n")
	b.WriteString("DO NOT USE for: simple lookups, single file edits, questions you can answer in 1-2 tool calls.\n\n")
	b.WriteString("To parallelize: call task multiple times in one turn with different prompts. Each runs independently and reports back.\n\n")
	b.WriteString("The subagent cannot ask you questions. Make your prompt completely self-contained.\n\n")
	b.WriteString("SWARM: you also have a 'swarm' tool for multi-agent collaboration with messaging between agents.\n")
	b.WriteString("Use 'swarm' with action='spawn' to create named agents that can message each other and coordinate.\n")
	b.WriteString("Use 'task' for one-shot delegation, 'swarm' for ongoing multi-agent coordination.\n\n")
	b.WriteString("The optional background=true returns immediately with an agent ID; use /agents or chat/steer to follow it.\n\n")
	b.WriteString("Available subagent types:\n")
	names := sortedSubagentNames(t.Specs)
	for _, name := range names {
		fmt.Fprintf(&b, "- %s: %s\n", name, t.Specs[name].Description)
	}
	return b.String()
}

// Schema implements tools.Tool.
func (t TaskTool) Schema() map[string]any {
	kinds := sortedSubagentNames(t.Specs)
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"subagent_type": map[string]any{
				"type": "string", "enum": kinds,
				"description": "Which subagent to spawn.",
			},
			"description": map[string]any{
				"type": "string", "description": "Short (3-6 word) label shown in the UI.",
			},
			"prompt": map[string]any{
				"type":        "string",
				"description": "The complete, self-contained task. Include all context, file paths and constraints the subagent needs, and state exactly what it should report back.",
			},
			"background": map[string]any{
				"type":        "boolean",
				"description": "Run in the background and return an agent ID immediately (default false).",
			},
		},
		"required": []string{"subagent_type", "description", "prompt"},
	}
}

type taskArgs struct {
	SubagentType string `json:"subagent_type"`
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	Background   bool   `json:"background"`
}

func titleFor(description, kind string) string {
	if strings.TrimSpace(description) != "" {
		return description
	}
	return kind + " subagent"
}

// Run implements tools.Tool.
func (t TaskTool) Run(ctx context.Context, tc tools.Context, in json.RawMessage) (tools.Result, error) {
	var a taskArgs
	if err := json.Unmarshal(in, &a); err != nil {
		return tools.Errf("invalid arguments: %v", err), nil
	}
	if a.Prompt == "" {
		return tools.Errf("prompt is required"), nil
	}
	if a.SubagentType == "" {
		a.SubagentType = SubagentGeneral
	}
	if _, ok := t.Specs[a.SubagentType]; !ok {
		kinds := sortedSubagentNames(t.Specs)
		return tools.Errf("unknown subagent_type %q (have: %s)", a.SubagentType, strings.Join(kinds, ", ")), nil
	}

	maxDepth := t.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 1
	}
	if tc.Depth >= maxDepth {
		return tools.Errf(
			"subagent depth limit reached (%d) — do this work yourself instead of delegating further",
			maxDepth), nil
	}
	if t.Spawn == nil {
		return tools.Errf("subagents are not available in this context"), nil
	}

	if a.Background {
		if t.SpawnBackground == nil {
			return tools.Errf("background subagents are not available in this context"), nil
		}
		id, err := t.SpawnBackground(ctx, tc.AgentID, a.SubagentType, a.Description, a.Prompt, tc.Depth+1)
		if err != nil {
			return tools.Errf("background subagent failed: %v", err), nil
		}
		return tools.Result{
			Output: fmt.Sprintf("background agent started: %s", id),
			Title:  fmt.Sprintf("%s (%s) started", titleFor(a.Description, a.SubagentType), a.SubagentType),
			Meta:   map[string]any{"agent_id": id, "background": true, "subagent": a.SubagentType},
		}, nil
	}
	out, err := t.Spawn(ctx, a.SubagentType, a.Description, a.Prompt, tc.Depth+1)
	if err != nil {
		return tools.Errf("subagent failed: %v", err), nil
	}
	if strings.TrimSpace(out) == "" {
		out = "<the subagent returned no output>"
	}
	title := a.Description
	if title == "" {
		title = a.SubagentType + " subagent"
	}
	return tools.Result{
		Output: capSubagentReport(out),
		Title:  fmt.Sprintf("%s (%s)", title, a.SubagentType),
		Meta:   map[string]any{"subagent": a.SubagentType, "description": a.Description},
	}, nil
}

// RunSubagent executes a subagent to completion and returns its final text.
// It reuses the ordinary Runner, so subagents get the same tool loop,
// permission checks and streaming semantics as the primary agent.
func RunSubagent(ctx context.Context, cfg Config, prompt string, onEvent func(Event)) (string, error) {
	ch := make(chan Event, 128)
	runner := New(cfg)

	var (
		mu         sync.Mutex
		currentTxt strings.Builder
		finalTxt   strings.Builder
	)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range ch {
			if onEvent != nil {
				onEvent(ev)
			}
			switch ev.Kind {
			case EvText:
				mu.Lock()
				currentTxt.WriteString(ev.Text)
				mu.Unlock()
			case EvTurnEnd:
				mu.Lock()
				if strings.TrimSpace(currentTxt.String()) != "" {
					finalTxt.Reset()
					finalTxt.WriteString(currentTxt.String())
				}
				currentTxt.Reset()
				mu.Unlock()
			}
		}
	}()

	_, err := runner.Run(ctx, []provider.Message{provider.UserText(prompt)}, ch)
	<-done

	mu.Lock()
	defer mu.Unlock()
	out := strings.TrimSpace(finalTxt.String())
	if out == "" {
		out = strings.TrimSpace(currentTxt.String())
	}
	return out, err
}

// SubagentToolFilter restricts a subagent's tool set.
func SubagentToolFilter(spec SubagentSpec, base func(string) bool) func(string) bool {
	writeTools := map[string]bool{
		"write": true, "edit": true, "apply_patch": true, "bash": true,
	}
	return func(name string) bool {
		if name == "task" || name == "parallel_tasks" || name == "swarm" {
			return false // subagents never delegate further
		}
		if spec.ReadOnly && writeTools[name] {
			return false
		}
		if base != nil {
			return base(name)
		}
		return true
	}
}

// SubagentPermissions tightens the policy for a read-only subagent unless the
// parent is already in yolo mode. Yolo is an explicit user decision to bypass
// permission restrictions, and that decision must propagate to child runs.
func SubagentPermissions(spec SubagentSpec, base *permission.Engine, root string) *permission.Engine {
	if base == nil || !spec.ReadOnly || base.Yolo() {
		return base
	}
	p := base.Permission()
	cp := *p
	cp.Edit, cp.Write = "deny", "deny"
	cp.Bash = map[string]string{"*": "deny"}
	e := permission.New(&cp, root)
	return e
}
