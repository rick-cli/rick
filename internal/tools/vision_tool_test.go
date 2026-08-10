package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rick/internal/config"
)

func visionTestTool() VisionTool {
	enabled := true
	return VisionTool{Loaded: &config.Loaded{Config: config.Config{
		Vision: &config.VisionConfig{Enabled: &enabled, APIKey: "test-key"},
	}}}
}

func TestVisionToolRequiresPath(t *testing.T) {
	res, err := VisionTool{}.Run(context.Background(), Context{Cwd: "."}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Output, "path is required") {
		t.Fatalf("want path-required error, got %q", res.Output)
	}
}

func TestVisionToolRejectsMissingFile(t *testing.T) {
	res, err := VisionTool{}.Run(context.Background(), Context{Cwd: t.TempDir()},
		json.RawMessage(`{"path":"nope.png"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Output, "cannot stat") {
		t.Fatalf("want stat error, got %q", res.Output)
	}
}

func TestVisionToolRejectsNonImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := VisionTool{}.Run(context.Background(), Context{Cwd: dir},
		json.RawMessage(`{"path":"notes.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Output, "not a supported image") {
		t.Fatalf("want non-image error, got %q", res.Output)
	}
}

func TestVisionToolMissingKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(path, []byte("fake-png"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := VisionTool{Loaded: &config.Loaded{Config: config.Config{}}}.Run(
		context.Background(), Context{Cwd: dir}, json.RawMessage(`{"path":"shot.png"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Output, "/visionapi") {
		t.Fatalf("want missing-key error mentioning /visionapi, got %q", res.Output)
	}
}

func TestVisionToolResolvesRelativeToCwd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), []byte("fake-png"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A valid image with no key: the path resolution already happened, so the
	// error must be the missing key (not a stat error on a bogus path).
	res, err := VisionTool{Loaded: &config.Loaded{Config: config.Config{}}}.Run(
		context.Background(), Context{Cwd: dir}, json.RawMessage(`{"path":"shot.png"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "cannot stat") {
		t.Fatalf("path was not resolved against cwd: %q", res.Output)
	}
}
