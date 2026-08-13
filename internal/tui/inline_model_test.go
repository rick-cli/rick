package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"rick/internal/agent"
	"rick/internal/config"
	"rick/internal/provider"
	"rick/internal/provider/catalog"
	"rick/internal/swarm"
)

type modelChoiceTestProvider struct{}

func (modelChoiceTestProvider) Name() string { return "openai" }
func (modelChoiceTestProvider) Models() []provider.ModelInfo {
	return []provider.ModelInfo{{ID: "first", Name: "first"}, {ID: "second", Name: "second"}}
}
func (modelChoiceTestProvider) Stream(context.Context, provider.Request, chan<- provider.Event) {}

func newModelChoiceTestModel() *Model {
	return &Model{
		ready:    true,
		width:    100,
		height:   30,
		styles:   NewStyles(nil),
		tx:       newTranscript(),
		viewport: viewport.New(100, 20),
		deps: Deps{
			Loaded: &config.Loaded{
				TUI: config.TUI{ScrollSpeed: 3, Mouse: false},
			},
		},
	}
}

func TestTabCompletesSelectedSlashCommand(t *testing.T) {
	m := newModelChoiceTestModel()
	m.input = textarea.New()
	m.input.SetValue("/sess")

	if !m.completeSlashCommand() {
		t.Fatal("slash command was not considered completable")
	}
	if got := m.input.Value(); got != "/sessions" {
		t.Fatalf("completed command = %q, want /sessions", got)
	}
}

func TestTabCompletesTheHighlightedSlashCommand(t *testing.T) {
	m := newModelChoiceTestModel()
	m.input = textarea.New()
	m.input.SetValue("/m")
	m.slashCursor = 1

	if !m.completeSlashCommand() {
		t.Fatal("slash command was not considered completable")
	}
	if got := m.input.Value(); got != "/models" {
		t.Fatalf("highlighted completion = %q, want /models", got)
	}
}

func TestPromptHistoryUsesArrowKeys(t *testing.T) {
	m := newModelChoiceTestModel()
	m.input = textarea.New()
	m.inputHist = []string{"first prompt", "second prompt"}
	m.histIdx = -1
	m.input.SetValue("current draft")

	// Deliberate arrow presses are spaced well beyond the wheel-pair window
	// (a human takes 300ms+ between presses); the wheel detector must not
	// eat them. Pressing twice instantly would be a wheel pair by design.
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.input.Value(); got != "second prompt" {
		t.Fatalf("first Up returned %q, want latest prompt", got)
	}
	time.Sleep(400 * time.Millisecond)
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.input.Value(); got != "first prompt" {
		t.Fatalf("second Up returned %q, want oldest prompt", got)
	}
	time.Sleep(400 * time.Millisecond)
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if got := m.input.Value(); got != "second prompt" {
		t.Fatalf("Down returned %q, want newer prompt", got)
	}
	time.Sleep(400 * time.Millisecond)
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if got := m.input.Value(); got != "current draft" {
		t.Fatalf("final Down returned %q, want saved draft", got)
	}
}

func TestMouseWheelOverPromptScrollsChatNotActivity(t *testing.T) {
	m := newModelChoiceTestModel()
	m.deps = Deps{Loaded: &config.Loaded{TUI: config.TUI{ScrollSpeed: 1}}}
	m.viewport.SetContent(strings.Repeat("chat line\n", 80))
	m.teamViews = map[string]*SwarmView{
		"swarm": {
			SwarmID:  "swarm",
			Name:     "team",
			Active:   true,
			AgentOrd: []string{"agent"},
			Agents: map[string]*AgentView{
				"agent": {Name: "agent", Status: swarm.StatusWorking},
			},
		},
	}

	_, panelBottom := m.activityPanelBounds()
	if panelBottom <= 0 {
		t.Fatal("activity panel has no bounds")
	}
	before := m.viewport.YOffset
	m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Y: panelBottom})
	if m.viewport.YOffset <= before {
		t.Fatalf("wheel at prompt row moved chat offset from %d to %d", before, m.viewport.YOffset)
	}
	if m.activityFocused {
		t.Fatal("wheel at prompt row focused the activity panel")
	}

	m.activityFocused = false
	m.activityCursor = 0
	m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Y: m.activityPanelTop() + 1})
	if !m.activityFocused || m.activityCursor != 0 {
		t.Fatalf("wheel inside activity panel did not preserve activity interaction: focused=%v cursor=%d", m.activityFocused, m.activityCursor)
	}
}

func TestWheelOverInputHistoryOnlyScrollsChat(t *testing.T) {
	m := newModelChoiceTestModel()
	m.deps = Deps{Loaded: &config.Loaded{TUI: config.TUI{ScrollSpeed: 1}}}
	m.viewport.SetContent(strings.Repeat("chat line\n", 80))
	// Seed history exactly like a real session would.
	m.inputHist = []string{"first prompt", "second prompt"}
	m.histIdx = -1
	m.input = textarea.New()
	m.input.SetValue("current draft")

	// Simulate the exact burst sequence Windows Terminal sends when the
	// scroll wheel is used over the prompt: three rapid same-direction
	// up/down key events. These must scroll the chat and must NOT touch the
	// prompt history or the saved draft.
	m.isWheelKey(1) // prime the detector
	before := m.viewport.YOffset
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if got := m.input.Value(); got != "current draft" {
		t.Fatalf("wheel over prompt changed input to %q, want unchanged draft", got)
	}
	if m.viewport.YOffset <= before {
		t.Fatalf("wheel over prompt did not scroll chat: offset %d -> %d", before, m.viewport.YOffset)
	}
	if m.activityFocused {
		t.Fatal("wheel over prompt focused the activity panel")
	}
}

// TestWheelBurstRestoresDraftWithoutPriming is the real-world regression:
// Windows Terminal delivers a wheel as up/down key events with no priming, so
// the first event is indistinguishable from an arrow key and may browse
// history. Once the burst is confirmed the draft must be restored and the
// chat must scroll — the input must never be left on an old prompt.
func TestWheelBurstRestoresDraftWithoutPriming(t *testing.T) {
	m := newModelChoiceTestModel()
	m.deps = Deps{Loaded: &config.Loaded{TUI: config.TUI{ScrollSpeed: 1}}}
	m.input = textarea.New()
	m.ready = false
	for i := 0; i < 60; i++ {
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "chat line " + strings.Repeat("x", i%20), Time: nowFn()})
	}
	m.handleResize(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.viewport.GotoBottom()
	start := m.viewport.YOffset
	m.inputHist = []string{"first prompt", "second prompt"}
	m.histIdx = -1
	m.input.SetValue("current draft")
	m.histDraft = "current draft"

	// No priming: six rapid Up events (30ms apart), exactly what the terminal
	// sends for a wheel-up over the prompt.
	for i := 0; i < 6; i++ {
		m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
		time.Sleep(30 * time.Millisecond)
	}
	if got := m.input.Value(); got != "current draft" {
		t.Fatalf("wheel-up left input on %q, want draft restored", got)
	}
	if m.histIdx != -1 {
		t.Fatalf("wheel-up left history index at %d", m.histIdx)
	}
	if m.viewport.YOffset >= start {
		t.Fatalf("wheel-up did not scroll up: offset %d (start %d)", m.viewport.YOffset, start)
	}
}

// TestMediumWheelScrollsNotHistory covers a wheel at ~4 notches/sec (250ms
// spacing), which the old 3-event burst rule missed entirely — every event
// fell through to history. A same-direction pair must confirm the wheel.
func TestMediumWheelScrollsNotHistory(t *testing.T) {
	m := newModelChoiceTestModel()
	m.deps = Deps{Loaded: &config.Loaded{TUI: config.TUI{ScrollSpeed: 1}}}
	m.input = textarea.New()
	m.ready = false
	for i := 0; i < 60; i++ {
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "chat line " + strings.Repeat("x", i%20), Time: nowFn()})
	}
	m.handleResize(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.viewport.GotoBottom()
	start := m.viewport.YOffset
	m.inputHist = []string{"first prompt", "second prompt"}
	m.histIdx = -1
	m.input.SetValue("draft")
	m.histDraft = "draft"

	for i := 0; i < 4; i++ {
		m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
		time.Sleep(250 * time.Millisecond)
	}
	if got := m.input.Value(); got != "draft" {
		t.Fatalf("medium wheel-up corrupted input to %q", got)
	}
	if m.viewport.YOffset >= start {
		t.Fatalf("medium wheel-up did not scroll up: offset %d (start %d)", m.viewport.YOffset, start)
	}
}

func TestWheelBurstOverActivityMovesCursorNotChat(t *testing.T) {
	m := newModelChoiceTestModel()
	m.deps = Deps{Loaded: &config.Loaded{TUI: config.TUI{ScrollSpeed: 1}}}
	m.viewport.SetContent(strings.Repeat("chat line\n", 80))
	m.teamViews = map[string]*SwarmView{
		"swarm": {
			SwarmID:  "swarm",
			Name:     "team",
			Active:   true,
			AgentOrd: []string{"agent"},
			Agents: map[string]*AgentView{
				"agent": {Name: "agent", Status: swarm.StatusWorking},
			},
		},
	}
	_, panelBottom := m.activityPanelBounds()
	if panelBottom <= 0 {
		t.Fatal("activity panel has no bounds")
	}
	m.activityFocused = false
	m.activityCursor = 0
	before := m.viewport.YOffset

	// Prime the burst detector, then burst at the panel's own row.
	m.isWheelKey(-1)
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if !m.activityFocused {
		t.Fatal("wheel burst over activity panel did not focus the panel")
	}
	if m.viewport.YOffset != before {
		t.Fatalf("wheel over activity moved chat offset %d -> %d", before, m.viewport.YOffset)
	}
}

func TestPromptHistoryTakesPriorityOverActivityFocus(t *testing.T) {
	m := newModelChoiceTestModel()
	m.activityFocused = true
	m.input = textarea.New()
	m.inputHist = []string{"first prompt", "second prompt"}
	m.histIdx = -1
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.input.Value(); got != "second prompt" {
		t.Fatalf("Up while activity is focused returned %q, want latest prompt", got)
	}
}

func TestMouseCaptureOffInChatViewForNativeSelection(t *testing.T) {
	m := newModelChoiceTestModel()
	// Test models lack deps; give it a Loaded so wantsMouseCapture can read
	// the mouse preference.
	m.deps.Loaded = &config.Loaded{TUI: config.TUI{Mouse: false}}
	// The plain chat view must NOT capture the mouse on any platform — the
	// terminal owns drag selection and copy there. The scroll wheel still
	// scrolls the chat via the same-direction key-burst path (isWheelKeyBurst)
	// instead of arriving as a MouseMsg.
	if m.wantsMouseCapture() {
		t.Fatal("plain chat view must NOT capture the mouse — the terminal owns selection there")
	}

	// Interactive overlays keep mouse capture.
	m.auth.active = true
	if !m.wantsMouseCapture() {
		t.Fatal("auth overlay must capture the mouse")
	}
	m.auth.active = false

	m.web.active = true
	if !m.wantsMouseCapture() {
		t.Fatal("web overlay must capture the mouse")
	}
	m.web.active = false

	m.activityFocused = true
	if !m.wantsMouseCapture() {
		t.Fatal("focused activity panel must capture the mouse")
	}
	m.activityFocused = false

	// tui.mouse: true forces capture everywhere (legacy behavior).
	m.deps.Loaded.TUI.Mouse = true
	if !m.wantsMouseCapture() {
		t.Fatal("tui.mouse: true must force mouse capture")
	}
}

func TestModelChoiceSupportsArrowsAndNumberedInput(t *testing.T) {
	m := newModelChoiceTestModel()
	m.armChoice("select a model · test", pendingModel, "test", []choiceOption{
		{value: "first", label: "first"},
		{value: "second", label: "second", active: true},
		{value: "third", label: "third"},
	})

	if m.pending.cursor != 1 {
		t.Fatalf("cursor = %d, want active option index 1", m.pending.cursor)
	}
	m.movePendingCursor(1)
	if m.pending.cursor != 2 {
		t.Fatalf("down moved cursor to %d, want 2", m.pending.cursor)
	}
	m.movePendingCursor(-1)
	if m.pending.cursor != 1 {
		t.Fatalf("up moved cursor to %d, want 1", m.pending.cursor)
	}

	rendered := m.renderPendingMenu(m.contentWidth())
	for _, label := range []string{"second", "↑/↓ select", "← Back", "↵ Select", "esc/backspace back"} {
		if !strings.Contains(rendered, label) {
			t.Fatalf("interactive model choice is missing %q:\n%s", label, rendered)
		}
	}

	m.clearPending()
	m.armChoice("select a model · test", pendingModel, "test", []choiceOption{
		{value: "first", label: "first"},
		{value: "second", label: "second"},
	})
	_, _, handled := m.handlePendingInput("2")
	if !handled {
		t.Fatal("numbered model selection was not consumed")
	}
}

func TestGenericChoiceMenusHaveWorkingButtons(t *testing.T) {
	m := newModelChoiceTestModel()
	m.viewport.YPosition = 0
	m.viewport.Height = 20
	m.armChoice("thinking", pendingReasoning, "", []choiceOption{
		{value: string(provider.ReasoningOff), label: "off"},
		{value: string(provider.ReasoningHigh), label: "high"},
	})
	m.movePendingCursor(1)
	m.refresh()

	if len(m.choiceButtons) != 2 {
		t.Fatalf("generic choice menu did not create buttons: %+v", m.choiceButtons)
	}
	selectButton := m.choiceButtons[1]
	_, _ = m.Update(tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		X:      selectButton.x + 1,
		Y:      selectButton.y,
	})
	if m.pending.kind != pendingNone || m.reasoning != provider.ReasoningHigh {
		t.Fatalf("generic Select button did not apply choice: pending=%d reasoning=%q", m.pending.kind, m.reasoning)
	}
}

type advertisedReasoningProvider struct{}

func (advertisedReasoningProvider) Name() string { return "openrouter" }
func (advertisedReasoningProvider) Models() []provider.ModelInfo {
	return []provider.ModelInfo{{
		ID:                    "vendor/model",
		ReasoningKnown:        true,
		ReasoningEffortsKnown: true,
		ReasoningEfforts:      []provider.ReasoningEffort{provider.ReasoningMax, provider.ReasoningHigh, provider.ReasoningLow},
		ReasoningDefault:      provider.ReasoningMax,
	}}
}
func (advertisedReasoningProvider) Stream(context.Context, provider.Request, chan<- provider.Event) {}

func TestReasoningChoiceUsesActiveModelVocabulary(t *testing.T) {
	m := newModelChoiceTestModel()
	m.deps = Deps{
		Loaded:    &config.Loaded{},
		Providers: map[string]provider.Provider{"openrouter": advertisedReasoningProvider{}},
	}
	m.modelID = "openrouter/vendor/model"
	m.updateContextWindow()
	if _, _ = m.cmdReasoning(""); m.pending.kind != pendingReasoning {
		t.Fatalf("reasoning command did not open a choice menu: %+v", m.pending)
	}
	got := make([]string, 0, len(m.pending.options))
	for _, option := range m.pending.options {
		got = append(got, option.value)
	}
	want := []string{"off", "low", "high", "max"}
	if !equalStrings(got, want) {
		t.Fatalf("reasoning options = %v, want %v", got, want)
	}
}

type incompleteAdvertisedReasoningProvider struct{}

func (incompleteAdvertisedReasoningProvider) Name() string { return "openrouter" }
func (incompleteAdvertisedReasoningProvider) Models() []provider.ModelInfo {
	return []provider.ModelInfo{{ID: "openai/gpt-5.2", ReasoningKnown: true}}
}
func (incompleteAdvertisedReasoningProvider) Stream(context.Context, provider.Request, chan<- provider.Event) {
}

func TestReasoningChoiceKeepsFallbackForIncompleteModelMetadata(t *testing.T) {
	m := newModelChoiceTestModel()
	m.deps = Deps{
		Loaded:    &config.Loaded{},
		Providers: map[string]provider.Provider{"openrouter": incompleteAdvertisedReasoningProvider{}},
	}
	m.modelID = "openrouter/openai/gpt-5.2"
	m.updateContextWindow()
	if _, _ = m.cmdReasoning(""); m.pending.kind != pendingReasoning {
		t.Fatalf("reasoning command did not open a choice menu: %+v", m.pending)
	}
	got := make([]string, 0, len(m.pending.options))
	for _, option := range m.pending.options {
		got = append(got, option.value)
	}
	want := []string{"off", "low", "medium", "high", "xhigh"}
	if !equalStrings(got, want) {
		t.Fatalf("reasoning options with incomplete metadata = %v, want %v", got, want)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func TestAuthModelBodyOnlyLabelsConfirmedReasoning(t *testing.T) {
	m := newModelChoiceTestModel()
	m.creds = &config.Credentials{Providers: map[string]config.Credential{"openai": {}}}
	m.auth.draftID = "openai"
	m.auth.models = []catalog.Model{
		{ID: "custom-unknown"},
		{ID: "gpt-5"},
	}

	rendered := m.authModelBody(100)
	for _, line := range strings.Split(rendered, "\n") {
		if strings.Contains(line, "custom-unknown") && strings.Contains(line, "reasoning") {
			t.Fatalf("unknown model was labelled as confirmed reasoning: %q", line)
		}
		if strings.Contains(line, "gpt-5") && !strings.Contains(line, "reasoning") {
			t.Fatalf("known reasoning model was not labelled: %q", line)
		}
	}
}

func TestModelChoiceSelectButtonSelectsHighlightedModel(t *testing.T) {
	m := newModelChoiceTestModel()
	m.viewport.YPosition = 0
	m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "previous transcript message", Time: nowFn()})
	m.armChoice("select a model · test", pendingModel, "test", []choiceOption{
		{value: "first", label: "first"},
		{value: "second", label: "second"},
	})
	m.movePendingCursor(1)
	m.refresh()

	if len(m.choiceButtons) != 2 {
		t.Fatalf("want two choice buttons, got %+v", m.choiceButtons)
	}
	button, ok := m.choiceButtonAt(20, m.choiceButtons[1].y)
	if !ok || button.id != choiceButtonSelect {
		t.Fatalf("select button was not mapped: buttons=%+v", m.choiceButtons)
	}
	buttonLine := -1
	for row, line := range strings.Split(m.chatContent, "\n") {
		if strings.Contains(line, "Select") {
			buttonLine = row
			break
		}
	}
	if buttonLine != button.y {
		t.Fatalf("select button row = %d, rendered row = %d", button.y, buttonLine)
	}
	m.Update(tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		X:      button.x + 1,
		Y:      button.y + m.viewport.YPosition,
	})
	if m.pending.kind != pendingNone || m.modelID != "test/second" {
		t.Fatalf("select button state: pending=%d model=%q", m.pending.kind, m.modelID)
	}
}

// TestChoiceMenuSticksToBottomWhileStreaming locks the pinned-menu behaviour:
// once a menu is armed, streaming a long tail after it must not push the menu
// out of the viewport. The menu is re-rendered as a pinned tail of its own,
// so the final chat content ends with the menu's option block, and the choice
// buttons map to the last rows of the rendered content.
func TestChoiceMenuSticksToBottomWhileStreaming(t *testing.T) {
	m := newModelChoiceTestModel()
	m.viewport.YPosition = 0
	m.viewport.Height = 20
	m.armChoice("select a model · test", pendingModel, "test", []choiceOption{
		{value: "first", label: "first"},
		{value: "second", label: "second"},
	})
	// Stream a tail far taller than the viewport.
	for i := 0; i < 30; i++ {
		m.PushStreamChunk("streamed token line " + string(rune('a'+i%26)) + "\n")
	}
	lines := strings.Split(m.chatContent, "\n")
	// The pinned menu occupies the bottom block: title, options, hint and
	// buttons. Verify the last non-blank line is the button row and that the
	// final option is one line above it, i.e. the whole menu sits at the tail.
	lastRow := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			lastRow = i
			break
		}
	}
	if lastRow < 2 || !strings.Contains(lines[lastRow-2], "2 second") {
		t.Fatalf("menu was pushed out of the transcript bottom; rows around tail: %q / %q / %q",
			lines[lastRow-2], lines[lastRow-1], lines[lastRow])
	}
	if len(m.choiceButtons) != 2 {
		t.Fatalf("want two choice buttons while menu pinned, got %+v", m.choiceButtons)
	}
	for _, b := range m.choiceButtons {
		if b.y != lastRow {
			t.Fatalf("choice button y=%d, want last transcript row %d", b.y, lastRow)
		}
	}
}

func TestModelChoiceKeyboardBackAndEnter(t *testing.T) {
	m := newModelChoiceTestModel()
	m.creds = &config.Credentials{}
	m.deps.Providers = map[string]provider.Provider{"openai": modelChoiceTestProvider{}}
	m.armChoice("select a provider", pendingProvider, "", []choiceOption{
		{value: "openai", label: "OpenAI"},
	})

	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.pending.kind != pendingModel || m.pending.context != "openai" {
		t.Fatalf("provider enter did not open model selection: pending=%d context=%q", m.pending.kind, m.pending.context)
	}

	m.handleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if m.pending.kind != pendingProvider {
		t.Fatalf("backspace did not return to provider selection: pending=%d", m.pending.kind)
	}

	m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
	if m.pending.kind != pendingNone {
		t.Fatalf("escape did not cancel provider selection: pending=%d", m.pending.kind)
	}

	m.armChoice("select a model", pendingModel, "openai", []choiceOption{
		{value: "first", label: "first"},
		{value: "second", label: "second"},
	})
	m.movePendingCursor(1)
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.pending.kind != pendingNone || m.modelID != "openai/second" {
		t.Fatalf("enter did not select highlighted model: pending=%d model=%q", m.pending.kind, m.modelID)
	}
}

func newAuthNavigationTestModel() *Model {
	m := newModelChoiceTestModel()
	m.creds = &config.Credentials{Providers: map[string]config.Credential{
		"provider-a": {BaseURL: "https://a.example.com"},
		"provider-b": {BaseURL: "https://b.example.com"},
	}}
	m.deps.Loaded = &config.Loaded{Config: config.Config{Providers: map[string]config.Provider{}}}
	m.auth = authState{
		active: true,
		stage:  authList,
		rows: []authRow{
			{id: "provider-a", label: "Provider A", connected: true},
			{id: "provider-b", label: "Provider B", connected: true},
		},
	}
	return m
}

func TestAuthMenusUseEnterToSelectAndEscBackspaceToGoBack(t *testing.T) {
	m := newAuthNavigationTestModel()

	m.handleAuthKey(tea.KeyMsg{Type: tea.KeyDown}, "down")
	m.handleAuthKey(tea.KeyMsg{Type: tea.KeyEnter}, "enter")
	if m.auth.stage != authEditMenu || m.auth.draftID != "provider-b" {
		t.Fatalf("provider Enter did not select highlighted row: stage=%d provider=%q", m.auth.stage, m.auth.draftID)
	}

	m.handleAuthKey(tea.KeyMsg{Type: tea.KeyDown}, "down")
	m.handleAuthKey(tea.KeyMsg{Type: tea.KeyEnter}, "enter")
	if m.auth.stage != authAddURL {
		t.Fatalf("edit-menu Enter did not select highlighted action: stage=%d", m.auth.stage)
	}

	m.handleAuthKey(tea.KeyMsg{Type: tea.KeyEscape}, "esc")
	if m.auth.stage != authEditMenu {
		t.Fatalf("Escape did not return from URL editor: stage=%d", m.auth.stage)
	}
	m.handleAuthKey(tea.KeyMsg{Type: tea.KeyBackspace}, "backspace")
	if m.auth.stage != authList {
		t.Fatalf("Backspace did not return from edit menu: stage=%d", m.auth.stage)
	}
}

func TestAuthBackspaceStillEditsCredentialInput(t *testing.T) {
	m := newAuthNavigationTestModel()
	m.auth.stage = authEnterKey
	m.auth.inputBuf = "secret"

	m.handleAuthKey(tea.KeyMsg{Type: tea.KeyBackspace}, "backspace")
	if m.auth.inputBuf != "secre" || !m.auth.active {
		t.Fatalf("Backspace should edit credential input: input=%q active=%t", m.auth.inputBuf, m.auth.active)
	}
}

func TestAuthKeyMenuEnterSelectsNestedMenu(t *testing.T) {
	m := newAuthNavigationTestModel()
	m.auth.active = true
	m.auth.stage = authKeyMenu
	m.auth.cursor = 0

	m.handleAuthKey(tea.KeyMsg{Type: tea.KeyDown}, "down")
	m.handleAuthKey(tea.KeyMsg{Type: tea.KeyEnter}, "enter")
	if m.auth.stage != authKeyMode || m.auth.cursor != 0 {
		t.Fatalf("key-menu Enter did not open highlighted nested menu: stage=%d cursor=%d", m.auth.stage, m.auth.cursor)
	}

	m.handleAuthKey(tea.KeyMsg{Type: tea.KeyBackspace}, "backspace")
	if m.auth.stage != authEditMenu {
		t.Fatalf("Backspace did not leave key-mode menu: stage=%d", m.auth.stage)
	}
}

func TestAuthButtonsRenderForEveryStage(t *testing.T) {
	stages := []struct {
		stage authStage
		label string
	}{
		{authList, "↵ Configure"},
		{authEnterKey, "↵ Continue"},
		{authEditMenu, "↵ Select"},
		{authAddName, "↵ Continue"},
		{authAddURL, "↵ Continue"},
		{authAddKey, "↵ Continue"},
		{authProbing, "× Cancel"},
		{authPickModel, "↵ Select"},
		{authEnterModel, "↵ Save"},
		{authDeviceCode, "↵ Continue"},
		{authOAuthWaiting, "× Cancel"},
		{authKeyMenu, "↵ Select"},
		{authKeyAdd, "↵ Select"},
		{authKeyMode, "↵ Select"},
	}
	for _, tc := range stages {
		m := newAuthNavigationTestModel()
		m.auth.stage = tc.stage
		m.auth.draftID = "provider-a"
		m.auth.models = []catalog.Model{{ID: "model-a"}}
		rendered := m.authView()
		if !strings.Contains(rendered, "← Back") || !strings.Contains(rendered, tc.label) {
			t.Errorf("stage %d missing buttons %q:\n%s", tc.stage, tc.label, rendered)
		}
		if zones := m.authButtonZones(); len(zones) != 2 {
			t.Errorf("stage %d mapped %d auth buttons, want 2", tc.stage, len(zones))
		}
	}
}

func renderedAuthButtonPoint(m *Model, label string) (int, int, bool) {
	panel := m.authView()
	panelLeft := (m.width - lipgloss.Width(panel)) / 2
	panelTop := (m.height - lipgloss.Height(panel)) / 2
	for row, line := range strings.Split(panel, "\n") {
		if index := strings.Index(line, label); index >= 0 {
			return panelLeft + lipgloss.Width(line[:index]), panelTop + row, true
		}
	}
	return 0, 0, false
}

func TestAuthTopLevelButtonsWorkWithOverflowingProviderList(t *testing.T) {
	m := newAuthNavigationTestModel()
	for i := 0; i < 40; i++ {
		m.auth.rows = append(m.auth.rows, authRow{
			id:    fmt.Sprintf("provider-%02d", i),
			label: fmt.Sprintf("Provider %02d", i),
		})
	}
	panel := m.authView()
	if lipgloss.Height(panel) > m.height {
		t.Fatalf("top-level auth panel overflows terminal: panel=%d terminal=%d", lipgloss.Height(panel), m.height)
	}
	x, y, ok := renderedAuthButtonPoint(m, "↵ Configure")
	if !ok {
		t.Fatal("could not locate top-level Configure button")
	}
	button, ok := m.authButtonAt(x, y)
	if !ok || button.id != authButtonPrimary {
		t.Fatalf("top-level visible Configure button is not clickable: point=(%d,%d) button=%+v", x, y, button)
	}
	_, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: x, Y: y})
	if m.auth.stage != authEditMenu || m.auth.draftID != "provider-a" {
		t.Fatalf("top-level Configure click selected wrong provider/state: stage=%d provider=%q", m.auth.stage, m.auth.draftID)
	}
}

func TestAuthButtonsRenderAndDispatchThroughMouseUpdate(t *testing.T) {
	m := newAuthNavigationTestModel()
	rendered := m.authView()
	for _, label := range []string{"← Back", "↵ Configure"} {
		if !strings.Contains(rendered, label) {
			t.Fatalf("auth view is missing button %q:\n%s", label, rendered)
		}
	}

	zones := m.authButtonZones()
	if len(zones) != 2 || zones[1].id != authButtonPrimary {
		t.Fatalf("auth buttons were not mapped: %+v", zones)
	}
	primary := zones[1]
	visibleX, visibleY, ok := renderedAuthButtonPoint(m, "↵ Configure")
	if !ok {
		t.Fatal("could not locate visible Configure button")
	}
	mapped, ok := m.authButtonAt(visibleX, visibleY)
	if !ok || mapped.id != authButtonPrimary {
		t.Fatalf("visible Configure label is outside primary hitbox: point=(%d,%d) zone=%+v", visibleX, visibleY, primary)
	}
	_, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: visibleX, Y: visibleY})
	if m.auth.stage != authEditMenu || m.auth.draftID != "provider-a" {
		t.Fatalf("mouse Configure did not select provider: stage=%d provider=%q", m.auth.stage, m.auth.draftID)
	}

	m.auth.cursor = 1
	zones = m.authButtonZones()
	if len(zones) != 2 || zones[1].id != authButtonPrimary {
		t.Fatalf("nested primary auth button was not mapped: %+v", zones)
	}
	primary = zones[1]
	visibleX, visibleY, ok = renderedAuthButtonPoint(m, "↵ Select")
	if !ok {
		t.Fatal("could not locate visible Select button")
	}
	mapped, ok = m.authButtonAt(visibleX, visibleY)
	if !ok || mapped.id != authButtonPrimary {
		t.Fatalf("visible Select label is outside primary hitbox: point=(%d,%d) zone=%+v", visibleX, visibleY, primary)
	}
	_, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: visibleX, Y: visibleY})
	if m.auth.stage != authAddURL {
		t.Fatalf("mouse Select did not activate nested edit action: stage=%d", m.auth.stage)
	}

	zones = m.authButtonZones()
	if len(zones) != 2 || zones[0].id != authButtonBack {
		t.Fatalf("nested Back button was not mapped: %+v", zones)
	}
	back := zones[0]
	visibleX, visibleY, ok = renderedAuthButtonPoint(m, "← Back")
	if !ok {
		t.Fatal("could not locate visible Back button")
	}
	mapped, ok = m.authButtonAt(visibleX, visibleY)
	if !ok || mapped.id != authButtonBack {
		t.Fatalf("visible Back label is outside back hitbox: point=(%d,%d) zone=%+v", visibleX, visibleY, back)
	}
	_, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: visibleX, Y: visibleY})
	if m.auth.stage != authEditMenu {
		t.Fatalf("mouse Back did not return from nested auth screen: stage=%d", m.auth.stage)
	}
}

func TestModelsForAppliesOpenCodeZenOverrideToCachedModels(t *testing.T) {
	m := &Model{
		creds: &config.Credentials{Providers: map[string]config.Credential{
			"opencode-zen": {
				Models:         []string{"nemotron-3-ultra-free"},
				ContextWindows: map[string]int{"nemotron-3-ultra-free": 128_000},
				// A persisted source of api means the stored value came from the
				// endpoint; the hardcoded gateway override must not replace it.
				ContextSources: map[string]provider.ContextSource{
					"nemotron-3-ultra-free": provider.ContextSourceAPI,
				},
			},
		}},
	}

	models := m.modelsFor("opencode-zen")
	if len(models) != 1 || models[0].ContextWindow != 128_000 {
		t.Fatalf("API-reported cached context = %+v, want 128000 (override must not clobber API)", models)
	}
}

// TestModelsForOpenCodeZenOverrideFillsMissingValue: with no stored value the
// hardcoded gateway limit still fills in the gap.
func TestModelsForOpenCodeZenOverrideFillsMissingValue(t *testing.T) {
	m := &Model{
		creds: &config.Credentials{Providers: map[string]config.Credential{
			"opencode-zen": {
				Models: []string{"nemotron-3-ultra-free"},
			},
		}},
	}

	models := m.modelsFor("opencode-zen")
	if len(models) != 1 || models[0].ContextWindow != 1_000_000 {
		t.Fatalf("OpenCode Zen model context = %+v, want 1000000", models)
	}
}

func TestAuthModelSelectionUsesVisibleFilteredModel(t *testing.T) {
	m := newModelChoiceTestModel()
	m.creds = &config.Credentials{Providers: map[string]config.Credential{
		"openai": {OnlyFree: true},
	}}
	m.auth.draftID = "openai"
	m.auth.models = []catalog.Model{
		{ID: "paid-first", Free: false, ChatCapable: true},
		{ID: "free-model", Free: true, ChatCapable: true},
	}
	m.auth.cursor = 0

	_, _ = m.authModelKey("enter")
	if m.modelID != "openai/free-model" {
		t.Fatalf("filtered auth selection chose %q, want openai/free-model", m.modelID)
	}
}

func TestReloadProvidersClearsRemovedModel(t *testing.T) {
	m := newModelChoiceTestModel()
	m.modelID = "openai/fixture-model"
	m.creds = &config.Credentials{Providers: map[string]config.Credential{}}
	m.deps.Loaded = &config.Loaded{
		Config: config.Config{Providers: map[string]config.Provider{
			"openai": {APIKey: "fixture-key"},
		}},
	}
	m.deps.Providers = map[string]provider.Provider{
		"openai": modelChoiceTestProvider{},
	}

	m.reloadProviders()

	if m.modelID != "" {
		t.Fatalf("removed provider left model selected: %q", m.modelID)
	}
	if got := m.displayModel(); got != "None selected" {
		t.Fatalf("removed provider label = %q, want None selected", got)
	}
}

func TestActivityItemsIncludeRunningRegistryAgent(t *testing.T) {
	m := newModelChoiceTestModel()
	m.deps.AgentRegistry = agent.NewRegistry(2, 2)
	_, err := m.deps.AgentRegistry.Register(&agent.AgentEntry{
		Name: "worker", Status: agent.AgentRunning, Description: "inspect source",
	})
	if err != nil {
		t.Fatal(err)
	}

	items := m.activityItems()
	if len(items) != 1 || items[0].label != "worker" || items[0].kind != activityAgent {
		t.Fatalf("activity items = %+v, want one running worker", items)
	}
}
