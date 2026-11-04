// Package demomodel supplies deterministic, local-only model responses for examples.
package demomodel

import (
	"context"
	"sync/atomic"

	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/llm/constants"
)

// Model counts actual transport entries; replay never enters it.
type Model struct {
	Calls   *atomic.Int64
	Respond func(*llm.CompletionRequest) llm.StreamChunk
}

func (m *Model) Provider() constants.Provider { return constants.ProviderOpenAI }
func (m *Model) CompleteStream(ctx context.Context, r *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.Calls.Add(1)
	turn := m.Respond(r)
	turn.Done = true
	turn.Usage = &llm.Usage{TotalTokens: 2}
	if len(turn.ToolCalls) > 0 {
		turn.FinishReason = "tool_calls"
	} else {
		turn.FinishReason = "stop"
	}
	ch := make(chan *llm.StreamChunk, 1)
	ch <- &turn
	close(ch)
	return ch, nil
}
func (m *Model) CompleteToolsStream(ctx context.Context, r *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	return m.CompleteStream(ctx, r)
}

// Call constructs a complete native tool call, never a partial argument fragment.
func Call(id, name string) llm.ToolCall {
	c := llm.ToolCall{ID: id, Type: "function"}
	c.Function.Name = name
	c.Function.Arguments = "{}"
	return c
}
