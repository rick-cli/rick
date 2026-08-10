// Package contextbudget manages the provider-facing conversation view:
// content-addressed deduplication of repeated tool outputs, stable cache
// boundary selection for prompt caching, reversible live-zone compression,
// and the tool_use/tool_result atomic-pair safety invariant.
//
// The budget never mutates canonical history; it only builds the view that is
// serialized into a provider request.
package contextbudget

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"rick/internal/provider"
	"rick/internal/tokens"
)

// Options configures a Budget.
type Options struct {
	// Enabled is the master switch. Zero value leaves the budget disabled so
	// callers can opt in explicitly (or via New).
	Enabled bool
	// MaxLivePayloads caps the reversible live-zone store.
	MaxLivePayloads int
	// MaxCABPayloads caps the content-addressed store.
	MaxCABPayloads int
	// MinDedupBytes is the minimum tool-result size considered for
	// content-addressed deduplication.
	MinDedupBytes int
	// MinStableTurns is how many consecutive identical observations a history
	// prefix needs before it becomes a cache boundary. Defaults to 1: the
	// system prompt, tool list, and volatile tail are frozen per session, so
	// a single observation is enough to trust the prefix.
	MinStableTurns int
	// LiveZoneTurns is how many newest logical turns are excluded from cache
	// boundaries (the volatile tail of the conversation). Defaults to 1.
	LiveZoneTurns int
	// MaxStableBytes is the minimum serialized prefix size worth caching.
	MaxStableBytes int
	// MaxBoundaries caps the cache breakpoints emitted per request. Anthropic
	// allows 4 breakpoints total per request (system + tools + message
	// boundaries), so the message-boundary budget is capped at 2.
	MaxBoundaries int
	// MinCacheTokens is the minimum prefix size (in tokens) a breakpoint must
	// guard; providers silently ignore breakpoints on smaller prefixes. When
	// Encoding is empty the prefix bytes/4 estimate is used.
	MinCacheTokens int
	// Encoding is the tokenizer used for the MinCacheTokens guard. Empty uses
	// the byte/4 estimate.
	Encoding tokens.Encoding
	// LiveZoneCapBytes bounds live-zone compressed tool output.
	LiveZoneCapBytes int
	// PruneMinReclaimBytes is the minimum provider-facing bytes a proactive
	// prune must reclaim before it commits a rewrite of already-sent history.
	// Committing is a deliberate cache-boundary event (the head rewrites
	// once), so it must be worth it; below the floor the prune waits for the
	// history to grow more. Default 4 KiB.
	PruneMinReclaimBytes int
	// PruneMinResultBytes is the minimum size of an old tool result that is
	// considered for a deterministic 1-line summary. Results at or below
	// MinDedupBytes are left verbatim. Default 8 KiB.
	PruneMinResultBytes int
	// PruneLiveZoneTurns is how many newest logical turns are never pruned
	// (the volatile tail stays byte-exact so the cache prefix keeps
	// extending). Default 2.
	PruneLiveZoneTurns int
}

func (o Options) withDefaults() Options {
	o.Enabled = true
	if o.MaxLivePayloads <= 0 {
		o.MaxLivePayloads = 512
	}
	if o.MaxCABPayloads <= 0 {
		o.MaxCABPayloads = 1024
	}
	if o.MinDedupBytes <= 0 {
		o.MinDedupBytes = 2048
	}
	if o.MinStableTurns <= 0 {
		o.MinStableTurns = 1
	}
	if o.LiveZoneTurns <= 0 {
		o.LiveZoneTurns = 1
	}
	if o.MaxStableBytes <= 0 {
		o.MaxStableBytes = 4096
	}
	if o.MaxBoundaries <= 0 {
		o.MaxBoundaries = 2
	}
	if o.MinCacheTokens <= 0 {
		o.MinCacheTokens = 1024
	}
	if o.LiveZoneCapBytes <= 0 {
		o.LiveZoneCapBytes = 8 << 10
	}
	if o.PruneMinReclaimBytes <= 0 {
		o.PruneMinReclaimBytes = 4 << 10
	}
	if o.PruneMinResultBytes <= 0 {
		o.PruneMinResultBytes = 8 << 10
	}
	if o.PruneLiveZoneTurns <= 0 {
		o.PruneLiveZoneTurns = 2
	}
	return o
}

// Budget is a per-session context manager. It is safe for concurrent use
// because tool execution may run in parallel.
type Budget struct {
	mu        sync.Mutex
	opts      Options
	live      map[string]string
	liveOrd   []string
	cab       map[string]string
	cabOrd    []string
	stability map[int]*prefixState
	// dedupIDs records the persistent provider-facing decision per
	// tool_result (keyed by tool_use_id): true means the result is
	// serialized as a self-contained pointer. Deciding once and never
	// re-evaluating keeps every message's bytes byte-identical across turns.
	dedupIDs map[string]bool
	// verbatim marks payload hashes that were serialized verbatim at least
	// once, so a repeated payload collapses to a pointer even after the
	// original copy is trimmed from the view.
	verbatim map[string]bool
	// pruneArmed is true while a proactive prune may commit a rewrite of the
	// already-sent head. After a commit it disarms until the history regrows
	// a full trigger-sized runway (measured in provider-facing bytes), so
	// prunes are episodic (one cache boundary) instead of firing every turn.
	pruneArmed    bool
	pruneRearmAt  int
	pruneSumBytes int
	pruneSumCalls int
	// summarizedIDs records tool_use_ids whose result was already replaced by
	// a deterministic 1-line summary, so a result is summarized exactly once
	// and its bytes never change between turns.
	summarizedIDs map[string]bool
	// Per-message analysis of the previous ChooseBoundaries input, valid for
	// the shared prefix of the next call: message hashes, serialized byte
	// lengths, token lengths, cumulative prefix bytes/tokens, and the
	// combined prefix hash at each boundary index. Steady turns only append
	// at the tail, so the byte/token/prefix work for the stable head is
	// reused instead of recomputed; byte identity is proven by re-hashing
	// the prefix, never assumed.
	lastHashes     []string
	lastByteLen    []int
	lastTokenLen   []int
	lastPrefixByte []int
	lastPrefixTok  []int
	lastPrefixHash []string
}

type prefixState struct {
	hash  string
	count int
}

// New builds an enabled Budget with defaults applied.
func New(opts Options) *Budget {
	return &Budget{
		opts:          opts.withDefaults(),
		live:          map[string]string{},
		cab:           map[string]string{},
		stability:     map[int]*prefixState{},
		dedupIDs:      map[string]bool{},
		verbatim:      map[string]bool{},
		summarizedIDs: map[string]bool{},
		pruneArmed:    true,
	}
}

// Enabled reports whether the budget applies any transformation.
func (b *Budget) Enabled() bool { return b != nil && b.opts.Enabled }

// StoreLive keeps the original payload under key for reversible retrieval.
func (b *Budget) StoreLive(key, original string) {
	if b == nil || !b.opts.Enabled || key == "" || original == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.live[key]; exists {
		b.live[key] = original
		return
	}
	b.live[key] = original
	b.liveOrd = append(b.liveOrd, key)
	for len(b.liveOrd) > b.opts.MaxLivePayloads {
		oldest := b.liveOrd[0]
		b.liveOrd = b.liveOrd[1:]
		delete(b.live, oldest)
	}
}

// LiveOriginal returns a stored live-zone original, if still retained.
func (b *Budget) LiveOriginal(key string) (string, bool) {
	if b == nil {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	original, ok := b.live[key]
	return original, ok
}

// LiveKeys lists stored live-zone keys in insertion order.
func (b *Budget) LiveKeys() []string {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.liveOrd...)
}

// Hash returns the content-address of a payload (first 16 hex chars).
func Hash(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:8])
}

// StoredPayload returns a content-addressed payload, if still retained.
func (b *Budget) StoredPayload(hash string) (string, bool) {
	if b == nil {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	payload, ok := b.cab[hash]
	return payload, ok
}

// DedupResult reports what ApplyDedup changed.
type DedupResult struct {
	View       []provider.Message
	SavedBytes int
	Replaced   int
}

// ApplyDedup replaces repeated large tool_result payloads within one
// transcript with a self-contained reference. The replacement decision is
// persistent for each tool_result (keyed by its tool_use_id) and is made
// exactly once, the first time the result enters the view: a message's bytes
// are decided when it is first serialized and never change afterwards — even
// when head trimming removes the payload's original copy — so the
// provider-facing prefix stays byte-identical across turns and the automatic
// cache keeps hitting. The original payload stays retrievable from the
// content-addressed store.
func (b *Budget) ApplyDedup(messages []provider.Message) DedupResult {
	result := DedupResult{View: append([]provider.Message(nil), messages...)}
	if !b.Enabled() {
		return result
	}

	// Freshly appearing duplicates within this one view (doesn't cross calls).
	seenThisView := map[string]bool{}
	for index := range result.View {
		for blockIndex := range result.View[index].Content {
			block := &result.View[index].Content[blockIndex]
			if block.Type != "tool_result" || len(block.Content) < b.opts.MinDedupBytes {
				continue
			}
			hash := Hash(block.Content)
			replace := b.decideDedup(block.ToolUseID, block.Content, hash, seenThisView)
			if !replace {
				continue
			}
			originalLen := len(block.Content)
			block.Content = fmt.Sprintf("[duplicate payload sha256:%s; identical to an earlier tool result — retrieve via retrieve_uncompressed_context key %s]", hash, hash)
			result.SavedBytes += originalLen - len(block.Content)
			result.Replaced++
		}
	}
	return result
}

// PruneResult reports what a proactive prune changed.
type PruneResult struct {
	View       []provider.Message
	SavedBytes int
	Summarized int
	Committed  bool
}

// PruneOldToolResults deterministically shrinks old, bulky tool results in
// the provider view: each result older than the prune live zone and larger
// than PruneMinResultBytes is replaced once by a 1-line informative summary
// (tool name + command/path + exit state), with the original stored under
// its content address so retrieve_uncompressed_context can still fetch it.
//
// Cache contract: rewriting already-sent history is a deliberate cache
// boundary, so a commit only happens when the estimated reclaim is at least
// PruneMinReclaimBytes AND the budget is armed. After a commit it disarms
// until the history regrows a full trigger-sized runway (measured in
// provider-facing bytes), making prunes episodic — one boundary per commit,
// never a per-turn rewrite. Every summary is decided exactly once per
// tool_use_id so the surviving bytes never change between turns.
func (b *Budget) PruneOldToolResults(messages []provider.Message) PruneResult {
	result := PruneResult{View: append([]provider.Message(nil), messages...)}
	if !b.Enabled() {
		return result
	}

	// Group tool_use/tool_result pairs so the live-zone boundary never
	// splits a pair, and count how much of the head is reclaimable.
	groups := logicalGroups(messages)
	cutoff := len(groups) - b.opts.PruneLiveZoneTurns
	if cutoff <= 0 {
		return result
	}

	// Estimate reclaim with the current byte sizes first (id-aware, so a
	// result already summarized or dedup'd is not re-counted).
	reclaim := 0
	for gi, g := range groups {
		if gi >= cutoff {
			break
		}
		for mi := g.start; mi < len(messages) && mi <= g.start+1; mi++ {
			for _, block := range messages[mi].Content {
				if block.Type != "tool_result" || len(block.Content) < b.opts.PruneMinResultBytes {
					continue
				}
				if b.summarizedIDs[block.ToolUseID] {
					continue
				}
				summary := summarizeToolResult(block.ToolUseID, block.Content)
				if len(summary) >= len(block.Content) {
					continue
				}
				reclaim += len(block.Content) - len(summary)
			}
		}
	}
	b.mu.Lock()
	armed := b.pruneArmed
	if !armed && b.pruneSumBytes >= b.pruneRearmAt {
		armed = true
		b.pruneArmed = true
	}
	b.mu.Unlock()
	if !armed {
		return result
	}
	if reclaim < b.opts.PruneMinReclaimBytes {
		return result
	}

	// Commit: apply summaries, one per tool_use_id, storing the original.
	saved := 0
	summarized := 0
	for gi, g := range groups {
		if gi >= cutoff {
			break
		}
		// A group is messages[g.start .. g.start+1] (a lone message or a
		// tool_use/tool_result pair).
		for mi := g.start; mi < len(result.View) && mi <= g.start+1; mi++ {
			for bi := range result.View[mi].Content {
				block := &result.View[mi].Content[bi]
				if block.Type != "tool_result" || len(block.Content) < b.opts.PruneMinResultBytes {
					continue
				}
				if b.summarizedIDs[block.ToolUseID] {
					continue
				}
				summary := summarizeToolResult(block.ToolUseID, block.Content)
				if len(summary) >= len(block.Content) {
					continue
				}
				b.storeCABLocked(Hash(block.Content), block.Content)
				saved += len(block.Content) - len(summary)
				summarized++
				block.Content = summary
				b.mu.Lock()
				b.summarizedIDs[block.ToolUseID] = true
				b.mu.Unlock()
			}
		}
	}

	// Rearm after the history regrows the reclaimed amount.
	b.mu.Lock()
	b.pruneArmed = false
	b.pruneRearmAt = saved
	b.pruneSumBytes = 0
	b.pruneSumCalls = 0
	b.mu.Unlock()

	result.SavedBytes = saved
	result.Summarized = summarized
	result.Committed = true
	return result
}

// NotePruneGrowth feeds the prune-rearm accumulator with the provider-facing
// byte growth of the view since the last prune commit. The budget rearms
// once the growth covers the reclaimed bytes, so the next prune fires on a
// fresh trigger-sized runway instead of churning the same head.
func (b *Budget) NotePruneGrowth(viewBytes int) {
	if b == nil || !b.Enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pruneArmed {
		return
	}
	b.pruneSumBytes += viewBytes
	b.pruneSumCalls++
}

// summarizeToolResult renders a deterministic one-line summary of a tool
// result: the tool id, the tool's command/path when recoverable from common
// shapes, and the size of the original. The bytes are stable for a given
// payload so re-summarizing the same call id never changes the prefix.
func summarizeToolResult(id, content string) string {
	line := "tool result"
	if id != "" {
		line += " " + id
	}
	// Peek at the leading line for a command/path hint (bash command, file
	// path, package name) so the model keeps the actionable part.
	hint := ""
	for _, candidate := range strings.Split(content, "\n") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if len(candidate) > 120 {
			candidate = candidate[:120]
		}
		hint = candidate
		break
	}
	if hint != "" {
		line += ": " + hint
	}
	line += fmt.Sprintf(" [%d bytes; retrieve via retrieve_uncompressed_context key %s]", len(content), Hash(content))
	if len(line) > 512 {
		line = line[:512]
	}
	return "[summarized] " + line
}
func (b *Budget) decideDedup(id, content, hash string, seenThisView map[string]bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id != "" {
		if replaced, ok := b.dedupIDs[id]; ok {
			return replaced
		}
	}
	replace := b.verbatim[hash] || seenThisView[hash]
	if id != "" {
		b.dedupIDs[id] = replace
	}
	if !replace {
		seenThisView[hash] = true
		b.verbatim[hash] = true
		b.storeCABLocked(hash, content)
	}
	return replace
}

// storeCABLocked records a payload under its content address; callers must
// hold b.mu.
func (b *Budget) storeCABLocked(hash, payload string) {
	if _, exists := b.cab[hash]; exists {
		return
	}
	b.cab[hash] = payload
	b.cabOrd = append(b.cabOrd, hash)
	for len(b.cabOrd) > b.opts.MaxCABPayloads {
		oldest := b.cabOrd[0]
		b.cabOrd = b.cabOrd[1:]
		delete(b.cab, oldest)
	}
}

// Boundary selection ------------------------------------------------

// messageHash is a stable per-message fingerprint used to detect prefix
// stability across requests. The serialized bytes come from the shared
// marshalled-bytes memo so the boundary pass does not re-marshal messages the
// token-counting passes already serialized.
func messageHash(message provider.Message) string {
	return Hash(string(tokens.Marshal(message)))
}

// messageHashBytes returns the stable per-message fingerprint together with
// the raw serialized bytes that produced it, so the boundary pass never
// marshals a message twice (once for the hash, once for its byte length).
func messageHashBytes(message provider.Message) (string, []byte) {
	raw := tokens.Marshal(message)
	return Hash(string(raw)), raw
}

// ChooseBoundaries returns message indices that delimit a stable history
// prefix worth caching. A boundary at index i means "cache everything before
// message i"; the returned map has true at those indices.
//
// Stability is measured across calls: a prefix must be byte-identical for
// MinStableTurns consecutive observations before it is proposed, and the live
// zone (the newest turns) is never a boundary.
func (b *Budget) ChooseBoundaries(messages []provider.Message) map[int]bool {
	out := map[int]bool{}
	if !b.Enabled() || len(messages) < 2 {
		return out
	}

	n := len(messages)
	hashes := make([]string, n)
	byteLen := make([]int, n)
	tokenLen := make([]int, n)
	prefixByte := make([]int, n)
	prefixTok := make([]int, n)
	prefixHash := make([]string, n)

	b.mu.Lock()
	lastHashes, lastByteLen, lastTokenLen, lastPrefixByte, lastPrefixTok, lastPrefixHash :=
		b.lastHashes, b.lastByteLen, b.lastTokenLen, b.lastPrefixByte, b.lastPrefixTok, b.lastPrefixHash
	b.mu.Unlock()

	// Reuse the previous call's per-message analysis for the shared prefix:
	// steady turns only append at the tail, so the stable head is re-hashed
	// (byte identity is proven, not assumed) but its byte/token/prefix work
	// is not repeated. The first divergent message invalidates the cache
	// from that point on.
	shared := len(lastHashes)
	if shared > n {
		shared = n
	}
	for i := 0; i < shared; i++ {
		hashes[i] = messageHash(messages[i])
		if hashes[i] != lastHashes[i] {
			shared = i
			break
		}
	}
	for i := 0; i < shared; i++ {
		byteLen[i] = lastByteLen[i]
		tokenLen[i] = lastTokenLen[i]
		prefixByte[i] = lastPrefixByte[i]
		prefixTok[i] = lastPrefixTok[i]
		prefixHash[i] = lastPrefixHash[i]
	}

	prefixBytes := 0
	prefixTokens := 0
	if shared > 0 {
		prefixBytes = lastPrefixByte[shared-1]
		prefixTokens = lastPrefixTok[shared-1]
	}
	encoding := b.opts.Encoding
	for i := shared; i < n; i++ {
		hash, raw := messageHashBytes(messages[i])
		hashes[i] = hash
		byteLen[i] = len(raw)
		if encoding != "" {
			tokenLen[i] = tokens.Count(string(raw), encoding).Count + 4
		} else {
			tokenLen[i] = len(raw) / 4
		}
		prefixBytes += byteLen[i]
		prefixTokens += tokenLen[i]
		prefixByte[i] = prefixBytes
		prefixTok[i] = prefixTokens
	}
	for i := shared; i < n; i++ {
		prefixHash[i] = combineHashes(hashes[:i])
	}

	candidates := boundaryCandidates(messages, b.opts.LiveZoneTurns)
	if len(candidates) > 32 {
		candidates = candidates[:32]
	}
	chosen := make([]int, 0, len(candidates))
	lastChosen := -1
	lastChosenBytes := 0
	for _, index := range candidates {
		if index >= n {
			continue
		}
		prefixBytes := prefixByte[index-1]
		prefixTokens := prefixTok[index-1]
		if prefixBytes < b.opts.MaxStableBytes || prefixTokens < b.opts.MinCacheTokens {
			continue
		}
		state := b.observePrefixHash(index, prefixHash[index])
		if state.count < b.opts.MinStableTurns {
			continue
		}
		// Keep boundaries spread apart so the cache has real content between
		// consecutive breakpoints.
		if lastChosen >= 0 && prefixBytes-lastChosenBytes < b.opts.MaxStableBytes {
			continue
		}
		chosen = append(chosen, index)
		lastChosen = index
		lastChosenBytes = prefixBytes
		if len(chosen) >= b.opts.MaxBoundaries {
			break
		}
	}
	for _, index := range chosen {
		out[index] = true
	}

	b.mu.Lock()
	b.lastHashes = hashes
	b.lastByteLen = byteLen
	b.lastTokenLen = tokenLen
	b.lastPrefixByte = prefixByte
	b.lastPrefixTok = prefixTok
	b.lastPrefixHash = prefixHash
	b.mu.Unlock()
	return out
}

// boundaryCandidates lists message indices that end a logical group (never
// splitting a tool_use/tool_result pair) and sit outside the live zone.
func boundaryCandidates(messages []provider.Message, liveTurns int) []int {
	groups := logicalGroups(messages)
	cutoff := len(groups) - liveTurns
	if cutoff < 1 {
		return nil
	}
	var indices []int
	for index := 1; index < cutoff; index++ {
		indices = append(indices, groups[index].start)
	}
	return indices
}

// SeedStability primes the stability tracker with the transcript that was
// last sent to the provider, so a resumed session emits cache boundaries on
// its first turn instead of starting cold at a 100% miss.
func (b *Budget) SeedStability(messages []provider.Message) {
	if b == nil || !b.opts.Enabled || len(messages) < 2 {
		return
	}
	hashes := make([]string, len(messages))
	for i, message := range messages {
		hashes[i] = messageHash(message)
	}
	for _, index := range boundaryCandidates(messages, b.opts.LiveZoneTurns) {
		b.mu.Lock()
		b.stability[index] = &prefixState{hash: combineHashes(hashes[:index]), count: b.opts.MinStableTurns}
		b.mu.Unlock()
	}
}

// observePrefixHash records one observation of the prefix ending at index
// (its combined hash is supplied by the caller, which already computed it
// for the boundary pass) and returns the running stability state for it.
func (b *Budget) observePrefixHash(index int, hash string) *prefixState {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.stability[index]
	if state == nil {
		state = &prefixState{hash: hash, count: 1}
		b.stability[index] = state
		return state
	}
	if state.hash == hash {
		state.count++
	} else {
		state.hash = hash
		state.count = 1
	}
	return state
}

func combineHashes(hashes []string) string {
	sum := sha256.New()
	for _, h := range hashes {
		sum.Write([]byte(h))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil)[:8])
}

// logicalGroups groups messages so tool_use and its tool_result are atomic.
type group struct {
	start int
}

func logicalGroups(messages []provider.Message) []group {
	groups := make([]group, 0, len(messages))
	for index := 0; index < len(messages); index++ {
		start := index
		if hasBlock(messages[index], "tool_use") && index+1 < len(messages) && hasBlock(messages[index+1], "tool_result") {
			index++
		}
		groups = append(groups, group{start: start})
	}
	return groups
}

func hasBlock(message provider.Message, kind string) bool {
	for _, block := range message.Content {
		if block.Type == kind {
			return true
		}
	}
	return false
}

// Pair safety ------------------------------------------------------

// VerifyPairSafety returns an error when any tool_result in messages lacks its
// paired tool_use in the immediately preceding message of the same logical
// group, or when any tool_use is the final message of the transcript.
func VerifyPairSafety(messages []provider.Message) error {
	for index := 0; index < len(messages); index++ {
		message := messages[index]
		if hasBlock(message, "tool_use") {
			if index+1 >= len(messages) {
				return fmt.Errorf("contextbudget: tool_use at message %d has no tool_result", index)
			}
			next := messages[index+1]
			if !hasBlock(next, "tool_result") {
				return fmt.Errorf("contextbudget: tool_use at message %d not followed by tool_result", index)
			}
			index++ // skip the paired result
		}
		if hasBlock(message, "tool_result") {
			// The pairing above already consumed results after a tool_use.
			// A result here means its tool_use was evicted.
			return fmt.Errorf("contextbudget: orphaned tool_result at message %d", index)
		}
	}
	return nil
}

// Live-zone compression ---------------------------------------------

// CompressLive returns a compact, reversible view of a fresh tool result. The
// original payload is stored under key so the model can pull it back. The
// boolean reports whether the output actually changed.
func (b *Budget) CompressLive(key, text string) (string, bool) {
	if !b.Enabled() || text == "" {
		return text, false
	}
	compressed := minifyJSON(text)
	if compressed == text {
		compressed = capLive(key, text, b.opts.LiveZoneCapBytes)
	}
	changed := compressed != text
	if changed {
		b.StoreLive(key, text)
	}
	return compressed, changed
}

// minifyJSON applies a structural mask to bulky JSON: whitespace is removed
// and long arrays/objects are summarized while preserving shape.
func minifyJSON(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return text
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return text
	}
	return maskJSON(value, 0)
}

func maskJSON(value any, depth int) string {
	if depth > 3 {
		return "…"
	}
	switch value := value.(type) {
	case nil:
		return "null"
	case bool:
		if value {
			return "true"
		}
		return "false"
	case float64:
		return fmt.Sprintf("%v", value)
	case string:
		if len(value) > 160 {
			// Truncate on a rune boundary so multi-byte characters are never
			// sliced in half; json.Marshal would otherwise escape the broken
			// tail as a U+FFFD replacement char and corrupt the value.
			truncated, _ := json.Marshal(cutRunes(value, 157))
			return string(truncated) + "…"
		}
		encoded, _ := json.Marshal(value)
		return string(encoded)
	case []any:
		if len(value) == 0 {
			return "[]"
		}
		if len(value) <= 6 {
			parts := make([]string, 0, len(value))
			for _, item := range value {
				parts = append(parts, maskJSON(item, depth+1))
			}
			return "[" + strings.Join(parts, ",") + "]"
		}
		head := value[:2]
		parts := make([]string, 0, len(head)+1)
		for _, item := range head {
			parts = append(parts, maskJSON(item, depth+1))
		}
		return fmt.Sprintf("[%s,…<%d more items>]", strings.Join(parts, ","), len(value)-2)
	case map[string]any:
		if len(value) == 0 {
			return "{}"
		}
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) > 8 {
			keys = keys[:8]
		}
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%q:%s", key, maskJSON(value[key], depth+1)))
		}
		if len(keys) < len(value) {
			parts = append(parts, fmt.Sprintf("…<%d more keys>", len(value)-len(keys)))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	return "…"
}

func capLive(key, text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	omitted := len(text) - limit
	marker := fmt.Sprintf("\n… <live-zone compressed; %d bytes omitted; retrieve original via retrieve_uncompressed_context key %s>", omitted, key)
	if len(marker) >= limit {
		return marker
	}
	keep := limit - len(marker)
	headBytes := keep / 2
	tailBytes := keep - headBytes
	head := safeCut(text, headBytes, false)
	tail := safeCut(text, tailBytes, true)
	return head + marker + tail
}

// safeCut returns a UTF-8-safe prefix (fromStart) or suffix (toEnd) of text.
func safeCut(text string, limit int, toEnd bool) string {
	if limit <= 0 {
		return ""
	}
	if limit >= len(text) {
		return text
	}
	if !toEnd {
		for limit > 0 && !utf8.RuneStart(text[limit]) {
			limit--
		}
		return text[:limit]
	}
	start := len(text) - limit
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}

// cutRunes returns the longest rune-aligned prefix of text no longer than
// limit bytes, walking back from the byte cut so a multi-byte character is
// never sliced in half.
func cutRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if limit >= len(text) {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}
