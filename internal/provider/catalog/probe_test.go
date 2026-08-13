package catalog

import (
	"testing"

	"rick/internal/provider"
)

// TestParseModelsReadsCodexSlugs covers the ChatGPT Codex backend envelope:
// {"models":[{"slug":"gpt-5.5","display_name":...,"context_window":...}]}.
func TestParseModelsReadsCodexSlugs(t *testing.T) {
	body := []byte(`{"models":[
		{"slug":"gpt-5.5","display_name":"GPT-5.5","context_window":400000},
		{"slug":"gpt-5.4","display_name":"GPT-5.4"}
	]}`)
	models, _, err := ParseModels(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("parsed %d models, want 2", len(models))
	}
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	gpt55 := byID["gpt-5.5"]
	if gpt55.Name != "GPT-5.5" {
		t.Fatalf("gpt-5.5 name = %q, want display_name mapping", gpt55.Name)
	}
	if gpt55.Context != 400000 || gpt55.ContextSource != provider.ContextSourceAPI {
		t.Fatalf("gpt-5.5 context = %d/%v, want 400000/API", gpt55.Context, gpt55.ContextSource)
	}
	// No context_window: inferred from the model id catalog.
	gpt54 := byID["gpt-5.4"]
	if gpt54.Context != provider.KnownContextWindow("gpt-5.4") {
		t.Fatalf("gpt-5.4 context = %d, want inferred", gpt54.Context)
	}
	if gpt54.ContextSource != provider.ContextSourceInferred {
		t.Fatalf("gpt-5.4 source = %v, want inferred", gpt54.ContextSource)
	}
}

func TestParseModelsReadsReasoningCapabilities(t *testing.T) {
	body := []byte(`{"data":[
		{"id":"effort-model","reasoning":{"supported_efforts":["high","low"],"default_effort":"high","default_enabled":true,"mandatory":true}},
		{"id":"all-efforts-model","reasoning":{"supported_efforts":null}},
		{"id":"budget-model","reasoning":{"supports_max_tokens":true,"default_enabled":false}}
	]}`)
	models, _, err := ParseModels(body)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	explicit := byID["effort-model"]
	if !explicit.ReasoningKnown || !explicit.ReasoningEffortsKnown || !explicit.ReasoningMandatory || len(explicit.ReasoningEfforts) != 2 {
		t.Fatalf("explicit reasoning metadata = %+v", explicit)
	}
	if explicit.ReasoningDefault != provider.ReasoningHigh {
		t.Fatalf("explicit default = %q", explicit.ReasoningDefault)
	}
	if !explicit.ReasoningDefaultEnabled || !explicit.ReasoningDefaultEnabledKnown {
		t.Fatalf("explicit default enabled = %+v", explicit)
	}
	allEfforts := byID["all-efforts-model"]
	if !allEfforts.ReasoningEffortsAll || !allEfforts.ReasoningEffortsKnown {
		t.Fatalf("all-efforts metadata = %+v", allEfforts)
	}
	budget := byID["budget-model"]
	if !budget.ReasoningKnown || budget.ReasoningEffortsKnown || !budget.ReasoningSupportsMaxTokens || budget.ReasoningDefaultEnabled || !budget.ReasoningDefaultEnabledKnown {
		t.Fatalf("budget metadata = %+v", budget)
	}
}

func TestParseModelsReadsImageCapability(t *testing.T) {
	body := []byte(`{"object":"list","data":[
		{"id":"text-model","architecture":{"input_modalities":["text"]}},
		{"id":"vision-model:free","architecture":{"input_modalities":["text","image"]}}
	]}`)
	models, _, err := ParseModels(body)
	if err != nil {
		t.Fatal(err)
	}
	if !models[0].ModalitiesKnown || models[0].SupportsImages {
		t.Fatalf("text model capabilities: %+v", models[0])
	}
	if !models[1].ModalitiesKnown || !models[1].SupportsImages {
		t.Fatalf("vision model capabilities: %+v", models[1])
	}
}

func TestParseModelsPrefersProviderContextAndFiltersGenerationModels(t *testing.T) {
	body := []byte(`{"data":[
		{"id":"chat-model","context_length":"131072","architecture":{"input_modalities":["text"],"output_modalities":["text"]}},
		{"id":"image-edit-model","context_length":4096,"architecture":{"input_modalities":["text","image"],"output_modalities":["image"]}},
		{"id":"voice-model","task":"text-to-speech"},
		{"id":"embedding-model","type":"embedding"},
		{"id":"fallback-chat"}
	]}`)
	models, _, err := ParseModels(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 5 {
		t.Fatalf("parsed %d models, want 5", len(models))
	}
	if models[0].Context != 131072 || models[0].ContextSource != provider.ContextSourceAPI {
		t.Fatalf("provider context was not preserved: %+v", models[0])
	}
	filtered := FilterChatModels(models)
	if len(filtered) != 2 || filtered[0].ID != "chat-model" || filtered[1].ID != "fallback-chat" {
		t.Fatalf("filtered models = %+v", filtered)
	}
}

func TestParseModelsDetectsFreeModelIDs(t *testing.T) {
	body := []byte(`{"data":[
		{"id":"deepseek-v4-flash-free"},
		{"id":"big-pickle"},
		{"id":"provider/model:free"},
		{"id":"paid-model"}
	]}`)
	models, _, err := ParseModels(body)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	for _, id := range []string{"deepseek-v4-flash-free", "big-pickle", "provider/model:free"} {
		if !byID[id].Free {
			t.Errorf("%q was not detected as free", id)
		}
	}
	if byID["paid-model"].Free {
		t.Error("paid-model was detected as free")
	}
}
func TestParseModelsAcceptsNestedAndQuotedContextLimits(t *testing.T) {
	body := []byte(`{"models":[
		{"id":"nested-limit","limit":{"context":"262144"}},
		{"id":"top-provider","top_provider":{"context_length":65536}},
		{"id":"architecture-modality","architecture":{"modality":"text+image->text"}}
	]}`)
	models, _, err := ParseModels(body)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	if byID["nested-limit"].Context != 262144 || byID["top-provider"].Context != 65536 {
		t.Fatalf("nested context limits = %+v", models)
	}
	modalityModel := byID["architecture-modality"]
	if !modalityModel.SupportsImages || !modalityModel.ChatCapable {
		t.Fatalf("modality capability = %+v", modalityModel)
	}
}

// TestParseModelsReadsCapabilitiesContextWindow covers 9router-style gateways
// that report the real deployment limit under capabilities.contextWindow.
func TestParseModelsReadsCapabilitiesContextWindow(t *testing.T) {
	body := []byte(`{"models":[
		{"id":"cx/gpt-5.6-sol","capabilities":{"contextWindow":272000}},
		{"id":"cx/gpt-5.6-terra","capabilities":{"contextWindow":272000}},
		{"id":"cx/gpt-5.6-luna","capabilities":{"contextWindow":272000}},
		{"id":"cx/gpt-5.6-nova"}
	]}`)
	models, _, err := ParseModels(body)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	for _, id := range []string{"cx/gpt-5.6-sol", "cx/gpt-5.6-terra", "cx/gpt-5.6-luna"} {
		if byID[id].Context != 272000 {
			t.Fatalf("%s context = %d, want 272000", id, byID[id].Context)
		}
		if byID[id].ContextSource != provider.ContextSourceAPI {
			t.Fatalf("%s source = %q, want %q", id, byID[id].ContextSource, provider.ContextSourceAPI)
		}
	}
	// No capabilities.contextWindow means the gpt-5 family fallback applies
	// (the probe leaves inference to KnownContextWindow).
	if byID["cx/gpt-5.6-nova"].Context != provider.KnownContextWindow("cx/gpt-5.6-nova") {
		t.Fatalf("fallback for model without contextWindow = %d", byID["cx/gpt-5.6-nova"].Context)
	}
}
