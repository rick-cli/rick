// Package prefixstore persists session-stable provider-prefix artifacts so
// a resumed or forked session can replay byte-identical bytes and keep the
// provider prompt cache warm across sessions.
//
// The key insight: a provider's automatic prefix cache is keyed by the exact
// bytes of the request head (system + tools + the first messages). A session
// that derives the same stable head as an older one can load the older
// session's pinned snippet — the deterministic goal-state snapshot — and
// splice it at the same prefix position, so the provider serves the shared
// prefix from cache instead of re-priming cold.
//
// The store is a small content-addressed directory under the rick data dir:
//
//	<data>/memory/
//	  loadkeys/<loadkey>.json    -> the pinned snippet for one (project, model, agent)
//	  snapshots/<id>.json        -> the snapshot bodies (content-addressed)
//
// Write-through is synchronous and atomic (tmp + rename), same convention as
// the goal store.
package prefixstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"rick/internal/memory"
)

// Store is the on-disk shared prefix store.
type Store struct {
	mu        sync.Mutex
	root      string
	loadDir   string
	snapDir   string
	loadCache map[string]string // loadkey -> snapshot id
}

// New opens (and creates) the store under root. root is typically
// config.DataDir() + "/memory".
func New(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("prefixstore: empty root")
	}
	loadDir := filepath.Join(root, "loadkeys")
	snapDir := filepath.Join(root, "snapshots")
	for _, dir := range []string{root, loadDir, snapDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{root: root, loadDir: loadDir, snapDir: snapDir, loadCache: map[string]string{}}, nil
}

// LoadKey derives the session-stable key under which a pinned snippet is
// shared. project, model, and agent identity are frozen per session, so a
// resumed session (new session id) with the same header derives the same key
// and reuses the older session's pinned prefix instead of starting cold.
func LoadKey(project, model, agent string) string {
	parts := []string{}
	for _, p := range []string{project, model, agent} {
		parts = append(parts, strings.TrimSpace(p))
	}
	key := strings.Join(parts, "\x00")
	if len(key) > 200 {
		key = key[:200]
	}
	// Filesystem-safe: the key is a digest, not the raw path.
	return memory.IDFromText(key)
}

// PutSnapshot stores a snapshot and returns its id.
func (s *Store) PutSnapshot(snap memory.Snapshot) (string, error) {
	if snap.ID == "" {
		return "", fmt.Errorf("prefixstore: snapshot has no id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.snapDir, snap.ID+".json")
	if _, err := os.Stat(path); err == nil {
		return snap.ID, nil
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return "", err
	}
	if err := atomicWrite(path, data); err != nil {
		return "", err
	}
	return snap.ID, nil
}

// LoadSnapshot reads a snapshot by id.
func (s *Store) LoadSnapshot(id string) (memory.Snapshot, bool) {
	if id == "" || strings.ContainsAny(id, "/\\") {
		return memory.Snapshot{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(s.snapDir, id+".json"))
	if err != nil {
		return memory.Snapshot{}, false
	}
	var snap memory.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return memory.Snapshot{}, false
	}
	return snap, true
}

// Pin binds a loadkey to a snapshot id. The binding is written through so a
// resumed session in a fresh process sees it.
func (s *Store) Pin(loadKey, snapID string) error {
	if loadKey == "" || snapID == "" {
		return fmt.Errorf("prefixstore: empty pin")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.loadDir, loadKey+".json")
	entry := struct {
		SnapshotID string `json:"snapshot_id"`
	}{SnapshotID: snapID}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(path, data); err != nil {
		return err
	}
	s.loadCache[loadKey] = snapID
	return nil
}

// LoadPinned returns the snapshot currently pinned for a loadkey.
func (s *Store) LoadPinned(loadKey string) (memory.Snapshot, bool) {
	if loadKey == "" {
		return memory.Snapshot{}, false
	}
	s.mu.Lock()
	id, cached := s.loadCache[loadKey]
	if !cached {
		data, err := os.ReadFile(filepath.Join(s.loadDir, loadKey+".json"))
		if err == nil {
			var entry struct {
				SnapshotID string `json:"snapshot_id"`
			}
			if json.Unmarshal(data, &entry) == nil && entry.SnapshotID != "" {
				id = entry.SnapshotID
				cached = true
				s.loadCache[loadKey] = id
			}
		}
	}
	s.mu.Unlock()
	if !cached || id == "" {
		return memory.Snapshot{}, false
	}
	return s.LoadSnapshot(id)
}

// ListLoadKeys returns the pinned load keys, sorted. Diagnostics / /memory.
func (s *Store) ListLoadKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.loadDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(out)
	return out
}

// Root returns the store root for diagnostics.
func (s *Store) Root() string { return s.root }

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
