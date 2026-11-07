package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent-examples/orchestration/internal/demomodel"
	shared "github.com/kart-io/goagent/constants"
	"github.com/kart-io/goagent/core"
	"github.com/kart-io/goagent/core/usage"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

type InputCapture struct {
	MaxTokens       int
	Role, SessionID string
	Messages        []llm.Message
	Tools           []string
}
type transportLimiter struct {
	slots                  chan struct{}
	mu                     sync.Mutex
	requests, active, peak int
	idle                   chan struct{}
}

func newTransportLimiter(n int) *transportLimiter {
	idle := make(chan struct{})
	close(idle)
	return &transportLimiter{slots: make(chan struct{}, n), idle: idle}
}
func (l *transportLimiter) acquire(ctx context.Context) (func(), error) {
	select {
	case l.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-l.slots
		return nil, err
	}
	l.mu.Lock()
	if l.active == 0 {
		l.idle = make(chan struct{})
	}
	l.active++
	l.requests++
	if l.active > l.peak {
		l.peak = l.active
	}
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.active--
			if l.active == 0 {
				close(l.idle)
			}
			l.mu.Unlock()
			<-l.slots
		})
	}, nil
}
func (l *transportLimiter) stats() (int, int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.requests, l.active, l.peak
}
func (l *transportLimiter) wait(ctx context.Context) error {
	l.mu.Lock()
	idle := l.idle
	l.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("unsettled model transport: %w", ctx.Err())
	}
}

// limitedClient is deliberately below Builder's reservation path. It never calls Reserve.
// A permit is retained until the underlying transport channel closes, even after cancellation.
type limitedClient struct {
	llm.ToolStreamClient
	host          *host
	role, session string
}

func (c *limitedClient) CompleteToolsStream(ctx context.Context, r *llm.CompletionRequest, offered []interfaces.Tool) (out <-chan *llm.StreamChunk, err error) {
	release, err := c.host.limiter.acquire(ctx)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			release()
		}
	}()
	names := make([]string, len(offered))
	for i, t := range offered {
		names[i] = t.Name()
	}
	c.host.mu.Lock()
	c.host.inputs = append(c.host.inputs, InputCapture{MaxTokens: r.MaxTokens, Role: c.role, SessionID: c.session, Messages: clone(r.Messages), Tools: names})
	c.host.mu.Unlock()
	src, err := c.ToolStreamClient.CompleteToolsStream(ctx, r, offered)
	if err != nil {
		return nil, err
	}
	if src == nil {
		return nil, fmt.Errorf("nil model stream")
	}
	dst := make(chan *llm.StreamChunk)
	transferred = true
	go func() {
		defer close(dst)
		defer release()
		discard := false
		for chunk := range src {
			if discard {
				continue
			}
			select {
			case dst <- chunk:
			case <-ctx.Done():
				discard = true
			}
		}
	}()
	return dst, nil
}
func (c *limitedClient) CompleteStream(ctx context.Context, r *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	return c.CompleteToolsStream(ctx, r, nil)
}
func (c *limitedClient) StreamMode() core.StreamMode {
	if v, ok := c.ToolStreamClient.(core.StreamCapability); ok {
		return v.StreamMode()
	}
	return core.StreamModeTokens
}
func (c *limitedClient) metadata() llm.ToolStreamClient {
	if live, ok := c.ToolStreamClient.(*livemodel.Client); ok {
		return live.ToolStreamClient
	}
	return c.ToolStreamClient
}
func (c *limitedClient) ModelName() string {
	if v, ok := c.metadata().(interface{ ModelName() string }); ok {
		return v.ModelName()
	}
	if v, ok := c.ToolStreamClient.(*livemodel.Client); ok {
		return v.Live.Model
	}
	return "unknown"
}
func (c *limitedClient) ResolveReasoning(r *llm.CompletionRequest) *usage.ReasoningConfig {
	if v, ok := c.metadata().(interface {
		ResolveReasoning(*llm.CompletionRequest) *usage.ReasoningConfig
	}); ok {
		return v.ResolveReasoning(r)
	}
	return r.Reasoning
}
func (c *limitedClient) ValidateReasoning(r *llm.CompletionRequest) error {
	return llm.ValidateReasoningRequest(c.metadata(), r)
}

// scriptedModel replaces only network transport. Builder selects/executes native tools,
// builds the real history, emits usage and applies the same host acceptance as live mode.
type scriptedModel struct{ role string }

func (*scriptedModel) Provider() shared.Provider { return shared.ProviderOpenAI }
func (*scriptedModel) ModelName() string         { return "offline-agent-context" }
func (m *scriptedModel) CompleteStream(ctx context.Context, r *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	return m.CompleteToolsStream(ctx, r, nil)
}
func (m *scriptedModel) CompleteToolsStream(ctx context.Context, r *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var turn llm.StreamChunk
	if m.role == "parent" {
		found := false
		for _, msg := range r.Messages {
			if msg.Role == "tool" {
				found = true
			}
		}
		if !found {
			turn.ToolCalls = []llm.ToolCall{demomodel.Call("delegate-trip", "analyze_trip")}
		} else {
			turn.Content = "The accepted synthetic itinerary satisfies the confirmed constraints; no booking was made."
		}
	} else {
		var b Brief
		for _, msg := range r.Messages {
			if msg.Role == "user" {
				if err := json.Unmarshal([]byte(msg.Content), &b); err != nil {
					return nil, fmt.Errorf("actual model brief: %w", err)
				}
			}
		}
		var evidenceSeen bool
		for _, msg := range r.Messages {
			if msg.Role == "tool" {
				var v struct{ Reference ArtifactRef }
				if json.Unmarshal([]byte(msg.Content), &v) == nil && v.Reference.Schema == "evidence" {
					evidenceSeen = true
				}
			}
		}
		if !evidenceSeen {
			call := demomodel.Call("read-"+m.role, "read_evidence")
			call.Function.Arguments = string(encode(map[string]any{"id": b.Evidence[0].ID}))
			turn.ToolCalls = []llm.ToolCall{call}
		} else {
			// Decode the actual tool response, not a second hidden source of facts.
			var facts Candidate
			for _, msg := range r.Messages {
				if msg.Role == "tool" {
					var value struct{ Evidence Candidate }
					if err := json.Unmarshal([]byte(msg.Content), &value); err != nil {
						return nil, err
					}
					if value.Evidence.CandidateID != "" {
						facts = value.Evidence
					}
				}
			}
			report := ReportDraft{Conclusion: "feasible", Summary: m.role + " verified both legs, family total and conditional fare rules against the immutable synthetic supplier evidence.", Claims: facts, DecisionIDs: []string{"direct-only", "family-count"}, Evidence: b.Evidence[0].ID}
			call := demomodel.Call("submit-"+m.role, "submit_report")
			call.Function.Arguments = string(encode(report))
			turn.ToolCalls = []llm.ToolCall{call}
		}
	}
	turn.Done = true
	turn.Usage = &llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	turn.FinishReason = "stop"
	if len(turn.ToolCalls) > 0 {
		turn.FinishReason = "tool_calls"
	}
	ch := make(chan *llm.StreamChunk, 1)
	ch <- &turn
	close(ch)
	return ch, nil
}

var _ llm.ToolStreamClient = (*limitedClient)(nil)
var _ llm.ToolStreamClient = (*scriptedModel)(nil)
