package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kart-io/goagent-examples/orchestration/internal/demomodel"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/usage"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/tools"
)

type transportFunc struct {
	*scriptedModel
	fn func(context.Context, *llm.CompletionRequest, []interfaces.Tool) (<-chan *llm.StreamChunk, error)
}

func (m *transportFunc) CompleteToolsStream(ctx context.Context, r *llm.CompletionRequest, ts []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	return m.fn(ctx, r, ts)
}
func streamTurn(turn llm.StreamChunk) <-chan *llm.StreamChunk {
	turn.Done = true
	if len(turn.ToolCalls) > 0 {
		turn.FinishReason = "tool_calls"
	} else {
		turn.FinishReason = "stop"
	}
	turn.Usage = &llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	ch := make(chan *llm.StreamChunk, 1)
	ch <- &turn
	close(ch)
	return ch
}
func testDeadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}
func TestTransportPermitUntilSourceClosureAndCancelDrain(t *testing.T) {
	h := setupHost(t)
	src := make(chan *llm.StreamChunk)
	model := &transportFunc{scriptedModel: &scriptedModel{}, fn: func(context.Context, *llm.CompletionRequest, []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
		return src, nil
	}}
	client := &limitedClient{ToolStreamClient: model, host: h, role: "unit"}
	ctx, cancel := context.WithCancel(testDeadline(t))
	out, err := client.CompleteToolsStream(ctx, &llm.CompletionRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sent := make(chan struct{})
	go func() { src <- &llm.StreamChunk{Done: true}; close(sent) }()
	<-out
	<-sent
	if _, active, _ := h.limiter.stats(); active != 1 {
		t.Fatal("permit released before transport really closed")
	}
	cancel()
	if _, active, _ := h.limiter.stats(); active != 1 {
		t.Fatal("cancel falsely marked unknown transport settled")
	}
	close(src)
	if err := h.limiter.wait(testDeadline(t)); err != nil {
		t.Fatal(err)
	}
	if _, active, _ := h.limiter.stats(); active != 0 {
		t.Fatal("settled transport retained permit")
	}
}
func TestTransportErrorPanicAndWaitingCancellationRelease(t *testing.T) {
	for _, name := range []string{"error", "panic"} {
		t.Run(name, func(t *testing.T) {
			h := setupHost(t)
			model := &transportFunc{scriptedModel: &scriptedModel{}, fn: func(context.Context, *llm.CompletionRequest, []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
				if name == "panic" {
					panic("controlled transport panic")
				}
				return nil, errors.New("controlled transport error")
			}}
			c := &limitedClient{ToolStreamClient: model, host: h}
			func() {
				defer func() { _ = recover() }()
				_, _ = c.CompleteToolsStream(context.Background(), &llm.CompletionRequest{}, nil)
			}()
			if _, active, _ := h.limiter.stats(); active != 0 {
				t.Fatal("failed transport leaked permit")
			}
		})
	}
	limiter := newTransportLimiter(1)
	release, err := limiter.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := limiter.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation: %v", err)
	}
	release()
	if requests, active, peak := limiter.stats(); requests != 1 || active != 0 || peak != 1 {
		t.Fatalf("queued request counted as transport: %d %d %d", requests, active, peak)
	}
}
func TestNativeParallelActualTransportConcurrencyAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			var mu sync.Mutex
			entered := 0
			both := make(chan struct{})
			closed := 0
			model := &transportFunc{scriptedModel: &scriptedModel{}}
			model.fn = func(ctx context.Context, r *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
				var b Brief
				for _, m := range r.Messages {
					if m.Role == "user" {
						if err := jsonUnmarshal([]byte(m.Content), &b); err != nil {
							return nil, err
						}
					}
				}
				toolResults := 0
				for _, m := range r.Messages {
					if m.Role == "tool" {
						toolResults++
					}
				}
				if toolResults == 0 {
					mu.Lock()
					entered++
					if entered == 2 {
						close(both)
					}
					mu.Unlock()
					select {
					case <-both:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					if fail {
						if b.Role == "itinerary" {
							return nil, errors.New("controlled itinerary failure")
						}
						ch := make(chan *llm.StreamChunk)
						go func() { <-ctx.Done(); mu.Lock(); closed++; mu.Unlock(); close(ch) }()
						return ch, nil
					}
				}
				return (&scriptedModel{role: b.Role}).CompleteToolsStream(ctx, r, nil)
			}
			result, err := Run(testDeadline(t), Config{Mode: "parallel", Dir: t.TempDir(), Client: model}, DefaultTask())
			if result.Peak != 2 || result.InFlight != 0 || result.ChildrenCreated != result.ChildrenClosed {
				t.Fatalf("bad concurrency/cleanup: %+v %v", result, err)
			}
			if fail {
				if err == nil || len(result.Accepted) != 0 || result.Requests != 2 || result.Usage.Root.ReservedRequests != 2 || result.Usage.UnknownUsageEntries < 1 || closed != 1 {
					t.Fatalf("failure incorrectly merged or lost usage: requests=%d reserved=%d unknown=%d closed=%d err=%v", result.Requests, result.Usage.Root.ReservedRequests, result.Usage.UnknownUsageEntries, closed, err)
				}
			} else if err != nil || len(result.Accepted) != 2 {
				t.Fatalf("parallel success: %v", err)
			}
		})
	}
}
func TestBuilderBudgetReservationPrecedesTransportAndNeverDoubleReserves(t *testing.T) {
	h := setupHost(t)
	// Run the actual Builder path with a transport that reads the real shared controller.
	seen := 0
	model := &transportFunc{scriptedModel: &scriptedModel{}, fn: func(ctx context.Context, r *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
		controller, ok := budget.FromContext(ctx)
		if !ok {
			return nil, errors.New("missing root controller")
		}
		snapshot, err := controller.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		seen++
		if snapshot.ReservedRequests != seen {
			return nil, fmt.Errorf("expected exactly one pre-transport reservation per request: %d vs %d", snapshot.ReservedRequests, seen)
		}
		var b Brief
		for _, m := range r.Messages {
			if m.Role == "user" {
				_ = jsonUnmarshal([]byte(m.Content), &b)
			}
		}
		return (&scriptedModel{role: b.Role}).CompleteToolsStream(ctx, r, nil)
	}}
	_ = h
	result, err := Run(context.Background(), Config{Mode: "pipeline", Dir: t.TempDir(), Client: model}, DefaultTask())
	if err != nil {
		t.Fatal(err)
	}
	if seen != 4 || result.Requests != 4 || result.Usage.Root.ReservedRequests != 4 || result.Usage.Root.KnownTokens.TotalTokens != 60 || result.UsageEvents != 4 {
		t.Fatalf("budget or cumulative-hook double counting: seen=%d requests=%d reserved=%d tokens=%d hooks=%d", seen, result.Requests, result.Usage.Root.ReservedRequests, result.Usage.Root.KnownTokens.TotalTokens, result.UsageEvents)
	}
}

type metadataModel struct{ *scriptedModel }

func (*metadataModel) ModelName() string { return "metadata-model" }
func (*metadataModel) ResolveReasoning(*llm.CompletionRequest) *usage.ReasoningConfig {
	return &usage.ReasoningConfig{}
}
func (*metadataModel) ValidateReasoning(*llm.CompletionRequest) error {
	return errors.New("sentinel validation")
}
func TestWrapperForwardsOptionalMetadata(t *testing.T) {
	c := &limitedClient{ToolStreamClient: &metadataModel{&scriptedModel{}}}
	if c.ModelName() != "metadata-model" || c.ResolveReasoning(&llm.CompletionRequest{}) == nil || c.ValidateReasoning(&llm.CompletionRequest{}) == nil {
		t.Fatal("optional model metadata/reasoning lost")
	}
}

func TestSharedRootHardCapTwelveAcrossIndependentBuilders(t *testing.T) {
	h := setupHost(t)
	ctx := budget.WithController(testDeadline(t), h.controller)
	var gateMu sync.Mutex
	started := 0
	gate := make(chan struct{})
	model := &transportFunc{scriptedModel: &scriptedModel{}, fn: func(ctx context.Context, _ *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
		gateMu.Lock()
		started++
		if started == 2 {
			close(gate)
		}
		gateMu.Unlock()
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return streamTurn(llm.StreamChunk{Content: "read-only transport complete"}), nil
	}}
	var wg sync.WaitGroup
	successes := make(chan bool, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, err := dispatch(tools.DispatchConfig{}, &nativeTool{name: "read_only", description: "read-only", schema: `{"type":"object"}`, run: func(context.Context, *interfaces.ToolInput) (any, error) { return "data", nil }})
			if err != nil {
				successes <- false
				return
			}
			client := &limitedClient{ToolStreamClient: model, host: h, role: fmt.Sprintf("stress-%d", i)}
			a, err := builder.NewSimpleBuilder(client).WithAgentID(fmt.Sprintf("stress-%d", i)).WithTools(d.Tools()...).WithMaxIterations(1).Build()
			if err == nil {
				_, err = a.AsRunnable().Invoke(ctx, "read-only root cap stress")
			}
			successes <- err == nil
		}(i)
	}
	wg.Wait()
	close(successes)
	count := 0
	for ok := range successes {
		if ok {
			count++
		}
	}
	if err := h.limiter.wait(testDeadline(t)); err != nil {
		t.Fatal(err)
	}
	requests, active, peak := h.limiter.stats()
	snapshot, err := h.controller.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 12 || requests != 12 || snapshot.ReservedRequests != 12 || active != 0 || peak != 2 {
		t.Fatalf("root bound broken: successes=%d requests=%d reservations=%d active=%d peak=%d", count, requests, snapshot.ReservedRequests, active, peak)
	}
}

func TestInvocationSpecificCancelOrReplacementReachesActualTransport(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			h := setupHost(t)
			entered := make(chan struct{})
			transportCanceled := make(chan struct{})
			h.client.Client = &transportFunc{scriptedModel: &scriptedModel{}, fn: func(ctx context.Context, r *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
				var b Brief
				for _, m := range r.Messages {
					if m.Role == "user" {
						_ = jsonUnmarshal([]byte(m.Content), &b)
					}
				}
				ch := make(chan *llm.StreamChunk, 1)
				close(entered)
				go func() {
					<-ctx.Done()
					close(transportCanceled)
					report := ReportDraft{Conclusion: "feasible", Summary: "A late otherwise valid report", Claims: b.RequiredFacts, DecisionIDs: []string{"direct-only", "family-count"}, Evidence: b.Evidence[0].ID}
					call := demomodel.Call("late", "submit_report")
					call.Function.Arguments = string(encode(report))
					ch <- &llm.StreamChunk{Done: true, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{call}}
					close(ch)
				}()
				return ch, nil
			}}
			ctx, cancel := context.WithCancel(budget.WithController(testDeadline(t), h.controller))
			defer cancel()
			b, err := h.brief("itinerary", nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := h.execute(ctx, b); done <- err }()
			<-entered
			h.mu.Lock()
			var id string
			for key := range h.invocations {
				id = key
			}
			h.mu.Unlock()
			if replace {
				if _, err := h.register(ctx, b); err != nil {
					t.Fatal(err)
				}
			} else {
				h.cancelInvocation(id)
			}
			select {
			case <-transportCanceled:
			case <-time.After(time.Second):
				cancel()
				<-done
				t.Fatal("invocation cancellation never reached the actual model transport")
			}
			if err := <-done; err == nil {
				t.Fatal("canceled child returned success")
			}
			if err := h.limiter.wait(testDeadline(t)); err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			adopted := len(h.receipts)
			created, closed := h.created, h.closed
			h.mu.Unlock()
			if adopted != 0 || created != closed {
				t.Fatalf("late work adopted or cleanup leaked: receipts=%d children=%d/%d", adopted, closed, created)
			}
		})
	}
}

func TestCanceledBeforeCancelAttachmentNeverStartsObsoleteTransport(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			h := setupHost(t)
			b, err := h.brief("itinerary", nil)
			if err != nil {
				t.Fatal(err)
			}
			childCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e, err := h.register(childCtx, b)
			if err != nil {
				t.Fatal(err)
			}
			if replace {
				if _, err := h.register(context.Background(), b); err != nil {
					t.Fatal(err)
				}
			} else {
				h.cancelInvocation(e.InvocationID)
			}
			// This is the exact interleaving between registration and factory attachment.
			h.attachCancel(e.InvocationID, cancel)
			if childCtx.Err() == nil {
				t.Fatal("already ineligible invocation attached a live cancel context")
			}
			calls := 0
			model := &transportFunc{scriptedModel: &scriptedModel{}, fn: func(context.Context, *llm.CompletionRequest, []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
				calls++
				return streamTurn(llm.StreamChunk{Content: "obsolete"}), nil
			}}
			c := &limitedClient{ToolStreamClient: model, host: h}
			_, _ = c.CompleteToolsStream(childCtx, &llm.CompletionRequest{}, nil)
			if calls != 0 {
				t.Fatal("known obsolete invocation entered model transport")
			}
		})
	}
}
