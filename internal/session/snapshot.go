package session

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Snapshotter captures project file state into a shadow git repository that is
// completely independent of the user's own git history.
//
// It works by running git with a separate GIT_DIR pointing at
// <data>/snapshots/<hash-of-project>/ while the work tree stays the project
// directory. The user's .git is never touched.
type Snapshotter struct {
	mu       sync.Mutex
	workTree string
	gitDir   string
	enabled  bool
	history  []Snapshot
	cursor   int // index into history for undo/redo; len(history) == "present"
}

const maxSnapshotHistory = 100

// NewSnapshotter prepares a shadow repo for a project directory.
func NewSnapshotter(workTree, dataDir string) (*Snapshotter, error) {
	s := &Snapshotter{workTree: workTree}
	if _, err := exec.LookPath("git"); err != nil {
		return s, fmt.Errorf("git not found: snapshots disabled")
	}

	absWork, err := filepath.Abs(workTree)
	if err != nil {
		return s, err
	}
	s.workTree = absWork

	// Never shadow-repo a well-known personal folder wholesale: starting rick
	// in Downloads (or the profile root itself) would snapshot the entire user
	// tree into <data>/snapshots — multi-GB growth with no undo value.
	if isWellKnownUserDir(absWork) {
		return s, fmt.Errorf("snapshots disabled: %s is a well-known user folder", absWork)
	}

	absData, err := filepath.Abs(dataDir)
	if err != nil {
		return s, err
	}
	// The shadow repo must never live inside the work tree: `git checkout`
	// would try to overwrite its own index.lock and fail with a confusing
	// "unable to unlink old ... index.lock" error.
	if rel, err := filepath.Rel(absWork, absData); err == nil &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return s, fmt.Errorf("snapshot dir %s is inside the work tree; snapshots disabled", absData)
	}

	key := sanitize(absWork)
	s.gitDir = filepath.Join(absData, "snapshots", key)
	if err := os.MkdirAll(s.gitDir, 0o755); err != nil {
		return s, err
	}
	// Defer `git init` to first use — a cold `git init` costs ~300ms and would
	// block the whole startup before the first frame paints and the input box
	// becomes live. The actual init happens lazily inside Enabled().
	return s, nil
}

// Enabled returns true once the shadow repo is ready, initializing it on
// first call. Safe to call every frame; the init runs at most once.
func (s *Snapshotter) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enabled {
		return true
	}
	if _, err := os.Stat(filepath.Join(s.gitDir, "HEAD")); err != nil {
		if _, err := s.git("init", "--quiet"); err != nil {
			return false
		}
		s.git("config", "user.email", "rick@localhost")
		s.git("config", "user.name", "rick")
		s.git("config", "core.autocrlf", "false")
		s.git("config", "core.safecrlf", "false")
		excl := filepath.Join(s.gitDir, "info")
		if err := os.MkdirAll(excl, 0o755); err == nil {
			_ = os.WriteFile(filepath.Join(excl, "exclude"),
				[]byte("node_modules/\nvendor/\n.venv/\ntarget/\ndist/\nbuild/\n__pycache__/\n*.exe\n*.dll\n*.so\n*.dylib\n"),
				0o644)
		}
	}
	s.enabled = true
	return s.enabled
}

func sanitize(p string) string {
	r := strings.NewReplacer(`\`, "_", "/", "_", ":", "", " ", "-")
	out := r.Replace(p)
	if len(out) > 80 {
		out = out[len(out)-80:]
	}
	return strings.Trim(out, "_")
}

// wellKnownUserDirNames are personal folders that rick must never shadow-repo
// wholesale. Subfolders inside them (a real project under ~/Desktop/work) are
// still snapshotted normally; only the personal roots themselves are refused.
var wellKnownUserDirNames = []string{"Downloads", "Documents", "Desktop", "Pictures", "Videos", "Music"}

func isWellKnownUserDir(absWork string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	absHome, err := filepath.Abs(home)
	if err != nil {
		return false
	}
	if strings.EqualFold(absWork, absHome) {
		return true
	}
	if !strings.EqualFold(filepath.Dir(absWork), absHome) {
		return false
	}
	base := filepath.Base(absWork)
	for _, name := range wellKnownUserDirNames {
		if strings.EqualFold(base, name) {
			return true
		}
	}
	return false
}

func (s *Snapshotter) git(args ...string) (string, error) {
	full := append([]string{"--git-dir", s.gitDir, "--work-tree", s.workTree}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = s.workTree
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// Snapshot commits the current work tree state and returns the commit hash.
//
// A snapshot is a checkpoint of the tree AS IT IS NOW; the agent calls it
// before each mutating tool batch. Commits that would be empty are skipped, so
// a batch that touches nothing does not create a no-op checkpoint that undo
// would then uselessly step through.
func (s *Snapshotter) Snapshot(label string) (string, error) {
	if !s.Enabled() {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked(label)
}

func (s *Snapshotter) snapshotLocked(label string) (string, error) {
	if _, err := s.git("add", "-A", "--", "."); err != nil {
		return "", nil // best-effort: never break a turn over snapshots
	}
	// Skip an empty commit unless this is the very first snapshot.
	if len(s.history) > 0 {
		if out, err := s.git("diff", "--cached", "--quiet"); err == nil && out == "" {
			return "", nil // nothing changed since the last checkpoint
		}
	}

	msg := fmt.Sprintf("rick: %s @ %s", label, time.Now().Format(time.RFC3339))
	if out, err := s.git("commit", "--quiet", "--allow-empty", "-m", msg); err != nil {
		return "", fmt.Errorf("snapshot commit: %s", out)
	}
	hash, err := s.git("rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	// A new snapshot truncates the redo tail.
	if s.cursor < len(s.history) {
		s.history = s.history[:s.cursor+1]
	}
	s.history = append(s.history, Snapshot{ID: hash, Label: label, Created: time.Now()})
	if len(s.history) > maxSnapshotHistory {
		s.history = append([]Snapshot(nil), s.history[len(s.history)-maxSnapshotHistory:]...)
		_, _ = s.git("gc", "--auto")
	}
	s.cursor = len(s.history)
	return hash, nil
}

// History returns recorded snapshots.
func (s *Snapshotter) History() []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Snapshot(nil), s.history...)
}

// WorkTree returns the project directory this snapshotter shadows.
func (s *Snapshotter) WorkTree() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workTree
}

// Restore resets the work tree to a snapshot.
func (s *Snapshotter) Restore(hash string) error {
	if !s.Enabled() {
		return fmt.Errorf("snapshots are disabled")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restoreLocked(hash)
}

func (s *Snapshotter) restoreLocked(hash string) error {
	if !s.enabled {
		return fmt.Errorf("snapshots are disabled")
	}
	if out, err := s.git("reset", "--hard", hash); err != nil {
		return fmt.Errorf("restore: %s", out)
	}
	for index, snapshot := range s.history {
		if snapshot.ID == hash {
			s.cursor = index
			return nil
		}
	}
	s.history = nil
	s.cursor = 0
	return nil
}

// CanUndo reports whether an undo target exists.
func (s *Snapshotter) CanUndo() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor > 0
}

// CanRedo reports whether a redo target exists.
func (s *Snapshotter) CanRedo() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor < len(s.history)-1
}

// Undo steps back to the most recent checkpoint whose tree actually differs
// from what is on disk right now, and restores it.
//
// Checkpoints are taken BEFORE each mutating tool, so several consecutive
// checkpoints can describe an identical tree (a bash command that only reads,
// for example, still gets a checkpoint). Stepping over those is what makes
// undo feel correct: one press = one visible change reverted.
func (s *Snapshotter) Undo() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.history) == 0 {
		return Snapshot{}, fmt.Errorf("nothing to undo")
	}
	// Capture the present so redo can return to it.
	if s.cursor >= len(s.history) {
		_, _ = s.snapshotLocked("present")
		s.cursor = len(s.history) - 1
	}

	// Walk back past checkpoints identical to the current tree.
	for i := s.cursor; i >= 0; i-- {
		if s.sameAsWorkTree(s.history[i].ID) {
			continue
		}
		s.cursor = i
		target := s.history[i]
		if err := s.restoreLocked(target.ID); err != nil {
			return Snapshot{}, err
		}
		return target, nil
	}
	return Snapshot{}, fmt.Errorf("nothing to undo")
}

// Redo steps forward to the next checkpoint that differs from the current tree.
func (s *Snapshotter) Redo() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := s.cursor + 1; i < len(s.history); i++ {
		if s.sameAsWorkTree(s.history[i].ID) {
			continue
		}
		s.cursor = i
		target := s.history[i]
		if err := s.restoreLocked(target.ID); err != nil {
			return Snapshot{}, err
		}
		return target, nil
	}
	return Snapshot{}, fmt.Errorf("nothing to redo")
}

// sameAsWorkTree reports whether a commit's tree matches the files on disk.
func (s *Snapshotter) sameAsWorkTree(hash string) bool {
	if _, err := s.git("diff", "--quiet", hash, "--", "."); err != nil {
		return false
	}
	untracked, err := s.git("ls-files", "--others", "--exclude-standard", "--", ".")
	return err == nil && strings.TrimSpace(untracked) == ""
}

// LoadHistory restores snapshot bookkeeping from a resumed session.
func (s *Snapshotter) LoadHistory(h []Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append([]Snapshot(nil), h...)
	s.cursor = len(s.history)
}

// GitInfo returns a short description of the user's real git state.
func GitInfo(dir string) string {
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat")
		var out bytes.Buffer
		cmd.Stdout = &out
		if cmd.Run() != nil {
			return ""
		}
		return strings.TrimSpace(out.String())
	}
	branch := run("rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" {
		return ""
	}
	status := run("status", "--porcelain")
	dirty := ""
	if status != "" {
		dirty = fmt.Sprintf(", %d file(s) modified", len(strings.Split(status, "\n")))
	}
	return fmt.Sprintf("branch %s%s", branch, dirty)
}
