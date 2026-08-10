// Package tokens centralizes provider-facing token estimation.
package tokens

import (
	"encoding/json"
	"strings"
	"sync"
	"unicode/utf8"

	"rick/internal/provider"

	"github.com/ron2111/omnitoken"
)

// Encoding identifies the tokenizer vocabulary used by a provider/model.
type Encoding string

// ImageTokenEstimate is the flat token cost attributed to a base64 image
// block. Counting the raw base64 bytes as text would massively over-estimate
// (a 1 MB image ≈ 250K tokens) and trigger premature trimming; counting it as
// zero under-budgets vision sessions. The flat estimate matches how vision
// models actually bill images (a small fixed cost each).
const ImageTokenEstimate = 1600

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

// ---------- marshalled-bytes memo ----------

// Marshal returns the canonical JSON bytes for a provider.Message, memoized
// by its marshalled text so the same stable message is never re-serialized
// across the per-turn passes (countMessages, history.Retain, ChooseBoundaries
// each marshal every message). Entries are bounded by count and bytes with
// FIFO eviction like the count memo.
func Marshal(message provider.Message) []byte {
	text := message.Text()
	if len(text) < memoMinText {
		raw, err := json.Marshal(message)
		if err != nil {
			return []byte("{}")
		}
		return raw
	}
	key := text

	marshalMu.Lock()
	if raw, ok := marshalLRU.entries[key]; ok {
		marshalMu.Unlock()
		return raw
	}
	marshalMu.Unlock()

	raw, err := json.Marshal(message)
	if err != nil {
		return []byte("{}")
	}
	marshalStore(key, raw)
	return raw
}

// CountMessage returns the token count of a message for budget decisions.
// Base64 image blocks are charged at the flat ImageTokenEstimate instead of
// their byte length, so a vision turn is neither over-budgeted (base64 is
// not text) nor under-budgeted (images do cost tokens).
func CountMessage(message provider.Message, encoding Encoding) int {
	total := Count(string(Marshal(message)), encoding).Count + 4
	for _, block := range message.Content {
		if block.Type == "image" && block.Data != "" {
			// Replace the counted base64 bytes with the flat image cost:
			// subtract what the base64 data added, add the flat estimate.
			dataTokens := Count(block.Data, encoding).Count
			total += ImageTokenEstimate - dataTokens
		}
	}
	if total < 0 {
		total = 0
	}
	return total
}

type bytesMemo struct {
	entries map[string][]byte
	order   []string
	bytes   int
}

const (
	marshalMemoMaxEntries = 4096
	marshalMemoMaxBytes   = 32 << 20
)

var (
	marshalMu  sync.Mutex
	marshalLRU = bytesMemo{entries: map[string][]byte{}}
)

func marshalStore(key string, raw []byte) {
	marshalMu.Lock()
	defer marshalMu.Unlock()
	if _, exists := marshalLRU.entries[key]; exists {
		return
	}
	marshalLRU.entries[key] = raw
	marshalLRU.order = append(marshalLRU.order, key)
	marshalLRU.bytes += len(raw)
	for marshalLRU.bytes > marshalMemoMaxBytes || len(marshalLRU.order) > marshalMemoMaxEntries {
		oldest := marshalLRU.order[0]
		marshalLRU.order = marshalLRU.order[1:]
		marshalLRU.bytes -= len(marshalLRU.entries[oldest])
		delete(marshalLRU.entries, oldest)
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

// conservativeFallback estimates tokens for an unknown encoding without a
// BPE vocabulary. It is CJK-aware: Han/Hangul/Kana codepoints cost roughly one
// token each (vs four characters per token for Latin text), so a transcript
// that is mostly CJK is not over-estimated by the byte/4 rule and does not
// trigger premature trimming or distillation.
func conservativeFallback(text string) int {
	if text == "" {
		return 0
	}
	runeCount := utf8.RuneCountInString(text)
	byteEstimate := (len(text) + 3) / 4
	denseEstimate := cjkDenseTokens(text)
	// The CJK estimate is the tighter bound when the text is dense; the
	// byte/4 rule dominates for long ASCII strings. Take the conservative
	// (larger) of the two so we never under-budget the provider.
	estimate := byteEstimate
	if denseEstimate > estimate {
		estimate = denseEstimate
	}
	if estimate < runeCount {
		estimate = runeCount
	}
	return estimate
}

// cjkDenseTokens counts Han, Hangul and Kana codepoints (roughly one token
// each in most modern tokenizers) plus one token per rune for the rest.
func cjkDenseTokens(text string) int {
	total := 0
	for _, r := range text {
		switch {
		case r >= 0x4E00 && r <= 0x9FFF, // CJK Unified Ideographs
			r >= 0x3400 && r <= 0x4DBF, // CJK Ext A
			r >= 0xAC00 && r <= 0xD7AF, // Hangul syllables
			r >= 0x3040 && r <= 0x30FF, // Hiragana + Katakana
			r >= 0xF900 && r <= 0xFAFF: // CJK compatibility ideographs
			total++
		default:
			total++
		}
	}
	return total
}
