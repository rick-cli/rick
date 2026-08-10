package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"rick/internal/swarm"
	"rick/internal/tools"
)

type TeamTool struct {
	Swarm *swarm.Swarm
}

func (TeamTool) Name() string   { return "team" }
func (TeamTool) ReadOnly() bool { return false }

func (TeamTool) Description() string {
	return "Coordinate with your agent team through its shared task list, mailboxes, and board. " +
		"Claim ready work before starting, update it when finished, send direct messages only when another teammate needs the information, and inspect your inbox between major steps."
}

func (TeamTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": []string{"list_tasks", "claim_task", "complete_task", "fail_task", "send_message", "read_messages", "board_put", "board_list"},
			},
			"task_id": map[string]any{"type": "string", "description": "Task ID for completion or failure."},
			"result":  map[string]any{"type": "string", "description": "Concrete task result returned to the team lead."},
			"to":      map[string]any{"type": "string", "description": "Teammate name or '*' for broadcast."},
			"type":    map[string]any{"type": "string", "enum": []string{"task", "response", "broadcast", "result", "question"}},
			"content": map[string]any{"type": "string", "description": "Message content."},
			"key":     map[string]any{"type": "string", "description": "Shared board key."},
			"value":   map[string]any{"type": "string", "description": "Shared board value."},
		},
		"required": []string{"action"},
	}
}

type teamArgs struct {
	Action  string `json:"action"`
	TaskID  string `json:"task_id"`
	Result  string `json:"result"`
	To      string `json:"to"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Key     string `json:"key"`
	Value   string `json:"value"`
}

func (t TeamTool) Run(_ context.Context, tc tools.Context, input json.RawMessage) (tools.Result, error) {
	if t.Swarm == nil {
		return tools.Errf("team is unavailable"), nil
	}
	if tc.Agent == "" {
		return tools.Errf("team member identity is unavailable"), nil
	}
	if _, err := t.Swarm.GetAgent(tc.Agent); err != nil {
		return tools.Errf("agent %q is not a member of this team", tc.Agent), nil
	}

	var args teamArgs
	if err := tools.RepairDecode(input, &args, t.Schema(), tc.Repair); err != nil {
		return tools.Errf("invalid arguments: %v", err), nil
	}
	args.Action = strings.TrimSpace(args.Action)
	args.TaskID = strings.TrimSpace(args.TaskID)
	args.To = strings.TrimSpace(args.To)
	args.Type = strings.TrimSpace(args.Type)
	args.Content = strings.TrimSpace(args.Content)
	args.Key = strings.TrimSpace(args.Key)
	args.Result = strings.TrimSpace(args.Result)

	switch args.Action {
	case "list_tasks":
		return tools.Result{Title: "team tasks", Output: formatTeamTasks(t.Swarm.Tasks.List())}, nil
	case "claim_task":
		var (
			task swarm.Task
			err  error
		)
		if args.TaskID != "" {
			task, err = t.Swarm.Tasks.Claim(args.TaskID, tc.Agent)
		} else {
			task, err = t.Swarm.Tasks.ClaimNext(tc.Agent)
		}
		if err != nil {
			return tools.Errf("claim failed: %v", err), nil
		}
		t.Swarm.Emit(swarm.Event{Kind: swarm.EventTaskUpdate, Agent: tc.Agent, Detail: task.ID, Meta: map[string]any{"status": string(task.Status)}})
		return tools.Result{Title: "claimed " + task.ID, Output: formatTeamTask(task)}, nil
	case "complete_task":
		if err := t.Swarm.Tasks.Complete(args.TaskID, tc.Agent, args.Result); err != nil {
			return tools.Errf("complete failed: %v", err), nil
		}
		t.Swarm.Emit(swarm.Event{Kind: swarm.EventTaskUpdate, Agent: tc.Agent, Detail: args.TaskID, Meta: map[string]any{"status": string(swarm.TaskCompleted)}})
		return tools.Result{Title: "completed " + args.TaskID, Output: "task completed"}, nil
	case "fail_task":
		if err := t.Swarm.Tasks.Fail(args.TaskID, tc.Agent, args.Result); err != nil {
			return tools.Errf("fail failed: %v", err), nil
		}
		t.Swarm.Emit(swarm.Event{Kind: swarm.EventTaskUpdate, Agent: tc.Agent, Detail: args.TaskID, Meta: map[string]any{"status": string(swarm.TaskFailed)}})
		return tools.Result{Title: "failed " + args.TaskID, Output: "task marked failed"}, nil
	case "send_message":
		if args.To == "" || args.Content == "" {
			return tools.Errf("to and content are required"), nil
		}
		messageType := swarm.MessageType(args.Type)
		if messageType == "" {
			messageType = swarm.MsgResponse
		}
		switch messageType {
		case swarm.MsgTask, swarm.MsgResponse, swarm.MsgBroadcast, swarm.MsgResult, swarm.MsgQuestion:
		default:
			return tools.Errf("unsupported message type %q", args.Type), nil
		}
		if err := t.Swarm.Message(swarm.NewMessage(tc.Agent, args.To, messageType, args.Content)); err != nil {
			return tools.Errf("send failed: %v", err), nil
		}
		return tools.Result{Title: "message to " + args.To, Output: "message sent"}, nil
	case "read_messages":
		member, err := t.Swarm.GetAgent(tc.Agent)
		if err != nil {
			return tools.Errf("inbox failed: %v", err), nil
		}
		return tools.Result{Title: "team inbox", Output: formatTeamMessages(member.ReadMessages())}, nil
	case "board_put":
		if args.Key == "" {
			return tools.Errf("key is required"), nil
		}
		t.Swarm.BoardPut(args.Key, args.Value, tc.Agent)
		return tools.Result{Title: "shared " + args.Key, Output: "board updated"}, nil
	case "board_list":
		var lines []string
		for _, entry := range t.Swarm.Board.List() {
			lines = append(lines, fmt.Sprintf("[%s] %s = %s", entry.Author, entry.Key, entry.Value))
		}
		if len(lines) == 0 {
			lines = append(lines, "(board is empty)")
		}
		return tools.Result{Title: "team board", Output: strings.Join(lines, "\n")}, nil
	default:
		return tools.Errf("unknown team action %q", args.Action), nil
	}
}

func formatTeamTasks(tasks []swarm.Task) string {
	if len(tasks) == 0 {
		return "(no tasks)"
	}
	lines := make([]string, 0, len(tasks))
	for _, task := range tasks {
		lines = append(lines, formatTeamTask(task))
	}
	return strings.Join(lines, "\n")
}

func formatTeamTask(task swarm.Task) string {
	owner := task.Owner
	if owner == "" {
		owner = "unassigned"
	}
	return fmt.Sprintf("[%s] %s: %s (owner: %s)", task.Status, task.ID, task.Subject, owner)
}

func formatTeamMessages(messages []swarm.Message) string {
	if len(messages) == 0 {
		return "(inbox is empty)"
	}
	lines := make([]string, 0, len(messages))
	for _, message := range messages {
		lines = append(lines, fmt.Sprintf("[%s] %s: %s", message.Type, message.From, message.Content))
	}
	return strings.Join(lines, "\n")
}
