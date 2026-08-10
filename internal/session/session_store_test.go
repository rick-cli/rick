package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rick/internal/provider"
)

func TestListDoesNotReloadMetadataBackedSessionsAsLegacy(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	modern := &Session{
		ID:       "modern",
		Title:    "modern session",
		Cwd:      t.TempDir(),
		Messages: []provider.Message{provider.UserText("modern")},
	}
	if err := store.Save(modern); err != nil {
		t.Fatal(err)
	}

	legacy := &Session{
		ID:       "legacy",
		Title:    "legacy session",
		Cwd:      modern.Cwd,
		Messages: []provider.Message{provider.UserText("legacy")},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "legacy.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	metas, err := store.List(modern.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("got %d sessions, want modern plus legacy: %+v", len(metas), metas)
	}
	seen := make(map[string]int)
	for _, meta := range metas {
		seen[meta.ID]++
	}
	if seen["modern"] != 1 || seen["legacy"] != 1 {
		t.Fatalf("unexpected session counts: %+v", seen)
	}
}

// TestSearchUsesSidecar verifies Search matches message text via the
// lightweight .search.txt sidecar without needing the full session JSON, and
// falls back to Load() for legacy sessions saved before the sidecar existed.
func TestSearchUsesSidecar(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	modern := &Session{
		ID:    "modern",
		Title: "alpha",
		Cwd:   t.TempDir(),
		Messages: []provider.Message{
			provider.UserText("the quick brown fox"),
			provider.AssistantText("jumps over the lazy dog"),
		},
	}
	if err := store.Save(modern); err != nil {
		t.Fatal(err)
	}
	// The sidecar must exist and be lowercase.
	idxData, err := os.ReadFile(store.searchPath("modern"))
	if err != nil {
		t.Fatalf("search sidecar missing after Save: %v", err)
	}
	if !strings.Contains(string(idxData), "quick brown fox") {
		t.Fatalf("sidecar missing message text: %q", string(idxData))
	}

	// Title match via metadata.
	metas, err := store.Search("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].ID != "modern" {
		t.Fatalf("title search: got %+v", metas)
	}

	// Message-text match via the sidecar.
	metas, err = store.Search("LAZY DOG")
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].ID != "modern" {
		t.Fatalf("message search: got %+v", metas)
	}

	// No match.
	metas, err = store.Search("nonexistent-term")
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Fatalf("expected no matches, got %+v", metas)
	}
}

// TestSearchFallbackLegacyWithoutSidecar verifies Search still finds message
// text in a session written before the sidecar existed.
func TestSearchFallbackLegacyWithoutSidecar(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &Session{
		ID:       "legacy",
		Title:    "title-not-matching",
		Cwd:      t.TempDir(),
		Messages: []provider.Message{provider.UserText("needle-in-haystack")},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "legacy.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// No .search.txt sidecar, no .meta.json — the Load fallback must find it.
	metas, err := store.Search("needle")
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].ID != "legacy" {
		t.Fatalf("legacy search: got %+v", metas)
	}
}

// TestDeleteRemovesSearchSidecar verifies Delete cleans up the sidecar.
func TestDeleteRemovesSearchSidecar(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{
		ID:       "doomed",
		Title:    "temp",
		Cwd:      t.TempDir(),
		Messages: []provider.Message{provider.UserText("gone soon")},
	}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("doomed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.searchPath("doomed")); !os.IsNotExist(err) {
		t.Fatalf("search sidecar not removed after Delete: %v", err)
	}
}
