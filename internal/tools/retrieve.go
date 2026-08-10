package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"rick/pkg/contextbudget"
)

// RetrieveUncompressedTool lets the model pull back an original tool payload
// that was compressed or deduplicated in the live zone.
type RetrieveUncompressedTool struct {
	// Store holds the reversible payload map. It must be the same Budget the
	// agent loop uses for live-zone compression and deduplication.
	Store *contextbudget.Budget
}

// Name implements Tool.
func (RetrieveUncompressedTool) Name() string { return "retrieve_uncompressed_context" }

// ReadOnly implements Tool.
func (RetrieveUncompressedTool) ReadOnly() bool { return true }

// Description implements Tool.
func (RetrieveUncompressedTool) Description() string {
	return "Retrieve the original, uncompressed payload of a tool result that was " +
		"compressed or deduplicated. Pass the key shown in the compressed output " +
		"(a tool call id or sha256 reference), or set list:true to enumerate " +
		"currently stored keys."
}

// Schema implements Tool.
func (RetrieveUncompressedTool) Schema() map[string]any {
	return obj(map[string]any{
		"key":  strProp("Key of the stored payload (call id or sha256 reference)."),
		"list": boolProp("List stored keys instead of retrieving one."),
	})
}

type retrieveArgs struct {
	Key  string `json:"key"`
	List bool   `json:"list"`
}

// Run implements Tool.
func (t RetrieveUncompressedTool) Run(_ context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a retrieveArgs
	if err := RepairDecode(in, &a, t.Schema(), tc.Repair); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	if t.Store == nil {
		return Result{Output: "uncompressed payload store is unavailable", Title: "retrieve"}, nil
	}

	if a.List {
		keys := t.Store.LiveKeys()
		sort.Strings(keys)
		if len(keys) == 0 {
			return Result{Output: "no compressed payloads are currently stored", Title: "retrieve"}, nil
		}
		return repairNote(Result{
			Output: fmt.Sprintf("%d stored payloads:\n%s", len(keys), strings.Join(keys, "\n")),
			Title:  fmt.Sprintf("retrieve (%d keys)", len(keys)),
		}, noteOf(tc)), nil
	}
	key := strings.TrimSpace(a.Key)
	if key == "" {
		return Errf("provide either a key or list:true"), nil
	}
	if original, ok := t.Store.LiveOriginal(key); ok {
		return repairNote(Result{Output: original, Title: "retrieve " + shortKey(key)}, noteOf(tc)), nil
	}
	if payload, ok := t.Store.StoredPayload(key); ok {
		return repairNote(Result{Output: payload, Title: "retrieve " + shortKey(key)}, noteOf(tc)), nil
	}
	return Errf("no stored payload for key %q; run with list:true to see available keys", key), nil
}

func shortKey(key string) string {
	if len(key) <= 16 {
		return key
	}
	return key[:16] + "…"
}
