package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"rick/internal/agent"
	"rick/internal/config"
	"rick/internal/plugin"
	"rick/internal/provider"
)

func TestCmdDesignEnablesAndArmsPrompt(t *testing.T) {
	m := newModelChoiceTestModel()
	m.runSlash("/design")

	if !m.designModeEnabled() {
		t.Fatal("/design did not enable design mode")
	}
	if m.pending.kind != pendingDesignPrompt || !m.pending.textInput {
		t.Fatalf("pending kind = %v textInput=%v, want pendingDesignPrompt text input", m.pending.kind, m.pending.textInput)
	}
}

func TestCmdDesignTaskStartsRun(t *testing.T) {
	m := newModelChoiceTestModel()
	_, cmd := m.cmdDesign("make the sidebar feel modern")

	if !m.designModeEnabled() {
		t.Fatal("/design <task> did not enable design mode")
	}
	// Without a configured provider startAgent fails fast and returns nil.
	// The design task must still be queued as the user message.
	if cmd != nil {
		t.Fatal("expected startAgent to fail fast without a provider")
	}
	lastUser := ""
	for _, msg := range m.msgs {
		if msg.Kind == MsgUser {
			lastUser = msg.Text
		}
	}
	if lastUser != "make the sidebar feel modern" {
		t.Fatalf("last user message = %q, want the design task", lastUser)
	}
}

func TestCmdDesignOffDisables(t *testing.T) {
	m := newModelChoiceTestModel()
	m.designMode = true
	m.runSlash("/design off")

	if m.designModeEnabled() {
		t.Fatal("/design off did not disable design mode")
	}
}

func TestDesignBriefInjectedIntoStableSystemPrompt(t *testing.T) {
	m := newModelChoiceTestModel()
	m.deps.Loaded = &config.Loaded{Config: config.Config{}}
	m.designMode = true

	stable, full := m.sessionSystemParts("build", "test-model", "C:\\work", "C:\\work", config.Config{}, []plugin.Skill{})

	if !strings.Contains(stable, "Design engineering brief") {
		t.Fatalf("design brief missing from stable system prompt: %q", stable)
	}
	if !strings.Contains(stable, "AI-generated look") {
		t.Fatalf("design brief missing the tells section: %q", stable)
	}
	if !strings.Contains(full, "Design engineering brief") {
		t.Fatal("design brief missing from full system prompt")
	}
}

func TestDesignToggleInvalidatesSysPartsFreeze(t *testing.T) {
	m := newModelChoiceTestModel()
	m.deps.Loaded = &config.Loaded{Config: config.Config{}}

	stableOff, _ := m.sessionSystemParts("build", "test-model", "C:\\work", "C:\\work", config.Config{}, nil)
	// Capture the frozen key state for the off configuration.
	keyOff := m.sysPartsKey

	m.designMode = true
	stableOn, _ := m.sessionSystemParts("build", "test-model", "C:\\work", "C:\\work", config.Config{}, nil)
	keyOn := m.sysPartsKey

	if keyOff == keyOn {
		t.Fatal("design toggle did not change the system-prompt freeze key")
	}
	if stableOff == stableOn {
		t.Fatal("design toggle did not change the stable system prompt")
	}
}

func TestAgentPromptPlacesDesignBriefBeforeProjectContext(t *testing.T) {
	m := newModelChoiceTestModel()
	m.designMode = true
	stable, _ := m.sessionSystemParts("build", "test-model", "C:\\work", "C:\\work", config.Config{}, nil)
	briefIndex := strings.Index(stable, "Design engineering brief")
	buildIndex := strings.Index(stable, "You are rick")
	if briefIndex < 0 || buildIndex < 0 {
		t.Fatalf("design brief (%d) or base prompt (%d) missing", briefIndex, buildIndex)
	}
	if buildIndex > briefIndex {
		t.Fatalf("design brief must follow the base prompt: base=%d brief=%d", buildIndex, briefIndex)
	}
	_ = agent.BuildPrompt
}

var _ = provider.RoleUser
var _ = tea.KeyMsg{}
