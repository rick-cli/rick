package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Load resolves the full config chain for a working directory.
//
// Precedence (later wins):
//  1. built-in defaults
//  2. global   ~/.config/rick/{rick,tui}.json
//  3. RICK_CONFIG  (explicit file path)
//  4. project  <root>/rick.json + <root>/tui.json  (and .rick/ variants)
//  5. RICK_CONFIG_CONTENT (inline JSON override)
//
// Results are memoized per working directory, keyed by the mtimes of every
// input file plus the RICK_CONFIG* environment values, so the (expensive)
// JSONC parse chain runs once per process. Callers always receive a copy, so
// mutating the returned config can never poison the cache.
func Load(cwd string) (*Loaded, error) {
	root := cachedProjectRoot(cwd)
	fp := configFingerprint(cwd, root)

	loadCacheMu.Lock()
	if entry, ok := loadCache[cwd]; ok && entry.fingerprint == fp {
		loadCacheMu.Unlock()
		return cloneLoaded(entry.loaded), nil
	}
	loadCacheMu.Unlock()

	loaded, err := loadUncached(cwd)
	if err != nil {
		return nil, err
	}
	loadCacheMu.Lock()
	loadCache[cwd] = &cachedLoad{fingerprint: fp, loaded: loaded}
	if len(loadCache) > 64 {
		for k := range loadCache {
			if k != cwd {
				delete(loadCache, k)
				break
			}
		}
	}
	loadCacheMu.Unlock()
	return cloneLoaded(loaded), nil
}

type cachedLoad struct {
	fingerprint string
	loaded      *Loaded
}

var (
	loadCacheMu sync.Mutex
	loadCache   = map[string]*cachedLoad{}
	rootCacheMu sync.Mutex
	rootCache   = map[string]string{}
)

// cachedProjectRoot memoizes the FindProjectRoot walk per working directory.
func cachedProjectRoot(cwd string) string {
	rootCacheMu.Lock()
	if root, ok := rootCache[cwd]; ok {
		rootCacheMu.Unlock()
		return root
	}
	rootCacheMu.Unlock()
	root := FindProjectRoot(cwd)
	rootCacheMu.Lock()
	rootCache[cwd] = root
	if len(rootCache) > 128 {
		for k := range rootCache {
			if k != cwd {
				delete(rootCache, k)
				break
			}
		}
	}
	rootCacheMu.Unlock()
	return root
}

// configFingerprint captures every input Load reads: the mtimes and sizes of
// the candidate config files plus the RICK_CONFIG* environment overrides.
func configFingerprint(cwd, root string) string {
	g := GlobalDir()
	candidates := []string{
		firstExisting(filepath.Join(g, "rick.jsonc"), filepath.Join(g, "rick.json")),
		firstExisting(filepath.Join(g, "tui.jsonc"), filepath.Join(g, "tui.json")),
	}
	if p := os.Getenv("RICK_CONFIG"); p != "" {
		candidates = append(candidates, p)
	}
	candidates = append(candidates,
		firstExisting(
			filepath.Join(root, "rick.jsonc"), filepath.Join(root, "rick.json"),
			filepath.Join(root, ".rick", "rick.jsonc"), filepath.Join(root, ".rick", "rick.json"),
		),
		firstExisting(
			filepath.Join(root, "tui.jsonc"), filepath.Join(root, "tui.json"),
			filepath.Join(root, ".rick", "tui.jsonc"), filepath.Join(root, ".rick", "tui.json"),
		),
	)
	var b strings.Builder
	b.WriteString("cwd=" + cwd + ";")
	for _, p := range candidates {
		if p == "" {
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			b.WriteString(p + ":gone;")
			continue
		}
		fmt.Fprintf(&b, "%s:%d:%d;", p, st.Size(), st.ModTime().UnixNano())
	}
	b.WriteString("rc=" + os.Getenv("RICK_CONFIG") + ";")
	b.WriteString("ri=" + os.Getenv("RICK_CONFIG_CONTENT") + ";")
	return b.String()
}

// cloneLoaded copies a cached Loaded so callers can mutate it freely.
func cloneLoaded(l *Loaded) *Loaded {
	out := &Loaded{ProjectRoot: l.ProjectRoot, SandboxRoot: l.SandboxRoot}
	out.Sources = append([]string(nil), l.Sources...)
	out.Config = cloneConfig(l.Config)
	out.TUI = cloneTUI(l.TUI)
	return out
}

func cloneConfig(c Config) Config {
	var out Config
	if data, err := json.Marshal(c); err == nil && json.Unmarshal(data, &out) == nil {
		return out
	}
	return c
}

func cloneTUI(t TUI) TUI {
	var out TUI
	if data, err := json.Marshal(t); err == nil && json.Unmarshal(data, &out) == nil {
		return out
	}
	return t
}

// loadUncached performs the full file parse chain without any memoization.
func loadUncached(cwd string) (*Loaded, error) {
	cfg, tui := Defaults()
	root := FindProjectRoot(cwd)
	l := &Loaded{ProjectRoot: root}

	apply := func(path string) {
		if path == "" {
			return
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		base := filepath.Dir(path)
		if err := mergeInto(&cfg, &tui, raw, base, filepath.Base(path)); err != nil {
			l.Sources = append(l.Sources, fmt.Sprintf("%s (ERROR: %v)", path, err))
			return
		}
		l.Sources = append(l.Sources, path)
	}

	g := GlobalDir()
	apply(firstExisting(filepath.Join(g, "rick.jsonc"), filepath.Join(g, "rick.json")))
	apply(firstExisting(filepath.Join(g, "tui.jsonc"), filepath.Join(g, "tui.json")))

	if p := os.Getenv("RICK_CONFIG"); p != "" {
		apply(p)
	}

	apply(firstExisting(
		filepath.Join(root, "rick.jsonc"), filepath.Join(root, "rick.json"),
		filepath.Join(root, ".rick", "rick.jsonc"), filepath.Join(root, ".rick", "rick.json"),
	))
	apply(firstExisting(
		filepath.Join(root, "tui.jsonc"), filepath.Join(root, "tui.json"),
		filepath.Join(root, ".rick", "tui.jsonc"), filepath.Join(root, ".rick", "tui.json"),
	))

	if inline := os.Getenv("RICK_CONFIG_CONTENT"); inline != "" {
		if err := mergeInto(&cfg, &tui, []byte(inline), root, "rick.json"); err == nil {
			l.Sources = append(l.Sources, "RICK_CONFIG_CONTENT")
		} else {
			l.Sources = append(l.Sources, "RICK_CONFIG_CONTENT (ERROR: "+err.Error()+")")
		}
	}

	// Environment API keys fill in providers that lack one.
	applyEnvKeys(&cfg)
	if cfg.SubagentDepth == nil {
		depth := 1
		cfg.SubagentDepth = &depth
	}
	if err := ValidateSubagentDepth(*cfg.SubagentDepth); err != nil {
		return nil, err
	}
	if err := ValidateCacheRetention(cfg.CacheRetention); err != nil {
		return nil, err
	}
	if cfg.MaxBackground <= 0 {
		cfg.MaxBackground = 8
	}

	l.Config = cfg
	l.TUI = tui
	l.SandboxRoot = SandboxRoot(cfg.Sandbox, root)
	return l, nil
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// mergeInto decodes raw (JSONC + substitutions) and merges it key-by-key.
// A document is routed to the TUI struct if its filename starts with "tui",
// or if it contains no rick-only keys but does contain tui-only keys.
func mergeInto(cfg *Config, tui *TUI, raw []byte, baseDir, name string) error {
	clean := Substitute(StripJSONC(raw), baseDir)

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(clean, &probe); err != nil {
		return err
	}

	isTUIFile := len(name) >= 3 && name[:3] == "tui"
	if !isTUIFile && containsTUIKey(probe) {
		isTUIFile = !containsConfigKey(probe)
	}
	if isTUIFile {
		var t TUI
		if err := json.Unmarshal(clean, &t); err != nil {
			return err
		}
		mergeTUI(tui, t, probe)
		return nil
	}

	// A combined rick.json may carry a "tui" sub-object.
	if sub, ok := probe["tui"]; ok {
		var t TUI
		if err := json.Unmarshal(sub, &t); err == nil {
			var subProbe map[string]json.RawMessage
			_ = json.Unmarshal(sub, &subProbe)
			mergeTUI(tui, t, subProbe)
		}
		delete(probe, "tui")
	}

	var c Config
	if err := json.Unmarshal(clean, &c); err != nil {
		return err
	}
	mergeConfig(cfg, c, probe)
	return nil
}

func containsTUIKey(p map[string]json.RawMessage) bool {
	for key := range p {
		switch key {
		case "theme", "diff", "diff_threshold", "scroll_speed", "notifications", "show_thinking", "tool_details", "hide_status", "hide_tips", "mouse", "links", "keybinds":
			return true
		}
	}
	return false
}

func containsConfigKey(p map[string]json.RawMessage) bool {
	for key := range p {
		switch key {
		case "model", "small_model", "providers", "permissions", "sandbox", "tools", "web_search", "max_tokens":
			return true
		}
	}
	return false
}

func has(p map[string]json.RawMessage, k string) bool { _, ok := p[k]; return ok }

func mergeConfig(dst *Config, src Config, p map[string]json.RawMessage) {
	if has(p, "model") {
		dst.Model = src.Model
	}
	if has(p, "small_model") {
		dst.SmallModel = src.SmallModel
	}
	if has(p, "max_tokens") {
		dst.MaxTokens = src.MaxTokens
	}
	if has(p, "context_reserve") {
		dst.ContextReserve = src.ContextReserve
	}
	if has(p, "cache_retention") {
		dst.CacheRetention = src.CacheRetention
	}
	if has(p, "cache_warm") {
		dst.WarmCache = src.WarmCache
	}
	if has(p, "cache_max_reasoning_turns") {
		dst.CacheMaxReasoningTurns = src.CacheMaxReasoningTurns
	}
	if has(p, "cache_max_tool_result_bytes") {
		dst.CacheMaxToolResultBytes = src.CacheMaxToolResultBytes
	}
	if has(p, "autocompact") {
		dst.AutoCompact = src.AutoCompact
	}
	if has(p, "subagent_depth") {
		dst.SubagentDepth = src.SubagentDepth
	}
	if has(p, "background_notify") {
		dst.BackgroundNotify = src.BackgroundNotify
	}
	if has(p, "max_background") {
		dst.MaxBackground = src.MaxBackground
	}
	if has(p, "instructions") {
		dst.Instructions = append(dst.Instructions, src.Instructions...)
	}
	if has(p, "plugin") {
		dst.Plugins = append(dst.Plugins, src.Plugins...)
	}
	if has(p, "provider") {
		if dst.Providers == nil {
			dst.Providers = map[string]Provider{}
		}
		for k, v := range src.Providers {
			cur := dst.Providers[k]
			if v.Type != "" {
				cur.Type = v.Type
			}
			if v.APIKey != "" {
				cur.APIKey = v.APIKey
			}
			if v.BaseURL != "" {
				cur.BaseURL = v.BaseURL
			}
			if v.Enabled != nil {
				cur.Enabled = v.Enabled
			}
			dst.Providers[k] = cur
		}
	}
	if has(p, "tools") {
		if dst.Tools == nil {
			dst.Tools = map[string]bool{}
		}
		for k, v := range src.Tools {
			dst.Tools[k] = v
		}
	}
	if has(p, "agent") {
		if dst.Agents == nil {
			dst.Agents = map[string]Agent{}
		}
		for k, v := range src.Agents {
			dst.Agents[k] = mergeAgent(dst.Agents[k], v)
		}
	}
	if has(p, "mcp") {
		if dst.MCP == nil {
			dst.MCP = map[string]MCPServer{}
		}
		for k, v := range src.MCP {
			dst.MCP[k] = v
		}
	}
	if has(p, "command") {
		if dst.Commands == nil {
			dst.Commands = map[string]Command{}
		}
		for k, v := range src.Commands {
			dst.Commands[k] = v
		}
	}
	if has(p, "permission") && src.Permission != nil {
		dst.Permission = MergePermission(dst.Permission, src.Permission)
	}
	if has(p, "permission_profile") {
		if dst.Profiles == nil {
			dst.Profiles = map[string]Permission{}
		}
		for k, v := range src.Profiles {
			// A profile redefined in a closer config layer overlays the
			// outer one rather than replacing it wholesale.
			if existing, ok := dst.Profiles[k]; ok {
				dst.Profiles[k] = *MergePermission(&existing, &v)
			} else {
				dst.Profiles[k] = v
			}
		}
	}
	if has(p, "sandbox") && src.Sandbox != nil {
		dst.Sandbox = MergeSandbox(dst.Sandbox, src.Sandbox)
	}
	if has(p, "web_search") && src.WebSearch != nil {
		dst.WebSearch = mergeWebSearchConfig(dst.WebSearch, src.WebSearch)
	}
}

func mergeWebSearchConfig(base, over *WebSearchConfig) *WebSearchConfig {
	if base == nil {
		base = &WebSearchConfig{}
	}
	out := *base
	if over.AllowDomains != nil {
		out.AllowDomains = append([]string(nil), over.AllowDomains...)
	}
	if over.DenyDomains != nil {
		out.DenyDomains = append([]string(nil), over.DenyDomains...)
	}
	if over.MaxResults != 0 {
		out.MaxResults = over.MaxResults
	}
	if over.MaxSearchesPerSession != 0 {
		out.MaxSearchesPerSession = over.MaxSearchesPerSession
	}
	if over.Provider != "" {
		out.Provider = over.Provider
	}
	if over.Mode != "" {
		out.Mode = over.Mode
	}
	if over.Parallel != nil {
		out.Parallel = over.Parallel
	}
	if over.MaxParallel != 0 {
		out.MaxParallel = over.MaxParallel
	}
	if over.HedgeAfterMS != 0 {
		out.HedgeAfterMS = over.HedgeAfterMS
	}
	if over.LogicalBudget != 0 {
		out.LogicalBudget = over.LogicalBudget
	}
	if over.CacheTTLSeconds != 0 {
		out.CacheTTLSeconds = over.CacheTTLSeconds
	}
	if over.MaxConcurrent != 0 {
		out.MaxConcurrent = over.MaxConcurrent
	}
	if over.Providers != nil {
		if out.Providers == nil {
			out.Providers = map[string]WebSearchProviderConfig{}
		}
		for name, provider := range over.Providers {
			out.Providers[name] = mergeWebSearchProvider(out.Providers[name], provider)
		}
	}
	return &out
}

func mergeWebSearchProvider(base, over WebSearchProviderConfig) WebSearchProviderConfig {
	out := base
	if over.Enabled != nil {
		out.Enabled = over.Enabled
	}
	if over.ClearAPIKey {
		out.APIKey = ""
	}
	if over.ClearBaseURL {
		out.BaseURL = ""
	}
	if over.APIKey != "" {
		out.APIKey = over.APIKey
		out.ClearAPIKey = false
	}
	if over.APIKeyEnv != "" {
		out.APIKeyEnv = over.APIKeyEnv
	}
	if over.BaseURL != "" {
		out.BaseURL = over.BaseURL
		out.ClearBaseURL = false
	}
	if over.Instances != nil {
		out.Instances = append([]string(nil), over.Instances...)
	}
	if over.Kind != "" {
		out.Kind = over.Kind
	}
	if over.Backend != "" {
		out.Backend = over.Backend
	}
	if over.Region != "" {
		out.Region = over.Region
	}
	if over.SafeSearch != "" {
		out.SafeSearch = over.SafeSearch
	}
	if over.TimeRange != "" {
		out.TimeRange = over.TimeRange
	}
	if over.Type != "" {
		out.Type = over.Type
	}
	if over.Livecrawl != "" {
		out.Livecrawl = over.Livecrawl
	}
	if over.MaxAgeHours != nil {
		out.MaxAgeHours = over.MaxAgeHours
	}
	if over.IncludeDomains != nil {
		out.IncludeDomains = append([]string(nil), over.IncludeDomains...)
	}
	if over.ExcludeDomains != nil {
		out.ExcludeDomains = append([]string(nil), over.ExcludeDomains...)
	}
	if over.Weight != 0 {
		out.Weight = over.Weight
	}
	if over.Priority != 0 {
		out.Priority = over.Priority
	}
	if over.MaxRPM != 0 {
		out.MaxRPM = over.MaxRPM
	}
	if over.MaxConcurrency != 0 {
		out.MaxConcurrency = over.MaxConcurrency
	}
	if over.DailyBudget != 0 {
		out.DailyBudget = over.DailyBudget
	}
	if over.MonthlyBudget != 0 {
		out.MonthlyBudget = over.MonthlyBudget
	}
	if over.TimeoutSeconds != 0 {
		out.TimeoutSeconds = over.TimeoutSeconds
	}
	if over.CacheTTLSeconds != 0 {
		out.CacheTTLSeconds = over.CacheTTLSeconds
	}
	return out
}

func mergeAgent(dst, src Agent) Agent {
	if src.Description != "" {
		dst.Description = src.Description
	}
	if src.Mode != "" {
		dst.Mode = src.Mode
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.Temperature != nil {
		dst.Temperature = src.Temperature
	}
	if src.Prompt != "" {
		dst.Prompt = src.Prompt
	}
	if src.Tools != nil {
		if dst.Tools == nil {
			dst.Tools = map[string]bool{}
		}
		for k, v := range src.Tools {
			dst.Tools[k] = v
		}
	}
	if src.Permission != nil {
		dst.Permission = MergePermission(dst.Permission, src.Permission)
	}
	return dst
}

// MergePermission overlays src onto dst, returning a new value.
func MergePermission(dst, src *Permission) *Permission {
	out := Permission{}
	if dst != nil {
		out = *dst
		out.Bash = copyLevels(dst.Bash)
		out.Tools = copyLevels(dst.Tools)
		out.Paths = copyLevels(dst.Paths)
		out.Hosts = copyLevels(dst.Hosts)
	}
	if src == nil {
		return &out
	}
	if src.Default != "" {
		out.Default = src.Default
	}
	if src.Edit != "" {
		out.Edit = src.Edit
	}
	if src.Write != "" {
		out.Write = src.Write
	}
	if src.Read != "" {
		out.Read = src.Read
	}
	if src.WebF != "" {
		out.WebF = src.WebF
	}
	if len(src.Extends) > 0 {
		out.Extends = append([]string(nil), src.Extends...)
	}
	out.Bash = overlayLevels(out.Bash, src.Bash)
	out.Tools = overlayLevels(out.Tools, src.Tools)
	out.Paths = overlayLevels(out.Paths, src.Paths)
	out.Hosts = overlayLevels(out.Hosts, src.Hosts)
	if src.Sandbox != nil {
		out.Sandbox = MergeSandbox(out.Sandbox, src.Sandbox)
	}
	return &out
}

func copyLevels(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func overlayLevels(dst, src map[string]string) map[string]string {
	if src == nil {
		return dst
	}
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// MergeSandbox overlays src onto dst, returning a new value.
//
// Scalars replace when set; list fields replace rather than append, because a
// closer config layer narrowing "writable_roots" must not silently inherit the
// wider set from the layer above it.
func MergeSandbox(dst, src *SandboxConfig) *SandboxConfig {
	out := SandboxConfig{}
	if dst != nil {
		out = *dst
	}
	if src == nil {
		return &out
	}
	if src.Mode != "" {
		out.Mode = src.Mode
	}
	if src.Enforcement != "" {
		out.Enforcement = src.Enforcement
	}
	if src.Network != nil {
		out.Network = src.Network
	}
	if src.KeepCredentials != nil {
		out.KeepCredentials = src.KeepCredentials
	}
	if src.AllowHosts != nil {
		out.AllowHosts = src.AllowHosts
	}
	if src.DenyHosts != nil {
		out.DenyHosts = src.DenyHosts
	}
	if src.WritableRoots != nil {
		out.WritableRoots = src.WritableRoots
	}
	if src.ReadableRoots != nil {
		out.ReadableRoots = src.ReadableRoots
	}
	if src.DenyPaths != nil {
		out.DenyPaths = src.DenyPaths
	}
	if src.AllowEnv != nil {
		out.AllowEnv = src.AllowEnv
	}
	if src.DenyEnv != nil {
		out.DenyEnv = src.DenyEnv
	}
	if src.MemoryMB > 0 {
		out.MemoryMB = src.MemoryMB
	}
	if src.CPUSeconds > 0 {
		out.CPUSeconds = src.CPUSeconds
	}
	if src.Processes > 0 {
		out.Processes = src.Processes
	}
	if src.FileSizeMB > 0 {
		out.FileSizeMB = src.FileSizeMB
	}
	return &out
}

func mergeTUI(dst *TUI, src TUI, p map[string]json.RawMessage) {
	if has(p, "theme") {
		dst.Theme = src.Theme
	}
	if has(p, "diff") {
		dst.DiffMode = src.DiffMode
	}
	if has(p, "diff_threshold") {
		dst.DiffThreshold = src.DiffThreshold
	}
	if has(p, "scroll_speed") {
		dst.ScrollSpeed = src.ScrollSpeed
	}
	if has(p, "notifications") {
		dst.Notifications = src.Notifications
	}
	if has(p, "show_thinking") {
		dst.ShowThinking = src.ShowThinking
	}
	if has(p, "tool_details") {
		dst.ToolDetails = src.ToolDetails
	}
	if has(p, "hide_status") {
		dst.HideStatus = src.HideStatus
	}
	if has(p, "hide_tips") {
		dst.HideTips = src.HideTips
	}
	if has(p, "mouse") {
		dst.Mouse = src.Mouse
	}
	if has(p, "links") {
		dst.Links = src.Links
	}
	if has(p, "keybinds") {
		k := src.Keybinds
		d := &dst.Keybinds
		set := func(dstf *string, v string) {
			if v != "" {
				*dstf = v
			}
		}
		set(&d.Leader, k.Leader)
		set(&d.AppExit, k.AppExit)
		set(&d.SessionNew, k.SessionNew)
		set(&d.SessionList, k.SessionList)
		set(&d.MessagesUndo, k.MessagesUndo)
		set(&d.MessagesRedo, k.MessagesRedo)
		set(&d.ModelList, k.ModelList)
		set(&d.ThemeList, k.ThemeList)
		set(&d.AgentCycle, k.AgentCycle)
		set(&d.ToolDetails, k.ToolDetails)
		set(&d.MessagesPageUp, k.MessagesPageUp)
		set(&d.MessagesPageDown, k.MessagesPageDown)
		set(&d.Help, k.Help)
		set(&d.InputClear, k.InputClear)
		set(&d.Interrupt, k.Interrupt)
	}
}

var envKeys = map[string][]string{
	"anthropic":  {"ANTHROPIC_API_KEY"},
	"openai":     {"OPENAI_API_KEY"},
	"openrouter": {"OPENROUTER_API_KEY"},
	"google":     {"GEMINI_API_KEY", "GOOGLE_API_KEY"},
}

func applyEnvKeys(cfg *Config) {
	if cfg.Providers == nil {
		cfg.Providers = map[string]Provider{}
	}
	for name, vars := range envKeys {
		p := cfg.Providers[name]
		if p.APIKey == "" {
			for _, v := range vars {
				if val := os.Getenv(v); val != "" {
					p.APIKey = val
					break
				}
			}
		}
		if p.Type == "" {
			p.Type = name
		}
		if p.APIKey != "" || cfg.Providers[name].BaseURL != "" {
			cfg.Providers[name] = p
		}
	}
}

// SplitModel splits "provider/model-id" into its parts. A bare model id
// defaults to the anthropic provider.
func SplitModel(s string) (providerID, modelID string) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:]
		}
	}
	return "anthropic", s
}
