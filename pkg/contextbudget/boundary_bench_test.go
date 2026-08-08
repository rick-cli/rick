package contextbudget

import (
	"fmt"
	"strings"
	"testing"

	"rick/internal/provider"
	"rick/internal/tokens"
)

// BenchmarkChooseBoundariesSteadyTurn approximates a long session's steady
// state: a large byte-stable history with one new user turn appended per
// call, the pattern buildRequest exercises on every turn. It is the
// regression guard for the per-turn tokenization cost (C1/S1) and the
// boundary pass (C2/S2, C3/S5).
func BenchmarkChooseBoundariesSteadyTurn(b *testing.B) {
	budget := New(Options{MinStableTurns: 2, LiveZoneTurns: 1, MaxStableBytes: 64, MinCacheTokens: 1, Encoding: tokens.EncodingCl100kBase})
	history := make([]provider.Message, 0, 600)
	for i := 0; i < 300; i++ {
		history = append(history,
			provider.UserText(fmt.Sprintf("user message %d with a reasonable amount of context ", i)),
			provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{
				Type: "text", Text: strings.Repeat("assistant answer tokens ", 12),
			}}},
		)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		turn := make([]provider.Message, 0, len(history)+1)
		turn = append(turn, history...)
		turn = append(turn, provider.UserText(fmt.Sprintf("newest turn %d", i)))
		budget.ChooseBoundaries(turn)
	}
}
