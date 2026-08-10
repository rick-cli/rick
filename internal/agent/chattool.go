package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"rick/internal/tools"
)

// ChatTool delivers a message to another live agent.
type ChatTool struct {
	Registry *Registry
}

func (ChatTool) Name() string   { return "chat" }
func (ChatTool) ReadOnly() bool { return false }

func (ChatTool) Description() string {
	return "Send a message to another live agent in the current hierarchy. Use an agent ID or unique name."
}

func (ChatTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target_agent": map[string]any{"type": "string", "description": "Agent ID or unique name."},
			"message":      map[string]any{"type": "string", "description": "Message to deliver before the target's next model turn."},
		},
		"required": []string{"target_agent", "message"},
	}
}

type chatArgs struct {
	TargetAgent string `json:"target_agent"`
	Message     string `json:"message"`
}

func (t ChatTool) Run(_ context.Context, tc tools.Context, input json.RawMessage) (tools.Result, error) {
	var args chatArgs
	if err := tools.RepairDecode(input, &args, t.Schema(), tc.Repair); err != nil {
		return tools.Errf("invalid arguments: %v", err), nil
	}
	args.TargetAgent = strings.TrimSpace(args.TargetAgent)
	args.Message = strings.TrimSpace(args.Message)
	if args.TargetAgent == "" || args.Message == "" {
		return tools.Errf("target_agent and message are required"), nil
	}
	if t.Registry == nil {
		return tools.Errf("agent registry is unavailable"), nil
	}
	target, ok := t.Registry.Find(args.TargetAgent)
	if !ok {
		return tools.Errf("agent %q was not found or is not unique", args.TargetAgent), nil
	}
	sender := tc.AgentID
	if sender == "" {
		sender = tc.Agent
	}
	if err := t.Registry.Send(target.ID, sender, args.Message); err != nil {
		return tools.Errf("chat failed: %v", err), nil
	}
	return tools.Result{
		Output: fmt.Sprintf("message sent to %s", target.ID),
		Title:  "agent message sent",
		Meta:   map[string]any{"target_agent": target.ID},
	}, nil
}

// SteerTool injects a live instruction into another agent's conversation.
type SteerTool struct {
	Registry *Registry
}

func (SteerTool) Name() string   { return "steer" }
func (SteerTool) ReadOnly() bool { return false }

func (SteerTool) Description() string {
	return "Steer a live agent without cancelling it. The instruction is injected before its next model turn."
}

func (SteerTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target_agent": map[string]any{"type": "string", "description": "Agent ID or unique name."},
			"instruction":  map[string]any{"type": "string", "description": "Instruction that changes the live agent's focus."},
		},
		"required": []string{"target_agent", "instruction"},
	}
}

type steerArgs struct {
	TargetAgent string `json:"target_agent"`
	Instruction string `json:"instruction"`
}

func (t SteerTool) Run(_ context.Context, tc tools.Context, input json.RawMessage) (tools.Result, error) {
	var args steerArgs
	if err := tools.RepairDecode(input, &args, t.Schema(), tc.Repair); err != nil {
		return tools.Errf("invalid arguments: %v", err), nil
	}
	args.TargetAgent = strings.TrimSpace(args.TargetAgent)
	args.Instruction = strings.TrimSpace(args.Instruction)
	if args.TargetAgent == "" || args.Instruction == "" {
		return tools.Errf("target_agent and instruction are required"), nil
	}
	if t.Registry == nil {
		return tools.Errf("agent registry is unavailable"), nil
	}
	target, ok := t.Registry.Find(args.TargetAgent)
	if !ok {
		return tools.Errf("agent %q was not found or is not unique", args.TargetAgent), nil
	}
	sender := tc.AgentID
	if sender == "" {
		sender = tc.Agent
	}
	if err := t.Registry.Steer(target.ID, sender, args.Instruction); err != nil {
		return tools.Errf("steer failed: %v", err), nil
	}
	return tools.Result{
		Output: fmt.Sprintf("steering instruction sent to %s", target.ID),
		Title:  "agent steered",
		Meta:   map[string]any{"target_agent": target.ID},
	}, nil
}
