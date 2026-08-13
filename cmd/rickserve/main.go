// Command rickserve runs rick as a headless daemon. It reads newline-delimited
// JSON requests from stdin (or a TCP socket) and writes newline-delimited JSON
// events back, so editors, desktop apps, CI runners and other agents can drive
// the agent loop without a TUI.
//
// Protocol v2 (one JSON object per line):
//
// Requests (→):
//
//	{"type":"run","session_id":"abc","prompt":"hi","model":"anthropic/...","permission_profile":"standard","sandbox":"workspace-write","thinking":"auto","yolo":false,"agent":"build","cwd":"/proj","resume":false,"attachments":[{"name":"shot.png","media_type":"image/png","data":"<base64>"}]}
//	{"type":"permission_response","request_id":"r1","decision":"accept"}
//	{"type":"interrupt","session_id":"abc"}
//	{"type":"sessions","cwd":"/proj"}
//	{"type":"models"}
//	{"type":"tools"}
//	{"type":"config","cwd":"/proj"}
//	{"type":"snapshot","action":"list","cwd":"/proj"}              // list | can | undo | redo
//	{"type":"goal","action":"list"}                                  // list | create | update | step | abort | delete | set_active | active
//	{"type":"compact","session_id":"abc"}
//	{"type":"mcp"}
//	{"type":"plugins","action":"list"}                               // list | toggle
//	{"type":"agents","action":"list"}                                // list | kill | send | steer
//	{"type":"ping"}
//	{"type":"shutdown"}
//
// Events (←):
//
//	{"type":"event","session_id":"abc","event":"Content","data":{"text":"..."}}
//	{"type":"event","session_id":"abc","event":"Thinking","data":{"text":"..."}}
//	{"type":"event","session_id":"abc","event":"ToolUse","data":{"name":"bash","title":"...","input":{...}}}
//	{"type":"event","session_id":"abc","event":"ToolResult","data":{"name":"bash","output":"...","is_error":false,"elapsed":"1.2s"}}
//	{"type":"event","session_id":"abc","event":"Usage","data":{"input_tokens":100,"output_tokens":50}}
//	{"type":"event","session_id":"abc","event":"PermissionRequest","data":{"request_id":"r1","tool":"bash","command":"rm -rf /","title":"...","body":"..."}}
//	{"type":"done","session_id":"abc"}
//	{"type":"sessions","data":[...]}
//	{"type":"models","data":[...]}
//	{"type":"config","data":{...}}
//	{"type":"pong"}
//	{"type":"error","error":"..."}
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"rick/internal/agent"
	"rick/internal/config"
	"rick/internal/goal"
	"rick/internal/mcp"
	"rick/internal/permission"
	"rick/internal/plugin"
	"rick/internal/provider"
	"rick/internal/provider/anthropic"
	"rick/internal/provider/catalog"
	"rick/internal/provider/openai"
	"rick/internal/sandbox"
	"rick/internal/session"
	"rick/internal/swarm"
	"rick/internal/tools"
	"rick/internal/usage"
)

// Version is the daemon protocol version reported in the ready banner.
const Version = "2.1.0"

// Request is one inbound ndjson line.
type Request struct {
	Type              string `json:"type"`
	RunID             string `json:"run_id,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
	Prompt            string `json:"prompt,omitempty"`
	Model             string `json:"model,omitempty"`
	PermissionProfile string `json:"permission_profile,omitempty"`
	Sandbox           string `json:"sandbox,omitempty"`
	Thinking          string `json:"thinking,omitempty"`
	Reasoning         string `json:"reasoning,omitempty"` // compatibility alias for thinking
	Yolo              bool   `json:"yolo,omitempty"`
	MaxTurns          int    `json:"max_turns,omitempty"`
	Agent             string `json:"agent,omitempty"`
	Cwd               string `json:"cwd,omitempty"`
	Resume            bool   `json:"resume,omitempty"`
	// Attachments carries base64-encoded files sent with the prompt.
	// Images are delivered to vision-capable models as image blocks;
	// text files are embedded verbatim after the prompt.
	Attachments []Attachment `json:"attachments,omitempty"`
	// Permission response fields.
	RequestID string `json:"request_id,omitempty"`
	Decision  string `json:"decision,omitempty"` // accept | reject | always

	// Snapshot / goal / compact / mcp / plugins / agents control.
	Action  string         `json:"action,omitempty"`   // snapshot: list|can|undo|redo; goal: list|create|update|step|abort|delete|set_active|active; plugins: list|toggle; agents: list|kill|send|steer; config: set
	GoalID  string         `json:"goal_id,omitempty"`  // goal id for update/step/abort/delete/set_active
	StepID  string         `json:"step_id,omitempty"`  // goal step id for step
	Title   string         `json:"title,omitempty"`    // goal title for create/update
	Status  string         `json:"status,omitempty"`   // goal/step status
	Budget  int            `json:"budget,omitempty"`   // goal token budget
	Steps   []string       `json:"steps,omitempty"`    // goal step contents for create
	Name    string         `json:"name,omitempty"`     // plugin name for toggle
	Enabled *bool          `json:"enabled,omitempty"`  // plugin enabled state for toggle
	AgentID string         `json:"agent_id,omitempty"` // agent id for agents kill/send/steer
	From    string         `json:"from,omitempty"`     // sender label for agents send/steer
	Content string         `json:"content,omitempty"`  // message content for agents send/steer
	Patch   map[string]any `json:"patch,omitempty"`    // config keys for config action=set

	// Auth control (auth: list|save|update|add_keys|remove_key|remove).
	Provider     string   `json:"provider,omitempty"` // provider id for auth save/remove
	APIKey       string   `json:"api_key,omitempty"`
	APIKeys      []string `json:"api_keys,omitempty"`  // keys to add in one call
	KeyIndex     int      `json:"key_index,omitempty"` // 1-based key position to remove
	BaseURL      string   `json:"base_url,omitempty"`
	Label        string   `json:"label,omitempty"`
	DefaultModel string   `json:"default_model,omitempty"`
	OnlyFree     *bool    `json:"only_free,omitempty"`
	Disabled     *bool    `json:"disabled,omitempty"`
	KeyMode      string   `json:"key_mode,omitempty"` // single | round-robin | failover

	// Plugin add: file path or http(s) URL to load a manifest from.
	Source string `json:"source,omitempty"`
}

// Attachment is one file attached to a run request.
type Attachment struct {
	Name      string `json:"name,omitempty"`
	MediaType string `json:"media_type,omitempty"` // image/png, text/plain, ...
	Data      string `json:"data,omitempty"`       // base64-encoded bytes
}

// Response is one outbound ndjson line.
type Response struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id,omitempty"`
	RunID     string          `json:"run_id,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Event     string          `json:"event,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// writer serialises ndjson output so concurrent runs cannot interleave lines.
type writer struct {
	mu  sync.Mutex
	bw  *bufio.Writer
	enc *json.Encoder
}

func newWriter(w io.Writer) *writer {
	bw := bufio.NewWriter(w)
	return &writer{bw: bw, enc: json.NewEncoder(bw)}
}

type responseEmitter interface {
	emit(Response)
}

type runWriter struct {
	parent    *writer
	requestID string
	runID     string
}

func (w *runWriter) emit(response Response) {
	if response.RequestID == "" {
		response.RequestID = w.requestID
	}
	if response.RunID == "" {
		response.RunID = w.runID
	}
	w.parent.emit(response)
}

// emit writes one response line and flushes it immediately.
func (w *writer) emit(r Response) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.enc.Encode(r)
	_ = w.bw.Flush()
}

// flush drains any buffered output.
func (w *writer) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.bw.Flush()
}

// pendingPerm tracks an outstanding permission request awaiting a client reply.
type pendingPerm struct {
	ch chan agent.PermissionDecision
}

// server holds the shared infrastructure reused across requests.
type server struct {
	loaded  *config.Loaded
	cwd     string
	provs   map[string]provider.Provider
	tools   *tools.Registry
	plugins *plugin.Registry
	store   *session.Store
	mcp     *mcp.Manager
	sandbox *sandbox.Holder
	// Startup security flags are defaults only. Every run gets an isolated
	// permission engine and sandbox holder so concurrent Desktop runs cannot
	// change each other's policy.
	defaultProfile string
	defaultSandbox string
	usage          *usage.Tracker
	creds          *config.Credentials
	goals          *goal.Store

	// Snapshotter per cwd so undo/redo survives across runs.
	snapMu sync.Mutex
	snaps  map[string]*session.Snapshotter

	// Serializes auth credential read-modify-write cycles. Requests dispatch
	// concurrently, so batched save/remove calls would otherwise clobber each
	// other's in-memory snapshot before Save().
	authMu sync.Mutex

	// Agent registry per session so clients can list/kill/send/steer agents.
	agentsMu sync.Mutex
	agents   map[string]*agent.Registry

	// Permission routing: request_id -> pending decision channel.
	permMu      sync.Mutex
	permPending map[string]*pendingPerm
	permCounter atomic.Int64

	// Active run cancellation: session_id -> cancel func.
	runMu     sync.Mutex
	runCancel map[string]activeRun

	// Lazy model reload runs at most once per process: providers that have no
	// fetched model list yet are probed on the first models query.
	modelsOnce sync.Once
}

type activeRun struct {
	runID  string
	cancel context.CancelFunc
}

func main() {
	var (
		flagPort    int
		flagCwd     string
		flagSandbox string
		flagProfile string
	)

	root := &cobra.Command{
		Use:   "rickserve",
		Short: "Run rick as a headless ndjson daemon",
		Long: "rickserve accepts newline-delimited JSON run requests on stdin (or a TCP\n" +
			"port) and streams newline-delimited JSON events back.\n\n" +
			"Protocol v2 adds interactive permission routing, interrupt, session\n" +
			"resume, and query endpoints (sessions, models, config).\n\n" +
			"Examples:\n" +
			"  echo '{\"type\":\"run\",\"prompt\":\"hello\"}' | rickserve\n" +
			"  rickserve --port 7333",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			srv, err := newServer(flagCwd, flagSandbox, flagProfile)
			if err != nil {
				return err
			}
			defer srv.mcp.Close()

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			if len(srv.loaded.Config.MCP) > 0 {
				srv.mcp.Connect(ctx, srv.loaded.Config.MCP)
				// Registering exposes the connected servers' tools to the
				// daemon-wide registry, so handleTools and every session
				// surface them. Connect alone only dials + discovers them.
				srv.mcp.Register(srv.tools, srv.loaded.Config.Tools)
			}

			// Emit a ready banner so clients know the daemon is alive.
			out := newWriter(os.Stdout)
			out.emit(Response{
				Type: "ready",
				Data: mustJSON(map[string]string{
					"protocol": Version,
					"version":  "v" + rickVersion,
				}),
			})
			out.flush()

			if flagPort > 0 {
				return srv.serveTCP(ctx, flagPort)
			}
			defer out.flush()
			srv.serveConn(ctx, os.Stdin, out)
			return nil
		},
	}

	root.Flags().IntVar(&flagPort, "port", 0, "listen on a TCP port instead of stdin/stdout")
	root.Flags().StringVar(&flagCwd, "cwd", ".", "working directory for agent runs")
	root.Flags().StringVar(&flagSandbox, "sandbox", "",
		"command sandbox: read-only | workspace-write | trusted | off")
	root.Flags().StringVar(&flagProfile, "permission-profile", "",
		"permission profile: readonly | standard | trusted | ci")

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "rickserve: "+err.Error())
		os.Exit(1)
	}
}

// rickVersion is injected at build time; fallback for dev builds.
var rickVersion = "0.1.17"

// newServer assembles the shared dependencies once at startup.
func newServer(dir, sandboxMode, profile string) (*server, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", abs)
	}

	loaded, err := config.Load(abs)
	if err != nil {
		return nil, err
	}
	var creds *config.Credentials
	if c, cerr := config.LoadCredentials(); cerr == nil {
		creds = c
		config.MergeCredentials(&loaded.Config, creds)
		if loaded.Config.Model == "" {
			for _, id := range creds.IDs() {
				if mdl := config.FirstConfiguredModel(creds, id); mdl != "" {
					loaded.Config.Model = mdl
					break
				}
			}
		}
	}

	_, holder, err := resolveRunSecurity(loaded, profile, sandboxMode, false)
	if err != nil {
		return nil, err
	}

	todos := tools.NewTodoStore()
	reg := tools.NewRegistry()
	reg.Register(tools.ReadTool{})
	reg.Register(tools.WriteTool{})
	reg.Register(tools.EditTool{})
	reg.Register(tools.BashTool{Sandbox: holder})
	reg.Register(tools.GrepTool{})
	reg.Register(tools.GlobTool{})
	reg.Register(tools.ListTool{})
	reg.Register(tools.ApplyPatchTool{})
	reg.Register(tools.TodoWriteTool{Store: todos})
	reg.Register(tools.TodoReadTool{Store: todos})
	reg.Register(tools.CodeSymbolsTool{})
	reg.Register(tools.GitTool{})
	reg.Register(tools.DiagnosticsTool{})
	reg.Register(tools.TestTool{})
	reg.Register(tools.TreeTool{})
	reg.Register(tools.FetchTool{})
	reg.Register(tools.MemoryTool{})
	reg.Register(tools.WebSearchTool{Restrictions: loaded.Config.WebSearch})

	goals, _ := goal.NewStore(filepath.Join(config.DataDir(), "goals"))
	if goals != nil {
		reg.Register(goal.GoalWriteTool{Store: goals})
		reg.Register(goal.GoalReadTool{Store: goals})
		reg.Register(goal.GoalStepTool{Store: goals})
		reg.Register(goal.GoalAbortTool{Store: goals})
	}

	plugins := plugin.NewRegistry()
	globalPluginDir := filepath.Join(config.GlobalDir(), "plugins")
	projectPluginDir := filepath.Join(loaded.ProjectRoot, ".rick", "plugins")
	var pluginURLs []string
	for _, entry := range loaded.Config.Plugins {
		if strings.HasPrefix(entry, "http://") || strings.HasPrefix(entry, "https://") {
			pluginURLs = append(pluginURLs, entry)
		}
	}
	manifests, _ := plugin.LoadAll(globalPluginDir, projectPluginDir, pluginURLs)
	for _, man := range manifests {
		hooks := plugin.ManifestToHooks(man)
		plugins.Register(hooks)
		plugins.SetEnabled(man.Name, man.IsEnabled())
	}

	store, err := session.NewStore(filepath.Join(config.DataDir(), "sessions"))
	if err != nil {
		return nil, err
	}

	usageTracker := usage.New(config.GlobalDir())

	return &server{
		loaded:         loaded,
		cwd:            abs,
		provs:          buildProviders(loaded.Config, creds),
		creds:          creds,
		tools:          reg,
		plugins:        plugins,
		store:          store,
		mcp:            mcp.NewManager(),
		sandbox:        holder,
		defaultProfile: profile,
		defaultSandbox: sandboxMode,
		usage:          usageTracker,
		goals:          goals,
		snaps:          map[string]*session.Snapshotter{},
		agents:         map[string]*agent.Registry{},
		permPending:    make(map[string]*pendingPerm),
		runCancel:      make(map[string]activeRun),
	}, nil
}

// resolveRunSecurity mirrors cmd/rick's resolveSecurity for one daemon run.
// Profile and sandbox overrides are deliberately run-local: rickserve can run
// multiple Desktop sessions concurrently, each with different execution
// controls.
func resolveRunSecurity(loaded *config.Loaded, profile, mode string, yolo bool) (*permission.Engine, *sandbox.Holder, error) {
	return resolveRunSecurityAtRoot(loaded, loaded.ProjectRoot, profile, mode, yolo)
}

func resolveRunSecurityAtRoot(loaded *config.Loaded, projectRoot, profile, mode string, yolo bool) (*permission.Engine, *sandbox.Holder, error) {
	cfg := loaded.Config
	perm := cfg.Permission
	if profile != "" {
		resolved, err := config.ResolveProfileByName(cfg, profile)
		if err != nil {
			return nil, nil, err
		}
		perm = resolved
	} else {
		perm = config.ResolvePermission(cfg, perm)
	}
	perms := permission.New(perm, projectRoot)
	perms.SetProfile(profile)

	sbCfg := cfg.Sandbox
	if perm != nil && perm.Sandbox != nil {
		if profile != "" {
			sbCfg = config.MergeSandbox(cfg.Sandbox, perm.Sandbox)
		} else {
			sbCfg = config.MergeSandbox(perm.Sandbox, cfg.Sandbox)
		}
	}
	policy := sandbox.FromConfig(sbCfg, projectRoot)
	if mode != "" {
		m, ok := sandbox.ParseMode(mode)
		if !ok {
			return nil, nil, fmt.Errorf("unknown sandbox mode %q (want read-only, workspace-write, trusted or off)", mode)
		}
		policy.Mode = m
	}
	// Match TUI YOLO semantics: bypass both approval prompts and command
	// sandboxing. Explicit protected-path denials remain enforced by perms.
	if yolo {
		policy.Mode = sandbox.ModeOff
	}
	policy = policy.Normalize(policy.Workspace)
	perms.SetSandboxRoot(policy.Workspace, policy.Mode == sandbox.ModeWorkspace)
	perms.SetProtectedPaths(policy.DenyPaths)
	perms.SetYolo(yolo)
	return perms, sandbox.NewHolder(policy), nil
}

// resolveRunReasoning accepts the TUI's thinking vocabulary. "auto" selects
// the active model's model-specific default; an omitted value preserves the
// daemon's legacy provider default behavior. Explicit Desktop choices pass
// through unchanged because its generic selector is not model-specific.
func resolveRunReasoning(providerID, modelID, value string, models []provider.ModelInfo) (provider.ReasoningEffort, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	var advertised *provider.ModelInfo
	for i := range models {
		if models[i].ID == modelID {
			advertised = &models[i]
			break
		}
	}
	caps := provider.ReasoningCapabilitiesForProvider(providerID, modelID, advertised)
	if strings.EqualFold(value, "auto") {
		return caps.Default, nil
	}
	effort, ok := provider.ParseEffort(value)
	if !ok {
		return "", fmt.Errorf("unknown thinking level %q", value)
	}
	if len(caps.Efforts) > 0 {
		supported := false
		for _, candidate := range caps.Efforts {
			if candidate == effort {
				supported = true
				break
			}
		}
		if !supported {
			return "", fmt.Errorf("thinking level %q is not supported by %s/%s", value, providerID, modelID)
		}
	}
	return effort, nil
}

// buildProviders instantiates every configured provider that has credentials.
// creds, when non-nil, is used to persist refreshed OAuth tokens for the
// ChatGPT / Codex provider.
func buildProviders(cfg config.Config, creds *config.Credentials) map[string]provider.Provider {
	out := map[string]provider.Provider{}
	for name, p := range cfg.Providers {
		if p.Enabled != nil && !*p.Enabled {
			continue
		}
		kind := p.Type
		if kind == "" {
			if e, ok := catalog.Get(name); ok {
				kind = e.Flavor
			} else {
				kind = name
			}
		}
		switch kind {
		case "anthropic":
			if p.APIKey == "" && p.BaseURL == "" {
				continue
			}
			out[name] = anthropic.New(p.APIKey, p.BaseURL)
		case "openai", "openrouter", "groq", "deepseek", "together", "openai-compatible":
			if p.APIKey == "" && p.BaseURL == "" {
				continue
			}
			c := openai.New(name, p.APIKey, p.BaseURL)
			if p.RefreshToken != "" {
				c.SetCodex(p.RefreshToken, p.AccountID, p.TokenExpiresAt, func(access, refresh string, expiresAt int64) {
					if creds != nil {
						_ = creds.SaveTokens(name, access, refresh, expiresAt)
					}
				})
			}
			c.SetKeepalive(time.Duration(cfg.CacheKeepaliveSeconds) * time.Second)
			c.SetOpenRouterResponseCache(cfg.CacheOpenRouterResponse, cfg.CacheOpenRouterResponseTTL)
			out[name] = c
		default:
			if p.BaseURL != "" {
				c := openai.New(name, p.APIKey, p.BaseURL)
				if p.RefreshToken != "" {
					c.SetCodex(p.RefreshToken, p.AccountID, p.TokenExpiresAt, func(access, refresh string, expiresAt int64) {
						if creds != nil {
							_ = creds.SaveTokens(name, access, refresh, expiresAt)
						}
					})
				}
				c.SetKeepalive(time.Duration(cfg.CacheKeepaliveSeconds) * time.Second)
				c.SetOpenRouterResponseCache(cfg.CacheOpenRouterResponse, cfg.CacheOpenRouterResponseTTL)
				out[name] = c
			}
		}
	}
	return out
}

// resolveProvider picks the provider for a fully-qualified model string,
// tolerating multi-segment model ids such as "openrouter/meta/llama-3".
func (s *server) resolveProvider(model string) (provider.Provider, string, error) {
	provID, modelID := config.SplitModel(model)
	if p, ok := s.provs[provID]; ok {
		return p, modelID, nil
	}
	idx := strings.Index(model, "/")
	for idx >= 0 && idx < len(model)-1 {
		if p, found := s.provs[model[:idx]]; found {
			return p, model[idx+1:], nil
		}
		next := strings.Index(model[idx+1:], "/")
		if next < 0 {
			break
		}
		idx = idx + 1 + next
	}
	return nil, "", fmt.Errorf("no provider configured for model %q", model)
}

// serveTCP listens on a port and serves each client connection concurrently.
func (s *server) serveTCP(ctx context.Context, port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "rickserve v%s listening on %s\n", Version, ln.Addr())

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			fmt.Fprintf(os.Stderr, "rickserve: accept: %v\n", err)
			continue
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			defer c.Close()
			out := newWriter(c)
			s.serveConn(ctx, c, out)
			out.flush()
		}(conn)
	}
	wg.Wait()
	return nil
}

// serveConn reads ndjson requests from r until EOF or shutdown.
func (s *server) serveConn(ctx context.Context, r io.Reader, out *writer) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var inflight sync.WaitGroup
	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req Request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			out.emit(Response{Type: "error", Error: "malformed request: " + err.Error()})
			continue
		}
		switch req.Type {
		case "shutdown":
			// Drain requests dispatched before shutdown so their responses
			// are not lost when the connection closes.
			inflight.Wait()
			out.emit(Response{Type: "done", SessionID: req.SessionID})
			return
		case "", "run", "compact":
			// Long-running handlers run on their own goroutine so a run does
			// not block permission responses, interrupts, or status queries
			// on the same connection.
			inflight.Add(1)
			go func(req Request) {
				defer inflight.Done()
				s.dispatch(ctx, req, out)
			}(req)
		default:
			// Everything else answers quickly; handle it in order so batched
			// mutations (auth save/remove, config set) stay deterministic.
			s.dispatch(ctx, req, out)
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		out.emit(Response{Type: "error", Error: "read: " + err.Error()})
	}
}

// dispatch routes one request to its handler on its own goroutine.
func (s *server) dispatch(ctx context.Context, req Request, out *writer) {
	switch req.Type {
	case "", "run":
		s.handleRun(ctx, req, &runWriter{parent: out, requestID: req.RequestID, runID: req.RunID})
	case "permission_response":
		s.handlePermissionResponse(req)
	case "interrupt":
		s.handleInterrupt(req)
	case "sessions":
		s.handleSessions(req, out)
	case "models":
		s.handleModels(out)
	case "tools":
		s.handleTools(out)
	case "config":
		s.handleConfig(req, out)
	case "snapshot":
		s.handleSnapshot(req, out)
	case "goal":
		s.handleGoal(req, out)
	case "compact":
		s.handleCompact(ctx, req, out)
	case "mcp":
		s.handleMCP(out)
	case "plugins":
		s.handlePlugins(req, out)
	case "auth":
		s.handleAuth(req, out)
	case "agents":
		s.handleAgents(req, out)
	case "ping":
		out.emit(Response{Type: "pong", SessionID: req.SessionID})
	default:
		out.emit(Response{Type: "error", SessionID: req.SessionID,
			Error: fmt.Sprintf("unknown request type %q", req.Type)})
	}
}

// ---------- permission routing ----------

// registerPerm creates a pending permission slot and returns its request_id.
func (s *server) registerPerm() (string, *pendingPerm) {
	id := fmt.Sprintf("perm_%d", s.permCounter.Add(1))
	p := &pendingPerm{ch: make(chan agent.PermissionDecision, 1)}
	s.permMu.Lock()
	s.permPending[id] = p
	s.permMu.Unlock()
	return id, p
}

// handlePermissionResponse delivers a client's decision to the waiting agent.
func (s *server) handlePermissionResponse(req Request) {
	s.permMu.Lock()
	p, ok := s.permPending[req.RequestID]
	if ok {
		delete(s.permPending, req.RequestID)
	}
	s.permMu.Unlock()
	if !ok {
		return // stale or unknown — ignore
	}
	switch req.Decision {
	case "accept":
		p.ch <- agent.DecideAccept
	case "always":
		p.ch <- agent.DecideAlways
	default:
		p.ch <- agent.DecideReject
	}
}

// ---------- interrupt ----------

func (s *server) handleInterrupt(req Request) {
	s.runMu.Lock()
	run, ok := s.runCancel[req.SessionID]
	s.runMu.Unlock()
	if ok && (req.RunID == "" || req.RunID == run.runID) {
		run.cancel()
	}
}

// ---------- query handlers ----------

func (s *server) handleSessions(req Request, out *writer) {
	cwd := req.Cwd // empty = all
	metas, err := s.store.List(cwd)
	if err != nil {
		out.emit(Response{Type: "error", Error: err.Error()})
		return
	}
	out.emit(Response{Type: "sessions", Data: mustJSON(metas)})
}

func (s *server) handleModels(out *writer) {
	// Lazy reload: a provider added without a /models probe (desktop custom
	// form) has no fetched list; probe it once here so the picker shows the
	// endpoint's real models instead of static placeholders.
	s.modelsOnce.Do(s.refreshMissingModels)

	type modelEntry struct {
		Provider           string                     `json:"provider"`
		ID                 string                     `json:"id"`
		Name               string                     `json:"name"`
		ContextWindow      int                        `json:"context_window"`
		ReasoningEfforts   []provider.ReasoningEffort `json:"reasoning_efforts,omitempty"`
		ReasoningDefault   provider.ReasoningEffort   `json:"reasoning_default,omitempty"`
		ReasoningMandatory bool                       `json:"reasoning_mandatory,omitempty"`
	}
	var entries []modelEntry
	for name, p := range s.provs {
		for _, mi := range s.modelsForProvider(name, p) {
			caps := provider.ReasoningCapabilitiesForProvider(name, mi.ID, &mi)
			entries = append(entries, modelEntry{
				Provider:           name,
				ID:                 mi.ID,
				Name:               mi.Name,
				ContextWindow:      mi.ContextWindow,
				ReasoningEfforts:   caps.Efforts,
				ReasoningDefault:   caps.Default,
				ReasoningMandatory: caps.Mandatory,
			})
		}
	}
	out.emit(Response{Type: "models", Data: mustJSON(entries)})
}

// modelsForProvider returns the models a provider should advertise. When the
// user has authenticated the provider, the credential's real fetched model
// list (auth.json) wins over the static catalogue defaults, so the desktop and
// protocol clients see exactly what the API actually serves.
func (s *server) modelsForProvider(name string, p provider.Provider) []provider.ModelInfo {
	if s.creds == nil {
		return provider.FilterChatModels(p.Models())
	}
	cred, ok := s.creds.Get(name)
	if !ok || len(cred.Models) == 0 {
		return provider.FilterChatModels(p.Models())
	}
	vision := make(map[string]bool, len(cred.VisionModels))
	for _, id := range cred.VisionModels {
		vision[id] = true
	}
	providerModels := make(map[string]provider.ModelInfo)
	for _, modelInfo := range p.Models() {
		providerModels[modelInfo.ID] = modelInfo
	}
	advertised := make([]provider.ModelInfo, 0, len(cred.Models))
	for _, id := range cred.Models {
		if strings.TrimSpace(id) == "" {
			continue
		}
		// Provider-specific deployments override the API-reported window so
		// the daemon advertises exactly what the TUI model list shows.
		contextWindow := cred.ContextWindows[id]
		if override, ok := provider.ProviderContextWindow(name, id); ok {
			contextWindow = override
		}
		modelInfo := providerModels[id]
		modelInfo.ID = id
		if modelInfo.Name == "" {
			modelInfo.Name = id
		}
		if contextWindow == 0 {
			contextWindow = modelInfo.ContextWindow
		}
		modelInfo.ContextWindow = contextWindow
		modelInfo.SupportsImages = modelInfo.SupportsImages || vision[id]
		advertised = append(advertised, modelInfo)
	}
	if len(advertised) == 0 {
		return provider.FilterChatModels(p.Models())
	}
	return advertised
}

// refreshMissingModels probes every authenticated provider whose credential
// has no fetched model list yet (e.g. a provider added through the desktop
// "add custom provider" form, which writes the key without probing /models)
// and persists the real list so the model picker stops advertising static
// placeholder models that do not belong to the endpoint. It is a lazy reload:
// providers that already have a fetched list are left untouched.
func (s *server) refreshMissingModels() {
	if s.creds == nil {
		return
	}
	changed := false
	for id, cred := range s.creds.Snapshot() {
		if cred.Disabled || cred.BaseURL == "" {
			continue
		}
		if len(cred.Models) > 0 {
			continue // already fetched — nothing to do
		}
		key := s.creds.CurrentKey(id)
		if key == "" {
			continue // no credential to probe with
		}
		res := catalog.ProbeWithAccount(context.Background(), cred.BaseURL, key, cred.AccountID)
		if res.Err != nil || len(res.Models) == 0 {
			continue // endpoint does not publish a list; leave as-is
		}
		models := catalog.FilterChatModels(res.Models)
		if len(models) == 0 {
			continue
		}
		cred.Models = nil
		cred.ContextWindows = map[string]int{}
		cred.ContextSources = map[string]provider.ContextSource{}
		cred.VisionModels = nil
		for _, mm := range models {
			cred.Models = append(cred.Models, mm.ID)
			if mm.Context > 0 {
				cred.ContextWindows[mm.ID] = mm.Context
			}
			if mm.ContextSource != provider.ContextSourceUnknown {
				cred.ContextSources[mm.ID] = mm.ContextSource
			}
			if mm.SupportsImages {
				cred.VisionModels = append(cred.VisionModels, mm.ID)
			}
		}
		s.creds.Set(id, cred)
		changed = true
	}
	if changed {
		// modelsForProvider reads s.creds directly, so the freshly persisted
		// list is visible on the next models query without a daemon restart.
		_ = s.creds.Save()
	}
}

func (s *server) handleConfig(req Request, out *writer) {
	cwd := s.cwd
	if req.Cwd != "" {
		if abs, err := filepath.Abs(req.Cwd); err == nil {
			cwd = abs
		}
	}
	loaded, err := config.Load(cwd)
	if err != nil {
		out.emit(Response{Type: "error", Error: err.Error()})
		return
	}
	if req.Action == "set" {
		if err := applyConfigPatch(req.Patch); err != nil {
			out.emit(Response{Type: "error", SessionID: req.SessionID, Error: err.Error()})
			return
		}
		// Re-resolve so the response reflects the new state.
		loaded, err = config.Load(cwd)
		if err != nil {
			out.emit(Response{Type: "error", Error: err.Error()})
			return
		}
	}
	out.emit(Response{Type: "config", Data: mustJSON(map[string]any{
		"project_root": loaded.ProjectRoot,
		"global_dir":   config.GlobalDir(),
		"data_dir":     config.DataDir(),
		"sources":      loaded.Sources,
		"config":       loaded.Config,
		"tui":          loaded.TUI,
	})})
}

// applyConfigPatch splits a generic patch into rick.json and tui.json keys so
// runtime behaviour lives in rick.json and presentation in tui.json.
func applyConfigPatch(patch map[string]any) error {
	var rickPatch, tuiPatch map[string]any
	for key, value := range patch {
		switch key {
		case "theme", "diff", "diff_threshold", "scroll_speed", "notifications",
			"show_thinking", "tool_details", "hide_status", "hide_tips", "mouse", "links", "keybinds":
			if tuiPatch == nil {
				tuiPatch = map[string]any{}
			}
			tuiPatch[key] = value
		default:
			if rickPatch == nil {
				rickPatch = map[string]any{}
			}
			rickPatch[key] = value
		}
	}
	if len(rickPatch) > 0 {
		if err := config.SaveConfigPatch(rickPatch); err != nil {
			return err
		}
	}
	if len(tuiPatch) > 0 {
		if err := config.SaveTUIPatch(tuiPatch); err != nil {
			return err
		}
	}
	return nil
}

// ---------- snapshots (undo / redo) ----------

func (s *server) snapshotFor(req Request) *session.Snapshotter {
	cwd := s.cwd
	if req.Cwd != "" {
		if abs, err := filepath.Abs(req.Cwd); err == nil {
			cwd = abs
		}
	}
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	snaps, ok := s.snaps[cwd]
	if !ok {
		snaps, _ = session.NewSnapshotter(cwd, config.DataDir())
		s.snaps[cwd] = snaps
	}
	return snaps
}

// handleSnapshot serves undo/redo bookkeeping over the protocol.
// Actions: list, can, snapshot, undo, redo.
func (s *server) handleSnapshot(req Request, out *writer) {
	snaps := s.snapshotFor(req)
	action := req.Action
	if action == "" {
		action = "list"
	}
	emitErr := func(err error) {
		if err != nil {
			out.emit(Response{Type: "error", SessionID: req.SessionID, Error: err.Error()})
		}
	}

	switch action {
	case "list":
		out.emit(Response{Type: "snapshot", SessionID: req.SessionID, Data: mustJSON(map[string]any{
			"history":  snaps.History(),
			"can_undo": snaps.CanUndo(),
			"can_redo": snaps.CanRedo(),
			"git_info": session.GitInfo(snaps.WorkTree()),
		})})
	case "can":
		out.emit(Response{Type: "snapshot", SessionID: req.SessionID, Data: mustJSON(map[string]any{
			"can_undo": snaps.CanUndo(),
			"can_redo": snaps.CanRedo(),
		})})
	case "snapshot":
		hash, err := snaps.Snapshot(req.Title)
		if err != nil {
			emitErr(err)
			return
		}
		out.emit(Response{Type: "snapshot", SessionID: req.SessionID, Data: mustJSON(map[string]any{
			"hash": hash,
		})})
	case "undo":
		snap, err := snaps.Undo()
		if err != nil {
			emitErr(err)
			return
		}
		out.emit(Response{Type: "snapshot", SessionID: req.SessionID, Data: mustJSON(snap)})
	case "redo":
		snap, err := snaps.Redo()
		if err != nil {
			emitErr(err)
			return
		}
		out.emit(Response{Type: "snapshot", SessionID: req.SessionID, Data: mustJSON(snap)})
	default:
		out.emit(Response{Type: "error", SessionID: req.SessionID,
			Error: fmt.Sprintf("unknown snapshot action %q", action)})
	}
}

// ---------- goals ----------

// handleGoal serves the goal store over the protocol.
// Actions: list, create, update, step, abort, delete, set_active, active.
func (s *server) handleGoal(req Request, out *writer) {
	if s.goals == nil {
		out.emit(Response{Type: "error", SessionID: req.SessionID, Error: "goals not available"})
		return
	}
	action := req.Action
	if action == "" {
		action = "list"
	}
	emitErr := func(err error) {
		if err != nil {
			out.emit(Response{Type: "error", SessionID: req.SessionID, Error: err.Error()})
		}
	}

	switch action {
	case "list":
		gs, err := s.goals.List()
		if err != nil {
			emitErr(err)
			return
		}
		out.emit(Response{Type: "goal", SessionID: req.SessionID, Data: mustJSON(gs)})
	case "create":
		g := &goal.Goal{
			Title:       req.Title,
			Description: req.Content,
			Status:      req.Status,
			TokenBudget: req.Budget,
		}
		if g.Status == "" {
			g.Status = "active"
		}
		for _, stepText := range req.Steps {
			g.Steps = append(g.Steps, goal.Step{ID: goal.NewID(), Content: stepText, Status: "pending"})
		}
		if err := s.goals.Save(g); err != nil {
			emitErr(err)
			return
		}
		if g.Status == "active" {
			_ = s.goals.SetActive(g.ID)
		}
		out.emit(Response{Type: "goal", SessionID: req.SessionID, Data: mustJSON(g)})
	case "update":
		g, err := s.goals.Load(req.GoalID)
		if err != nil {
			emitErr(err)
			return
		}
		if req.Title != "" {
			g.Title = req.Title
		}
		if req.Status != "" {
			g.Status = req.Status
		}
		if req.Budget > 0 {
			g.TokenBudget = req.Budget
		}
		if req.Content != "" {
			g.Description = req.Content
		}
		if err := s.goals.Save(g); err != nil {
			emitErr(err)
			return
		}
		out.emit(Response{Type: "goal", SessionID: req.SessionID, Data: mustJSON(g)})
	case "step":
		if err := s.goals.UpdateStep(req.GoalID, req.StepID, req.Status); err != nil {
			emitErr(err)
			return
		}
		g, _ := s.goals.Load(req.GoalID)
		out.emit(Response{Type: "goal", SessionID: req.SessionID, Data: mustJSON(g)})
	case "abort":
		g, err := s.goals.Load(req.GoalID)
		if err != nil {
			emitErr(err)
			return
		}
		g.Status = "aborted"
		if err := s.goals.Save(g); err != nil {
			emitErr(err)
			return
		}
		if active, _ := s.goals.GetActive(); active != nil && active.ID == g.ID {
			_ = s.goals.ClearActive()
		}
		out.emit(Response{Type: "goal", SessionID: req.SessionID, Data: mustJSON(g)})
	case "delete":
		if err := s.goals.Delete(req.GoalID); err != nil {
			emitErr(err)
			return
		}
		out.emit(Response{Type: "goal", SessionID: req.SessionID,
			Data: mustJSON(map[string]string{"deleted": req.GoalID})})
	case "set_active":
		if err := s.goals.SetActive(req.GoalID); err != nil {
			emitErr(err)
			return
		}
		out.emit(Response{Type: "goal", SessionID: req.SessionID,
			Data: mustJSON(map[string]string{"active": req.GoalID})})
	case "active":
		g, _ := s.goals.GetActive()
		out.emit(Response{Type: "goal", SessionID: req.SessionID, Data: mustJSON(g)})
	default:
		out.emit(Response{Type: "error", SessionID: req.SessionID,
			Error: fmt.Sprintf("unknown goal action %q", action)})
	}
}

// ---------- compact ----------

// handleCompact summarises a session's history into the provider's small model
// and rewrites the stored session to the summary + trailing messages.
func (s *server) handleCompact(ctx context.Context, req Request, out *writer) {
	sid := req.SessionID
	if sid == "" {
		out.emit(Response{Type: "error", SessionID: sid, Error: "session_id is required"})
		return
	}
	sess, err := s.store.Load(sid)
	if err != nil {
		out.emit(Response{Type: "error", SessionID: sid, Error: "load session: " + err.Error()})
		return
	}
	if len(sess.Messages) < 4 {
		out.emit(Response{Type: "error", SessionID: sid, Error: "nothing to compact"})
		return
	}

	prov, modelID, err := s.resolveProvider(sess.Model)
	if err != nil {
		out.emit(Response{Type: "error", SessionID: sid, Error: err.Error()})
		return
	}
	if small := s.loaded.Config.SmallModel; small != "" {
		if pid, mid := config.SplitModel(small); s.provs[pid] != nil {
			prov = s.provs[pid]
			modelID = mid
		}
	}

	keep := 4
	head := append([]provider.Message(nil), sess.Messages[:len(sess.Messages)-keep]...)
	tail := append([]provider.Message(nil), sess.Messages[len(sess.Messages)-keep:]...)

	compactCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	reqMsg := provider.Request{
		Model:     modelID,
		System:    agent.CompactPrompt,
		Messages:  append(head, provider.UserText("Summarise the conversation above now.")),
		MaxTokens: compactionTokenLimit(s.loaded.Config.MaxTokens),
	}
	ch := make(chan provider.Event, 128)
	go prov.Stream(compactCtx, reqMsg, ch)

	var (
		sb    strings.Builder
		usage provider.Usage
	)
	for ev := range ch {
		switch ev.Kind {
		case provider.EventText:
			sb.WriteString(ev.Text)
		case provider.EventUsage:
			if ev.Usage != nil {
				usage.InputTokens += ev.Usage.InputTokens
				usage.OutputTokens += ev.Usage.OutputTokens
				usage.CacheReadTokens += ev.Usage.CacheReadTokens
				usage.CacheWriteTokens += ev.Usage.CacheWriteTokens
			}
		case provider.EventError:
			if ev.Err != nil {
				out.emit(Response{Type: "error", SessionID: sid, Error: "compact: " + ev.Err.Error()})
				return
			}
		}
	}
	summary := strings.TrimSpace(sb.String())
	if summary == "" {
		out.emit(Response{Type: "error", SessionID: sid, Error: "compact produced no summary"})
		return
	}

	// Rewrite the stored session: summary pair + trailing messages.
	sess.Messages = append([]provider.Message{
		provider.UserText("Summary of the conversation so far:\n\n" + summary),
		provider.AssistantText("Understood. Continuing from that state."),
	}, tail...)
	sess.Usage = session.Usage{
		Input:      usage.InputTokens,
		Output:     usage.OutputTokens,
		CacheRead:  usage.CacheReadTokens,
		CacheWrite: usage.CacheWriteTokens,
	}
	if err := s.store.Save(sess); err != nil {
		out.emit(Response{Type: "error", SessionID: sid, Error: "save compacted session: " + err.Error()})
		return
	}
	if s.usage != nil && modelID != "" {
		_ = s.usage.Record(modelID, usage.InputTokens, usage.OutputTokens,
			usage.CacheReadTokens, usage.CacheWriteTokens)
	}
	out.emit(Response{Type: "compact", SessionID: sid,
		Data: mustJSON(map[string]any{
			"summary": summary,
			"model":   modelID,
		})})
}

func compactionTokenLimit(configured int) int {
	if configured > 0 && configured < 2048 {
		return configured
	}
	return 2048
}

// ---------- mcp ----------

// handleMCP reports the live MCP server connection state.
func (s *server) handleMCP(out *writer) {
	names := s.mcp.ServerNames()
	errs := s.mcp.Errors()
	entries := make([]map[string]any, 0, len(names)+len(errs))
	for _, name := range names {
		tools := s.mcp.ServerTools(name)
		descs := make([]map[string]any, 0, len(tools))
		for _, td := range tools {
			descs = append(descs, map[string]any{
				"name":        td.Name,
				"description": td.Description,
			})
		}
		entries = append(entries, map[string]any{
			"name":   name,
			"status": "connected",
			"tools":  descs,
		})
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
	}
	for name, err := range errs {
		if seen[name] {
			continue
		}
		entries = append(entries, map[string]any{
			"name": name, "status": "error", "error": err.Error(),
		})
	}
	out.emit(Response{Type: "mcp", Data: mustJSON(entries)})
}

// ---------- plugins ----------

// handlePlugins lists, toggles, adds, and removes loaded plugins.
// Actions: list, toggle, add, remove.
func (s *server) handlePlugins(req Request, out *writer) {
	if s.plugins == nil {
		out.emit(Response{Type: "error", SessionID: req.SessionID, Error: "plugins not available"})
		return
	}
	switch req.Action {
	case "", "list":
		out.emit(Response{Type: "plugins", SessionID: req.SessionID, Data: mustJSON(s.plugins.List())})
	case "toggle":
		if req.Name == "" {
			out.emit(Response{Type: "error", SessionID: req.SessionID, Error: "plugin name is required"})
			return
		}
		if req.Enabled != nil {
			s.plugins.SetEnabled(req.Name, *req.Enabled)
		} else {
			s.plugins.Toggle(req.Name)
		}
		out.emit(Response{Type: "plugins", SessionID: req.SessionID,
			Data: mustJSON(map[string]any{
				"name":    req.Name,
				"enabled": s.plugins.IsEnabled(req.Name),
			})})
	case "add":
		source := strings.TrimSpace(req.Source)
		if source == "" {
			out.emit(Response{Type: "error", SessionID: req.SessionID, Error: "source is required (file path or http(s) URL)"})
			return
		}
		var manifests []plugin.Manifest
		if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
			man, err := plugin.LoadURL(source)
			if err != nil {
				out.emit(Response{Type: "error", SessionID: req.SessionID, Error: "plugin add: " + err.Error()})
				return
			}
			manifests = append(manifests, man)
			// Persist URL plugins so they load on the next launch, matching
			// how the config "plugin" list is read at startup.
			if !slicesContains(s.loaded.Config.Plugins, source) {
				s.loaded.Config.Plugins = append(s.loaded.Config.Plugins, source)
				if err := config.SaveConfigPatch(map[string]any{"plugin": s.loaded.Config.Plugins}); err != nil {
					out.emit(Response{Type: "error", SessionID: req.SessionID, Error: "plugin add: " + err.Error()})
					return
				}
			}
		} else {
			ms, err := plugin.LoadDir(source)
			if err != nil || len(ms) == 0 {
				man, ok := plugin.LoadFile(source)
				if !ok {
					out.emit(Response{Type: "error", SessionID: req.SessionID,
						Error: "no plugin manifests found at: " + source})
					return
				}
				manifests = append(manifests, man)
			} else {
				manifests = ms
			}
		}
		var names []string
		for _, man := range manifests {
			hooks := plugin.ManifestToHooks(man)
			s.plugins.Register(hooks)
			s.plugins.SetEnabled(man.Name, man.IsEnabled())
			names = append(names, man.Name)
		}
		out.emit(Response{Type: "plugins", SessionID: req.SessionID,
			Data: mustJSON(map[string]any{
				"added": names,
				"list":  s.plugins.List(),
			})})
	case "remove":
		if req.Name == "" {
			out.emit(Response{Type: "error", SessionID: req.SessionID, Error: "plugin name is required"})
			return
		}
		if !s.plugins.Remove(req.Name) {
			out.emit(Response{Type: "error", SessionID: req.SessionID, Error: "plugin not found: " + req.Name})
			return
		}
		// Drop the name from the persisted config list if it was there.
		filtered := s.loaded.Config.Plugins[:0]
		for _, entry := range s.loaded.Config.Plugins {
			if entry != req.Name {
				filtered = append(filtered, entry)
			}
		}
		if len(filtered) != len(s.loaded.Config.Plugins) {
			s.loaded.Config.Plugins = filtered
			if err := config.SaveConfigPatch(map[string]any{"plugin": filtered}); err != nil {
				out.emit(Response{Type: "error", SessionID: req.SessionID, Error: "plugin remove: " + err.Error()})
				return
			}
		}
		out.emit(Response{Type: "plugins", SessionID: req.SessionID, Data: mustJSON(s.plugins.List())})
	default:
		out.emit(Response{Type: "error", SessionID: req.SessionID,
			Error: fmt.Sprintf("unknown plugins action %q", req.Action)})
	}
}

func slicesContains(list []string, target string) bool {
	for _, entry := range list {
		if entry == target {
			return true
		}
	}
	return false
}

// ---------- auth ----------

// authRow is one provider line in the /auth-style listing.
type authRow struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Type         string `json:"type"` // wire flavor: openai | anthropic
	Auth         string `json:"auth"` // api_key | oauth_device_code | ...
	Connected    bool   `json:"connected"`
	EnvOnly      bool   `json:"env_only"`
	Custom       bool   `json:"custom"`
	EnvVar       string `json:"env_var,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
	Detail       string `json:"detail,omitempty"`
	ModelCount   int    `json:"model_count,omitempty"`
	DefaultModel string `json:"default_model,omitempty"`
	KeyCount     int    `json:"key_count,omitempty"`
	MaskedKey    string `json:"masked_key,omitempty"` // never the real key
	KeyMode      string `json:"key_mode,omitempty"`   // single | round-robin | failover
	OnlyFree     bool   `json:"only_free,omitempty"`
	Disabled     bool   `json:"disabled,omitempty"`
}

// maskKey renders a key with only its first and last four characters, the
// same way the terminal /auth flow does. Keys are never returned in full.
func maskKey(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("•", len(s))
	}
	return s[:4] + strings.Repeat("•", len(s)-8) + s[len(s)-4:]
}

// handleAuth serves provider status and credential editing, mirroring the
// /auth flow in the terminal so the desktop can manage the same auth.json.
// Actions: list (default), get, save, update, add_keys, remove_key, remove.
func (s *server) handleAuth(req Request, out *writer) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	creds, err := config.LoadCredentials()
	if err != nil {
		out.emit(Response{Type: "error", SessionID: req.SessionID, Error: "load credentials: " + err.Error()})
		return
	}
	emitErr := func(err error) {
		out.emit(Response{Type: "error", SessionID: req.SessionID, Error: err.Error()})
	}
	emitRows := func() {
		out.emit(Response{Type: "auth", SessionID: req.SessionID, Data: mustJSON(authRows(creds))})
	}

	switch req.Action {
	case "", "list":
		emitRows()
	case "get":
		if strings.TrimSpace(req.Provider) == "" {
			emitErr(errors.New("provider is required"))
			return
		}
		rows := authRows(creds)
		for _, row := range rows {
			if row.ID == req.Provider {
				out.emit(Response{Type: "auth", SessionID: req.SessionID, Data: mustJSON(row)})
				return
			}
		}
		out.emit(Response{Type: "error", SessionID: req.SessionID, Error: "provider not found: " + req.Provider})
	case "save":
		if strings.TrimSpace(req.Provider) == "" {
			emitErr(errors.New("provider is required"))
			return
		}
		// Preserve existing fields (multi-key, mode, disabled, model list,
		// default model) when the client only sends the fields it changed.
		cred, _ := creds.Get(req.Provider)
		if key := strings.TrimSpace(req.APIKey); key != "" {
			if len(cred.APIKeys) == 0 {
				cred.APIKey = key
			} else {
				cred.APIKeys = append(cred.APIKeys, key)
			}
		}
		if base := strings.TrimSpace(req.BaseURL); base != "" {
			cred.BaseURL = base
		}
		if label := strings.TrimSpace(req.Label); label != "" {
			cred.Label = label
		}
		if entry, ok := catalog.Get(req.Provider); ok {
			if cred.Type == "" {
				cred.Type = entry.Flavor
			}
			if cred.Label == "" {
				cred.Label = entry.Name
			}
			cred.Custom = false
		} else if cred.Type == "" {
			// Unknown id: treat it as a custom OpenAI-flavoured endpoint.
			cred.Type = catalog.FlavorOpenAI
			cred.Custom = true
		}
		creds.Set(req.Provider, cred)
		if err := creds.Save(); err != nil {
			emitErr(err)
			return
		}
		emitRows()
	case "update":
		if strings.TrimSpace(req.Provider) == "" {
			emitErr(errors.New("provider is required"))
			return
		}
		cred, ok := creds.Get(req.Provider)
		if !ok {
			emitErr(errors.New("provider not configured: " + req.Provider))
			return
		}
		if req.OnlyFree != nil {
			cred.OnlyFree = *req.OnlyFree
		}
		if req.Disabled != nil {
			cred.Disabled = *req.Disabled
		}
		if mode := strings.TrimSpace(req.KeyMode); mode != "" {
			switch mode {
			case "single", "round-robin", "failover":
				cred.APIKeyMode = mode
			default:
				emitErr(errors.New("key_mode must be single, round-robin, or failover"))
				return
			}
		}
		if base := strings.TrimSpace(req.BaseURL); base != "" {
			cred.BaseURL = base
		}
		if label := strings.TrimSpace(req.Label); label != "" {
			cred.Label = label
		}
		if model := strings.TrimSpace(req.DefaultModel); model != "" {
			cred.Default = model
		}
		creds.Set(req.Provider, cred)
		if err := creds.Save(); err != nil {
			emitErr(err)
			return
		}
		emitRows()
	case "add_keys":
		if strings.TrimSpace(req.Provider) == "" {
			emitErr(errors.New("provider is required"))
			return
		}
		if len(req.APIKeys) == 0 {
			emitErr(errors.New("api_keys is required"))
			return
		}
		cred, ok := creds.Get(req.Provider)
		if !ok {
			emitErr(errors.New("provider not configured: " + req.Provider))
			return
		}
		existing := creds.AllKeys(req.Provider)
		for _, key := range req.APIKeys {
			if clean := strings.TrimSpace(key); clean != "" {
				existing = append(existing, clean)
			}
		}
		if len(existing) == 1 {
			cred.APIKey = existing[0]
			cred.APIKeys = nil
		} else {
			cred.APIKeys = existing
		}
		creds.Set(req.Provider, cred)
		if err := creds.Save(); err != nil {
			emitErr(err)
			return
		}
		emitRows()
	case "remove_key":
		if strings.TrimSpace(req.Provider) == "" {
			emitErr(errors.New("provider is required"))
			return
		}
		cred, ok := creds.Get(req.Provider)
		if !ok {
			emitErr(errors.New("provider not configured: " + req.Provider))
			return
		}
		keys := creds.AllKeys(req.Provider)
		if req.KeyIndex < 1 || req.KeyIndex > len(keys) {
			emitErr(fmt.Errorf("key_index out of range (1..%d)", len(keys)))
			return
		}
		keys = append(keys[:req.KeyIndex-1], keys[req.KeyIndex:]...)
		if len(keys) == 1 {
			cred.APIKey = keys[0]
			cred.APIKeys = nil
		} else {
			cred.APIKeys = keys
		}
		creds.Set(req.Provider, cred)
		if err := creds.Save(); err != nil {
			emitErr(err)
			return
		}
		emitRows()
	case "remove":
		if strings.TrimSpace(req.Provider) == "" {
			emitErr(errors.New("provider is required"))
			return
		}
		creds.Remove(req.Provider)
		if err := creds.Save(); err != nil {
			emitErr(err)
			return
		}
		emitRows()
	default:
		out.emit(Response{Type: "error", SessionID: req.SessionID,
			Error: fmt.Sprintf("unknown auth action %q", req.Action)})
	}
}

// authRows renders the provider list the same way the TUI does: saved
// credentials first, then the catalog with env-var-only entries marked.
func authRows(creds *config.Credentials) []authRow {
	var rows []authRow
	seen := map[string]bool{}
	add := func(row authRow) {
		if seen[row.ID] {
			return
		}
		seen[row.ID] = true
		rows = append(rows, row)
	}

	hostOf := func(u string) string {
		s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
		if i := strings.IndexByte(s, '/'); i > 0 {
			s = s[:i]
		}
		return s
	}

	// Saved credentials, including custom providers.
	for _, id := range creds.IDs() {
		cred, _ := creds.Get(id)
		label := cred.Label
		if label == "" {
			if entry, ok := catalog.Get(id); ok {
				label = entry.Name
			} else {
				label = id
			}
		}
		detail := hostOf(cred.BaseURL)
		if n := len(cred.Models); n > 0 {
			detail += fmt.Sprintf(" · %d models", n)
		}
		keys := creds.AllKeys(id)
		mode := cred.APIKeyMode
		if mode == "" {
			mode = "single"
		}
		row := authRow{ID: id, Label: label, Type: cred.Type, Auth: "api_key",
			Connected: !cred.Disabled, Custom: cred.Custom, Detail: detail,
			BaseURL: cred.BaseURL, ModelCount: len(cred.Models),
			DefaultModel: cred.Default, KeyCount: len(keys),
			KeyMode: mode, OnlyFree: cred.OnlyFree, Disabled: cred.Disabled}
		if len(keys) > 0 {
			row.MaskedKey = maskKey(keys[0])
		}
		add(row)
	}

	// Catalog entries, marking any satisfied purely by an environment variable.
	for _, entry := range catalog.Registry {
		if seen[entry.ID] {
			continue
		}
		if key, envName := entry.EnvKey(); key != "" {
			add(authRow{ID: entry.ID, Label: entry.Name, Type: entry.Flavor, Auth: entry.Auth,
				Connected: true, EnvOnly: true, EnvVar: envName,
				Detail: "from $" + envName, BaseURL: entry.EnvBaseURL()})
			continue
		}
		detail := entry.Note
		if detail == "" {
			detail = hostOf(entry.BaseURL)
		}
		if entry.Auth == catalog.AuthDeviceCode {
			detail = "browser sign-in · " + detail
		}
		add(authRow{ID: entry.ID, Label: entry.Name, Type: entry.Flavor, Auth: entry.Auth,
			Detail: detail, BaseURL: entry.BaseURL})
	}
	return rows
}

// ---------- agents ----------

// handleAgents lists, kills, sends to, and steers live agents in a session.
// Actions: list, kill, send, steer.
func (s *server) handleAgents(req Request, out *writer) {
	s.agentsMu.Lock()
	reg := s.agents[req.SessionID]
	s.agentsMu.Unlock()
	if reg == nil {
		out.emit(Response{Type: "error", SessionID: req.SessionID,
			Error: "no active run for session " + req.SessionID})
		return
	}
	switch req.Action {
	case "", "list":
		out.emit(Response{Type: "agents", SessionID: req.SessionID, Data: mustJSON(reg.List())})
	case "kill":
		if !reg.Kill(req.AgentID) {
			out.emit(Response{Type: "error", SessionID: req.SessionID,
				Error: fmt.Sprintf("agent %q not found", req.AgentID)})
			return
		}
		out.emit(Response{Type: "agents", SessionID: req.SessionID,
			Data: mustJSON(map[string]string{"killed": req.AgentID})})
	case "send":
		if err := reg.Send(req.AgentID, req.From, req.Content); err != nil {
			out.emit(Response{Type: "error", SessionID: req.SessionID, Error: err.Error()})
			return
		}
		out.emit(Response{Type: "agents", SessionID: req.SessionID,
			Data: mustJSON(map[string]string{"sent": req.AgentID})})
	case "steer":
		if err := reg.Steer(req.AgentID, req.From, req.Content); err != nil {
			out.emit(Response{Type: "error", SessionID: req.SessionID, Error: err.Error()})
			return
		}
		out.emit(Response{Type: "agents", SessionID: req.SessionID,
			Data: mustJSON(map[string]string{"steered": req.AgentID})})
	default:
		out.emit(Response{Type: "error", SessionID: req.SessionID,
			Error: fmt.Sprintf("unknown agents action %q", req.Action)})
	}
}

// ---------- agent run ----------

// handleRun executes one agent run using agent.Runner directly, streaming
// events back as ndjson and routing permission requests to the client.
func (s *server) handleRun(ctx context.Context, req Request, out responseEmitter) {
	sid := req.SessionID
	if sid == "" {
		sid = session.NewID()
	}
	if req.Prompt == "" {
		out.emit(Response{Type: "error", SessionID: sid, Error: "prompt is required"})
		return
	}

	model := s.loaded.Config.Model
	if req.Model != "" {
		model = req.Model
	}
	prov, modelID, err := s.resolveProvider(model)
	if err != nil {
		out.emit(Response{Type: "error", SessionID: sid, Error: err.Error()})
		return
	}
	cacheRetention, cacheTTLSeconds, _, cacheWarm := s.loaded.Config.CacheForProvider(prov.Name())
	models := s.modelsForProvider(prov.Name(), prov)
	thinking := req.Thinking
	if thinking == "" {
		thinking = req.Reasoning
	}
	reasoning, err := resolveRunReasoning(prov.Name(), modelID, thinking, models)
	if err != nil {
		out.emit(Response{Type: "error", SessionID: sid, Error: err.Error()})
		return
	}

	cwd := s.cwd
	if req.Cwd != "" {
		if abs, aerr := filepath.Abs(req.Cwd); aerr == nil {
			cwd = abs
		}
	}
	agentName := req.Agent
	if agentName == "" {
		agentName = "build"
	}
	// maxTurns <= 0 means unlimited; the repeated-call guard still stops loops.
	maxTurns := req.MaxTurns

	// Build run-local permission and sandbox state. Desktop can dispatch
	// concurrent sessions with different execution controls; mutating the
	// daemon-wide holder here would make those runs race and leak policy.
	profile := req.PermissionProfile
	if profile == "" {
		profile = s.defaultProfile
	}
	sandboxMode := req.Sandbox
	if sandboxMode == "" {
		sandboxMode = s.defaultSandbox
	}
	perms, runSandbox, err := resolveRunSecurityAtRoot(s.loaded, cwd, profile, sandboxMode, req.Yolo)
	if err != nil {
		out.emit(Response{Type: "error", SessionID: sid, Error: err.Error()})
		return
	}

	// Build the permission asker that routes to the client via ndjson.
	var ask agent.PermissionAsker
	if perms.Yolo() {
		ask = func(_ context.Context, _ permission.Request) agent.PermissionDecision {
			return agent.DecideAlways
		}
	} else {
		ask = func(askCtx context.Context, permReq permission.Request) agent.PermissionDecision {
			reqID, pending := s.registerPerm()
			paths := permReq.Paths
			if len(paths) == 0 && permReq.Path != "" {
				paths = []string{permReq.Path}
			}
			out.emit(Response{
				Type:      "event",
				SessionID: sid,
				Event:     "PermissionRequest",
				Data: mustJSON(map[string]any{
					"request_id": reqID,
					"tool":       permReq.Tool,
					"command":    permReq.Command,
					"path":       permReq.Path,
					"paths":      paths,
					"host":       permReq.Host,
					"title":      permReq.Title,
					"body":       permReq.Body,
				}),
			})
			select {
			case decision := <-pending.ch:
				return decision
			case <-askCtx.Done():
				return agent.DecideReject
			}
		}
	}

	// Build system prompt. The instruction globs and the model id must match
	// the TUI so the injected markdown is byte-identical in both frontends.
	stableSystem := agent.BuildPrompt + agent.ProjectContext(cwd, s.loaded.Config.Instructions)
	system := stableSystem + agent.Environment(cwd, modelID, agentName, "")

	// Snapshotter for undo support, shared per project so undo/redo survives
	// across runs. Created lazily; disabled when git is missing.
	var snapshotter agent.Snapshotter
	s.snapMu.Lock()
	snaps, ok := s.snaps[cwd]
	if !ok {
		snaps, _ = session.NewSnapshotter(cwd, config.DataDir())
		s.snaps[cwd] = snaps
	}
	s.snapMu.Unlock()
	if snaps != nil && snaps.Enabled() {
		snapshotter = snaps
	}

	// Build history: resume from existing session or start fresh.
	var (
		history      []provider.Message
		priorSession *session.Session
	)
	if req.Resume {
		if existing, lerr := s.store.Load(sid); lerr == nil {
			priorSession = existing
			history = append([]provider.Message(nil), existing.Messages...)
		}
	}
	if len(history) == 0 {
		history = []provider.Message{provider.UserText(req.Prompt)}
	} else if req.Prompt != "" {
		// A resumed run continues the thread with the new instruction.
		history = append(history, provider.UserText(req.Prompt))
	}

	// Attach files to the user message. Images require a vision-capable model;
	// plain-text attachments are embedded after the prompt text.
	if len(req.Attachments) > 0 {
		userMsg, err := buildUserMessage(req.Prompt, req.Attachments, models, modelID)
		if err != nil {
			out.emit(Response{Type: "error", SessionID: sid, Error: err.Error()})
			return
		}
		// On resume the last message is the freshly appended prompt; on a new
		// thread it is the only message.
		history[len(history)-1] = userMsg
	}

	// Create a cancellable context for this run. Only one generation may own a
	// session at a time; otherwise cancellation and persisted history become
	// ambiguous even when the client can reject stale correlated events.
	runCtx, cancel := context.WithCancel(ctx)
	s.runMu.Lock()
	if _, exists := s.runCancel[sid]; exists {
		s.runMu.Unlock()
		cancel()
		out.emit(Response{Type: "error", SessionID: sid, Error: "session already has an active run"})
		return
	}
	s.runCancel[sid] = activeRun{runID: req.RunID, cancel: cancel}
	s.runMu.Unlock()
	defer func() {
		s.runMu.Lock()
		delete(s.runCancel, sid)
		s.runMu.Unlock()
		cancel()
	}()

	// Per-session agent registry so clients can list, kill, send, and steer
	// agents while the run is live. The root entry is the orchestrator.
	s.agentsMu.Lock()
	reg := s.agents[sid]
	if reg == nil {
		reg = agent.NewRegistry(agent.MaxAllowedDepth, 8)
		s.agents[sid] = reg
	}
	s.agentsMu.Unlock()
	agentID := ""
	if reg != nil {
		agentID, _ = reg.Register(&agent.AgentEntry{
			Name:        agentName,
			Depth:       0,
			Status:      agent.AgentIdle,
			Description: req.Prompt,
			Cancel:      cancel,
		})
	}
	defer func() {
		s.agentsMu.Lock()
		delete(s.agents, sid)
		s.agentsMu.Unlock()
	}()

	runner := agent.New(agent.Config{
		Provider:           prov,
		Model:              modelID,
		System:             system,
		SystemStable:       stableSystem,
		MaxTokens:          s.loaded.Config.MaxTokens,
		Reasoning:          reasoning,
		Tools:              s.sessionTools(sid, cwd, model, reasoning, ask, perms, runSandbox, reg, out),
		Perms:              perms,
		Ask:                ask,
		Cwd:                cwd,
		SessionID:          sid,
		AgentName:          agentName,
		AgentID:            agentID,
		Registry:           reg,
		MaxTurns:           maxTurns,
		Plugins:            s.plugins,
		Parallel:           true,
		Snapshotter:        snapshotter,
		CacheRetention:     provider.CacheRetention(cacheRetention),
		CacheTTLSeconds:    cacheTTLSeconds,
		WarmCache:          cacheWarm,
		MaxReasoningTurns:  s.loaded.Config.CacheMaxReasoningTurns,
		MaxToolResultBytes: s.loaded.Config.CacheMaxToolResultBytes,
	})

	ch := make(chan agent.Event, 256)
	var (
		appended []provider.Message
		runErr   error
		wg       sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		appended, runErr = runner.Run(runCtx, history, ch)
	}()

	// Track current context separately from cumulative billing usage. Context is
	// the full provider-facing prompt for the latest turn, including cached
	// tokens, whereas session counters accumulate over every resumed run.
	var totalUsage session.Usage
	var optimization session.OptimizationUsage
	lastContextUsed := 0
	if priorSession != nil {
		totalUsage = priorSession.Usage
		optimization = priorSession.Optimization
		lastContextUsed = priorSession.ContextUsed
	}
	contextLimit := 0
	for _, modelInfo := range models {
		if modelInfo.ID == modelID {
			contextLimit = modelInfo.ContextWindow
			break
		}
	}

	for ev := range ch {
		switch ev.Kind {
		case agent.EvUsage:
			if ev.Usage == nil {
				break
			}
			totalUsage.Input += ev.Usage.InputTokens
			totalUsage.Output += ev.Usage.OutputTokens
			totalUsage.CacheRead += ev.Usage.CacheReadTokens
			totalUsage.CacheWrite += ev.Usage.CacheWriteTokens
			turnContextUsed := ev.Usage.InputTokens + ev.Usage.CacheReadTokens + ev.Usage.CacheWriteTokens
			if turnContextUsed > 0 {
				lastContextUsed = turnContextUsed
			}
			// Record in the usage tracker.
			if s.usage != nil {
				_ = s.usage.Record(model, ev.Usage.InputTokens, ev.Usage.OutputTokens,
					ev.Usage.CacheReadTokens, ev.Usage.CacheWriteTokens)
			}
			out.emit(Response{Type: "event", SessionID: sid, Event: "Usage",
				Data: mustJSON(map[string]int{
					"input_tokens":       ev.Usage.InputTokens,
					"output_tokens":      ev.Usage.OutputTokens,
					"cache_read_tokens":  ev.Usage.CacheReadTokens,
					"cache_write_tokens": ev.Usage.CacheWriteTokens,
					"context_tokens":     lastContextUsed,
					"context_limit":      contextLimit,
				})})

		case agent.EvPermissionAsk:
			// Handled by the ask callback — no separate event needed.

		case agent.EvToolEnd:
			if ev.Tool != nil && ev.Tool.Optimization != nil {
				stats := ev.Tool.Optimization
				optimization.ToolResults++
				optimization.OriginalTokens += stats.OriginalTokens
				optimization.ProviderTokens += stats.CompressedTokens
				optimization.SavedTokens += stats.OriginalTokens - stats.CompressedTokens
			}
			emitAgentEvent(out, sid, ev)

		case agent.EvDone:
			// Loop will end when ch closes.

		default:
			emitAgentEvent(out, sid, ev)
		}
	}

	wg.Wait()

	// Persist the session.
	allMsgs := history
	if len(appended) > 0 {
		allMsgs = append(append([]provider.Message{}, history...), appended...)
	}
	sess := &session.Session{
		ID:             sid,
		Title:          session.Title(allMsgs),
		Cwd:            cwd,
		Model:          model,
		Agent:          agentName,
		Messages:       allMsgs,
		Usage:          totalUsage,
		ContextUsed:    lastContextUsed,
		SentTranscript: append([]provider.Message(nil), allMsgs...),
		Optimization:   optimization,
	}
	// Preserve existing metadata on resume.
	metadataSource := priorSession
	if latest, loadErr := s.store.Load(sid); loadErr == nil {
		metadataSource = latest
	}
	if metadataSource != nil {
		if metadataSource.Title != "" && metadataSource.Title != "untitled" {
			sess.Title = metadataSource.Title
		}
		sess.Created = metadataSource.Created
		sess.Category = metadataSource.Category
		sess.Favorite = metadataSource.Favorite
		sess.Parent = metadataSource.Parent
		sess.EnvGit = metadataSource.EnvGit
	}
	if err := s.store.Save(sess); err != nil {
		out.emit(Response{Type: "error", SessionID: sid, Error: fmt.Sprintf("failed to save session: %v", err)})
		out.emit(Response{Type: "done", SessionID: sid})
		return
	}
	_ = s.store.SetCurrent(cwd, sid)

	if runErr != nil {
		if runCtx.Err() != nil {
			// The run was cancelled (interrupt or shutdown), not a failure.
			out.emit(Response{Type: "cancelled", SessionID: sid})
		} else {
			out.emit(Response{Type: "error", SessionID: sid, Error: runErr.Error()})
		}
	}
	out.emit(Response{Type: "done", SessionID: sid})
}

// ---------- delegation tools ----------

// sessionTools returns a per-session registry that layers the delegation
// tools (subagent spawn plus swarm/chat/steer/report) on top of the daemon-wide
// tool set, so rickserve exposes the same tools the TUI gives its primary
// agent. The registry is per-session because the spawn closure and the agent
// registry are session-bound.
func (s *server) sessionTools(sid, cwd, model string, reasoning provider.ReasoningEffort, ask agent.PermissionAsker, perms *permission.Engine, runSandbox *sandbox.Holder, reg *agent.Registry, out responseEmitter) *tools.Registry {
	rt := tools.NewRegistry()
	for _, name := range s.tools.Names() {
		if t, ok := s.tools.Get(name); ok {
			rt.Register(t)
		}
	}
	// Replace the daemon-default bash tool with one bound to this run's holder.
	// Registry.Register replaces by name without changing tool order.
	rt.Register(tools.BashTool{Sandbox: runSandbox})

	specs := s.subagentSpecs()
	maxDepth := 1
	if d := s.loaded.Config.SubagentDepth; d != nil && *d > 0 {
		maxDepth = *d
	}
	spawn := s.spawnSubagent(sid, cwd, reasoning, ask, perms, reg, out, rt, specs, maxDepth)
	rt.Register(agent.TaskTool{Specs: specs, MaxDepth: maxDepth, Spawn: spawn})
	rt.Register(agent.ParallelTaskTool{Specs: specs, MaxDepth: maxDepth, Spawn: spawn})
	// Headless swarm support: the TUI registers the swarm tool only when a
	// UI swarm manager exists; rickserve runs the same team machinery through
	// RunTaskTeam so desktop sessions can spawn collaborative agent teams.
	rt.Register(agent.SwarmTool{Manager: s.spawnSwarm(sid, cwd, model, reasoning, ask, perms, out, rt)})
	if reg != nil {
		rt.Register(agent.ChatTool{Registry: reg})
		rt.Register(agent.SteerTool{Registry: reg})
		rt.Register(agent.ReportTool{Registry: reg})
	}
	return rt
}

// subagentSpecs merges the built-in subagent types with config-defined
// subagents, exactly like the TUI's registerTaskTool.
func (s *server) subagentSpecs() map[string]agent.SubagentSpec {
	specs := agent.BuiltinSubagents()
	for name, a := range s.loaded.Config.Agents {
		if a.Mode != "subagent" && a.Mode != "all" {
			continue
		}
		spec := agent.SubagentSpec{
			Name:        name,
			Description: a.Description,
			Prompt:      a.Prompt,
			Model:       a.Model,
		}
		if spec.Description == "" {
			spec.Description = "Custom subagent defined in config."
		}
		if spec.Prompt == "" {
			spec.Prompt = agent.GeneralSubagentPrompt
		}
		specs[name] = spec
	}
	return specs
}

// spawnSubagent runs one subagent with the same provider, permission engine,
// plugin set and event stream as the parent run, mirroring the TUI's
// foreground delegation path.
func (s *server) spawnSubagent(sid, cwd string, reasoning provider.ReasoningEffort, ask agent.PermissionAsker, perms *permission.Engine, reg *agent.Registry, out responseEmitter, sessionTools *tools.Registry, specs map[string]agent.SubagentSpec, maxDepth int) func(context.Context, string, string, string, int) (string, error) {
	return func(ctx context.Context, kind, description, prompt string, depth int) (string, error) {
		if depth > maxDepth || depth > agent.MaxAllowedDepth {
			return "", fmt.Errorf("subagent depth %d exceeds configured limit %d", depth, maxDepth)
		}
		spec, ok := specs[kind]
		if !ok {
			return "", fmt.Errorf("unknown subagent type %q", kind)
		}
		modelRef := spec.Model
		if modelRef == "" {
			modelRef = s.loaded.Config.Model
		}
		provID, modelID := config.SplitModel(modelRef)
		prov, ok := s.provs[provID]
		if !ok {
			return "", fmt.Errorf("subagent: unknown provider %q", provID)
		}
		cacheRetention, _, _, cacheWarm := s.loaded.Config.CacheForProvider(provID)

		subPerms := agent.SubagentPermissions(spec, perms, cwd)
		stableSys := spec.Prompt + agent.ProjectContext(cwd, s.loaded.Config.Instructions)
		sys := stableSys + agent.Environment(cwd, modelID, kind, "")
		toolSpec := spec
		if perms != nil && perms.Yolo() {
			toolSpec.ReadOnly = false
		}

		out.emit(Response{Type: "event", SessionID: sid, Event: "SubagentStart",
			Data: mustJSON(map[string]string{"kind": kind, "description": description})})
		toolCount := 0
		budgetAgentName := kind + "-" + session.NewID()
		result, runErr := agent.RunSubagent(ctx, agent.Config{
			Provider:           prov,
			Model:              modelID,
			System:             sys,
			SystemStable:       stableSys,
			MaxTokens:          s.loaded.Config.MaxTokens,
			Reasoning:          reasoning,
			Tools:              sessionTools,
			ToolFilter:         agent.SubagentToolFilter(toolSpec, nil),
			Perms:              subPerms,
			Ask:                ask,
			Cwd:                cwd,
			SessionID:          sid,
			AgentName:          budgetAgentName,
			Depth:              depth,
			MaxTurns:           0, // unlimited; the repeated-call guard still stops loops
			Plugins:            s.plugins,
			CacheRetention:     provider.CacheRetention(cacheRetention),
			WarmCache:          cacheWarm,
			MaxReasoningTurns:  s.loaded.Config.CacheMaxReasoningTurns,
			MaxToolResultBytes: s.loaded.Config.CacheMaxToolResultBytes,
			Parallel:           true,
			Registry:           reg,
		}, prompt, func(ev agent.Event) {
			if ev.Kind == agent.EvUsage && ev.Usage != nil && s.usage != nil {
				_ = s.usage.Record(modelRef, ev.Usage.InputTokens, ev.Usage.OutputTokens,
					ev.Usage.CacheReadTokens, ev.Usage.CacheWriteTokens)
			}
			if ev.Kind == agent.EvToolEnd {
				toolCount++
			}
			emitAgentEvent(out, sid, ev)
		})
		out.emit(Response{Type: "event", SessionID: sid, Event: "SubagentDone",
			Data: mustJSON(map[string]any{"kind": kind, "description": description, "tools": toolCount})})
		return result, runErr
	}
}

func swarmRuntimeResponse(sid, swarmID, swarmName string, event swarm.RuntimeEvent) (Response, bool) {
	agentState := map[string]any{"id": event.Name, "name": event.Name}
	switch event.Kind {
	case swarm.EventAgentStart:
		agentState["status"] = "working"
		agentState["action"] = "starting"
	case swarm.EventAgentDone:
		agentState["status"] = "completed"
		agentState["action"] = "done"
		if result, ok := event.Value.(string); ok {
			agentState["result"] = truncate(result, 4000)
		}
	case swarm.EventAgentFailed:
		agentState["status"] = "failed"
		agentState["action"] = "failed"
		if err, ok := event.Value.(error); ok {
			agentState["error"] = err.Error()
		}
	case swarm.EventAgentTool:
		agentEvent, ok := event.Value.(agent.Event)
		if !ok {
			return Response{}, false
		}
		if agentEvent.Kind == agent.EvUsage {
			return Response{}, false
		}
		if (agentEvent.Kind != agent.EvToolStart && agentEvent.Kind != agent.EvToolEnd) || agentEvent.Tool == nil {
			return Response{}, false
		}
		agentState["status"] = "working"
		agentState["current_tool"] = agentEvent.Tool.Name
		if agentEvent.Kind == agent.EvToolStart {
			agentState["action"] = strings.TrimSpace(agentEvent.Tool.Name + " " + agentEvent.Tool.Title)
		} else if agentEvent.Tool.IsError {
			agentState["action"] = agentEvent.Tool.Name + " failed"
		} else {
			agentState["action"] = agentEvent.Tool.Name + " completed"
		}
	default:
		return Response{}, false
	}
	return Response{Type: "event", SessionID: sid, Event: "agent.updated", Data: mustJSON(map[string]any{
		"swarm_id": swarmID,
		"name":     swarmName,
		"agents":   []any{agentState},
	})}, true
}

// spawnSwarm runs a swarm headlessly through the same worker machinery as the
// TUI: one agent.Runner per teammate, a shared task board, and RunTaskTeam for
// dependency-aware scheduling. Worker activity is folded into one AgentUpdate
// stream so desktop renders the same in-place team status the TUI does instead
// of appending every worker tool call to the parent transcript.
func (s *server) spawnSwarm(sid, cwd, model string, reasoning provider.ReasoningEffort, ask agent.PermissionAsker, perms *permission.Engine, out responseEmitter, sessionTools *tools.Registry) func(context.Context, string, string, []agent.SwarmAgentSpec, swarm.Topology) (string, error) {
	return func(ctx context.Context, name, goal string, specs []agent.SwarmAgentSpec, topo swarm.Topology) (string, error) {
		if len(specs) < 2 {
			return "", fmt.Errorf("at least 2 agents are required for a swarm (got %d)", len(specs))
		}
		prov, modelID, err := s.resolveProvider(model)
		if err != nil {
			return "", err
		}
		cacheRetention, _, _, cacheWarm := s.loaded.Config.CacheForProvider(prov.Name())
		team := swarm.NewSwarmContext(ctx, "swarm-"+session.NewID(), name, goal, topo)
		for _, spec := range specs {
			team.AddAgent(spec.Name, spec.Role)
		}
		if topo == swarm.TopologyStar && len(specs) > 0 {
			team.Primary = specs[0].Name
		}
		taskSpecs := make([]swarm.TaskSpec, 0, len(specs))
		for _, spec := range specs {
			taskID := spec.TaskID
			if taskID == "" {
				taskID = spec.Name
			}
			subject := spec.Subject
			if subject == "" {
				subject = spec.Role
			}
			if subject == "" {
				subject = "Complete assigned team work"
			}
			taskSpecs = append(taskSpecs, swarm.TaskSpec{ID: taskID, Subject: subject, Description: spec.Role, Owner: spec.Name, DependsOn: spec.DependsOn})
		}
		if err := team.Tasks.AddBatch(taskSpecs); err != nil {
			return "", err
		}

		jobs := make([]swarm.TeamJob, 0, len(specs))
		for _, spec := range specs {
			var allowedTools []string
			if strings.TrimSpace(spec.Tools) != "" {
				allowedTools = strings.FieldsFunc(spec.Tools, func(r rune) bool {
					return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
				})
			}
			workerTools := tools.NewFilteredSwarmRegistry(sessionTools, nil, allowedTools...)
			workerTools.Register(agent.TeamTool{Swarm: team})
			system := "You are an independent teammate reporting to the lead agent. Use the team tool to confirm your assigned task, inspect messages, share only useful findings, and complete or fail the task explicitly. Do not delegate or spawn agents. Return ONLY factual findings—no narration, no 'I'll research', and no 'Let me dig deeper'. Output clean, complete results with sources when applicable."
			cfg := agent.Config{
				Provider:           prov,
				Model:              modelID,
				System:             system,
				MaxTokens:          s.loaded.Config.MaxTokens,
				Reasoning:          reasoning,
				Tools:              workerTools,
				Perms:              perms,
				Ask:                ask,
				Cwd:                cwd,
				SessionID:          sid,
				AgentName:          spec.Name,
				AgentID:            team.ID + "/" + spec.Name,
				Depth:              1,
				MaxTurns:           0, // unlimited; the repeated-call guard still stops loops
				Plugins:            s.plugins,
				Parallel:           true,
				CacheRetention:     provider.CacheRetention(cacheRetention),
				WarmCache:          cacheWarm,
				MaxReasoningTurns:  s.loaded.Config.CacheMaxReasoningTurns,
				MaxToolResultBytes: s.loaded.Config.CacheMaxToolResultBytes,
			}
			taskID := spec.TaskID
			if taskID == "" {
				taskID = spec.Name
			}
			prompt := fmt.Sprintf("Team goal: %s\n\nYour identity: %s\nYour task ID: %s\nYour assignment: %s\n\nYour task is already claimed for you. Do not claim it again; coordinate through team messages, and finish with complete_task or fail_task.", goal, spec.Name, taskID, spec.Role)
			jobs = append(jobs, swarm.TeamJob{Name: spec.Name, TaskID: taskID, Runner: agent.NewAgentRunner(cfg, prompt)})
		}

		out.emit(Response{Type: "event", SessionID: sid, Event: "SwarmStart",
			Data: mustJSON(map[string]any{"swarm_id": team.ID, "name": name, "goal": goal, "agents": len(specs)})})
		results := swarm.RunTaskTeam(ctx, jobs, team.Tasks, 4, func(ev swarm.RuntimeEvent) {
			if aev, ok := ev.Value.(agent.Event); ok {
				if aev.Kind == agent.EvUsage && aev.Usage != nil && s.usage != nil {
					_ = s.usage.Record(model, aev.Usage.InputTokens, aev.Usage.OutputTokens,
						aev.Usage.CacheReadTokens, aev.Usage.CacheWriteTokens)
				}
			}
			if response, ok := swarmRuntimeResponse(sid, team.ID, name, ev); ok {
				out.emit(response)
			}
		})
		var b strings.Builder
		fmt.Fprintf(&b, "Swarm %q completed. Goal: %s\n", name, goal)
		for _, result := range results {
			if result.Err != nil {
				fmt.Fprintf(&b, "[%s] failed: %v\n", result.Name, result.Err)
			} else {
				fmt.Fprintf(&b, "[%s] %s\n", result.Name, result.Output)
			}
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
}

// emitAgentEvent streams a non-usage agent event to the client in the exact
// shape the main run loop uses, so subagent activity renders identically.
func emitAgentEvent(out responseEmitter, sid string, ev agent.Event) {
	switch ev.Kind {
	case agent.EvText:
		out.emit(Response{Type: "event", SessionID: sid, Event: "Content",
			Data: mustJSON(map[string]string{"text": ev.Text})})

	case agent.EvThinking:
		out.emit(Response{Type: "event", SessionID: sid, Event: "Thinking",
			Data: mustJSON(map[string]string{"text": ev.Text})})

	case agent.EvToolStart:
		if ev.Tool == nil {
			return
		}
		out.emit(Response{Type: "event", SessionID: sid, Event: "ToolUse",
			Data: mustJSON(map[string]any{
				"call_id": ev.Tool.CallID,
				"name":    ev.Tool.Name,
				"title":   ev.Tool.Title,
				"input":   json.RawMessage(orNull(ev.Tool.Input)),
			})})

	case agent.EvToolEnd:
		if ev.Tool == nil {
			return
		}
		out.emit(Response{Type: "event", SessionID: sid, Event: "ToolResult",
			Data: mustJSON(map[string]any{
				"call_id":  ev.Tool.CallID,
				"name":     ev.Tool.Name,
				"title":    ev.Tool.Title,
				"output":   ev.Tool.Output,
				"is_error": ev.Tool.IsError,
				"elapsed":  ev.Tool.Elapsed.Round(time.Millisecond).String(),
			})})

	case agent.EvError:
		if ev.Err != nil {
			out.emit(Response{Type: "event", SessionID: sid, Event: "Error",
				Data: mustJSON(map[string]string{"error": ev.Err.Error()})})
		}
	}
}

// ---------- tools ----------

// handleTools reports the exact tool set exposed to the agent loop, so
// clients can mirror the TUI's tool inventory 1:1. The session-bound
// delegation tools are listed alongside the daemon-wide registry.
func (s *server) handleTools(out *writer) {
	entries := make([]map[string]any, 0, 24)
	for _, schema := range s.tools.Schemas(nil) {
		entries = append(entries, map[string]any{
			"name":        schema.Name,
			"description": schema.Description,
		})
	}
	reg := agent.NewRegistry(agent.MaxAllowedDepth, 1)
	maxDepth := 1
	if d := s.loaded.Config.SubagentDepth; d != nil && *d > 0 {
		maxDepth = *d
	}
	specs := s.subagentSpecs()
	for _, tool := range []tools.Tool{
		agent.TaskTool{Specs: specs, MaxDepth: maxDepth},
		agent.ParallelTaskTool{Specs: specs, MaxDepth: maxDepth},
		agent.SwarmTool{},
		agent.ChatTool{Registry: reg},
		agent.SteerTool{Registry: reg},
		agent.ReportTool{Registry: reg},
	} {
		entries = append(entries, map[string]any{"name": tool.Name(), "description": tool.Description()})
	}
	out.emit(Response{Type: "tools", Data: mustJSON(entries)})
}

// ---------- helpers ----------

// buildUserMessage assembles the first user turn from a text prompt and any
// attached files. Images become provider image blocks and are only accepted
// when the target model reports vision support; other files are embedded as
// text after the prompt so the model still sees their contents.
func buildUserMessage(prompt string, attachments []Attachment, models []provider.ModelInfo, modelID string) (provider.Message, error) {
	var imageAtts, textAtts []Attachment
	for _, att := range attachments {
		if strings.HasPrefix(att.MediaType, "image/") {
			imageAtts = append(imageAtts, att)
		} else {
			textAtts = append(textAtts, att)
		}
	}

	if len(imageAtts) > 0 {
		supports := false
		for _, mi := range models {
			if strings.EqualFold(mi.ID, modelID) || strings.EqualFold(mi.Name, modelID) {
				if mi.SupportsImages {
					supports = true
				}
				break
			}
		}
		if !supports {
			return provider.Message{}, fmt.Errorf("model %q does not support image attachments", modelID)
		}
	}

	msg := provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.TextBlock(prompt)}}
	for _, att := range textAtts {
		msg.Content = append(msg.Content, provider.TextBlock(fmt.Sprintf("\n\n[attachment: %s]\n%s", att.Name, att.Data)))
	}
	for _, att := range imageAtts {
		msg.Content = append(msg.Content, provider.ImageBlock(att.MediaType, att.Data))
	}
	return msg, nil
}

// mustJSON marshals v, returning null on the (impossible) error path.
func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return data
}

func orNull(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}
	return string(raw)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
