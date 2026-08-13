package agent

import (
	"testing"
	"time"
)

// TestInferredEvictionPointDetectsPartialWave pins the bounded-LRU partial
// eviction inference: a turn whose cache-read share collapses from the
// previous turn (while still reading something) marks the eviction point.
func TestInferredEvictionPointDetectsPartialWave(t *testing.T) {
	r := New(Config{})
	// Two healthy turns: 90% of the prompt is read from cache.
	r.recordUsageSample(usageSample{prompt: 10_000, cacheRead: 9_000, gapBefore: time.Second})
	r.recordUsageSample(usageSample{prompt: 11_000, cacheRead: 9_900, gapBefore: time.Second})
	// A wave drops the mid-prefix region: the read share collapses to ~30%.
	r.recordUsageSample(usageSample{prompt: 12_000, cacheRead: 3_500, gapBefore: 30 * time.Second})

	point := r.inferredEvictionPoint()
	if point == 0 {
		t.Fatal("expected a partial eviction point to be inferred")
	}
	// The eviction cut is roughly what this turn still read.
	if point < 3_000 || point > 4_000 {
		t.Fatalf("eviction point = %d, want ~3500", point)
	}
}

// TestInferredEvictionPointNoneWhenStable pins the no-false-positive path:
// a steady cache-read share must not trigger a warm.
func TestInferredEvictionPointNoneWhenStable(t *testing.T) {
	r := New(Config{})
	for i := 0; i < 5; i++ {
		r.recordUsageSample(usageSample{prompt: 10_000 + i*100, cacheRead: 9_000 + i*90, gapBefore: time.Second})
	}
	if point := r.inferredEvictionPoint(); point != 0 {
		t.Fatalf("stable cache-read share inferred an eviction point: %d", point)
	}
}

// TestInferredEvictionPointTotalDropIsNotPartial pins that a total drop
// (cacheRead == 0) is NOT treated as a partial wave: the total-drop path is
// handled by cacheEvicted, and a zero read must not mislabel it partial.
func TestInferredEvictionPointTotalDropIsNotPartial(t *testing.T) {
	r := New(Config{})
	r.recordUsageSample(usageSample{prompt: 10_000, cacheRead: 9_000})
	r.recordUsageSample(usageSample{prompt: 10_000, cacheRead: 0})
	if point := r.inferredEvictionPoint(); point != 0 {
		t.Fatalf("total drop inferred as a partial eviction: %d", point)
	}
}
