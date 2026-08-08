package maintenance

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneSnapshotsRemovesOldTreesAndHonorsByteCeiling(t *testing.T) {
	dataDir := t.TempDir()
	root := filepath.Join(dataDir, "snapshots")

	mkTree := func(name string, modTime time.Time, payload []byte) string {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "HEAD"), payload, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatal(err)
		}
		return path
	}

	old := time.Now().Add(-40 * 24 * time.Hour)
	fresh := time.Now()
	mkTree("old_tree", old, []byte("x"))
	freshPath := mkTree("fresh_tree", fresh, make([]byte, 100))

	removed, freed, err := PruneSnapshots(dataDir, 30*24*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || freed != 1 {
		t.Fatalf("age prune removed=%d freed=%d, want 1/1", removed, freed)
	}
	if _, err := os.Stat(filepath.Join(root, "old_tree")); !os.IsNotExist(err) {
		t.Fatal("old tree still exists after age prune")
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Fatalf("fresh tree removed by age prune: %v", err)
	}

	// Byte ceiling: fresh tree (100 B) exceeds the 50 B budget, so it must go.
	removed, freed, err = PruneSnapshots(dataDir, 30*24*time.Hour, 50)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || freed != 100 {
		t.Fatalf("byte-ceiling prune removed=%d freed=%d, want 1/100", removed, freed)
	}
	if _, err := os.Stat(freshPath); !os.IsNotExist(err) {
		t.Fatal("fresh tree survived the byte ceiling")
	}
}

func TestPruneSnapshotDirsSkipsMissingRoot(t *testing.T) {
	removed, err := PruneSnapshotDirs(t.TempDir(), SnapshotRetentionMaxAge)
	if err != nil || removed != 0 {
		t.Fatalf("missing root: removed=%d err=%v, want 0/nil", removed, err)
	}
}
