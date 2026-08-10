package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"os"
	"sync"
	"time"
)

// resultMemo is a short-TTL, content-addressed memo for deterministic tool
// calls. The key carries a content fingerprint (file mtime/size or a dir
// stat), so the memo only serves identical calls while the inputs are
// unchanged. Its purpose is to avoid re-executing the same external tool
// (ripgrep, file reads) several times within one turn.
type resultMemo struct {
	mu    sync.Mutex
	ttl   time.Duration
	items map[string]memoItem
}

type memoItem struct {
	value    Result
	storedAt time.Time
}

// memoKey hashes the canonical inputs for a memo entry.
func memoKey(parts ...any) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(parts...)))
	return hex.EncodeToString(sum[:])
}

// getConsume returns a memo hit and deletes the entry. Consume-on-hit is the
// read-dedup contract: the first unchanged re-read gets a stub (the real
// content is already in context); the entry is dropped so the next read
// returns real content again. This prevents a stale stub from pointing the
// model at a result that was since compacted out of context — the failure
// mode the article calls "sticky cache" — at the cost of one extra turn
// (consume-on-hit's one-turn premium).
func (m *resultMemo) getConsume(key string) (Result, bool) {
	if m == nil {
		return Result{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[key]
	if !ok || time.Since(item.storedAt) > m.ttl {
		if ok {
			delete(m.items, key)
		}
		return Result{}, false
	}
	delete(m.items, key)
	return item.value, true
}

func (m *resultMemo) get(key string) (Result, bool) {
	if m == nil {
		return Result{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[key]
	if !ok || time.Since(item.storedAt) > m.ttl {
		if ok {
			delete(m.items, key)
		}
		return Result{}, false
	}
	return item.value, true
}

func (m *resultMemo) put(key string, result Result) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.items) >= 256 {
		oldest := ""
		var oldestAt time.Time
		for k, item := range m.items {
			if oldest == "" || item.storedAt.Before(oldestAt) {
				oldest, oldestAt = k, item.storedAt
			}
		}
		delete(m.items, oldest)
	}
	m.items[key] = memoItem{value: result, storedAt: time.Now()}
}

var (
	grepMemo = &resultMemo{ttl: 10 * time.Second, items: map[string]memoItem{}}
	readMemo = &resultMemo{ttl: 30 * time.Second, items: map[string]memoItem{}}
)

// fileFingerprint returns a content fingerprint for a path: mtime and size.
// Empty when the path cannot be stat'ed.
func fileFingerprint(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.ModTime().UnixNano(), st.Size())
}
