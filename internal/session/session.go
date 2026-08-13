// Package session persists conversations to disk and provides snapshot-backed
// undo/redo of file changes.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"rick/internal/provider"
)

// Session is one conversation.
type Session struct {
	ID       string             `json:"id"`
	Title    string             `json:"title"`
	Cwd      string             `json:"cwd"`
	Model    string             `json:"model"`
	Agent    string             `json:"agent"`
	Parent   string             `json:"parent,omitempty"`
	Category string             `json:"category,omitempty"`
	Favorite bool               `json:"favorite,omitempty"`
	Created  time.Time          `json:"created"`
	Updated  time.Time          `json:"updated"`
	Messages []provider.Message `json:"messages"`
	// RunError records the last terminal provider/agent failure for diagnostics.
	// It is not part of Messages and is never replayed to the provider.
	RunError string `json:"run_error,omitempty"`
	// SentTranscript is the exact bounded provider-facing view last sent, so
	// a resumed session can replay byte-identical bytes and warm the provider
	// prompt cache on its first turn.
	SentTranscript []provider.Message `json:"sent_transcript,omitempty"`
	// EnvGit is the git-state line (branch + dirty count) frozen when the
	// session started. Resuming reuses it so the system prompt's environment
	// block is byte-identical and the provider cache survives the restart.
	EnvGit    string     `json:"env_git,omitempty"`
	Snapshots []Snapshot `json:"snapshots,omitempty"`
	Usage     Usage      `json:"usage"`
	// TotalUsage is the durable whole-log cumulative token accounting: the
	// sum of every request's usage across the session, persisted at turn/end
	// (saveSession). Unlike Usage (context occupancy of the latest request),
	// TotalUsage survives compaction and resume so /stats and the analyzer
	// can compute the true whole-session cache hit rate without replaying
	// per-request rows.
	TotalUsage Usage `json:"total_usage,omitempty"`
	// Requests records provider per-request token accounting (one entry per
	// provider request / EvUsage event), not just the cumulative totals. This
	// makes per-turn cache-hit/miss behavior observable so cache changes
	// (warm, retention, reasoning caps) can be measured request-by-request.
	Requests []RequestUsage `json:"requests,omitempty"`
	// ContextUsed is the provider-facing prompt size from the latest turn. It is
	// intentionally separate from cumulative Usage so resumed clients can
	// restore an accurate context gauge.
	ContextUsed  int               `json:"context_used,omitempty"`
	Optimization OptimizationUsage `json:"optimization,omitempty"`
	// Epoch is the durable provider-request header identity (harness-style
	// request/header event): the frozen model, system prompt bytes, canonical
	// tool list, and derived epoch hash. Persisted when the first request is
	// built so a resumed session can prove its header is byte-identical (or
	// detect drift — e.g. the repo-map block changed with cwd) instead of
	// recomputing from config silently and cold-starting a new cache bucket.
	Epoch EpochHeader `json:"epoch,omitempty"`
	// Prunes is the durable per-node shadow-price ledger of every proactive
	// tool-result prune: the content address and sizes of each replaced
	// result. Persisted so a pruned tool result stays replay-safe — the
	// original bytes are recoverable from the content-addressed store and
	// the reclaim is auditable per node (harness-style compaction/prune
	// event).
	Prunes []PruneRecord `json:"prunes,omitempty"`
	// Compactions records every durable compaction transaction: the exact
	// message span replaced, the summary's token cost, and the provider
	// usage of the summarization call. Resume uses it to keep the summary
	// at byte-identical position and to never re-compact the same span.
	Compactions []CompactionRecord `json:"compactions,omitempty"`
}

// CompactionRecord is one durable compaction transaction. ReplacedRange is
// the inclusive message-index span folded into the summary; SummaryTokens is
// the provider-reported cost of the summarization call; Usage carries the
// summary call's token accounting so the aux cost is measurable.
type CompactionRecord struct {
	Time          string       `json:"time,omitempty"`
	ReplacedStart int          `json:"replaced_start"`
	ReplacedEnd   int          `json:"replaced_end"`
	SummaryTokens int          `json:"summary_tokens,omitempty"`
	Usage         RequestUsage `json:"usage,omitempty"`
}

// EpochHeader is the durable provider-request header identity of a session
// (harness-style request/header event): the frozen model, the byte-stable
// system prompt, the canonical tool schema list, and the content-addressed
// epoch hash derived from them. Persisted with the first built request so a
// resumed session can verify its header is byte-identical to what the
// provider still has cached — or detect drift and re-prime — instead of
// recomputing silently and cold-starting a new cache bucket.
type EpochHeader struct {
	// Model is the routed model id at the time the header was frozen.
	Model string `json:"model,omitempty"`
	// System is the full provider-facing system prompt bytes (stable +
	// volatile) frozen at the first request.
	System string `json:"system,omitempty"`
	// SystemStable is the stable system-prefix half (cached region).
	SystemStable string `json:"system_stable,omitempty"`
	// Tools is the canonical (sorted, deep-normalized) tool schema list.
	Tools []provider.ToolSchema `json:"tools,omitempty"`
	// Hash is the content-addressed epoch hash derived from Model + System +
	// Tools (agent.CacheScopeKeyFor). A resumed session whose recomputed
	// header produces a different hash knows the provider prefix drifted.
	Hash string `json:"hash,omitempty"`
}

// PruneRecord is one durable tool-result prune (harness-style
// compaction/prune shadow-price event): the content address of the original
// payload, its size, the provider-facing size of the summary that replaced
// it, and the request that committed the prune.
type PruneRecord struct {
	Time        string `json:"time,omitempty"`
	ToolUseID   string `json:"tool_use_id,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
	OriginalLen int    `json:"original_len,omitempty"`
	SummaryLen  int    `json:"summary_len,omitempty"`
	Request     int    `json:"request,omitempty"`
}

// RequestUsage is one request's provider-reported token accounting.
type RequestUsage struct {
	// Index is the chronological request number within the session (1-based).
	Index int `json:"index"`
	// Agent labels which sub-run produced the request ("" = primary session).
	Agent string `json:"agent,omitempty"`
	// Input, Output, CacheRead, CacheWrite mirror provider.Usage for this
	// single request.
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
	// Divergence, when set, is the prefix-divergence diagnostics for this
	// request ("message@7;dedup"): where the provider-facing bytes stopped
	// matching the previous turn and the inferred cause.
	Divergence string `json:"divergence,omitempty"`
	// Eviction, when set, labels why this request re-billed tokens the
	// previous request had already sent, when no client-side byte divergence
	// was detected ("idle gap (cache expired)", "provider served no prefix
	// cache"). It is the analyzer's second opinion that tells a provider
	// eviction apart from a client rewrite.
	Eviction string `json:"eviction,omitempty"`
	// ReasoningTokens is the client-side token size of the deep-reasoning
	// echo sent with this request (all `thinking` blocks in the message
	// view). It makes the reasoning-echo fresh-tail cost measurable per
	// request so `cache_max_reasoning_turns` can be tuned from real data.
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	// ResponseCacheHit records whether the gateway served this request from
	// its response cache (OpenRouter X-OpenRouter-Cache-Status: HIT) — a
	// byte-identical repeated request billed at zero. Counted separately from
	// prompt-cache reads so the response-cache hit rate is measurable.
	ResponseCacheHit bool `json:"response_cache_hit,omitempty"`
	// Boundary records the deliberate cache-boundary decision this request's
	// build made ("tool-prune;committed;reclaim-gated episodic prune" or
	// "distill;deferred;planned prefix still warm"). It is the audit trail
	// for every deliberate prefix rewrite (or shadow-price deferral).
	Boundary string `json:"boundary,omitempty"`
}

// OptimizationUsage accumulates exact local measurements for provider-facing
// tool-output reduction. It is separate from cumulative billing usage.
type OptimizationUsage struct {
	ToolResults    int `json:"tool_results"`
	OriginalTokens int `json:"original_tokens"`
	ProviderTokens int `json:"provider_tokens"`
	SavedTokens    int `json:"saved_tokens"`
}

// SavingsPercent returns the measured provider-facing reduction as a percentage
// of the original tokens. Zero original tokens returns zero.
func (u OptimizationUsage) SavingsPercent() float64 {
	if u.OriginalTokens <= 0 {
		return 0
	}
	return float64(u.SavedTokens) * 100 / float64(u.OriginalTokens)
}

// Add merges one optimization measurement.
func (u *OptimizationUsage) Add(original, provider, saved int) {
	u.ToolResults++
	u.OriginalTokens += original
	u.ProviderTokens += provider
	u.SavedTokens += saved
}

// Usage is the cumulative token count for a session.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}

// Snapshot references a point-in-time file state.
type Snapshot struct {
	ID      string    `json:"id"`      // git commit hash in the shadow repo
	Label   string    `json:"label"`   // what triggered it
	MsgIdx  int       `json:"msg_idx"` // message count when taken
	Created time.Time `json:"created"`
}

// Meta is the lightweight listing entry.
type Meta struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Cwd        string    `json:"cwd"`
	Model      string    `json:"model"`
	Parent     string    `json:"parent,omitempty"`
	Category   string    `json:"category,omitempty"`
	Favorite   bool      `json:"favorite,omitempty"`
	Messages   int       `json:"messages"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
	ByteSize   int64     `json:"byte_size"`
	LastPrompt string    `json:"last_prompt,omitempty"`
}

// Store is a directory of session files.
type Store struct {
	mu  sync.Mutex
	dir string
}

// NewStore opens (and creates) a session directory.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir returns the backing directory.
func (s *Store) Dir() string { return s.dir }

// SearchPath returns the search-sidecar path for a session id. The sidecar
// is written by Save and read by Search/resume browsing to avoid loading the
// full session JSON when only message text is needed.
func (s *Store) SearchPath(id string) string { return s.searchPath(id) }

// NewID mints a sortable, filesystem-safe id.
func NewID() string {
	now := time.Now()
	return fmt.Sprintf("%s_%04x", now.Format("2006-01-02T15-04-05"), now.Nanosecond()&0xFFFF)
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".json") }

// searchPath returns the lightweight search-text sidecar for a session: the
// lowercased concatenation of all message text, written at Save time so
// Search can answer without unmarshalling the full session JSON.
func (s *Store) searchPath(id string) string { return filepath.Join(s.dir, id+".search.txt") }

func validID(id string) bool {
	return id != "" && id != "." && id != ".." && filepath.Base(id) == id &&
		!strings.ContainsAny(id, "/\\\x00")
}

func (s *Store) metaPath(id string) string { return filepath.Join(s.dir, id+".meta.json") }

// Save atomically writes a session.
func (s *Store) Save(sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.ID == "" {
		sess.ID = NewID()
	}
	if !validID(sess.ID) {
		return fmt.Errorf("invalid session id")
	}
	sess.Updated = time.Now()
	if sess.Created.IsZero() {
		sess.Created = sess.Updated
	}
	data, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	final := s.path(sess.ID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	meta := metaFrom(sess)
	meta.ByteSize = int64(len(data))
	metaData, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	metaTmp := s.metaPath(sess.ID) + ".tmp"
	if err := os.WriteFile(metaTmp, metaData, 0o644); err != nil {
		return err
	}
	if err := os.Rename(metaTmp, s.metaPath(sess.ID)); err != nil {
		return err
	}
	// Search sidecar: lowercased concatenated message text so Search avoids
	// unmarshalling the full session JSON. Written best-effort — a failure
	// only degrades Search to the Load() fallback.
	if idxText := searchTextOf(sess); idxText != "" {
		idxTmp := s.searchPath(sess.ID) + ".tmp"
		if err := os.WriteFile(idxTmp, []byte(idxText), 0o644); err == nil {
			_ = os.Rename(idxTmp, s.searchPath(sess.ID))
		} else {
			_ = os.Remove(idxTmp)
		}
	}
	return nil
}

// SearchTextOf builds the search-sidecar payload for a session: the
// lowercased concatenation of every text-bearing block, bounded so the
// sidecar cannot balloon with a huge transcript. Exported so the resume
// browser can backfill sidecars for sessions saved before the feature.
func SearchTextOf(sess *Session) string {
	const maxSearchSidecarBytes = 1 << 20 // 1 MiB per session is ample for search
	var b strings.Builder
	for _, m := range sess.Messages {
		b.WriteString(m.Text())
		b.WriteByte('\n')
	}
	if b.Len() > maxSearchSidecarBytes {
		return strings.ToLower(b.String()[:maxSearchSidecarBytes])
	}
	return strings.ToLower(b.String())
}

// searchTextOf is the unexported alias used by Save.
func searchTextOf(sess *Session) string { return SearchTextOf(sess) }

// HasSearchText reports whether a search sidecar exists for a session.
func (s *Store) HasSearchText(id string) bool {
	if !validID(id) {
		return false
	}
	_, err := os.Stat(s.searchPath(id))
	return err == nil
}

// WriteSearchText persists a session's search sidecar so later searches can
// skip the full-JSON parse. Writes are atomic (tmp + rename).
func (s *Store) WriteSearchText(id, text string) error {
	if !validID(id) {
		return fmt.Errorf("invalid session id")
	}
	if text == "" {
		return nil
	}
	tmp := s.searchPath(id) + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.searchPath(id))
}

// Load reads a session by id.
func (s *Store) Load(id string) (*Session, error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid session id")
	}
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

// Delete removes a session file, its lightweight metadata companion, and the
// search sidecar.
func (s *Store) Delete(id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid session id")
	}
	sessionErr := os.Remove(s.path(id))
	if sessionErr != nil && !os.IsNotExist(sessionErr) {
		return sessionErr
	}
	metaErr := os.Remove(s.metaPath(id))
	if metaErr != nil && !os.IsNotExist(metaErr) {
		return metaErr
	}
	searchErr := os.Remove(s.searchPath(id))
	if searchErr != nil && !os.IsNotExist(searchErr) {
		return searchErr
	}
	return nil
}

// List returns session metadata, newest first, optionally filtered by cwd.
func (s *Store) List(cwd string) ([]Meta, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Meta
	var legacyEntries []os.DirEntry
	// known records every session that already has a meta.json, regardless of
	// the cwd filter. listLegacy skips sessions in this set, so a cwd-filtered
	// list must not re-parse the full JSON of sessions whose metadata merely
	// does not match the filter — they can never appear in the result anyway.
	known := make(map[string]struct{})
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".meta.json") {
			id := strings.TrimSuffix(e.Name(), ".meta.json")
			known[id] = struct{}{}
			data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
			if err != nil {
				continue
			}
			var meta Meta
			if json.Unmarshal(data, &meta) != nil {
				continue
			}
			if meta.ID == "" {
				meta.ID = id
			}
			if cwd != "" && meta.Cwd != cwd {
				continue
			}
			out = append(out, meta)
			continue
		}
		if strings.HasSuffix(e.Name(), ".json") && e.Name() != "current.json" {
			legacyEntries = append(legacyEntries, e)
		}
	}
	legacy := s.listLegacy(cwd, known, legacyEntries)
	for _, meta := range legacy {
		if _, ok := known[meta.ID]; !ok {
			out = append(out, meta)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

func (s *Store) listLegacy(cwd string, known map[string]struct{}, entries []os.DirEntry) []Meta {
	var out []Meta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".meta.json") || e.Name() == "current.json" {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if _, ok := known[id]; ok {
			continue
		}
		meta, err := legacyMetaFromFile(filepath.Join(s.dir, e.Name()))
		if err != nil || (cwd != "" && meta.Cwd != cwd) {
			continue
		}
		if meta.ID == "" {
			meta.ID = id
		}
		if meta.Created.IsZero() {
			meta.Created = meta.Updated
		}
		if meta.Updated.IsZero() {
			if info, statErr := e.Info(); statErr == nil {
				meta.Updated = info.ModTime()
				if meta.Created.IsZero() {
					meta.Created = meta.Updated
				}
			}
		}
		out = append(out, meta)
	}
	return out
}
func legacyMetaFromFile(path string) (Meta, error) {
	file, err := os.Open(path)
	if err != nil {
		return Meta{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	start, err := decoder.Token()
	if err != nil {
		return Meta{}, err
	}
	if delim, ok := start.(json.Delim); !ok || delim != '{' {
		return Meta{}, fmt.Errorf("session file is not a JSON object")
	}
	var meta Meta
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return Meta{}, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return Meta{}, fmt.Errorf("session field name is not a string")
		}
		switch key {
		case "id":
			err = decoder.Decode(&meta.ID)
		case "title":
			err = decoder.Decode(&meta.Title)
		case "cwd":
			err = decoder.Decode(&meta.Cwd)
		case "model":
			err = decoder.Decode(&meta.Model)
		case "parent":
			err = decoder.Decode(&meta.Parent)
		case "category":
			err = decoder.Decode(&meta.Category)
		case "favorite":
			err = decoder.Decode(&meta.Favorite)
		case "created":
			err = decoder.Decode(&meta.Created)
		case "updated":
			err = decoder.Decode(&meta.Updated)
		case "messages":
			meta.Messages, err = skipJSONValue(decoder)
		default:
			_, err = skipJSONValue(decoder)
		}
		if err != nil {
			return Meta{}, err
		}
	}
	_, err = decoder.Token()
	return meta, err
}

// skipJSONValue consumes one JSON value without materializing it. It returns
// the number of elements when the value is an array, which lets legacy session
// listings retain their message count without loading message bodies.
func skipJSONValue(decoder *json.Decoder) (int, error) {
	token, err := decoder.Token()
	if err != nil {
		return 0, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return 0, nil
	}
	switch delim {
	case '{':
		for decoder.More() {
			if _, err := decoder.Token(); err != nil {
				return 0, err
			}
			if _, err := skipJSONValue(decoder); err != nil {
				return 0, err
			}
		}
		_, err := decoder.Token()
		return 0, err
	case '[':
		count := 0
		for decoder.More() {
			if _, err := skipJSONValue(decoder); err != nil {
				return 0, err
			}
			count++
		}
		_, err := decoder.Token()
		return count, err
	default:
		return 0, nil
	}
}

func (s *Store) SetCurrent(cwd, id string) error {
	m, _ := s.currentMap()
	if m == nil {
		m = map[string]string{}
	}
	m[cwd] = id
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(s.dir, "current.json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// GetCurrent returns the last-active session id for a working directory.
func (s *Store) GetCurrent(cwd string) string {
	m, err := s.currentMap()
	if err != nil {
		return ""
	}
	return m[cwd]
}

func (s *Store) currentMap() (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, "current.json"))
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// Title derives a fallback title from the first user message.
func Title(msgs []provider.Message) string {
	for _, m := range msgs {
		if m.Role != provider.RoleUser {
			continue
		}
		t := strings.TrimSpace(m.Text())
		if t == "" {
			continue
		}
		t = strings.ReplaceAll(t, "\n", " ")
		if len(t) > 48 {
			t = t[:48] + "…"
		}
		return t
	}
	return "untitled"
}

// PruneOlderThan deletes sessions whose Updated timestamp is older than
// maxAge, across every working directory. current.json pointers to removed
// sessions are dropped. Returns the number of sessions removed.
func (s *Store) PruneOlderThan(maxAge time.Duration) (int, error) {
	metas, err := s.List("")
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, m := range metas {
		if !m.Updated.Before(cutoff) {
			continue
		}
		if err := s.Delete(m.ID); err != nil {
			return removed, err
		}
		removed++
	}
	if removed > 0 {
		s.dropCurrentRefs()
	}
	return removed, nil
}

// dropCurrentRefs rewrites current.json without pointers to sessions that no
// longer exist, so a resumed session never resurrects a removed id.
func (s *Store) dropCurrentRefs() {
	m, err := s.currentMap()
	if err != nil || len(m) == 0 {
		return
	}
	kept := make(map[string]string, len(m))
	for cwd, id := range m {
		if _, err := os.Stat(s.path(id)); err == nil {
			kept[cwd] = id
		}
	}
	if len(kept) == len(m) {
		return
	}
	data, err := json.MarshalIndent(kept, "", "  ")
	if err != nil {
		return
	}
	p := filepath.Join(s.dir, "current.json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err == nil {
		_ = os.Rename(tmp, p)
	}
}

// Fork deep-copies a session with a new ID, sets Parent to the original, and
// appends "(fork)" to the title.
func (s *Store) Fork(id string) (*Session, error) {
	orig, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	fork := *orig
	fork.ID = NewID()
	fork.Parent = orig.ID
	fork.Title = orig.Title + " (fork)"
	fork.Created = time.Now()
	fork.Updated = fork.Created
	// Deep-copy messages so mutations don't leak back.
	fork.Messages = make([]provider.Message, len(orig.Messages))
	for i, m := range orig.Messages {
		cp := m
		cp.Content = make([]provider.ContentBlock, len(m.Content))
		copy(cp.Content, m.Content)
		fork.Messages[i] = cp
	}
	fork.Snapshots = nil
	if err := s.Save(&fork); err != nil {
		return nil, err
	}
	return &fork, nil
}

// Search returns sessions whose title or message text contains query
// (case-insensitive substring match), newest first.
func (s *Store) Search(query string) ([]Meta, error) {
	q := strings.ToLower(query)
	metas, err := s.List("")
	if err != nil {
		return nil, err
	}
	var out []Meta
	for _, meta := range metas {
		if strings.Contains(strings.ToLower(meta.Title), q) {
			out = append(out, meta)
			continue
		}
		// Fast path: the search sidecar holds the lowercased message text,
		// so matching does not require unmarshalling the full session JSON.
		// Sessions saved before the sidecar existed fall back to Load().
		idxText, idxErr := os.ReadFile(s.searchPath(meta.ID))
		if idxErr == nil {
			if strings.Contains(string(idxText), q) {
				out = append(out, meta)
			}
			continue
		}
		sess, err := s.Load(meta.ID)
		if err != nil {
			continue
		}
		for _, m := range sess.Messages {
			if strings.Contains(strings.ToLower(m.Text()), q) {
				out = append(out, meta)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// Rename updates a session's title.
func (s *Store) Rename(id, title string) error {
	sess, err := s.Load(id)
	if err != nil {
		return err
	}
	sess.Title = title
	return s.Save(sess)
}

// SetFavorite toggles the favorite flag on a session.
func (s *Store) SetFavorite(id string, fav bool) error {
	sess, err := s.Load(id)
	if err != nil {
		return err
	}
	sess.Favorite = fav
	return s.Save(sess)
}

// SetCategory assigns a human-readable category to a session. An empty
// category intentionally means uncategorized.
func (s *Store) SetCategory(id, category string) error {
	sess, err := s.Load(id)
	if err != nil {
		return err
	}
	sess.Category = strings.TrimSpace(category)
	return s.Save(sess)
}

func metaFrom(s *Session) Meta {
	lastPrompt := ""
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Role == provider.RoleUser {
			lastPrompt = s.Messages[i].Text()
			break
		}
	}
	if len(lastPrompt) > 1000 {
		lastPrompt = lastPrompt[:1000]
	}
	return Meta{
		ID: s.ID, Title: s.Title, Cwd: s.Cwd, Model: s.Model,
		Parent: s.Parent, Category: s.Category, Messages: len(s.Messages), Created: s.Created, Updated: s.Updated,
		Favorite: s.Favorite, LastPrompt: lastPrompt,
	}
}
