package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"rick/internal/glob"
)

// ripgrepPath resolves the rg binary once.
var ripgrepPath = func() string {
	if p := os.Getenv("RICK_RIPGREP"); p != "" {
		return p
	}
	if p, err := exec.LookPath("rg"); err == nil {
		return p
	}
	return ""
}()

// HasRipgrep reports whether rg is available.
func HasRipgrep() bool { return ripgrepPath != "" }

// ---------- grep ----------

// GrepTool searches file contents with ripgrep.
type GrepTool struct{ MaxResults int }

// Name implements Tool.
func (GrepTool) Name() string { return "grep" }

// ReadOnly implements Tool.
func (GrepTool) ReadOnly() bool { return true }

// Description implements Tool.
func (GrepTool) Description() string {
	return "Search file contents with a regular expression (ripgrep). Respects " +
		".gitignore. Use 'files_with_matches' when you only need to know which " +
		"files match. Prefer this over 'grep' via bash."
}

// Schema implements Tool.
func (GrepTool) Schema() map[string]any {
	return obj(map[string]any{
		"pattern": strProp("Regular expression to search for (Rust regex syntax)."),
		"path":    strProp("Directory or file to search. Defaults to the project root."),
		"include": strProp("Glob filter for filenames, e.g. '*.go' or '**/*.{ts,tsx}'."),
		"mode": map[string]any{
			"type": "string", "enum": []string{"content", "files_with_matches", "count"},
			"description": "Output mode. Default 'content'.",
		},
		"case_insensitive": boolProp("Case-insensitive search."),
		"context":          numProp("Lines of context around each match. Default 0."),
		"limit":            numProp("Maximum matches to return. Default 100."),
	}, "pattern")
}

type grepArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Include         string `json:"include"`
	Mode            string `json:"mode"`
	CaseInsensitive bool   `json:"case_insensitive"`
	Context         int    `json:"context"`
	Limit           int    `json:"limit"`
}

// Run implements Tool.
func (t GrepTool) Run(ctx context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a grepArgs
	if err := decodeArgs(in, &a); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	if a.Pattern == "" {
		return Errf("pattern is required"), nil
	}
	searchPath := tc.Cwd
	if a.Path != "" {
		searchPath = resolvePath(tc.Cwd, a.Path)
	}
	limit := a.Limit
	if limit <= 0 {
		limit = t.MaxResults
	}
	if limit <= 0 {
		limit = 100
	}
	// Content-addressed memo: identical calls with an unchanged directory are
	// served without re-executing ripgrep. The TTL bounds staleness from
	// nested file edits, which the directory stat alone cannot see.
	key := memoKey("grep", searchPath, fileFingerprint(searchPath), a.Pattern, a.Include, a.Mode, a.CaseInsensitive, a.Context, limit)
	if cached, ok := grepMemo.get(key); ok {
		return cached, nil
	}
	if ripgrepPath == "" {
		return Errf("ripgrep (rg) not found on PATH; install it or set RICK_RIPGREP"), nil
	}

	args := []string{"--no-heading", "--color=never", "--line-number", "--with-filename"}
	switch a.Mode {
	case "files_with_matches":
		args = []string{"--files-with-matches", "--color=never"}
	case "count":
		args = []string{"--count-matches", "--color=never"}
	default:
		if a.Context > 0 {
			args = append(args, "--context", strconv.Itoa(a.Context))
		}
		args = append(args, "--max-count", strconv.Itoa(limit))
	}
	if a.CaseInsensitive {
		args = append(args, "-i")
	}
	if a.Include != "" {
		args = append(args, "--glob", a.Include)
	}
	args = append(args, "--", a.Pattern, searchPath)

	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, ripgrepPath, args...)
	cmd.Dir = tc.Cwd
	out := boundedBuffer{limit: defaultSearchOutputLimit}
	errb := boundedBuffer{limit: 64 << 10}
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()

	if out.Len() == 0 {
		if err != nil && errb.Len() > 0 && !isExitCode(err, 1) {
			return Errf("ripgrep: %s", strings.TrimSpace(errb.String())), nil
		}
		noMatch := Result{Output: "no matches found", Title: "grep " + a.Pattern}
		grepMemo.put(key, noMatch)
		return noMatch, nil
	}

	lines := []string{}
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	n := 0
	truncatedAt := 0
	for sc.Scan() {
		line := capGrepLine(compactGrepLine(tc.Cwd, sc.Text()))
		lines = append(lines, line)
		n++
		if n >= limit {
			truncatedAt = limit
			break
		}
	}
	// rg scans in filesystem order; sort by (path, line) so the output —
	// and the provider cache prefix — is byte-stable across runs.
	sortGrepLines(lines)

	output := strings.Join(lines, "\n")
	if truncatedAt > 0 {
		output += fmt.Sprintf("\n… <truncated at %d results>", truncatedAt)
	}
	if out.Truncated() {
		output += "\n… <ripgrep output capped>"
	}
	result := Result{
		Output: output,
		Title:  fmt.Sprintf("grep %q (%d)", a.Pattern, n),
		Meta:   map[string]any{"count": n},
	}
	grepMemo.put(key, result)
	return result, nil
}

func compactGrepLine(cwd, line string) string {
	if filepath.IsAbs(line) {
		return relTo(cwd, line)
	}
	// On Windows rg emits `C:\\path\\file.go:12:text`. Splitting at the
	// first colon mistakes the drive letter for the path, so locate the
	// colon that starts the line-number suffix instead.
	for i := 2; i+1 < len(line); i++ {
		if line[i] != ':' || line[i+1] < '0' || line[i+1] > '9' {
			continue
		}
		pathPart := line[:i]
		if filepath.IsAbs(pathPart) || isWindowsAbsolute(pathPart) {
			return relTo(cwd, pathPart) + line[i:]
		}
	}
	return line
}

// grepLineKey splits a "path:line:text" output line into (path, numeric line)
// so matches sort stably by path then line, independent of rg's scan order.
func grepLineKey(line string) (string, int) {
	for i := 1; i < len(line); i++ {
		if line[i] != ':' {
			continue
		}
		j := i + 1
		digits := 0
		lineNum := 0
		for j < len(line) && line[j] >= '0' && line[j] <= '9' {
			lineNum = lineNum*10 + int(line[j]-'0')
			digits++
			j++
		}
		if digits == 0 {
			continue
		}
		return line[:i], lineNum
	}
	return line, 0
}

func sortGrepLines(lines []string) {
	sort.SliceStable(lines, func(i, j int) bool {
		leftPath, leftLine := grepLineKey(lines[i])
		rightPath, rightLine := grepLineKey(lines[j])
		if leftPath != rightPath {
			return leftPath < rightPath
		}
		return leftLine < rightLine
	})
}

const maxGrepLineBytes = 4 << 10

func capGrepLine(line string) string {
	if len(line) <= maxGrepLineBytes {
		return line
	}
	suffix := fmt.Sprintf("… <line truncated at %d bytes>", maxGrepLineBytes)
	limit := maxGrepLineBytes - len(suffix)
	for limit > 0 && !utf8.RuneStart(line[limit]) {
		limit--
	}
	return line[:limit] + suffix
}

func isWindowsAbsolute(path string) bool {
	return len(path) >= 3 &&
		((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) &&
		path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}

func isExitCode(err error, code int) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode() == code
	}
	return false
}

// ---------- glob ----------

// GlobTool finds files by name pattern, newest first.
type GlobTool struct{ MaxResults int }

// Name implements Tool.
func (GlobTool) Name() string { return "glob" }

// ReadOnly implements Tool.
func (GlobTool) ReadOnly() bool { return true }

// Description implements Tool.
func (GlobTool) Description() string {
	return "Find files by glob pattern (e.g. '**/*.go', 'src/**/test_*.py'). " +
		"Respects .gitignore; a .ignore file with '!' lines can re-include " +
		"ignored paths. Results are sorted newest-modified first."
}

// Schema implements Tool.
func (GlobTool) Schema() map[string]any {
	return obj(map[string]any{
		"pattern": strProp("Glob pattern to match filenames against."),
		"path":    strProp("Directory to search. Defaults to the project root."),
		"limit":   numProp("Maximum results. Default 200."),
	}, "pattern")
}

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Limit   int    `json:"limit"`
}

// Run implements Tool.
func (t GlobTool) Run(ctx context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a globArgs
	if err := decodeArgs(in, &a); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	if a.Pattern == "" {
		a.Pattern = "**/*"
	}
	limit := a.Limit
	if limit <= 0 {
		limit = t.MaxResults
	}
	if limit <= 0 {
		limit = 200
	}
	searchPath := tc.Cwd
	if a.Path != "" {
		searchPath = resolvePath(tc.Cwd, a.Path)
	}

	var paths []string
	var scanErr error
	if ripgrepPath != "" {
		runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(runCtx, ripgrepPath,
			"--files", "--color=never", "--glob", a.Pattern, searchPath)
		cmd.Dir = tc.Cwd
		out := boundedBuffer{limit: defaultSearchOutputLimit}
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			// rg --files exits 1 for the normal "no matches" case, which is
			// not an error. Any other exit code (permission denied on a
			// subtree, unreadable path, missing binary) is a genuine failure
			// that must be surfaced instead of reported as an empty match.
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				// no matches — fine
			} else {
				scanErr = fmt.Errorf("glob: rg failed: %w", err)
			}
		}
		sc := bufio.NewScanner(strings.NewReader(out.String()))
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			if l := strings.TrimSpace(sc.Text()); l != "" {
				paths = append(paths, l)
			}
		}
	} else {
		paths = walkGlob(searchPath, a.Pattern, limit*4)
	}
	if scanErr != nil {
		return Errf("%v", scanErr), nil
	}

	entries := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		entries = append(entries, p)
	}
	// Sort by path for deterministic output; mtime ordering makes the prompt
	// bytes (and the provider cache prefix) depend on filesystem timestamps.
	sort.Slice(entries, func(i, j int) bool {
		return filepath.ToSlash(entries[i]) < filepath.ToSlash(entries[j])
	})

	truncated := false
	if len(entries) > limit {
		entries = entries[:limit]
		truncated = true
	}
	if len(entries) == 0 {
		return Result{Output: "no files matched " + a.Pattern, Title: "glob " + a.Pattern}, nil
	}

	var b strings.Builder
	for _, e := range entries {
		b.WriteString(relTo(tc.Cwd, e))
		b.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&b, "… <truncated at %d results>\n", limit)
	}
	return Result{
		Output: strings.TrimRight(b.String(), "\n"),
		Title:  fmt.Sprintf("glob %s (%d)", a.Pattern, len(entries)),
		Meta:   map[string]any{"count": len(entries)},
	}, nil
}

// walkGlob is the no-ripgrep fallback.
func walkGlob(root, pattern string, max int) []string {
	var out []string
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "dist": true,
		"build": true, "target": true, ".venv": true, "__pycache__": true,
	}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if globMatch(pattern, rel) || globMatch(pattern, d.Name()) {
			out = append(out, p)
		}
		if len(out) >= max {
			return filepath.SkipAll
		}
		return nil
	})
	return out
}

// globMatch matches a path against a pattern, falling back to the basename so
// "*.go" finds "src/main.go" the way users expect.
func globMatch(pattern, name string) bool {
	if strings.Contains(pattern, "**") {
		return glob.MatchPath(pattern, name)
	}
	if ok, err := filepath.Match(pattern, name); err == nil && ok {
		return true
	}
	ok, _ := filepath.Match(pattern, filepath.Base(name))
	return ok
}

// ---------- list ----------

// ListTool prints a directory tree.
type ListTool struct{}

// Name implements Tool.
func (ListTool) Name() string { return "list" }

// ReadOnly implements Tool.
func (ListTool) ReadOnly() bool { return true }

// Description implements Tool.
func (ListTool) Description() string {
	return "List the contents of a directory as a tree. Useful for orienting " +
		"yourself in an unfamiliar project."
}

// Schema implements Tool.
func (ListTool) Schema() map[string]any {
	return obj(map[string]any{
		"path":  strProp("Directory to list. Defaults to the project root."),
		"depth": numProp("How many levels deep to descend. Default 2."),
	})
}

type listArgs struct {
	Path  string `json:"path"`
	Depth int    `json:"depth"`
}

// Run implements Tool.
func (ListTool) Run(_ context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a listArgs
	if err := decodeArgs(in, &a); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	root := tc.Cwd
	if a.Path != "" {
		root = resolvePath(tc.Cwd, a.Path)
	}
	depth := a.Depth
	if depth <= 0 {
		depth = 2
	}
	skip := map[string]bool{".git": true, "node_modules": true, "vendor": true, ".venv": true}

	const maxListEntries = 500
	var b strings.Builder
	b.WriteString(relTo(tc.Cwd, root) + "/\n")
	count := 0
	truncated := false
	var walk func(dir, prefix string, level int)
	walk = func(dir, prefix string, level int) {
		if level > depth || count >= maxListEntries {
			truncated = count >= maxListEntries
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return entries[i].Name() < entries[j].Name()
		})
		for i, e := range entries {
			if count >= maxListEntries {
				truncated = true
				return
			}
			if strings.HasPrefix(e.Name(), ".") && e.Name() != ".rick" || skip[e.Name()] {
				continue
			}
			last := i == len(entries)-1
			branch := "├─ "
			nextPrefix := prefix + "│  "
			if last {
				branch = "└─ "
				nextPrefix = prefix + "   "
			}
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			b.WriteString(prefix + branch + name + "\n")
			count++
			if e.IsDir() {
				walk(filepath.Join(dir, e.Name()), nextPrefix, level+1)
			}
		}
	}
	walk(root, "", 1)
	if truncated {
		fmt.Fprintf(&b, "… <truncated at %d entries>\n", maxListEntries)
	}
	return Result{Output: b.String(), Title: fmt.Sprintf("list %s (%d)", relTo(tc.Cwd, root), count)}, nil
}
