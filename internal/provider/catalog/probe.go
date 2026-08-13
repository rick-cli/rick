package catalog

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"rick/internal/provider"
)

// Model is one model advertised by a live endpoint.
type Model struct {
	ID                           string
	Name                         string
	Context                      int
	ContextSource                provider.ContextSource
	Free                         bool // zero-cost / free-tier model (id ends with :free)
	SupportsImages               bool
	ModalitiesKnown              bool
	CapabilitiesKnown            bool
	ChatCapable                  bool
	ReasoningEfforts             []provider.ReasoningEffort
	ReasoningEffortsKnown        bool
	ReasoningEffortsAll          bool
	ReasoningDefault             provider.ReasoningEffort
	ReasoningDefaultEnabled      bool
	ReasoningDefaultEnabledKnown bool
	ReasoningMandatory           bool
	ReasoningSupportsMaxTokens   bool
	ReasoningKnown               bool
}

type reasoningMetadata struct {
	SupportedEfforts  json.RawMessage `json:"supported_efforts"`
	DefaultEffort     string          `json:"default_effort"`
	DefaultEnabled    *bool           `json:"default_enabled"`
	Mandatory         bool            `json:"mandatory"`
	SupportsMaxTokens bool            `json:"supports_max_tokens"`
}

// attemptRec records one probe attempt so the best error can be chosen.
type attemptRec struct {
	base, flavor string
	err          error
}

// ProbeResult is what Probe learned about an endpoint.
type ProbeResult struct {
	Flavor  string // openai | anthropic
	BaseURL string // normalised, possibly corrected
	Models  []Model
	// Partial is set when the protocol was identified but the model list
	// could not be read (many gateways do not expose /models). The provider
	// is still usable — the user just types a model id manually.
	Partial bool
	Err     error
}

// Probe determines whether an endpoint speaks the OpenAI or Anthropic wire
// format and, when possible, fetches its model list.
//
// Real endpoints vary far more than the specs suggest, so this proceeds in
// stages and only gives up when every one fails:
//
//  1. Normalise what the user pasted (strip /models, /chat/completions,
//     /v1/messages, query strings, trailing slashes).
//  2. Try GET {base}/models for each candidate base and each flavor.
//  3. If no candidate served a model list, identify the protocol from the
//     auth challenge instead (Anthropic rejects a bearer token but accepts
//     x-api-key, and vice versa) and return a Partial result.
func Probe(ctx context.Context, baseURL, apiKey string) ProbeResult {
	return ProbeWithAccount(ctx, baseURL, apiKey, "")
}

// ProbeWithAccount is Probe for backends that also need a ChatGPT account id
// header (ChatGPT-Account-ID). Other endpoints ignore the extra argument.
func ProbeWithAccount(ctx context.Context, baseURL, apiKey, accountID string) ProbeResult {
	apiKey = CleanSecret(apiKey)
	bases := candidateBases(baseURL)
	if len(bases) == 0 {
		return ProbeResult{Err: fmt.Errorf("base URL is empty")}
	}

	var attempts []attemptRec

	for _, base := range bases {
		for _, flavor := range flavorOrder(base) {
			models, err := listModels(ctx, base, apiKey, flavor, accountID)
			if err == nil {
				return ProbeResult{Flavor: flavor, BaseURL: base, Models: models}
			}
			attempts = append(attempts, attemptRec{base, flavor, err})
			if isMissingPath(err) {
				break // the path itself is absent; the protocol is irrelevant
			}
		}
	}

	// No model list anywhere. Identify the protocol from how the server
	// reacts to each auth scheme, so the provider is still usable.
	for _, base := range bases {
		if flavor, ok := detectByAuth(ctx, base, apiKey); ok {
			return ProbeResult{Flavor: flavor, BaseURL: base, Partial: true}
		}
	}

	// Every candidate 404'd: the host is up but serves no API at these paths.
	if allMissing(attempts) {
		return ProbeResult{Err: fmt.Errorf(
			"%s: no API found at that URL — check the path (many gateways need /v1, /openai/v1 or /anthropic/v1)",
			hostOf(bases[0]))}
	}

	// Everything failed: report the most informative error. An auth failure
	// from the flavor the URL suggests beats a generic 404 from a guess.
	best := attempts[0].err
	bestScore := -1
	for _, a := range attempts {
		score := 0
		if isAuthErr(a.err) {
			score += 2 // the endpoint exists, the credential is wrong
		}
		if a.flavor == flavorOrder(a.base)[0] {
			score++ // matches the URL's own hint
		}
		if score > bestScore {
			bestScore, best = score, a.err
		}
	}
	return ProbeResult{Err: best}
}

// candidateBases turns whatever the user pasted into base URLs worth trying.
func candidateBases(raw string) []string {
	s := strings.TrimSpace(raw)
	s = strings.ReplaceAll(s, "\x00", "")
	if s == "" {
		return nil
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "https://" + s
	}
	s = strings.TrimRight(s, "/")

	// Users routinely paste a full endpoint rather than the base.
	for _, suffix := range []string{
		"/chat/completions", "/v1/messages", "/messages",
		"/completions", "/responses", "/models",
	} {
		if strings.HasSuffix(strings.ToLower(s), suffix) {
			s = s[:len(s)-len(suffix)]
			s = strings.TrimRight(s, "/")
			break
		}
	}
	if s == "" {
		return nil
	}

	out := []string{s}
	add := func(u string) {
		u = strings.TrimRight(u, "/")
		for _, e := range out {
			if e == u {
				return
			}
		}
		out = append(out, u)
	}
	// A missing version segment is the most common mistake.
	if !hasVersionSuffix(s) {
		add(s + "/v1")
	}
	// Gateways that host several protocols behind one host expose them under
	// a prefix (api.example.com/openai/v1, /anthropic/v1). If the user gave
	// only the host, the bare paths 404 and the prefixed ones work.
	if !strings.Contains(strings.ToLower(s), "/openai") &&
		!strings.Contains(strings.ToLower(s), "/anthropic") {
		root := strings.TrimSuffix(s, "/v1")
		root = strings.TrimRight(root, "/")
		add(root + "/openai/v1")
		add(root + "/anthropic/v1")
	}
	return out
}

func hasVersionSuffix(u string) bool {
	l := strings.ToLower(u)
	for _, suf := range []string{"/v1", "/v1beta", "/v4", "/anthropic", "/openai", "/api/v1"} {
		if strings.HasSuffix(l, suf) {
			return true
		}
	}
	return strings.Contains(l, "/v1/") || strings.Contains(l, "compatible-mode")
}

// flavorOrder puts the likelier protocol first based on URL shape.
func flavorOrder(u string) []string {
	l := strings.ToLower(u)
	if strings.Contains(l, "anthropic") || strings.Contains(l, "claude") {
		return []string{FlavorAnthropic, FlavorOpenAI}
	}
	return []string{FlavorOpenAI, FlavorAnthropic}
}

func isAuthErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "http 401") || strings.Contains(s, "http 403")
}

// isMissingPath reports whether the route simply does not exist there.
func isMissingPath(err error) bool {
	s := err.Error()
	return strings.Contains(s, "http 404") || strings.Contains(s, "http 405")
}

func allMissing(attempts []attemptRec) bool {
	if len(attempts) == 0 {
		return false
	}
	for _, a := range attempts {
		if !isMissingPath(a.err) {
			return false
		}
	}
	return true
}

func authHeaders(req *http.Request, apiKey, flavor string) {
	if apiKey == "" {
		return
	}
	if flavor == FlavorAnthropic {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		return
	}
	req.Header.Set("authorization", "Bearer "+apiKey)
}

func listModels(ctx context.Context, base, apiKey, flavor, accountID string) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	authHeaders(req, apiKey, flavor)
	req.Header.Set("accept", "application/json")
	// The ChatGPT Codex backend requires the OAuth bearer token plus the
	// account id; originator identifies the client for routing/analytics.
	if strings.Contains(base, "chatgpt.com/backend-api/codex") {
		// The ChatGPT Codex backend requires client_version on /models
		// (Pydantic "Field required" 400 without it).
		q := req.URL.Query()
		q.Set("client_version", "0.0.0")
		req.URL.RawQuery = q.Encode()
		req.Header.Set("originator", "rick-cli")
		req.Header.Set("User-Agent", "rick-cli")
		// Raw map assignment preserves the exact casing the backend expects
		// (Header.Set would emit "Oai-Product-Sku").
		req.Header["OAI-Product-Sku"] = []string{"codex"}
		if accountID != "" {
			// Preserve the exact casing the backend expects. The header is
			// "ChatGPT-Account-ID" (capital "ID"); Go's Header.Set would
			// canonicalize it to "Chatgpt-Account-Id", so assign the raw map key.
			req.Header["ChatGPT-Account-ID"] = []string{accountID}
		}
	}

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, describeTransportErr(base, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		// A 404 page is HTML; echoing the markup tells the user nothing.
		if looksLikeHTML(body) {
			return nil, fmt.Errorf("http %d: no API at %s%s", resp.StatusCode, hostOf(base), pathOf(base)+"/models")
		}
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, snippet(body))
	}

	models, shape, err := ParseModels(body)
	if err != nil {
		return nil, err
	}
	// The payload tells us which protocol we are really talking to. Reject a
	// mismatch so the caller keeps looking instead of saving a wrong flavor.
	switch {
	case flavor == FlavorAnthropic && shape == shapeOpenAI:
		return nil, fmt.Errorf("endpoint is openai-shaped, not anthropic")
	case flavor == FlavorOpenAI && shape == shapeAnthropic:
		return nil, fmt.Errorf("endpoint is anthropic-shaped, not openai")
	}
	return models, nil
}

type payloadShape int

const (
	shapeUnknown payloadShape = iota
	shapeOpenAI
	shapeAnthropic
)

// ParseModels accepts every model-list shape seen in the wild:
//
//	{"object":"list","data":[{"id":…}]}   OpenAI
//	{"data":[{"type":"model","id":…}]}    Anthropic
//	{"models":[{"id"|"name":…}]}          assorted gateways, Ollama
//	[{"id":…}]                            bare arrays
func ParseModels(body []byte) ([]Model, payloadShape, error) {
	type rawModel struct {
		ID            string          `json:"id"`
		Slug          string          `json:"slug"`
		Name          string          `json:"name"`
		Model         string          `json:"model"`
		DisplayName   string          `json:"display_name"`
		Type          string          `json:"type"`
		Object        string          `json:"object"`
		ContextLength json.RawMessage `json:"context_length"`
		ContextWindow json.RawMessage `json:"context_window"`
		MaxTokens     json.RawMessage `json:"max_input_tokens"`
		MaxContext    json.RawMessage `json:"max_context_tokens"`
		MaxModelLen   json.RawMessage `json:"max_model_len"`
		InputLimit    json.RawMessage `json:"input_token_limit"`
		TopProvider   struct {
			ContextLength  json.RawMessage `json:"context_length"`
			MaxInputTokens json.RawMessage `json:"max_input_tokens"`
		} `json:"top_provider"`
		Capabilities struct {
			ContextWindow      json.RawMessage `json:"contextWindow"`
			ContextWindowSnake json.RawMessage `json:"context_window"`
		} `json:"capabilities"`
		Limits struct {
			Context        json.RawMessage `json:"context"`
			MaxInputTokens json.RawMessage `json:"max_input_tokens"`
		} `json:"limits"`
		Limit struct {
			Context        json.RawMessage `json:"context"`
			MaxInputTokens json.RawMessage `json:"max_input_tokens"`
		} `json:"limit"`
		Architecture struct {
			InputModalities  []string `json:"input_modalities"`
			OutputModalities []string `json:"output_modalities"`
			Modality         string   `json:"modality"`
		} `json:"architecture"`
		InputModalities  []string           `json:"input_modalities"`
		OutputModalities []string           `json:"output_modalities"`
		Modality         string             `json:"modality"`
		Task             string             `json:"task"`
		Reasoning        *reasoningMetadata `json:"reasoning"`
	}

	var envelope struct {
		Object string     `json:"object"`
		Data   []rawModel `json:"data"`
		Models []rawModel `json:"models"`
	}
	raws := []rawModel(nil)

	if err := json.Unmarshal(body, &envelope); err == nil {
		switch {
		case len(envelope.Data) > 0:
			raws = envelope.Data
		case len(envelope.Models) > 0:
			raws = envelope.Models
		}
	}
	if raws == nil {
		var bare []rawModel
		if err := json.Unmarshal(body, &bare); err == nil && len(bare) > 0 {
			raws = bare
		}
	}
	if len(raws) == 0 {
		return nil, shapeUnknown, fmt.Errorf("no models in response: %s", snippet(body))
	}

	// Shape detection: Anthropic tags entries type:"model" and has no
	// "object" field; OpenAI uses object:"model" / object:"list".
	shape := shapeUnknown
	first := raws[0]
	switch {
	case first.Object == "model" || envelope.Object == "list":
		shape = shapeOpenAI
	case first.Type == "model":
		shape = shapeAnthropic
	}

	out := make([]Model, 0, len(raws))
	for _, m := range raws {
		id := FirstNonEmpty(m.ID, m.Slug, m.Model, m.Name)
		if id == "" {
			continue
		}
		name := FirstNonEmpty(m.DisplayName, m.Name, id)
		ctxLen := firstPositiveModelInt(
			m.ContextLength, m.ContextWindow, m.TopProvider.ContextLength,
			m.Capabilities.ContextWindow, m.Capabilities.ContextWindowSnake,
			m.MaxContext, m.MaxModelLen, m.InputLimit, m.MaxTokens,
			m.TopProvider.MaxInputTokens, m.Limits.Context, m.Limits.MaxInputTokens,
			m.Limit.Context, m.Limit.MaxInputTokens,
		)
		contextSource := provider.ContextSourceUnknown
		if ctxLen > 0 {
			contextSource = provider.ContextSourceAPI
		}
		if ctxLen == 0 {
			ctxLen = provider.KnownContextWindow(id)
			if ctxLen > 0 {
				contextSource = provider.ContextSourceInferred
			}
		}
		free := isFreeModelID(id)
		inputModalities := append([]string(nil), m.InputModalities...)
		inputModalities = append(inputModalities, m.Architecture.InputModalities...)
		outputModalities := append([]string(nil), m.OutputModalities...)
		outputModalities = append(outputModalities, m.Architecture.OutputModalities...)
		modality := catalogFirstNonEmpty(m.Modality, m.Architecture.Modality)
		if modality != "" {
			parts := strings.SplitN(strings.ToLower(modality), "->", 2)
			if len(inputModalities) == 0 {
				inputModalities = splitModalities(parts[0])
			}
			if len(outputModalities) == 0 && len(parts) == 2 {
				outputModalities = splitModalities(parts[1])
			}
		}
		supportsImages := false
		for _, modality := range inputModalities {
			if modality == "image" {
				supportsImages = true
				break
			}
		}
		explicitTask := strings.TrimSpace(m.Task) != "" || (strings.TrimSpace(m.Type) != "" && strings.TrimSpace(m.Type) != "model")
		capabilitiesKnown := len(inputModalities) > 0 || len(outputModalities) > 0 || explicitTask
		chatCapable := modelHasTextCapability(inputModalities, outputModalities) &&
			!looksLikeNonChatTask(m.Task, m.Type, modality)
		reasoning := parseReasoning(m.Reasoning)
		out = append(out, Model{
			ID: id, Name: name, Context: ctxLen, ContextSource: contextSource, Free: free,
			SupportsImages: supportsImages, ModalitiesKnown: len(inputModalities) > 0 || len(outputModalities) > 0,
			CapabilitiesKnown: capabilitiesKnown, ChatCapable: chatCapable,
			ReasoningEfforts: reasoning.efforts, ReasoningEffortsKnown: reasoning.effortsKnown,
			ReasoningEffortsAll: reasoning.effortsAll, ReasoningDefault: reasoning.defaultEffort,
			ReasoningDefaultEnabled: reasoning.defaultEnabled, ReasoningDefaultEnabledKnown: reasoning.defaultEnabledKnown,
			ReasoningMandatory: reasoning.mandatory, ReasoningSupportsMaxTokens: reasoning.supportsMaxTokens,
			ReasoningKnown: reasoning.known,
		})
	}
	if len(out) == 0 {
		return nil, shape, fmt.Errorf("no usable model ids in response")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, shape, nil
}

func isFreeModelID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	return id == "big-pickle" || strings.HasSuffix(id, ":free") || strings.HasSuffix(id, "-free")
}

// firstPositiveModelInt returns the first usable context limit. Context fields
// are intentionally ordered from the most specific provider value to generic
// compatibility aliases; max_tokens is not accepted because it is an output
// limit, not a context window.
func firstPositiveModelInt(values ...json.RawMessage) int {
	for _, raw := range values {
		if n := parseModelInt(raw); n > 0 {
			return n
		}
	}
	return 0
}

func parseModelInt(raw json.RawMessage) int {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var quoted string
		if json.Unmarshal(raw, &quoted) != nil {
			return 0
		}
		s = strings.TrimSpace(quoted)
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		return int(n)
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
		return int(n)
	}
	return 0
}

type parsedReasoning struct {
	efforts             []provider.ReasoningEffort
	effortsKnown        bool
	effortsAll          bool
	defaultEffort       provider.ReasoningEffort
	defaultEnabled      bool
	defaultEnabledKnown bool
	mandatory           bool
	supportsMaxTokens   bool
	known               bool
}

func parseReasoning(raw *reasoningMetadata) parsedReasoning {
	if raw == nil {
		return parsedReasoning{}
	}
	parsed := parsedReasoning{
		mandatory:         raw.Mandatory,
		supportsMaxTokens: raw.SupportsMaxTokens,
		known:             true,
	}
	if raw.SupportedEfforts != nil {
		parsed.effortsKnown = true
		if string(raw.SupportedEfforts) == "null" {
			parsed.effortsAll = true
		} else {
			var values []string
			if err := json.Unmarshal(raw.SupportedEfforts, &values); err == nil {
				for _, value := range values {
					if effort, ok := provider.ParseEffort(value); ok && effort != provider.ReasoningOn {
						parsed.efforts = append(parsed.efforts, effort)
					}
				}
			}
		}
	}
	parsed.defaultEffort, _ = provider.ParseEffort(raw.DefaultEffort)
	if raw.DefaultEnabled != nil {
		parsed.defaultEnabled = *raw.DefaultEnabled
		parsed.defaultEnabledKnown = true
	}
	return parsed
}

func catalogFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func splitModalities(value string) []string {
	value = strings.NewReplacer("+", " ", ",", " ", "|", " ").Replace(value)
	return strings.Fields(strings.ToLower(value))
}

func modelHasTextCapability(input, output []string) bool {
	if len(input) > 0 && !containsModality(input, "text") {
		return false
	}
	if len(output) > 0 && !containsModality(output, "text") {
		return false
	}
	return true
}

func containsModality(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), wanted) {
			return true
		}
	}
	return false
}

func looksLikeNonChatTask(values ...string) bool {
	joined := strings.ToLower(strings.Join(values, " "))
	for _, marker := range []string{"embedding", "rerank", "moderation", "text-to-speech", "tts", "transcri", "whisper", "speech", "audio", "image-generation", "image-edit", "video", "music"} {
		if strings.Contains(joined, marker) {
			return true
		}
	}
	return false
}

// FirstNonEmpty returns the first value that is not blank.
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// detectByAuth identifies the protocol when /models is unavailable, by making
// a deliberately tiny chat request and reading how the server complains.
//
// A 401/403 means the wrong auth scheme; anything else (400 for a bad body,
// 200, 404 on a different path) means the scheme was accepted, which
// identifies the protocol.
func detectByAuth(ctx context.Context, base, apiKey string) (string, bool) {
	apiKey = CleanSecret(apiKey)
	for _, flavor := range flavorOrder(base) {
		var path string
		var body []byte
		if flavor == FlavorAnthropic {
			path = "/messages"
			body = []byte(`{"model":"probe","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
		} else {
			path = "/chat/completions"
			body = []byte(`{"model":"probe","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
		}

		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, base+path, bytes.NewReader(body))
		if err != nil {
			cancel()
			continue
		}
		authHeaders(req, apiKey, flavor)
		req.Header.Set("content-type", "application/json")

		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			cancel()
			continue
		}
		code := resp.StatusCode
		resp.Body.Close()
		cancel()

		switch {
		case code == 401 || code == 403:
			continue // wrong auth scheme for this endpoint — try the other
		case code == 404 || code == 405:
			continue // endpoint does not exist in this protocol
		default:
			// 200, 400 (our fake model), 422, 429 — the server understood us.
			return flavor, true
		}
	}
	return "", false
}

// errWSAConnRefused is the Windows socket error for a refused connection.
// Naming it avoids a build-tagged file for a single constant.
const errWSAConnRefused = syscall.Errno(10061)

// describeTransportErr turns Go's low-level failures into advice. The header
// error in particular names "Authorization" but not the real cause, which is
// almost always a newline smuggled in by a paste.
func describeTransportErr(base string, err error) error {
	host := hostOf(base)
	s := err.Error()

	// Match structurally where possible: Windows localises socket errors, so
	// string matching on "connection refused" fails on a German machine.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Errorf("%s: host not found — check the URL", host)
	}
	// Windows reports WSAECONNREFUSED (10061), not the Unix ECONNREFUSED
	// (111), so errors.Is against the Unix constant alone never matches.
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, errWSAConnRefused) {
		return fmt.Errorf("%s: connection refused — is the server running?", host)
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return fmt.Errorf("%s: timed out", host)
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return fmt.Errorf("%s: TLS certificate problem — %v", host, err)
	}

	switch {
	case strings.Contains(s, "invalid header field value"):
		return fmt.Errorf("%s: the API key contains an invalid character "+
			"(usually a line break from pasting) — re-copy the key without a trailing newline", host)
	case strings.Contains(s, "certificate"):
		return fmt.Errorf("%s: TLS certificate problem — %v", host, err)
	}
	return fmt.Errorf("%s: %w", host, err)
}

// looksLikeHTML reports whether a response body is a web page rather than an
// API payload — the signature of hitting a reverse proxy's error page.
func looksLikeHTML(b []byte) bool {
	s := strings.ToLower(strings.TrimSpace(string(b)))
	return strings.HasPrefix(s, "<!doctype html") || strings.HasPrefix(s, "<html")
}

func pathOf(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[i:]
	}
	return ""
}

func hostOf(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i > 0 {
		return s[:i]
	}
	return s
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > 160 {
		s = string([]rune(s)[:160]) + "…"
	}
	if s == "" {
		s = "(empty response)"
	}
	return s
}
