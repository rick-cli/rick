package usage

import (
	"bytes"
	"os"
	"testing"
	"time"
)

func TestRecordBatchesUntilFlush(t *testing.T) {
	tracker := New(t.TempDir())
	if err := tracker.Record("test-model", 1, 2, 0, 0); err != nil {
		t.Fatalf("first Record returned error: %v", err)
	}

	path := tracker.Path()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read initial usage file: %v", err)
	}

	tracker.mu.Lock()
	tracker.lastPersist = time.Now()
	tracker.mu.Unlock()

	if err := tracker.Record("test-model", 3, 4, 0, 0); err != nil {
		t.Fatalf("batched Record returned error: %v", err)
	}
	afterRecord, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read batched usage file: %v", err)
	}
	if !bytes.Equal(afterRecord, before) {
		t.Fatal("usage file changed before the persistence interval elapsed")
	}

	if err := tracker.Flush(); err != nil {
		t.Fatalf("Flush returned error: %v", err)
	}
	afterFlush, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read flushed usage file: %v", err)
	}
	if bytes.Equal(afterFlush, before) {
		t.Fatal("Flush did not persist the pending usage update")
	}
}

func TestDayCacheHitRate(t *testing.T) {
	cases := []struct {
		day  Day
		want float64
	}{
		{Day{Input: 0, CacheRead: 0}, 0},
		{Day{Input: 100, CacheRead: 0}, 0},
		{Day{Input: 100, CacheRead: 900}, 90},
		{Day{Input: 0, CacheRead: 500}, 100},
		// Cache writes were part of the prompt footprint (billed at miss
		// price), so they belong in the denominator.
		{Day{Input: 300, CacheRead: 700, CacheWrite: 50}, 200.0 / 3.0},
	}
	for _, c := range cases {
		if got := c.day.CacheHitRate(); got != c.want {
			t.Errorf("CacheHitRate(%+v) = %v, want %v", c.day, got, c.want)
		}
	}
}
