// Package compress produces deterministic provider-facing tool output while
// leaving canonical tool results untouched.
package compress

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Input is the lossless output plus a provider-facing byte budget. Command is
// the tool name or shell command that produced the output; Tool is the tool
// name when known (so dedicated git/grep/list tools route to the same
// command-aware compactors as their bash equivalents).
type Input struct {
	Text     string
	Query    string
	Command  string
	Tool     string
	MaxBytes int
	IsError  bool
}

// Result contains the compressed view and measurable reduction metadata.
type Result struct {
	Text            string
	OriginalBytes   int
	CompressedBytes int
	Truncated       bool
	Stage           string
	Fallback        bool
}

var (
	ansiSequence   = regexp.MustCompile("\\x1b\\[[0-?]*[ -/]*[@-~]")
	oscSequence    = regexp.MustCompile("\\x1b\\][^\\a]*(\\a|\\x1b\\\\)")
	progressLine   = regexp.MustCompile(`(?i)^\s*(progress|downloading|uploading)\b.*\d{1,3}%\s*$`)
	goSuccessLine  = regexp.MustCompile(`^\s*(?:ok|\?\s+\S+\s+\[no test files\])\b`)
	goLocationLine = regexp.MustCompile(`(?:^|\s)[^\s:]+\.(?:go|c|cc|cpp|h):\d+(?::\d+)?(?:\s|$)`)
)

// ForTool selects a deterministic, command-aware reducer. Unknown commands
// use the generic normalizer and cap rather than applying a risky heuristic.
func ForTool(input Input) Result {
	command := strings.ToLower(strings.TrimSpace(input.Command))
	// The dedicated tools are routed by name so their output gets the same
	// command-aware compaction as the equivalent bash command.
	switch strings.ToLower(strings.TrimSpace(input.Tool)) {
	case "git":
		return finish(input, compactGit(input.Text), "git")
	case "grep", "glob", "list", "tree", "find":
		return finish(input, compactSearch(input.Text), "search")
	case "test", "diagnostics", "go":
		return finish(input, compactGoDiagnostics(input.Text, input.IsError), "go-diagnostics")
	}
	switch {
	case isGitCommand(command):
		return finish(input, compactGit(input.Text), "git")
	case isGoCommand(command):
		return finish(input, compactGoDiagnostics(input.Text, input.IsError), "go-diagnostics")
	case isSearchCommand(command):
		return finish(input, compactSearch(input.Text), "search")
	default:
		// Structured documents (JSON/YAML) are minified before the byte cap.
		normalized := normalize(input.Text)
		if compact, ok := Minify(normalized); ok && len(compact) < len(normalized) {
			return finish(input, compact, "minify")
		}
		result := Generic(input)
		result.Stage = "generic"
		result.Fallback = true
		return result
	}
}

// Generic normalizes terminal output, removes transient progress noise, and
// applies an explicit UTF-8-safe truncation marker when needed.
func Generic(input Input) Result {
	return finish(input, normalize(input.Text), "generic")
}

func finish(input Input, text, stage string) Result {
	normalized := normalize(text)
	result := Result{
		Text:            normalized,
		OriginalBytes:   len(input.Text),
		CompressedBytes: len(normalized),
		Stage:           stage,
	}
	if input.MaxBytes <= 0 || len(normalized) <= input.MaxBytes {
		return result
	}

	omitted := len(normalized) - input.MaxBytes
	marker := fmt.Sprintf("\n… <output truncated; %d bytes omitted>", omitted)
	if len(marker) >= input.MaxBytes {
		result.Text = marker
		result.CompressedBytes = len(marker)
		result.Truncated = true
		return result
	}

	remaining := input.MaxBytes - len(marker)
	headBytes := remaining / 2
	tailBytes := remaining - headBytes
	result.Text = safePrefix(normalized, headBytes) + marker + safeSuffix(normalized, tailBytes)
	result.CompressedBytes = len(result.Text)
	result.Truncated = true
	return result
}

func isGitCommand(command string) bool {
	return command == "git" || strings.HasPrefix(command, "git ") || strings.Contains(command, " git ")
}

func isGoCommand(command string) bool {
	return command == "go" || strings.HasPrefix(command, "go ") ||
		command == "diagnostics" || command == "test" || strings.Contains(command, " go ")
}

func isSearchCommand(command string) bool {
	return command == "grep" || command == "glob" || command == "list" ||
		strings.HasPrefix(command, "grep ") || strings.HasPrefix(command, "rg ") ||
		strings.HasPrefix(command, "find ")
}

// compactGit keeps every non-duplicate line. Git status/diffs contain paths,
// hunks, modes, and failure diagnostics that must not be summarized away.
func compactGit(text string) string {
	return collapseConsecutiveDuplicates(text)
}

// compactGoDiagnostics drops only successful package chatter when the command
// has a failure. Failure summaries, source locations, test names, and all
// non-success lines remain intact.
func compactGoDiagnostics(text string, isError bool) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	failed := isError || strings.Contains(strings.ToUpper(text), "FAIL") ||
		strings.Contains(strings.ToLower(text), "panic:") || goLocationLine.MatchString(text)
	if !failed {
		return collapseConsecutiveDuplicates(text)
	}

	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if goSuccessLine.MatchString(line) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// Search results often repeat the same match through overlapping globs. Only
// exact consecutive duplicates are removed; distinct paths and line numbers
// remain lossless.
func compactSearch(text string) string {
	return collapseConsecutiveDuplicates(text)
}

func collapseConsecutiveDuplicates(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	previous := ""
	repeated := 0
	for _, line := range lines {
		if line != "" && line == previous {
			repeated++
			continue
		}
		if repeated > 0 {
			kept = append(kept, fmt.Sprintf("… <%d duplicate line(s) omitted>", repeated))
			repeated = 0
		}
		kept = append(kept, line)
		previous = line
	}
	if repeated > 0 {
		kept = append(kept, fmt.Sprintf("… <%d duplicate line(s) omitted>", repeated))
	}
	return strings.Join(kept, "\n")
}

func normalize(text string) string {
	text = ansiSequence.ReplaceAllString(text, "")
	text = oscSequence.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	previous := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if progressLine.MatchString(line) {
			continue
		}
		if trimmed == "" && previous == "" {
			continue
		}
		kept = append(kept, line)
		previous = trimmed
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func safePrefix(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if limit >= len(text) {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

func safeSuffix(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if limit >= len(text) {
		return text
	}
	start := len(text) - limit
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}
