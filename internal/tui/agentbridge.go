package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"rick/internal/agent"
	"rick/internal/cache"
	"rick/internal/config"
	"rick/internal/glob"
	"rick/internal/permission"
	"rick/internal/plugin"
	"rick/internal/provider"
	"rick/internal/session"
	"rick/internal/tokens"
	"rick/pkg/repomap"
)

// agentArchiveDir is where trimmed originals land for the TUI's runs:
// <store dir>/archive. The store is always present in interactive mode, but
// fall back to "" (no archiving) when running without one.
func agentArchiveDir(store *session.Store) string {
	if store == nil {
		return ""
	}
	return filepath.Join(store.Dir(), "archive")
}

// startAgent kicks off a run and returns the drain command.
//
// The goroutine NEVER touches *Model — it only writes to m.agentCh, which the
// Update loop drains via readAgentMsg ticks.
func (m *Model) startAgent(prompt string) tea.Cmd {
	prov, modelID, err := m.resolveProvider()
	if err != nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: err.Error(), Time: time.Now()})
		return nil
	}

	if prompt != "" {
		m.history = append(m.history, provider.UserText(prompt))
	}

	ch := make(chan agent.Event, 128)
	ctx, cancel := context.WithCancel(context.Background())
	m.agentRunID++
	runID := m.agentRunID
	m.agentCh = ch
	m.agentCancel = cancel
	m.running = true
	m.turnStart = time.Now()
	m.streamBuf.Reset()
	m.thinkBuf.Reset()
	if m.deps.AgentRegistry != nil {
		id, registerErr := m.deps.AgentRegistry.Register(&agent.AgentEntry{
			Name: m.agentName, Depth: 0, Status: agent.AgentIdle,
			Description: prompt, Cancel: cancel,
		})
		if registerErr != nil {
			cancel()
			m.running = false
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "agent registry: " + registerErr.Error(), Time: time.Now()})
			return nil
		}
		m.agentID = id
	}
	agentID := m.agentID
	m.resizeForActivity()
	cfg := m.deps.Loaded.Config
	cfg.Instructions = append([]string(nil), cfg.Instructions...)
	agentName := m.agentName
	reasoning := m.reasoning
	cwd := m.deps.Cwd
	projectRoot := m.deps.Loaded.ProjectRoot
	registry := m.deps.Registry
	perms := m.deps.Perms
	plugins := m.deps.Plugins
	ask := m.makeAsker()
	toolFilter := m.toolFilter()
	sessionID := m.sessionID()

	var snapshotter agent.Snapshotter
	if m.deps.Snapshots.Enabled() {
		snapshotter = m.deps.Snapshots
	}

	history := append([]provider.Message(nil), m.history...)
	skills := m.deps.Skills
	stableSystem, systemPrompt := m.sessionSystemParts(agentName, modelID, cwd, projectRoot, cfg, skills)
	schemas := m.pinnedToolSchemas()
	go func() {
		runner := agent.New(agent.Config{
			Provider:           prov,
			Model:              modelID,
			System:             systemPrompt,
			SystemStable:       stableSystem,
			MaxTokens:          cfg.MaxTokens,
			Reasoning:          reasoning,
			Tools:              registry,
			ToolFilter:         toolFilter,
			Perms:              perms,
			Ask:                ask,
			Cwd:                cwd,
			SessionID:          sessionID,
			AgentName:          agentName,
			AgentID:            agentID,
			Registry:           m.deps.AgentRegistry,
			Snapshotter:        snapshotter,
			Plugins:            plugins,
			Parallel:           true,
			Goals:              m.deps.Goals,
			Budget:             m.deps.Budget,
			RepoMapRoot:        projectRoot,
			RepoMapBlock:       m.sessionRepoMap(projectRoot, modelID),
			EnableDistillation: cfg.DistillEnabled != nil && *cfg.DistillEnabled,
			DistillModel:       cfg.DistillModelFor(),
			CacheRetention:     provider.CacheRetention(cfg.CacheRetention),
			WarmCache:          cfg.WarmCache,
			MaxReasoningTurns:  cfg.CacheMaxReasoningTurns,
			MaxToolResultBytes: cfg.CacheMaxToolResultBytes,
			PinnedToolSchemas:  schemas,
			ArchiveDir:         agentArchiveDir(m.deps.Store),
		})
		appended, _ := runner.Run(ctx, history, ch)
		// Results are delivered through the channel; the appended slice is
		// recovered from the events we already saw, so nothing is written to
		// the model from here.
		_ = appended
	}()

	// Track appended messages by reconstructing them in the Update loop.
	m.pendingTools = map[string]int{}
	m.turnBoundaryPending = false
	return tea.Batch(m.drainCmd(runID), m.spinnerCmd())
}

func (m *Model) drainCmd(runID uint64) tea.Cmd {
	return tea.Tick(40*time.Millisecond, func(time.Time) tea.Msg { return readAgentMsg{runID: runID} })
}

// sessionID returns the active session id, creating the session eagerly so
// session-scoped freeze keys (system prompt parts, pinned tools) are unique
// even before the first save.
func (m *Model) sessionID() string {
	if m.sess == nil {
		m.sess = &session.Session{
			ID:      session.NewID(),
			Cwd:     m.deps.Cwd,
			Created: time.Now(),
		}
	}
	return m.sess.ID
}

// drainAgent pulls whatever is available from the agent channel.
func (m *Model) drainAgent(runID uint64) (tea.Model, tea.Cmd) {
	if runID != m.agentRunID {
		return m, nil
	}
	if m.agentCh == nil {
		return m, nil
	}
	processed := false
	for i := 0; i < 64; i++ {
		select {
		case ev, ok := <-m.agentCh:
			processed = true
			if !ok {
				if m.agentCh != nil {
					return m, m.finishRun(fmt.Errorf("agent event stream ended unexpectedly"))
				}
				return m, nil
			}
			if cmd, stop := m.applyAgentEvent(ev); stop {
				return m, cmd
			}
		default:
			if processed {
				m.refresh()
			}
			return m, m.drainCmd(runID)
		}
	}
	if processed {
		m.refresh()
	}
	return m, m.drainCmd(runID)
}

func (m *Model) applyAgentEvent(ev agent.Event) (tea.Cmd, bool) {
	switch ev.Kind {
	case agent.EvText:
		m.flushTurnBoundary()
		m.streamBuf.WriteString(ev.Text)

	case agent.EvThinking:
		m.flushTurnBoundary()
		m.thinkBuf.WriteString(ev.Text)

	case agent.EvToolStart:
		m.flushStream()
		if ev.Tool != nil {
			if ev.Tool.CallID != "" {
				if _, pending := m.pendingTools[ev.Tool.CallID]; pending {
					return nil, false
				}
				if _, completed := m.toolOutputs[ev.Tool.CallID]; completed {
					return nil, false
				}
			}
			idx := len(m.msgs)
			m.pendingTools[ev.Tool.CallID] = idx
			m.msgs = append(m.msgs, toolMsgFromEvent(ev.Tool, true))
			m.tx.noteAppend()
		}

	case agent.EvToolEnd:
		if ev.Tool != nil {
			if ev.Tool.CallID != "" {
				if _, completed := m.toolOutputs[ev.Tool.CallID]; completed {
					return nil, false
				}
			}
			if ev.Tool.Optimization != nil {
				stats := ev.Tool.Optimization
				m.optimization.Add(stats.OriginalTokens, stats.CompressedTokens, stats.SavedTokens)
			}
			if m.toolOutputs == nil {
				m.toolOutputs = make(map[string]string)
			}
			m.toolOutputs[ev.Tool.CallID] = ev.Tool.Output
			if idx, ok := m.pendingTools[ev.Tool.CallID]; ok && idx < len(m.msgs) {
				m.msgs[idx] = toolMsgFromEvent(ev.Tool, false)
				m.touch(idx)
				delete(m.pendingTools, ev.Tool.CallID)
			} else {
				m.msgs = append(m.msgs, toolMsgFromEvent(ev.Tool, false))
				m.tx.noteAppend()
			}
			if d, ok := diffMsgFromMeta(ev.Tool.Meta); ok {
				m.msgs = append(m.msgs, d)
				m.tx.noteAppend()
			}
		}

	case agent.EvCacheDivergence:
		if ev.Divergence != nil {
			if ev.Divergence.Index >= 0 {
				m.pendingDivergence = fmt.Sprintf("%s@%d;%s", ev.Divergence.Kind, ev.Divergence.Index, ev.Divergence.Reason)
			} else {
				m.pendingDivergence = fmt.Sprintf("%s;%s", ev.Divergence.Kind, ev.Divergence.Reason)
			}
		}

	case agent.EvUsage:
		if ev.Usage != nil {
			// Cumulative counters are for billing; the context gauge needs
			// occupancy. Every request resends the whole conversation, so
			// the newest call's input already includes all prior turns —
			// adding them up double-counts the history on every round trip.
			//
			// Anthropic reports:
			//   InputTokens = cache miss (new tokens billed at full price)
			//   CacheReadTokens = cache hit (discounted, previously cached)
			// CacheCreationTokens = cache write (newly written to cache)
			// Context occupancy is the total request footprint: miss + hit + write.
			m.usage.Input = ev.Usage.InputTokens
			m.usage.Output = ev.Usage.OutputTokens
			m.usage.CacheRead = ev.Usage.CacheReadTokens
			m.usage.CacheWrite = ev.Usage.CacheWriteTokens
			m.billed.Input += ev.Usage.InputTokens
			m.billed.Output += ev.Usage.OutputTokens
			m.billed.CacheRead += ev.Usage.CacheReadTokens
			m.billed.CacheWrite += ev.Usage.CacheWriteTokens
			eviction := m.observeCacheUsage(ev.Usage)
			// Persist one telemetry entry per provider request so per-turn
			// cache-hit/miss behavior can be measured from the session file.
			if m.sess != nil {
				m.requestSeq++
				m.sess.Requests = append(m.sess.Requests, session.RequestUsage{
					Index:           m.requestSeq,
					Agent:           m.agentName,
					Input:           ev.Usage.InputTokens,
					Output:          ev.Usage.OutputTokens,
					CacheRead:       ev.Usage.CacheReadTokens,
					CacheWrite:      ev.Usage.CacheWriteTokens,
					Divergence:      m.pendingDivergence,
					Eviction:        eviction,
					ReasoningTokens: ev.ReasoningTokens,
				})
				m.pendingDivergence = ""
			}
			if m.deps.Usage != nil {
				_ = m.deps.Usage.Record(m.modelID,
					ev.Usage.InputTokens, ev.Usage.OutputTokens,
					ev.Usage.CacheReadTokens, ev.Usage.CacheWriteTokens)
			}
			m.maybeAutoCompact()
		}

	case agent.EvTurnEnd:
		m.flushStream()
		m.flushTurnBoundary()
		m.turnBoundaryPending = true

	case agent.EvAgentBackground, agent.EvAgentReattached, agent.EvAgentMessage:
		if strings.TrimSpace(ev.Text) != "" {
			m.msgs = append(m.msgs, ChatMsg{Kind: MsgSystem, Text: ev.Text, Time: time.Now()})
			m.tx.noteAppend()
		}

	case agent.EvError:
		m.flushStream()
		m.flushTurnBoundary()
		if ev.Err != nil {
			m.msgs = append(m.msgs, ChatMsg{Kind: MsgError, Text: ev.Err.Error(), Time: time.Now()})
		}
		return m.finishRun(ev.Err), true

	case agent.EvDone:
		m.flushStream()
		m.flushTurnBoundary()
		return m.finishRun(nil), true
	}
	return nil, false
}

func (m *Model) flushTurnBoundary() {
	if !m.turnBoundaryPending {
		return
	}
	for index := len(m.msgs) - 1; index >= 0; index-- {
		if m.msgs[index].Kind == MsgTool {
			m.msgs[index].TurnBoundary = true
			break
		}
	}
	m.turnBoundaryPending = false
}

// observeCacheUsage detects per-turn cache misses (pi's cache-stats.ts
// contract): prompt tokens that were in the previous request's prompt but
// were not read from cache, above a 1024-token noise floor. Consecutive
// misses get a one-line system notice so cache regressions are visible. The
// notice distinguishes the two causes: an idle gap that outlived the
// provider's cache TTL (a timeout) versus a genuine prompt prefix change.
// The miss cause is returned so the request's telemetry row can be labelled
// ("eviction") — the analyzer then tells a provider eviction apart from a
// client rewrite even when no byte divergence was detected.
func (m *Model) observeCacheUsage(u *provider.Usage) string {
	promptTokens := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
	if promptTokens <= 0 {
		return ""
	}
	reported := u.CacheReadTokens+u.CacheWriteTokens > 0
	// A turn that reports no cache tokens is still a full re-bill when its
	// fresh input alone covers the previously sent span — some gateways
	// omit the cache fields on those turns. A normal growing turn whose
	// input is only the new tail must not be counted as a miss, otherwise
	// one cached turn would make every later cache-less turn a false alarm.
	fullRebill := !reported && u.InputTokens >= m.cachePrevPrompt-cacheMissNoiseFloor
	if m.cachePrevPrompt > 0 && (reported || fullRebill) {
		missed := min(m.cachePrevPrompt, promptTokens) - u.CacheReadTokens
		if missed > cacheMissNoiseFloor {
			m.cacheMissTokens += missed
			m.cacheMissCount++
			m.cacheMissStreak++
			missCause := m.cacheMissReason(reported)
			if m.cacheMissStreak == 2 {
				m.appendMsg(ChatMsg{Kind: MsgSystem,
					Text: fmt.Sprintf("cache miss: ~%s tokens re-billed (%s)",
						humanTokens(missed), missCause), Time: time.Now()})
			}
			return missCause
		}
		m.cacheMissStreak = 0
	}
	m.cacheLastUsage = time.Now()
	m.cachePrevPrompt = promptTokens
	return ""
}

// cacheTTL returns the provider cache TTL for the configured retention and
// vendor (per-vendor table in provider.DefaultCacheTTL): DeepSeek-line
// endpoints keep their prefix cache for a day, Anthropic's ephemeral cache
// lives ~5 minutes (1h with long retention), everything else 5m by default.
func (m *Model) cacheTTL() time.Duration {
	retention := provider.CacheRetentionAuto
	if m.deps.Loaded != nil {
		retention = provider.CacheRetention(m.deps.Loaded.Config.CacheRetention)
	}
	provID, _ := config.SplitModel(m.modelID)
	return provider.DefaultCacheTTL(provID, retention)
}

// cacheMissReason tells apart an idle-gap timeout from a prefix change, so
// cache regressions caused by conversation edits are distinguishable from
// the cheap misses that come from simply waiting too long.
func (m *Model) cacheMissReason(reported bool) string {
	if m.pendingDivergence != "" {
		return "prefix change: " + m.pendingDivergence
	}
	if !m.cacheLastUsage.IsZero() && time.Since(m.cacheLastUsage) > m.cacheTTL() {
		return "idle gap (cache expired)"
	}
	if !reported {
		return "provider served no prefix cache"
	}
	return "prefix change"
}

// recordChildUsage updates persistent accounting from a child runner. Child
// runners execute outside Bubble Tea's update loop, so the tracker is updated
// at the event source rather than relying on a UI message arriving.
func (m *Model) recordChildUsage(modelID string, usage provider.Usage) {
	if m.deps.Usage != nil {
		_ = m.deps.Usage.Record(modelID, usage.InputTokens, usage.OutputTokens,
			usage.CacheReadTokens, usage.CacheWriteTokens)
	}
	if p := m.program; p != nil {
		p.Send(childUsageMsg{usage: usage})
	}
}

func (m *Model) recordUsageOnly(modelID string, usage provider.Usage) {
	if m.deps.Usage != nil {
		_ = m.deps.Usage.Record(modelID, usage.InputTokens, usage.OutputTokens,
			usage.CacheReadTokens, usage.CacheWriteTokens)
	}
}
func (m *Model) flushStream() {
	if m.thinkBuf.Len() > 0 {
		m.msgs = append(m.msgs, ChatMsg{Kind: MsgThinking, Text: m.thinkBuf.String(), Time: time.Now()})
		m.tx.noteAppend()
		m.history = append(m.history, provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.ContentBlock{{Type: "thinking", Text: m.thinkBuf.String()}},
		})
		m.thinkBuf.Reset()
	}
	if m.streamBuf.Len() > 0 {
		text := m.streamBuf.String()
		m.msgs = append(m.msgs, ChatMsg{Kind: MsgAssistant, Text: text, Time: time.Now()})
		m.tx.noteAppend()
		m.streamBuf.Reset()
	}
}

func (m *Model) finishRun(err error) tea.Cmd {
	m.flushStream()
	m.flushTurnBoundary()
	m.running = false
	if !m.turnStart.IsZero() {
		m.turnElapsed = time.Since(m.turnStart)
	}
	if m.agentCancel != nil {
		m.agentCancel()
		m.agentCancel = nil
	}
	m.agentCh = nil
	m.resizeForActivity()

	// Rebuild the canonical history from what actually happened so the next
	// turn replays tool calls and results correctly.
	m.rebuildHistory()
	m.recordRunError(err)
	if saveErr := m.saveSession(); saveErr != nil {
		m.reportSessionSaveError(saveErr)
	}
	m.refresh()

	if err != nil {
		m.setStatus("error: " + truncate(err.Error(), 60))
	}
	if m.autoCompactPending {
		m.autoCompactPending = false
		_, compactCmd := m.cmdCompact()
		if compactCmd != nil {
			m.lastAutoCompact = time.Now()
		}
		return compactCmd
	}
	return nil
}

func (m *Model) reportSessionSaveError(err error) {
	if err == nil {
		return
	}
	m.msgs = append(m.msgs, ChatMsg{Kind: MsgError, Text: "session save: " + err.Error(), Time: time.Now()})
	m.tx.noteAppend()
	m.setStatus("session save failed: " + truncate(err.Error(), 48))
}

func (m *Model) recordRunError(err error) {
	m.lastRunError = ""
	if err != nil {
		m.lastRunError = err.Error()
	}
	if m.sess != nil {
		m.sess.RunError = m.lastRunError
	}
}

func (m *Model) restoreRunError(sess *session.Session) {
	m.lastRunError = ""
	if sess != nil {
		m.lastRunError = sess.RunError
	}
}

// rebuildHistory reconstructs bounded provider messages from the rendered
// transcript. Large tool results remain available to local session export and
// in-memory replay, but are not replayed in full on every provider turn.
func (m *Model) rebuildHistory() {
	m.history = m.capHistoryCacheAware(m.buildHistory(historyToolOutputChars))
}

func (m *Model) buildHistory(toolOutputLimit int) []provider.Message {
	var out []provider.Message
	var pendingAssistant *provider.Message
	var pendingResults []provider.ContentBlock

	flushAssistant := func() {
		if pendingAssistant != nil && len(pendingAssistant.Content) > 0 {
			out = append(out, *pendingAssistant)
			pendingAssistant = nil
		}
	}
	flushResults := func() {
		if len(pendingResults) > 0 {
			out = append(out, provider.Message{Role: provider.RoleUser, Content: pendingResults})
			pendingResults = nil
		}
	}

	for _, msg := range m.msgs {
		switch msg.Kind {
		case MsgUser:
			flushAssistant()
			flushResults()
			out = append(out, provider.UserText(msg.Text))
		case MsgThinking:
			// GLM/DeepSeek-style endpoints require a prior turn's reasoning to
			// be echoed back verbatim as reasoning_content. Keep the thinking
			// block inside the assistant turn it belongs to so it is replayed.
			if pendingAssistant == nil {
				pendingAssistant = &provider.Message{Role: provider.RoleAssistant}
			}
			pendingAssistant.Content = append(pendingAssistant.Content, provider.ContentBlock{
				Type: "thinking", Text: msg.Text,
			})
		case MsgAssistant:
			if len(pendingResults) > 0 {
				flushAssistant()
				flushResults()
			}
			if pendingAssistant == nil {
				pendingAssistant = &provider.Message{Role: provider.RoleAssistant}
			}
			pendingAssistant.Content = append(pendingAssistant.Content, provider.TextBlock(msg.Text))
		case MsgTool:
			if msg.ToolRunning {
				if pendingAssistant == nil {
					pendingAssistant = &provider.Message{Role: provider.RoleAssistant}
				}
				callID := msg.CallID
				if callID == "" {
					callID = fmt.Sprintf("interrupted-tool-%d", len(pendingResults))
				}
				input := msg.ToolInput
				if len(input) == 0 {
					input = []byte("{}")
				}
				pendingAssistant.Content = append(pendingAssistant.Content, provider.ContentBlock{
					Type: "tool_use", ID: callID, Name: msg.ToolName, Input: input,
				})
				pendingResults = append(pendingResults, provider.ToolResultBlock(callID, "interrupted", true))
				continue
			}
			if pendingAssistant == nil {
				pendingAssistant = &provider.Message{Role: provider.RoleAssistant}
			}
			input := msg.ToolInput
			if len(input) == 0 {
				input = []byte("{}")
			}
			pendingAssistant.Content = append(pendingAssistant.Content, provider.ContentBlock{
				Type: "tool_use", ID: msg.CallID, Name: msg.ToolName, Input: input,
			})
			pendingResults = append(pendingResults,
				provider.ToolResultBlock(msg.CallID, compactToolOutput(m.fullToolOutput(msg), toolOutputLimit), msg.ToolErr))
			if msg.TurnBoundary {
				flushAssistant()
				flushResults()
			}
		}
	}
	flushAssistant()
	flushResults()
	return out
}

const (
	maxHistoryMessages = 500
	maxHistoryBytes    = 2 << 20
)

func capHistory(history []provider.Message) []provider.Message {
	if len(history) <= maxHistoryMessages && historyByteSize(history) <= maxHistoryBytes {
		return history
	}

	removed := 0
	remainingBytes := historyByteSize(history)
	for removed < len(history)-1 && (len(history)-removed+1 > maxHistoryMessages || remainingBytes > maxHistoryBytes) {
		remainingBytes -= messageByteSize(history[removed])
		removed++
	}
	if removed == 0 {
		return history
	}

	summaryText := fmt.Sprintf("Earlier conversation compacted: %d messages omitted.", removed)
	summary := provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: summaryText}}}
	return append([]provider.Message{summary}, history[removed:]...)
}

// capHistoryCacheAware bounds the provider-facing transcript without
// rewriting the stable cached prefix: the compaction summary is inserted
// right after the last cache boundary that survives the trim, so the bytes
// the provider still has cached stay byte-identical. Falls back to the plain
// front-summary form when no budget/boundary is known or the cap would blow.
func (m *Model) capHistoryCacheAware(history []provider.Message) []provider.Message {
	if len(history) <= maxHistoryMessages && historyByteSize(history) <= maxHistoryBytes {
		return history
	}

	removed := 0
	remainingBytes := historyByteSize(history)
	for removed < len(history)-1 && (len(history)-removed+1 > maxHistoryMessages || remainingBytes > maxHistoryBytes) {
		remainingBytes -= messageByteSize(history[removed])
		removed++
	}
	if removed == 0 {
		return history
	}

	summaryText := fmt.Sprintf("Earlier conversation compacted: %d messages omitted.", removed)
	summary := provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: summaryText}}}
	plain := append([]provider.Message{summary}, history[removed:]...)

	insert := 0
	if m.deps.Budget != nil {
		insert = lastBoundaryBefore(history, removed, m.deps.Budget.ChooseBoundaries(history))
	}
	// Reuse a previous summary's position so repeated compactions keep the
	// summary at the same bytes; a moving summary would rewrite the
	// canonical prefix and invalidate the provider prefix cache.
	if existing := existingCompactSummaryAt(history); existing >= 0 && existing < removed {
		insert = existing
	}
	if insert == 0 {
		return plain
	}
	kept := append([]provider.Message{}, history[:insert]...)
	kept = append(kept, summary)
	kept = append(kept, history[removed:]...)
	if len(kept) > maxHistoryMessages || historyByteSize(kept) > maxHistoryBytes {
		return plain
	}
	return kept
}

// existingCompactSummaryAt returns the message index of a previously
// inserted compacted-history summary, or -1 when none exists.
func existingCompactSummaryAt(history []provider.Message) int {
	for i, msg := range history {
		if len(msg.Content) == 1 && msg.Content[0].Type == "text" &&
			strings.HasPrefix(msg.Content[0].Text, "Earlier conversation compacted:") {
			return i
		}
	}
	return -1
}

// summaryPairAt returns the message index of a previously inserted
// user-requested compaction summary pair, or -1 when none exists.
func summaryPairAt(history []provider.Message) int {
	for i, msg := range history {
		if len(msg.Content) == 1 && msg.Content[0].Type == "text" &&
			strings.HasPrefix(msg.Content[0].Text, "Summary of the conversation so far:") {
			return i
		}
	}
	return -1
}

// lastBoundaryBefore returns the newest cache boundary index strictly before
// the trim point, so the summary lands immediately after the cached prefix.
func lastBoundaryBefore(history []provider.Message, removed int, boundaries map[int]bool) int {
	insert := 0
	for index := range boundaries {
		if index > insert && index < removed {
			insert = index
		}
	}
	return insert
}

func historyByteSize(history []provider.Message) int {
	total := 0
	for _, message := range history {
		total += messageByteSize(message)
	}
	return total
}

func messageByteSize(message provider.Message) int {
	total := len(message.Role) + 16
	for _, block := range message.Content {
		total += len(block.Type) + len(block.Text) + len(block.Signature)
		total += len(block.ID) + len(block.Name) + len(block.Input)
		total += len(block.ToolUseID) + len(block.Content)
		total += len(block.Source) + len(block.MediaType) + len(block.Data) + 32
	}
	return total
}

func (m *Model) interrupt() {
	m.cancelCompaction()
	if m.permReply != nil {
		m.answerPermission(agent.DecideReject)
	}
	if m.agentCh != nil {
		// Invalidate already-scheduled drain ticks before cancelling the runner.
		// The runner may still close its old channel after a new run starts.
		agentID := m.agentID
		m.agentRunID++
		_ = m.finishRun(context.Canceled)
		if m.deps.AgentRegistry != nil && agentID != "" {
			m.deps.AgentRegistry.Update(agentID, agent.AgentKilled, "", context.Canceled)
		}
		m.setStatus("interrupted")
		return
	}
	if m.agentCancel != nil {
		m.agentCancel()
		m.agentCancel = nil
	}
	m.running = false
	m.setStatus("interrupted")
}

// makeAsker builds a permission callback that hands the request to the UI and
// blocks the agent goroutine until the user answers.
func (m *Model) makeAsker() agent.PermissionAsker {
	program := m.program
	gate := m.permGate
	return func(ctx context.Context, req permission.Request) agent.PermissionDecision {
		if program == nil || gate == nil {
			return agent.DecideReject
		}
		select {
		case <-gate:
			defer func() { gate <- struct{}{} }()
		case <-ctx.Done():
			return agent.DecideReject
		}
		reply := make(chan agent.PermissionDecision, 1)
		program.Send(permAskMsg{req: req, reply: reply})
		select {
		case d := <-reply:
			return d
		case <-ctx.Done():
			return agent.DecideReject
		}
	}
}

// resolveProvider picks the provider implementation for the active model.
// Model ids from OpenAI-style endpoints may contain slashes (e.g.
// "nous/tencent/hy3:free"), so we try each '/' position and pick the first
// that matches a known provider.
func (m *Model) resolveProvider() (provider.Provider, string, error) {
	provID, modelID := config.SplitModel(m.modelID)
	if p, ok := m.deps.Providers[provID]; ok {
		return p, modelID, nil
	}
	// The direct split didn't match — try later slash positions so a model
	// like "nous/tencent/hy3:free" resolves to provider "nous" with model
	// "tencent/hy3:free" even if "tencent" isn't a configured provider.
	idx := strings.Index(m.modelID, "/")
	for idx >= 0 && idx < len(m.modelID)-1 {
		if p, ok := m.deps.Providers[m.modelID[:idx]]; ok {
			return p, m.modelID[idx+1:], nil
		}
		next := strings.Index(m.modelID[idx+1:], "/")
		if next < 0 {
			break
		}
		idx += 1 + next
	}
	avail := make([]string, 0, len(m.deps.Providers))
	for k := range m.deps.Providers {
		avail = append(avail, k)
	}
	if len(avail) == 0 {
		return nil, "", fmt.Errorf("no providers configured — set ANTHROPIC_API_KEY or add one to rick.json")
	}
	return nil, "", fmt.Errorf("unknown provider %q (have: %s)", provID, strings.Join(avail, ", "))
}

func buildSystemPrompt(agentName, modelID, cwd, projectRoot string, cfg config.Config, skills []plugin.Skill, userText string) string {
	_, prompt := buildSystemPromptParts(agentName, modelID, cwd, projectRoot, cfg, skills, userText)
	return prompt
}

// sessionSkillBlock renders the skills block against the session-stable user
// prompt, so its bytes never change between turns.
func sessionSkillBlock(skills []plugin.Skill, userText string) string {
	if len(skills) == 0 || userText == "" {
		return ""
	}
	return plugin.SkillBlock(plugin.MatchSkills(skills, userText))
}

func buildSystemPromptParts(agentName, modelID, cwd, projectRoot string, cfg config.Config, skills []plugin.Skill, userText string) (string, string) {
	base := agent.BuildPrompt
	if agentName == "plan" {
		base = agent.PlanPrompt
	}
	if a, ok := cfg.Agents[agentName]; ok && a.Prompt != "" {
		base = a.Prompt
	}

	stable := base + agent.ProjectContext(projectRoot, cfg.Instructions)
	volatile := sessionSkillBlock(skills, userText) + agent.Environment(cwd, modelID, agentName, session.GitInfo(cwd))
	return stable, stable + volatile
}

// sessionSystemParts returns the system prompt split for the active session.
// The volatile bytes (skills match, environment git state) are frozen once
// per (session, model, agent) so every turn sends a byte-identical cached
// prefix. The session id in the key prevents a brand-new session from
// reusing the previous one's frozen volatile bytes.
func (m *Model) sessionSystemParts(agentName, modelID, cwd, projectRoot string, cfg config.Config, skills []plugin.Skill) (string, string) {
	key := m.sessionID() + "\x00" + agentName + "\x00" + modelID
	if m.sysPartsStable != "" && m.sysPartsKey == key {
		return m.sysPartsStable, m.sysPartsStable + m.sysPartsVolatile
	}
	base := agent.BuildPrompt
	if agentName == "plan" {
		base = agent.PlanPrompt
	}
	if a, ok := cfg.Agents[agentName]; ok && a.Prompt != "" {
		base = a.Prompt
	}
	stable := base + agent.ProjectContext(projectRoot, cfg.Instructions)
	volatile := sessionSkillBlock(skills, m.initialPrompt()) +
		agent.Environment(cwd, modelID, agentName, m.sessionGitInfo())
	m.sysPartsKey = key
	m.sysPartsStable = stable
	m.sysPartsVolatile = volatile
	return stable, stable + volatile
}

// sessionGitInfo returns the git-state line frozen at session start. The
// first turn captures it and the session file persists it, so a resumed
// session reproduces byte-identical environment bytes and the provider cache
// stays warm across the restart.
func (m *Model) sessionGitInfo() string {
	if m.sess != nil && m.sess.EnvGit != "" {
		return m.sess.EnvGit
	}
	info := session.GitInfo(m.deps.Cwd)
	if m.sess != nil {
		m.sess.EnvGit = info
	}
	return info
}

// pinnedToolSchemas returns the provider-facing tool list frozen for the
// session. The list is recomputed only when the session, model, agent, or
// the user's /tools toggles change, so mid-session plugin churn never
// alters the cached prefix bytes.
func (m *Model) pinnedToolSchemas() []provider.ToolSchema {
	key := m.sessionID() + "\x00" + m.agentName + "\x00" + m.modelID + "\x00" + toolToggleSignature(m.disabledTools)
	if m.toolSchemasPinned != nil && m.toolSchemasKey == key {
		return m.toolSchemasPinned
	}
	m.toolSchemasPinned = m.deps.Registry.Schemas(m.toolFilter())
	m.toolSchemasKey = key
	return m.toolSchemasPinned
}

// toolToggleSignature folds the user's /tools toggles into the pin key so a
// deliberate tool change invalidates the pinned list (and the cache) exactly
// once, instead of every turn.
func toolToggleSignature(disabled map[string]bool) string {
	if len(disabled) == 0 {
		return ""
	}
	names := make([]string, 0, len(disabled))
	for name, value := range disabled {
		if value {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// sessionRepoMap returns the session-wide RepoMap block, computed once so
// every turn of the session sends a byte-identical system suffix and the
// provider prompt cache stays warm. The prompt weighting uses the first user
// request. Blocks are also cached on disk keyed by the git tree hash, so a
// restarted session with an unchanged tree skips the expensive rebuild.
func (m *Model) sessionRepoMap(projectRoot, modelID string) string {
	if projectRoot == "" {
		return ""
	}
	m.repoMapOnce.Do(func() {
		encoding := tokens.EncodingForModel(modelID)
		prompt := m.initialPrompt()
		build := func() string {
			block, err := repomap.Build(repomap.Options{
				Root:      projectRoot,
				Prompt:    prompt,
				MaxTokens: 0,
				Encoding:  encoding,
			})
			if err != nil {
				return ""
			}
			return block
		}
		if disk := m.repoDisk(); disk != nil {
			if tree := gitTreeHash(projectRoot); tree != "" {
				// The RepoMap skeleton depends on the file tree and token
				// encoding, not on the prompt: opts.Prompt only weights which
				// files rank higher after the global set is built. Leaving the
				// prompt out of the key means a changed first task reuses the
				// cached skeleton (re-weighting is cheap) instead of forcing a
				// full rebuild — that rebuild is a CPU/IO spike on every new
				// session and the main source of RepoMap cache misses.
				key := strings.Join([]string{projectRoot, tree, string(encoding)}, "\x00")
				if data, ok := disk.Get(key); ok && len(data) > 0 {
					m.repoMapBlock = string(data)
					return
				}
				if block := build(); block != "" {
					m.repoMapBlock = block
					disk.Put(key, []byte(block))
				}
				return
			}
		}
		m.repoMapBlock = build()
	})
	return m.repoMapBlock
}

// repoDisk lazily opens the content-addressed disk cache shared by sessions.
func (m *Model) repoDisk() *cache.Dir {
	m.repoDiskOnce.Do(func() {
		dir, err := cache.New(filepath.Join(config.GlobalDir(), "cache"), 128)
		if err == nil {
			m.repoDiskDir = dir
		}
	})
	return m.repoDiskDir
}

// gitTreeHash returns the content hash of the working tree (HEAD^{tree}),
// used to key the disk-cached RepoMap so a stale map is never reused.
func gitTreeHash(dir string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD^{tree}")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// initialPrompt returns the first plain user text of the session, used to
// weight the session-wide RepoMap.
func (m *Model) initialPrompt() string {
	for _, message := range m.history {
		if message.Role != provider.RoleUser {
			continue
		}
		for _, block := range message.Content {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				return block.Text
			}
		}
	}
	return ""
}

// toolFilter honours config tool enable/disable globs, plan-mode limits,
// and the user's interactive /tools toggles.
func (m *Model) toolFilter() func(string) bool {
	cfg := m.deps.Loaded.Config
	agentCfg, hasAgent := cfg.Agents[m.agentName]
	disabled := make(map[string]bool, len(m.disabledTools))
	for name, value := range m.disabledTools {
		disabled[name] = value
	}
	return func(name string) bool {
		if disabled[name] {
			return false
		}
		if hasAgent && agentCfg.Tools != nil {
			if v, ok := glob.Lookup(agentCfg.Tools, name); ok {
				return v
			}
		}
		if cfg.Tools != nil {
			if v, ok := glob.Lookup(cfg.Tools, name); ok {
				return v
			}
		}
		return true
	}
}
