package main

import (
	"context"
	"testing"
	"time"

	"rick/internal/agent"
	"rick/internal/provider"
	"rick/internal/tools"
)

// panicProvider streams a burst of events, then panics mid-stream. A runner
// panic must not leave the drain goroutine blocked forever (the old
// `wg.Wait()` after `runner.Run` would deadlock because Run's `defer
// close(out)` never runs).
type panicProvider struct{}

func (panicProvider) Name() string                   { return "panic" }
func (panicProvider) Models() []provider.ModelInfo   { return nil }
func (panicProvider) SetModels([]provider.ModelInfo) {}
func (panicProvider) Stream(ctx context.Context, req provider.Request, ch chan<- provider.Event) {
	defer close(ch)
	ch <- provider.Event{Kind: provider.EventText, Text: "x"}
	panic("boom")
}

func TestRunTurnPanicDoesNotDeadlock(t *testing.T) {
	runner := agent.New(agent.Config{
		Provider: panicProvider{},
		Model:    "panic",
		System:   "sys",
		Tools:    tools.NewRegistry(),
	})
	done := make(chan struct{})
	go func() {
		_, _, _ = runTurnSafe(context.Background(), runner, []provider.Message{provider.UserText("hi")})
		close(done)
	}()
	select {
	case <-done:
		// Good: the panic surfaced instead of deadlocking.
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: runTurnSafe blocked >5s after runner panic")
	}
}

// TestRunRawTurnPanicDoesNotDeadlock is the provider-level analogue: a panic
// in Stream must not hang the raw harness drain.
func TestRunRawTurnPanicDoesNotDeadlock(t *testing.T) {
	done := make(chan struct{})
	go func() {
		_, _ = runRawTurnSafe(context.Background(), panicProvider{}, "panic", "sys",
			"session", []provider.Message{provider.UserText("hi")}, 512)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: runRawTurnSafe blocked >5s after provider panic")
	}
}
