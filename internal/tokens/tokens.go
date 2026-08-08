// Package tokens centralizes provider-facing token estimation.
package tokens

import (
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ron2111/omnitoken"
)

// Encoding identifies the tokenizer vocabulary used by a provider/model.
type Encoding string

const (
	EncodingCl100kBase Encoding = omnitoken.EncodingCL100KBase
	EncodingO200kBase  Encoding = omnitoken.EncodingO200KBase
)

// Result describes a token count and whether the exact vocabulary was used.
type Result struct {
	Count    int
	Encoding Encoding
	Exact    bool
}

var engines sync.Map

// Count returns an exact count for the built-in encodings and a conservative
// Unicode-safe estimate for unknown encodings. It never downloads assets or
// invokes an external process on the request path.
func Count(text string, encoding Encoding) Result {
	if count, ok := memoLookup(text, encoding); ok {
		return Result{Count: count, Encoding: encoding, Exact: true}
	}
	if engine, ok := engines.Load(string(encoding)); ok {
		count := engine.(omnitoken.ModelEngine).CountTokens(text)
		memoStore(text, encoding, count)
		return Result{Count: count, Encoding: encoding, Exact: true}
	}

	engine, err := omnitoken.ForEncoding(string(encoding))
	if err == nil {
		engines.Store(string(encoding), engine)
		count := engine.CountTokens(text)
		memoStore(text, encoding, count)
		return Result{Count: count, Encoding: encoding, Exact: true}
	}

	return Result{Count: conservativeFallback(text), Encoding: encoding}
}

// ---------- exact-count memo ----------

// The agent BPE-counts the same stable prefix several times per turn — once
// per pass in countMessages, history.Retain and ChooseBoundaries — so the
// exact count for repeated texts is memoized here. The key is the counted
// text itself (no copying); entries are bounded by count and by total text
// bytes with FIFO eviction, so a long session cannot grow the cache past its
// budget. Small texts are skipped: counting them is cheap and they would
// only churn the FIFO.
type countMemo struct {
	entries map[string]int
	order   []string
	bytes   int
}

const (
	memoMaxEntries = 4096
	memoMaxBytes   = 32 << 20 // 32 MiB of cached text
	memoMinText    = 32
)

var (
	memoMu sync.Mutex
	memos  = map[Encoding]*countMemo{}
)

func memoLookup(text string, encoding Encoding) (int, bool) {
	if len(text) < memoMinText {
		return 0, false
	}
	memoMu.Lock()
	defer memoMu.Unlock()
	memo := memos[encoding]
	if memo == nil {
		return 0, false
	}
	count, ok := memo.entries[text]
	return count, ok
}

func memoStore(text string, encoding Encoding, count int) {
	if len(text) < memoMinText {
		return
	}
	memoMu.Lock()
	defer memoMu.Unlock()
	memo := memos[encoding]
	if memo == nil {
		memo = &countMemo{entries: map[string]int{}}
		memos[encoding] = memo
	}
	if _, exists := memo.entries[text]; exists {
		return
	}
	memo.entries[text] = count
	memo.order = append(memo.order, text)
	memo.bytes += len(text)
	for memo.bytes > memoMaxBytes || len(memo.order) > memoMaxEntries {
		oldest := memo.order[0]
		memo.order = memo.order[1:]
		memo.bytes -= len(oldest)
		delete(memo.entries, oldest)
	}
}

// EncodingForModel selects the exact BPE vocabulary that matches the tokenizer
// family of a model id. OpenAI's modern models (gpt-4o, gpt-5, o-series,
// codex) tokenize with o200k_base; everything else falls back to cl100k_base.
func EncodingForModel(modelID string) Encoding {
	id := strings.ToLower(modelID)
	for _, marker := range []string{"gpt-4o", "gpt-5", "o1", "o3", "o4", "codex", "o200k"} {
		if strings.Contains(id, marker) {
			return EncodingO200kBase
		}
	}
	return EncodingCl100kBase
}

func conservativeFallback(text string) int {
	if text == "" {
		return 0
	}
	runeCount := utf8.RuneCountInString(text)
	byteEstimate := (len(text) + 3) / 4
	if byteEstimate > runeCount {
		return byteEstimate
	}
	return runeCount
}
