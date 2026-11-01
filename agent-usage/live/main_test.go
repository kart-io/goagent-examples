package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kart-io/goagent-examples/agent-usage/internal/demo"
	"github.com/kart-io/goagent/llm"
)

// Offline tests verify the live runner's checks, not provider connectivity.
func TestScenarioAccountingChecks(t *testing.T) {
	cases := []struct {
		name  string
		turns []*llm.StreamChunk
	}{
		{"single", []*llm.StreamChunk{demo.Answer(7, 3)}},
		{"child", []*llm.StreamChunk{demo.ToolTurn(2, 1, "write"), demo.Answer(7, 3), demo.Answer(3, 1)}},
		{"task", []*llm.StreamChunk{demo.ToolTurn(2, 1, "research", "review"), demo.ToolTurn(1, 1, "lookup_order"), demo.Answer(3, 1), demo.Answer(2, 1), demo.Answer(4, 1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "task" {
				tc.turns[1].ToolCalls[0].Function.Arguments = `{"order_id":"synthetic-order"}`
			}
			r := runScenario(context.Background(), demo.NewModel(tc.turns...), tc.name)
			if !r.Passed {
				t.Fatalf("scenario failed: %+v", r)
			}
			if r.Finished.Before(r.Started) {
				t.Fatal("missing completion timestamp")
			}
			r.Attempts[0].Tokens.ReasoningTokens++
			if validate(r) == nil {
				t.Fatal("mismatched reasoning detail must fail validation")
			}
			r.Attempts[0].Tokens.ReasoningTokens--
			r.Attempts = nil
			if validate(r) == nil {
				t.Fatal("missing hook evidence must fail validation")
			}
		})
	}
}
func TestMissingCredentialsNeverFallBack(t *testing.T) {
	for _, provider := range []string{"deepseek", "minimax", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			name := strings.ToUpper(provider) + "_API_KEY"
			t.Setenv(name, "")
			_, err := client(provider, "test-model")
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("expected missing credential error, got %v", err)
			}
		})
	}
}
