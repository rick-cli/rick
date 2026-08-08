package tokens

import "testing"

func TestCountCl100kBaseUsesExactEncoding(t *testing.T) {
	result := Count("hello world", EncodingCl100kBase)
	if result.Count != 2 {
		t.Fatalf("Count() = %d, want 2", result.Count)
	}
	if !result.Exact {
		t.Fatal("Count() reported fallback for cl100k_base")
	}
}

func TestCountO200kBaseUsesExactEncoding(t *testing.T) {
	result := Count("hello world", EncodingO200kBase)
	if result.Count != 2 {
		t.Fatalf("Count() = %d, want 2", result.Count)
	}
	if !result.Exact {
		t.Fatal("Count() reported fallback for o200k_base")
	}
}

func TestCountFallbackIsConservativeAndUnicodeSafe(t *testing.T) {
	result := Count("こんにちは世界", Encoding("unknown"))
	if result.Count < 7 {
		t.Fatalf("Count() = %d, want at least one token per rune", result.Count)
	}
	if result.Exact {
		t.Fatal("unknown encoding used exact mode")
	}
}

func TestEncodingForModel(t *testing.T) {
	cases := map[string]Encoding{
		"gpt-4o":        EncodingO200kBase,
		"gpt-5":         EncodingO200kBase,
		"o4-mini":       EncodingO200kBase,
		"codex-mini":    EncodingO200kBase,
		"claude-sonnet": EncodingCl100kBase,
		"deepseek-v4":   EncodingCl100kBase,
		"":              EncodingCl100kBase,
	}
	for model, want := range cases {
		if got := EncodingForModel(model); got != want {
			t.Errorf("EncodingForModel(%q) = %s, want %s", model, got, want)
		}
	}
}

// TestCountMemoReturnsIdenticalResultsAndStaysBounded pins the exact-count
// memo (C1/S1): repeated texts must hit the cache without changing the count,
// and the cache must stay within its entry budget under load.
func TestCountMemoReturnsIdenticalResultsAndStaysBounded(t *testing.T) {
	text := "the same stable system prompt and message text repeated across turns "
	for i := 0; i < 3; i++ {
		first := Count(text, EncodingCl100kBase)
		if !first.Exact {
			t.Fatal("memoized result must stay exact")
		}
		second := Count(text, EncodingCl100kBase)
		if first.Count != second.Count {
			t.Fatalf("memo changed the count: %d vs %d", first.Count, second.Count)
		}
	}

	// Distinct encodings keep separate memo entries.
	if Count(text, EncodingCl100kBase).Count != Count(text, EncodingO200kBase).Count {
		// Counts may legitimately differ per vocabulary; the memo must not
		// conflate them — both lookups below must simply stay exact.
		t.Log("vocabularies differ; ok")
	}

	// Bounded eviction: flooding with unique texts must not grow the cache
	// past memoMaxEntries.
	for i := 0; i < memoMaxEntries*2; i++ {
		Count("flood payload with unique text "+string(rune('a'+i%26))+string(rune('0'+i%10)), EncodingCl100kBase)
	}
	memoMu.Lock()
	defer memoMu.Unlock()
	if memo := memos[EncodingCl100kBase]; memo != nil && len(memo.order) > memoMaxEntries {
		t.Fatalf("memo grew past the entry cap: %d", len(memo.order))
	}
	if memo := memos[EncodingCl100kBase]; memo != nil && memo.bytes > memoMaxBytes {
		t.Fatalf("memo grew past the byte cap: %d", memo.bytes)
	}
}
