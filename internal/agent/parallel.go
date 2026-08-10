package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"rick/internal/tools"
)

// ParallelTaskTool allows spawning multiple subagents concurrently.
type ParallelTaskTool struct {
	Spawn           func(ctx context.Context, kind, description, prompt string, depth int) (string, error)
	SpawnBackground func(ctx context.Context, parentID, kind, description, prompt string, depth int) (string, error)
	Specs           map[string]SubagentSpec
	MaxDepth        int
}

func (ParallelTaskTool) Name() string   { return "parallel_tasks" }
func (ParallelTaskTool) ReadOnly() bool { return false }

// maxSubagentReportBytes bounds a single subagent's report echoed to the
// parent. A report beyond this is head+tail trimmed with a marker so the
// parent still gets the conclusion and the beginning, not the transcript.
const maxSubagentReportBytes = 8 << 10

func capSubagentReport(report string) string {
	if len(report) <= maxSubagentReportBytes {
		return report
	}
	marker := fmt.Sprintf("\n… <%d bytes of subagent report omitted>", len(report)-maxSubagentReportBytes)
	limit := maxSubagentReportBytes - len(marker)
	head := limit * 3 / 4
	tail := limit - head
	var b strings.Builder
	b.WriteString(report[:head])
	b.WriteString(marker)
	if tail > 0 {
		b.WriteString(report[len(report)-tail:])
	}
	return b.String()
}

func (ParallelTaskTool) Description() string {
	return "Spawn multiple subagents in parallel. Each runs independently and concurrently.\n" +
		"Pass an array of tasks; all are launched at once and results are collected.\n" +
		"Useful for independent work that can proceed simultaneously."
}

func (ParallelTaskTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tasks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"subagent_type": map[string]any{"type": "string", "description": "general, explore, or custom type"},
						"description":   map[string]any{"type": "string", "description": "Short label for this task"},
						"prompt":        map[string]any{"type": "string", "description": "Full self-contained task prompt"},
					},
					"required": []string{"subagent_type", "description", "prompt"},
				},
				"description": "Array of tasks to run concurrently",
			},
			"max_concurrent": map[string]any{"type": "number", "description": "Max concurrent agents (default 4)"},
			"background":     map[string]any{"type": "boolean", "description": "Start all agents in the background and return IDs immediately (default false)"},
		},
		"required": []string{"tasks"},
	}
}

type parallelArgs struct {
	Tasks []struct {
		SubagentType string `json:"subagent_type"`
		Description  string `json:"description"`
		Prompt       string `json:"prompt"`
	} `json:"tasks"`
	MaxConcurrent int  `json:"max_concurrent"`
	Background    bool `json:"background"`
}

func (t ParallelTaskTool) Run(ctx context.Context, tc tools.Context, in json.RawMessage) (tools.Result, error) {
	var a parallelArgs
	if err := json.Unmarshal(in, &a); err != nil {
		return tools.Errf("invalid arguments: %v", err), nil
	}
	if len(a.Tasks) == 0 {
		return tools.Errf("no tasks provided"), nil
	}
	if a.MaxConcurrent <= 0 {
		a.MaxConcurrent = 4
	}
	if t.MaxDepth > 0 && tc.Depth >= t.MaxDepth {
		return tools.Errf("maximum subagent depth reached (%d)", t.MaxDepth), nil
	}
	if a.Background && t.SpawnBackground == nil {
		return tools.Errf("background subagents are unavailable in this context"), nil
	}
	if !a.Background && t.Spawn == nil {
		return tools.Errf("subagent spawning is unavailable in this context"), nil
	}

	type result struct {
		desc string
		out  string
		err  error
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, a.MaxConcurrent)
	results := make([]result, len(a.Tasks))

	for i, task := range a.Tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, tk struct {
			SubagentType string `json:"subagent_type"`
			Description  string `json:"description"`
			Prompt       string `json:"prompt"`
		}) {
			defer wg.Done()
			defer func() { <-sem }()

			if strings.TrimSpace(tk.SubagentType) == "" {
				results[idx] = result{desc: tk.Description, err: fmt.Errorf("subagent_type is required")}
				return
			}
			if len(t.Specs) > 0 {
				if _, ok := t.Specs[tk.SubagentType]; !ok {
					results[idx] = result{desc: tk.Description, err: fmt.Errorf("unknown subagent_type %q", tk.SubagentType)}
					return
				}
			}
			var out string
			var err error
			if a.Background {
				if t.SpawnBackground == nil {
					err = fmt.Errorf("background subagents are not available in this context")
				} else {
					out, err = t.SpawnBackground(ctx, tc.AgentID, tk.SubagentType, tk.Description, tk.Prompt, tc.Depth+1)
				}
			} else {
				out, err = t.Spawn(ctx, tk.SubagentType, tk.Description, tk.Prompt, tc.Depth+1)
			}
			results[idx] = result{desc: tk.Description, out: out, err: err}
		}(i, task)
	}
	wg.Wait()

	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "--- Task %d: %s ---\n", i+1, r.desc)
		if r.err != nil {
			fmt.Fprintf(&b, "ERROR: %v\n\n", r.err)
		} else {
			// A subagent's full report can run to tens of KBs; the parent
			// needs the conclusion, not the transcript. Cap each report and
			// keep the byte budget per task bounded.
			fmt.Fprintf(&b, "%s\n\n", capSubagentReport(r.out))
		}
	}

	return tools.Result{
		Output: strings.TrimRight(b.String(), "\n"),
		Title:  fmt.Sprintf("%d parallel tasks", len(a.Tasks)),
		Meta:   map[string]any{"count": len(a.Tasks)},
	}, nil
}
