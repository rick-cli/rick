// Package history selects a provider-facing conversation view without mutating
// the canonical transcript. Tool calls and their results are atomic groups.
package history

import (
	"encoding/json"

	"rick/internal/provider"
	"rick/internal/tokens"
)

type group struct {
	messages []provider.Message
	cost     int
}

// Retain returns an ordered, token-bounded provider view and the number of
// omitted logical groups. It never returns an orphaned tool result.
//
// Trimming is prefix-preserving: whole logical groups are dropped only from
// the oldest end, so the surviving messages keep their exact bytes from the
// previous turn and the provider prompt-cache prefix stays warm.
func Retain(messages []provider.Message, maxTokens int, encoding tokens.Encoding) ([]provider.Message, int) {
	copied := append([]provider.Message(nil), messages...)
	if len(copied) == 0 || maxTokens <= 0 {
		return copied, 0
	}

	groups := logicalGroups(copied, encoding)
	total := 0
	for _, item := range groups {
		total += item.cost
	}
	if total <= maxTokens {
		return copied, 0
	}

	// The newest logical group always survives: it is the current user
	// request or the latest tool result needed to continue the turn.
	keep := 1
	used := groups[len(groups)-1].cost
	for keep < len(groups) {
		next := groups[len(groups)-1-keep].cost
		if used+next > maxTokens {
			break
		}
		used += next
		keep++
	}

	start := len(groups) - keep
	retained := make([]provider.Message, 0, len(copied))
	for _, item := range groups[start:] {
		retained = append(retained, item.messages...)
	}
	return retained, start
}

// DropFirstGroups returns the messages without their first n logical groups (a
// tool_use/tool_result pair counts as one) while preserving every surviving
// message's bytes. It backs the agent's stable-head trimming: once the head is
// dropped we never drop again, so a provider view only ever grows at the tail
// and the prompt-cache prefix stays warm across requests.
func DropFirstGroups(messages []provider.Message, n int, encoding tokens.Encoding) []provider.Message {
	if n <= 0 {
		return append([]provider.Message(nil), messages...)
	}
	groups := logicalGroups(messages, encoding)
	if n > len(groups) {
		n = len(groups)
	}
	out := make([]provider.Message, 0, len(messages))
	for _, item := range groups[n:] {
		out = append(out, item.messages...)
	}
	return out
}

// TakeFirstGroups returns the messages that make up the first n logical groups
// (the same groups Retain/DropFirstGroups would omit); n is clamped to the
// number of groups. It backs archiving so trimmed originals stay traceable.
func TakeFirstGroups(messages []provider.Message, n int, encoding tokens.Encoding) []provider.Message {
	if n <= 0 {
		return nil
	}
	groups := logicalGroups(messages, encoding)
	if n > len(groups) {
		n = len(groups)
	}
	out := make([]provider.Message, 0, n)
	for _, item := range groups[:n] {
		out = append(out, item.messages...)
	}
	return out
}

func logicalGroups(messages []provider.Message, encoding tokens.Encoding) []group {
	groups := make([]group, 0, len(messages))
	for index := 0; index < len(messages); index++ {
		item := group{messages: []provider.Message{messages[index]}}
		if hasBlock(messages[index], "tool_use") && index+1 < len(messages) && hasBlock(messages[index+1], "tool_result") {
			item.messages = append(item.messages, messages[index+1])
			index++
		}
		item.cost = messageTokens(item.messages, encoding)
		groups = append(groups, item)
	}
	return groups
}

func messageTokens(messages []provider.Message, encoding tokens.Encoding) int {
	total := 0
	for _, message := range messages {
		encoded, err := json.Marshal(message)
		if err != nil {
			total += tokens.Count(message.Text(), encoding).Count
			continue
		}
		total += tokens.Count(string(encoded), encoding).Count + 4
	}
	return total
}

func hasBlock(message provider.Message, kind string) bool {
	for _, block := range message.Content {
		if block.Type == kind {
			return true
		}
	}
	return false
}
