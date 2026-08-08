package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsWellKnownUserDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	if !isWellKnownUserDir(filepath.Join(home, "Downloads")) {
		t.Error("Downloads must be refused")
	}
	if !isWellKnownUserDir(home) {
		t.Error("the home dir itself must be refused")
	}
	// Case-insensitivity on Windows, exact elsewhere.
	if !isWellKnownUserDir(filepath.Join(home, "DESKTOP")) {
		t.Error("user dirs must be matched case-insensitively")
	}
	if isWellKnownUserDir(filepath.Join(home, "Downloads", "myproj")) {
		t.Error("a project inside a user folder must still be snapshottable")
	}
	if isWellKnownUserDir(t.TempDir()) {
		t.Error("an ordinary project dir must not be refused")
	}
}

func TestNewSnapshotterRefusesWellKnownUserDirs(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	dataDir := t.TempDir()
	snaps, err := NewSnapshotter(filepath.Join(home, "Downloads"), dataDir)
	if err == nil {
		t.Fatal("expected an error for Downloads")
	}
	if snaps.Enabled() {
		t.Fatal("snapshotter must stay disabled for a well-known user dir")
	}
}
