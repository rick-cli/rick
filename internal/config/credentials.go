package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"rick/internal/provider"
	"rick/internal/provider/catalog"
)

var credentialsFileMu sync.Mutex

// Credentials is the on-disk auth store: ~/.config/rick/auth.json.
//
// It is kept separate from rick.json so a project config can be committed to
// version control without leaking keys, and so the file can be chmod 0600.
type Credentials struct {
	Providers map[string]Credential `json:"provider"`
	// rotationIndex tracks round-robin/failover key position per provider.
	// Not persisted — reset on each session.
	rotationIndex map[string]int `json:"-"`
	mu            sync.RWMutex
}

// Credential is one saved provider login.
type Credential struct {
	Type    string   `json:"type,omitempty"` // anthropic | openai (wire flavor)
	APIKey  string   `json:"apiKey,omitempty"`
	BaseURL string   `json:"baseUrl,omitempty"`
	Label   string   `json:"label,omitempty"` // display name
	Models  []string `json:"models,omitempty"`
	// ContextWindows maps model id -> window size, as reported by the
	// endpoint or inferred at connect time.
	ContextWindows  map[string]int                    `json:"context_windows,omitempty"` // last fetched model ids
	ContextSources  map[string]provider.ContextSource `json:"context_sources,omitempty"`
	VisionModels    []string                          `json:"vision_models,omitempty"`
	ModalitiesKnown bool                              `json:"modalities_known,omitempty"`
	Default         string                            `json:"default,omitempty"` // preferred model id
	Custom          bool                              `json:"custom,omitempty"`  // user-added, not in the catalog
	Disabled        bool                              `json:"disabled,omitempty"`
	// OnlyFree filters model listings to zero-cost / :free models only.
	OnlyFree bool `json:"only_free,omitempty"`
	// APIKeys holds multiple API keys for key rotation. When APIKey is set
	// and this is empty, it's treated as a single-key config.
	APIKeys []string `json:"apiKeys,omitempty"`
	// APIKeyMode controls how multiple keys are used:
	// "single" (default) - use the first key only
	// "round-robin" - rotate through keys on each request
	// "failover" - rotate to next key on rate-limit/quota errors
	APIKeyMode string `json:"apiKeyMode,omitempty"` // single | round-robin | failover

	// RefreshToken is the OAuth refresh token for OAuth providers (ChatGPT /
	// Codex). The access token in APIKey is short-lived and must be refreshed
	// with this before expiry.
	RefreshToken string `json:"refreshToken,omitempty"`
	// TokenExpiresAt is the unix-epoch second when APIKey expires. Zero means
	// the token does not expire (API-key providers).
	TokenExpiresAt int64 `json:"tokenExpiresAt,omitempty"`
	// AccountID is the ChatGPT account id the Codex backend requires in the
	// ChatGPT-Account-ID header. Extracted from the OAuth id_token.
	AccountID string `json:"accountId,omitempty"`
}

// AuthPath is the credential file location.
func AuthPath() string { return filepath.Join(GlobalDir(), "auth.json") }

// LoadCredentials reads the auth store, returning an empty one if absent.
func LoadCredentials() (*Credentials, error) {
	c := &Credentials{Providers: map[string]Credential{}}
	data, err := os.ReadFile(AuthPath())
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(StripJSONC(data), c); err != nil {
		return c, fmt.Errorf("%s: %w", AuthPath(), err)
	}
	if c.Providers == nil {
		c.Providers = map[string]Credential{}
	}
	return c, nil
}

// Save atomically writes the auth store with owner-only permissions.
func (c *Credentials) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked()
}

func (c *Credentials) saveLocked() error {
	credentialsFileMu.Lock()
	defer credentialsFileMu.Unlock()
	if c.Providers == nil {
		c.Providers = map[string]Credential{}
	}
	dir := GlobalDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	final := AuthPath()
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	_ = os.Chmod(final, 0o600)
	return nil
}

// Set upserts a credential.
func (c *Credentials) Set(id string, cred Credential) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Providers == nil {
		c.Providers = map[string]Credential{}
	}
	c.Providers[id] = cred
}

// Get returns a copy of one credential.
func (c *Credentials) Get(id string) (Credential, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cred, ok := c.Providers[id]
	return cloneCredential(cred), ok
}

// Snapshot returns a deep copy safe for use outside the credentials lock.
func (c *Credentials) Snapshot() map[string]Credential {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]Credential, len(c.Providers))
	for id, cred := range c.Providers {
		out[id] = cloneCredential(cred)
	}
	return out
}

func cloneCredential(cred Credential) Credential {
	cred.Models = append([]string(nil), cred.Models...)
	cred.VisionModels = append([]string(nil), cred.VisionModels...)
	cred.APIKeys = append([]string(nil), cred.APIKeys...)
	if cred.ContextWindows != nil {
		cred.ContextWindows = cloneMap(cred.ContextWindows)
	}
	if cred.ContextSources != nil {
		cred.ContextSources = cloneMap(cred.ContextSources)
	}
	return cred
}

func cloneMap[T any](in map[string]T) map[string]T {
	out := make(map[string]T, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// Remove deletes a credential.
func (c *Credentials) Remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.Providers, id)
}

// SaveTokens updates a credential's access/refresh token pair and expiry and
// persists the store. It is a no-op when the provider is not stored. The new
// access token replaces APIKey so subsequent reads see the refreshed token.
func (c *Credentials) SaveTokens(id, accessToken, refreshToken string, expiresAt int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cred, ok := c.Providers[id]
	if !ok {
		return nil
	}
	if accessToken != "" {
		cred.APIKey = accessToken
	}
	if refreshToken != "" {
		cred.RefreshToken = refreshToken
	}
	cred.TokenExpiresAt = expiresAt
	c.Providers[id] = cred
	return c.saveLocked()
}

// AllKeys returns the effective list of API keys for a credential.
// When APIKey is set and APIKeys is empty, it returns [APIKey].
func (c *Credentials) AllKeys(id string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.allKeysLocked(id)
}

func (c *Credentials) allKeysLocked(id string) []string {
	cred, ok := c.Providers[id]
	if !ok {
		return nil
	}
	if len(cred.APIKeys) > 0 {
		keys := make([]string, len(cred.APIKeys))
		for i, key := range cred.APIKeys {
			keys[i] = catalog.CleanSecret(key)
		}
		return keys
	}
	if cred.APIKey != "" {
		return []string{catalog.CleanSecret(cred.APIKey)}
	}
	return nil
}

// CurrentKey returns the active key based on mode and rotation state.
func (c *Credentials) CurrentKey(id string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentKeyLocked(id)
}

func (c *Credentials) currentKeyLocked(id string) string {
	keys := c.allKeysLocked(id)
	if len(keys) == 0 {
		return ""
	}
	mode := c.Providers[id].APIKeyMode
	if mode == "" {
		mode = "single"
	}
	if mode == "round-robin" || mode == "failover" {
		idx := c.rotationIndex[id] % len(keys)
		return catalog.CleanSecret(keys[idx])
	}
	return catalog.CleanSecret(keys[0])
}

// RotateKey advances the rotation counter and returns the next key.
func (c *Credentials) RotateKey(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureRotation()
	c.rotationIndex[id]++
	return c.currentKeyLocked(id)
}

// RotateKeyAndSave advances key rotation, updates the active API key, and
// persists the complete operation under one lock. This prevents concurrent
// rate-limit retries from overwriting one another's credential state.
func (c *Credentials) RotateKeyAndSave(id string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cred, ok := c.Providers[id]
	if !ok || (cred.APIKeyMode != "failover" && cred.APIKeyMode != "round-robin") {
		return "", nil
	}
	keys := c.allKeysLocked(id)
	if len(keys) < 2 {
		return "", nil
	}
	c.ensureRotation()
	c.rotationIndex[id]++
	newKey := c.currentKeyLocked(id)
	if newKey == "" {
		return "", nil
	}
	cred.APIKey = newKey
	c.Providers[id] = cred
	return newKey, c.saveLocked()
}

// rotationIndex tracks round-robin/failover key position per provider.
func (c *Credentials) ensureRotation() {
	if c.rotationIndex == nil {
		c.rotationIndex = map[string]int{}
	}
}

// IDs lists configured provider ids, sorted.
func (c *Credentials) IDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.Providers))
	for id := range c.Providers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// MergeCredentials overlays the auth store onto a loaded config. Credentials
// never override an explicit provider block in rick.json — a project that
// pins its own endpoint keeps it.
func MergeCredentials(cfg *Config, creds *Credentials) {
	if creds == nil {
		return
	}
	providers := creds.Snapshot()
	if len(providers) == 0 {
		return
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]Provider{}
	}
	for id, cred := range providers {
		if cred.Disabled {
			continue
		}
		p := cfg.Providers[id]
		if p.Type == "" {
			p.Type = cred.Type
		}
		if p.APIKey == "" {
			p.APIKey = cred.APIKey
		}
		if p.BaseURL == "" {
			p.BaseURL = cred.BaseURL
		}
		p.RefreshToken = cred.RefreshToken
		p.TokenExpiresAt = cred.TokenExpiresAt
		p.AccountID = cred.AccountID
		cfg.Providers[id] = p
	}
}

// FirstConfiguredModel picks a sensible default model after login: the
// credential's preferred model, else its first fetched model.
func FirstConfiguredModel(creds *Credentials, id string) string {
	if creds == nil {
		return ""
	}
	cred, ok := creds.Get(id)
	if !ok {
		return ""
	}
	if cred.Default != "" {
		return id + "/" + cred.Default
	}
	if len(cred.Models) > 0 {
		return id + "/" + cred.Models[0]
	}
	return ""
}
