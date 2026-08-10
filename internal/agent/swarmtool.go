package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"rick/internal/provider"
	"rick/internal/swarm"
	"rick/internal/tools"
)

// SwarmTool lets the primary agent spawn and manage swarms.
type SwarmTool struct {
	Manager func(ctx context.Context, name, goal string, agents []SwarmAgentSpec, topo swarm.Topology) (string, error)
}

// SwarmAgentSpec describes an agent to spawn in a swarm.
type SwarmAgentSpec struct {
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	Tools     string   `json:"tools,omitempty"`
	TaskID    string   `json:"task_id,omitempty"`
	Subject   string   `json:"subject,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
}

func (SwarmTool) Name() string   { return "swarm" }
func (SwarmTool) ReadOnly() bool { return false }

func (t SwarmTool) Description() string {
	return "Spawn and manage a multi-agent swarm for complex collaborative work.\n\n" +
		"USE THIS when the task benefits from multiple agents working in parallel with messaging.\n" +
		"Examples:\n" +
		"- Research a topic from multiple angles simultaneously\n" +
		"- Coordinate a multi-step workflow where agents build on each other's findings\n\n" +
		"IMPORTANT: The tool waits for all teammates and returns their full declared-order results. Do not duplicate their work while it runs.\n\n" +
		"ACTIONS:\n" +
		"spawn: Create a named team, wait for it, and return every teammate result.\n\n" +
		"TOPOLOGIES: mesh (any-to-any), star (through primary), ring, pipeline\n\n" +
		"Inside a team, teammates use the trusted 'team' tool for tasks, messages, and shared-board coordination."
}

func (t SwarmTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"spawn"},
				"description": "What to do with the swarm",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Swarm name",
			},
			"goal": map[string]any{
				"type":        "string",
				"description": "The shared goal for all agents",
			},
			"topology": map[string]any{
				"type":        "string",
				"enum":        []string{"mesh", "star", "ring", "pipeline"},
				"description": "How agents communicate (default: mesh)",
			},
			"agents": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":       map[string]any{"type": "string", "description": "Agent name"},
						"role":       map[string]any{"type": "string", "description": "Agent role/prompt"},
						"tools":      map[string]any{"type": "string", "description": "Optional tool restriction"},
						"task_id":    map[string]any{"type": "string", "description": "Stable shared task ID (defaults to name)"},
						"subject":    map[string]any{"type": "string", "description": "Shared task subject"},
						"depends_on": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Task IDs that must complete first"},
					},
					"required": []string{"name", "role"},
				},
				"description": "Agents to spawn",
			},
		},
		"required": []string{"action"},
	}
}

type swarmArgs struct {
	Action   string           `json:"action"`
	Name     string           `json:"name"`
	Goal     string           `json:"goal"`
	Topology string           `json:"topology"`
	Agents   []SwarmAgentSpec `json:"agents"`
}

func (t SwarmTool) Run(ctx context.Context, tc tools.Context, in json.RawMessage) (tools.Result, error) {
	var a swarmArgs
	if err := tools.RepairDecode(in, &a, t.Schema(), tc.Repair); err != nil {
		return tools.Errf("invalid arguments: %v", err), nil
	}
	a.Action = strings.TrimSpace(a.Action)
	a.Name = strings.TrimSpace(a.Name)
	a.Goal = strings.TrimSpace(a.Goal)
	a.Topology = strings.ToLower(strings.TrimSpace(a.Topology))
	seenAgents := map[string]bool{}
	for i := range a.Agents {
		a.Agents[i].Name = strings.TrimSpace(a.Agents[i].Name)
		a.Agents[i].Role = strings.TrimSpace(a.Agents[i].Role)
		if a.Agents[i].Name == "" || a.Agents[i].Role == "" {
			return tools.Errf("agent names and roles must be non-empty"), nil
		}
		if seenAgents[a.Agents[i].Name] {
			return tools.Errf("duplicate agent name %q", a.Agents[i].Name), nil
		}
		seenAgents[a.Agents[i].Name] = true
	}

	switch a.Action {
	case "spawn":
		if a.Goal == "" {
			return tools.Errf("goal is required for spawn"), nil
		}
		if len(a.Agents) < 2 {
			return tools.Errf("at least 2 agents are required for a swarm (got %d)", len(a.Agents)), nil
		}
		topo := swarm.TopologyMesh
		if a.Topology != "" {
			topo = swarm.Topology(a.Topology)
		}
		switch topo {
		case swarm.TopologyMesh, swarm.TopologyStar, swarm.TopologyRing, swarm.TopologyPipeline:
		default:
			return tools.Errf("unsupported topology %q", a.Topology), nil
		}
		if t.Manager == nil {
			return tools.Errf("swarm manager not available"), nil
		}
		result, err := t.Manager(ctx, a.Name, a.Goal, a.Agents, topo)
		if err != nil {
			return tools.Errf("swarm spawn failed: %v", err), nil
		}
		return tools.Result{Output: capSubagentReport(result), Title: "agent team completed"}, nil

	default:
		return tools.Errf("unknown action %q (use: spawn)", a.Action), nil
	}
}

// MessageTool lets swarm agents send messages to each other.
type MessageTool struct {
	Send      func(m swarm.Message) error
	AgentName string
}

func (MessageTool) Name() string   { return "message" }
func (MessageTool) ReadOnly() bool { return false }

func (t MessageTool) Description() string {
	return "Send a message to another agent in your swarm.\n\n" +
		"USE THIS to coordinate with other agents: ask questions, share findings, request work.\n\n" +
		"TYPES:\n" +
		"task: Assign work to an agent\n" +
		"response: Reply to a question from another agent\n" +
		"broadcast: Send to all agents at once\n" +
		"result: Post a completed finding (signals you're done)\n" +
		"question: Ask another agent something\n\n" +
		"Use to='*' for broadcast, or target a specific agent by name."
}

func (t MessageTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"to": map[string]any{
				"type":        "string",
				"description": "Target agent name, or '*' for broadcast",
			},
			"type": map[string]any{
				"type":        "string",
				"enum":        []string{"task", "response", "broadcast", "result", "question"},
				"description": "Message type",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "The message content",
			},
		},
		"required": []string{"to", "type", "content"},
	}
}

type messageArgs struct {
	To      string `json:"to"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

func (t MessageTool) Run(ctx context.Context, tc tools.Context, in json.RawMessage) (tools.Result, error) {
	var a messageArgs
	if err := tools.RepairDecode(in, &a, t.Schema(), tc.Repair); err != nil {
		return tools.Errf("invalid arguments: %v", err), nil
	}
	if a.To == "" {
		return tools.Errf("to is required"), nil
	}
	if a.Content == "" {
		return tools.Errf("content is required"), nil
	}

	msg := swarm.NewMessage(t.AgentName, a.To, swarm.MessageType(a.Type), a.Content)
	if t.Send == nil {
		return tools.Errf("message routing not available"), nil
	}
	if err := t.Send(msg); err != nil {
		return tools.Errf("send failed: %v", err), nil
	}
	return tools.Result{
		Output: fmt.Sprintf("message sent to %s", a.To),
		Title:  "message sent",
		Meta:   map[string]any{"to": a.To, "type": a.Type},
	}, nil
}

// BoardTool lets swarm agents read and write the shared scratch board.
type BoardTool struct {
	Board     *swarm.Board
	AgentName string
}

func (BoardTool) Name() string   { return "board" }
func (BoardTool) ReadOnly() bool { return false }

func (t BoardTool) Description() string {
	return "Read and write the shared scratch board for your swarm.\n\n" +
		"USE THIS to share findings with other agents: post analysis results, read what others found, coordinate work.\n\n" +
		"ACTIONS:\n" +
		"put: Write a key-value pair to the board\n" +
		"get: Read a value by key\n" +
		"list: Show all board entries\n\n" +
		"Example: board put key=\"auth_analysis\" value=\"JWT tokens in middleware.go\""
}

func (t BoardTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"put", "get", "list"},
				"description": "Board action",
			},
			"key": map[string]any{
				"type":        "string",
				"description": "Key for put/get",
			},
			"value": map[string]any{
				"type":        "string",
				"description": "Value for put",
			},
		},
		"required": []string{"action"},
	}
}

type boardArgs struct {
	Action string `json:"action"`
	Key    string `json:"key"`
	Value  string `json:"value"`
}

func (t BoardTool) Run(ctx context.Context, tc tools.Context, in json.RawMessage) (tools.Result, error) {
	var a boardArgs
	if err := tools.RepairDecode(in, &a, t.Schema(), tc.Repair); err != nil {
		return tools.Errf("invalid arguments: %v", err), nil
	}
	if t.Board == nil {
		return tools.Errf("board not available"), nil
	}

	switch a.Action {
	case "put":
		if a.Key == "" {
			return tools.Errf("key is required for put"), nil
		}
		t.Board.Put(a.Key, a.Value, t.AgentName)
		return tools.Result{
			Output: fmt.Sprintf("board[%s] = %s", a.Key, a.Value),
			Title:  "board write",
			Meta:   map[string]any{"key": a.Key},
		}, nil

	case "get":
		entry, err := t.Board.Get(a.Key)
		if err != nil {
			return tools.Errf("%v", err), nil
		}
		return tools.Result{
			Output: fmt.Sprintf("[%s] %s = %s", entry.Author, entry.Key, entry.Value),
			Title:  "board read",
			Meta:   map[string]any{"key": a.Key, "author": entry.Author},
		}, nil

	case "list":
		entries := t.Board.List()
		if len(entries) == 0 {
			return tools.Result{Output: "(board is empty)", Title: "board list"}, nil
		}
		var b strings.Builder
		for _, e := range entries {
			fmt.Fprintf(&b, "[%s] %s = %s\n", e.Author, e.Key, e.Value)
		}
		return tools.Result{
			Output: strings.TrimRight(b.String(), "\n"),
			Title:  fmt.Sprintf("board (%d entries)", len(entries)),
		}, nil

	default:
		return tools.Errf("unknown action %q (use: put, get, list)", a.Action), nil
	}
}

// AgentRunner adapts agent.Runner to swarm.Runner interface.
type AgentRunner struct {
	cfg    Config
	prompt string
}

// NewAgentRunner creates a runner for a swarm agent.
func NewAgentRunner(cfg Config, prompt string) *AgentRunner {
	return &AgentRunner{cfg: cfg, prompt: prompt}
}

// Run executes the agent's tool loop.
func (r *AgentRunner) Run(ctx context.Context, onEvent func(any)) (string, error) {
	runner := New(r.cfg)
	ch := make(chan Event, 128)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range ch {
			if onEvent != nil {
				onEvent(ev)
			}
		}
	}()

	history := []provider.Message{provider.UserText(r.prompt)}
	appended, err := runner.Run(ctx, history, ch)
	<-done
	return lastAssistantText(appended), err
}

func lastAssistantText(messages []provider.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == provider.RoleAssistant {
			return messages[i].Text()
		}
	}
	return ""
}

// Ensure AgentRunner satisfies swarm.Runner interface.
var _ swarm.Runner = (*AgentRunner)(nil)
