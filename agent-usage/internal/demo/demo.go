// Package demo supplies shared helpers for usage examples. Scripted responses
// are offline fixtures; NewLiveClient is an explicit real-provider constructor.
package demo

import (
	"context"
	"fmt"
	"sync"

	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/tools"
)

type Model struct {
	*llm.MockStreamClient
	mu    sync.Mutex
	turns []*llm.StreamChunk
}

func NewModel(turns ...*llm.StreamChunk) *Model {
	return &Model{MockStreamClient: llm.NewMockStreamClient(), turns: turns}
}
func (*Model) ModelName() string { return "offline-usage-demo" }
func (m *Model) CompleteStream(ctx context.Context, r *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	return m.CompleteToolsStream(ctx, r, nil)
}
func (m *Model) CompleteToolsStream(ctx context.Context, _ *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.turns) == 0 {
		return nil, fmt.Errorf("unexpected model request")
	}
	ch := make(chan *llm.StreamChunk, 1)
	ch <- m.turns[0]
	m.turns = m.turns[1:]
	close(ch)
	return ch, nil
}
func Answer(prompt, completion int) *llm.StreamChunk {
	return &llm.StreamChunk{Delta: "done", Done: true, FinishReason: "stop", Usage: &llm.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}}
}
func ToolTurn(prompt, completion int, names ...string) *llm.StreamChunk {
	c := Answer(prompt, completion)
	c.Delta = ""
	c.FinishReason = "tool_calls"
	for i, name := range names {
		call := llm.ToolCall{ID: fmt.Sprintf("call-%d", i), Type: "function"}
		call.Function.Name, call.Function.Arguments = name, "{}"
		c.ToolCalls = append(c.ToolCalls, call)
	}
	return c
}
func Must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
func Check(err error) {
	if err != nil {
		panic(err)
	}
}
func Context(taskID string) (context.Context, *budget.Controller) {
	c := Must(budget.New(budget.Config{RootKey: taskID, Version: "usage-demo", MaxModelRequests: 20, MaxAgentCalls: 10, MaxToolCalls: 10}))
	return budget.WithController(context.Background(), c), c
}
func Dispatcher(ts ...interfaces.Tool) *tools.Dispatcher {
	r := tools.NewRegistry()
	for _, t := range ts {
		Check(r.Register(t))
	}
	return Must(tools.NewDispatcher(r, tools.DispatchConfig{}))
}
func ChildTool(name string, a *builder.SimpleAgent) interfaces.Tool {
	return Must(tools.NewRunnableTool(a.AsRunnable(), tools.RunnableToolConfig[any, *builder.AgentOutput]{
		Name: name, Description: "Delegate to a child Agent", Version: "usage-demo", ModelInvoking: true,
		ArgsSchema: `{"type":"object"}`,
		Decode:     func(map[string]any) (any, error) { return "complete the delegated task", nil },
		Encode:     func(o *builder.AgentOutput) (any, error) { return o.Result, nil },
	}))
}
