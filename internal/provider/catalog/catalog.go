// Package catalog holds the built-in provider registry — the list rick offers
// in /auth before the user has configured anything.
//
// Entries are ported from the Hermes agent's PROVIDER_REGISTRY so the two
// tools agree on ids, endpoints and environment variables.
package catalog

import (
	"os"
	"sort"
	"strings"
)

// Auth kinds.
const (
	AuthAPIKey     = "api_key"           // paste a key
	AuthNone       = "none"              // local server, no credentials
	AuthDeviceCode = "oauth_device_code" // RFC 8628 browser+code flow
	AuthExternal   = "oauth_external"    // vendor CLI owns the flow
)

// Flavor identifies the wire protocol.
const (
	FlavorOpenAI    = "openai"
	FlavorAnthropic = "anthropic"
)

// Entry describes one known provider.
type Entry struct {
	ID      string
	Name    string
	Auth    string
	Flavor  string
	BaseURL string
	KeyEnv  []string   // checked in order; first non-empty wins
	BaseEnv string     // optional base-url override variable
	Note    string     // shown in the picker when non-empty
	KeyHint string     // where to get a key
	OAuth   DeviceAuth // device-code flow (RFC 8628 DeviceFlow or CodexDeviceFlow)
	// CopilotExchange marks providers that need a post-OAuth token exchange
	// (e.g. GitHub OAuth → Copilot API token).
	CopilotExchange bool `json:"-"`
}

// Registry is the built-in list, in display order: the providers most people
// want first, then the long tail alphabetically.
var Registry = []Entry{
	{ID: "anthropic", Name: "Anthropic", Auth: AuthAPIKey, Flavor: FlavorAnthropic,
		BaseURL: "https://api.anthropic.com",
		KeyEnv:  []string{"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN"}, BaseEnv: "ANTHROPIC_BASE_URL",
		KeyHint: "console.anthropic.com/settings/keys"},

	{ID: "openai", Name: "OpenAI", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.openai.com/v1",
		KeyEnv:  []string{"OPENAI_API_KEY"}, BaseEnv: "OPENAI_BASE_URL",
		KeyHint: "platform.openai.com/api-keys"},

	{ID: "openrouter", Name: "OpenRouter", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://openrouter.ai/api/v1",
		KeyEnv:  []string{"OPENROUTER_API_KEY"}, BaseEnv: "OPENROUTER_BASE_URL",
		Note: "one key, every model", KeyHint: "openrouter.ai/keys"},

	{ID: "nous", Name: "Nous Portal", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://inference-api.nousresearch.com/v1",
		KeyEnv:  []string{"NOUS_API_KEY"},
		Note:    "sk- API key from the Portal", KeyHint: "portal.nousresearch.com"},

	{ID: "chatgpt", Name: "ChatGPT / Codex", Auth: AuthDeviceCode, Flavor: FlavorOpenAI,
		BaseURL: "https://chatgpt.com/backend-api/codex",
		Note:    "OAuth — sign in with your OpenAI account", KeyHint: "auth.openai.com",
		OAuth: &CodexBrowserFlow{
			Issuer:   "https://auth.openai.com",
			ClientID: "app_EMoamEEZ73f0CkXaXp7hrann",
		}},

	{ID: "copilot", Name: "GitHub Copilot", Auth: AuthDeviceCode, Flavor: FlavorOpenAI,
		BaseURL: "https://api.githubcopilot.com",
		Note:    "OAuth — sign in with GitHub", KeyHint: "github.com/login/device",
		CopilotExchange: true,
		OAuth: &DeviceFlow{
			DeviceAuthURL: "https://github.com/login/device/code",
			TokenURL:      "https://github.com/login/oauth/access_token",
			ClientID:      "Iv1.b507a08c87ecfe98",
			Scope:         "read:user",
		}},

	{ID: "zai", Name: "Z.AI / GLM", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.z.ai/api/paas/v4",
		KeyEnv:  []string{"GLM_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY"}, BaseEnv: "GLM_BASE_URL"},

	{ID: "deepseek", Name: "DeepSeek", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.deepseek.com/v1",
		KeyEnv:  []string{"DEEPSEEK_API_KEY"}, BaseEnv: "DEEPSEEK_BASE_URL"},

	{ID: "xai", Name: "xAI Grok", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.x.ai/v1",
		KeyEnv:  []string{"XAI_API_KEY"}, BaseEnv: "XAI_BASE_URL"},

	{ID: "gemini", Name: "Google AI Studio", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		KeyEnv:  []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}, BaseEnv: "GEMINI_BASE_URL",
		KeyHint: "aistudio.google.com/apikey"},

	{ID: "groq", Name: "Groq", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.groq.com/openai/v1",
		KeyEnv:  []string{"GROQ_API_KEY"}, BaseEnv: "GROQ_BASE_URL", Note: "very fast"},

	{ID: "kimi", Name: "Kimi / Moonshot", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.moonshot.ai/v1",
		KeyEnv:  []string{"KIMI_API_KEY", "MOONSHOT_API_KEY"}, BaseEnv: "KIMI_BASE_URL"},

	{ID: "kimi-cn", Name: "Kimi / Moonshot (China)", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.moonshot.cn/v1", KeyEnv: []string{"KIMI_CN_API_KEY"}},

	{ID: "alibaba", Name: "Qwen Cloud", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
		KeyEnv:  []string{"DASHSCOPE_API_KEY"}, BaseEnv: "DASHSCOPE_BASE_URL"},

	{ID: "minimax", Name: "MiniMax", Auth: AuthAPIKey, Flavor: FlavorAnthropic,
		BaseURL: "https://api.minimax.io/anthropic",
		KeyEnv:  []string{"MINIMAX_API_KEY"}, BaseEnv: "MINIMAX_BASE_URL"},

	{ID: "mistral", Name: "Mistral", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.mistral.ai/v1",
		KeyEnv:  []string{"MISTRAL_API_KEY"}, BaseEnv: "MISTRAL_BASE_URL"},

	{ID: "cerebras", Name: "Cerebras", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.cerebras.ai/v1",
		KeyEnv:  []string{"CEREBRAS_API_KEY"}, BaseEnv: "CEREBRAS_BASE_URL"},

	{ID: "together", Name: "Together AI", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.together.xyz/v1",
		KeyEnv:  []string{"TOGETHER_API_KEY"}, BaseEnv: "TOGETHER_BASE_URL"},

	{ID: "fireworks", Name: "Fireworks AI", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.fireworks.ai/inference/v1",
		KeyEnv:  []string{"FIREWORKS_API_KEY"}, BaseEnv: "FIREWORKS_BASE_URL"},

	{ID: "nvidia", Name: "NVIDIA NIM", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://integrate.api.nvidia.com/v1",
		KeyEnv:  []string{"NVIDIA_API_KEY"}, BaseEnv: "NVIDIA_BASE_URL"},

	{ID: "huggingface", Name: "Hugging Face", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://router.huggingface.co/v1",
		KeyEnv:  []string{"HF_TOKEN"}, BaseEnv: "HF_BASE_URL"},

	{ID: "novita", Name: "NovitaAI", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.novita.ai/openai/v1",
		KeyEnv:  []string{"NOVITA_API_KEY"}, BaseEnv: "NOVITA_BASE_URL"},

	{ID: "longcat", Name: "LongCat", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.longcat.chat/openai/v1",
		KeyEnv:  []string{"LONGCAT_API_KEY"}, BaseEnv: "LONGCAT_BASE_URL"},

	{ID: "opencode-zen", Name: "OpenCode Zen", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://opencode.ai/zen/v1",
		KeyEnv:  []string{"OPENCODE_ZEN_API_KEY"}, BaseEnv: "OPENCODE_ZEN_BASE_URL"},

	{ID: "opencode-go", Name: "OpenCode Go", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://opencode.ai/zen/go/v1",
		KeyEnv:  []string{"OPENCODE_GO_API_KEY", "OPENCODE_ZEN_API_KEY"}, BaseEnv: "OPENCODE_GO_BASE_URL",
		Note: "open-weight tier of Zen", KeyHint: "opencode.ai/auth"},

	{ID: "tokenrouter", Name: "TokenRouter", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.tokenrouter.com/v1",
		KeyEnv:  []string{"TOKENROUTER_API_KEY"}, BaseEnv: "TOKENROUTER_BASE_URL",
		Note: "one key, 300+ models", KeyHint: "tokenrouter.com"},

	{ID: "arcee", Name: "Arcee AI", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.arcee.ai/api/v1",
		KeyEnv:  []string{"ARCEEAI_API_KEY"}, BaseEnv: "ARCEE_BASE_URL"},

	{ID: "gmi", Name: "GMI Cloud", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.gmi-serving.com/v1",
		KeyEnv:  []string{"GMI_API_KEY"}, BaseEnv: "GMI_BASE_URL"},

	{ID: "stepfun", Name: "StepFun", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.stepfun.ai/step_plan/v1",
		KeyEnv:  []string{"STEPFUN_API_KEY"}, BaseEnv: "STEPFUN_BASE_URL"},

	{ID: "xiaomi", Name: "Xiaomi MiMo", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.xiaomimimo.com/v1",
		KeyEnv:  []string{"XIAOMI_API_KEY"}, BaseEnv: "XIAOMI_BASE_URL"},

	{ID: "ollama-cloud", Name: "Ollama Cloud", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://ollama.com/v1",
		KeyEnv:  []string{"OLLAMA_API_KEY"}, BaseEnv: "OLLAMA_BASE_URL"},

	{ID: "perplexity", Name: "Perplexity", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.perplexity.ai",
		KeyEnv:  []string{"PERPLEXITY_API_KEY"}, BaseEnv: "PERPLEXITY_BASE_URL",
		KeyHint: "perplexity.ai/settings/api"},

	{ID: "cohere", Name: "Cohere", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.cohere.ai/compatibility/v1",
		KeyEnv:  []string{"COHERE_API_KEY"}, BaseEnv: "COHERE_BASE_URL",
		KeyHint: "dashboard.cohere.com/api-keys"},

	{ID: "inference", Name: "Inference.Net", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.inference.net/v1",
		KeyEnv:  []string{"INFERENCE_API_KEY"}, BaseEnv: "INFERENCE_BASE_URL"},

	{ID: "lambda", Name: "Lambda Cloud", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://api.lambda.ai/v1",
		KeyEnv:  []string{"LAMBDA_API_KEY"}, BaseEnv: "LAMBDA_BASE_URL"},

	{ID: "azure", Name: "Azure OpenAI", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "",
		KeyEnv:  []string{"AZURE_OPENAI_API_KEY", "AZURE_API_KEY"}, BaseEnv: "AZURE_OPENAI_BASE_URL",
		Note: "set base URL to your resource endpoint", KeyHint: "portal.azure.com → OpenAI resource"},

	{ID: "google-vertex", Name: "Google Vertex AI", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://us-central1-aiplatform.googleapis.com/v1beta1",
		KeyEnv:  []string{"VERTEX_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS"}, BaseEnv: "VERTEX_BASE_URL",
		Note: "GCP service account or API key", KeyHint: "console.cloud.google.com/vertex-ai"},

	{ID: "amazon-bedrock", Name: "Amazon Bedrock", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
		KeyEnv:  []string{"AWS_BEARER_TOKEN_BEDROCK", "AWS_ACCESS_KEY_ID"}, BaseEnv: "AWS_BEDROCK_BASE_URL",
		Note: "AWS credentials or bearer token", KeyHint: "console.aws.amazon.com/bedrock"},

	{ID: "gitlab", Name: "GitLab Duo", Auth: AuthAPIKey, Flavor: FlavorOpenAI,
		BaseURL: "https://gitlab.com/api/v4/ai/gateway",
		KeyEnv:  []string{"GITLAB_TOKEN", "GITLAB_PERSONAL_ACCESS_TOKEN"}, BaseEnv: "GITLAB_BASE_URL",
		Note: "GitLab personal access token", KeyHint: "gitlab.com/-/user_settings/personal_access_tokens"},

	{ID: "ollama", Name: "Ollama (local)", Auth: AuthNone, Flavor: FlavorOpenAI,
		BaseURL: "http://127.0.0.1:11434/v1", BaseEnv: "OLLAMA_HOST",
		Note: "no key needed"},

	{ID: "lmstudio", Name: "LM Studio (local)", Auth: AuthNone, Flavor: FlavorOpenAI,
		BaseURL: "http://127.0.0.1:1234/v1",
		KeyEnv:  []string{"LM_API_KEY"}, BaseEnv: "LM_BASE_URL", Note: "no key needed"},
}

// appendUnique appends generated entries while preserving the first entry for
// each provider id. Curated entries therefore remain authoritative.
func appendUnique(existing, additions []Entry) []Entry {
	seen := make(map[string]struct{}, len(existing)+len(additions))
	out := append([]Entry(nil), existing...)
	for _, entry := range existing {
		seen[entry.ID] = struct{}{}
	}
	for _, entry := range additions {
		if _, ok := seen[entry.ID]; ok {
			continue
		}
		seen[entry.ID] = struct{}{}
		out = append(out, entry)
	}
	return out
}

// Get returns the registry entry for an id.
func Get(id string) (Entry, bool) {
	for _, e := range Registry {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// EnvKey returns the first non-empty environment key for an entry.
func (e Entry) EnvKey() (string, string) {
	for _, v := range e.KeyEnv {
		if val := strings.TrimSpace(os.Getenv(v)); val != "" {
			return val, v
		}
	}
	return "", ""
}

// EnvBaseURL returns the base-url override from the environment, if set.
func (e Entry) EnvBaseURL() string {
	if e.BaseEnv == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(e.BaseEnv))
}

// NeedsKey reports whether the provider requires credentials.
func (e Entry) NeedsKey() bool { return e.Auth == AuthAPIKey }

// IDs returns every registry id, sorted.
func IDs() []string {
	out := make([]string, 0, len(Registry))
	for _, e := range Registry {
		out = append(out, e.ID)
	}
	sort.Strings(out)
	return out
}
