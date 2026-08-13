package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"rick/internal/config"
	"rick/internal/provider/openai"
)

// ImageGenTool generates images through the ChatGPT OAuth login (the Codex
// backend's image_generation tool, gpt-image-2). It only works when the user
// has connected a ChatGPT account via the browser-login flow; otherwise Run
// returns a clear error. Registered unconditionally like VisionTool so a
// mid-session login is picked up on the next call.
type ImageGenTool struct {
	// Loaded is the live resolved config (same object the TUI mutates), used
	// for the cwd when resolving a relative out_dir.
	Loaded *config.Loaded
}

// Name implements Tool.
func (ImageGenTool) Name() string { return "imagegen" }

// ReadOnly implements Tool: it writes the generated image to disk.
func (ImageGenTool) ReadOnly() bool { return false }

// Description implements Tool.
func (ImageGenTool) Description() string {
	return "Generate an image from a text prompt using ChatGPT's image model " +
		"(gpt-image-2) through the ChatGPT OAuth login. Use this when the user " +
		"asks to generate, draw, create, or imagine an image. Requires the " +
		"ChatGPT account to be connected (browser login). Saves the image as a " +
		"PNG to the requested directory (or the Downloads folder by default) and " +
		"returns the saved path."
}

// Schema implements Tool.
func (ImageGenTool) Schema() map[string]any {
	return obj(map[string]any{
		"prompt":  strProp("The image prompt to generate."),
		"out_dir": pathProp("Optional directory to save the image in. Defaults to the user's Downloads folder."),
		"size":    enumProp("Optional output size.", "", "1024x1024", "1536x1024", "1024x1536"),
		"quality": enumProp("Optional quality.", "auto", "low", "medium", "high"),
	}, "prompt")
}

type imageGenArgs struct {
	Prompt  string `json:"prompt"`
	OutDir  string `json:"out_dir"`
	Size    string `json:"size"`
	Quality string `json:"quality"`
}

// Run implements Tool.
func (t ImageGenTool) Run(ctx context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a imageGenArgs
	if err := RepairDecode(in, &a, t.Schema(), tc.Repair); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	a.Prompt = strings.TrimSpace(a.Prompt)
	if a.Prompt == "" {
		return Errf("prompt is required"), nil
	}

	client, _, err := codexClient()
	if err != nil {
		return Errf("%v", err), nil
	}

	outDir := a.OutDir
	if strings.TrimSpace(outDir) == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return Errf("cannot find home directory: %v", homeErr), nil
		}
		outDir = filepath.Join(home, "Downloads")
	} else if !filepath.IsAbs(outDir) {
		outDir = filepath.Join(tc.Cwd, outDir)
	}
	outDir = filepath.Clean(outDir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Errf("cannot create output directory %s: %v", outDir, err), nil
	}

	results, err := client.GenerateImage(ctx, openai.CodexImageRequest{
		Prompt:  a.Prompt,
		Size:    a.Size,
		Quality: a.Quality,
	})
	if err != nil {
		return Errf("image generation failed: %v", err), nil
	}
	if len(results) == 0 {
		return Errf("image generation returned no results"), nil
	}

	var saved []string
	for i, res := range results {
		data, err := base64.StdEncoding.DecodeString(res.Base64)
		if err != nil {
			return Errf("image generation returned invalid base64: %v", err), nil
		}
		path := filepath.Join(outDir, imageFileName(a.Prompt, i))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return Errf("cannot save image to %s: %v", path, err), nil
		}
		saved = append(saved, path)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Generated %d image(s). Saved to:\n", len(saved))
	for _, p := range saved {
		fmt.Fprintln(&b, p)
	}
	if len(saved) == 1 {
		fmt.Fprintf(&b, "Click the path above to open the image.\n")
	}
	if results[0].RevisedPrompt != "" && results[0].RevisedPrompt != a.Prompt {
		fmt.Fprintf(&b, "Revised prompt: %s\n", results[0].RevisedPrompt)
	}
	return Result{Output: b.String(), Title: fmt.Sprintf("imagegen → %s", saved[0])}, nil
}

// codexClient builds an openai.Client for the ChatGPT provider from the saved
// credentials so the tool works with a login that happened mid-session. It
// returns an error when no ChatGPT OAuth credential is present.
func codexClient() (*openai.Client, *config.Credential, error) {
	creds, err := config.LoadCredentials()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot load credentials: %v", err)
	}
	cred, ok := creds.Get("chatgpt")
	if !ok {
		return nil, nil, fmt.Errorf("the ChatGPT account is not connected — run the ChatGPT browser-login flow in rick first")
	}
	client := openai.New("chatgpt", cred.APIKey, cred.BaseURL)
	client.SetCodex(cred.RefreshToken, cred.AccountID, cred.TokenExpiresAt, func(access, refresh string, expiresAt int64) {
		_ = creds.SaveTokens("chatgpt", access, refresh, expiresAt)
	})
	return client, &cred, nil
}

// imageFileName builds a filesystem-safe PNG name from the prompt plus a
// high-resolution timestamp so repeated calls never collide.
func imageFileName(prompt string, index int) string {
	slug := strings.ToLower(strings.TrimSpace(prompt))
	slug = imageFileRe.ReplaceAllString(slug, " ")
	slug = strings.Join(strings.Fields(slug), "_")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	if slug == "" {
		slug = "image"
	}
	ts := time.Now().Format("20060102-150405.000")
	n := imageNameSeq.Add(1)
	if index > 0 {
		return fmt.Sprintf("%s_%s_%d_%d.png", slug, ts, n, index)
	}
	return fmt.Sprintf("%s_%s_%d.png", slug, ts, n)
}

// imageNameSeq guarantees two calls within the same millisecond still get
// distinct filenames.
var imageNameSeq atomic.Uint64

// imageFileRe strips characters that are unsafe in filenames.
var imageFileRe = regexp.MustCompile(`[^a-z0-9]+`)
