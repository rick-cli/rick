package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeRipgrep returns a path to a script that behaves like `rg --files` for
// the glob tool: exit 1 (no matches) or exit 2 (real failure), optionally
// printing a match line first.
func fakeRipgrep(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	var script string
	switch mode {
	case "nomatch":
		script = "#!/bin/sh\nexit 1\n"
	case "fail":
		script = "#!/bin/sh\necho 'rg: permission denied' >&2\nexit 2\n"
	case "match":
		script = "#!/bin/sh\necho 'somefile.txt'\nexit 0\n"
	}
	path := filepath.Join(dir, "fake-rg")
	if runtime.GOOS == "windows" {
		path += ".cmd"
		if err := os.WriteFile(path, []byte("@echo off\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestGlobRipgrepNoMatchIsNotError(t *testing.T) {
	old := ripgrepPath
	ripgrepPath = fakeRipgrep(t, "nomatch")
	defer func() { ripgrepPath = old }()

	tool := GlobTool{}
	in, _ := json.Marshal(map[string]string{"pattern": "*.go"})
	res, err := tool.Run(context.Background(), Context{Cwd: t.TempDir()}, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("no-match exit 1 should not be an error, got: %s", res.Output)
	}
	if res.Output != "no files matched *.go" {
		t.Fatalf("unexpected output: %q", res.Output)
	}
}

func TestGlobRipgrepRealFailureIsError(t *testing.T) {
	old := ripgrepPath
	ripgrepPath = fakeRipgrep(t, "fail")
	defer func() { ripgrepPath = old }()

	tool := GlobTool{}
	in, _ := json.Marshal(map[string]string{"pattern": "*.go"})
	res, err := tool.Run(context.Background(), Context{Cwd: t.TempDir()}, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("exit 2 should surface as an error, got: %s", res.Output)
	}
}

func TestGlobRipgrepMatch(t *testing.T) {
	old := ripgrepPath
	ripgrepPath = fakeRipgrep(t, "match")
	defer func() { ripgrepPath = old }()

	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "somefile.txt"), []byte("x"), 0o644)
	tool := GlobTool{}
	in, _ := json.Marshal(map[string]string{"pattern": "*.txt"})
	res, err := tool.Run(context.Background(), Context{Cwd: dir}, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
}
