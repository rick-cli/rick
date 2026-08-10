package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ApplyPatchTool applies a structured multi-file patch in one shot.
//
// Format (V4A-style):
//
//	*** Begin Patch
//	*** Add File: path/to/new.go
//	+package main
//	+
//	*** Update File: path/to/existing.go
//	@@ optional context hint @@
//	 unchanged line
//	-removed line
//	+added line
//	*** Move File: old/path.go -> new/path.go
//	*** Delete File: path/to/dead.go
//	*** End Patch
type ApplyPatchTool struct{}

// Name implements Tool.
func (ApplyPatchTool) Name() string { return "apply_patch" }

// ReadOnly implements Tool.
func (ApplyPatchTool) ReadOnly() bool { return false }

// Description implements Tool.
func (ApplyPatchTool) Description() string {
	return "Apply a structured multi-file patch. Use this when a single logical " +
		"change spans several files; use 'edit' for one-off replacements.\n\n" +
		"Format:\n" +
		"*** Begin Patch\n" +
		"*** Add File: path/new.go\n" +
		"+line one\n" +
		"*** Update File: path/existing.go\n" +
		"@@ context hint @@\n" +
		" unchanged\n-removed\n+added\n" +
		"*** Move File: a.go -> b.go\n" +
		"*** Delete File: gone.go\n" +
		"*** End Patch\n\n" +
		"Context lines start with a space, removals with '-', additions with '+'."
}

// Schema implements Tool.
func (ApplyPatchTool) Schema() map[string]any {
	return obj(map[string]any{
		"patch": strProp("The full patch text, including the Begin/End Patch markers."),
	}, "patch")
}

type applyPatchArgs struct {
	Patch string `json:"patch"`
}

type patchAction int

const (
	actionAdd patchAction = iota
	actionUpdate
	actionDelete
	actionMove
)

type patchFile struct {
	action patchAction
	path   string
	dest   string   // move target
	lines  []string // raw body lines for add/update
}

// Run implements Tool.
func (ApplyPatchTool) Run(_ context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a applyPatchArgs
	if err := RepairDecode(in, &a, ApplyPatchTool{}.Schema(), tc.Repair); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	files, err := ParsePatch(a.Patch)
	if err != nil {
		return Errf("%v", err), nil
	}
	if len(files) == 0 {
		return Errf("patch contained no file sections"), nil
	}
	fileWriteMu.Lock()
	defer fileWriteMu.Unlock()

	// Stage everything, then commit; a failure leaves the tree untouched.
	type staged struct {
		path, dest string
		action     patchAction
		old, new   string
	}
	var plan []staged

	for _, f := range files {
		abs := resolvePath(tc.Cwd, f.path)
		switch f.action {
		case actionAdd:
			if _, err := os.Stat(abs); err == nil {
				return Errf("apply_patch: %s already exists (use Update File)", f.path), nil
			}
			var b strings.Builder
			for _, l := range f.lines {
				if strings.HasPrefix(l, "+") {
					b.WriteString(l[1:])
				} else {
					b.WriteString(l)
				}
				b.WriteByte('\n')
			}
			plan = append(plan, staged{path: abs, action: actionAdd, new: b.String()})

		case actionDelete:
			raw, err := os.ReadFile(abs)
			if err != nil {
				return Errf("apply_patch: cannot delete %s: %v", f.path, err), nil
			}
			if !wasRead(abs) {
				return Errf("apply_patch: refusing to delete %s: read it first", f.path), nil
			}
			plan = append(plan, staged{path: abs, action: actionDelete, old: string(raw)})

		case actionMove:
			raw, err := os.ReadFile(abs)
			if err != nil {
				return Errf("apply_patch: cannot move %s: %v", f.path, err), nil
			}
			if !wasRead(abs) {
				return Errf("apply_patch: refusing to move %s: read it first", f.path), nil
			}
			content := string(raw)
			if len(f.lines) > 0 {
				content, err = applyHunks(content, f.lines)
				if err != nil {
					return Errf("apply_patch: %s: %v", f.path, err), nil
				}
			}
			plan = append(plan, staged{
				path: abs, dest: resolvePath(tc.Cwd, f.dest),
				action: actionMove, old: string(raw), new: content,
			})

		case actionUpdate:
			raw, err := os.ReadFile(abs)
			if err != nil {
				return Errf("apply_patch: cannot read %s: %v", f.path, err), nil
			}
			if !wasRead(abs) {
				return Errf("apply_patch: refusing to update %s: read it first", f.path), nil
			}
			content, err := applyHunks(string(raw), f.lines)
			if err != nil {
				return Errf("apply_patch: %s: %v", f.path, err), nil
			}
			plan = append(plan, staged{path: abs, action: actionUpdate, old: string(raw), new: content})
		}
	}

	var summary strings.Builder
	changes := make([]map[string]any, 0, len(plan))
	for _, s := range plan {
		switch s.action {
		case actionAdd:
			if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
				return Errf("%v", err), nil
			}
			if err := os.WriteFile(s.path, []byte(s.new), 0o644); err != nil {
				return Errf("%v", err), nil
			}
			markRead(s.path)
			fmt.Fprintf(&summary, "A  %s\n", relTo(tc.Cwd, s.path))
		case actionUpdate:
			if err := os.WriteFile(s.path, []byte(s.new), 0o644); err != nil {
				return Errf("%v", err), nil
			}
			markRead(s.path)
			add, del := DiffStat(s.old, s.new)
			fmt.Fprintf(&summary, "M  %s  +%d -%d\n", relTo(tc.Cwd, s.path), add, del)
		case actionDelete:
			if err := os.Remove(s.path); err != nil {
				return Errf("%v", err), nil
			}
			fmt.Fprintf(&summary, "D  %s\n", relTo(tc.Cwd, s.path))
		case actionMove:
			if err := os.MkdirAll(filepath.Dir(s.dest), 0o755); err != nil {
				return Errf("%v", err), nil
			}
			if err := os.WriteFile(s.dest, []byte(s.new), 0o644); err != nil {
				return Errf("%v", err), nil
			}
			if err := os.Remove(s.path); err != nil {
				return Errf("%v", err), nil
			}
			markRead(s.dest)
			fmt.Fprintf(&summary, "R  %s -> %s\n", relTo(tc.Cwd, s.path), relTo(tc.Cwd, s.dest))
		}
		changes = append(changes, map[string]any{
			"path": s.path, "dest": s.dest, "old": s.old, "new": s.new, "action": int(s.action),
		})
	}

	var note string
	if tc.Repair != nil && tc.Repair.Note != nil {
		note = *tc.Repair.Note
	}
	return repairNote(Result{
		Output: fmt.Sprintf("applied patch to %d file(s):\n%s", len(plan), summary.String()),
		Title:  fmt.Sprintf("apply_patch (%d files)", len(plan)),
		Meta:   map[string]any{"changes": changes},
	}, note), nil
}

// ParsePatch splits a patch document into per-file sections.
func ParsePatch(patch string) ([]patchFile, error) {
	patch = strings.ReplaceAll(patch, "\r\n", "\n")
	lines := strings.Split(patch, "\n")

	started := false
	var files []patchFile
	var cur *patchFile

	flush := func() {
		if cur != nil {
			files = append(files, *cur)
			cur = nil
		}
	}

	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(trimmed, "*** Begin Patch"):
			started = true
		case strings.HasPrefix(trimmed, "*** End Patch"):
			flush()
			started = false
		case strings.HasPrefix(trimmed, "*** Add File:"):
			flush()
			cur = &patchFile{action: actionAdd, path: strings.TrimSpace(trimmed[len("*** Add File:"):])}
		case strings.HasPrefix(trimmed, "*** Update File:"):
			flush()
			cur = &patchFile{action: actionUpdate, path: strings.TrimSpace(trimmed[len("*** Update File:"):])}
		case strings.HasPrefix(trimmed, "*** Delete File:"):
			flush()
			cur = &patchFile{action: actionDelete, path: strings.TrimSpace(trimmed[len("*** Delete File:"):])}
			flush()
		case strings.HasPrefix(trimmed, "*** Move File:"):
			flush()
			spec := strings.TrimSpace(trimmed[len("*** Move File:"):])
			parts := strings.SplitN(spec, "->", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("malformed Move File line: %s", trimmed)
			}
			cur = &patchFile{action: actionMove,
				path: strings.TrimSpace(parts[0]), dest: strings.TrimSpace(parts[1])}
		case strings.HasPrefix(trimmed, "*** "):
			return nil, fmt.Errorf("unknown patch directive: %s", trimmed)
		default:
			if cur != nil {
				cur.lines = append(cur.lines, l)
			}
		}
	}
	flush()
	if !started && len(files) == 0 {
		return nil, fmt.Errorf("patch must start with '*** Begin Patch'")
	}
	return files, nil
}

// PatchPaths returns every source and destination path touched by a patch.
func PatchPaths(patch string) ([]string, error) {
	files, err := ParsePatch(patch)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(files)*2)
	for _, file := range files {
		paths = append(paths, file.path)
		if file.dest != "" {
			paths = append(paths, file.dest)
		}
	}
	return paths, nil
}

// applyHunks applies context/-/+ hunks to content.
func applyHunks(content string, body []string) (string, error) {
	crlf := strings.Contains(content, "\r\n")
	norm := strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(norm, "\n")

	type hunk struct {
		hint    string
		context []string // lines expected in the original (context + removals)
		result  []string // lines to write (context + additions)
	}
	var hunks []hunk
	var cur *hunk
	push := func() {
		if cur != nil && (len(cur.context) > 0 || len(cur.result) > 0) {
			hunks = append(hunks, *cur)
		}
		cur = nil
	}
	for _, l := range body {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "@@") {
			push()
			cur = &hunk{hint: strings.Trim(strings.TrimPrefix(t, "@@"), " @")}
			continue
		}
		if cur == nil {
			cur = &hunk{}
		}
		switch {
		case strings.HasPrefix(l, "+"):
			cur.result = append(cur.result, l[1:])
		case strings.HasPrefix(l, "-"):
			cur.context = append(cur.context, l[1:])
		case strings.HasPrefix(l, " "):
			cur.context = append(cur.context, l[1:])
			cur.result = append(cur.result, l[1:])
		case l == "":
			cur.context = append(cur.context, "")
			cur.result = append(cur.result, "")
		default:
			// Tolerate un-prefixed context lines.
			cur.context = append(cur.context, l)
			cur.result = append(cur.result, l)
		}
	}
	push()

	if len(hunks) == 0 {
		return content, nil
	}

	searchFrom := 0
	for hi, h := range hunks {
		// Drop trailing blank artefacts that models often emit.
		ctx := trimTrailingBlank(h.context)
		res := trimTrailingBlank(h.result)
		if len(ctx) == 0 {
			// Pure insertion with no anchor: append at end.
			lines = append(lines, res...)
			continue
		}
		idx := indexSlice(lines, ctx, searchFrom)
		if idx < 0 {
			idx = indexSliceTrimmed(lines, ctx, searchFrom)
		}
		if idx < 0 {
			return "", fmt.Errorf("hunk %d does not match the file (hint: %q); re-read the file", hi+1, h.hint)
		}
		out := make([]string, 0, len(lines)-len(ctx)+len(res))
		out = append(out, lines[:idx]...)
		out = append(out, res...)
		out = append(out, lines[idx+len(ctx):]...)
		lines = out
		searchFrom = idx + len(res)
	}

	joined := strings.Join(lines, "\n")
	if crlf {
		joined = strings.ReplaceAll(joined, "\n", "\r\n")
	}
	return joined, nil
}

func trimTrailingBlank(s []string) []string {
	for len(s) > 0 && strings.TrimSpace(s[len(s)-1]) == "" {
		s = s[:len(s)-1]
	}
	return s
}

func indexSlice(hay, needle []string, from int) int {
	if len(needle) == 0 || from < 0 {
		return -1
	}
	for i := from; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func indexSliceTrimmed(hay, needle []string, from int) int {
	if len(needle) == 0 || from < 0 {
		return -1
	}
	for i := from; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if strings.TrimRight(hay[i+j], " \t") != strings.TrimRight(needle[j], " \t") {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
