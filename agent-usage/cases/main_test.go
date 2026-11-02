package main

import (
	"context"
	"github.com/kart-io/goagent-examples/agent-usage/internal/demo"
	"github.com/kart-io/goagent/core/usage"
	"github.com/kart-io/goagent/llm"
	"testing"
)

func withArgs(c *llm.StreamChunk, args string) *llm.StreamChunk {
	for i := range c.ToolCalls {
		c.ToolCalls[i].Function.Arguments = args
	}
	return c
}
func TestMixedTaskAndReplay(t *testing.T) {
	clients := map[string]llm.ToolStreamClient{
		"gemini":   demo.NewModel(withArgs(demo.ToolTurn(2, 1, "research", "review"), `{"task":"synthetic-order"}`), demo.Answer(3, 1)),
		"deepseek": demo.NewModel(detailed(withArgs(demo.ToolTurn(1, 1, "lookup_order"), `{"order_id":"synthetic-order"}`)), demo.Answer(2, 1)),
		"minimax":  demo.NewModel(demo.Answer(3, 1)),
	}
	// Preserve actual response metadata and known details through a child tool replay.
	r := runCase(context.Background(), "mixed-replay", clients, t.TempDir())
	if !r.Passed {
		t.Fatalf("case failed: %+v", r)
	}
	if r.Before.Root.KnownTokens.TotalTokens != 16 || r.After.Root.KnownTokens.TotalTokens != 16 {
		t.Fatalf("bad total: %+v", r)
	}
	if r.NetworkCallsBefore != 5 || r.NetworkCallsAfter != 5 || r.ToolRuns != 1 {
		t.Fatalf("replayed side effect: %+v", r)
	}
	if r.Platforms["deepseek"].ReasoningTokens != 1 || r.After.Root.KnownTokens.ReasoningTokens != 1 {
		t.Fatalf("lost reasoning detail: %+v", r)
	}
	if len(r.Platforms) != 3 {
		t.Fatalf("missing platforms: %+v", r.Platforms)
	}
}
func TestFailureAndCancellationRetainUsage(t *testing.T) {
	for _, name := range []string{"tool-error", "cancel"} {
		t.Run(name, func(t *testing.T) {
			clients := map[string]llm.ToolStreamClient{"gemini": demo.NewModel(withArgs(demo.ToolTurn(2, 1, "lookup_order"), `{"order_id":"synthetic-order"}`))}
			r := runCase(context.Background(), name, clients, t.TempDir())
			if !r.Passed || !r.ExpectedErrorObserved || r.Before.Root.KnownTokens.TotalTokens != 3 || r.NetworkCallsAfter != 1 || r.ToolRuns != 1 {
				t.Fatalf("bad failure accounting: %+v", r)
			}
		})
	}
}

func detailed(c *llm.StreamChunk) *llm.StreamChunk {
	c.ResponseModel = "actual-child"
	fields := usage.BasicFields | usage.Reasoning
	c.Usage.KnownFields = &fields
	c.Usage.ReasoningTokens = 1
	return c
}
