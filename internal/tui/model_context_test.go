package tui

import (
	"testing"

	"rick/internal/config"
	"rick/internal/provider"
)

func TestBetterContextCandidatePrefersAPIMetadata(t *testing.T) {
	value, source := betterContextCandidate(1_000_000, provider.ContextSourceInferred, 128_000, provider.ContextSourceAPI)
	if value != 128_000 || source != provider.ContextSourceAPI {
		t.Fatalf("candidate = %d/%q, want API value", value, source)
	}

	value, source = betterContextCandidate(128_000, provider.ContextSourceAPI, 1_000_000, provider.ContextSourceInferred)
	if value != 128_000 || source != provider.ContextSourceAPI {
		t.Fatalf("inferred candidate replaced API value: %d/%q", value, source)
	}
}

func TestBetterContextCandidateUsesLargerValueWithinSameSource(t *testing.T) {
	value, source := betterContextCandidate(128_000, provider.ContextSourceAPI, 256_000, provider.ContextSourceAPI)
	if value != 256_000 || source != provider.ContextSourceAPI {
		t.Fatalf("same-source candidate = %d/%q", value, source)
	}
}

// TestBetterContextCandidatePrefersConfigured verifies the patch hierarchy:
// a user-configured context_windows value beats API metadata, catalogs, and
// the gpt-5 family inference (400k).
func TestBetterContextCandidatePrefersConfigured(t *testing.T) {
	// configured 272k overrides inferred 400k (the gpt-5 fallback).
	value, source := betterContextCandidate(400_000, provider.ContextSourceInferred, 272_000, provider.ContextSourceConfigured)
	if value != 272_000 || source != provider.ContextSourceConfigured {
		t.Fatalf("configured candidate = %d/%q, want 272000/configured", value, source)
	}

	// configured beats API metadata from the /v1/models probe.
	value, source = betterContextCandidate(272_000, provider.ContextSourceAPI, 250_000, provider.ContextSourceConfigured)
	if value != 250_000 || source != provider.ContextSourceConfigured {
		t.Fatalf("configured over API = %d/%q, want 250000/configured", value, source)
	}

	// configured beats the builtin/catalog source.
	value, source = betterContextCandidate(128_000, provider.ContextSourceCatalog, 272_000, provider.ContextSourceConfigured)
	if value != 272_000 || source != provider.ContextSourceConfigured {
		t.Fatalf("configured over catalog = %d/%q", value, source)
	}
}

// TestUpdateContextWindowClassifiesStoredValueAsConfigured proves a
// context_windows entry with no persisted source is treated as configured and
// overrides the gpt-5 400k id inference.
func TestUpdateContextWindowClassifiesStoredValueAsConfigured(t *testing.T) {
	m := newModelChoiceTestModel()
	// No ContextSources persisted: the stored window is a user constraint.
	m.creds = &config.Credentials{
		Providers: map[string]config.Credential{
			"cx": {ContextWindows: map[string]int{"gpt-5.6-terra": 272_000}},
		},
	}
	m.SetModelID("cx/gpt-5.6-terra")
	if m.ctxWindow != 272_000 {
		t.Fatalf("ctxWindow = %d, want 272000 (configured overrides gpt-5 400k)", m.ctxWindow)
	}
}
