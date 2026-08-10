package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"rick/internal/config"
	"rick/internal/vision"
)

// ---------- /visionds ----------

// cmdVisionDS toggles the vision bridge that gives a text-only model
// (DeepSeek) sight: with no argument it flips the current state, or takes
// on|off. The choice is persisted to the global rick.json.
func (m *Model) cmdVisionDS(args string) (tea.Model, tea.Cmd) {
	cur := false
	if cfg := m.deps.Loaded.Config.Vision; cfg != nil && cfg.Enabled != nil && *cfg.Enabled {
		cur = true
	}

	want := !cur
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "on", "yes", "true", "1":
		want = true
	case "off", "no", "false", "0":
		want = false
	case "toggle":
		want = !cur
	case "":
	default:
		m.appendMsg(ChatMsg{Kind: MsgError,
			Text: "usage: /visionds [on|off]", Time: time.Now()})
		return m, nil
	}

	enabled := want
	// Persist the full vision block: patchGlobal shallow-replaces the whole
	// "vision" object, so writing only {enabled} would wipe the saved key.
	if err := config.SaveConfigPatch(map[string]any{
		"vision": m.visionPatch(map[string]any{"enabled": enabled}),
	}); err != nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "vision: " + err.Error(), Time: time.Now()})
		return m, nil
	}
	// Apply to the live config so the current session sees it immediately.
	if m.deps.Loaded.Config.Vision == nil {
		m.deps.Loaded.Config.Vision = &config.VisionConfig{}
	}
	m.deps.Loaded.Config.Vision.Enabled = &enabled

	if want {
		m.appendMsg(ChatMsg{Kind: MsgSystem,
			Text: "vision bridge ON — image attachments are read by " + vision.DefaultModel + " and the evidence is given to the text-only model.\nSet a free key with /visionapi <key> if you have not already.", Time: time.Now()})
	} else {
		m.appendMsg(ChatMsg{Kind: MsgSystem,
			Text: "vision bridge OFF — image attachments go to the active model untouched.", Time: time.Now()})
	}
	m.setStatus(fmt.Sprintf("vision: %s", onOff(want)))
	return m, nil
}

// ---------- /visionapi ----------

// cmdVisionAPI sets or clears the vision bridge API key (the free Google AI
// Studio tier). The key is persisted to the global rick.json.
func (m *Model) cmdVisionAPI(args string) (tea.Model, tea.Cmd) {
	key := strings.TrimSpace(args)
	if key == "" {
		cur := ""
		if cfg := m.deps.Loaded.Config.Vision; cfg != nil {
			cur = cfg.APIKey
		}
		if cur == "" {
			m.appendMsg(ChatMsg{Kind: MsgSystem,
				Text: "no vision API key set. Get a free Google AI Studio key at https://aistudio.google.com and run /visionapi <key>.", Time: time.Now()})
		} else {
			m.appendMsg(ChatMsg{Kind: MsgSystem,
				Text: "vision API key: " + maskKey(cur) + " (clear with /visionapi clear)", Time: time.Now()})
		}
		return m, nil
	}

	if strings.EqualFold(key, "clear") || strings.EqualFold(key, "remove") || strings.EqualFold(key, "delete") {
		if err := config.SaveConfigPatch(map[string]any{
			"vision": m.visionPatch(map[string]any{"api_key": ""}),
		}); err != nil {
			m.appendMsg(ChatMsg{Kind: MsgError, Text: "vision: " + err.Error(), Time: time.Now()})
			return m, nil
		}
		if m.deps.Loaded.Config.Vision != nil {
			m.deps.Loaded.Config.Vision.APIKey = ""
		}
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "vision API key cleared.", Time: time.Now()})
		return m, nil
	}

	// Reject keys that are obviously not Gemini keys (a pasted prompt or a
	// path), mirroring the auth flow's sanity checks.
	if strings.ContainsAny(key, " \n") || len(key) < 10 {
		m.appendMsg(ChatMsg{Kind: MsgError,
			Text: "that does not look like an API key (must be a single token of at least 10 characters).", Time: time.Now()})
		return m, nil
	}

	if err := config.SaveConfigPatch(map[string]any{
		"vision": m.visionPatch(map[string]any{"api_key": key}),
	}); err != nil {
		m.appendMsg(ChatMsg{Kind: MsgError, Text: "vision: " + err.Error(), Time: time.Now()})
		return m, nil
	}
	if m.deps.Loaded.Config.Vision == nil {
		m.deps.Loaded.Config.Vision = &config.VisionConfig{}
	}
	m.deps.Loaded.Config.Vision.APIKey = key

	m.appendMsg(ChatMsg{Kind: MsgSystem,
		Text: "vision API key saved (" + maskKey(key) + "). Toggle the bridge with /visionds on.", Time: time.Now()})
	m.setStatus("vision API key saved")
	return m, nil
}

// visionPatch builds the full vision block to persist, layered on the current
// live config so a partial change (enabled xor api_key) never wipes the other.
func (m *Model) visionPatch(over map[string]any) map[string]any {
	out := map[string]any{}
	cur := m.deps.Loaded.Config.Vision
	if cur != nil {
		if cur.Enabled != nil {
			out["enabled"] = *cur.Enabled
		}
		if cur.APIKey != "" {
			out["api_key"] = cur.APIKey
		}
		if cur.Model != "" {
			out["model"] = cur.Model
		}
		if cur.BaseURL != "" {
			out["base_url"] = cur.BaseURL
		}
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}
