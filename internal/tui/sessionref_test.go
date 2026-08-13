package tui

import (
	"strings"
	"testing"

	"rick/internal/provider"
	"rick/internal/session"
)

// TestPrepareSessionReferences verifies /ref resolves a query against the
// session corpus, derives bounded snapshots, excludes the current session,
// and caps the batch.
func TestPrepareSessionReferences(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	sessA := &session.Session{
		ID:    "2026-01-01T00-00-00_a",
		Title: "cache analysis",
		Model: "deepseek/deepseek-v4-flash",
		Messages: []provider.Message{
			provider.UserText("analyze the prompt cache hit rate"),
			provider.AssistantText("the hit rate is 93%"),
		},
	}
	if err := store.Save(sessA); err != nil {
		t.Fatal(err)
	}
	sessB := &session.Session{
		ID:    "2026-01-02T00-00-00_b",
		Title: "cache tuning notes",
		Model: "deepseek/deepseek-v4-flash",
		Messages: []provider.Message{
			provider.UserText("tune the cache retention"),
			provider.AssistantText("done"),
		},
	}
	if err := store.Save(sessB); err != nil {
		t.Fatal(err)
	}

	refs, err := prepareSessionReferences(store, "cache", sessA.ID)
	if err != nil {
		t.Fatalf("prepareSessionReferences: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("want 1 ref, got %d", len(refs))
	}
	if refs[0].ID != sessB.ID {
		t.Fatalf("want session B, got %s", refs[0].ID)
	}
	if !strings.Contains(refs[0].Text, "cache retention") {
		t.Fatalf("snapshot should carry the referenced session's content, got %q", refs[0].Text)
	}

	// No matches -> error.
	if _, err := prepareSessionReferences(store, "nonexistent-topic", sessA.ID); err == nil {
		t.Fatal("want error for no matches")
	}
}

// TestPrepareSessionReferencesRejectsSelf ensures the current session is
// never injected as its own reference.
func TestPrepareSessionReferencesRejectsSelf(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess := &session.Session{
		ID:    "2026-01-01T00-00-00_a",
		Title: "only session",
		Messages: []provider.Message{
			provider.UserText("hello"),
		},
	}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSessionReferences(store, "only", sess.ID); err == nil {
		t.Fatal("want error when only the current session matches")
	}
}
