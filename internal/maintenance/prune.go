package maintenance

import (
	"os"
	"path/filepath"
	"sort"
	"time"
)

// SnapshotRetentionMaxAge is how long a project's shadow-repo snapshot tree
// may go untouched before it is pruned. Every snapshot commit updates the
// tree's directory mtimes, so mtime is a reliable last-use signal.
const SnapshotRetentionMaxAge = 30 * 24 * time.Hour

// PruneSnapshots deletes stale shadow-repo trees under <dataDir>/snapshots.
// A tree whose mtime is older than maxAge is removed outright; if the
// remaining trees still exceed maxBytes, the oldest are removed first.
// Returns the number of removed trees and the bytes freed (best-effort:
// a tree removed for age is walked once so its size can be reported).
func PruneSnapshots(dataDir string, maxAge time.Duration, maxBytes int64) (removed int, freed int64, err error) {
	trees, err := snapshotTrees(dataDir)
	if err != nil || len(trees) == 0 {
		return 0, 0, err
	}

	cutoff := time.Now().Add(-maxAge)
	var kept []snapshotTree
	for _, tree := range trees {
		if tree.modTime.Before(cutoff) {
			if size := removeTree(tree.path); size >= 0 {
				removed++
				freed += size
			}
			continue
		}
		kept = append(kept, tree)
	}

	if maxBytes > 0 {
		total := int64(0)
		for i := range kept {
			kept[i].size = dirSize(kept[i].path)
			total += kept[i].size
		}
		sort.Slice(kept, func(i, j int) bool { return kept[i].modTime.Before(kept[j].modTime) })
		for _, tree := range kept {
			if total <= maxBytes {
				break
			}
			if size := removeTree(tree.path); size >= 0 {
				removed++
				freed += size
				total -= size
			}
		}
	}
	return removed, freed, nil
}

// PruneSnapshotDirs is the cheap, stat-only startup variant: it removes trees
// older than maxAge without walking their contents, so it costs a directory
// listing per rick start regardless of how much data is pruned.
func PruneSnapshotDirs(dataDir string, maxAge time.Duration) (removed int, err error) {
	trees, err := snapshotTrees(dataDir)
	if err != nil || len(trees) == 0 {
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	for _, tree := range trees {
		if tree.modTime.Before(cutoff) {
			if os.RemoveAll(tree.path) == nil {
				removed++
			}
		}
	}
	return removed, nil
}

type snapshotTree struct {
	path    string
	modTime time.Time
	size    int64
}

func snapshotTrees(dataDir string) ([]snapshotTree, error) {
	root := filepath.Join(dataDir, "snapshots")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	trees := make([]snapshotTree, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		trees = append(trees, snapshotTree{path: filepath.Join(root, entry.Name()), modTime: info.ModTime()})
	}
	return trees, nil
}

func removeTree(path string) int64 {
	size := dirSize(path)
	if err := os.RemoveAll(path); err != nil {
		return -1
	}
	return size
}

func dirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
