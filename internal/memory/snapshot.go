// Package memory provides deterministic, LLM-free cross-session context
// reuse for the provider prompt cache: a session can pick up where an older
// one left off by re-injecting a stable goal-state snapshot as the first
// user message, so the byte-stable prefix (system + tools + pinned snippet)
// stays warm across sessions instead of re-priming cold.
//
// The snapshot is derived entirely from the transcript — no LLM calls, no
// embeddings — so it never spends tokens and never blocks a turn. It is a
// structured checkpoint with fixed sections (mirroring the harness's
// compacted-summary shape): the model can resume work from it without the
// older session's full transcript, and because the same transcript always
// produces the same bytes, a snapshot loaded at the same prefix position is
// a cache hit rather than a rewrite.
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Snapshot is one deterministic goal-state checkpoint derived from a
// transcript. Text is the fixed-section Markdown block that becomes a user
// message at the cache prefix.
type Snapshot struct {
	// ID is the content address of the snapshot (first 16 hex chars of the
	// SHA-256 of the canonical serialization).
	ID string `json:"id"`
	// Text is the fixed-section Markdown checkpoint.
	Text string `json:"text"`
	// Sections holds the per-section line counts so a consumer can show a
	// compact preview without re-splitting Text.
	Sections map[string]int `json:"sections,omitempty"`
	// MessageCount is the number of transcript messages the snapshot covers.
	MessageCount int `json:"message_count"`
	// LastUserText is the most recent plain user turn, quoted for resume.
	LastUserText string `json:"last_user_text,omitempty"`
}

// Options controls snapshot derivation.
type Options struct {
	// MaxSections caps the number of goal/facts/errors bullets emitted per
	// section. Zero uses the default (12).
	MaxSections int
	// MaxLineChars truncates each quoted line. Zero uses the default (200).
	MaxLineChars int
	// MaxLastUserChars caps the quoted last user turn. Zero uses 400.
	MaxLastUserChars int
}

// Defaults applied when Options fields are zero.
const (
	DefaultMaxSections   = 12
	DefaultMaxLineChars  = 200
	DefaultLastUserChars = 400
)

// Section names (stable, fixed order).
const (
	SectionGoal   = "Goal"
	SectionFacts  = "Facts"
	SectionErrors = "Errors"
	SectionNext   = "Next Step"
)

// Derive builds a deterministic snapshot from a transcript. It never mutates
// messages. The same messages always derive the same Text and ID.
func Derive(messages []MessageLike, opts Options) Snapshot {
	if opts.MaxSections <= 0 {
		opts.MaxSections = DefaultMaxSections
	}
	if opts.MaxLineChars <= 0 {
		opts.MaxLineChars = DefaultMaxLineChars
	}
	if opts.MaxLastUserChars <= 0 {
		opts.MaxLastUserChars = DefaultLastUserChars
	}
	s := deriveFrom(messages, opts)
	s.ID = idOf(s.Text)
	return s
}

// MessageLike is the subset of a provider message the snapshot needs. The
// concrete provider.Message type is intentionally not referenced so this
// package stays provider-free and testable in isolation.
type MessageLike struct {
	// Role is "user", "assistant", or anything else (tool results carry
	// tool_use_id fragments inside their content and are read as text).
	Role string
	// Text is the flattened text of the message.
	Text string
	// IsError marks a tool_result error block.
	IsError bool
}

// deriveFrom walks the transcript newest-first and fills the fixed sections.
func deriveFrom(messages []MessageLike, opts Options) Snapshot {
	goal := collect(messages, opts.MaxSections, func(m MessageLike) bool {
		return m.Role == "user" && strings.TrimSpace(m.Text) != "" && !looksLikeToolResult(m.Text)
	})
	facts := collect(messages, opts.MaxSections, func(m MessageLike) bool {
		return m.Role == "assistant" && strings.TrimSpace(m.Text) != ""
	})
	errors := collect(messages, opts.MaxSections, func(m MessageLike) bool {
		return m.IsError || (strings.Contains(strings.ToLower(m.Text), "error") &&
			strings.Contains(m.Text, ":") && len(m.Text) < 600)
	})

	lastUser := lastUserText(messages, opts.MaxLastUserChars)

	var b strings.Builder
	b.WriteString("[cross-session checkpoint — resume from here without the earlier transcript]\n\n")
	b.WriteString("## " + SectionGoal + "\n")
	writeSection(&b, goal)
	b.WriteString("## " + SectionFacts + "\n")
	writeSection(&b, facts)
	b.WriteString("## " + SectionErrors + "\n")
	writeSection(&b, errors)
	b.WriteString("## " + SectionNext + "\n")
	if lastUser != "" {
		b.WriteString("- " + lastUser + "\n")
	} else {
		b.WriteString("- (none)\n")
	}

	return Snapshot{
		Text:         b.String(),
		Sections:     map[string]int{SectionGoal: len(goal), SectionFacts: len(facts), SectionErrors: len(errors)},
		MessageCount: len(messages),
		LastUserText: lastUser,
	}
}

// collect returns up to max newest matching lines, newest first.
func collect(messages []MessageLike, max int, keep func(MessageLike) bool) []string {
	var out []string
	for i := len(messages) - 1; i >= 0 && len(out) < max; i-- {
		m := messages[i]
		if !keep(m) {
			continue
		}
		for _, line := range strings.Split(m.Text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			out = append(out, clip(line, DefaultMaxLineChars))
			if len(out) >= max {
				break
			}
		}
	}
	return out
}

func writeSection(b *strings.Builder, lines []string) {
	if len(lines) == 0 {
		b.WriteString("- (none)\n")
		return
	}
	for _, line := range lines {
		b.WriteString("- " + line + "\n")
	}
}

func lastUserText(messages []MessageLike, max int) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role == "user" && strings.TrimSpace(m.Text) != "" && !looksLikeToolResult(m.Text) {
			return clip(m.Text, max)
		}
	}
	return ""
}

// looksLikeToolResult heuristically tells a real user turn from a tool-result
// user message. Tool results arrive as user messages whose content is JSON-ish
// or starts with a bracketed size/sha marker; the snapshot should not quote a
// giant JSON blob as the "next step".
func looksLikeToolResult(text string) bool {
	t := strings.TrimSpace(text)
	if len(t) > 3000 {
		return true
	}
	if strings.HasPrefix(t, "[duplicate payload sha256:") || strings.HasPrefix(t, "[") && strings.Contains(t, "retrieve via retrieve_uncompressed_context") {
		return true
	}
	if strings.HasPrefix(t, `{"`) && strings.Contains(t, `"`) && strings.HasSuffix(t, "}") {
		return true
	}
	return false
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := s[:max/2]
	tail := s[len(s)-max/2:]
	return head + "…" + tail
}

func idOf(text string) string {
	return IDFromText(text)
}

// IDFromText returns the content address (first 16 hex chars of the SHA-256)
// of text. Exported so prefixstore can derive session-stable load keys from
// the same content-address scheme the snapshot ids use.
func IDFromText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}

// SortKeys returns a stable key ordering for a map, used by callers that
// serialize the snapshot canonically.
func SortKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
