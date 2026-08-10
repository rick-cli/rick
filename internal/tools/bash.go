package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"rick/internal/sandbox"
)

// BashTool runs shell commands inside the active sandbox policy.
type BashTool struct {
	Timeout   time.Duration
	MaxOutput int
	ShellPath string          // override for tests
	Sandbox   *sandbox.Holder // nil means unconfined (tests, legacy callers)
}

// Name implements Tool.
func (BashTool) Name() string { return "bash" }

// ReadOnly implements Tool.
func (BashTool) ReadOnly() bool { return false }

// Description implements Tool.
func (t BashTool) Description() string {
	base := "Run a shell command in the project directory and return its combined " +
		"output. Prefer the dedicated read/write/edit/grep/glob tools over cat, " +
		"sed, grep and find — they are faster and safer. Use bash for builds, " +
		"tests, git, package managers and process control. Commands are subject " +
		"to the permission policy."

	p := t.policy()
	if !p.Confined() {
		return base
	}
	extra := fmt.Sprintf(" Commands run in a %s sandbox", p.Mode)
	if !p.Network {
		extra += " with no network access"
	}
	if p.Mode == sandbox.ModeWorkspace {
		extra += "; writes outside the project directory are rejected"
	}
	return base + extra + "."
}

// Schema implements Tool.
func (BashTool) Schema() map[string]any {
	return obj(map[string]any{
		"command":     strProp("Shell command to execute."),
		"description": strProp("Short description of what this command does (shown to the user)."),
		"timeout":     numProp("Timeout in seconds. Default 120, max 600."),
		"cwd":         pathProp("Working directory. Defaults to the project root."),
	}, "command")
}

type bashArgs struct {
	Command     string  `json:"command"`
	Description string  `json:"description"`
	Timeout     float64 `json:"timeout"`
	Cwd         string  `json:"cwd"`
}

// policy returns the active sandbox policy, or the unconfined one when no
// holder is wired up.
func (t BashTool) policy() sandbox.Policy {
	if t.Sandbox == nil {
		return sandbox.Off()
	}
	return t.Sandbox.Policy()
}

// Shell returns the shell binary and its argument prefix for this platform.
func Shell() (string, []string) {
	if runtime.GOOS == "windows" {
		// Prefer a POSIX shell if one is present (git-bash ships with git).
		for _, c := range []string{
			os.Getenv("RICK_SHELL"),
			`C:\Program Files\Git\bin\bash.exe`,
			`C:\Program Files (x86)\Git\bin\bash.exe`,
		} {
			if c == "" {
				continue
			}
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return c, []string{"-lc"}
			}
		}
		if p, err := exec.LookPath("bash"); err == nil {
			return p, []string{"-lc"}
		}
		return "cmd.exe", []string{"/c"}
	}
	if sh := os.Getenv("RICK_SHELL"); sh != "" {
		return sh, []string{"-lc"}
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p, []string{"-lc"}
	}
	return "/bin/sh", []string{"-c"}
}

// Run implements Tool.
func (t BashTool) Run(ctx context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a bashArgs
	if err := RepairDecode(in, &a, t.Schema(), tc.Repair); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Command) == "" {
		return Errf("command is required"), nil
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if a.Timeout > 0 {
		timeout = time.Duration(a.Timeout) * time.Second
	}
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}

	cwd := tc.Cwd
	if a.Cwd != "" {
		cwd = resolvePath(tc.Cwd, a.Cwd)
	}

	shell, prefix := Shell()
	if t.ShellPath != "" {
		shell = t.ShellPath
	}

	policy := t.policy()
	if policy.Workspace == "" {
		policy = policy.Normalize(tc.Cwd)
	}
	// A cwd outside the workspace would let the model sidestep the write
	// confinement by simply changing directory first.
	if policy.Confined() && !policy.Writable(cwd) && !policy.Readable(cwd) {
		return Errf("sandbox: working directory %s is outside the sandbox", cwd), nil
	}

	maxOut := t.MaxOutput
	if maxOut <= 0 {
		maxOut = 60 << 10
	}
	buf := boundedBuffer{limit: maxOut}

	outcome := sandbox.Run(ctx, policy, sandbox.Spec{
		Command: a.Command,
		Shell:   shell,
		Prefix:  prefix,
		Dir:     cwd,
		Env:     sandbox.Environ(policy),
		Timeout: timeout,
		Stdout:  &buf,
		Stderr:  &buf,
	})

	out := buf.Output()
	truncated := buf.Truncated()

	isErr := outcome.ExitCode != 0 || outcome.Err != nil
	switch {
	case outcome.TimedOut:
		out += fmt.Sprintf("\n<command timed out after %s>", timeout)
	case outcome.Err != nil:
		out += "\n" + outcome.Err.Error()
	}

	title := a.Description
	if title == "" {
		title = firstLine(a.Command)
	}
	body := strings.TrimRight(out, "\n")
	if body == "" {
		body = "<no output>"
	}
	body = fmt.Sprintf("$ %s\n%s\n<exit %d in %s>",
		firstLine(a.Command), body, outcome.ExitCode, outcome.Elapsed.Round(time.Millisecond))

	var note string
	if tc.Repair != nil && tc.Repair.Note != nil {
		note = *tc.Repair.Note
	}
	return repairNote(Result{
		Output:  body,
		Title:   title,
		IsError: isErr,
		Meta: map[string]any{
			"exit": outcome.ExitCode, "elapsed_ms": outcome.Elapsed.Milliseconds(),
			"truncated": truncated, "command": a.Command,
			"sandbox": outcome.Applied, "sandbox_mode": string(policy.Mode),
		},
	}, note), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
