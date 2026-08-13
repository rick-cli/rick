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

func TestDayNetCostHitRate(t *testing.T) {
	cases := []struct {
		day  Day
		want float64
	}{
		{Day{Input: 100, CacheRead: 0}, 0},
		{Day{Input: 100, CacheRead: 900}, 90},
		// No writes: net == count-based hit rate.
		{Day{Input: 300, CacheRead: 700}, 70},
		// Writes are weighted at 1.25x: the denominator grows, so the net
		// rate is below the count-based rate on write-heavy turns (OpenAI
		// GPT-5.6+ bills writes at 1.25x uncached input).
		{Day{Input: 300, CacheRead: 700, CacheWrite: 100}, 700.0 / (300.0 + 700.0 + 125.0) * 100},
	}
	for _, c := range cases {
		if got := c.day.NetCostHitRate(); got != c.want {
			t.Errorf("NetCostHitRate(%+v) = %v, want %v", c.day, got, c.want)
		}
	}
	// Write-heavy: net must be strictly below the count-based rate.
	heavy := Day{Input: 100, CacheRead: 100, CacheWrite: 200}
	if net, count := heavy.NetCostHitRate(), heavy.CacheHitRate(); net >= count {
		t.Errorf("NetCostHitRate(%+v) = %v should be < CacheHitRate %v", heavy, net, count)
	}
}

func TestRecordRepairPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	tracker := New(dir)
	if err := tracker.RecordRepair("deepseek/deepseek-v4-flash", "read"); err != nil {
		t.Fatalf("RecordRepair returned error: %v", err)
	}
	if err := tracker.RecordRepair("deepseek/deepseek-v4-flash", "read"); err != nil {
		t.Fatalf("second RecordRepair returned error: %v", err)
	}
	if err := tracker.RecordRepair("deepseek/deepseek-v4-flash", "edit"); err != nil {
		t.Fatalf("third RecordRepair returned error: %v", err)
	}
	if err := tracker.Flush(); err != nil {
		t.Fatalf("Flush returned error: %v", err)
	}

	reloaded := New(dir)
	counts := reloaded.RepairsForModel("deepseek/deepseek-v4-flash")
	if counts.Total != 3 {
		t.Fatalf("total = %d, want 3", counts.Total)
	}
	if counts.Tools["read"] != 2 || counts.Tools["edit"] != 1 {
		t.Fatalf("tool counts = %+v, want read:2 edit:1", counts.Tools)
	}
}

func TestRecordRepairIgnoresEmptyKeys(t *testing.T) {
	tracker := New(t.TempDir())
	if err := tracker.RecordRepair("", "read"); err != nil {
		t.Fatalf("empty model returned error: %v", err)
	}
	if err := tracker.RecordRepair("model", ""); err != nil {
		t.Fatalf("empty tool returned error: %v", err)
	}
	if counts := tracker.RepairsForModel("model"); counts.Total != 0 {
		t.Fatalf("empty-key calls must not be counted, got %+v", counts)
	}
}

func TestLoadLegacyFileWithoutRepairs(t *testing.T) {
	// Files written before repair telemetry existed must still load.
	dir := t.TempDir()
	legacy := "{\n  \"2026-08-01\": {\n    \"model\": {\n      \"days\": {\"2026-08-01\": {\"input\": 1, \"output\": 2}},\n      \"total\": {\"input\": 1, \"output\": 2}\n    }\n  }\n}\n"
	if err := os.WriteFile(dir+"/usage.json", []byte(legacy), 0o644); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}
	tracker := New(dir)
	if !tracker.DateExists("2026-08-01") {
		t.Fatal("legacy usage not loaded")
	}
	if counts := tracker.RepairsForModel("model"); counts.Total != 0 {
		t.Fatalf("legacy file must have no repairs, got %+v", counts)
	}
}
