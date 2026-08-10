package compress

import (
	"strings"
	"testing"
)

func TestGenericRemovesTerminalNoiseAndPreservesFailure(t *testing.T) {
	input := Input{
		Text:     "\x1b[2K\rprogress 10%\n\x1b[31mFAIL\x1b[0m: TestThing\n/path/file.go:12: expected value\nPASS\n",
		MaxBytes: 512,
	}
	result := Generic(input)
	if strings.Contains(result.Text, "progress 10%") {
		t.Fatalf("compressed output retained progress noise: %q", result.Text)
	}
	for _, required := range []string{"FAIL: TestThing", "/path/file.go:12: expected value"} {
		if !strings.Contains(result.Text, required) {
			t.Fatalf("compressed output lost %q: %q", required, result.Text)
		}
	}
}

func TestGenericAddsExplicitTruncationMarker(t *testing.T) {
	result := Generic(Input{Text: "first\nsecond\nthird\n", MaxBytes: 12})
	if !result.Truncated {
		t.Fatal("Generic() did not report truncation")
	}
	if !strings.Contains(result.Text, "output truncated") {
		t.Fatalf("missing truncation marker: %q", result.Text)
	}
}

func TestForToolCompactsGoSuccessChatterOnlyOnFailure(t *testing.T) {
	result := ForTool(Input{
		Command:  "go test ./...",
		IsError:  true,
		Text:     "ok example/a 0.01s\n/path/to/file.go:12:2: undefined: Missing\nFAIL\texample/b\t0.02s\n",
		MaxBytes: 4096,
	})
	if result.Stage != "go-diagnostics" {
		t.Fatalf("stage = %q, want go-diagnostics", result.Stage)
	}
	if strings.Contains(result.Text, "ok example/a") {
		t.Fatalf("successful package chatter was retained: %q", result.Text)
	}
	for _, required := range []string{"file.go:12:2", "undefined: Missing", "FAIL"} {
		if !strings.Contains(result.Text, required) {
			t.Fatalf("diagnostic lost %q: %q", required, result.Text)
		}
	}
}

func TestForToolPreservesGitDiffLinesAndReportsStage(t *testing.T) {
	result := ForTool(Input{
		Command:  "git diff",
		Text:     "diff --git a/main.go b/main.go\n@@ -1 +1 @@\n-old\n+new\n",
		MaxBytes: 4096,
	})
	if result.Stage != "git" || result.Fallback {
		t.Fatalf("unexpected git result metadata: %#v", result)
	}
	for _, required := range []string{"diff --git", "@@ -1 +1 @@", "-old", "+new"} {
		if !strings.Contains(result.Text, required) {
			t.Fatalf("git output lost %q: %q", required, result.Text)
		}
	}
}

func TestForToolUnknownCommandUsesGenericFallback(t *testing.T) {
	result := ForTool(Input{Command: "custom-tool", Text: "line\r\nline", MaxBytes: 4096})
	if !result.Fallback || result.Stage != "generic" {
		t.Fatalf("unexpected fallback metadata: %#v", result)
	}
	if result.Text != "line\nline" {
		t.Fatalf("generic fallback did not normalize safely: %q", result.Text)
	}
}

func TestForToolRoutesDedicatedToolsByName(t *testing.T) {
	// The dedicated git tool (not bash) must still get git compaction.
	git := ForTool(Input{
		Tool:     "git",
		Text:     "diff --git a/main.go b/main.go\n@@ -1 +1 @@\n-old\n+new\n",
		MaxBytes: 4096,
	})
	if git.Stage != "git" || git.Fallback {
		t.Fatalf("git tool not routed to git compactor: %#v", git)
	}

	// The dedicated grep tool must get search compaction (consecutive dups).
	search := ForTool(Input{
		Tool:     "grep",
		Text:     "a.go:1:x\na.go:1:x\nb.go:2:y\n",
		MaxBytes: 4096,
	})
	if search.Stage != "search" {
		t.Fatalf("grep tool not routed to search compactor: %#v", search)
	}
	if strings.Contains(search.Text, "a.go:1:x\na.go:1:x") {
		t.Fatalf("search compactor did not collapse duplicates: %q", search.Text)
	}

	// The dedicated test tool must get go-diagnostics compaction (success
	// chatter dropped only when a failure is present).
	diag := ForTool(Input{
		Tool:     "diagnostics",
		Text:     "ok  rick/internal/tools\nFAIL  rick/internal/agent\n# bad.go:12: error\n",
		MaxBytes: 4096,
		IsError:  true,
	})
	if diag.Stage != "go-diagnostics" {
		t.Fatalf("diagnostics tool not routed: %#v", diag)
	}
	if strings.Contains(diag.Text, "ok  rick/internal/tools") {
		t.Fatalf("go-diagnostics kept success chatter: %q", diag.Text)
	}
}
