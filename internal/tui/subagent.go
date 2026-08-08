package tui

import (
	"context"
	"fmt"
	"strings"

	"rick/internal/agent"
	"rick/internal/config"
	"rick/internal/plugin"
	"rick/internal/provider"
)

// registerTaskTool wires the subagent spawner into the tool registry.
//
// The spawn closure reuses the same registry, permission engine and provider
// set as the primary agent, but with a restricted tool filter, a tightened
// permission policy for read-only subagents, and an incremented depth so
// recursion is capped.
func (m *Model) registerTaskTool() {
	specs := agent.BuiltinSubagents()

	// Config-defined subagents override / extend the built-ins.
	for name, a := range m.deps.Loaded.Config.Agents {
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

	maxDepth := 1
	if d := m.deps.Loaded.Config.SubagentDepth; d != nil && *d > 0 {
		maxDepth = *d
	}

	m.deps.Registry.Register(agent.TaskTool{
		Specs:           specs,
		MaxDepth:        maxDepth,
		Spawn:           m.spawnSubagent(specs, maxDepth),
		SpawnBackground: m.spawnSubagentBackground(specs, maxDepth),
	})
	m.deps.Registry.Register(agent.ParallelTaskTool{
		Specs:           specs,
		MaxDepth:        maxDepth,
		Spawn:           m.spawnSubagent(specs, maxDepth),
		SpawnBackground: m.spawnSubagentBackground(specs, maxDepth),
	})
	if m.deps.AgentRegistry != nil {
		m.deps.Registry.Register(agent.ChatTool{Registry: m.deps.AgentRegistry})
		m.deps.Registry.Register(agent.SteerTool{Registry: m.deps.AgentRegistry})
		m.deps.Registry.Register(agent.ReportTool{Registry: m.deps.AgentRegistry})
	}

	// Register the swarm tool if a swarm manager is available.
	if m.deps.SwarmManager != nil {
		m.deps.Registry.Register(agent.SwarmTool{
			Manager: m.spawnSwarm,
		})
	}
}

func (m *Model) spawnSubagent(specs map[string]agent.SubagentSpec, maxDepth int) func(context.Context, string, string, string, int) (string, error) {
	return func(ctx context.Context, kind, description, prompt string, depth int) (string, error) {
		spec, ok := specs[kind]
		if !ok {
			return "", fmt.Errorf("unknown subagent type %q", kind)
		}

		modelRef := m.modelID
		if spec.Model != "" {
			modelRef = spec.Model
		}
		provID, modelID := config.SplitModel(modelRef)
		prov, ok := m.deps.Providers[provID]
		if !ok {
			return "", fmt.Errorf("subagent: unknown provider %q", provID)
		}

		perms := agent.SubagentPermissions(spec, m.deps.Perms, m.deps.Loaded.ProjectRoot)

		stableSys := spec.Prompt + agent.ProjectContext(m.deps.Loaded.ProjectRoot, m.deps.Loaded.Config.Instructions)
		sys := stableSys + agent.Environment(m.deps.Cwd, modelID, kind, "")

		// Report progress into the parent transcript.
		if p := m.program; p != nil {
			p.Send(subagentEventMsg{kind: kind, description: description, phase: "start"})
		}

		// Lifecycle hook: subagent start.
		if m.deps.Plugins != nil && m.deps.Plugins.Len() > 0 {
			pluginErrs := m.deps.Plugins.DispatchSubagentStart(ctx, &plugin.SubagentStartEvent{
				SessionID: m.sessionID(), Agent: m.agentName,
				SubagentName: kind, Task: description,
			})
			for _, pluginErr := range pluginErrs {
				if p := m.program; p != nil {
					p.Send(subagentEventMsg{kind: kind, description: description, phase: "error", detail: pluginErr.Error()})
				}
			}
		}

		// /yolo is an explicit request to give child runs the same effective
		// tool access as Rick. Keep the explore prompt's guidance intact, but do
		// not hide write/shell tools behind its read-only filter in that mode.
		toolSpec := spec
		if m.deps.Perms != nil && m.deps.Perms.Yolo() {
			toolSpec.ReadOnly = false
		}

		cfg := agent.Config{
			Provider:           prov,
			Model:              modelID,
			System:             sys,
			SystemStable:       stableSys,
			MaxTokens:          m.deps.Loaded.Config.MaxTokens,
			Tools:              m.deps.Registry,
			ToolFilter:         agent.SubagentToolFilter(toolSpec, m.toolFilter()),
			Perms:              perms,
			Ask:                m.makeAsker(),
			Cwd:                m.deps.Cwd,
			SessionID:          m.sessionID(),
			AgentName:          kind,
			Depth:              depth,
			MaxTurns:           0, // unlimited; the repeated-call guard still stops loops
			Plugins:            m.deps.Plugins,
			Parallel:           true,
			CacheRetention:     provider.CacheRetention(m.deps.Loaded.Config.CacheRetention),
			WarmCache:          m.deps.Loaded.Config.WarmCache,
			MaxReasoningTurns:  m.deps.Loaded.Config.CacheMaxReasoningTurns,
			MaxToolResultBytes: m.deps.Loaded.Config.CacheMaxToolResultBytes,
			ArchiveDir:         agentArchiveDir(m.deps.Store),
		}

		toolCount := 0
		out, err := agent.RunSubagent(ctx, cfg, prompt, func(ev agent.Event) {
			if ev.Kind == agent.EvUsage && ev.Usage != nil {
				m.recordChildUsage(modelRef, *ev.Usage)
			}
			if ev.Kind == agent.EvToolEnd {
				toolCount++
				if p := m.program; p != nil && ev.Tool != nil {
					p.Send(subagentEventMsg{
						kind: kind, description: description, phase: "tool",
						detail: ev.Tool.Name + " " + ev.Tool.Title, count: toolCount,
					})
				}
			}
		})

		if p := m.program; p != nil {
			p.Send(subagentEventMsg{
				kind: kind, description: description, phase: "done", count: toolCount,
			})
		}
		return out, err
	}
}

// spawnSubagentBackground starts the same runner used by foreground delegation,
// but registers the child first and returns its ID without waiting for completion.
func (m *Model) spawnSubagentBackground(specs map[string]agent.SubagentSpec, maxDepth int) func(context.Context, string, string, string, string, int) (string, error) {
	return func(ctx context.Context, parentID, kind, description, prompt string, depth int) (string, error) {
		if depth > maxDepth || depth > agent.MaxAllowedDepth {
			return "", fmt.Errorf("subagent depth %d exceeds configured limit %d", depth, maxDepth)
		}
		spec, ok := specs[kind]
		if !ok {
			return "", fmt.Errorf("unknown subagent type %q", kind)
		}
		if m.deps.AgentRegistry == nil {
			return "", fmt.Errorf("agent registry is unavailable")
		}
		if err := m.deps.AgentRegistry.AcquireBackground(); err != nil {
			return "", err
		}
		parentID = strings.TrimSpace(parentID)
		if parentID == "" {
			parentID = m.agentID
		}
		parent, ok := m.deps.AgentRegistry.Get(parentID)
		if !ok {
			m.deps.AgentRegistry.ReleaseBackground()
			return "", fmt.Errorf("parent agent %q is not registered", parentID)
		}
		if depth != parent.Depth+1 {
			m.deps.AgentRegistry.ReleaseBackground()
			return "", fmt.Errorf("subagent depth must be parent depth + 1")
		}
		modelRef := m.modelID
		if spec.Model != "" {
			modelRef = spec.Model
		}
		provID, modelID := config.SplitModel(modelRef)
		prov, ok := m.deps.Providers[provID]
		if !ok {
			m.deps.AgentRegistry.ReleaseBackground()
			return "", fmt.Errorf("subagent: unknown provider %q", provID)
		}
		childCtx, cancel := context.WithCancel(ctx)
		id, err := m.deps.AgentRegistry.Register(&agent.AgentEntry{
			Name: kind, ParentID: parentID, Depth: depth, Status: agent.AgentIdle,
			Description: description, Cancel: cancel,
		})
		if err != nil {
			cancel()
			m.deps.AgentRegistry.ReleaseBackground()
			return "", err
		}
		perms := agent.SubagentPermissions(spec, m.deps.Perms, m.deps.Loaded.ProjectRoot)
		stableSys := spec.Prompt + agent.ProjectContext(m.deps.Loaded.ProjectRoot, m.deps.Loaded.Config.Instructions)
		sys := stableSys + agent.Environment(m.deps.Cwd, modelID, kind, "")
		toolSpec := spec
		if m.deps.Perms != nil && m.deps.Perms.Yolo() {
			toolSpec.ReadOnly = false
		}
		cfg := agent.Config{
			Provider: prov, Model: modelID, System: sys, SystemStable: stableSys,
			MaxTokens: m.deps.Loaded.Config.MaxTokens, Tools: m.deps.Registry,
			ToolFilter: agent.SubagentToolFilter(toolSpec, m.toolFilter()), Perms: perms,
			Ask: m.makeAsker(), Cwd: m.deps.Cwd, SessionID: m.sessionID(),
			AgentName: kind, AgentID: id, Depth: depth, MaxTurns: 0, // unlimited; the repeated-call guard still stops loops
			Plugins: m.deps.Plugins, Parallel: true, Registry: m.deps.AgentRegistry,
			CacheRetention:     provider.CacheRetention(m.deps.Loaded.Config.CacheRetention),
			WarmCache:          m.deps.Loaded.Config.WarmCache,
			MaxReasoningTurns:  m.deps.Loaded.Config.CacheMaxReasoningTurns,
			MaxToolResultBytes: m.deps.Loaded.Config.CacheMaxToolResultBytes,
			ArchiveDir:         agentArchiveDir(m.deps.Store),
		}
		if p := m.program; p != nil {
			p.Send(subagentEventMsg{kind: kind, description: description, phase: "start"})
		}
		m.deps.AgentRegistry.Publish(parentID, agent.Event{Kind: agent.EvAgentBackground, Text: fmt.Sprintf("background agent %s started: %s", id, description)})
		go func() {
			defer m.deps.AgentRegistry.ReleaseBackground()
			toolCount := 0
			out, runErr := agent.RunSubagent(childCtx, cfg, prompt, func(ev agent.Event) {
				if ev.Kind == agent.EvUsage && ev.Usage != nil {
					m.recordChildUsage(modelRef, *ev.Usage)
				}
				if ev.Kind == agent.EvToolEnd {
					toolCount++
					if p := m.program; p != nil && ev.Tool != nil {
						p.Send(subagentEventMsg{kind: kind, description: description, phase: "tool", detail: ev.Tool.Name + " " + ev.Tool.Title, count: toolCount})
					}
				}
			})
			m.deps.AgentRegistry.Update(id, agent.AgentDone, out, runErr)
			if p := m.program; p != nil {
				p.Send(subagentResultMsg{id: id, kind: kind, description: description, output: out, err: runErr})
				p.Send(subagentEventMsg{kind: kind, description: description, phase: "done", count: toolCount})
			}
		}()
		return id, nil
	}
}

// subagentResultMsg notifies the orchestrator that a background child produced a result.
type subagentResultMsg struct {
	id          string
	kind        string
	description string
	output      string
	err         error
}

type childUsageMsg struct {
	usage provider.Usage
}

// subagentEventMsg reports child-session progress to the parent UI.
type subagentEventMsg struct {
	kind        string
	description string
	phase       string // start | tool | done
	detail      string
	count       int
}

func (m *Model) applySubagentResult(msg subagentResultMsg) {
	label := msg.description
	if label == "" {
		label = msg.kind
	}
	if msg.err != nil {
		m.setStatus(fmt.Sprintf("background agent %s failed: %s", label, truncate(msg.err.Error(), 80)))
		return
	}
	m.setStatus(fmt.Sprintf("background agent %s completed", label))
}

// applySubagentEvent renders child progress into the transcript.
func (m *Model) applySubagentEvent(msg subagentEventMsg) {
	label := msg.description
	if label == "" {
		label = msg.kind
	}
	switch msg.phase {
	case "start":
		m.childActive = append(m.childActive, label)
		m.setStatus(fmt.Sprintf("subagent %s: %s", msg.kind, label))
	case "tool":
		m.setStatus(fmt.Sprintf("subagent %s · %d tools · %s",
			msg.kind, msg.count, truncate(msg.detail, 40)))
	case "done":
		for i, c := range m.childActive {
			if c == label {
				m.childActive = append(m.childActive[:i], m.childActive[i+1:]...)
				break
			}
		}
		m.setStatus(fmt.Sprintf("subagent %s finished (%d tools)", msg.kind, msg.count))
	}
}

// mcpStatus summarises MCP connectivity for /mcp.
func (m *Model) mcpStatus() string {
	if m.deps.MCP == nil {
		return "MCP is not initialised"
	}
	names := m.deps.MCP.ServerNames()
	errs := m.deps.MCP.Errors()
	if len(names) == 0 && len(errs) == 0 {
		return "no MCP servers configured\n\nAdd one to rick.json:\n" +
			`  "mcp": { "myserver": { "type": "local", "command": ["npx","-y","@some/mcp-server"] } }`
	}
	var b strings.Builder
	for _, n := range names {
		count := 0
		for _, t := range m.deps.Registry.Names() {
			if strings.HasPrefix(t, n+"_") {
				count++
			}
		}
		fmt.Fprintf(&b, "● %s — connected, %d tool(s)\n", n, count)
	}
	for n, err := range errs {
		fmt.Fprintf(&b, "✗ %s — %v\n", n, err)
	}
	return strings.TrimRight(b.String(), "\n")
}

// pluginStatus summarises loaded plugins.
func (m *Model) pluginStatus() string {
	if m.deps.Plugins == nil || m.deps.Plugins.Len() == 0 {
		return "no plugins loaded"
	}
	return fmt.Sprintf("%d plugin(s): %s",
		m.deps.Plugins.Len(), strings.Join(m.deps.Plugins.Names(), ", "))
}

// expandAgentMentions rewrites a leading @subagent mention into a task
// delegation instruction so the model calls the task tool.
func (m *Model) expandAgentMentions(text string) (string, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "@") {
		return text, false
	}
	name := strings.TrimPrefix(fields[0], "@")
	specs := agent.BuiltinSubagents()
	if _, ok := specs[name]; !ok {
		if a, ok2 := m.deps.Loaded.Config.Agents[name]; !ok2 || (a.Mode != "subagent" && a.Mode != "all") {
			return text, false
		}
	}
	rest := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	if rest == "" {
		return text, false
	}
	return fmt.Sprintf(
		"Use the task tool with subagent_type=%q to handle this, then report the result:\n\n%s",
		name, rest), true
}
