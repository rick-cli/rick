package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rick/internal/config"
	"rick/internal/provider"
	"rick/internal/provider/openai"
)

// testModelsServer speaks the OpenAI /models shape and requires Bearer auth.
func testModelsServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-test-1234" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"message":"invalid key"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[
			{"id":"my-real-model","object":"model","context_length":128000},
			{"id":"my-other-model","object":"model","context_length":32768}
		]}`)
	}))
}

// TestRefreshMissingModelsProbesEmptyProvider locks the lazy reload: a provider
// added through the desktop "add custom provider" form writes the key without
// probing /models, so its credential has an empty model list. The first models
// query must probe the endpoint and persist the real list instead of falling
// back to placeholder models.
func TestRefreshMissingModelsProbesEmptyProvider(t *testing.T) {
	srv := testModelsServer(t)
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("RICK_HOME", home)

	creds, err := config.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	creds.Set("my-gateway", config.Credential{
		Type:    "openai",
		APIKey:  "sk-test-1234",
		BaseURL: srv.URL + "/v1",
	})
	if err := creds.Save(); err != nil {
		t.Fatal(err)
	}

	// Reload from disk so the in-memory state mirrors a fresh daemon.
	creds2, err := config.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(creds2.Providers["my-gateway"].Models); got != 0 {
		t.Fatalf("precondition: expected 0 models, got %d", got)
	}

	s := &server{creds: creds2}
	s.refreshMissingModels()

	cred, _ := s.creds.Get("my-gateway")
	if len(cred.Models) != 2 {
		t.Fatalf("lazy reload did not fetch models: got %v", cred.Models)
	}
	gotSet := map[string]bool{cred.Models[0]: true, cred.Models[1]: true}
	if !gotSet["my-real-model"] || !gotSet["my-other-model"] {
		t.Fatalf("unexpected models %v", cred.Models)
	}
	if cred.ContextWindows["my-real-model"] != 128000 {
		t.Fatalf("context window not persisted: %v", cred.ContextWindows)
	}

	// Persisted to disk too.
	reloaded, err := config.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Providers["my-gateway"].Models
	if len(got) != 2 {
		t.Fatalf("models not persisted to disk: %v", got)
	}
}

// TestRefreshMissingModelsSkipsFetchedProvider ensures the lazy reload never
// re-probes (and never clobbers) a provider that already has a fetched list.
func TestRefreshMissingModelsSkipsFetchedProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("RICK_HOME", home)
	creds, err := config.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	creds.Set("already-done", config.Credential{
		Type:    "openai",
		APIKey:  "sk-anything",
		BaseURL: "https://example.invalid/v1", // would fail if probed
		Models:  []string{"existing-model"},
	})
	if err := creds.Save(); err != nil {
		t.Fatal(err)
	}
	creds2, _ := config.LoadCredentials()

	s := &server{creds: creds2}
	s.refreshMissingModels() // must not touch the already-fetched provider

	cred, _ := s.creds.Get("already-done")
	if len(cred.Models) != 1 || cred.Models[0] != "existing-model" {
		t.Fatalf("already-fetched provider was clobbered: %v", cred.Models)
	}
}

// TestHandleModelsLazyReloadsOnce verifies the models endpoint runs the lazy
// reload exactly once (sync.Once) even when asked repeatedly.
func TestHandleModelsLazyReloadsOnce(t *testing.T) {
	srv := testModelsServer(t)
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("RICK_HOME", home)
	creds, _ := config.LoadCredentials()
	creds.Set("my-gateway", config.Credential{
		Type: "openai", APIKey: "sk-test-1234", BaseURL: srv.URL + "/v1",
	})
	_ = creds.Save()

	s := &server{creds: creds, provs: map[string]provider.Provider{
		"my-gateway": openai.New("my-gateway", "sk-test-1234", srv.URL+"/v1"),
	}}

	modelCount := func() int {
		var buf bytes.Buffer
		s.handleModels(newWriter(&buf))
		var resp Response
		if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		var entries []map[string]any
		if err := json.Unmarshal(resp.Data, &entries); err != nil {
			t.Fatalf("decode entries: %v", err)
		}
		return len(entries)
	}

	if n := modelCount(); n != 2 {
		t.Fatalf("first models query: expected 2 models, got %d", n)
	}
	if n := modelCount(); n != 2 {
		t.Fatalf("second models query: expected 2 models, got %d", n)
	}
}
