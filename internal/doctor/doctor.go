// Package doctor runs local diagnostics on a rick installation.
package doctor

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"rick/internal/config"
	"rick/internal/maintenance"
	"rick/internal/plugin"
	"rick/internal/theme"
)

// Status levels reported by each check.
const (
	StatusPass = "PASS"
	StatusWarn = "WARN"
	StatusFail = "FAIL"
)

// Check is one diagnostic and its outcome.
type Check struct {
	Category string
	Name     string
	Status   string
	Message  string
}

// CheckNetwork controls whether network probes are run.
var CheckNetwork bool

// RunChecks executes every diagnostic and returns them sorted by category.
func RunChecks() []Check {
	loaded, loadErr := config.Load(".")

	checks := []Check{
		mk("toolchain", "go version", checkGoVersion),
		mk("toolchain", "ripgrep", checkRipgrep),
		mk("toolchain", "git", checkGit),
		mkArg("config", "config load", func() (string, string) { return checkConfigLoad(loaded, loadErr) }),
		mkArg("config", "provider credentials", func() (string, string) { return checkCredentials(loaded) }),
		mkArg("config", "mcp servers", func() (string, string) { return checkMCPServers(loaded) }),
		mk("storage", "data directory", checkDataDir),
		mk("storage", "themes", checkThemes),
		mk("storage", "snapshots", checkSnapshots),
		mk("storage", "stale executables", checkStaleBinaries),
		mkArg("storage", "plugins", func() (string, string) { return checkPlugins(loaded) }),
		mk("network", "connectivity", checkConnectivity),
	}

	sort.SliceStable(checks, func(i, j int) bool {
		if checks[i].Category != checks[j].Category {
			return categoryRank(checks[i].Category) < categoryRank(checks[j].Category)
		}
		return false
	})
	return checks
}

func categoryRank(c string) int {
	switch c {
	case "toolchain":
		return 0
	case "config":
		return 1
	case "storage":
		return 2
	case "network":
		return 3
	}
	return 4
}

func mk(category, name string, fn func() (string, string)) Check {
	status, msg := fn()
	return Check{Category: category, Name: name, Status: status, Message: msg}
}

func mkArg(category, name string, fn func() (string, string)) Check {
	return mk(category, name, fn)
}

func checkGoVersion() (string, string) {
	v := runtime.Version()
	major, minor, ok := parseGoVersion(v)
	if !ok {
		return StatusWarn, fmt.Sprintf("unrecognised runtime version %q", v)
	}
	if major > 1 || (major == 1 && minor >= 24) {
		return StatusPass, fmt.Sprintf("%s (%s/%s)", v, runtime.GOOS, runtime.GOARCH)
	}
	return StatusFail, fmt.Sprintf("%s is older than the required go1.24", v)
}

func parseGoVersion(v string) (int, int, bool) {
	s := strings.TrimPrefix(v, "go")
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minorStr := parts[1]
	for i, r := range minorStr {
		if r < '0' || r > '9' {
			minorStr = minorStr[:i]
			break
		}
	}
	minor, err := strconv.Atoi(minorStr)
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

func checkRipgrep() (string, string) {
	p, err := exec.LookPath("rg")
	if err != nil {
		return StatusWarn, "rg not found on PATH (grep falls back to a slower scanner)"
	}
	return StatusPass, p
}

func checkGit() (string, string) {
	p, err := exec.LookPath("git")
	if err != nil {
		return StatusFail, "git not found on PATH"
	}
	return StatusPass, p
}

func checkConfigLoad(loaded *config.Loaded, err error) (string, string) {
	if err != nil {
		return StatusFail, "config.Load: " + err.Error()
	}
	if loaded == nil {
		return StatusFail, "config.Load returned no configuration"
	}
	if len(loaded.Sources) == 0 {
		return StatusWarn, "built-in defaults only (no rick.json found)"
	}
	return StatusPass, fmt.Sprintf("%d source(s): %s", len(loaded.Sources), strings.Join(loaded.Sources, ", "))
}

func checkCredentials(loaded *config.Loaded) (string, string) {
	if loaded == nil {
		return StatusFail, "no configuration loaded"
	}
	cfg := loaded.Config
	if creds, err := config.LoadCredentials(); err == nil {
		config.MergeCredentials(&cfg, creds)
	}
	if len(cfg.Providers) == 0 {
		return StatusWarn, "no providers configured (run rickauth or set ANTHROPIC_API_KEY)"
	}

	var ready, missing []string
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := cfg.Providers[name]
		if p.Enabled != nil && !*p.Enabled {
			continue
		}
		switch {
		case p.APIKey != "":
			ready = append(ready, name)
		case envKeyFor(name) != "":
			ready = append(ready, name+" (env)")
		case p.BaseURL != "":
			ready = append(ready, name+" (base url, no key)")
		default:
			missing = append(missing, name)
		}
	}
	if len(ready) == 0 {
		return StatusFail, "no provider has credentials: " + strings.Join(missing, ", ")
	}
	msg := "ready: " + strings.Join(ready, ", ")
	if len(missing) > 0 {
		return StatusWarn, msg + " · missing: " + strings.Join(missing, ", ")
	}
	return StatusPass, msg
}

func envKeyFor(name string) string {
	upper := strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
	for _, suffix := range []string{"_API_KEY", "_KEY", "_TOKEN"} {
		if v := os.Getenv(upper + suffix); v != "" {
			return v
		}
	}
	return ""
}

func checkMCPServers(loaded *config.Loaded) (string, string) {
	if loaded == nil {
		return StatusWarn, "no configuration loaded"
	}
	servers := loaded.Config.MCP
	if len(servers) == 0 {
		return StatusPass, "none configured"
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	var ok, bad []string
	for _, name := range names {
		s := servers[name]
		if s.Enabled != nil && !*s.Enabled {
			continue
		}
		if len(s.Command) == 0 {
			if s.URL != "" {
				ok = append(ok, name+" (remote)")
				continue
			}
			bad = append(bad, name+" (no command or url)")
			continue
		}
		if _, err := exec.LookPath(s.Command[0]); err != nil {
			bad = append(bad, fmt.Sprintf("%s (%s not on PATH)", name, s.Command[0]))
			continue
		}
		ok = append(ok, name)
	}
	if len(bad) > 0 {
		return StatusFail, fmt.Sprintf("%d ok, broken: %s", len(ok), strings.Join(bad, ", "))
	}
	return StatusPass, fmt.Sprintf("%d server(s): %s", len(ok), strings.Join(ok, ", "))
}

func checkDataDir() (string, string) {
	dir := config.DataDir()
	if dir == "" {
		return StatusFail, "config.DataDir() is empty"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return StatusFail, fmt.Sprintf("%s: %v", dir, err)
	}
	probe := filepath.Join(dir, ".rickdoctor-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return StatusFail, fmt.Sprintf("%s is not writable: %v", dir, err)
	}
	_ = os.Remove(probe)
	return StatusPass, dir + " (writable)"
}

// checkSnapshots warns when shadow-repo snapshot trees are stale or
// numerous: a tree older than the retention window means rick once
// shadow-repo'd a folder it no longer touches, and every GB of it is waste.
func checkSnapshots() (string, string) {
	root := filepath.Join(config.DataDir(), "snapshots")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return StatusPass, "no snapshot trees"
	}
	if err != nil {
		return StatusWarn, fmt.Sprintf("snapshots: %v", err)
	}
	cutoff := time.Now().Add(-maintenance.SnapshotRetentionMaxAge)
	stale := 0
	total := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		total++
		if info, err := entry.Info(); err == nil && info.ModTime().Before(cutoff) {
			stale++
		}
	}
	if stale > 0 {
		return StatusWarn, fmt.Sprintf("%d tree(s), %d stale (untouched >%s) — run `rick maintenance prune-snapshots`", total, stale, maintenance.SnapshotRetentionMaxAge)
	}
	return StatusPass, fmt.Sprintf("%d tree(s), none stale", total)
}

// checkStaleBinaries warns about leftover executables next to the running
// rick binary (deploy backups, benchmark builds, renamed test images) that
// are not part of the install and only waste disk.
func checkStaleBinaries() (string, string) {
	exe, err := os.Executable()
	if err != nil {
		return StatusWarn, fmt.Sprintf("cannot resolve executable: %v", err)
	}
	dir := filepath.Dir(exe)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return StatusWarn, fmt.Sprintf("cannot list %s: %v", dir, err)
	}
	self := strings.ToLower(filepath.Base(exe))
	var stale []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		lower := strings.ToLower(name)
		if (strings.HasPrefix(lower, "rick.before-") || strings.HasSuffix(lower, ".test.exe")) &&
			lower != self {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		return StatusWarn, fmt.Sprintf("stale executables in %s: %s", dir, strings.Join(stale, ", "))
	}
	return StatusPass, dir + " (clean)"
}

func checkThemes() (string, string) {
	dirs := []string{filepath.Join(config.GlobalDir(), "themes")}
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, filepath.Join(cwd, ".rick", "themes"))
	}
	reg := theme.Load(dirs...)
	if reg == nil {
		return StatusFail, "theme registry failed to load"
	}
	names := reg.Names()
	if len(names) == 0 {
		return StatusFail, "no themes available (built-ins missing)"
	}

	var broken []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if _, err := theme.LoadFromFile(path); err != nil {
				broken = append(broken, e.Name())
			}
		}
	}
	if len(broken) > 0 {
		return StatusWarn, fmt.Sprintf("%d theme(s) loaded, failed to parse: %s",
			len(names), strings.Join(broken, ", "))
	}
	return StatusPass, fmt.Sprintf("%d theme(s) available", len(names))
}

func checkPlugins(loaded *config.Loaded) (string, string) {
	globalDir := filepath.Join(config.GlobalDir(), "plugins")
	projectDir := ""
	if loaded != nil {
		projectDir = filepath.Join(loaded.ProjectRoot, ".rick", "plugins")
	}

	manifests, errs := plugin.LoadAll(globalDir, projectDir, nil)
	var found []string
	for _, m := range manifests {
		found = append(found, m.Name)
	}
	sort.Strings(found)

	if len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return StatusWarn, fmt.Sprintf("%d plugin(s) loaded, %d error(s): %s",
			len(found), len(errs), strings.Join(msgs, "; "))
	}
	if len(found) == 0 {
		return StatusPass, "none installed"
	}
	return StatusPass, fmt.Sprintf("%d plugin(s): %s", len(found), strings.Join(found, ", "))
}

func checkConnectivity() (string, string) {
	if !CheckNetwork {
		return StatusWarn, "skipped (pass --network to probe provider endpoints)"
	}
	hosts := []string{"chat.openai.com:443", "api.anthropic.com:443", "openrouter.ai:443"}
	var ok, bad []string
	for _, h := range hosts {
		conn, err := net.DialTimeout("tcp", h, 3*time.Second)
		if err != nil {
			bad = append(bad, strings.SplitN(h, ":", 2)[0])
			continue
		}
		_ = conn.Close()
		ok = append(ok, strings.SplitN(h, ":", 2)[0])
	}
	if len(ok) == 0 {
		return StatusFail, "no provider endpoint reachable: " + strings.Join(bad, ", ")
	}
	if len(bad) > 0 {
		return StatusWarn, "reachable: " + strings.Join(ok, ", ") + " · unreachable: " + strings.Join(bad, ", ")
	}
	return StatusPass, "reachable: " + strings.Join(ok, ", ")
}

// PrintReport writes the status table and a summary line.
func PrintReport(checks []Check) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CATEGORY\tCHECK\tSTATUS\tDETAIL")
	var pass, warn, fail int
	for _, c := range checks {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Category, c.Name, c.Status, c.Message)
		switch c.Status {
		case StatusPass:
			pass++
		case StatusWarn:
			warn++
		case StatusFail:
			fail++
		}
	}
	_ = w.Flush()
	fmt.Printf("\n%d passed / %d warnings / %d failures\n", pass, warn, fail)

	for _, c := range checks {
		switch c.Status {
		case StatusWarn:
			fmt.Fprintf(os.Stderr, "warning: %s: %s\n", c.Name, c.Message)
		case StatusFail:
			fmt.Fprintf(os.Stderr, "failure: %s: %s\n", c.Name, c.Message)
		}
	}
}

// ExitCode returns the appropriate exit code for a set of checks.
func ExitCode(checks []Check) int {
	for _, c := range checks {
		if c.Status == StatusFail {
			return 1
		}
	}
	return 0
}
