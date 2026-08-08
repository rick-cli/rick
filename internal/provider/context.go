package provider

import (
	"regexp"
	"strconv"
	"strings"

	"rick/internal/modelsdev"
)

// ProviderContextWindow returns an explicit provider-specific context window
// when the provider's deployment is smaller or larger than the base model.
func ProviderContextWindow(providerID, modelID string) (int, bool) {
	if !strings.EqualFold(strings.TrimSpace(providerID), "opencode-zen") {
		return 0, false
	}

	id := strings.ToLower(strings.TrimSpace(modelID))
	switch id {
	case "big-pickle":
		return 200_000, true
	case "deepseek-v4-flash-free", "mimo-v2.5-free":
		return 200_000, true
	case "laguna-s-2.1-free", "north-mini-code-free":
		return 256_000, true
	case "ling-3.0-flash-free":
		return 262_144, true
	case "nemotron-3-ultra-free":
		return 1_000_000, true
	default:
		return 0, false
	}
}

// KnownProviderContextWindow returns a provider-specific override when one is
// known, otherwise falling back to the generic model-id catalog.
func KnownProviderContextWindow(providerID, modelID string) int {
	if ctx, ok := ProviderContextWindow(providerID, modelID); ok {
		return ctx
	}
	return KnownContextWindow(modelID)
}

// KnownContextWindow returns a model's context window when it can be inferred
// from its id, or 0.
//
// Most OpenAI-compatible /models endpoints report no context length at all, so
// without this every model would fall back to a generic default and the usage
// gauge would be meaningless. The id is the only signal available.
func KnownContextWindow(modelID string) int {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if id == "" {
		return 0
	}

	// An explicit size in the id wins: "-128k", "-1m", "-32768".
	if n := sizeFromID(id); n > 0 {
		return n
	}

	// The embedded models.dev catalog is a maintained model-specific source and
	// is more reliable than broad family guesses. Explicit size suffixes above
	// still win because they usually describe a deployment-specific limit.
	if ctx, ok := modelsdev.Lookup(modelID); ok {
		return ctx
	}

	switch {
	// LongCat.
	case strings.HasPrefix(id, "longcat"):
		return 1_000_000

	// Anthropic.
	case strings.Contains(id, "claude"):
		return 200_000

	// OpenAI.
	case strings.HasPrefix(id, "gpt-5"), strings.Contains(id, "gpt-5"):
		return 400_000
	case strings.HasPrefix(id, "gpt-4.1"):
		return 1_047_576
	case strings.HasPrefix(id, "gpt-4o"):
		return 128_000
	case strings.HasPrefix(id, "o1"), strings.HasPrefix(id, "o3"), strings.HasPrefix(id, "o4"):
		return 200_000
	case strings.Contains(id, "codex"):
		return 400_000

	// Google.
	case strings.Contains(id, "gemini-2.5"), strings.Contains(id, "gemini-2.0"):
		return 1_048_576
	case strings.Contains(id, "gemini-1.5-pro"):
		return 2_097_152
	case strings.Contains(id, "gemini"):
		return 1_048_576

	// Chinese labs.
	case strings.HasPrefix(id, "deepseek-v4-flash-free"):
		return 200_000
	case strings.HasPrefix(id, "deepseek-v4-flash"), strings.HasPrefix(id, "deepseek-v4-pro"):
		return 1_000_000
	case strings.Contains(id, "deepseek"):
		return 128_000
	case strings.Contains(id, "kimi"), strings.Contains(id, "moonshot"):
		return 256_000
	case strings.Contains(id, "glm-4.6"), strings.Contains(id, "glm-5"):
		return 200_000
	case strings.Contains(id, "glm"):
		return 128_000
	case strings.Contains(id, "minimax"):
		return 1_000_000
	case strings.Contains(id, "qwen3"), strings.Contains(id, "qwen-max"):
		return 262_144
	case strings.Contains(id, "qwen"):
		return 131_072
	case strings.Contains(id, "step-"):
		return 256_000

	// Others.
	case strings.Contains(id, "grok-4"):
		return 256_000
	case strings.Contains(id, "grok"):
		return 131_072
	case strings.Contains(id, "mistral-large"):
		return 131_072
	case strings.Contains(id, "llama-4"):
		return 1_048_576
	case strings.Contains(id, "llama-3.3"), strings.Contains(id, "llama-3.1"):
		return 131_072
	case strings.Contains(id, "nemotron"):
		return 131_072
	}

	return 0
}

var sizeRE = regexp.MustCompile(`[-_:](\d+)(k|m)\b`)

// sizeFromID reads an explicit window size out of a model id.
func sizeFromID(id string) int {
	if mm := sizeRE.FindStringSubmatch(id); mm != nil {
		n, err := strconv.Atoi(mm[1])
		if err != nil {
			return 0
		}
		switch mm[2] {
		case "k":
			return n * 1000
		case "m":
			return n * 1_000_000
		}
	}
	return 0
}
