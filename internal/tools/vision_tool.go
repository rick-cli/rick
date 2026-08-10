package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"rick/internal/config"
	"rick/internal/vision"
)

// VisionTool lets the agent itself inspect an image: it sends the file to the
// configured vision model (Gemini) and returns structured text evidence, so a
// text-only model like DeepSeek can reason about screenshots and diagrams
// without a vision-capable backend.
type VisionTool struct {
	// Loaded is the live resolved config. It is the same object the TUI
	// mutates (/visionapi, /visionds), so a key set mid-session is visible
	// immediately.
	Loaded *config.Loaded
}

// Name implements Tool.
func (VisionTool) Name() string { return "vision" }

// ReadOnly implements Tool: it only reads a file and calls an external API.
func (VisionTool) ReadOnly() bool { return true }

// Description implements Tool.
func (VisionTool) Description() string {
	return "Inspect an image file with a vision model and return structured text " +
		"evidence (OCR, layout, semantics). Use this to read screenshots, diagrams, " +
		"charts, or any image whose contents the text model cannot see directly. " +
		"Requires the vision bridge to be enabled and an API key set (/visionds, /visionapi)."
}

// Schema implements Tool.
func (VisionTool) Schema() map[string]any {
	return obj(map[string]any{
		"path":  strProp("Path to the image file (.png, .jpg, .jpeg, .gif, .bmp, .webp, .tiff, .ico)."),
		"focus": strProp("Optional: what you specifically want to know about the image. The full evidence is still returned."),
	}, "path")
}

type visionArgs struct {
	Path  string `json:"path"`
	Focus string `json:"focus"`
}

// Run implements Tool.
func (t VisionTool) Run(ctx context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a visionArgs
	if err := decodeArgs(in, &a); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	a.Path = strings.TrimSpace(a.Path)
	if a.Path == "" {
		return Errf("path is required"), nil
	}

	p := a.Path
	if !filepath.IsAbs(p) {
		p = filepath.Join(tc.Cwd, p)
	}
	p = filepath.Clean(p)

	st, err := os.Stat(p)
	if err != nil {
		return Errf("cannot stat %s: %v", p, err), nil
	}
	if st.IsDir() {
		return Errf("%s is a directory, not an image", p), nil
	}
	if !toolIsImageFile(p) {
		return Errf("%s is not a supported image file (use .png, .jpg, .jpeg, .gif, .bmp, .webp, .tiff, .ico)", p), nil
	}

	cfg := t.visionConfig()
	if cfg.APIKey == "" {
		return Errf("vision bridge has no API key — the user must run /visionapi <key> in rick, or set vision.api_key in rick.json"), nil
	}

	data, err := os.ReadFile(p)
	if err != nil {
		return Errf("cannot read %s: %v", p, err), nil
	}

	result, err := vision.New(cfg).Analyze(ctx, toolMediaTypeFor(p), base64.StdEncoding.EncodeToString(data), a.Focus)
	if err != nil {
		return Errf("vision model error: %v", err), nil
	}

	rendered := vision.Render(result)
	if rendered == "" {
		return Result{Output: "the vision model returned no evidence for " + p, Title: "vision"}, nil
	}
	return Result{
		Output: rendered,
		Title:  fmt.Sprintf("vision %s", filepath.Base(p)),
	}, nil
}

// visionConfig resolves the bridge settings from the live config.
func (t VisionTool) visionConfig() vision.Config {
	if t.Loaded == nil || t.Loaded.Config.Vision == nil {
		return vision.Config{}
	}
	cfg := t.Loaded.Config.Vision
	return vision.Config{
		APIKey:  strings.TrimSpace(cfg.APIKey),
		Model:   cfg.Model,
		BaseURL: cfg.BaseURL,
	}
}

// toolIsImageFile reports whether path has an image extension.
func toolIsImageFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".bmp", ".webp", ".tiff", ".tif", ".ico":
		return true
	default:
		return false
	}
}

// toolMediaTypeFor returns the MIME type for an image path.
func toolMediaTypeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".tiff", ".tif":
		return "image/tiff"
	case ".ico":
		return "image/x-icon"
	}
	return "application/octet-stream"
}
