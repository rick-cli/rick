package tui

import (
	"fmt"
	"testing"
	"time"
)

// TestTrimTranscriptKeepsRenderCacheFlat locks the trimTranscript regression:
// once a session passes maxTranscriptMessages, the trim must bring the count
// down to the cap and stay there. The old off-by-one left the transcript at
// 501 forever, so every refresh (25 Hz drain + 90 ms spinner) re-entered the
// trim, called invalidateAll, and forced a full re-render of every block per
// frame — the CPU spike seen in long sessions. The cache must keep working:
// a streamed chunk re-renders zero entries, not the whole history.
func TestTrimTranscriptKeepsRenderCacheFlat(t *testing.T) {
	m := newModelChoiceTestModel()
	for i := 0; i < maxTranscriptMessages+20; i++ {
		m.PushSystem(fmt.Sprintf("message %03d with enough text to wrap when the terminal narrows", i))
	}
	if got := len(m.msgs); got > maxTranscriptMessages {
		t.Fatalf("transcript over cap after trim: %d > %d", got, maxTranscriptMessages)
	}

	// A stream chunk must not re-render the history: only the dirty tail
	// entry should be rendered, so RenderCount stays flat.
	before := m.RenderCount()
	m.PushStreamChunk("token ")
	added := m.RenderCount() - before
	if added != 0 {
		t.Fatalf("stream chunk re-rendered %d entries; render cache not working", added)
	}

	// The trim must not re-fire on later refreshes (it would invalidateAll
	// again). Several chunks in a row must stay flat.
	for i := 0; i < 20; i++ {
		m.PushStreamChunk("more ")
	}
	added = m.RenderCount() - before
	if added != 0 {
		t.Fatalf("repeated stream chunks re-rendered %d entries; trim is re-triggering", added)
	}
}

// TestStreamChunkStaysUnderBudgetAboveCap measures the per-chunk cost once
// the transcript sits at the cap, the state that used to take ~950 µs/chunk
// (18x the normal ~50 µs) and 3.14 ms in the selftest. It must stay under
// 2 ms and scale sub-linearly with the backlog.
func TestStreamChunkStaysUnderBudgetAboveCap(t *testing.T) {
	stream := func(n int) time.Duration {
		m := newModelChoiceTestModel()
		for i := 0; i < n; i++ {
			m.PushSystem(fmt.Sprintf("scrollback %d", i))
		}
		m.ForceRunning(true)
		start := time.Now()
		for i := 0; i < 100; i++ {
			m.PushStreamChunk("token ")
			m.Refresh()
		}
		return time.Since(start) / 100
	}
	small := stream(10)
	large := stream(maxTranscriptMessages + 5)
	if large >= 2*time.Millisecond {
		t.Fatalf("stream chunk over 2ms at transcript cap: %v", large)
	}
	if large >= small*12+time.Millisecond {
		t.Fatalf("stream chunk scales super-linearly with backlog: small=%v large=%v", small, large)
	}
}
