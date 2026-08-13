package session

import (
	"testing"
)

// TestCompactionRecordPersistsAcrossSaveLoad pins the durable compaction
// transaction: the replaced span and the summary call's token cost survive a
// Save/Load round-trip, so a resumed session can keep the summary at the
// byte-identical position and measure the aux cost of every fold.
func TestCompactionRecordPersistsAcrossSaveLoad(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{
		ID:    "compact-1",
		Title: "with compaction",
		Cwd:   t.TempDir(),
		Compactions: []CompactionRecord{
			{
				Time:          "2026-01-01T00:00:00Z",
				ReplacedStart: 4,
				ReplacedEnd:   38,
				SummaryTokens: 612,
				Usage: RequestUsage{
					Input:      400,
					Output:     212,
					CacheRead:  0,
					CacheWrite: 0,
				},
			},
		},
	}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load("compact-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Compactions) != 1 {
		t.Fatalf("loaded %d compaction records, want 1", len(loaded.Compactions))
	}
	rec := loaded.Compactions[0]
	if rec.ReplacedStart != 4 || rec.ReplacedEnd != 38 {
		t.Fatalf("replaced span = [%d,%d), want [4,38)", rec.ReplacedStart, rec.ReplacedEnd)
	}
	if rec.SummaryTokens != 612 {
		t.Fatalf("summary tokens = %d, want 612", rec.SummaryTokens)
	}
	if rec.Usage.Input != 400 || rec.Usage.Output != 212 {
		t.Fatalf("summary call usage = %+v, want input 400 output 212", rec.Usage)
	}
}
