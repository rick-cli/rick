package provider

import "strings"

// ReasoningEffort is how hard a model should think before answering. The
// values are kept provider-neutral and translated by each wire adapter.
type ReasoningEffort string

const (
	ReasoningOff     ReasoningEffort = "off"
	ReasoningOn      ReasoningEffort = "on"
	ReasoningMinimal ReasoningEffort = "minimal"
	ReasoningLow     ReasoningEffort = "low"
	ReasoningMedium  ReasoningEffort = "medium"
	ReasoningHigh    ReasoningEffort = "high"
	ReasoningXHigh   ReasoningEffort = "xhigh"
	ReasoningMax     ReasoningEffort = "max"
)

// ReasoningCapabilities describes the controls that are valid for one model.
// Efforts are ordered from disabled/least effort to greatest effort, except
// for boolean-style controls where On follows Off.
type ReasoningCapabilities struct {
	Style     ReasoningStyle
	Efforts   []ReasoningEffort
	Default   ReasoningEffort
	Mandatory bool
	Known     bool
}

// ReasoningLevels returns the complete vocabulary understood by Rick. The TUI
// must use ReasoningCapabilities.Efforts instead of this fallback list.
func ReasoningLevels() []ReasoningEffort {
	return []ReasoningEffort{
		ReasoningOff,
		ReasoningMinimal,
		ReasoningLow,
		ReasoningMedium,
		ReasoningHigh,
		ReasoningXHigh,
		ReasoningMax,
	}
}

// Valid reports whether e is a known level or boolean enable value.
func (e ReasoningEffort) Valid() bool {
	switch e {
	case ReasoningOff, ReasoningOn, ReasoningMinimal, ReasoningLow, ReasoningMedium,
		ReasoningHigh, ReasoningXHigh, ReasoningMax:
		return true
	}
	return false
}

// ParseEffort reads a level from user input, tolerating shorthand without
// collapsing distinct wire values such as max and xhigh into high.
func ParseEffort(s string) (ReasoningEffort, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "none", "no", "disabled", "false":
		return ReasoningOff, true
	case "on", "enable", "enabled", "yes", "true", "auto":
		return ReasoningOn, true
	case "minimal", "min", "xs":
		return ReasoningMinimal, true
	case "low", "l", "s":
		return ReasoningLow, true
	case "medium", "med", "m", "default":
		return ReasoningMedium, true
	case "high", "h":
		return ReasoningHigh, true
	case "xhigh", "x-high", "xxl":
		return ReasoningXHigh, true
	case "max", "xl":
		return ReasoningMax, true
	}
	return "", false
}

// Budget converts a level into an Anthropic thinking budget, scaled to the
// response limit. Anthropic requires budget_tokens < max_tokens, and a budget
// below 1024 is rejected outright.
func (e ReasoningEffort) Budget(maxTokens int) int {
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	var frac float64
	switch e {
	case ReasoningMinimal:
		frac = 0.15
	case ReasoningOn:
		frac = 0.5
	case ReasoningLow:
		frac = 0.25
	case ReasoningMedium:
		frac = 0.5
	case ReasoningHigh:
		frac = 0.8
	case ReasoningXHigh, ReasoningMax:
		frac = 0.95
	default:
		return 0
	}
	budget := int(float64(maxTokens) * frac)
	if budget < 1024 {
		budget = 1024
	}
	// Leave room for the visible answer.
	if budget > maxTokens-512 {
		budget = maxTokens - 512
	}
	if budget < 1024 {
		return 0 // the response limit is too small for thinking at all
	}
	return budget
}

// ReasoningStyle is the dialect a model expects.
type ReasoningStyle string

const (
	ReasoningStyleNone      ReasoningStyle = ""          // known non-reasoning model
	ReasoningStyleUnknown   ReasoningStyle = "unknown"   // no reliable capability signal
	ReasoningStyleOpenAI    ReasoningStyle = "effort"    // reasoning_effort
	ReasoningStyleGLM       ReasoningStyle = "glm"       // thinking.type enabled
	ReasoningStyleDeepSeek  ReasoningStyle = "deepseek"  // reasoning_effort + thinking.type
	ReasoningStyleAnthropic ReasoningStyle = "budget"    // thinking.budget_tokens
	ReasoningStyleQwen      ReasoningStyle = "enable"    // enable_thinking bool
	ReasoningStyleAlways    ReasoningStyle = "always_on" // reasons unconditionally
)

// DetectReasoning infers the wire dialect and a conservative default from a
// model id. Live model metadata takes precedence in
// ReasoningCapabilitiesForProvider.
func DetectReasoning(modelID string) (ReasoningStyle, ReasoningEffort) {
	id := strings.ToLower(modelID)
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}

	switch {
	case strings.Contains(id, "claude"):
		if strings.Contains(id, "-4") || strings.Contains(id, "3-7") ||
			strings.Contains(id, "3.7") || strings.Contains(id, "opus-4") {
			return ReasoningStyleAnthropic, ReasoningMedium
		}
		return ReasoningStyleNone, ReasoningOff

	case strings.HasPrefix(id, "o1"), strings.HasPrefix(id, "o3"), strings.HasPrefix(id, "o4"):
		return ReasoningStyleOpenAI, ReasoningMedium
	case strings.HasPrefix(id, "gpt-5"), strings.Contains(id, "gpt-5"),
		strings.Contains(id, "gpt-oss"):
		return ReasoningStyleOpenAI, ReasoningMedium
	case strings.Contains(id, "codex"):
		return ReasoningStyleOpenAI, ReasoningMedium

	case strings.Contains(id, "deepseek-r"), strings.Contains(id, "deepseek-reason"):
		return ReasoningStyleAlways, ReasoningMedium
	case strings.Contains(id, "deepseek-v3.2"), strings.Contains(id, "deepseek-v4"):
		return ReasoningStyleDeepSeek, ReasoningMedium

	case strings.Contains(id, "qwq"), strings.Contains(id, "qwen3"):
		return ReasoningStyleQwen, ReasoningMedium

	case strings.Contains(id, "gemini-2.5"), strings.Contains(id, "gemini-3"):
		return ReasoningStyleOpenAI, ReasoningMedium

	case strings.Contains(id, "glm-4.5"), strings.Contains(id, "glm-4.6"),
		strings.Contains(id, "glm-4.7"), strings.Contains(id, "glm-5"):
		return ReasoningStyleGLM, ReasoningMedium

	case strings.Contains(id, "grok-3"), strings.Contains(id, "grok-4"):
		return ReasoningStyleOpenAI, ReasoningMedium
	case strings.Contains(id, "minimax-m"):
		return ReasoningStyleOpenAI, ReasoningMedium
	case strings.Contains(id, "thinking"), strings.Contains(id, "reasoner"):
		return ReasoningStyleOpenAI, ReasoningMedium

	case strings.HasPrefix(id, "gpt-4"), strings.HasPrefix(id, "gpt-3.5"),
		strings.Contains(id, "gemini-1.5"), strings.Contains(id, "gemini-2.0"),
		strings.Contains(id, "claude-3-5"), strings.Contains(id, "claude-3.5"),
		strings.Contains(id, "llama"):
		return ReasoningStyleNone, ReasoningOff
	}
	return ReasoningStyleUnknown, ReasoningOff
}

// DetectReasoningForProvider applies the endpoint's wire flavor to the model
// heuristic. A normalized gateway can expose a model's native dialect through
// one common reasoning_effort field.
func DetectReasoningForProvider(providerID, modelID string) (ReasoningStyle, ReasoningEffort) {
	style, effort := DetectReasoning(modelID)
	providerID = strings.ToLower(strings.TrimSpace(providerID))

	switch {
	case providerID == "openrouter" && (style == ReasoningStyleGLM ||
		style == ReasoningStyleDeepSeek || style == ReasoningStyleQwen ||
		style == ReasoningStyleAnthropic):
		return ReasoningStyleOpenAI, effort
	case strings.Contains(strings.ToLower(modelID), "minimax-m") && providerID == "anthropic":
		return ReasoningStyleAnthropic, effort
	}
	return style, effort
}

// ReasoningCapabilitiesForProvider returns only controls valid for the active
// model. Advertised metadata is authoritative when present; the model/provider
// matrix below covers direct endpoints that do not publish capabilities.
func ReasoningCapabilitiesForProvider(providerID, modelID string, advertised *ModelInfo) ReasoningCapabilities {
	style, detectedDefault := DetectReasoningForProvider(providerID, modelID)
	caps := ReasoningCapabilities{Style: style, Default: detectedDefault}

	if advertised != nil && advertised.ReasoningKnown {
		caps.Known = true
		switch {
		case advertised.ReasoningEffortsAll:
			caps.Efforts = ReasoningLevels()
		case advertised.ReasoningEffortsKnown:
			caps.Efforts = normalizeEfforts(advertised.ReasoningEfforts)
			if len(caps.Efforts) == 0 {
				caps.Efforts = advertisedEnablementEfforts(style)
			}
		default:
			// A reasoning object without supported_efforts confirms that
			// reasoning exists, but not that the model is enablement-only.
			// Preserve the provider/model fallback until the endpoint gives
			// us an explicit effort vocabulary.
			caps.Efforts, caps.Mandatory = fallbackEfforts(providerID, modelID, style)
		}
		caps.Mandatory = caps.Mandatory || advertised.ReasoningMandatory
		if advertised.ReasoningDefault.Valid() {
			caps.Default = advertised.ReasoningDefault
		}
		if advertised.ReasoningDefaultEnabledKnown && !advertised.ReasoningDefaultEnabled {
			caps.Default = ReasoningOff
		}
		if len(caps.Efforts) == 0 {
			caps.Efforts = advertisedEnablementEfforts(style)
		}
	} else {
		caps.Efforts, caps.Mandatory = fallbackEfforts(providerID, modelID, style)
		caps.Default = fallbackDefault(modelID, style, caps.Efforts, caps.Default)
	}

	caps.Efforts = normalizeEfforts(caps.Efforts)
	if caps.Mandatory {
		caps.Efforts = removeEffort(caps.Efforts, ReasoningOff)
	} else if style != ReasoningStyleAlways && style != ReasoningStyleNone {
		caps.Efforts = addEffort(caps.Efforts, ReasoningOff)
	}
	if len(caps.Efforts) == 0 && style != ReasoningStyleNone && style != ReasoningStyleAlways {
		caps.Efforts = []ReasoningEffort{ReasoningOff}
	}
	if !containsEffort(caps.Efforts, caps.Default) {
		caps.Default = defaultEffort(caps.Efforts)
	}
	return caps
}

func fallbackDefault(modelID string, style ReasoningStyle, efforts []ReasoningEffort, detected ReasoningEffort) ReasoningEffort {
	id := strings.ToLower(modelID)
	if style == ReasoningStyleOpenAI {
		nativeStyle, _ := DetectReasoning(modelID)
		if nativeStyle == ReasoningStyleQwen || nativeStyle == ReasoningStyleGLM || nativeStyle == ReasoningStyleDeepSeek {
			style = nativeStyle
		}
	}
	switch style {
	case ReasoningStyleQwen:
		return ReasoningOn
	case ReasoningStyleGLM:
		if strings.Contains(id, "glm-5.2") {
			return ReasoningMax
		}
		return ReasoningOn
	case ReasoningStyleDeepSeek:
		return ReasoningHigh
	case ReasoningStyleOpenAI:
		if strings.Contains(id, "grok-4") {
			return ReasoningHigh
		}
	}
	if containsEffort(efforts, detected) {
		return detected
	}
	return defaultEffort(efforts)
}

func fallbackEfforts(providerID, modelID string, style ReasoningStyle) ([]ReasoningEffort, bool) {
	id := strings.ToLower(modelID)
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if strings.EqualFold(providerID, "openrouter") && style == ReasoningStyleOpenAI {
		nativeStyle, _ := DetectReasoning(modelID)
		switch nativeStyle {
		case ReasoningStyleQwen:
			return []ReasoningEffort{ReasoningOff, ReasoningOn}, false
		case ReasoningStyleGLM:
			if strings.Contains(id, "glm-5.2") {
				return ReasoningLevels(), false
			}
			return []ReasoningEffort{ReasoningOff, ReasoningOn}, false
		case ReasoningStyleDeepSeek:
			return []ReasoningEffort{ReasoningOff, ReasoningLow, ReasoningHigh, ReasoningMax}, false
		case ReasoningStyleAnthropic:
			return []ReasoningEffort{ReasoningOff, ReasoningOn}, false
		}
	}

	switch style {
	case ReasoningStyleNone, ReasoningStyleAlways:
		return nil, false
	case ReasoningStyleQwen:
		return []ReasoningEffort{ReasoningOff, ReasoningOn}, false
	case ReasoningStyleGLM:
		if strings.Contains(id, "glm-5.2") {
			return ReasoningLevels(), false
		}
		return []ReasoningEffort{ReasoningOff, ReasoningOn}, false
	case ReasoningStyleDeepSeek:
		return []ReasoningEffort{ReasoningOff, ReasoningLow, ReasoningHigh, ReasoningMax}, false
	case ReasoningStyleAnthropic:
		return []ReasoningEffort{ReasoningOff, ReasoningLow, ReasoningMedium, ReasoningHigh}, false
	case ReasoningStyleOpenAI:
		return openAIEfforts(id)
	default:
		// Unknown models keep a safe off state and one explicit generic opt-in;
		// they no longer display levels that the endpoint may reject.
		return []ReasoningEffort{ReasoningOff, ReasoningOn}, false
	}
}

func advertisedEnablementEfforts(style ReasoningStyle) []ReasoningEffort {
	switch style {
	case ReasoningStyleNone, ReasoningStyleAlways:
		return nil
	default:
		return []ReasoningEffort{ReasoningOff, ReasoningOn}
	}
}

func openAIEfforts(id string) ([]ReasoningEffort, bool) {
	switch {
	case strings.Contains(id, "grok-4.20"):
		return []ReasoningEffort{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh}, true
	case strings.Contains(id, "grok-4.5"):
		return []ReasoningEffort{ReasoningLow, ReasoningMedium, ReasoningHigh}, true
	case strings.HasPrefix(id, "o1"), strings.HasPrefix(id, "o3"), strings.HasPrefix(id, "o4"):
		return []ReasoningEffort{ReasoningLow, ReasoningMedium, ReasoningHigh}, true
	case strings.Contains(id, "gemini-3.1-pro"):
		return []ReasoningEffort{ReasoningLow, ReasoningMedium, ReasoningHigh}, true
	case strings.Contains(id, "gemini-3-pro"):
		return []ReasoningEffort{ReasoningLow, ReasoningHigh}, true
	case strings.Contains(id, "gemini-3.1-flash-lite-image"):
		return []ReasoningEffort{ReasoningMinimal, ReasoningHigh}, true
	case strings.Contains(id, "gemini-3"):
		return []ReasoningEffort{ReasoningMinimal, ReasoningLow, ReasoningMedium, ReasoningHigh}, true
	case strings.Contains(id, "gemini-2.5-pro"):
		return []ReasoningEffort{ReasoningLow, ReasoningMedium, ReasoningHigh}, true
	case strings.Contains(id, "gemini-2.5"):
		return []ReasoningEffort{ReasoningOff, ReasoningLow, ReasoningMedium, ReasoningHigh}, false
	case strings.Contains(id, "gpt-5.6"):
		return []ReasoningEffort{ReasoningOff, ReasoningMinimal, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMax}, false
	case strings.Contains(id, "gpt-5.4"), strings.Contains(id, "gpt-5.5"):
		return []ReasoningEffort{ReasoningOff, ReasoningMinimal, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh}, false
	case strings.Contains(id, "gpt-oss"):
		return []ReasoningEffort{ReasoningOff, ReasoningMinimal, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh}, false
	case strings.Contains(id, "codex"):
		return []ReasoningEffort{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh}, true
	case strings.Contains(id, "gpt-5.2"), strings.Contains(id, "gpt-5.3"):
		return []ReasoningEffort{ReasoningOff, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh}, false
	case strings.Contains(id, "gpt-5.1-codex-max"):
		return []ReasoningEffort{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh}, true
	case strings.Contains(id, "gpt-5.1"):
		return []ReasoningEffort{ReasoningOff, ReasoningLow, ReasoningMedium, ReasoningHigh}, false
	case strings.HasPrefix(id, "gpt-5"):
		return []ReasoningEffort{ReasoningMinimal, ReasoningLow, ReasoningMedium, ReasoningHigh}, true
	case strings.Contains(id, "minimax-m"):
		return []ReasoningEffort{ReasoningOff, ReasoningLow, ReasoningMedium, ReasoningHigh}, false
	default:
		return []ReasoningEffort{ReasoningOff, ReasoningLow, ReasoningMedium, ReasoningHigh}, false
	}
}

func normalizeEfforts(efforts []ReasoningEffort) []ReasoningEffort {
	if len(efforts) == 0 {
		return nil
	}
	ordered := make([]ReasoningEffort, 0, len(efforts))
	for _, candidate := range ReasoningLevels() {
		if containsEffort(efforts, candidate) {
			ordered = append(ordered, candidate)
		}
	}
	if containsEffort(efforts, ReasoningOn) {
		ordered = append(ordered, ReasoningOn)
	}
	return ordered
}

func containsEffort(efforts []ReasoningEffort, wanted ReasoningEffort) bool {
	for _, effort := range efforts {
		if effort == wanted {
			return true
		}
	}
	return false
}

func addEffort(efforts []ReasoningEffort, effort ReasoningEffort) []ReasoningEffort {
	if containsEffort(efforts, effort) {
		return efforts
	}
	return normalizeEfforts(append(append([]ReasoningEffort(nil), effort), efforts...))
}

func removeEffort(efforts []ReasoningEffort, effort ReasoningEffort) []ReasoningEffort {
	out := make([]ReasoningEffort, 0, len(efforts))
	for _, candidate := range efforts {
		if candidate != effort {
			out = append(out, candidate)
		}
	}
	return out
}

func defaultEffort(efforts []ReasoningEffort) ReasoningEffort {
	for i := len(efforts) - 1; i >= 0; i-- {
		if efforts[i] != ReasoningOff {
			return efforts[i]
		}
	}
	if containsEffort(efforts, ReasoningOff) {
		return ReasoningOff
	}
	return ""
}
