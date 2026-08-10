// Package config loads rick's two config files (rick.json for runtime
// behaviour, tui.json for presentation) with layered precedence.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"rick/pkg/contextbudget"
)

// Permission levels.
const (
	PermAllow = "allow"
	PermAsk   = "ask"
	PermDeny  = "deny"
)

const MaxSubagentDepth = 10

// ValidateSubagentDepth validates the configured maximum nesting depth.
func ValidateSubagentDepth(depth int) error {
	if depth < 1 || depth > MaxSubagentDepth {
		return fmt.Errorf("subagent_depth must be 1..%d", MaxSubagentDepth)
	}
	return nil
}

// ValidateCacheRetention validates the cache_retention setting.
func ValidateCacheRetention(retention string) error {
	switch retention {
	case "", "long", "none":
		return nil
	default:
		return fmt.Errorf("cache_retention must be one of \"long\", \"none\", or empty (default), got %q", retention)
	}
}

// Provider is one configured LLM backend.
type Provider struct {
	Type    string `json:"type,omitempty"` // anthropic | openai | openrouter | google
	APIKey  string `json:"apiKey,omitempty"`
	BaseURL string `json:"baseUrl,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

// Permission is the allow/ask/deny policy set.
//
// A permission block can either be written inline or inherit from a named
// profile via Extends. Resolution order, weakest to strongest, is:
// profile chain -> this block's coarse levels -> this block's glob rules.
type Permission struct {
	Bash    map[string]string `json:"bash,omitempty"`     // glob pattern -> level
	Edit    string            `json:"edit,omitempty"`     // level
	Write   string            `json:"write,omitempty"`    // level
	Read    string            `json:"read,omitempty"`     // level
	WebF    string            `json:"webfetch,omitempty"` // level
	Default string            `json:"default,omitempty"`  // fallback level

	// Extends names profiles from the top-level "permission_profile" map to
	// inherit from, in order. Later entries win, and this block wins over all
	// of them.
	Extends []string `json:"extends,omitempty"`

	// Tools maps a tool-name glob to a level, covering MCP tools and any
	// built-in the coarse fields above do not name explicitly.
	Tools map[string]string `json:"tools,omitempty"`

	// Paths maps a path glob to a level for file-touching tools. More
	// specific (longer) patterns win, so a deny on "**/.env" survives an
	// allow on "**".
	Paths map[string]string `json:"paths,omitempty"`

	// Hosts maps a hostname glob to a level for webfetch and websearch.
	Hosts map[string]string `json:"hosts,omitempty"`

	// Sandbox overrides the sandbox policy while this permission set is
	// active, letting a plan-mode agent run read-only without a global flag.
	Sandbox *SandboxConfig `json:"sandbox,omitempty"`
}

// SandboxConfig is the JSON shape of a sandbox policy. It mirrors
// sandbox.Policy but lives here so config has no dependency on the sandbox
// package.
type SandboxConfig struct {
	// Root is the workspace-write fence. Relative paths are resolved from the
	// project root; empty means the project root itself.
	Root            string   `json:"root,omitempty"`
	Mode            string   `json:"mode,omitempty"`        // read-only | workspace-write | trusted | off
	Enforcement     string   `json:"enforcement,omitempty"` // auto | os | static
	Network         *bool    `json:"network,omitempty"`
	AllowHosts      []string `json:"allow_hosts,omitempty"`
	DenyHosts       []string `json:"deny_hosts,omitempty"`
	WritableRoots   []string `json:"writable_roots,omitempty"`
	ReadableRoots   []string `json:"readable_roots,omitempty"`
	DenyPaths       []string `json:"deny_paths,omitempty"`
	AllowEnv        []string `json:"allow_env,omitempty"`
	DenyEnv         []string `json:"deny_env,omitempty"`
	KeepCredentials *bool    `json:"keep_credentials,omitempty"`
	MemoryMB        int      `json:"memory_mb,omitempty"`
	CPUSeconds      int      `json:"cpu_seconds,omitempty"`
	Processes       int      `json:"processes,omitempty"`
	FileSizeMB      int      `json:"file_size_mb,omitempty"`
}

// Agent is a named agent definition.
type Agent struct {
	Description string            `json:"description,omitempty"`
	Mode        string            `json:"mode,omitempty"` // primary | subagent | all
	Model       string            `json:"model,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	Prompt      string            `json:"prompt,omitempty"`
	Tools       map[string]bool   `json:"tools,omitempty"`
	Permission  *Permission       `json:"permission,omitempty"`
	Options     map[string]string `json:"options,omitempty"`
}

// MCPServer is one MCP server definition.
type MCPServer struct {
	Type        string            `json:"type,omitempty"` // local | remote
	Command     []string          `json:"command,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Enabled     *bool             `json:"enabled,omitempty"`
}

// Command is a config-defined custom slash command.
type Command struct {
	Description string `json:"description,omitempty"`
	Template    string `json:"template"`
	Agent       string `json:"agent,omitempty"`
	Model       string `json:"model,omitempty"`
}

// WebSearchProviderConfig contains provider-specific defaults and credentials.
// API keys may also be supplied through the provider's conventional environment
// variable so they do not need to be written to rick.json.
type WebSearchProviderConfig struct {
	Enabled         *bool    `json:"enabled,omitempty"`
	APIKey          string   `json:"api_key,omitempty"`
	APIKeyEnv       string   `json:"api_key_env,omitempty"`
	BaseURL         string   `json:"base_url,omitempty"`
	Instances       []string `json:"instances,omitempty"`
	Kind            string   `json:"kind,omitempty"` // api | local | public_instance | domain
	Backend         string   `json:"backend,omitempty"`
	Region          string   `json:"region,omitempty"`
	SafeSearch      string   `json:"safe_search,omitempty"`
	TimeRange       string   `json:"time_range,omitempty"`
	Type            string   `json:"type,omitempty"`
	Livecrawl       string   `json:"livecrawl,omitempty"`
	MaxAgeHours     *int     `json:"max_age_hours,omitempty"`
	IncludeDomains  []string `json:"include_domains,omitempty"`
	ExcludeDomains  []string `json:"exclude_domains,omitempty"`
	Priority        int      `json:"priority,omitempty"`
	Weight          float64  `json:"weight,omitempty"`
	MaxRPM          int      `json:"max_rpm,omitempty"`
	MaxConcurrency  int      `json:"max_concurrency,omitempty"`
	DailyBudget     int      `json:"daily_budget,omitempty"`
	MonthlyBudget   int      `json:"monthly_budget,omitempty"`
	TimeoutSeconds  int      `json:"timeout_seconds,omitempty"`
	CacheTTLSeconds int      `json:"cache_ttl_seconds,omitempty"`
	ClearAPIKey     bool     `json:"clear_api_key,omitempty"`
	ClearBaseURL    bool     `json:"clear_base_url,omitempty"`
}

// WebSearchConfig restricts web search results by domain and controls the
// provider pipeline. Existing domain and budget fields remain compatible with
// the original web_search configuration shape.
type WebSearchConfig struct {
	AllowDomains          []string `json:"allow_domains,omitempty"`            // if set, only these domains
	DenyDomains           []string `json:"deny_domains,omitempty"`             // always blocked
	MaxResults            int      `json:"max_results,omitempty"`              // default 5
	MaxSearchesPerSession int      `json:"max_searches_per_session,omitempty"` // budget, default 10
	// CacheMaxLen bounds the in-memory/disk query cache entries.
	CacheMaxLen     int                                `json:"cache_max_len,omitempty"`
	Provider        string                             `json:"provider,omitempty"`
	Mode            string                             `json:"mode,omitempty"`
	Parallel        *bool                              `json:"parallel,omitempty"`
	MaxParallel     int                                `json:"max_parallel,omitempty"`
	HedgeAfterMS    int                                `json:"hedge_after_ms,omitempty"`
	LogicalBudget   int                                `json:"logical_budget,omitempty"`
	CacheTTLSeconds int                                `json:"cache_ttl_seconds,omitempty"`
	MaxConcurrent   int                                `json:"max_concurrent,omitempty"`
	Providers       map[string]WebSearchProviderConfig `json:"providers,omitempty"`
}

// VisionConfig configures the vision bridge that gives text-only models
// (DeepSeek) sight. When Enabled, image attachments are sent to a
// vision-capable model via the Gemini generateContent API and the returned
// structured text evidence is fed to the text-only model in place of the raw
// image bytes.
type VisionConfig struct {
	// Enabled turns the bridge on. A pointer distinguishes "not set"
	// (inherit) from an explicit off, so a project block that only sets a
	// model keeps the global enabled state. Off leaves image attachments
	// untouched, so native-vision models keep receiving raw images.
	Enabled *bool `json:"enabled,omitempty"`
	// APIKey is the Google AI Studio key (free tier works). Stored in the
	// global rick.json; prefer /visionapi for setting it.
	APIKey string `json:"api_key,omitempty"`
	// Model is the vision model id. Defaults to the free "gemini-3.5-flash-lite".
	Model string `json:"model,omitempty"`
	// BaseURL overrides the Gemini API root; defaults to
	// https://generativelanguage.googleapis.com
	BaseURL string `json:"base_url,omitempty"`
}

// ContextBudgetConfig exposes the session context manager knobs
// (content-addressed dedup, cache boundaries, live-zone compression) as
// rick.json settings. Zero values keep the built-in defaults.
type ContextBudgetConfig struct {
	// MinStableTurns is how many identical observations a history prefix
	// needs before it becomes a cache boundary.
	MinStableTurns int `json:"min_stable_turns,omitempty"`
	// LiveZoneTurns is how many newest logical turns are excluded from
	// cache boundaries.
	LiveZoneTurns int `json:"live_zone_turns,omitempty"`
	// MaxStableBytes is the minimum serialized prefix size worth caching.
	MaxStableBytes int `json:"max_stable_bytes,omitempty"`
	// MaxBoundaries caps the cache breakpoints emitted per request
	// (Anthropic allows 4 total including system and tools).
	MaxBoundaries int `json:"max_boundaries,omitempty"`
	// MinCacheTokens is the minimum prefix size (in tokens) a breakpoint
	// must guard (providers ignore smaller prefixes).
	MinCacheTokens int `json:"min_cache_tokens,omitempty"`
	// MinDedupBytes is the minimum tool-result size considered for
	// content-addressed deduplication.
	MinDedupBytes int `json:"min_dedup_bytes,omitempty"`
	// MaxCABPayloads caps the content-addressed store size.
	MaxCABPayloads int `json:"max_cab_payloads,omitempty"`
	// MaxLivePayloads caps the reversible live-zone store size.
	MaxLivePayloads int `json:"max_live_payloads,omitempty"`
	// LiveZoneCapBytes bounds live-zone compressed tool output.
	LiveZoneCapBytes int `json:"live_zone_cap_bytes,omitempty"`
}

// ContextBudgetOptions renders the knobs as contextbudget.Options; zero
// values become defaults inside contextbudget.New.
func (c *ContextBudgetConfig) ContextBudgetOptions() contextbudget.Options {
	if c == nil {
		return contextbudget.Options{}
	}
	return contextbudget.Options{
		Enabled:          true,
		MinStableTurns:   c.MinStableTurns,
		LiveZoneTurns:    c.LiveZoneTurns,
		MaxStableBytes:   c.MaxStableBytes,
		MaxBoundaries:    c.MaxBoundaries,
		MinCacheTokens:   c.MinCacheTokens,
		MinDedupBytes:    c.MinDedupBytes,
		MaxCABPayloads:   c.MaxCABPayloads,
		MaxLivePayloads:  c.MaxLivePayloads,
		LiveZoneCapBytes: c.LiveZoneCapBytes,
	}
}

// Config is rick.json.
type Config struct {
	Schema     string              `json:"$schema,omitempty"`
	Model      string              `json:"model,omitempty"`
	SmallModel string              `json:"small_model,omitempty"`
	MaxTokens  int                 `json:"max_tokens,omitempty"`
	Providers  map[string]Provider `json:"provider,omitempty"`
	Permission *Permission         `json:"permission,omitempty"`
	// Profiles are reusable named permission sets referenced by
	// Permission.Extends and by agents. The built-ins (readonly, standard,
	// trusted, ci) are always present and may be overridden here.
	Profiles map[string]Permission `json:"permission_profile,omitempty"`
	// Sandbox is the global command-confinement policy. A permission block
	// or agent may override it.
	Sandbox        *SandboxConfig       `json:"sandbox,omitempty"`
	Tools          map[string]bool      `json:"tools,omitempty"`
	Agents         map[string]Agent     `json:"agent,omitempty"`
	MCP            map[string]MCPServer `json:"mcp,omitempty"`
	Commands       map[string]Command   `json:"command,omitempty"`
	Instructions   []string             `json:"instructions,omitempty"`
	AutoCompact    *bool                `json:"autocompact,omitempty"`
	ContextReserve int                  `json:"context_reserve,omitempty"`
	// DistillEnabled turns on state distillation: when the transcript
	// approaches the context budget, the oldest stable prefix is replaced by
	// a structured summary placed after the cache breakpoint. Requires an
	// extra provider round-trip, so it defaults to off.
	DistillEnabled *bool `json:"distill_enabled,omitempty"`
	// DistillModel is the fast model used for the background summary call.
	// Empty falls back to the primary model.
	DistillModel     string           `json:"distill_model,omitempty"`
	SubagentDepth    *int             `json:"subagent_depth,omitempty"`
	BackgroundNotify bool             `json:"background_notify,omitempty"`
	MaxBackground    int              `json:"max_background,omitempty"`
	Plugins          []string         `json:"plugin,omitempty"`
	WebSearch        *WebSearchConfig `json:"web_search,omitempty"`
	// Vision bridges images to a vision-capable model so text-only models
	// (DeepSeek) can "see". Enabled, the TUI replaces image attachments with
	// the vision engine's structured text evidence before the agent turn.
	Vision *VisionConfig `json:"vision,omitempty"`
	// CacheRetention is the prompt-cache retention policy for all requests:
	// "" (default/short), "long" (extended TTL), or "none" (caching off).
	CacheRetention string `json:"cache_retention,omitempty"`
	// WarmCache, when true, submits a small non-blocking request at session
	// start so the provider populates its prompt cache for the frozen
	// system+tools prefix before the first real turn. This lifts cache-hit
	// ratios for short sessions. Defaults to off until its cost is measured.
	WarmCache bool `json:"cache_warm,omitempty"`
	// CacheMaxReasoningTurns caps how many prior turns' reasoning blocks are
	// echoed to DeepSeek-line providers as reasoning_content (0 = keep all).
	// Keeps the serialized prefix byte-identical by default; a positive value
	// shrinks the prompt at the cost of one deliberate cache invalidation.
	CacheMaxReasoningTurns int `json:"cache_max_reasoning_turns,omitempty"`
	// CacheMaxToolResultBytes caps each tool_result payload sent to the model
	// (0 = default 16 KiB). Bounding the per-turn fresh tail keeps the
	// provider prompt-cache hit ratio high on tool-heavy turns.
	CacheMaxToolResultBytes int `json:"cache_max_tool_result_bytes,omitempty"`
	// CacheTTLSeconds overrides how long a warm prompt prefix is assumed to
	// survive at the provider before an idle gap forces a re-warm. Zero uses
	// the per-vendor table (provider.DefaultCacheTTL), which assumes a day
	// for DeepSeek-line endpoints — set this to the real retention when a
	// gateway (e.g. a free flash tier) expires entries far sooner.
	CacheTTLSeconds int `json:"cache_ttl_seconds,omitempty"`
	// CacheKeepaliveSeconds, when positive, keeps the provider prompt cache
	// alive during idle gaps by periodically re-sending the last stream's
	// exact prompt bytes as a minimal request. This is how long-idle sessions
	// hold a near-100% hit rate on gateways whose cache TTL is minutes, not
	// days. Zero disables the keep-alive.
	CacheKeepaliveSeconds int `json:"cache_keepalive_seconds,omitempty"`
	// CacheOpenRouterResponse, when true (default), enables OpenRouter's
	// response cache (X-OpenRouter-Cache header) so byte-identical repeated
	// requests — retries, warm, keep-alive, same sub-agent prompt twice —
	// are served from the gateway's response cache at zero billing. Response
	// caching is separate from prompt caching and works alongside it.
	CacheOpenRouterResponse bool `json:"cache_openrouter_response,omitempty"`
	// CacheOpenRouterResponseTTL is the response-cache TTL in seconds sent
	// with the X-OpenRouter-Cache-TTL header. Zero omits the TTL header and
	// uses OpenRouter's default.
	CacheOpenRouterResponseTTL int `json:"cache_openrouter_response_ttl,omitempty"`
	// ContextBudget exposes the session context manager knobs.
	ContextBudget *ContextBudgetConfig `json:"context_budget,omitempty"`
}

// DistillModelFor returns the model used for the background distillation
// summary: the explicit distill_model, else small_model, else the main model.
func (c *Config) DistillModelFor() string {
	if c.DistillModel != "" {
		return c.DistillModel
	}
	if c.SmallModel != "" {
		return c.SmallModel
	}
	return c.Model
}

// Keybinds is the tui.json keybind block.
type Keybinds struct {
	Leader           string `json:"leader,omitempty"`
	AppExit          string `json:"app_exit,omitempty"`
	SessionNew       string `json:"session_new,omitempty"`
	SessionList      string `json:"session_list,omitempty"`
	MessagesUndo     string `json:"messages_undo,omitempty"`
	MessagesRedo     string `json:"messages_redo,omitempty"`
	ModelList        string `json:"model_list,omitempty"`
	ThemeList        string `json:"theme_list,omitempty"`
	AgentCycle       string `json:"agent_cycle,omitempty"`
	ToolDetails      string `json:"tool_details,omitempty"`
	MessagesPageUp   string `json:"messages_page_up,omitempty"`
	MessagesPageDown string `json:"messages_page_down,omitempty"`
	Help             string `json:"help,omitempty"`
	InputClear       string `json:"input_clear,omitempty"`
	Interrupt        string `json:"interrupt,omitempty"`
}

// TUI is tui.json.
type TUI struct {
	Schema        string   `json:"$schema,omitempty"`
	Theme         string   `json:"theme,omitempty"`
	DiffMode      string   `json:"diff,omitempty"` // auto | stacked
	DiffThreshold int      `json:"diff_threshold,omitempty"`
	ScrollSpeed   int      `json:"scroll_speed,omitempty"`
	Notifications *bool    `json:"notifications,omitempty"`
	ShowThinking  *bool    `json:"show_thinking,omitempty"`
	ToolDetails   *bool    `json:"tool_details,omitempty"`
	HideStatus    bool     `json:"hide_status,omitempty"`
	HideTips      bool     `json:"hide_tips,omitempty"`
	Mouse         bool     `json:"mouse,omitempty"`
	Links         *bool    `json:"links,omitempty"`
	Keybinds      Keybinds `json:"keybinds,omitempty"`
}

// Loaded is the resolved config pair plus provenance.
type Loaded struct {
	Config      Config
	TUI         TUI
	ProjectRoot string
	// SandboxRoot is the effective workspace fence exposed to runtime clients.
	SandboxRoot string
	Sources     []string
}

// ---------- defaults ----------

// Defaults returns the built-in tier.
func Defaults() (Config, TUI) {
	yes := true
	depth := 1
	c := Config{
		Model:      "anthropic/claude-sonnet-4-5-20250929",
		SmallModel: "anthropic/claude-3-5-haiku-20241022",
		MaxTokens:  16384,
		Permission: &Permission{
			Default: PermAllow,
			Edit:    PermAsk,
			Write:   PermAsk,
			Read:    PermAllow,
			WebF:    PermAllow,
			Bash: map[string]string{
				"*":           PermAsk,
				"ls*":         PermAllow,
				"cat*":        PermAllow,
				"pwd":         PermAllow,
				"echo*":       PermAllow,
				"git status*": PermAllow,
				"git diff*":   PermAllow,
				"git log*":    PermAllow,
				"git show*":   PermAllow,
				"go build*":   PermAllow,
				"go test*":    PermAllow,
				"go vet*":     PermAllow,
				"rm *":        PermAsk,
				"git push*":   PermAsk,
				"sudo*":       PermDeny,
			},
		},
		AutoCompact:      &yes,
		ContextReserve:   24000,
		SubagentDepth:    &depth,
		BackgroundNotify: true,
		MaxBackground:    8,
		// Cache features are on by default: prompt caching keeps the provider
		// prefix warm across idle gaps ("long" retention) and a small warm
		// request primes each new session's prefix before the first turn.
		CacheRetention:          "long",
		WarmCache:               true,
		CacheMaxToolResultBytes: 16 << 10,
		CacheOpenRouterResponse: true,
	}
	t := TUI{
		Theme:         "pickle-rick",
		DiffMode:      "auto",
		DiffThreshold: 120,
		ScrollSpeed:   3,
		Mouse:         false, // off = terminal owns selection in the chat view
		ShowThinking:  &yes,
		Keybinds: Keybinds{
			Leader:           "ctrl+x",
			AppExit:          "ctrl+c",
			SessionNew:       "<leader>n",
			SessionList:      "<leader>l",
			MessagesUndo:     "<leader>u",
			MessagesRedo:     "<leader>r",
			ModelList:        "<leader>m",
			ThemeList:        "<leader>t",
			AgentCycle:       "tab",
			ToolDetails:      "<leader>d",
			MessagesPageUp:   "pgup",
			MessagesPageDown: "pgdown",
			Help:             "<leader>h",
			InputClear:       "ctrl+u",
			Interrupt:        "esc",
		},
	}
	return c, t
}

// ---------- JSONC ----------

// StripJSONC removes // and /* */ comments and trailing commas, ignoring
// anything inside string literals.
func StripJSONC(b []byte) []byte {
	var out []byte
	inStr, esc, inLine, inBlock := false, false, false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				out = append(out, c)
			}
		case inBlock:
			if c == '*' && i+1 < len(b) && b[i+1] == '/' {
				inBlock = false
				i++
			}
		case inStr:
			out = append(out, c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
		default:
			if c == '"' {
				inStr = true
				out = append(out, c)
			} else if c == '/' && i+1 < len(b) && b[i+1] == '/' {
				inLine = true
				i++
			} else if c == '/' && i+1 < len(b) && b[i+1] == '*' {
				inBlock = true
				i++
			} else {
				out = append(out, c)
			}
		}
	}
	return stripTrailingCommas(out)
}

func stripTrailingCommas(b []byte) []byte {
	var out []byte
	inStr, esc := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inStr {
			out = append(out, c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\n' || b[j] == '\r') {
				j++
			}
			if j < len(b) && (b[j] == '}' || b[j] == ']') {
				continue // drop the comma
			}
		}
		out = append(out, c)
	}
	return out
}

// ---------- substitution ----------

var subRe = regexp.MustCompile(`\{(env|file):([^}]+)\}`)

// Substitute expands {env:VAR} and {file:path} inside every string value.
func Substitute(b []byte, baseDir string) []byte {
	return subRe.ReplaceAllFunc(b, func(m []byte) []byte {
		parts := subRe.FindSubmatch(m)
		kind, arg := string(parts[1]), strings.TrimSpace(string(parts[2]))
		var val string
		switch kind {
		case "env":
			val = os.Getenv(arg)
		case "file":
			p := arg
			if !filepath.IsAbs(p) {
				p = filepath.Join(baseDir, p)
			}
			if data, err := os.ReadFile(p); err == nil {
				val = strings.TrimRight(string(data), "\r\n")
			}
		}
		// JSON-escape so the substituted value can't break the document.
		enc, err := json.Marshal(val)
		if err != nil {
			return []byte{}
		}
		return enc[1 : len(enc)-1]
	})
}

// ---------- paths ----------

// GlobalDir is ~/.config/rick (or %APPDATA%\rick on Windows).
func GlobalDir() string {
	if v := os.Getenv("RICK_HOME"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		if ad := os.Getenv("APPDATA"); ad != "" {
			return filepath.Join(ad, "rick")
		}
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "rick")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "rick")
}

// DataDir is where sessions and snapshots live.
func DataDir() string {
	if v := os.Getenv("RICK_DATA"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		if ad := os.Getenv("LOCALAPPDATA"); ad != "" {
			return filepath.Join(ad, "rick")
		}
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "rick")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "rick")
}

// FindProjectRoot walks up from dir looking for .git, falling back to dir.
func FindProjectRoot(dir string) string {
	d, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	for {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if _, err := os.Stat(filepath.Join(d, "rick.json")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// SandboxRoot resolves the configured workspace-write fence from one place.
// Relative roots are anchored to projectRoot so tools never need to invent
// their own interpretation of the sandbox configuration.
func SandboxRoot(sandbox *SandboxConfig, projectRoot string) string {
	root := strings.TrimSpace(projectRoot)
	if sandbox != nil && strings.TrimSpace(sandbox.Root) != "" {
		root = strings.TrimSpace(sandbox.Root)
		if strings.HasPrefix(root, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				root = filepath.Join(home, strings.TrimPrefix(root, "~"))
			}
		}
		if !filepath.IsAbs(root) {
			root = filepath.Join(projectRoot, root)
		}
	}
	if abs, err := filepath.Abs(root); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(root)
}

// PathWithinRoot reports whether path is safely contained by root. Existing
// symlinks and junctions are resolved, while new files are checked against the
// deepest existing parent. An error means containment could not be established
// and callers must fail closed.
func PathWithinRoot(root, path string) (bool, error) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(path) == "" {
		return false, fmt.Errorf("empty sandbox path")
	}
	rootPath, err := resolveContainmentPath(root)
	if err != nil {
		return false, fmt.Errorf("resolve sandbox root: %w", err)
	}
	targetPath, err := resolveContainmentPath(path)
	if err != nil {
		return false, fmt.Errorf("resolve target path: %w", err)
	}
	if runtime.GOOS == "windows" {
		rootPath = strings.ToLower(rootPath)
		targetPath = strings.ToLower(targetPath)
	}
	rel, err := filepath.Rel(rootPath, targetPath)
	if err != nil {
		return false, err
	}
	if rel == "." {
		return true, nil
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

func resolveContainmentPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	missing := ""
	for current := abs; ; current = filepath.Dir(current) {
		if _, statErr := os.Lstat(current); statErr == nil {
			resolved, evalErr := filepath.EvalSymlinks(current)
			if evalErr != nil {
				return "", evalErr
			}
			if missing != "" {
				resolved = filepath.Join(resolved, missing)
			}
			return filepath.Clean(resolved), nil
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", os.ErrNotExist
		}
		part := filepath.Base(current)
		if missing == "" {
			missing = part
		} else {
			missing = filepath.Join(part, missing)
		}
	}
}
