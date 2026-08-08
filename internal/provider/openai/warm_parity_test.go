package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"rick/internal/provider"
)

func TestWarmAndStreamBodiesMatchDeepseekReasoning(t *testing.T) {
	var warmBody, streamBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if stream, _ := m["stream"].(bool); stream {
			streamBody = m
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"x"},"usage":{"input_tokens":1,"cache_read_input_tokens":0}}]}`+"\n\n")
			_, _ = io.WriteString(w, `data: [DONE]`)
			return
		}
		warmBody = m
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()

	client := New("deepseek", "test-key", server.URL)
	client.HTTP = server.Client()

	msgs := []provider.Message{
		provider.UserText("hello"),
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "thinking", Text: "let me think"},
			{Type: "text", Text: "the answer"},
		}},
	}
	req := provider.Request{
		Model:             "deepseek-v4-flash",
		System:            "stable system",
		Messages:          msgs,
		Reasoning:         provider.ReasoningOn,
		MaxReasoningTurns: 0,
		SessionID:         "sess-1",
		CacheRetention:    provider.CacheRetentionLong,
		MaxTokens:         2048,
	}

	if cw, ok := interface{}(client).(provider.CacheWarmber); ok {
		if err := cw.Warm(context.Background(), req); err != nil {
			t.Fatalf("warm: %v", err)
		}
	}
	ch := make(chan provider.Event, 8)
	go client.Stream(context.Background(), req, ch)
	for range ch {
	}

	diffKeys(warmBody, streamBody, t)
}

func diffKeys(warm, stream map[string]any, t *testing.T) {
	for k := range stream {
		if k == "stream" || k == "stream_options" || k == "max_completion_tokens" || k == "max_tokens" {
			continue // legitimately differ
		}
		wv, ok := warm[k]
		if !ok {
			t.Errorf("warm body missing stream field %q (stream=%v)", k, stream[k])
			continue
		}
		ws, _ := json.Marshal(wv)
		ss, _ := json.Marshal(stream[k])
		if string(ws) != string(ss) {
			t.Errorf("field %q differs: warm=%s stream=%s", k, ws, ss)
		}
	}
	for k := range warm {
		if _, ok := stream[k]; !ok {
			t.Errorf("stream body missing warm field %q (warm=%v)", k, warm[k])
		}
	}
}
