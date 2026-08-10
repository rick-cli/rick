package vision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRenderFormatsEvidence(t *testing.T) {
	result := &Result{
		Summary: "A chart of model prices",
		OCR:     OCR{FullText: "Price\n0.02\n0.03"},
		Layout:  Layout{Regions: []Region{{Type: "chart", ReadingOrder: 1, Text: "scatter plot"}}},
		Semantics: Semantics{
			Scene:    "chart",
			Entities: []Entity{{Name: "deepseek", Type: "model", Evidence: "$0.028"}},
		},
		Uncertainty: []string{"axis labels partially cut off"},
	}
	out := Render(result)
	for _, want := range []string{"## Vision evidence", "Summary: A chart of model prices", "Transcription:", "deepseek (model) — $0.028", "Uncertainty"} {
		if !strings.Contains(out, want) {
			t.Fatalf("Render missing %q in:\n%s", want, out)
		}
	}
}

func TestAnalyzeRoundTrip(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "test-key" {
			t.Errorf("missing api key header")
		}
		if !strings.Contains(r.URL.Path, "/v1beta/models/gemini-3.5-flash-lite:generateContent") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		evidence, _ := json.Marshal(map[string]any{
			"summary": "ok",
			"ocr":     map[string]any{"full_text": "hi", "lines": []any{map[string]any{"text": "hi"}}},
			"layout":  map[string]any{"regions": []any{}},
			"semantics": map[string]any{
				"scene": "x", "entities": []any{}, "relations": []any{},
			},
			"uncertainty": []any{},
		})
		payload, _ := json.Marshal(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{
					"parts": []any{map[string]any{"text": string(evidence)}},
				},
			}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	c := New(Config{APIKey: "test-key", BaseURL: srv.URL})
	result, err := c.Analyze(context.Background(), "image/png", "aGVsbG8=", "")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if result.Summary != "ok" {
		t.Fatalf("summary = %q, want ok", result.Summary)
	}
	if gotBody == nil {
		t.Fatal("no request body captured")
	}
	contents, _ := gotBody["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents len = %d, want 1", len(contents))
	}
	genCfg, _ := gotBody["generationConfig"].(map[string]any)
	if genCfg["responseMimeType"] != "application/json" {
		t.Fatalf("responseMimeType = %v", genCfg["responseMimeType"])
	}
}

func TestAnalyzeHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"API key not valid"}}`))
	}))
	defer srv.Close()

	c := New(Config{APIKey: "bad", BaseURL: srv.URL})
	_, err := c.Analyze(context.Background(), "image/png", "aGVsbG8=", "")
	if err == nil {
		t.Fatal("expected error for HTTP 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error should mention status: %v", err)
	}
}

func TestAnalyzeRequiresKey(t *testing.T) {
	c := New(Config{})
	_, err := c.Analyze(context.Background(), "image/png", "aGVsbG8=", "")
	if err == nil {
		t.Fatal("expected missing-key error")
	}
	if !strings.Contains(err.Error(), "/visionapi") {
		t.Fatalf("error should suggest /visionapi: %v", err)
	}
}
