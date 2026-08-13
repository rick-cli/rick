package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"rick/internal/agent"
	"rick/internal/agentnames"
	"rick/internal/goal"
	"rick/internal/plugin"
	"rick/internal/provider"
	"rick/internal/swarm"
	"rick/internal/tools"
)

// AgentView is model-owned live teammate state. Background workers communicate
// exclusively with tea messages and never mutate it.
type AgentView struct {
	Name          string
	Role          string
	Color         string
	Status        swarm.AgentStatus
	Started       time.Time
	Finished      time.Time
	CurrentAction string
	ActionStart   time.Time
	TokensIn      int
	TokensOut     int
	CacheRead     int
	CacheWrite    int
	ToolsUsed     int
	LastTool      string
	Result        string
	Error         string
}

type SwarmView struct {
	SwarmID  string
	Name     string
	Goal     string
	Agents   map[string]*AgentView
	AgentOrd []string
	Tasks    *swarm.TaskBoard
	Started  time.Time
	Finished time.Time
	Active   bool
	MsgIndex int
	complete chan swarmStartReply
}

type swarmStartMsg struct {
	ctx      context.Context
	name     string
	goal     string
	agents   []agent.SwarmAgentSpec
	topology swarm.Topology
	reply    chan swarmStartReply
	complete chan swarmStartReply
}

type swarmStartReply struct {
	text string
	err  error
}

type swarmWorkerMsg struct {
	swarmID string
	name    string
	event   agent.Event
	kind    swarm.EventType
	err     error
	output  string
	at      time.Time
}

type swarmCompleteMsg struct {
	swarmID string
	results []swarm.TeamResult
}

type swarmRunPlan struct {
	ctx         context.Context
	swarmID     string
	modelID     string
	jobs        []swarm.TeamJob
	tasks       *swarm.TaskBoard
	concurrency int
}

func inheritSwarmWorkerRuntime(cfg agent.Config, snapshotter agent.Snapshotter, plugins *plugin.Registry, goals *goal.Store) agent.Config {
	cfg.Snapshotter = snapshotter
	cfg.Plugins = plugins
	cfg.Goals = goals
	return cfg
}

func (m *Model) spawnSwarm(ctx context.Context, name, goal string, agents []agent.SwarmAgentSpec, topo swarm.Topology) (string, error) {
	if m.program == nil {
		return "", fmt.Errorf("swarm program is unavailable")
	}
	reply := make(chan swarmStartReply, 1)
	complete := make(chan swarmStartReply, 1)
	m.program.Send(swarmStartMsg{ctx: ctx, name: name, goal: goal, agents: agents, topology: topo, reply: reply, complete: complete})
	select {
	case result := <-reply:
		if result.err != nil {
			return result.text, result.err
		}
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case result := <-complete:
		return result.text, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// beginSwarm runs only in Model.Update. It creates the single transcript card,
// captures all worker dependencies, and returns a background execution plan.
func (m *Model) beginSwarm(msg swarmStartMsg) (*swarmRunPlan, error) {
	if m.deps.SwarmManager == nil {
		return nil, fmt.Errorf("swarm manager not initialized")
	}
	if len(msg.agents) < 2 {
		return nil, fmt.Errorf("at least 2 agents are required")
	}
	prov, modelID, err := m.resolveProvider()
	if err != nil {
		return nil, err
	}
	cacheRetention, _, _, cacheWarm := m.deps.Loaded.Config.CacheForProvider(prov.Name())
	for i := range msg.agents {
		msg.agents[i].Name = strings.TrimSpace(msg.agents[i].Name)
		msg.agents[i].Role = strings.TrimSpace(msg.agents[i].Role)
		msg.agents[i].TaskID = strings.TrimSpace(msg.agents[i].TaskID)
		msg.agents[i].Subject = strings.TrimSpace(msg.agents[i].Subject)
		for dependencyIndex := range msg.agents[i].DependsOn {
			msg.agents[i].DependsOn[dependencyIndex] = strings.TrimSpace(msg.agents[i].DependsOn[dependencyIndex])
		}
	}
	id := fmt.Sprintf("swarm-%d", time.Now().UnixNano())
	s := swarm.NewSwarmContext(msg.ctx, id, msg.name, msg.goal, msg.topology)
	committed := false
	defer func() {
		if !committed {
			s.Cancel()
		}
	}()
	view := &SwarmView{SwarmID: id, Name: msg.name, Goal: msg.goal, Agents: map[string]*AgentView{}, Tasks: s.Tasks, Started: time.Now(), Active: true, complete: msg.complete}
	seen := map[string]bool{}
	for i, spec := range msg.agents {
		if spec.Name == "" || spec.Role == "" || seen[spec.Name] {
			return nil, fmt.Errorf("agent names and roles must be non-empty and names unique: %q", spec.Name)
		}
		seen[spec.Name] = true
		s.AddAgent(spec.Name, spec.Role)
		character := agentnames.AssignAt(i)
		view.Agents[spec.Name] = &AgentView{Name: character.Name, Role: spec.Role, Color: character.Color, Status: swarm.StatusIdle, CurrentAction: "queued"}
		view.AgentOrd = append(view.AgentOrd, spec.Name)
	}
	if msg.topology == swarm.TopologyStar {
		s.Primary = msg.agents[0].Name
	}
	taskSpecs := make([]swarm.TaskSpec, 0, len(msg.agents))
	for _, spec := range msg.agents {
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
	if err := s.Tasks.AddBatch(taskSpecs); err != nil {
		return nil, err
	}

	m.teamViews[id] = view
	m.deps.SwarmManager.Add(s)
	view.MsgIndex = len(m.msgs)
	m.msgs = append(m.msgs, ChatMsg{Kind: MsgSwarm, SwarmID: id, Time: time.Now()})
	m.tx.noteAppend()
	m.activeSwarms++
	m.refresh()

	jobs := make([]swarm.TeamJob, 0, len(msg.agents))
	toolFilter := m.toolFilter()
	var snapshotter agent.Snapshotter
	if m.deps.Snapshots != nil && m.deps.Snapshots.Enabled() {
		snapshotter = m.deps.Snapshots
	}
	for _, spec := range msg.agents {
		var allowedTools []string
		if strings.TrimSpace(spec.Tools) != "" {
			allowedTools = strings.FieldsFunc(spec.Tools, func(r rune) bool {
				return r == ',' || r == ';' || r == ' ' || r == '	' || r == '\n'
			})
		}
		workerTools := tools.NewFilteredSwarmRegistry(m.deps.Registry, toolFilter, allowedTools...)
		workerTools.Register(agent.TeamTool{Swarm: s})
		system := "You are an independent teammate reporting to the lead agent. Use the team tool to confirm your assigned task, inspect messages, share only useful findings, and complete or fail the task explicitly. Do not delegate or spawn agents. Return ONLY factual findings—no narration, no 'I'll research', and no 'Let me dig deeper'. Output clean, complete results with sources when applicable."
		cfg := agent.Config{
			Provider: prov, Model: modelID, System: system,
			MaxTokens: m.deps.Loaded.Config.MaxTokens, Tools: workerTools,
			ToolFilter: toolFilter, Perms: m.deps.Perms, Ask: m.makeAsker(),
			Cwd: m.deps.Cwd, SessionID: m.sessionID(), AgentName: spec.Name,
			Depth: 1, MaxTurns: 0, Parallel: true, // unlimited; the repeated-call guard still stops loops
			CacheRetention:     provider.CacheRetention(cacheRetention),
			WarmCache:          cacheWarm,
			MaxReasoningTurns:  m.deps.Loaded.Config.CacheMaxReasoningTurns,
			MaxToolResultBytes: m.deps.Loaded.Config.CacheMaxToolResultBytes,
		}
		cfg = inheritSwarmWorkerRuntime(cfg, snapshotter, m.deps.Plugins, m.deps.Goals)
		taskID := spec.TaskID
		if taskID == "" {
			taskID = spec.Name
		}
		prompt := fmt.Sprintf("Team goal: %s\n\nYour identity: %s\nYour task ID: %s\nYour assignment: %s\n\nYour task is already claimed for you. Do not claim it again; coordinate through team messages, and finish with complete_task or fail_task.", msg.goal, spec.Name, taskID, spec.Role)
		jobs = append(jobs, swarm.TeamJob{Name: spec.Name, TaskID: taskID, Runner: agent.NewAgentRunner(cfg, prompt)})
	}
	committed = true
	return &swarmRunPlan{ctx: s.Ctx, swarmID: id, modelID: m.modelID, jobs: jobs, tasks: s.Tasks, concurrency: 4}, nil
}

func (m *Model) updateManagedAgentStatus(swarmID, agentName string, kind swarm.EventType) {
	if m.deps.SwarmManager == nil {
		return
	}
	team, err := m.deps.SwarmManager.Get(swarmID)
	if err != nil {
		return
	}
	member, err := team.GetAgent(agentName)
	if err != nil {
		return
	}
	switch kind {
	case swarm.EventAgentStart:
		member.SetStatus(swarm.StatusWorking)
	case swarm.EventAgentDone:
		member.SetStatus(swarm.StatusDone)
	case swarm.EventAgentFailed:
		member.SetStatus(swarm.StatusFailed)
	}
}

func (m *Model) runSwarmPlan(plan *swarmRunPlan) {
	results := swarm.RunTaskTeam(plan.ctx, plan.jobs, plan.tasks, plan.concurrency, func(ev swarm.RuntimeEvent) {
		if aev, ok := ev.Value.(agent.Event); ok && aev.Kind == agent.EvUsage && aev.Usage != nil {
			m.recordUsageOnly(plan.modelID, *aev.Usage)
		}
		m.updateManagedAgentStatus(plan.swarmID, ev.Name, ev.Kind)
		msg := swarmWorkerMsg{swarmID: plan.swarmID, name: ev.Name, kind: ev.Kind, at: ev.Time}
		if aev, ok := ev.Value.(agent.Event); ok {
			msg.event = aev
		}
		if err, ok := ev.Value.(error); ok {
			msg.err = err
		}
		if output, ok := ev.Value.(string); ok {
			msg.output = output
		}
		m.program.Send(msg)
	})
	m.program.Send(swarmCompleteMsg{swarmID: plan.swarmID, results: results})
}

func (m *Model) applySwarmWorker(msg swarmWorkerMsg) {
	view := m.teamViews[msg.swarmID]
	if view == nil {
		return
	}
	av := view.Agents[msg.name]
	if av == nil {
		return
	}
	switch msg.kind {
	case swarm.EventAgentStart:
		av.Status, av.Started, av.CurrentAction, av.ActionStart = swarm.StatusWorking, msg.at, "starting", msg.at
	case swarm.EventAgentTool:
		switch msg.event.Kind {
		case agent.EvToolStart:
			if msg.event.Tool != nil {
				av.LastTool = msg.event.Tool.Name
				av.CurrentAction = strings.TrimSpace(msg.event.Tool.Name + " " + msg.event.Tool.Title)
				av.ActionStart = msg.at
				av.ToolsUsed++
			}
		case agent.EvText:
			if text := strings.TrimSpace(msg.event.Text); text != "" {
				av.CurrentAction = strings.Join(strings.Fields(text), " ")
				av.ActionStart = msg.at
			}
		case agent.EvUsage:
			if usage := msg.event.Usage; usage != nil {
				av.TokensIn += usage.InputTokens
				av.TokensOut += usage.OutputTokens
				av.CacheRead += usage.CacheReadTokens
				av.CacheWrite += usage.CacheWriteTokens
			}
		}
	case swarm.EventAgentDone:
		av.Status, av.CurrentAction, av.Result, av.Finished = swarm.StatusDone, "done", msg.output, msg.at
	case swarm.EventAgentFailed:
		av.Status, av.CurrentAction, av.Finished = swarm.StatusFailed, "failed", msg.at
		if msg.err != nil {
			av.Error = msg.err.Error()
		}
	}
	m.touch(view.MsgIndex)
	m.refresh()
}

func (m *Model) applySwarmComplete(msg swarmCompleteMsg) {
	view := m.teamViews[msg.swarmID]
	if view == nil || !view.Active {
		return
	}
	view.Active = false
	view.Finished = time.Now()
	if m.deps.SwarmManager != nil {
		if team, err := m.deps.SwarmManager.Get(msg.swarmID); err == nil {
			team.Cancel()
		}
		m.deps.SwarmManager.Remove(msg.swarmID)
	}
	m.activeSwarms--
	if m.activeSwarms < 0 {
		m.activeSwarms = 0
	}
	var history strings.Builder
	history.WriteString("Agent team results:\n")
	for _, result := range msg.results {
		av := view.Agents[result.Name]
		if av != nil {
			av.Status, av.Result, av.Finished = result.Status, result.Output, result.Finished
			if result.Err != nil {
				av.Error = result.Err.Error()
			}
		}
		fmt.Fprintf(&history, "\n[%s] status=%s\n", result.Name, result.Status)
		if result.Err != nil {
			fmt.Fprintf(&history, "ERROR: %v\n", result.Err)
		}
		history.WriteString(result.Output)
		history.WriteByte('\n')
	}
	teamResult := history.String()
	if view.complete != nil {
		select {
		case view.complete <- swarmStartReply{text: teamResult}:
		default:
		}
		view.complete = nil
	}
	in, out, cr, cw := view.computeTotals()
	m.billed.Input += in
	m.billed.Output += out
	m.billed.CacheRead += cr
	m.billed.CacheWrite += cw
	m.touch(view.MsgIndex)
	m.refresh()
	if err := m.saveSession(); err != nil {
		m.reportSessionSaveError(err)
	}
}

func (view *SwarmView) computeTotals() (in, out, cr, cw int) {
	for _, name := range view.AgentOrd {
		a := view.Agents[name]
		if a != nil {
			in += a.TokensIn
			out += a.TokensOut
			cr += a.CacheRead
			cw += a.CacheWrite
		}
	}
	return
}

func (m *Model) resetSwarmRuntime() {
	if m.deps.SwarmManager != nil {
		for id, view := range m.teamViews {
			if view != nil && view.Active {
				if team, err := m.deps.SwarmManager.Get(id); err == nil {
					team.Cancel()
				}
			}
			m.deps.SwarmManager.Remove(id)
		}
	}
	m.teamViews = map[string]*SwarmView{}
	m.activeSwarms = 0
}

func (m *Model) FormatSwarmCard(swarmID string, width int) string {
	view := m.teamViews[swarmID]
	if view == nil {
		return ""
	}
	return renderSwarmCard(view, width, time.Now(), m.styles, m.spinnerFrame())
}

// renderSwarmCard is pure: fixed state + width + time yields fixed output.
func renderSwarmCard(view *SwarmView, width int, now time.Time, styles *Styles, spinner string) string {
	if width < 1 {
		return ""
	}
	phase := "completed"
	if view.Active {
		phase = "active"
	}
	lines := []string{}
	appendWrapped := func(text, indent string, style lipgloss.Style) {
		for _, line := range strings.Split(wrapIndent(text, width, indent), "\n") {
			lines = append(lines, style.Render(line))
		}
	}
	appendWrapped("TEAM "+strings.ToUpper(view.Name)+" · "+phase, "", styles.Primary.Bold(true))
	if view.Goal != "" {
		appendWrapped("goal: "+view.Goal, "  ", styles.Faint)
	}
	if view.Tasks != nil {
		summary := view.Tasks.Summary()
		appendWrapped(fmt.Sprintf("tasks ready %d · pending %d · running %d · done %d · failed %d · blocked %d · cancelled %d", summary.Ready, summary.Pending, summary.InProgress, summary.Completed, summary.Failed, summary.Blocked, summary.Cancelled), "  ", styles.Muted)
	}
	for _, key := range view.AgentOrd {
		a := view.Agents[key]
		if a == nil {
			continue
		}
		icon := "○"
		switch a.Status {
		case swarm.StatusWorking:
			icon = spinner
		case swarm.StatusDone:
			icon = "✓"
		case swarm.StatusFailed:
			icon = "✗"
		}
		elapsedEnd := now
		if !a.Finished.IsZero() {
			elapsedEnd = a.Finished
		}
		elapsed := time.Duration(0)
		if !a.Started.IsZero() {
			elapsed = elapsedEnd.Sub(a.Started).Round(time.Second)
		}
		agentStyle := styles.Base.Foreground(lipgloss.Color(a.Color)).Bold(true)
		if width < 48 {
			appendWrapped(fmt.Sprintf("%s %s [%s] · elapsed %s", icon, a.Name, a.Status, elapsed), "  ", agentStyle)
		} else {
			details := []string{fmt.Sprintf("%s %s [%s]", icon, a.Name, a.Status)}
			if a.Role != "" {
				details = append(details, "role "+a.Role)
			}
			if a.CurrentAction != "" {
				details = append(details, a.CurrentAction)
			}
			details = append(details, fmt.Sprintf("tools %d", a.ToolsUsed), "elapsed "+elapsed.String())
			appendWrapped(strings.Join(details, " · "), "  ", agentStyle)
		}
		appendWrapped(fmt.Sprintf("tokens in %d · out %d · cache-read %d · cache-write %d", a.TokensIn, a.TokensOut, a.CacheRead, a.CacheWrite), "    ", styles.Muted)
		if !view.Active {
			result := a.Result
			if a.Error != "" {
				result = "ERROR: " + a.Error + func() string {
					if result != "" {
						return "\n" + result
					}
					return ""
				}()
			}
			if result != "" {
				appendWrapped(result, "    ", styles.Faint)
			}
		}
	}
	for i, line := range lines {
		lines[i] = clipANSI(line, width)
	}
	return strings.Join(lines, "\n")
}

func clipANSI(line string, width int) string {
	if lipgloss.Width(line) <= width {
		return line
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(line)
}
