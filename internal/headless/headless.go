// Package headless runs the rick agent non-interactively (no TUI), suitable
// for CI pipelines, scripting, and pipe-friendly workflows.
package headless

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"rick/internal/agent"
	"rick/internal/config"
	"rick/internal/permission"
	"rick/internal/plugin"
	"rick/internal/provider"
	"rick/internal/session"
	"rick/internal/tools"
)

// OutputFormat controls how results are rendered.
type OutputFormat string

// Supported output formats.
const (
	FormatText       OutputFormat = "text"
	FormatJSON       OutputFormat = "json"
	FormatStreamJSON OutputFormat = "stream-json"
)

// Options configures a headless run.
type Options struct {
	Prompt      string
	Model       string
	Yolo        bool
	MaxTurns    int // cap on agent turns; <= 0 means unlimited
	Format      OutputFormat
	Cwd         string
	ProjectRoot string
	AgentName   string
}

// Deps carries the shared infrastructure the headless runner needs.
type Deps struct {
	Provider provider.Provider
	ModelID  string
	Config   config.Config
	Tools    *tools.Registry
	Perms    *permission.Engine
	Plugins  *plugin.Registry
	Store    *session.Store
}

// ToolRecord captures one tool invocation for JSON output.
type ToolRecord struct {
	Name    string          `json:"name"`
	Title   string          `json:"title"`
	Input   json.RawMessage `json:"input,omitempty"`
	Output  string          `json:"output,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
	Elapsed string          `json:"elapsed,omitempty"`
}

// UsageRecord captures cumulative token usage.
type UsageRecord struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// Result is the structured output for --output-format json.
type Result struct {
	Response  string       `json:"response"`
	ToolsUsed []ToolRecord `json:"tools_used"`
	Usage     UsageRecord  `json:"usage"`
	SessionID string       `json:"session_id"`
	Error     string       `json:"error,omitempty"`
}

// streamEvent is one NDJSON line for --output-format stream-json.
type streamEvent struct {
	Type    string       `json:"type"`
	Text    string       `json:"text,omitempty"`
	Tool    *ToolRecord  `json:"tool,omitempty"`
	Usage   *UsageRecord `json:"usage,omitempty"`
	Error   string       `json:"error,omitempty"`
	Session string       `json:"session_id,omitempty"`
}

// Run executes the agent loop headlessly and writes output to stdout/stderr.
// It returns nil on success, or an error on failure.
func Run(ctx context.Context, opts Options, deps Deps, stdout, stderr io.Writer) error {
	if opts.Format == "" {
		opts.Format = FormatText
	}
	if opts.AgentName == "" {
		opts.AgentName = "build"
	}

	sessionID := session.NewID()

	// Build the permission asker: yolo auto-approves everything; normal mode
	// rejects anything that would require interactive approval (fail-safe).
	var ask agent.PermissionAsker
	if opts.Yolo {
		ask = func(_ context.Context, _ permission.Request) agent.PermissionDecision {
			return agent.DecideAlways
		}
	} else {
		ask = func(_ context.Context, _ permission.Request) agent.PermissionDecision {
			return agent.DecideReject
		}
	}

	// Build the system prompt with the stable prefix first. Providers that
	// support prompt caching can retain the instructions across turns while
	// the environment remains a volatile suffix.
	stableSystem := agent.BuildPrompt + agent.ProjectContext(opts.ProjectRoot, nil)
	system := stableSystem + agent.Environment(opts.Cwd, opts.Model, opts.AgentName, "")

	runner := agent.New(agent.Config{
		Provider:           deps.Provider,
		Model:              deps.ModelID,
		System:             system,
		SystemStable:       stableSystem,
		MaxTokens:          deps.Config.MaxTokens,
		Tools:              deps.Tools,
		Perms:              deps.Perms,
		Ask:                ask,
		Cwd:                opts.Cwd,
		SessionID:          sessionID,
		AgentName:          opts.AgentName,
		MaxTurns:           opts.MaxTurns,
		Plugins:            deps.Plugins,
		Parallel:           true,
		RepoMapRoot:        opts.ProjectRoot,
		EnableDistillation: deps.Config.DistillEnabled != nil && *deps.Config.DistillEnabled,
		DistillModel:       deps.Config.DistillModelFor(),
		CacheRetention:     provider.CacheRetention(deps.Config.CacheRetention),
		WarmCache:          deps.Config.WarmCache,
		MaxReasoningTurns:  deps.Config.CacheMaxReasoningTurns,
		MaxToolResultBytes: deps.Config.CacheMaxToolResultBytes,
	})

	history := []provider.Message{provider.UserText(opts.Prompt)}
	ch := make(chan agent.Event, 256)

	// Collect results for JSON mode.
	var (
		responseText strings.Builder
		toolRecords  []ToolRecord
		usage        UsageRecord
		runErr       error
	)

	// Run the agent in a goroutine; drain events on this goroutine.
	var appended []provider.Message
	done := make(chan struct{})
	go func() {
		appended, runErr = runner.Run(ctx, history, ch)
		close(done)
	}()

	enc := json.NewEncoder(stdout)

	for ev := range ch {
		switch ev.Kind {
		case agent.EvText:
			responseText.WriteString(ev.Text)
			switch opts.Format {
			case FormatText:
				fmt.Fprint(stdout, ev.Text)
			case FormatStreamJSON:
				_ = enc.Encode(streamEvent{Type: "text", Text: ev.Text})
			}

		case agent.EvThinking:
			if opts.Format == FormatStreamJSON {
				_ = enc.Encode(streamEvent{Type: "thinking", Text: ev.Text})
			}

		case agent.EvToolStart:
			if ev.Tool == nil {
				break
			}
			switch opts.Format {
			case FormatText:
				fmt.Fprintf(stderr, "⚡ %s\n", ev.Tool.Title)
			case FormatStreamJSON:
				_ = enc.Encode(streamEvent{
					Type: "tool_start",
					Tool: &ToolRecord{
						Name:  ev.Tool.Name,
						Title: ev.Tool.Title,
						Input: ev.Tool.Input,
					},
				})
			}

		case agent.EvToolEnd:
			if ev.Tool == nil {
				break
			}
			rec := ToolRecord{
				Name:    ev.Tool.Name,
				Title:   ev.Tool.Title,
				Input:   ev.Tool.Input,
				Output:  truncate(ev.Tool.Output, 2000),
				IsError: ev.Tool.IsError,
				Elapsed: ev.Tool.Elapsed.Round(time.Millisecond).String(),
			}
			toolRecords = append(toolRecords, rec)
			if opts.Format == FormatStreamJSON {
				_ = enc.Encode(streamEvent{Type: "tool_end", Tool: &rec})
			}

		case agent.EvUsage:
			if ev.Usage == nil {
				break
			}
			usage.InputTokens += ev.Usage.InputTokens
			usage.OutputTokens += ev.Usage.OutputTokens
			usage.CacheReadTokens += ev.Usage.CacheReadTokens
			usage.CacheWriteTokens += ev.Usage.CacheWriteTokens
			if opts.Format == FormatStreamJSON {
				_ = enc.Encode(streamEvent{
					Type: "usage",
					Usage: &UsageRecord{
						InputTokens:      ev.Usage.InputTokens,
						OutputTokens:     ev.Usage.OutputTokens,
						CacheReadTokens:  ev.Usage.CacheReadTokens,
						CacheWriteTokens: ev.Usage.CacheWriteTokens,
					},
				})
			}

		case agent.EvError:
			if ev.Err != nil {
				if opts.Format == FormatStreamJSON {
					_ = enc.Encode(streamEvent{Type: "error", Error: ev.Err.Error()})
				}
			}

		case agent.EvDone:
			if opts.Format == FormatStreamJSON {
				_ = enc.Encode(streamEvent{Type: "done", Session: sessionID})
			}
		}
	}

	<-done

	// Ensure text output ends with a newline.
	if opts.Format == FormatText && responseText.Len() > 0 {
		s := responseText.String()
		if !strings.HasSuffix(s, "\n") {
			fmt.Fprintln(stdout)
		}
	}

	// Emit final JSON result.
	if opts.Format == FormatJSON {
		result := Result{
			Response:  responseText.String(),
			ToolsUsed: toolRecords,
			Usage:     usage,
			SessionID: sessionID,
		}
		if runErr != nil {
			result.Error = runErr.Error()
		}
		if toolRecords == nil {
			result.ToolsUsed = []ToolRecord{}
		}
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return fmt.Errorf("headless: marshal result: %w", err)
		}
		fmt.Fprintln(stdout, string(data))
	}

	// Persist the session.
	if deps.Store != nil {
		allMsgs := append([]provider.Message{provider.UserText(opts.Prompt)}, appended...)
		sess := &session.Session{
			ID:       sessionID,
			Title:    session.Title(allMsgs),
			Cwd:      opts.Cwd,
			Model:    opts.Model,
			Agent:    opts.AgentName,
			Messages: allMsgs,
			Usage: session.Usage{
				Input:      usage.InputTokens,
				Output:     usage.OutputTokens,
				CacheRead:  usage.CacheReadTokens,
				CacheWrite: usage.CacheWriteTokens,
			},
		}
		if err := deps.Store.Save(sess); err != nil {
			fmt.Fprintf(stderr, "warning: failed to save session: %v\n", err)
		}
	}

	return runErr
}

// truncate clips s to at most n runes, appending an ellipsis marker.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
