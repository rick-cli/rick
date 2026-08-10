package openai

import (
	"context"
	"strings"
	"testing"

	"rick/internal/provider"
)

func TestReadSSEReportsMalformedData(t *testing.T) {
	client := New("test-gateway", "key", "https://example.invalid/v1")
	var events []provider.Event
	client.readSSE(context.Background(), strings.NewReader("data: {not-json}\n\ndata: [DONE]\n\n"), func(event provider.Event) bool {
		events = append(events, event)
		return true
	}, false)

	if len(events) != 1 || events[0].Kind != provider.EventError {
		t.Fatalf("events = %#v, want one error", events)
	}
	if events[0].Err == nil || !strings.Contains(events[0].Err.Error(), "malformed SSE data") {
		t.Fatalf("error = %v, want malformed SSE data", events[0].Err)
	}
}

func TestReadSSERejectsMalformedToolArguments(t *testing.T) {
	client := New("test-gateway", "key", "https://example.invalid/v1")
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		"data: [DONE]",
		"",
	}, "\n\n")
	var events []provider.Event
	client.readSSE(context.Background(), strings.NewReader(stream), func(event provider.Event) bool {
		events = append(events, event)
		return true
	}, false)

	for _, event := range events {
		if event.Kind == provider.EventToolCall || event.Kind == provider.EventDone {
			t.Fatalf("malformed call escaped as event: %#v", event)
		}
	}
	if len(events) == 0 || events[len(events)-1].Kind != provider.EventError {
		t.Fatalf("events = %#v, want terminal error", events)
	}
	if err := events[len(events)-1].Err; err == nil || !strings.Contains(err.Error(), "malformed arguments") {
		t.Fatalf("error = %v, want malformed arguments", err)
	}
}

func TestReadSSERejectsToolCallWithoutName(t *testing.T) {
	client := New("test-gateway", "key", "https://example.invalid/v1")
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		"data: [DONE]",
		"",
	}, "\n\n")
	var events []provider.Event
	client.readSSE(context.Background(), strings.NewReader(stream), func(event provider.Event) bool {
		events = append(events, event)
		return true
	}, false)

	for _, event := range events {
		if event.Kind == provider.EventToolCall || event.Kind == provider.EventDone {
			t.Fatalf("nameless call escaped as event: %#v", event)
		}
	}
	if len(events) == 0 || events[len(events)-1].Kind != provider.EventError {
		t.Fatalf("events = %#v, want terminal error", events)
	}
	if err := events[len(events)-1].Err; err == nil || !strings.Contains(err.Error(), "missing function name") {
		t.Fatalf("error = %v, want missing function name", err)
	}
}

func TestReadSSERejectsEmptyCompletion(t *testing.T) {
	client := New("test-gateway", "key", "https://example.invalid/v1")
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5000,"completion_tokens":0}}`,
		"data: [DONE]",
		"",
	}, "\n\n")
	var events []provider.Event
	client.readSSE(context.Background(), strings.NewReader(stream), func(event provider.Event) bool {
		events = append(events, event)
		return true
	}, false)

	for _, event := range events {
		if event.Kind == provider.EventDone {
			t.Fatalf("empty completion escaped as done: %#v", events)
		}
	}
	if len(events) == 0 || events[len(events)-1].Kind != provider.EventError {
		t.Fatalf("events = %#v, want terminal error", events)
	}
	if err := events[len(events)-1].Err; err == nil || !strings.Contains(err.Error(), "empty completion") {
		t.Fatalf("error = %v, want empty completion", err)
	}
}

func TestReadSSERejectsTruncatedToolCallStream(t *testing.T) {
	client := New("test-gateway", "key", "https://example.invalid/v1")
	stream := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"read","arguments":"{\"path\":\"file.txt\"}"}}]}}]}` + "\n\n"
	var events []provider.Event
	client.readSSE(context.Background(), strings.NewReader(stream), func(event provider.Event) bool {
		events = append(events, event)
		return true
	}, false)

	for _, event := range events {
		if event.Kind == provider.EventToolCall || event.Kind == provider.EventDone {
			t.Fatalf("truncated call escaped as successful event: %#v", event)
		}
	}
	if len(events) == 0 || events[len(events)-1].Kind != provider.EventError {
		t.Fatalf("events = %#v, want terminal truncation error", events)
	}
	if err := events[len(events)-1].Err; err == nil || !strings.Contains(err.Error(), "ended without a completion marker") {
		t.Fatalf("error = %v, want missing completion marker", err)
	}
}

func TestReadSSERejectsNonObjectToolArguments(t *testing.T) {
	client := New("test-gateway", "key", "https://example.invalid/v1")
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"read","arguments":"[]"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		"data: [DONE]",
		"",
	}, "\n\n")
	var events []provider.Event
	client.readSSE(context.Background(), strings.NewReader(stream), func(event provider.Event) bool {
		events = append(events, event)
		return true
	}, false)

	for _, event := range events {
		if event.Kind == provider.EventToolCall || event.Kind == provider.EventDone {
			t.Fatalf("non-object call escaped as successful event: %#v", event)
		}
	}
	if len(events) == 0 || events[len(events)-1].Kind != provider.EventError {
		t.Fatalf("events = %#v, want terminal argument-shape error", events)
	}
	if err := events[len(events)-1].Err; err == nil || !strings.Contains(err.Error(), "JSON object") {
		t.Fatalf("error = %v, want JSON object error", err)
	}
}

func TestReadSSERejectsDuplicateToolCallIDsAsOneInvalidBatch(t *testing.T) {
	client := New("test-gateway", "key", "https://example.invalid/v1")
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"duplicate","function":{"name":"read","arguments":"{}"}},{"index":1,"id":"duplicate","function":{"name":"read","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		"data: [DONE]",
		"",
	}, "\n\n")
	var events []provider.Event
	client.readSSE(context.Background(), strings.NewReader(stream), func(event provider.Event) bool {
		events = append(events, event)
		return true
	}, false)

	for _, event := range events {
		if event.Kind == provider.EventToolCall || event.Kind == provider.EventDone {
			t.Fatalf("duplicate-ID batch escaped partially: %#v", event)
		}
	}
	if len(events) == 0 || events[len(events)-1].Kind != provider.EventError {
		t.Fatalf("events = %#v, want terminal duplicate-ID error", events)
	}
	if err := events[len(events)-1].Err; err == nil || !strings.Contains(err.Error(), "duplicate tool call ID") {
		t.Fatalf("error = %v, want duplicate tool call ID", err)
	}
}

func TestReadSSESurfacesStreamedRefusalAsAssistantText(t *testing.T) {
	client := New("test-gateway", "key", "https://example.invalid/v1")
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"refusal":"I can’t help with that."}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"data: [DONE]",
		"",
	}, "\n\n")
	var events []provider.Event
	client.readSSE(context.Background(), strings.NewReader(stream), func(event provider.Event) bool {
		events = append(events, event)
		return true
	}, false)

	var text strings.Builder
	for _, event := range events {
		if event.Kind == provider.EventError {
			t.Fatalf("valid refusal became provider error: %v", event.Err)
		}
		if event.Kind == provider.EventText {
			text.WriteString(event.Text)
		}
	}
	if got := text.String(); got != "I can’t help with that." {
		t.Fatalf("assistant text = %q, want streamed refusal", got)
	}
	if len(events) == 0 || events[len(events)-1].Kind != provider.EventDone {
		t.Fatalf("events = %#v, want terminal done", events)
	}
}
