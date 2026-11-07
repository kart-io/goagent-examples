package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	shared "github.com/kart-io/goagent/constants"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/usage"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
)

// The network double returns the same late error frame as the native provider;
// real Builder, budget, tools, read registry and host acceptance still execute.
type geminiTransport struct{ *transportFunc }

func (*geminiTransport) Provider() shared.Provider { return shared.ProviderGemini }

func malformedFrames(err error, emitted bool) <-chan *llm.StreamChunk {
	ch := make(chan *llm.StreamChunk, 3)
	fields := usage.Total
	ch <- &llm.StreamChunk{Usage: &llm.Usage{TotalTokens: 7, KnownFields: &fields}}
	if emitted {
		ch <- &llm.StreamChunk{Delta: "Partial answer already emitted"}
	}
	ch <- &llm.StreamChunk{Error: err, Done: true, FinishReason: "MALFORMED_FUNCTION_CALL"}
	close(ch)
	return ch
}
func briefFromRequest(r *llm.CompletionRequest) Brief {
	var b Brief
	for _, m := range r.Messages {
		if m.Role == "user" {
			_ = jsonUnmarshal([]byte(m.Content), &b)
		}
	}
	return b
}
func TestGeminiMalformedLateFrameRetriesWithIndependentBudgetAndUsage(t *testing.T) {
	calls := 0
	checkedBeforeRetry := false
	client := &geminiTransport{&transportFunc{scriptedModel: &scriptedModel{}, fn: func(ctx context.Context, r *llm.CompletionRequest, ts []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
		calls++
		if calls == 2 {
			return malformedFrames(errors.New(`gemini: incomplete or blocked response (finish="MALFORMED_FUNCTION_CALL")`), false), nil
		}
		if calls == 3 {
			c, ok := budget.FromContext(ctx)
			if !ok {
				return nil, fmt.Errorf("missing root budget")
			}
			snapshot, err := c.Snapshot(ctx)
			if err != nil {
				return nil, err
			}
			if snapshot.ReservedRequests != 3 || snapshot.KnownUsageEntries != 2 || snapshot.KnownTokens.TotalTokens != 22 {
				return nil, fmt.Errorf("retry sent before prior evidence and new reservation: %+v", snapshot)
			}
			checkedBeforeRetry = true
		}
		return (&scriptedModel{role: briefFromRequest(r).Role}).CompleteToolsStream(ctx, r, ts)
	}}}
	result, err := Run(context.Background(), Config{Mode: "pipeline", Dir: t.TempDir(), Client: client}, DefaultTask())
	if err != nil {
		t.Fatal(err)
	}
	if !checkedBeforeRetry || result.Requests != 5 || result.Usage.Root.ReservedRequests != 5 || result.Usage.Root.KnownUsageEntries != 5 || result.Usage.Root.KnownTokens.TotalTokens != 67 || result.Usage.PartialUsageEntries != 1 {
		t.Fatalf("failed attempt accounting lost: requests=%d reserved=%d known=%d tokens=%d partial=%d", result.Requests, result.Usage.Root.ReservedRequests, result.Usage.Root.KnownUsageEntries, result.Usage.Root.KnownTokens.TotalTokens, result.Usage.PartialUsageEntries)
	}
	if len(result.Accepted) != 2 || len(result.Receipts) != 2 || len(result.Identities) != 4 {
		t.Fatal("model recovery repeated tools or acceptance")
	}
	if !reflect.DeepEqual(result.Inputs[1], result.Inputs[2]) {
		t.Fatal("retry changed messages, tools or invocation/session identity")
	}
	if result.InFlight != 0 || result.ChildrenCreated != result.ChildrenClosed {
		t.Fatal("recovery leaked lifecycle")
	}
}
func TestGeminiMalformedRecoveryHasOneInvocationAllowance(t *testing.T) {
	for _, scenario := range []string{"same-step", "across-steps"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			client := &geminiTransport{&transportFunc{scriptedModel: &scriptedModel{}, fn: func(ctx context.Context, r *llm.CompletionRequest, ts []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
				calls++
				if scenario == "same-step" || calls == 1 || calls == 3 {
					return malformedFrames(errors.New(`gemini: incomplete or blocked response (finish="MALFORMED_FUNCTION_CALL")`), false), nil
				}
				return (&scriptedModel{role: briefFromRequest(r).Role}).CompleteToolsStream(ctx, r, ts)
			}}}
			result, err := Run(context.Background(), Config{Mode: "pipeline", Dir: t.TempDir(), Client: client}, DefaultTask())
			want := 2
			if scenario == "across-steps" {
				want = 3
			}
			if err == nil || calls != want || result.Requests != want || result.Usage.Root.ReservedRequests != want || len(result.Accepted) != 0 {
				t.Fatalf("recovery allowance: calls=%d requests=%d reserved=%d err=%v", calls, result.Requests, result.Usage.Root.ReservedRequests, err)
			}
			if result.InFlight != 0 || result.ChildrenCreated != result.ChildrenClosed {
				t.Fatal("exhausted recovery leaked lifecycle")
			}
		})
	}
}
func TestGeminiMalformedRecoveryCannotBypassRootBudget(t *testing.T) {
	calls := 0
	client := &geminiTransport{&transportFunc{scriptedModel: &scriptedModel{}, fn: func(context.Context, *llm.CompletionRequest, []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
		calls++
		return malformedFrames(errors.New(`gemini: incomplete or blocked response (finish="MALFORMED_FUNCTION_CALL")`), false), nil
	}}}
	result, err := Run(context.Background(), Config{Mode: "pipeline", Dir: t.TempDir(), Client: client, MaxRequests: 1}, DefaultTask())
	if !errors.Is(err, agentErrors.ErrModelBudgetExceeded) || calls != 1 || result.Requests != 1 || result.Usage.Root.ReservedRequests != 1 || result.Usage.Root.KnownTokens.TotalTokens != 7 {
		t.Fatalf("recovery bypassed root budget or lost failure usage: calls=%d requests=%d tokens=%d err=%v", calls, result.Requests, result.Usage.Root.KnownTokens.TotalTokens, err)
	}
}

func TestGeminiMalformedRecoveryRejectsOtherErrorsProvidersAndEmittedOutput(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		err                    error
		emitted, otherProvider bool
	}{
		{"emitted", errors.New(`gemini: incomplete or blocked response (finish="MALFORMED_FUNCTION_CALL")`), true, false},
		{"other-provider", errors.New(`gemini: incomplete or blocked response (finish="MALFORMED_FUNCTION_CALL")`), false, true},
		{"max-tokens", errors.New(`gemini: incomplete or blocked response (finish="MAX_TOKENS")`), false, false},
		{"safety", errors.New(`gemini: incomplete or blocked response (finish="SAFETY")`), false, false},
		{"wrapped-tool-error", errors.New(`tool validation: gemini: incomplete or blocked response (finish="MALFORMED_FUNCTION_CALL")`), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			transport := &transportFunc{scriptedModel: &scriptedModel{}, fn: func(context.Context, *llm.CompletionRequest, []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
				calls++
				return malformedFrames(tc.err, tc.emitted), nil
			}}
			var client llm.ToolStreamClient = &geminiTransport{transport}
			if tc.otherProvider {
				client = transport
			}
			result, err := Run(context.Background(), Config{Mode: "pipeline", Dir: t.TempDir(), Client: client}, DefaultTask())
			if err == nil || calls != 1 || result.Requests != 1 || result.Usage.Root.ReservedRequests != 1 || len(result.Accepted) != 0 {
				t.Fatalf("ineligible error retried: calls=%d requests=%d err=%v", calls, result.Requests, err)
			}
		})
	}
}
func TestGeminiMalformedRecoveryDoesNotRetryHostValidation(t *testing.T) {
	calls := 0
	client := &geminiTransport{&transportFunc{scriptedModel: &scriptedModel{}, fn: func(ctx context.Context, r *llm.CompletionRequest, ts []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
		calls++
		out, err := (&scriptedModel{role: briefFromRequest(r).Role}).CompleteToolsStream(ctx, r, ts)
		if err != nil {
			return nil, err
		}
		chunk := <-out
		for i, call := range chunk.ToolCalls {
			if call.Function.Name == "submit_report" {
				var draft ReportDraft
				_ = jsonUnmarshal([]byte(call.Function.Arguments), &draft)
				draft.Claims.TotalMinor = 35000
				chunk.ToolCalls[i].Function.Arguments = string(encode(draft))
			}
		}
		return streamTurn(*chunk), nil
	}}}
	result, err := Run(context.Background(), Config{Mode: "pipeline", Dir: t.TempDir(), Client: client}, DefaultTask())
	if err == nil || calls != 2 || result.Requests != 2 || len(result.Accepted) != 0 || len(result.Receipts) != 1 || result.Receipts[0].Status != "rejected" {
		t.Fatalf("host rejection incorrectly recovered: calls=%d receipts=%d err=%v", calls, len(result.Receipts), err)
	}
}
func TestGeminiMalformedRecoveryStopsOnCancellationAfterFailureAccounting(t *testing.T) {
	h := setupHost(t)
	calls := 0
	h.client.Client = &geminiTransport{&transportFunc{scriptedModel: &scriptedModel{}, fn: func(context.Context, *llm.CompletionRequest, []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
		calls++
		return malformedFrames(errors.New(`gemini: incomplete or blocked response (finish="MALFORMED_FUNCTION_CALL")`), false), nil
	}}}
	ctx, cancel := context.WithCancel(budget.WithController(context.Background(), h.controller))
	defer cancel()
	// The hook fires after the failed frame has been accounted and before retry
	// admission. Cancel exactly at this boundary, without sleeping or racing it.
	var failedAttempt usage.ModelEvidence
	ctx = budget.WithUsageHook(ctx, func(_ context.Context, event budget.UsageEvent) { failedAttempt = event.Model; cancel() })
	b, err := h.brief("itinerary", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.execute(ctx, b)
	if !errors.Is(err, context.Canceled) || calls != 1 || failedAttempt.AttemptKey == "" || !failedAttempt.AccountingCommitted {
		t.Fatalf("cancellation did not stop regenerated request: calls=%d err=%v", calls, err)
	}
	snapshot, snapErr := h.controller.Snapshot(context.Background())
	if snapErr != nil {
		t.Fatal(snapErr)
	}
	if snapshot.ReservedRequests != 1 || snapshot.KnownTokens.TotalTokens != 7 {
		t.Fatalf("canceled recovery lost failed attempt: %+v", snapshot)
	}
	if waitErr := h.limiter.wait(testDeadline(t)); waitErr != nil {
		t.Fatal(waitErr)
	}
	if h.created != h.closed {
		t.Fatal("cancellation leaked child")
	}
}
