// Command rick is a lightweight terminal AI coding agent.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"rick/internal/agent"
	"rick/internal/apply"
	"rick/internal/config"
	"rick/internal/doctor"
	"rick/internal/goal"
	"rick/internal/headless"
	"rick/internal/mcp"
	"rick/internal/permission"
	"rick/internal/plugin"
	"rick/internal/provider"
	"rick/internal/provider/anthropic"
	"rick/internal/provider/catalog"
	"rick/internal/provider/openai"
	"rick/internal/sandbox"
	"rick/internal/security"
	"rick/internal/session"
	"rick/internal/swarm"
	"rick/internal/theme"
	"rick/internal/tools"
	"rick/internal/tui"
	"rick/internal/usage"
	"rick/pkg/contextbudget"
)

var Version = "0.1.14"

func main() {
	var (
		flagModel       string
		flagAgent       string
		flagResume      string
		flagContinue    bool
		flagNew         bool
		flagPrompt      string
		flagYolo        bool
		flagSandbox     string
		flagProfile     string
		flagNoNetwork   bool
		flagEnforcement string
		flagTerse       bool
	)

	root := &cobra.Command{
		Use:   "rick [path]",
		Short: "A lightweight terminal AI coding agent",
		Long: "rick is a fast, keyboard-driven coding agent for the terminal.\n" +
			"Run it in any project directory to start a session.",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}
			return runTUI(dir, opts{
				model: flagModel, agent: flagAgent, resume: flagResume, cont: flagContinue,
				fresh: flagNew, prompt: flagPrompt, yolo: flagYolo,
				sandbox: flagSandbox, profile: flagProfile,
				noNetwork: flagNoNetwork, enforcement: flagEnforcement, terse: flagTerse,
			})
		},
	}

	root.Flags().StringVarP(&flagModel, "model", "m", "", "model to use (provider/model-id)")
	root.Flags().StringVarP(&flagAgent, "agent", "a", "", "agent to start in (build | plan)")
	root.Flags().StringVar(&flagResume, "resume", "", "resume a session by id")
	root.Flags().BoolVarP(&flagContinue, "continue", "c", false,
		"continue the most recent session in this directory")
	root.Flags().BoolVar(&flagNew, "new", false,
		"start a fresh session (now the default; kept for compatibility)")
	root.Flags().StringVarP(&flagPrompt, "prompt", "p", "", "send an initial prompt on startup")
	root.Flags().BoolVar(&flagYolo, "yolo", false, "skip all permission prompts (dangerous)")
	root.Flags().BoolVar(&flagTerse, "terse", false,
		"caveman mode: instruct the model to return zero conversational filler")
	root.Flags().StringVar(&flagSandbox, "sandbox", "",
		"command sandbox: read-only | workspace-write | trusted | off")
	root.Flags().StringVar(&flagProfile, "permission-profile", "",
		"permission profile: readonly | standard | trusted | ci (or a custom one)")
	root.Flags().BoolVar(&flagNoNetwork, "no-network", false,
		"deny network access to sandboxed commands")
	root.Flags().StringVar(&flagEnforcement, "sandbox-enforcement", "",
		"auto (default) | os (refuse to run unconfined) | static (analysis only)")

	root.AddCommand(
		sessionsCmd(),
		sessionExportCmd(),
		resumeCmd(),
		configCmd(),
		modelsCmd(),
		versionCmd(),
		runCmd(),
		execCmd(),
		serveCmd(),
		doctorCmd(),
		securityCmd(),
		applyCmd(),
		updateCmd(),
		uninstallCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "rick: "+err.Error())
		os.Exit(1)
	}
}

type opts struct {
	model       string
	agent       string
	resume      string
	cont        bool
	fresh       bool
	prompt      string
	yolo        bool
	sandbox     string
	profile     string
	noNetwork   bool
	enforcement string
	terse       bool
}

// cavemanInstruction is appended to the project instructions in caveman mode
// (--terse). The model must return only data, code, or commands.
const cavemanInstruction = "Caveman mode: return zero conversational filler. " +
	"Output only data, code, or terminal commands. Use telegraphic syntax."

// resolveSecurity builds the permission engine and sandbox holder from config
// plus command-line overrides.
//
// Precedence is config < profile < individual flags, so `--sandbox off` beats
// a profile that asked for read-only, and `--yolo` skips prompting except for
// the explicit dangerous-path blocklist floor.
func resolveSecurity(loaded *config.Loaded, o opts) (*permission.Engine, *sandbox.Holder, error) {
	cfg := loaded.Config
	// Permission policy: an explicit profile replaces the configured block.
	perm := cfg.Permission
	profileName := ""
	if o.profile != "" {
		resolved, err := config.ResolveProfileByName(cfg, o.profile)
		if err != nil {
			return nil, nil, err
		}
		perm, profileName = resolved, o.profile
	} else {
		perm = config.ResolvePermission(cfg, perm)
	}

	perms := permission.New(perm, loaded.ProjectRoot)
	perms.SetProfile(profileName)

	// Sandbox: combine the global block with whatever the permission set
	// carries. Merging (rather than replacing) matters because a profile's
	// sandbox is a *default* — it names a mode and little else — while the
	// top-level block is where a user puts deny_paths and resource limits.
	// Replacing would silently drop those.
	//
	// Which side wins depends on how the profile was chosen. Picking one
	// explicitly with --permission-profile is a deliberate act, so it
	// overrides the global block; inheriting one via "extends" is a default,
	// so the global block the user wrote by hand wins.
	sbCfg := cfg.Sandbox
	if perm != nil && perm.Sandbox != nil {
		if profileName != "" {
			sbCfg = config.MergeSandbox(cfg.Sandbox, perm.Sandbox)
		} else {
			sbCfg = config.MergeSandbox(perm.Sandbox, cfg.Sandbox)
		}
	}
	policy := sandbox.FromConfig(sbCfg, loaded.ProjectRoot)

	if o.sandbox != "" {
		mode, ok := sandbox.ParseMode(o.sandbox)
		if !ok {
			return nil, nil, fmt.Errorf("unknown sandbox mode %q (want read-only, workspace-write, trusted or off)", o.sandbox)
		}
		policy.Mode = mode
	}
	if o.noNetwork {
		policy.Network = false
	}
	if o.enforcement != "" {
		switch o.enforcement {
		case "auto", "os", "static":
			policy.Enforcement = sandbox.Enforcement(o.enforcement)
		default:
			return nil, nil, fmt.Errorf("unknown sandbox enforcement %q (want auto, os or static)", o.enforcement)
		}
	}
	// yolo turns off permission prompts, not the kernel fence or dangerous-path
	// blocklist: an unattended run cannot wreck the host or touch protected paths.
	policy = policy.Normalize(policy.Workspace)
	perms.SetSandboxRoot(policy.Workspace, policy.Mode == sandbox.ModeWorkspace)
	perms.SetProtectedPaths(policy.DenyPaths)
	perms.SetYolo(o.yolo)
	loaded.SandboxRoot = policy.Workspace

	return perms, sandbox.NewHolder(policy), nil
}

// buildDeps assembles everything the TUI needs.
func buildDeps(dir string, o opts) (tui.Deps, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return tui.Deps{}, err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return tui.Deps{}, fmt.Errorf("not a directory: %s", abs)
	}

	loaded, err := config.Load(abs)
	if err != nil {
		return tui.Deps{}, err
	}
	if o.terse {
		loaded.Config.Instructions = append(loaded.Config.Instructions, cavemanInstruction)
	}
	// Saved /auth credentials fill in any provider rick.json did not pin.
	creds, cerr := config.LoadCredentials()
	if cerr != nil {
		return tui.Deps{}, fmt.Errorf("load credentials: %w", cerr)
	}
	config.MergeCredentials(&loaded.Config, creds)
	if loaded.Config.Model == "" {
		for _, id := range creds.IDs() {
			if mdl := config.FirstConfiguredModel(creds, id); mdl != "" {
				loaded.Config.Model = mdl
				break
			}
		}
	}
	if o.model != "" {
		loaded.Config.Model = o.model
	}

	// Markdown-defined agents and commands from .rick/
	for name, a := range config.LoadMarkdownAgents(
		filepath.Join(config.GlobalDir(), "agents"),
		filepath.Join(loaded.ProjectRoot, ".rick", "agents"),
	) {
		if loaded.Config.Agents == nil {
			loaded.Config.Agents = map[string]config.Agent{}
		}
		if _, exists := loaded.Config.Agents[name]; !exists {
			loaded.Config.Agents[name] = a
		}
	}
	for name, c := range config.LoadMarkdownCommands(
		filepath.Join(config.GlobalDir(), "commands"),
		filepath.Join(loaded.ProjectRoot, ".rick", "commands"),
	) {
		if loaded.Config.Commands == nil {
			loaded.Config.Commands = map[string]config.Command{}
		}
		if _, exists := loaded.Config.Commands[name]; !exists {
			loaded.Config.Commands[name] = c
		}
	}

	themeDirs := []string{
		filepath.Join(config.GlobalDir(), "themes"),
		filepath.Join(loaded.ProjectRoot, ".rick", "themes"),
	}
	themes := theme.Load(themeDirs...)
	themeWatch := theme.NewWatcher(themeDirs...)

	perms, sandboxHolder, err := resolveSecurity(loaded, o)
	if err != nil {
		return tui.Deps{}, err
	}

	todos := tools.NewTodoStore()
	reg := tools.NewRegistry()
	// The shared session context manager: content-addressed dedup, cache
	// boundaries, live-zone compression. Knobs come from rick.json's
	// context_budget block; zero values keep the built-in defaults. The
	// min-cache-token guard defaults to a cheap bytes/4 estimate — exact BPE
	// counting on the hot path would double every turn's tokenizer work.
	ctxBudget := contextbudget.New(loaded.Config.ContextBudget.ContextBudgetOptions())
	if ws := loaded.Config.WebSearch; ws != nil && ws.CacheMaxLen > 0 {
		tools.SetCacheMaxLen(ws.CacheMaxLen)
	}
	depth := 1
	if loaded.Config.SubagentDepth != nil {
		depth = *loaded.Config.SubagentDepth
	}
	agentRegistry := agent.NewRegistry(depth, loaded.Config.MaxBackground)

	// Token usage tracker: persists cumulative usage per model per day
	// at ~/.config/rick/usage.json.
	usageTracker := usage.New(config.GlobalDir())
	reg.Register(tools.ReadTool{Delta: tools.DeltaStore(), EnableSkeleton: true})
	reg.Register(tools.WriteTool{})
	reg.Register(tools.EditTool{})
	reg.Register(tools.BashTool{Sandbox: sandboxHolder})
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
	reg.Register(tools.RetrieveUncompressedTool{Store: ctxBudget})

	goals, _ := goal.NewStore(filepath.Join(config.DataDir(), "goals"))
	if goals != nil {
		reg.Register(goal.GoalWriteTool{Store: goals})
		reg.Register(goal.GoalReadTool{Store: goals})
		reg.Register(goal.GoalStepTool{Store: goals})
		reg.Register(goal.GoalAbortTool{Store: goals})
	}

	mcpMgr := mcp.NewManager()

	plugins := plugin.NewRegistry()

	// Load plugins from global dir, project dir, and config URLs.
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

	// Load skills from global and project dirs.
	skills := plugin.LoadSkills(
		filepath.Join(config.GlobalDir(), "skills"),
		filepath.Join(loaded.ProjectRoot, ".rick", "skills"),
	)

	store, err := session.NewStore(filepath.Join(config.DataDir(), "sessions"))
	if err != nil {
		return tui.Deps{}, err
	}
	snaps, _ := session.NewSnapshotter(loaded.ProjectRoot, config.DataDir())

	provs := buildProviders(loaded.Config)

	resume := o.resume
	if resume == "" && o.cont {
		resume = store.GetCurrent(abs)
		if resume != "" {
			if _, err := store.Load(resume); err != nil {
				resume = ""
			}
		}
	}

	return tui.Deps{
		Loaded: loaded, Themes: themes, ThemeDirs: themeWatch, Registry: reg, Todos: todos,
		Perms: perms, Sandbox: sandboxHolder, Store: store, Snapshots: snaps, Providers: provs,
		MCP: mcpMgr, Plugins: plugins, Skills: skills, Agent: o.agent, Credentials: creds,
		Cwd: abs, Version: "v" + Version, ResumeID: resume, InitialMsg: o.prompt,
		SwarmManager: swarm.NewSwarmManager(),
		Goals:        goals, Usage: usageTracker, AgentRegistry: agentRegistry,
		Budget: ctxBudget,
	}, nil
}

// buildProviders instantiates every configured provider that has credentials.
func buildProviders(cfg config.Config) map[string]provider.Provider {
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
			out[name] = openai.New(name, p.APIKey, p.BaseURL)
		default:
			if p.BaseURL != "" {
				out[name] = openai.New(name, p.APIKey, p.BaseURL)
			}
		}
	}
	return out
}

func runTUI(dir string, o opts) error {
	deps, err := buildDeps(dir, o)
	if err != nil {
		return err
	}
	m := tui.New(deps)
	popts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithReportFocus()}
	p := tea.NewProgram(m, popts...)
	m.SetProgram(p)
	if len(deps.Loaded.Config.MCP) > 0 {
		deps.MCP.ConnectAsync(context.Background(), deps.Loaded.Config.MCP, deps.Registry, deps.Loaded.Config.Tools)
	}

	// Route todo changes back into the event loop. Always hand off to another
	// goroutine to avoid deadlocking Update.
	deps.Todos.OnChange = func(items []tools.TodoItem) {
		go p.Send(tui.TodosChanged(items))
	}

	_, err = p.Run()
	deps.MCP.Close()
	if deps.Usage != nil {
		if flushErr := deps.Usage.Flush(); err == nil {
			err = flushErr
		}
	}
	return err
}

// ---------- subcommands ----------

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("rick v%s\n", Version)
		},
	}
}

func sessionsCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "sessions",
		Short: "List and manage saved sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := session.NewStore(filepath.Join(config.DataDir(), "sessions"))
			if err != nil {
				return err
			}
			cwd, _ := os.Getwd()
			filter := cwd
			if all {
				filter = ""
			}
			metas, err := store.List(filter)
			if err != nil {
				return err
			}
			if len(metas) == 0 {
				fmt.Println("no sessions")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID	MESSAGES	UPDATED	TITLE")
			for _, m := range metas {
				fmt.Fprintf(w, "%s	%d	%s	%s\n",
					m.ID, m.Messages, m.Updated.Format("2006-01-02 15:04"), m.Title)
			}
			return w.Flush()
		},
	}
	c.Flags().BoolVar(&all, "all", false, "list sessions from every directory")

	c.AddCommand(&cobra.Command{
		Use:   "search <query>",
		Short: "Search sessions by title or message text",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := session.NewStore(filepath.Join(config.DataDir(), "sessions"))
			if err != nil {
				return err
			}
			query := strings.Join(args, " ")
			metas, err := store.Search(query)
			if err != nil {
				return err
			}
			if len(metas) == 0 {
				fmt.Println("no matching sessions")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID	MESSAGES	UPDATED	TITLE")
			for _, m := range metas {
				fmt.Fprintf(w, "%s	%d	%s	%s\n",
					m.ID, m.Messages, m.Updated.Format("2006-01-02 15:04"), m.Title)
			}
			return w.Flush()
		},
	})

	c.AddCommand(&cobra.Command{
		Use:   "fork <id>",
		Short: "Fork a session into a new copy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := session.NewStore(filepath.Join(config.DataDir(), "sessions"))
			if err != nil {
				return err
			}
			fork, err := store.Fork(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("forked %s → %s (%q)\n", args[0], fork.ID, fork.Title)
			return nil
		},
	})

	c.AddCommand(&cobra.Command{
		Use:   "rename <id> <title>",
		Short: "Rename a session",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := session.NewStore(filepath.Join(config.DataDir(), "sessions"))
			if err != nil {
				return err
			}
			title := strings.Join(args[1:], " ")
			if err := store.Rename(args[0], title); err != nil {
				return err
			}
			fmt.Printf("renamed %s → %q\n", args[0], title)
			return nil
		},
	})

	c.AddCommand(
		sessionsImportCmd(),
	)

	return c
}

func resumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Browse and resume saved sessions interactively",
		RunE: func(cmd *cobra.Command, args []string) error {
			styles := tui.NewStyles(nil)
			id, err := tui.ResumeSessions(styles)
			if err != nil {
				return err
			}
			if id == "" {
				return nil
			}
			// Resume the selected session in the TUI
			return runTUI(".", opts{resume: id})
		},
	}
}

func configCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Show the resolved configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			loaded, err := config.Load(cwd)
			if err != nil {
				return err
			}
			fmt.Printf("project root:  %s\n", loaded.ProjectRoot)
			fmt.Printf("global dir:    %s\n", config.GlobalDir())
			fmt.Printf("data dir:      %s\n", config.DataDir())
			fmt.Printf("model:         %s\n", loaded.Config.Model)
			fmt.Printf("small model:   %s\n", loaded.Config.SmallModel)
			fmt.Printf("theme:         %s\n", loaded.TUI.Theme)
			if len(loaded.Sources) == 0 {
				fmt.Println("sources:       (built-in defaults only)")
			} else {
				fmt.Printf("sources:       %s\n", strings.Join(loaded.Sources, "\n               "))
			}
			fmt.Println("providers:")
			for name, p := range loaded.Config.Providers {
				state := "no credentials"
				if p.APIKey != "" {
					state = "key configured"
				}
				fmt.Printf("  %-12s %s\n", name, state)
			}
			fmt.Printf("ripgrep:       %v\n", tools.HasRipgrep())

			perm := config.ResolvePermission(loaded.Config, loaded.Config.Permission)
			fmt.Printf("\npermission profiles: %s\n", strings.Join(config.ProfileNames(loaded.Config), ", "))
			fmt.Printf("active policy: default=%s edit=%s write=%s read=%s\n",
				orEmpty(perm.Default, "ask"), orEmpty(perm.Edit, "-"),
				orEmpty(perm.Write, "-"), orEmpty(perm.Read, "-"))
			fmt.Printf("  rules: %d bash · %d path · %d host · %d tool\n",
				len(perm.Bash), len(perm.Paths), len(perm.Hosts), len(perm.Tools))

			sbCfg := loaded.Config.Sandbox
			if perm.Sandbox != nil {
				sbCfg = config.MergeSandbox(perm.Sandbox, loaded.Config.Sandbox)
			}
			policy := sandbox.FromConfig(sbCfg, loaded.ProjectRoot)
			fmt.Printf("\nsandbox:\n%s\n", indent(policy.Detail(sandbox.BackendName(policy)), "  "))
			return nil
		},
	}
}

func orEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func modelsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "models",
		Short: "List available models",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			loaded, err := config.Load(cwd)
			if err != nil {
				return err
			}
			provs := buildProviders(loaded.Config)
			if len(provs) == 0 {
				return fmt.Errorf("no providers configured (set ANTHROPIC_API_KEY)")
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "MODEL\tCONTEXT\tNAME")
			for name, p := range provs {
				for _, mi := range provider.FilterChatModels(p.Models()) {
					fmt.Fprintf(w, "%s/%s	%dk	%s\n", name, mi.ID, mi.ContextWindow/1000, mi.Name)
				}
			}
			return w.Flush()
		},
	}
}

func runCmd() *cobra.Command {
	var model string
	c := &cobra.Command{
		Use:   "run [prompt]",
		Short: "Run a single prompt in the TUI",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTUI(".", opts{prompt: strings.Join(args, " "), model: model, fresh: true})
		},
	}
	c.Flags().StringVarP(&model, "model", "m", "", "model to use")
	return c
}

func execCmd() *cobra.Command {
	var (
		flagExecModel   string
		flagExecYolo    bool
		flagExecTerse   bool
		flagExecSandbox string
		flagExecFormat  string
		flagExecTurns   int
		flagExecProfile string
		flagExecPrompt  string
	)
	c := &cobra.Command{
		Use:   "exec [prompt]",
		Short: "Run the agent non-interactively (headless/CI mode)",
		Long: "Execute a prompt without the TUI. Output goes to stdout; tool activity\ngoes to stderr. Suitable for CI pipelines and scripting.\n\n" +
			"Examples:\n" +
			"  rick exec \"explain this codebase\"\n" +
			"  rick exec --yolo -o json \"fix the failing tests\"\n" +
			"  echo \"summarise\" | rick exec --prompt -",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			prompt := flagExecPrompt
			if len(args) > 0 {
				prompt = strings.Join(args, " ")
			}
			if prompt == "" {
				return fmt.Errorf("no prompt provided (pass as argument or use --prompt)")
			}

			format := headless.OutputFormat(flagExecFormat)
			switch format {
			case headless.FormatText, headless.FormatJSON, headless.FormatStreamJSON:
			default:
				return fmt.Errorf("unknown output format %q (want text, json or stream-json)", flagExecFormat)
			}

			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			abs, err := filepath.Abs(cwd)
			if err != nil {
				return err
			}

			loaded, err := config.Load(abs)
			if err != nil {
				return err
			}
			if flagExecTerse {
				loaded.Config.Instructions = append(loaded.Config.Instructions, cavemanInstruction)
			}
			// Merge saved credentials.
			creds, cerr := config.LoadCredentials()
			if cerr == nil {
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
			if flagExecModel != "" {
				loaded.Config.Model = flagExecModel
			}

			// Resolve provider.
			provID, modelID := config.SplitModel(loaded.Config.Model)
			provs := buildProviders(loaded.Config)
			prov, ok := provs[provID]
			if !ok {
				// Try later slash positions for multi-segment model ids.
				idx := strings.Index(loaded.Config.Model, "/")
				for idx >= 0 && idx < len(loaded.Config.Model)-1 {
					if p, found := provs[loaded.Config.Model[:idx]]; found {
						prov = p
						modelID = loaded.Config.Model[idx+1:]
						ok = true
						break
					}
					next := strings.Index(loaded.Config.Model[idx+1:], "/")
					if next < 0 {
						break
					}
					idx = idx + 1 + next
				}
			}
			if !ok {
				return fmt.Errorf("no provider configured for model %q", loaded.Config.Model)
			}

			// Build security (permission + sandbox).
			o := opts{
				yolo:    flagExecYolo,
				sandbox: flagExecSandbox,
				profile: flagExecProfile,
			}
			perms, sandboxHolder, err := resolveSecurity(loaded, o)
			if err != nil {
				return err
			}

			// Build tools.
			todos := tools.NewTodoStore()
			reg := tools.NewRegistry()
			reg.Register(tools.ReadTool{Delta: tools.DeltaStore(), EnableSkeleton: true})
			reg.Register(tools.WriteTool{})
			reg.Register(tools.EditTool{})
			reg.Register(tools.BashTool{Sandbox: sandboxHolder})
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

			plugins := plugin.NewRegistry()

			store, err := session.NewStore(filepath.Join(config.DataDir(), "sessions"))
			if err != nil {
				return err
			}

			deps := headless.Deps{
				Provider: prov,
				ModelID:  modelID,
				Config:   loaded.Config,
				Tools:    reg,
				Perms:    perms,
				Plugins:  plugins,
				Store:    store,
			}
			hopts := headless.Options{
				Prompt:      prompt,
				Model:       loaded.Config.Model,
				Yolo:        flagExecYolo,
				MaxTurns:    flagExecTurns,
				Format:      format,
				Cwd:         abs,
				ProjectRoot: loaded.ProjectRoot,
				AgentName:   "build",
			}

			return headless.Run(cmd.Context(), hopts, deps, os.Stdout, os.Stderr)
		},
	}
	c.Flags().StringVarP(&flagExecModel, "model", "m", "", "model to use (provider/model-id)")
	c.Flags().BoolVar(&flagExecYolo, "yolo", false, "auto-approve all permissions (dangerous)")
	c.Flags().BoolVar(&flagExecTerse, "terse", false,
		"caveman mode: instruct the model to return zero conversational filler")
	c.Flags().StringVar(&flagExecSandbox, "sandbox", "",
		"command sandbox: read-only | workspace-write | trusted | off")
	c.Flags().StringVarP(&flagExecFormat, "output-format", "o", "text",
		"output format: text | json | stream-json")
	c.Flags().IntVar(&flagExecTurns, "max-turns", 0, "maximum agent turns (0 = unlimited)")
	c.Flags().StringVar(&flagExecProfile, "permission-profile", "",
		"permission profile: readonly | standard | trusted | ci")
	c.Flags().StringVarP(&flagExecPrompt, "prompt", "p", "", "prompt text (alternative to positional arg)")
	return c
}

// ---------- session export / import ----------

func sessionExportCmd() *cobra.Command {
	var output string
	var pretty bool
	c := &cobra.Command{
		Use:   "session export <id>",
		Short: "Export a session to JSON",
		Long: "Write a single session's full JSON (messages, snapshots, metadata) to a file.\n" +
			"Default: compact JSON in <session-id>.json; use --pretty for human-readable output.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := session.NewStore(filepath.Join(config.DataDir(), "sessions"))
			if err != nil {
				return err
			}
			sess, err := store.Load(args[0])
			if err != nil {
				return err
			}
			path := output
			if path == "" {
				path = sess.ID + ".json"
			}
			f, err := os.Create(path)
			if err != nil {
				return err
			}
			defer f.Close()
			var exportErr error
			if pretty {
				exportErr = session.ExportPretty(sess, f)
			} else {
				exportErr = session.Export(sess, f)
			}
			if exportErr != nil {
				return exportErr
			}
			fmt.Printf("exported %s → %s (%d messages)\n", sess.ID, path, len(sess.Messages))
			return nil
		},
	}
	c.Flags().StringVarP(&output, "output", "o", "", "output file path (default: <session-id>.json)")
	c.Flags().BoolVar(&pretty, "pretty", false, "indent JSON for human readability")
	return c
}

func sessionsImportCmd() *cobra.Command {
	var source string
	c := &cobra.Command{
		Use:   "import <file>",
		Short: "Import a session from JSON (rick, opencode, kilo, codex)",
		Long: "Read a session JSON file and load it into the local store. The source\n" +
			"format is auto-detected by default; use --source to force a specific parser.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer f.Close()
			sess, err := session.Import(f, session.SessionSource(source))
			if err != nil {
				return err
			}
			store, err := session.NewStore(filepath.Join(config.DataDir(), "sessions"))
			if err != nil {
				return err
			}
			if err := store.Save(sess); err != nil {
				return err
			}
			fmt.Printf("imported %s (%d messages, source: %s)\n", sess.ID, len(sess.Messages), sourceLabel(source))
			return nil
		},
	}
	c.Flags().StringVar(&source, "source", "auto", "session source format: auto | opencode | kilo | codex")
	return c
}

// sourceLabel returns a human-readable label for the import source.
func sourceLabel(s string) string {
	if s == "" || s == "auto" {
		return "auto-detect"
	}
	return s
}

// ---------- serve daemon ----------

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "serve",
		Short:              "Run the headless ndjson daemon (see rickserve)",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("use the rickserve command directly (run `rickserve --help`)")
		},
	}
}

// ---------- doctor ----------

func doctorCmd() *cobra.Command {
	var network bool
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Check the health of the rick installation and environment",
		Long: "rickdoctor runs a series of local diagnostics — toolchain, external\n" +
			"binaries, configuration, credentials, MCP servers, data directory,\n" +
			"themes and plugins — and prints a status table.\n\n" +
			"Network probes are skipped unless --network is passed.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			doctor.CheckNetwork = network
			checks := doctor.RunChecks()
			doctor.PrintReport(checks)
			os.Exit(doctor.ExitCode(checks))
			return nil
		},
	}
	c.Flags().BoolVar(&network, "network", false,
		"probe provider endpoints for connectivity (makes network calls)")
	return c
}

// ---------- security ----------

func securityCmd() *cobra.Command {
	var (
		flagDir    string
		flagForce  bool
		flagFormat string
	)
	c := &cobra.Command{
		Use:   "security",
		Short: "Audit dependencies for known vulnerabilities",
		Long: "Scan the project's dependency manifests (go.mod, package.json, Cargo.toml,\n" +
			"requirements.txt, pyproject.toml) and query OSV.dev for known\n" +
			"supply-chain vulnerabilities in each dependency.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagFormat != "table" && flagFormat != "json" {
				return fmt.Errorf("unknown format %q (want table or json)", flagFormat)
			}
			if flagForce {
				cachePath := filepath.Join(flagDir, ".rick", "security-cache.json")
				_ = os.Remove(cachePath)
			}
			findings, err := security.Audit(flagDir)
			if err != nil {
				return err
			}
			if flagFormat == "json" {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(findings)
			}
			renderSecurityTable(findings)
			renderSecuritySummary(findings)
			if len(findings) > 0 {
				os.Exit(1)
			}
			return nil
		},
	}
	c.Flags().StringVar(&flagDir, "dir", ".", "project directory to audit")
	c.Flags().BoolVar(&flagForce, "force", false, "skip the cache and re-query OSV.dev")
	c.Flags().StringVar(&flagFormat, "format", "table", "output format: table | json")
	return c
}

// ---------- apply ----------

func applyCmd() *cobra.Command {
	var (
		flagDryRun    bool
		flagSessionID string
		flagLast      bool
	)
	c := &cobra.Command{
		Use:   "apply",
		Short: "Apply the latest agent diff with git apply",
		Long: "rickapply finds the most recent unified diff in a rick session — from an\n" +
			"apply_patch tool call or any tool output containing patch text — and\n" +
			"applies it to the working tree via git apply.\n\n" +
			"Examples:\n" +
			"  rick apply\n" +
			"  rick apply --dry-run\n" +
			"  rick apply --session 01j2k3",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = flagLast
			return apply.Run(".", apply.Options{SessionID: flagSessionID, DryRun: flagDryRun})
		},
	}
	c.Flags().BoolVarP(&flagDryRun, "dry-run", "n", false, "only run git apply --check")
	c.Flags().StringVar(&flagSessionID, "session", "", "apply the diff from a specific session id")
	c.Flags().BoolVar(&flagLast, "last", true, "apply the most recent diff (default)")
	return c
}

// renderSecurityTable prints the vulnerability table.
func renderSecurityTable(findings []security.Finding) {
	if len(findings) == 0 {
		fmt.Println("no vulnerabilities found")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PACKAGE	VERSION	SEVERITY	CVE	OSV ID	URL")
	for _, f := range findings {
		fmt.Fprintf(w, "%s	%s	%s	%s	%s	%s\n",
			f.Package, f.Version, f.Severity, f.CVE, f.OSVID, f.URL)
	}
	w.Flush()
}

// renderSecuritySummary prints the vulnerability summary.
func renderSecuritySummary(findings []security.Finding) {
	counts := map[string]int{}
	for _, f := range findings {
		sev := f.Severity
		if sev == "" {
			sev = "unknown"
		}
		counts[sev]++
	}
	fmt.Printf("\n%d vulnerabilities found", len(findings))
	parts := []string{}
	for _, sev := range []string{"critical", "high", "moderate", "low", "unknown"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, sev))
		}
	}
	if len(parts) > 0 {
		fmt.Printf(" (%s)", strings.Join(parts, ", "))
	}
	fmt.Println()
}
