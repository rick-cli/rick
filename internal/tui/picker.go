package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// ---------- slash autocomplete ----------

type slashCmd struct{ cmd, desc string }

var slashCommands = []slashCmd{
	{"/stop", "interrupt the active run"},
	{"/help", "show keybindings and commands"},
	{"/new", "start a fresh session"},
	{"/sessions", "browse, resume, fork, rename sessions"},
	{"/auth", "connect a provider (api key / oauth / custom endpoint)"},
	{"/webproviders", "configure web-search providers and routing"},
	{"/visionds", "toggle the vision bridge for text-only models"},
	{"/visionapi", "set/clear the free Google AI Studio vision key"},
	{"/model", "switch directly to a model"},
	{"/models", "switch model"},
	{"/update", "update Rick to the latest GitHub release"},
	{"/uninstall", "remove Rick (FULL or PART)"},
	{"/themes", "switch theme (live preview)"},
	{"/config", "settings: theme, model, agent, details"},
	{"/agent", "toggle build / plan mode"},
	{"/goal", "set a goal and work until done"},
	{"/loop <dur>", "loop: work at least <dur>, retrying errors"},
	{"/compact", "summarise and shrink the context"},
	{"/undo", "revert the last file changes"},
	{"/redo", "reapply reverted changes"},
	{"/details", "toggle full tool output"},
	{"/thinking", "toggle reasoning display"},
	{"/tools", "enable/disable tools"},
	{"/mcp", "manage MCP servers"},
	{"/plugins", "manage plugins (on/off/add/remove)"},
	{"/skills", "list, view, and add skills"},
	{"/sandbox", "show or change command confinement"},
	{"/yolo", "bypass every permission prompt (dangerous)"},
	{"/edit", "edit a skill, agent, or mcp config"},
	{"/stats", "token usage summary"},
	{"/refreshmodellist", "refresh model list from providers"},
	{"/exit", "quit rick"},
}

func matchingSlash(prefix string) []slashCmd {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if !strings.HasPrefix(prefix, "/") {
		return nil
	}
	if i := strings.IndexByte(prefix, ' '); i >= 0 {
		return nil
	}
	var out []slashCmd
	for _, sc := range slashCommands {
		if strings.HasPrefix(sc.cmd, prefix) {
			out = append(out, sc)
		}
	}
	return out
}

func (m *Model) autocompleteHeight() int {
	v := m.input.Value()
	if v == "/" {
		return 1
	}
	n := len(matchingSlash(v))
	if n == 0 {
		return 1
	}
	if n > 6 {
		n = 6
	}
	return n
}

func (m *Model) autocompleteView() string {
	s := m.styles
	v := m.input.Value()
	if !strings.HasPrefix(v, "/") {
		return ""
	}
	if strings.Contains(v, " ") {
		return ""
	}

	matches := matchingSlash(v)
	if v == "/" {
		var pills []string
		for _, sc := range slashCommands {
			pills = append(pills, s.Pill.Render(sc.cmd))
		}
		line := strings.Join(pills, s.Faint.Render(" · "))
		return "  " + truncate(line, m.width-4)
	}
	if len(matches) == 0 {
		return "  " + s.Faint.Render("no matching commands")
	}
	if len(matches) > 6 {
		matches = matches[:6]
	}
	if m.slashCursor >= len(matches) {
		m.slashCursor = len(matches) - 1
	}
	if m.slashCursor < 0 {
		m.slashCursor = 0
	}
	var b strings.Builder
	for i, sc := range matches {
		marker := "  "
		nameStyle := s.Muted
		if i == m.slashCursor {
			marker = s.Primary.Render("❯ ")
			nameStyle = s.PillActive
		}
		b.WriteString(marker + nameStyle.Render(padRight(sc.cmd, 14)) +
			s.Faint.Render(sc.desc) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *Model) slashSelection() (string, bool) {
	value := m.input.Value()
	if !strings.HasPrefix(value, "/") || strings.Contains(value, " ") {
		return "", false
	}
	matches := matchingSlash(value)
	if len(matches) == 0 {
		return "", false
	}
	if m.slashCursor >= len(matches) {
		m.slashCursor = len(matches) - 1
	}
	if m.slashCursor < 0 {
		m.slashCursor = 0
	}
	return matches[m.slashCursor].cmd, true
}

func (m *Model) moveSlashCursor(delta int) bool {
	value := m.input.Value()
	if !strings.HasPrefix(value, "/") || strings.Contains(value, " ") {
		return false
	}
	matches := matchingSlash(value)
	if len(matches) == 0 {
		return false
	}
	m.slashCursor = (m.slashCursor + delta) % len(matches)
	if m.slashCursor < 0 {
		m.slashCursor += len(matches)
	}
	m.refresh()
	return true
}

func (m *Model) completeSlashCommand() bool {
	selected, ok := m.slashSelection()
	if !ok {
		return false
	}
	m.input.SetValue(selected)
	m.input.CursorEnd()
	m.slashCursor = 0
	m.resizeAfterInputEdit()
	m.refresh()
	return true
}

// ---------- @ file picker ----------

type fileEntry struct {
	path      string // relative, slash-separated
	name      string
	lowerPath string
	lowerName string
	dir       bool
}

type filePicker struct {
	active  bool
	query   string
	all     []fileEntry
	results []fileEntry
	cursor  int
	atIndex int // byte index of the '@' in the input
}

func (p filePicker) height() int {
	n := len(p.results)
	if n > 8 {
		n = 8
	}
	if n == 0 {
		return 1
	}
	return n
}

func (m *Model) openPicker() {
	v := m.input.Value()
	idx := strings.LastIndex(v, "@")
	if idx < 0 {
		return
	}
	m.picker = filePicker{active: true, atIndex: idx, all: scanProjectFiles(m.deps.Cwd)}
	m.picker.results = filterFiles(m.picker.all, "")
	m.handleResize(tea.WindowSizeMsg{Width: m.width, Height: m.height})
}

func (m *Model) updatePickerQuery() {
	v := m.input.Value()
	if m.picker.atIndex >= len(v) || m.picker.atIndex < 0 {
		m.closePicker()
		return
	}
	q := v[m.picker.atIndex+1:]
	if strings.ContainsAny(q, " \t") {
		m.closePicker()
		return
	}
	m.picker.query = q
	m.picker.results = filterFiles(m.picker.all, q)
	m.picker.cursor = 0
}

func (m *Model) closePicker() {
	m.picker = filePicker{}
	m.handleResize(tea.WindowSizeMsg{Width: m.width, Height: m.height})
}

func (m *Model) handlePickerKey(key string) (bool, tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		m.closePicker()
		return true, m, nil
	case "up", "ctrl+p":
		if m.picker.cursor > 0 {
			m.picker.cursor--
		}
		return true, m, nil
	case "down", "ctrl+n":
		if m.picker.cursor < len(m.picker.results)-1 {
			m.picker.cursor++
		}
		return true, m, nil
	case "enter", "tab":
		if len(m.picker.results) == 0 {
			m.closePicker()
			return true, m, nil
		}
		sel := m.picker.results[m.picker.cursor]
		v := m.input.Value()
		newVal := v[:m.picker.atIndex] + "@" + sel.path + " "
		m.input.SetValue(newVal)
		m.input.CursorEnd()
		m.closePicker()
		return true, m, nil
	}
	return false, m, nil
}

func (m *Model) pickerView() string {
	s := m.styles
	if len(m.picker.results) == 0 {
		return "  " + s.Faint.Render("no matching files")
	}
	results := m.picker.results
	if len(results) > 8 {
		results = results[:8]
	}
	var b strings.Builder
	for i, e := range results {
		marker := "  "
		style := s.Muted
		if i == m.picker.cursor {
			marker = s.Primary.Render("❯ ")
			style = s.Base
		}
		icon := "·"
		if e.dir {
			icon = "▸"
		}
		b.WriteString(marker + s.Faint.Render(icon+" ") + style.Render(truncate(e.path, m.width-8)) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true,
	"target": true, ".venv": true, "venv": true, "__pycache__": true, ".next": true,
	".idea": true, ".vscode": true, "bin": true, "obj": true,
}

func scanProjectFiles(root string) []fileEntry {
	var out []fileEntry
	const limit = 20000
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(out) >= limit {
			return filepath.SkipAll
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (skipDirs[name] || (strings.HasPrefix(name, ".") && name != ".rick")) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		relPath := filepath.ToSlash(rel)
		out = append(out, fileEntry{
			path:      relPath,
			name:      name,
			lowerPath: strings.ToLower(relPath),
			lowerName: strings.ToLower(name),
		})
		return nil
	})
	return out
}

// fuzzyMatch reports whether query is a subsequence of target, plus a score
// (lower is better).
func fuzzyMatch(query, target string) (int, bool) {
	return fuzzyMatchNormalized(strings.ToLower(query), strings.ToLower(target))
}

func fuzzyMatchNormalized(query, target string) (int, bool) {
	if query == "" {
		return len(target), true
	}
	q := query
	t := target

	// Exact substring is strongly preferred.
	if i := strings.Index(t, q); i >= 0 {
		return i, true
	}

	qi := 0
	last := -1
	gaps := 0
	for ti := 0; ti < len(t) && qi < len(q); ti++ {
		if t[ti] == q[qi] {
			if last >= 0 {
				gaps += ti - last - 1
			}
			last = ti
			qi++
		}
	}
	if qi < len(q) {
		return 0, false
	}
	return 1000 + gaps, true
}

func filterFiles(all []fileEntry, query string) []fileEntry {
	queryLower := strings.ToLower(query)
	type scored struct {
		e     fileEntry
		score int
	}
	var out []scored
	for _, e := range all {
		pathLower := e.lowerPath
		if pathLower == "" && e.path != "" {
			pathLower = strings.ToLower(e.path)
		}
		nameLower := e.lowerName
		if nameLower == "" && e.name != "" {
			nameLower = strings.ToLower(e.name)
		}
		s, ok := fuzzyMatchNormalized(queryLower, pathLower)
		if !ok {
			continue
		}
		// Prefer matches in the basename.
		if bs, bok := fuzzyMatchNormalized(queryLower, nameLower); bok && bs < s {
			s = bs
		}
		out = append(out, scored{e, s + len(e.path)/8})
		if len(out) > 4000 {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score < out[j].score })
	res := make([]fileEntry, 0, 40)
	for i := 0; i < len(out) && i < 40; i++ {
		res = append(res, out[i].e)
	}
	return res
}

// expandFileRefs replaces @path tokens with the file's contents appended to
// the prompt, and returns the list of attached paths.
func (m *Model) expandFileRefs(text string) (string, []string) {
	fields := strings.Fields(text)
	var attached []string
	var blocks []string
	seen := map[string]bool{}

	for _, f := range fields {
		if !strings.HasPrefix(f, "@") || len(f) < 2 {
			continue
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(f, "@"), ",")
		if seen[rel] {
			continue
		}
		seen[rel] = true
		p := rel
		if !filepath.IsAbs(p) {
			p = filepath.Join(m.deps.Cwd, rel)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// When the vision bridge is on, don't inline image files as text
		// (that would embed binary garbage). Leave the @token in the prompt
		// so imagePathsInPrompt can route it through the vision model.
		if isImageFile(p) && m.visionEnabled() {
			continue
		}
		if len(data) > 64<<10 {
			data = append(data[:64<<10], []byte("\n…<truncated>")...)
		}
		attached = append(attached, rel)
		blocks = append(blocks, fmt.Sprintf("### %s\n```\n%s\n```", rel, strings.TrimRight(string(data), "\n")))
	}
	if len(blocks) == 0 {
		return text, nil
	}
	return text + "\n\n<attached-files>\n" + strings.Join(blocks, "\n\n") + "\n</attached-files>", attached
}
