package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteRefusesPartialRead verifies the partial-view ledger: a windowed
// read must not permit a whole-file overwrite (the unseen tail would be
// clobbered), while a full read does.
func TestWriteRefusesPartialRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.go")
	content := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Windowed read: only lines 1-2 delivered.
	res, err := (ReadTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]any{"path": "big.go", "offset": 1, "limit": 2}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.IsError {
		t.Fatalf("window read errored: %q", res.Output)
	}
	if !strings.Contains(res.Output, "1|line1") {
		t.Fatalf("window read output unexpected: %q", res.Output)
	}

	// Overwrite must be refused — the model never saw lines 3-5.
	wres, err := (WriteTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]string{"path": "big.go", "content": "replacement"}))
	if err != nil {
		t.Fatalf("write returned error: %v", err)
	}
	if !wres.IsError || !strings.Contains(wres.Output, "only lines 1-2 of 5 were read") {
		t.Fatalf("expected partial-read refusal, got: %q", wres.Output)
	}
	// File untouched.
	if b, _ := os.ReadFile(path); string(b) != content {
		t.Fatalf("file was modified despite refusal: %q", string(b))
	}

	// Full read then overwrite succeeds.
	if _, err := (ReadTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]string{"path": "big.go"})); err != nil {
		t.Fatalf("full read: %v", err)
	}
	wres, err = (WriteTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]string{"path": "big.go", "content": "replacement"}))
	if err != nil {
		t.Fatalf("write after full read: %v", err)
	}
	if wres.IsError {
		t.Fatalf("full-read overwrite refused: %q", wres.Output)
	}
}

// TestEditAllowsPartialRead verifies surgical edits stay permissive after a
// windowed read (exact-match edits cannot corrupt unseen lines).
func TestEditAllowsPartialRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "code.go")
	if err := os.WriteFile(path, []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (ReadTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]any{"path": "code.go", "offset": 1, "limit": 2})); err != nil {
		t.Fatal(err)
	}
	res, err := (EditTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]string{"path": "code.go", "old_string": "line1", "new_string": "LINE1"}))
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if res.IsError {
		t.Fatalf("surgical edit after partial read refused: %q", res.Output)
	}
}

// TestReadStubDedup verifies consume-on-hit: the first unchanged re-read
// returns a stub, the next returns full content again.
func TestReadStubDedup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stable.go")
	if err := os.WriteFile(path, []byte("func main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// First read: full content.
	first, err := (ReadTool{}).Run(ctx, Context{Cwd: dir}, jsonArgs(map[string]string{"path": "stable.go"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Output, "func main() {}") {
		t.Fatalf("first read unexpected: %q", first.Output)
	}

	// Second read (unchanged): stub, not content.
	second, err := (ReadTool{}).Run(ctx, Context{Cwd: dir}, jsonArgs(map[string]string{"path": "stable.go"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Output, "<unchanged:") {
		t.Fatalf("second read should be a stub, got: %q", second.Output)
	}

	// Third read (memo consumed): full content again.
	third, err := (ReadTool{}).Run(ctx, Context{Cwd: dir}, jsonArgs(map[string]string{"path": "stable.go"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(third.Output, "func main() {}") {
		t.Fatalf("third read should be full content, got: %q", third.Output)
	}
}

// TestSuggestSimilarLevenshtein verifies AGENT.md -> AGENTS.md is caught by
// the bounded edit distance where substring matching finds nothing.
func TestSuggestSimilarLevenshtein(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sug := suggestSimilar(filepath.Join(dir, "AGENT.md"))
	if !strings.Contains(sug, "AGENTS.md") {
		t.Fatalf("expected AGENTS.md suggestion, got %q", sug)
	}
}

// TestSuggestSimilarUnicodeNormalization verifies an NFD-typed filename is
// matched against the NFC form on disk (macOS screenshot case).
func TestSuggestSimilarUnicodeNormalization(t *testing.T) {
	dir := t.TempDir()
	// NFC form: é as a single code point.
	nfcName := "Screenshot 2026-08-09 at 10.30.00 é.png"
	if err := os.WriteFile(filepath.Join(dir, nfcName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// NFD form: e + combining acute.
	nfdName := "Screenshot 2026-08-09 at 10.30.00 e\u0301.png"
	sug := suggestSimilar(filepath.Join(dir, nfdName))
	if !strings.Contains(sug, nfcName) {
		t.Fatalf("expected NFC suggestion for NFD input, got %q", sug)
	}
}

// TestRenderNotebook verifies cells are tagged, base64 images are stripped,
// and oversized outputs become pointers.
func TestRenderNotebook(t *testing.T) {
	dir := t.TempDir()
	longDump := "a very long dataframe dump " + strings.Repeat("x", 12000)
	nb := fmt.Sprintf(`{
  "nbformat": 4,
  "cells": [
    {"cell_type": "markdown", "source": ["# Title\n", "intro"]},
    {"cell_type": "code", "execution_count": 1, "source": ["print(1)"],
     "outputs": [
       {"output_type": "stream", "name": "stdout", "text": ["1\n"]},
       {"output_type": "display_data", "data": {
          "image/png": "iVBORw0KGgoAAAANSUhEUgAAAA==",
          "text/plain": ["<Figure size 640x480>"]
       }}
     ]},
    {"cell_type": "code", "execution_count": 2, "source": ["df.head()"],
     "outputs": [{"output_type": "execute_result", "execution_count": 2,
       "data": {"text/plain": ["%s"]}}]}
  ]
}`, longDump)
	path := filepath.Join(dir, "analysis.ipynb")
	if err := os.WriteFile(path, []byte(nb), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := (ReadTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]string{"path": "analysis.ipynb"}))
	if err != nil {
		t.Fatalf("read notebook: %v", err)
	}
	if !strings.Contains(res.Output, "markdown cell 1") {
		t.Fatalf("missing markdown header: %q", res.Output)
	}
	if !strings.Contains(res.Output, "code cell 2") {
		t.Fatalf("missing code header: %q", res.Output)
	}
	if strings.Contains(res.Output, "iVBORw0KGgo") {
		t.Fatalf("base64 image leaked into context: %q", res.Output)
	}
	if !strings.Contains(res.Output, "[image/plot omitted") {
		t.Fatalf("missing image-omitted note: %q", res.Output)
	}
	if !strings.Contains(res.Output, "more chars of output omitted") {
		t.Fatalf("oversized output not capped: %q", res.Output)
	}
	if strings.Contains(res.Output, strings.Repeat("x", 12000)) {
		t.Fatalf("oversized output leaked in full")
	}
}

// TestRenderNotebookNonNotebookFallback verifies a .json file that is not a
// notebook still reads as plain text.
func TestRenderNotebookNonNotebookFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")
	content := `{"name": "not a notebook", "values": [1, 2, 3]}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := (ReadTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]string{"path": "data.json"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("plain JSON read errored: %q", res.Output)
	}
	if !strings.Contains(res.Output, `"name"`) {
		t.Fatalf("expected plain JSON content, got: %q", res.Output)
	}
}

// TestEditOutputIsCompact verifies the edit tool echoes a short change
// summary + tiny snippet instead of a full unified diff (the model already
// knows old_string/new_string from its own call).
func TestEditOutputIsCompact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.go")
	content := strings.Repeat("unchanged line\n", 200) + "target\n" + strings.Repeat("trailing line\n", 200)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (ReadTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]string{"path": "edit.go"})); err != nil {
		t.Fatal(err)
	}
	res, err := (EditTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]string{"path": "edit.go", "old_string": "target", "new_string": "TARGET"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("edit errored: %q", res.Output)
	}
	// The output must be a compact summary, not the 400-line unified diff.
	if len(res.Output) > 4<<10 {
		t.Fatalf("edit output too large (%d bytes): %q", len(res.Output), res.Output)
	}
	if !strings.Contains(res.Output, "+1 -1") {
		t.Fatalf("edit output lacks change stats: %q", res.Output)
	}
	if !strings.Contains(res.Output, "TARGET") {
		t.Fatalf("edit output lacks the changed region: %q", res.Output)
	}
	// The full old/new content must still ride in Meta for the TUI.
	if res.Meta["old"] != content || res.Meta["new"] != strings.Replace(content, "target", "TARGET", 1) {
		t.Fatalf("edit Meta lacks full old/new content")
	}
}

// TestCapSnippet verifies long search snippets are bounded.
func TestCapSnippet(t *testing.T) {
	short := "short snippet"
	if got := capSnippet(short); got != short {
		t.Fatalf("short snippet changed: %q", got)
	}
	long := strings.Repeat("é", 500) // 500 runes, > 300
	got := capSnippet(long)
	if got != string([]rune(long)[:300])+" …" {
		t.Fatalf("long snippet not capped to 300 runes: len=%d", len(got))
	}
}

// TestClampedReadRefusesOverwrite verifies a read that byte-clamped a line is
// never treated as a full read: the clamp note names the line and its
// recovery, and a whole-file overwrite is refused (the model never saw the
// line's full content), while a surgical edit stays allowed.
func TestClampedReadRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minified.js")
	content := "let a='" + strings.Repeat("x", 100_000) + "';"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Full read: the giant line is clamped and the recovery note names it.
	res, err := (ReadTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]string{"path": "minified.js"}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.IsError {
		t.Fatalf("read errored: %q", res.Output)
	}
	if !strings.Contains(res.Output, "…<truncated>") {
		t.Fatalf("clamped line missing marker: %q", res.Output)
	}
	if !strings.Contains(res.Output, "was clamped to 2000 chars") {
		t.Fatalf("missing clamp recovery note: %q", res.Output)
	}

	// Whole-file overwrite must be refused — the model never saw the line.
	wres, err := (WriteTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]string{"path": "minified.js", "content": "replacement"}))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !wres.IsError || !strings.Contains(wres.Output, "too long to deliver in full") {
		t.Fatalf("expected clamp refusal, got: %q", wres.Output)
	}
	if b, _ := os.ReadFile(path); string(b) != content {
		t.Fatalf("file was modified despite refusal")
	}

	// Surgical edit after a clamped read stays permissive.
	eres, err := (EditTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]string{"path": "minified.js", "old_string": "let a='", "new_string": "const a='"}))
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if eres.IsError {
		t.Fatalf("surgical edit after clamped read refused: %q", eres.Output)
	}
}

// TestByteTruncatedResumeOffset verifies the byte-truncation marker names the
// correct resume offset: the first line NOT delivered (every delivered line
// is complete), so a follow-up read from that offset neither re-reads a line
// nor skips one.
func TestByteTruncatedResumeOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wide.txt")
	var lines []string
	for i := 1; i <= 50; i++ {
		lines = append(lines, fmt.Sprintf("line-%02d %s", i, strings.Repeat("x", 40)))
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := (ReadTool{MaxBytes: 512}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]string{"path": "wide.txt"}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.IsError {
		t.Fatalf("read errored: %q", res.Output)
	}
	marker := "continue with offset="
	idx := strings.Index(res.Output, marker)
	if idx < 0 {
		t.Fatalf("missing truncation marker: %q", res.Output)
	}
	rest := res.Output[idx+len(marker):]
	endPos := strings.IndexAny(rest, ">\n")
	if endPos < 0 {
		t.Fatalf("malformed truncation marker: %q", res.Output)
	}
	var resume int
	if _, err := fmt.Sscanf(rest[:endPos], "%d", &resume); err != nil {
		t.Fatalf("unparseable resume offset %q: %v", rest[:endPos], err)
	}

	// The resume offset must be exactly one past the last fully delivered line.
	lastLine := 0
	for _, l := range strings.Split(res.Output, "\n") {
		if n, ok := parseReadLineNumber(l); ok && n > lastLine {
			lastLine = n
		}
	}
	if resume != lastLine+1 {
		t.Fatalf("resume offset = %d, want last delivered line %d + 1", resume, lastLine)
	}

	// Resuming there must not duplicate or skip any line.
	res2, err := (ReadTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]any{"path": "wide.txt", "offset": resume}))
	if err != nil {
		t.Fatalf("resume read: %v", err)
	}
	first := 0
	for _, l := range strings.Split(res2.Output, "\n") {
		if n, ok := parseReadLineNumber(l); ok {
			first = n
			break
		}
	}
	if first != resume {
		t.Fatalf("resume read starts at line %d, want %d (duplicate or gap)", first, resume)
	}
}

// TestWriteRefusesSameMtimeExternalEdit verifies the ledger's size gate: an
// external edit that restores the original mtime (timestamp-preserving sync,
// coarse filesystem granularity) but changes the size still trips wasRead, so
// write refuses instead of clobbering a file the model's view no longer
// matches.
func TestWriteRefusesSameMtimeExternalEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "guarded.txt")
	content := "alpha\nbeta\ngamma\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mt := st.ModTime()

	if _, err := (ReadTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]string{"path": "guarded.txt"})); err != nil {
		t.Fatal(err)
	}

	// External edit that restores the original mtime; size differs.
	edited := "alpha\nbeta\ngamma\ndelta\n"
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}

	wres, err := (WriteTool{}).Run(context.Background(), Context{Cwd: dir},
		jsonArgs(map[string]string{"path": "guarded.txt", "content": "overwrite"}))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !wres.IsError || !strings.Contains(wres.Output, "read it first") {
		t.Fatalf("expected same-mtime edit refusal, got: %q", wres.Output)
	}
	if b, _ := os.ReadFile(path); string(b) != edited {
		t.Fatalf("file was modified despite refusal")
	}
}

// parseReadLineNumber extracts the line number from a "N|..." read line; ok is
// false for footer/marker lines.
func parseReadLineNumber(line string) (int, bool) {
	idx := strings.Index(line, "|")
	if idx <= 0 {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(line[:idx], "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}
