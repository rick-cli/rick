package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"rick/internal/delta"
	"rick/pkg/skeleton"
)

// ---------- shared file-state tracking ----------
//
// The model must read a file before editing it. We track read times so edit
// and write can refuse to clobber a file that changed underneath us.

var fileState struct {
	sync.Mutex
	reads map[string]readRecord // abs path -> coverage of the last delivered read
}

// readRecord remembers what the model actually saw for a file at the time it
// was read, keyed by the mtime observed. A partial read (offset/limit window
// or a byte-truncated view) records only the delivered line range, so a
// whole-file overwrite can refuse to clobber lines the model never saw.
// maxReadLineBytes clamps any single delivered line: a minified bundle must
// not eat the whole byte budget, and a clamped line is marked so the ledger
// knows the model never saw its full content.
const maxReadLineBytes = 2000

type readRecord struct {
	mtime   int64
	size    int64 // file size at read time; gates against same-mtime external edits
	start   int   // 1-indexed first delivered line; 0 when unset
	end     int   // last delivered line; 0 when unset
	total   int   // total lines at read time
	full    bool
	clamped bool // a delivered line was byte-clamped, so full is never true
}

var fileWriteMu sync.Mutex

// deltaStore is the shared per-session delta baseline store for ReadTool. It
// is reset whenever read tracking is reset so a new session never emits a
// delta against a stale baseline.
var deltaStore = delta.NewStore()

func init() { fileState.reads = map[string]readRecord{} }

// DeltaStore returns the shared delta baseline store used by ReadTool.
func DeltaStore() *delta.Store { return deltaStore }

// markReadRange records that the model was delivered lines start..end of a
// total-line file (1-indexed, inclusive) at the current mtime/size. A window
// read leaves full=false so a later whole-file write can detect the unseen
// tail; a clamped read (any delivered line byte-clamped) also leaves
// full=false because the model never saw that line's complete content.
func markReadRange(path string, start, end, total int, clamped bool) {
	if start < 1 {
		start = 1
	}
	if end < start {
		end = start
	}
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	fileState.Lock()
	fileState.reads[path] = readRecord{
		mtime: st.ModTime().UnixNano(),
		size:  st.Size(),
		start: start, end: end, total: total,
		full:    !clamped && start == 1 && end >= total,
		clamped: clamped,
	}
	fileState.Unlock()
}

// markRead records a whole-file view (used by skeleton, delta, and after a
// write) — the model saw enough to consider the whole file covered.
func markRead(path string) {
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	fileState.Lock()
	rec := fileState.reads[path]
	if rec.mtime == st.ModTime().UnixNano() {
		rec.size = st.Size()
		rec.full = true
		if rec.start == 0 {
			rec.start, rec.end = 1, 1
		}
		fileState.reads[path] = rec
		fileState.Unlock()
		return
	}
	fileState.reads[path] = readRecord{
		mtime: st.ModTime().UnixNano(), size: st.Size(), start: 1, end: 1, total: 1, full: true,
	}
	fileState.Unlock()
}

// wasRead reports whether the model has seen the file at its current mtime
// and size (full or partial). The size check catches same-mtime external
// edits (coarse timestamp granularity, timestamp-preserving sync tools) that
// would otherwise let edit/write clobber a file that changed underneath us.
// It is the permissive gate for surgical edits.
func wasRead(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	fileState.Lock()
	defer fileState.Unlock()
	rec, ok := fileState.reads[path]
	return ok && rec.mtime == st.ModTime().UnixNano() && rec.size == st.Size() && rec.end >= rec.start
}

// wasFullyRead reports whether the model saw the whole file at its current
// mtime and size — the strict gate for whole-file overwrites (write).
func wasFullyRead(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	fileState.Lock()
	defer fileState.Unlock()
	rec, ok := fileState.reads[path]
	return ok && rec.mtime == st.ModTime().UnixNano() && rec.size == st.Size() && rec.full
}

// readCoverage returns the delivered line range for the current mtime, for
// error messages that tell the model which lines were unseen.
func readCoverage(path string) (start, end, total int, ok bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, 0, 0, false
	}
	fileState.Lock()
	defer fileState.Unlock()
	rec, ok := fileState.reads[path]
	if !ok || rec.mtime != st.ModTime().UnixNano() || rec.size != st.Size() {
		return 0, 0, 0, false
	}
	return rec.start, rec.end, rec.total, true
}

// wasClampedRead reports whether the model's last view of the file delivered
// a byte-clamped line. Such a view covers every line number but not every
// byte, so a whole-file overwrite would still destroy content the model
// never saw.
func wasClampedRead(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	fileState.Lock()
	defer fileState.Unlock()
	rec, ok := fileState.reads[path]
	return ok && rec.mtime == st.ModTime().UnixNano() && rec.size == st.Size() && rec.clamped
}

// ResetFileState clears read tracking and delta baselines (new session).
func ResetFileState() {
	fileState.Lock()
	fileState.reads = map[string]readRecord{}
	fileState.Unlock()
	deltaStore.Reset()
}

func resolvePath(cwd, p string) string {
	p = unwrapMarkdownLink(p)
	if p == "" {
		return cwd
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(cwd, p))
}

func relTo(base, p string) string {
	if r, err := filepath.Rel(base, p); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return filepath.ToSlash(p)
}

func isBinary(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	chunk := b[:n]
	if bytesIndexZero(chunk) {
		return true
	}
	return !utf8.Valid(chunk) && n > 64
}

func bytesIndexZero(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

// isNotebook reports whether a path/prefix pair is a Jupyter notebook: the
// .ipynb extension, or content that parses as notebook JSON (a leading '{'
// with a "cells" or "nbformat" key). Content sniffing means a notebook with
// a wrong extension is still rendered, and a renamed .ipynb is not skipped.
func isNotebook(path string, prefix []byte) bool {
	if strings.EqualFold(filepath.Ext(path), ".ipynb") {
		return true
	}
	head := strings.TrimSpace(string(prefix))
	if !strings.HasPrefix(head, "{") {
		return false
	}
	// Cheap structural probe before a full parse: a notebook always has
	// "cells" or "nbformat" near the top.
	probe := head
	if len(probe) > 4096 {
		probe = probe[:4096]
	}
	return strings.Contains(probe, `"cells"`) || strings.Contains(probe, `"nbformat"`)
}

// ---------- read ----------

// ReadTool reads a file with line numbers and pagination. When Delta is set,
// repeat reads of a changed file return a token-level delta view instead of
// the whole file. When EnableSkeleton is set and a target symbol is named on
// a whole-file read, the AST skeleton with that one body expanded is returned.
type ReadTool struct {
	MaxBytes       int
	MaxInputBytes  int
	Delta          *delta.Store
	EnableSkeleton bool
}

// Name implements Tool.
func (ReadTool) Name() string { return "read" }

// ReadOnly implements Tool.
func (ReadTool) ReadOnly() bool { return true }

// Description implements Tool.
func (ReadTool) Description() string {
	return "Read a file from the filesystem. Returns contents with 1-indexed line numbers " +
		"in the form 'N|line'. Use offset/limit for large files. Always read a file " +
		"before editing it. Prefer this over 'cat' via bash. Pass target to get an " +
		"AST skeleton with only that symbol expanded, or full:true to force the " +
		"complete file (repeat reads of changed files return a delta view; unchanged " +
		"re-reads return a stub). Jupyter notebooks are rendered cell-by-cell."
}

// Schema implements Tool.
func (ReadTool) Schema() map[string]any {
	return obj(map[string]any{
		"path":   pathProp("File path (absolute, or relative to the project root)."),
		"offset": numProp("1-indexed line to start from. Default 1."),
		"limit":  numProp("Maximum number of lines to read. Default 2000."),
		"target": strProp("Optional symbol name: return an AST skeleton with signatures of " +
			"every top-level declaration but only the named symbol's body expanded."),
		"full": boolProp("Force the complete file, bypassing skeleton and delta views."),
	}, "path")
}

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
	Target string `json:"target"`
	Full   bool   `json:"full"`
}

// Run implements Tool.
func (t ReadTool) Run(_ context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a readArgs
	var repairNoteText string
	if err := RepairDecode(in, &a, t.Schema(), tc.Repair); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	if tc.Repair != nil && tc.Repair.Note != nil {
		repairNoteText = *tc.Repair.Note
	}
	if a.Path == "" {
		return Errf("path is required"), nil
	}
	p := resolvePath(tc.Cwd, a.Path)

	st, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			if sug := suggestSimilar(p); sug != "" {
				return Errf("file not found: %s\ndid you mean: %s", relTo(tc.Cwd, p), sug), nil
			}
			return Errf("file not found: %s", relTo(tc.Cwd, p)), nil
		}
		return Errf("%v", err), nil
	}
	if st.IsDir() {
		return Errf("%s is a directory; use glob or bash ls", relTo(tc.Cwd, p)), nil
	}

	maxBytes := t.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 400 << 10
	}
	inputLimit := t.MaxInputBytes
	if inputLimit <= 0 {
		inputLimit = defaultToolInputLimit
	}
	if st.Size() > int64(inputLimit) {
		return Errf("%s exceeds the read input limit of %d bytes", relTo(tc.Cwd, p), inputLimit), nil
	}
	file, err := os.Open(p)
	if err != nil {
		return Errf("%v", err), nil
	}
	defer file.Close()
	prefix := make([]byte, 8000)
	prefixBytes, prefixErr := io.ReadFull(file, prefix)
	if prefixErr != nil && prefixErr != io.EOF && prefixErr != io.ErrUnexpectedEOF {
		return Errf("%v", prefixErr), nil
	}
	prefix = prefix[:prefixBytes]
	if isBinary(prefix) {
		return Errf("%s appears to be a binary file (%d bytes)", relTo(tc.Cwd, p), st.Size()), nil
	}

	// Notebooks (by extension or content) are rendered cell-by-cell instead
	// of dumped as raw JSON: base64 image blobs never reach context, and
	// oversized cell outputs become pointers. A render replaces the whole
	// file (tagged view), so mark the file fully read.
	if isNotebook(p, prefix) {
		if content, err := os.ReadFile(p); err == nil {
			if view, ok := renderNotebook(content); ok {
				markRead(p)
				return repairNote(Result{
					Output: view,
					Title:  fmt.Sprintf("%s (notebook)", relTo(tc.Cwd, p)),
					Meta:   map[string]any{"path": p, "notebook": true},
				}, repairNoteText), nil
			}
		}
		// Parse failure falls through to the plain (JSON) read.
	}

	// Whole-file reads (no explicit pagination) can be served more cheaply: an
	// AST skeleton by default (all bodies collapsed; a named target keeps only
	// that body expanded), or a delta view when the file changed since the
	// model last saw it. full:true always bypasses both.
	if !a.Full && a.Offset < 1 && a.Limit <= 0 {
		if t.EnableSkeleton {
			if skel, err := skeleton.Skeleton(p, a.Target); err == nil {
				markRead(p)
				return repairNote(Result{
					Output: skel,
					Title:  fmt.Sprintf("%s (skeleton)", relTo(tc.Cwd, p)),
					Meta:   map[string]any{"path": p, "skeleton": true},
				}, repairNoteText), nil
			}
			// Parse failures fall through to the plain read.
		}
		if t.Delta != nil {
			if content, err := os.ReadFile(p); err == nil {
				if view, isDelta := t.Delta.Deliver(p, string(content), maxBytes); isDelta {
					markRead(p)
					return repairNote(Result{
						Output: view,
						Title:  fmt.Sprintf("%s (delta)", relTo(tc.Cwd, p)),
						Meta:   map[string]any{"path": p, "delta": true},
					}, repairNoteText), nil
				}
			}
		}
	}

	offset := a.Offset
	if offset < 1 {
		offset = 1
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 2000
	}
	// Surface the relational defaults we applied (offset alone → limit 2000,
	// limit alone → offset 1) so the model sees the chosen semantics and can
	// self-correct if the guess was wrong (Command Code: extend semantics
	// where you can't repair, surface the choice either way). Only fires when
	// the model explicitly asked for pagination with one side missing —
	// plain and full:true reads already have unambiguous semantics.
	var defaultsNote []string
	if a.Offset < 1 && a.Limit > 0 {
		defaultsNote = append(defaultsNote, "offset unset → 1")
	}
	if a.Limit <= 0 && a.Offset >= 1 {
		defaultsNote = append(defaultsNote, "limit unset → 2000")
	}

	// Content-addressed memo: identical reads of an unchanged file are served
	// without touching the disk again (the key carries mtime and size). A hit
	// returns a stub instead of the content, so the model's context does not
	// pay twice for the same bytes; the memo entry is consumed so a second
	// read still gets the real content (stale-stub safety).
	key := memoKey("read", p, fileFingerprint(p), offset, limit, a.Full, a.Target, maxBytes)
	if cached, ok := readMemo.getConsume(key); ok {
		// The memo only fires when the file is unchanged (fingerprint key),
		// so the ledger entry from the original read is still accurate at the
		// same mtime — leave it untouched (a full read stays full, a partial
		// stays partial).
		// The model already has this exact content in context from the
		// previous read. Return a stub that says so instead of re-sending the
		// bytes.
		title := cached.Title
		if title == "" {
			title = relTo(tc.Cwd, p)
		}
		return repairNote(Result{
			Output: fmt.Sprintf("<unchanged: %s; the exact content was already returned in an earlier read — "+
				"re-read with full:true to force it>", title),
			Title: title,
			Meta:  map[string]any{"path": p, "unchanged": true},
		}, repairNoteText), nil
	}

	reader := bufio.NewReaderSize(io.MultiReader(bytes.NewReader(prefix), file), 32<<10)
	requestedEnd := offset - 1 + limit
	lineCount := 0
	bytesRead := 0
	outputEnd := 0
	outputTruncated := false
	var b strings.Builder
	written := 0
	clampedCount := 0
	var clampedLines []int // first few clamped line numbers, for the recovery note
	for {
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			bytesRead += len(line)
			if bytesRead > inputLimit {
				return Errf("%s exceeds the read input limit of %d bytes", relTo(tc.Cwd, p), inputLimit), nil
			}
			lineCount++
			if strings.HasSuffix(line, "\r\n") {
				line = line[:len(line)-2]
			} else if strings.HasSuffix(line, "\n") {
				line = line[:len(line)-1]
			}
			if lineCount >= offset && lineCount <= requestedEnd && !outputTruncated {
				if len(line) > maxReadLineBytes {
					line = line[:maxReadLineBytes] + " …<truncated>"
					clampedCount++
					if len(clampedLines) < 3 {
						clampedLines = append(clampedLines, lineCount)
					}
				}
				lineNumber := strconv.Itoa(lineCount)
				fmt.Fprintf(&b, "%s|%s\n", lineNumber, line)
				written += len(lineNumber) + len(line) + 2
				outputEnd = lineCount
				if written > maxBytes {
					fmt.Fprintf(&b, "\n<output truncated at %d bytes; continue with offset=%d>\n", maxBytes, lineCount+1)
					outputTruncated = true
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return Errf("%v", readErr), nil
		}
	}
	total := lineCount
	if total == 0 {
		total = 1
		if offset == 1 {
			b.WriteString("1|\n")
			outputEnd = 1
		}
	}
	if offset > total {
		return Result{
			Output: fmt.Sprintf("<file %s has %d lines; offset %d is past the end>", relTo(tc.Cwd, p), total, offset),
			Title:  relTo(tc.Cwd, p),
		}, nil
	}
	markReadRange(p, offset, outputEnd, total, clampedCount > 0)

	foot := ""
	if outputEnd < total && !outputTruncated {
		foot = fmt.Sprintf("\n<showing lines %d-%d of %d; continue with offset=%d>", offset, outputEnd, total, outputEnd+1)
	}
	if clampedCount > 0 {
		foot += fmt.Sprintf("\n<%s>", clampNote(clampedLines, clampedCount))
	}
	if len(defaultsNote) > 0 {
		foot += fmt.Sprintf("\n<defaults applied: %s>", strings.Join(defaultsNote, ", "))
	}
	result := repairNote(Result{
		Output: b.String() + foot,
		Title:  fmt.Sprintf("%s (%d lines)", relTo(tc.Cwd, p), total),
		Meta:   map[string]any{"path": p, "lines": total},
	}, repairNoteText)
	readMemo.put(key, result)
	return result, nil
}

// clampNote renders the recovery note for a read that byte-clamped one or
// more lines: it names the clamped lines (capped at three, with a count of
// the rest) so the model knows exactly which content was withheld and how to
// recover — a re-read would clamp again, so grep with context is the path.
func clampNote(lines []int, count int) string {
	if count == 1 {
		return fmt.Sprintf("line %d was clamped to %d chars; its full content was not delivered — use grep with context to inspect it", lines[0], maxReadLineBytes)
	}
	var parts []string
	for _, l := range lines {
		parts = append(parts, strconv.Itoa(l))
	}
	list := strings.Join(parts, ", ")
	if count > len(lines) {
		return fmt.Sprintf("lines %s (and %d more) were clamped to %d chars; their full content was not delivered — use grep with context to inspect", list, count-len(lines), maxReadLineBytes)
	}
	return fmt.Sprintf("lines %s were clamped to %d chars; their full content was not delivered — use grep with context to inspect", list, maxReadLineBytes)
}

func suggestSimilar(p string) string {
	dir := filepath.Dir(p)
	base := strings.ToLower(filepath.Base(p))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	// Normalize the requested base so macOS NFD filenames (screenshots with
	// a narrow no-break space, decomposed umlauts, curly-quote renames) match
	// the NFC form the model is most likely to type.
	normBase := norm.NFC.String(base)
	type candidate struct {
		name  string
		score int
	}
	var cands []candidate
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		normN := norm.NFC.String(n)
		// Unicode-normalized substring match: catches NFD vs NFC spelling.
		if strings.Contains(normN, normBase) || strings.Contains(normBase, normN) {
			cands = append(cands, candidate{e.Name(), 1})
			continue
		}
		// Substring on the trimmed stem (ignoring the extension).
		stem := strings.TrimSuffix(normBase, filepath.Ext(normBase))
		if strings.Contains(normN, stem) || strings.Contains(stem, normN) {
			cands = append(cands, candidate{e.Name(), 2})
			continue
		}
		// Bounded Levenshtein(2): catches AGENT.md -> AGENTS.md where
		// substring matching finds nothing.
		if levenshteinAtMost(normBase, normN, 2) {
			cands = append(cands, candidate{e.Name(), 3})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score < cands[j].score
		}
		return cands[i].name < cands[j].name
	})
	if len(cands) > 3 {
		cands = cands[:3]
	}
	names := make([]string, len(cands))
	for i, c := range cands {
		names[i] = c.name
	}
	return strings.Join(names, ", ")
}

// levenshteinAtMost reports whether the edit distance between a and b is at
// most max, computed with an early-exit band so long unrelated names cost
// almost nothing.
func levenshteinAtMost(a, b string, max int) bool {
	ar := []rune(a)
	br := []rune(b)
	if len(ar)-len(br) > max || len(br)-len(ar) > max {
		return false
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := 0; j <= len(br); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		rowMin := i
		for j := 1; j <= len(br); j++ {
			cost := 0
			if ar[i-1] != br[j-1] {
				cost = 1
			}
			v := min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
			curr[j] = v
			if v < rowMin {
				rowMin = v
			}
		}
		if rowMin > max {
			return false
		}
		prev, curr = curr, prev
	}
	return prev[len(br)] <= max
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// ---------- write ----------

// WriteTool creates or overwrites a file.
type WriteTool struct{}

// Name implements Tool.
func (WriteTool) Name() string { return "write" }

// ReadOnly implements Tool.
func (WriteTool) ReadOnly() bool { return false }

// Description implements Tool.
func (WriteTool) Description() string {
	return "Write content to a file, creating parent directories as needed. " +
		"Overwrites the whole file: for targeted changes prefer 'edit'. " +
		"If the file already exists you must 'read' it first."
}

// Schema implements Tool.
func (WriteTool) Schema() map[string]any {
	return obj(map[string]any{
		"path":    pathProp("File path to write."),
		"content": strProp("Full file content."),
	}, "path", "content")
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Run implements Tool.
func (WriteTool) Run(_ context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a writeArgs
	if err := RepairDecode(in, &a, WriteTool{}.Schema(), tc.Repair); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	if a.Path == "" {
		return Errf("path is required"), nil
	}
	p := resolvePath(tc.Cwd, a.Path)
	fileWriteMu.Lock()
	defer fileWriteMu.Unlock()

	old := ""
	existed := false
	if b, err := os.ReadFile(p); err == nil {
		existed = true
		old = string(b)
		if !wasRead(p) {
			return Errf("refusing to overwrite %s: read it first", relTo(tc.Cwd, p)), nil
		}
		if !wasFullyRead(p) {
			if wasClampedRead(p) {
				return Errf("refusing to overwrite %s: it contains lines too long to deliver in full "+
					"(clamped at %d chars), so an overwrite would destroy content you never saw. "+
					"Use edit for surgical changes, or grep the file with context to inspect the long lines.",
					relTo(tc.Cwd, p), maxReadLineBytes), nil
			}
			if start, end, total, ok := readCoverage(p); ok {
				return Errf("refusing to overwrite %s: only lines %d-%d of %d were read; "+
					"re-read the whole file first (or read with full:true) before overwriting",
					relTo(tc.Cwd, p), start, end, total), nil
			}
			return Errf("refusing to overwrite %s: only part of the file was read; "+
				"re-read the whole file first (or read with full:true) before overwriting", relTo(tc.Cwd, p)), nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return Errf("%v", err), nil
	}
	if err := os.WriteFile(p, []byte(a.Content), 0o644); err != nil {
		return Errf("%v", err), nil
	}
	markRead(p)

	verb := "created"
	if existed {
		verb = "updated"
	}
	nl := strings.Count(a.Content, "\n") + 1
	var note string
	if tc.Repair != nil && tc.Repair.Note != nil {
		note = *tc.Repair.Note
	}
	return repairNote(Result{
		Output: fmt.Sprintf("%s %s (%d lines, %d bytes)", verb, relTo(tc.Cwd, p), nl, len(a.Content)),
		Title:  fmt.Sprintf("write %s", relTo(tc.Cwd, p)),
		Meta:   map[string]any{"path": p, "old": old, "new": a.Content, "created": !existed},
	}, note), nil
}

// ---------- edit ----------

// EditTool performs exact string replacement — the primary edit mechanism.
type EditTool struct{}

// Name implements Tool.
func (EditTool) Name() string { return "edit" }

// ReadOnly implements Tool.
func (EditTool) ReadOnly() bool { return false }

// Description implements Tool.
func (EditTool) Description() string {
	return "Replace an exact string in a file. old_string must appear EXACTLY once " +
		"unless replace_all is true — include enough surrounding context to make it " +
		"unique. Read the file first. Use an empty new_string to delete the match."
}

// Schema implements Tool.
func (EditTool) Schema() map[string]any {
	return obj(map[string]any{
		"path":        pathProp("File path to edit."),
		"old_string":  strProp("Exact text to find, including indentation."),
		"new_string":  strProp("Replacement text. Empty string deletes the match."),
		"replace_all": boolProp("Replace every occurrence instead of requiring uniqueness."),
	}, "path", "old_string", "new_string")
}

type editArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// Run implements Tool.
func (EditTool) Run(_ context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a editArgs
	if err := RepairDecode(in, &a, EditTool{}.Schema(), tc.Repair); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	if a.Path == "" {
		return Errf("path is required"), nil
	}
	if a.OldString == a.NewString {
		return Errf("old_string and new_string are identical"), nil
	}
	p := resolvePath(tc.Cwd, a.Path)
	fileWriteMu.Lock()
	defer fileWriteMu.Unlock()

	raw, err := os.ReadFile(p)
	if err != nil {
		return Errf("cannot read %s: %v", relTo(tc.Cwd, p), err), nil
	}
	if !wasRead(p) {
		return Errf("refusing to edit %s: read it first", relTo(tc.Cwd, p)), nil
	}
	content := string(raw)

	newContent, n, err := applyReplace(content, a.OldString, a.NewString, a.ReplaceAll)
	if err != nil {
		return Errf("%v", err), nil
	}
	if err := os.WriteFile(p, []byte(newContent), 0o644); err != nil {
		return Errf("%v", err), nil
	}
	markRead(p)

	// The model already holds old_string/new_string from its own call; echoing
	// a full unified diff (up to 24 KB) would only re-bill bytes the model
	// wrote itself. Show the touched line numbers + a short before/after of
	// the changed region instead; the full diff stays in Meta for the TUI.
	add, del := DiffStat(content, newContent)
	var b strings.Builder
	fmt.Fprintf(&b, "edited %s (%d replacement(s), +%d -%d)", relTo(tc.Cwd, p), n, add, del)
	if snippet, ok := editSnippet(content, newContent); ok {
		fmt.Fprintf(&b, "\n%s", snippet)
	}
	var note string
	if tc.Repair != nil && tc.Repair.Note != nil {
		note = *tc.Repair.Note
	}
	return repairNote(Result{
		Output: b.String(),
		Title:  fmt.Sprintf("edit %s", relTo(tc.Cwd, p)),
		Meta:   map[string]any{"path": p, "old": content, "new": newContent, "count": n},
	}, note), nil
}

// editSnippet renders the first changed hunk as a compact 2-line
// before/after pair, capped so a small edit echoes only the changed region.
func editSnippet(oldContent, newContent string) (string, bool) {
	const maxEditSnippetBytes = 2 << 10
	diff := UnifiedDiffLimited("", oldContent, newContent, 0, maxEditSnippetBytes)
	if diff == "" {
		return "", false
	}
	return diff, true
}

// applyReplace handles exact match then a small set of whitespace-tolerant
// fallbacks (trailing whitespace, CRLF, leading-indent shift).
func applyReplace(content, old, new string, all bool) (string, int, error) {
	if old == "" {
		return "", 0, fmt.Errorf("old_string must not be empty (use 'write' to create a file)")
	}
	count := strings.Count(content, old)
	if count == 0 {
		if alt, ok := fuzzyFind(content, old); ok {
			old = alt
			count = strings.Count(content, old)
		}
	}
	switch {
	case count == 0:
		return "", 0, fmt.Errorf("old_string not found in file; re-read the file and copy the exact text")
	case count > 1 && !all:
		return "", 0, fmt.Errorf("old_string appears %d times; add surrounding context to make it unique, or set replace_all", count)
	}
	if all {
		return strings.ReplaceAll(content, old, new), count, nil
	}
	return strings.Replace(content, old, new, 1), 1, nil
}

// fuzzyFind tries tolerant variants of old and returns the literal substring
// of content that should be replaced.
func fuzzyFind(content, old string) (string, bool) {
	// 1. CRLF normalisation.
	if strings.Contains(content, "\r\n") {
		if v := strings.ReplaceAll(old, "\n", "\r\n"); strings.Contains(content, v) {
			return v, true
		}
	}
	// 2. Trailing whitespace differences, line by line.
	oldLines := strings.Split(old, "\n")
	trimmed := make([]string, len(oldLines))
	for i, l := range oldLines {
		trimmed[i] = strings.TrimRight(l, " \t")
	}
	contentLines := strings.Split(content, "\n")
	for i := 0; i+len(oldLines) <= len(contentLines); i++ {
		match := true
		for j := range oldLines {
			if strings.TrimRight(contentLines[i+j], " \t") != trimmed[j] {
				match = false
				break
			}
		}
		if match {
			return strings.Join(contentLines[i:i+len(oldLines)], "\n"), true
		}
	}
	// 3. Indentation shift: compare after removing common leading whitespace.
	deindent := func(ls []string) []string {
		out := make([]string, len(ls))
		for i, l := range ls {
			out[i] = strings.TrimLeft(l, " \t")
		}
		return out
	}
	oldD := deindent(oldLines)
	for i := 0; i+len(oldLines) <= len(contentLines); i++ {
		cD := deindent(contentLines[i : i+len(oldLines)])
		match := true
		for j := range oldD {
			if strings.TrimRight(cD[j], " \t") != strings.TrimRight(oldD[j], " \t") {
				match = false
				break
			}
		}
		if match {
			return strings.Join(contentLines[i:i+len(oldLines)], "\n"), true
		}
	}
	return "", false
}
