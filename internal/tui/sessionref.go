package tui

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"rick/internal/memory"
	"rick/internal/provider"
	"rick/internal/session"
)

// sessionReference is a budgeted, redacted snapshot of another session's
// transcript, prepared for injection as a leading user message. It mirrors
// the harness's dsh-session-reference seam: host (TUI) resolves a session
// reference, reads a bounded surface snapshot, and injects it as a frozen
// user message without granting the source session authority over the
// current one.
type sessionReference struct {
	ID    string
	Title string
	Text  string // bounded, redacted markdown snapshot
}

// maxSessionReferences bounds how many other sessions one /ref may inject.
const maxSessionReferences = 3

// refMsg is the tea message carrying prepared session references.
type refMsg struct {
	refs []sessionReference
	err  error
}

// cmdRef implements /ref <query|id> — load up to maxSessionReferences other
// sessions matching the query (id, title, or message text), derive a bounded
// deterministic snapshot from each, and inject them as a leading user
// message before the next turn. Reads run in parallel; any failure rejects
// the whole operation (no partial context).
func (m *Model) cmdRef(args string) (tea.Model, tea.Cmd) {
	query := strings.TrimSpace(args)
	if query == "" {
		m.setStatus("usage: /ref <session id or search query>")
		return m, nil
	}
	if m.deps.Store == nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "session store is unavailable", Time: nowFn()})
		return m, nil
	}
	m.setStatus("loading session references…")
	return m, func() tea.Msg {
		refs, err := prepareSessionReferences(m.deps.Store, query, m.sessionID())
		if err != nil {
			return refMsg{err: err}
		}
		return refMsg{refs: refs}
	}
}

// prepareSessionReferences resolves a query against the session corpus and
// derives bounded snapshots from the top matches in parallel. The current
// session id is rejected. No partial result is ever returned: a load or
// derivation failure rejects the whole batch, so the injected context is
// always complete.
func prepareSessionReferences(store *session.Store, query, selfID string) ([]sessionReference, error) {
	metas, err := store.Search(query)
	if err != nil {
		return nil, err
	}
	// Prefer the newest matches; exclude the current session.
	var candidates []session.Meta
	for _, meta := range metas {
		if meta.ID == selfID {
			continue
		}
		candidates = append(candidates, meta)
		if len(candidates) >= maxSessionReferences {
			break
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no other session matches %q", query)
	}

	// Parallel bounded reads: each candidate's full message list is loaded
	// independently, then folded into a deterministic snapshot. A single
	// failure (corrupt file, I/O) rejects the whole batch.
	results := make([]sessionReference, len(candidates))
	errs := make([]error, len(candidates))
	var wg sync.WaitGroup
	for i, meta := range candidates {
		wg.Add(1)
		go func(i int, meta session.Meta) {
			defer wg.Done()
			sess, loadErr := store.Load(meta.ID)
			if loadErr != nil {
				errs[i] = loadErr
				return
			}
			likes := make([]memory.MessageLike, 0, len(sess.Messages))
			for _, msg := range sess.Messages {
				likes = append(likes, memory.MessageLike{
					Role:    msg.Role,
					Text:    msg.Text(),
					IsError: messageHasErrorBlock(msg),
				})
			}
			snap := memory.Derive(likes, memory.Options{})
			results[i] = sessionReference{
				ID:    meta.ID,
				Title: meta.Title,
				Text:  snap.Text,
			}
		}(i, meta)
	}
	wg.Wait()
	for _, loadErr := range errs {
		if loadErr != nil {
			return nil, fmt.Errorf("session reference load failed: %w", loadErr)
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })
	return results, nil
}

// applyRef injects the prepared references as a leading user message, so the
// next turn's provider view grows append-only from the existing prefix with
// the referenced context at the head of the fresh tail. Each reference is
// labeled with its source session so the model can weigh it accordingly.
func (m *Model) applyRef(refs []sessionReference) {
	if len(refs) == 0 {
		m.setStatus("no session references loaded")
		return
	}
	var b strings.Builder
	b.WriteString("Context from other sessions (for reference only):\n")
	for _, ref := range refs {
		label := ref.ID
		if ref.Title != "" {
			label = fmt.Sprintf("%s (%s)", ref.ID, ref.Title)
		}
		b.WriteString("\n--- session " + label + " ---\n")
		b.WriteString(ref.Text)
		b.WriteString("\n")
	}
	m.history = append(m.history, provider.UserText(b.String()))
	m.msgs = append(m.msgs, ChatMsg{Kind: MsgSystem,
		Text: fmt.Sprintf("referenced %d session(s)", len(refs)), Time: nowFn()})
	m.tx.noteAppend()
	m.setStatus(fmt.Sprintf("referenced %d session(s)", len(refs)))
	_ = m.saveSession()
}
