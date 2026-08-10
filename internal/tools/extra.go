package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"rick/internal/config"
)

const defaultToolOutputLimit = 15 << 10
const maxFetchBytes = 8 << 10
const fetchCacheTTL = 30 * time.Second
const maxFetchCacheEntries = 32

var hrefRe = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+)["']`)
var fetchNoiseRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)

type fetchCacheEntry struct {
	created time.Time
	result  Result
}

var fetchCache = struct {
	sync.Mutex
	entries map[string]fetchCacheEntry
}{entries: make(map[string]fetchCacheEntry)}

func fetchCacheKey(url, extract string) string {
	return url + "\x00" + extract
}

func cachedFetchResult(key string) (Result, bool) {
	fetchCache.Lock()
	defer fetchCache.Unlock()
	entry, ok := fetchCache.entries[key]
	if !ok || time.Since(entry.created) >= fetchCacheTTL {
		if ok {
			delete(fetchCache.entries, key)
		}
		return Result{}, false
	}
	entry.result.Meta = map[string]any{"cached": true}
	return entry.result, true
}

func storeFetchResult(key string, result Result) {
	fetchCache.Lock()
	defer fetchCache.Unlock()
	if _, exists := fetchCache.entries[key]; !exists && len(fetchCache.entries) >= maxFetchCacheEntries {
		var oldestKey string
		var oldest time.Time
		for existingKey, entry := range fetchCache.entries {
			if oldestKey == "" || entry.created.Before(oldest) {
				oldestKey, oldest = existingKey, entry.created
			}
		}
		delete(fetchCache.entries, oldestKey)
	}
	result.Meta = nil
	fetchCache.entries[key] = fetchCacheEntry{created: time.Now(), result: result}
	persistFetchCache()
}

func resetFetchCache() {
	fetchCache.Lock()
	fetchCache.entries = make(map[string]fetchCacheEntry)
	fetchCache.Unlock()
}

// --- disk persistence ---

var fetchCacheFile = filepath.Join(config.GlobalDir(), "fetch_cache.json")

type diskFetchEntry struct {
	Key     string    `json:"key"`
	Created time.Time `json:"created"`
	Output  string    `json:"output"`
	Title   string    `json:"title"`
}

// persistFetchCache mirrors the in-memory fetch cache to disk so repeated
// fetches survive process restarts within the TTL. fetchCache must be held.
func persistFetchCache() {
	entries := make([]diskFetchEntry, 0, len(fetchCache.entries))
	for key, entry := range fetchCache.entries {
		entries = append(entries, diskFetchEntry{
			Key: key, Created: entry.created, Output: entry.result.Output, Title: entry.result.Title,
		})
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	tmp := fetchCacheFile + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, fetchCacheFile)
	}
}

func loadFetchCache() {
	data, err := os.ReadFile(fetchCacheFile)
	if err != nil {
		return
	}
	var entries []diskFetchEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return
	}
	for _, e := range entries {
		if time.Since(e.Created) >= fetchCacheTTL {
			continue
		}
		if _, exists := fetchCache.entries[e.Key]; exists || len(fetchCache.entries) >= maxFetchCacheEntries {
			continue
		}
		fetchCache.entries[e.Key] = fetchCacheEntry{
			created: e.Created,
			result:  Result{Output: e.Output, Title: e.Title},
		}
	}
}

func init() {
	loadFetchCache()
}

func runBoundedCommand(ctx context.Context, cwd, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = cwd
	out := boundedBuffer{limit: defaultToolOutputLimit}
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.Output(), err
}

// GitTool provides structured access to git operations: status, diff, log, branches.
type GitTool struct{}

func (GitTool) Name() string { return "git" }

func (GitTool) ReadOnly() bool { return true }

func (GitTool) Description() string {
	return "Structured git operations: status, diff, log, branches, or changed_files."
}

func (GitTool) Schema() map[string]any {
	return obj(map[string]any{
		"action": enumProp("What to do.", "status", "diff", "log", "branches", "changed_files"),
		"path":   pathProp("Specific file path for diff (optional)."),
		"staged": boolProp("Show staged diff instead of unstaged."),
		"count":  strProp("Number of log entries (default 10)."),
		"since":  strProp("Ref to compare against for changed_files (default HEAD~1)."),
	}, "action")
}

type gitArgs struct {
	Action string `json:"action"`
	Path   string `json:"path"`
	Staged bool   `json:"staged"`
	Count  string `json:"count"`
	Since  string `json:"since"`
}

func (GitTool) Run(ctx context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a gitArgs
	if err := RepairDecode(in, &a, GitTool{}.Schema(), tc.Repair); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	if a.Count == "" {
		a.Count = "10"
	}
	if a.Since == "" {
		a.Since = "HEAD~1"
	}

	switch a.Action {
	case "status":
		res, err := gitRun(ctx, tc.Cwd, "status", "--short", "--branch")
		return repairNote(res, noteOf(tc)), err
	case "diff":
		args := []string{"diff"}
		if a.Staged {
			args = append(args, "--staged")
		}
		args = append(args, "--")
		if a.Path != "" {
			args = append(args, a.Path)
		} else {
			args = append(args, ".")
		}
		res, err := gitRun(ctx, tc.Cwd, args...)
		return repairNote(res, noteOf(tc)), err
	case "log":
		res, err := gitRun(ctx, tc.Cwd, "log", "--oneline", "-n", a.Count)
		return repairNote(res, noteOf(tc)), err
	case "branches":
		res, err := gitRun(ctx, tc.Cwd, "branch", "--list", "--no-color")
		return repairNote(res, noteOf(tc)), err
	case "changed_files":
		res, err := gitRun(ctx, tc.Cwd, "diff", "--name-only", a.Since, "--", ".")
		return repairNote(res, noteOf(tc)), err
	default:
		return Errf("unknown action %q", a.Action), nil
	}
}

func gitRun(ctx context.Context, cwd string, args ...string) (Result, error) {
	out, err := runBoundedCommand(ctx, cwd, "git", args...)
	if err != nil {
		return Errf("git %s: %s\n%s", strings.Join(args, " "), err, strings.TrimSpace(out)), nil
	}
	s := strings.TrimSpace(out)
	if s == "" {
		return Result{Output: "(no output)", Title: "git " + strings.Join(args[:min(2, len(args))], " ")}, nil
	}
	return Result{Output: s, Title: "git " + strings.Join(args[:min(2, len(args))], " ")}, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// DiagnosticsTool captures compiler/type errors for the current package.
type DiagnosticsTool struct{}

func (DiagnosticsTool) Name() string { return "diagnostics" }

func (DiagnosticsTool) ReadOnly() bool { return true }

func (DiagnosticsTool) Description() string {
	return "Capture compiler and type errors for the current Go package. Runs 'go build ./...' and returns failures in a compact format."
}

func (DiagnosticsTool) Schema() map[string]any {
	return obj(map[string]any{
		"scope": strProp("Package path to check (default '.')."),
	})
}

func (DiagnosticsTool) Run(ctx context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a struct {
		Scope string `json:"scope"`
	}
	if err := RepairDecode(in, &a, DiagnosticsTool{}.Schema(), tc.Repair); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	if a.Scope == "" {
		a.Scope = "."
	}

	out, err := runBoundedCommand(ctx, tc.Cwd, "go", "build", a.Scope)
	if err == nil {
		return repairNote(Result{Output: fmt.Sprintf("no errors in %s", a.Scope), Title: "diagnostics"}, noteOf(tc)), nil
	}
	return repairNote(Result{Output: fmt.Sprintf("go build %s:\n%s", a.Scope, strings.TrimSpace(out)), Title: "build errors"}, noteOf(tc)), nil
}

// TestTool runs scoped Go tests with failures-only output.
type TestTool struct{}

func (TestTool) Name() string { return "test" }

func (TestTool) ReadOnly() bool { return false }

func (TestTool) Description() string {
	return "Run Go tests for a package. Returns compact pass/fail summary."
}

func (TestTool) Schema() map[string]any {
	return obj(map[string]any{
		"scope":   strProp("Package path to test (default '.')."),
		"verbose": boolProp("Verbose output (default false)."),
		"related": boolProp("Run only tests related to current changes (default false)."),
	})
}

func (TestTool) Run(ctx context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a struct {
		Scope   string `json:"scope"`
		Verbose bool   `json:"verbose"`
		Related bool   `json:"related"`
	}
	if err := RepairDecode(in, &a, TestTool{}.Schema(), tc.Repair); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	if a.Scope == "" {
		a.Scope = "."
	}

	args := []string{"test"}
	if a.Verbose {
		args = append(args, "-v")
	}
	if a.Related {
		args = append(args, "-run", "TestRelated")
	}
	args = append(args, a.Scope)

	out, err := runBoundedCommand(ctx, tc.Cwd, "go", args...)

	if err == nil {
		return repairNote(Result{Output: fmt.Sprintf("PASS: go test %s", a.Scope), Title: "test " + a.Scope}, noteOf(tc)), nil
	}
	return repairNote(Result{Output: fmt.Sprintf("FAIL: go test %s\n%s", a.Scope, strings.TrimSpace(out)), Title: "test " + a.Scope}, noteOf(tc)), nil
}

// TreeTool provides a token-capped directory listing.
type TreeTool struct{}

func (TreeTool) Name() string { return "tree" }

func (TreeTool) ReadOnly() bool { return true }

func (TreeTool) Description() string {
	return "List directory structure as a tree. Token-capped to avoid context overflow."
}

func (TreeTool) Schema() map[string]any {
	return obj(map[string]any{
		"path":    pathProp("Directory to list (default project root)."),
		"depth":   strProp("Max depth (default 3)."),
		"pattern": strProp("Glob pattern to filter (e.g. '*.go', '*.md')."),
	}, "path")
}

func (TreeTool) Run(_ context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a struct {
		Path    string `json:"path"`
		Depth   int    `json:"depth"`
		Pattern string `json:"pattern"`
	}
	if err := RepairDecode(in, &a, TreeTool{}.Schema(), tc.Repair); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	if a.Path == "" {
		a.Path = tc.Cwd
	}
	if a.Depth == 0 {
		a.Depth = 3
	}

	tree, err := buildTree(a.Path, a.Depth, a.Pattern, "")
	if err != nil {
		return Errf("tree: %v", err), nil
	}
	return repairNote(Result{Output: tree, Title: "tree " + a.Path}, noteOf(tc)), nil
}

func buildTree(root string, depth int, pattern string, indent string) (string, error) {
	var out boundedBuffer
	out.limit = defaultToolOutputLimit
	if err := buildTreeInto(root, depth, pattern, indent, &out); err != nil {
		return "", err
	}
	return out.Output(), nil
}

func buildTreeInto(root string, depth int, pattern string, indent string, out *boundedBuffer) error {
	if depth <= 0 || out.Truncated() {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if out.Truncated() {
			return nil
		}
		if strings.HasPrefix(e.Name(), ".") || e.Name() == "node_modules" || e.Name() == "vendor" {
			continue
		}
		full := filepath.Join(root, e.Name())
		if e.IsDir() {
			fmt.Fprintf(out, "%s%s/\n", indent, e.Name())
			if err := buildTreeInto(full, depth-1, pattern, indent+"  ", out); err != nil {
				continue
			}
		} else {
			if pattern != "" && !matchGlob(e.Name(), pattern) {
				continue
			}
			fmt.Fprintf(out, "%s%s\n", indent, e.Name())
		}
	}
	return nil
}

func matchGlob(name, pattern string) bool {
	matched, _ := matchGlobHelper(name, pattern)
	return matched
}

func matchGlobHelper(name, pattern string) (bool, error) {
	if pattern == "*" {
		return true, nil
	}
	if strings.HasPrefix(pattern, "*.") {
		ext := pattern[1:]
		return strings.HasSuffix(name, ext), nil
	}
	return name == pattern, nil
}

// FetchTool does safe HTTP GET and returns compact Markdown.
type FetchTool struct{}

func (FetchTool) Name() string { return "fetch" }

func (FetchTool) ReadOnly() bool { return true }

func (FetchTool) Description() string {
	return "Fetch a URL and return compact text. Only HTTP/HTTPS GET. Max 8KB returned."
}

func (FetchTool) Schema() map[string]any {
	return obj(map[string]any{
		"url":     strProp("URL to fetch."),
		"extract": strProp("Optional: 'text' for plain text, 'links' for links only."),
	}, "url")
}

var fetchHTTPClient = &http.Client{Timeout: 10 * time.Second}

func (FetchTool) Run(ctx context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a struct {
		URL     string `json:"url"`
		Extract string `json:"extract"`
	}
	if err := RepairDecode(in, &a, FetchTool{}.Schema(), tc.Repair); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	if !strings.HasPrefix(a.URL, "http://") && !strings.HasPrefix(a.URL, "https://") {
		return Errf("only HTTP/HTTPS URLs allowed"), nil
	}
	extract := strings.ToLower(strings.TrimSpace(a.Extract))
	if extract == "" {
		extract = "text"
	}
	if extract != "text" && extract != "links" {
		return Errf("extract must be 'text' or 'links'"), nil
	}
	cacheKey := fetchCacheKey(a.URL, extract)
	if result, ok := cachedFetchResult(cacheKey); ok {
		return result, nil
	}

	req, err := http.NewRequestWithContext(ctx, "GET", a.URL, nil)
	if err != nil {
		return Errf("request: %v", err), nil
	}
	req.Header.Set("User-Agent", "rick-agent/1.0")
	resp, err := fetchHTTPClient.Do(req)
	if err != nil {
		return Errf("fetch: %v", err), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return Errf("read response: %v", err), nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Errf("fetch: HTTP %s", resp.Status), nil
	}
	truncated := len(body) > maxFetchBytes
	if truncated {
		body = body[:maxFetchBytes]
	}
	content := extractFetchedContent(string(body), resp.Header.Get("Content-Type"), extract)
	if truncated {
		content += "\n… <response capped at 8 KiB>"
	}
	result := Result{Output: content, Title: a.URL}
	if extract == "links" {
		result.Title = "links: " + a.URL
	}
	storeFetchResult(cacheKey, result)
	return repairNote(result, noteOf(tc)), nil
}

func extractFetchedContent(body, contentType, extract string) string {
	isHTML := strings.Contains(strings.ToLower(contentType), "html") || strings.Contains(strings.ToLower(body), "<html")
	if extract == "links" {
		if isHTML {
			return extractHTMLLinks(body)
		}
		var links []string
		for _, line := range strings.Split(body, "\n") {
			if idx := strings.Index(line, "http"); idx >= 0 {
				end := strings.IndexAny(line[idx:], " 	\"'>")
				if end > 0 {
					links = append(links, line[idx:idx+end])
				}
			}
		}
		return strings.Join(links, "\n")
	}
	if isHTML {
		body = fetchNoiseRe.ReplaceAllString(body, " ")
		body = tagRe.ReplaceAllString(body, " ")
	}
	return cleanHTML(body)
}

func extractHTMLLinks(body string) string {
	var links []string
	for _, match := range hrefRe.FindAllStringSubmatch(body, -1) {
		if len(match) > 1 && (strings.HasPrefix(match[1], "http://") || strings.HasPrefix(match[1], "https://")) {
			links = append(links, match[1])
		}
	}
	return strings.Join(links, "\n")
}

// MemoryTool persists and retrieves project facts (decisions, conventions).
type MemoryTool struct{}

const (
	maxMemoryValueBytes = 64 << 10
	maxMemoryFileBytes  = maxMemoryValueBytes + 4096
	maxMemoryEntries    = 100
)

func (MemoryTool) Name() string { return "memory" }

func (MemoryTool) ReadOnly() bool { return false }

func (MemoryTool) Description() string {
	return "Persistent project memory in .rick/memory/: store, get, list, or delete facts."
}

func (MemoryTool) Schema() map[string]any {
	return obj(map[string]any{
		"action": enumProp("What to do.", "store", "get", "list", "delete"),
		"key":    strProp("Key for the fact (e.g. 'auth_strategy')."),
		"value":  strProp("Value to store (required for store action)."),
	}, "action")
}

func (MemoryTool) Run(_ context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a struct {
		Action string `json:"action"`
		Key    string `json:"key"`
		Value  string `json:"value"`
	}
	if err := RepairDecode(in, &a, MemoryTool{}.Schema(), tc.Repair); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	if a.Action == "" {
		return Errf("action is required"), nil
	}

	memDir := filepath.Join(tc.Cwd, ".rick", "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		return Errf("memory directory: %v", err), nil
	}

	switch a.Action {
	case "store":
		if a.Key == "" || a.Value == "" {
			return Errf("key and value required"), nil
		}
		if len(a.Value) > maxMemoryValueBytes {
			return Errf("value exceeds the memory limit of %d bytes", maxMemoryValueBytes), nil
		}
		path := filepath.Join(memDir, sanitizeKey(a.Key)+".json")
		data, err := json.Marshal(map[string]string{"key": a.Key, "value": a.Value})
		if err != nil {
			return Errf("store: %v", err), nil
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return Errf("store: %v", err), nil
		}
		return repairNote(Result{Output: fmt.Sprintf("stored %s", a.Key), Title: "memory"}, noteOf(tc)), nil
	case "get":
		if a.Key == "" {
			return Errf("key required"), nil
		}
		path := filepath.Join(memDir, sanitizeKey(a.Key)+".json")
		file, err := os.Open(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return Errf("get: %v", err), nil
			}
			return repairNote(Result{Output: fmt.Sprintf("no memory for %s", a.Key), Title: "memory"}, noteOf(tc)), nil
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, maxMemoryFileBytes+1))
		if err != nil {
			return Errf("get: %v", err), nil
		}
		if len(data) > maxMemoryFileBytes {
			return Errf("get: memory file exceeds the limit of %d bytes", maxMemoryFileBytes), nil
		}
		var m map[string]string
		if err := json.Unmarshal(data, &m); err != nil {
			return Errf("get: invalid memory file: %v", err), nil
		}
		return repairNote(Result{Output: m["value"], Title: "memory " + a.Key}, noteOf(tc)), nil
	case "list":
		entries, err := os.ReadDir(memDir)
		if err != nil && !os.IsNotExist(err) {
			return Errf("list: %v", err), nil
		}
		var keys []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				keys = append(keys, strings.TrimSuffix(e.Name(), ".json"))
				if len(keys) == maxMemoryEntries {
					break
				}
			}
		}
		return repairNote(Result{Output: strings.Join(keys, "\n"), Title: "memory"}, noteOf(tc)), nil
	case "delete":
		if a.Key == "" {
			return Errf("key required"), nil
		}
		if err := os.Remove(filepath.Join(memDir, sanitizeKey(a.Key)+".json")); err != nil && !os.IsNotExist(err) {
			return Errf("delete: %v", err), nil
		}
		return repairNote(Result{Output: fmt.Sprintf("deleted %s", a.Key), Title: "memory"}, noteOf(tc)), nil
	default:
		return Errf("unknown action %q", a.Action), nil
	}
}

func sanitizeKey(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	var b strings.Builder
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ' || r == '/' || r == '\\':
			b.WriteByte('_')
		}
	}
	k = strings.Trim(b.String(), ".")
	if k == "" {
		return "memory"
	}
	if len(k) > 50 {
		k = k[:50]
	}
	return k
}
