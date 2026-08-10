package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rick/internal/config"
)

func TestVisionDSToggleOn(t *testing.T) {
	m := newModelChoiceTestModel()
	m.deps.Loaded = &config.Loaded{Config: config.Config{}}

	mm, _ := m.cmdVisionDS("on")
	m = mm.(*Model)

	if m.deps.Loaded.Config.Vision == nil || m.deps.Loaded.Config.Vision.Enabled == nil || !*m.deps.Loaded.Config.Vision.Enabled {
		t.Fatal("vision bridge should be enabled after /visionds on")
	}
	// Without a key the bridge reports off (it cannot do anything).
	if m.visionEnabled() {
		t.Fatal("visionEnabled should be false until a key is set")
	}

	// Set a key; the bridge is now live.
	m.deps.Loaded.Config.Vision.APIKey = "test-key"
	if !m.visionEnabled() {
		t.Fatal("visionEnabled should be true with enabled+key set")
	}
}

func TestVisionDSOff(t *testing.T) {
	enabled := true
	m := newModelChoiceTestModel()
	m.deps.Loaded = &config.Loaded{Config: config.Config{
		Vision: &config.VisionConfig{Enabled: &enabled, APIKey: "test-key"},
	}}

	mm, _ := m.cmdVisionDS("off")
	m = mm.(*Model)

	if m.deps.Loaded.Config.Vision == nil || m.deps.Loaded.Config.Vision.Enabled == nil || *m.deps.Loaded.Config.Vision.Enabled {
		t.Fatal("vision bridge should be disabled after /visionds off")
	}
	if m.visionEnabled() {
		t.Fatal("visionEnabled should be false after off")
	}
}

func TestVisionEnabledRequiresKey(t *testing.T) {
	enabled := true
	m := newModelChoiceTestModel()
	m.deps.Loaded = &config.Loaded{Config: config.Config{
		Vision: &config.VisionConfig{Enabled: &enabled},
	}}
	if m.visionEnabled() {
		t.Fatal("visionEnabled must be false when no API key is set")
	}
}

func TestVisionAPIKeySaveAndMask(t *testing.T) {
	m := newModelChoiceTestModel()
	m.deps.Loaded = &config.Loaded{Config: config.Config{}}

	mm, _ := m.cmdVisionAPI("AIzaSy0123456789abcdefghijklmnop")
	m = mm.(*Model)

	if got := m.deps.Loaded.Config.Vision.APIKey; got != "AIzaSy0123456789abcdefghijklmnop" {
		t.Fatalf("API key = %q, want saved key", got)
	}
	// The key must not leak in plaintext into the transcript.
	for _, msg := range m.msgs {
		if strings.Contains(msg.Text, "AIzaSy0123456789abcdefghijklmnop") {
			t.Fatalf("key leaked in transcript: %q", msg.Text)
		}
	}
}

func TestVisionAPIRejectsGarbage(t *testing.T) {
	m := newModelChoiceTestModel()
	m.deps.Loaded = &config.Loaded{Config: config.Config{}}

	mm, _ := m.cmdVisionAPI("not a key")
	m = mm.(*Model)
	if m.deps.Loaded.Config.Vision != nil && m.deps.Loaded.Config.Vision.APIKey != "" {
		t.Fatal("garbage key should not be saved")
	}
	if m.deps.Loaded.Config.Vision == nil {
		// fine — nothing written
	}
}

func TestVisionAPIClear(t *testing.T) {
	enabled := true
	m := newModelChoiceTestModel()
	m.deps.Loaded = &config.Loaded{Config: config.Config{
		Vision: &config.VisionConfig{Enabled: &enabled, APIKey: "secret-key"},
	}}

	mm, _ := m.cmdVisionAPI("clear")
	m = mm.(*Model)
	if got := m.deps.Loaded.Config.Vision.APIKey; got != "" {
		t.Fatalf("API key after clear = %q, want empty", got)
	}
}

func TestImagePathsInPrompt(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "shot.png")
	txt := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(png, []byte("fake-png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(txt, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := newModelChoiceTestModel()
	m.deps.Cwd = dir

	prompt := "look at " + png + " and " + txt + " also @" + png + ", thanks"
	got := m.imagePathsInPrompt(prompt, nil)
	if len(got) != 1 {
		t.Fatalf("imagePathsInPrompt = %v, want exactly the png path", got)
	}
	if filepath.Clean(got[0]) != filepath.Clean(png) {
		t.Fatalf("path = %q, want %q", got[0], png)
	}
}

func TestImagePathsInPromptSkipsAttached(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(png, []byte("fake-png"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := newModelChoiceTestModel()
	m.deps.Cwd = dir

	// A path that expandFileRefs already inlined must not be re-attached.
	got := m.imagePathsInPrompt("@shot.png what do you see", []string{"shot.png"})
	if len(got) != 0 {
		t.Fatalf("imagePathsInPrompt = %v, want none (already attached)", got)
	}
}

func TestVisionPatchKeepsEnabledWhenSettingKey(t *testing.T) {
	enabled := true
	m := newModelChoiceTestModel()
	m.deps.Loaded = &config.Loaded{Config: config.Config{
		Vision: &config.VisionConfig{Enabled: &enabled},
	}}

	// Setting a key must not drop the enabled flag from the persisted block.
	mm, _ := m.cmdVisionAPI("AIzaSy0123456789abcdefghijklmnop")
	m = mm.(*Model)
	patch := m.visionPatch(map[string]any{})
	if got, ok := patch["enabled"].(bool); !ok || !got {
		t.Fatalf("visionPatch dropped enabled: %#v", patch)
	}
	if got, ok := patch["api_key"].(string); !ok || got != "AIzaSy0123456789abcdefghijklmnop" {
		t.Fatalf("visionPatch missing api_key: %#v", patch)
	}
}

func TestVisionPatchKeepsKeyWhenToggling(t *testing.T) {
	enabled := true
	m := newModelChoiceTestModel()
	m.deps.Loaded = &config.Loaded{Config: config.Config{
		Vision: &config.VisionConfig{Enabled: &enabled, APIKey: "my-free-key"},
	}}

	// Toggling off must keep the key in the persisted block.
	mm, _ := m.cmdVisionDS("off")
	m = mm.(*Model)
	patch := m.visionPatch(map[string]any{})
	if got, ok := patch["api_key"].(string); !ok || got != "my-free-key" {
		t.Fatalf("visionPatch dropped api_key on toggle: %#v", patch)
	}
	if got, ok := patch["enabled"].(bool); !ok || got {
		t.Fatalf("visionPatch enabled should be false after off: %#v", patch)
	}
}
