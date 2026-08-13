package prefixstore

import (
	"path/filepath"
	"testing"

	"rick/internal/memory"
)

func TestStoreRoundTrip(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatal(err)
	}
	snap := memory.Derive([]memory.MessageLike{
		{Role: "user", Text: "build the harness"},
		{Role: "assistant", Text: "added cmd/cachehit"},
	}, memory.Options{})

	id, err := store.PutSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	if id != snap.ID {
		t.Fatalf("PutSnapshot id = %q, want %q", id, snap.ID)
	}
	got, ok := store.LoadSnapshot(id)
	if !ok || got.Text != snap.Text {
		t.Fatalf("LoadSnapshot mismatch: ok=%v", ok)
	}
}

func TestLoadKeyStableAcrossSessions(t *testing.T) {
	a := LoadKey("/repo", "deepseek/deepseek-v4", "main")
	b := LoadKey("/repo", "deepseek/deepseek-v4", "main")
	if a != b {
		t.Fatalf("same identity derived different load keys: %q vs %q", a, b)
	}
	c := LoadKey("/repo", "deepseek/deepseek-v4", "explore")
	if a == c {
		t.Fatal("distinct agent shared a load key")
	}
}

func TestPinAndLoadPinned(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatal(err)
	}
	snap := memory.Derive([]memory.MessageLike{{Role: "user", Text: "task"}}, memory.Options{})
	_, _ = store.PutSnapshot(snap)
	key := LoadKey("/repo", "model", "main")
	if err := store.Pin(key, snap.ID); err != nil {
		t.Fatal(err)
	}
	got, ok := store.LoadPinned(key)
	if !ok || got.ID != snap.ID {
		t.Fatalf("LoadPinned = ok:%v id:%q", ok, got.ID)
	}
	if keys := store.ListLoadKeys(); len(keys) != 1 || keys[0] != key {
		t.Fatalf("ListLoadKeys = %v", keys)
	}
}

func TestPinPersistsAcrossStores(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	store, _ := New(dir)
	snap := memory.Derive([]memory.MessageLike{{Role: "user", Text: "task"}}, memory.Options{})
	_, _ = store.PutSnapshot(snap)
	key := LoadKey("/repo", "model", "main")
	_ = store.Pin(key, snap.ID)

	// A fresh store over the same directory (a new process/session) still
	// sees the pin: this is the cross-session resume path.
	store2, _ := New(dir)
	got, ok := store2.LoadPinned(key)
	if !ok || got.ID != snap.ID {
		t.Fatalf("second store LoadPinned = ok:%v id:%q", ok, got.ID)
	}
}
