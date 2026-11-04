package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/llm/constants"
	"github.com/stretchr/testify/require"
)

type completionFunc func(context.Context, *llm.CompletionRequest) (*llm.CompletionResponse, error)

func (f completionFunc) CompleteStream(ctx context.Context, req *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	response, err := f(ctx, req)
	if err != nil {
		return nil, err
	}
	stream := make(chan *llm.StreamChunk, 1)
	if response != nil {
		final := &llm.StreamChunk{Content: response.Content, FinishReason: response.FinishReason, Done: true}
		if response.Usage != nil {
			final.Usage = &llm.Usage{PromptTokens: response.Usage.PromptTokens, CompletionTokens: response.Usage.CompletionTokens, TotalTokens: response.Usage.TotalTokens}
		}
		stream <- final
	}
	close(stream)
	return stream, nil
}
func (completionFunc) Provider() constants.Provider { return constants.ProviderCustom }

type decisionStreamFunc func(context.Context, *llm.CompletionRequest) (<-chan *llm.StreamChunk, error)

func (f decisionStreamFunc) CompleteStream(ctx context.Context, req *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	return f(ctx, req)
}
func (decisionStreamFunc) Provider() constants.Provider { return constants.ProviderCustom }

// 完整 JSON 也不能使缺少成功终态或带终态错误的流触发业务副作用。
func TestModelDecisionRequiresSuccessfulStreamTermination(t *testing.T) {
	const answer = `{"decision":"reply","reason":"FAQ match","source":"demo-faq-v1/hours"}`
	for _, mode := range []string{"success", "missing_done", "terminal_error"} {
		t.Run(mode, func(t *testing.T) {
			store, b := testStores(t)
			b.model = &modelJudge{provider: "fake", model: "stream", client: decisionStreamFunc(func(ctx context.Context, req *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
				require.Equal(t, "stream", req.Model)
				require.Len(t, req.Messages, 2)
				require.Equal(t, "system", req.Messages[0].Role)
				require.Equal(t, "user", req.Messages[1].Role)
				chunks := make(chan *llm.StreamChunk, 3)
				chunks <- &llm.StreamChunk{Delta: answer[:20]}
				chunks <- &llm.StreamChunk{Delta: answer[20:]}
				if mode != "missing_done" {
					final := &llm.StreamChunk{Done: true, FinishReason: "stop", Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30}}
					if mode == "terminal_error" {
						final.Error = errors.New("终态失败")
					}
					chunks <- final
				}
				close(chunks)
				return chunks, nil
			})}
			g, err := replyGraph(context.Background(), store, b)
			require.NoError(t, err)
			snap, err := g.Run(context.Background(), "stream", replyState{MessageID: "stream", Recipient: "demo-user", Message: "几点营业？"}, "")
			if mode == "success" {
				require.NoError(t, err)
				require.True(t, snap.Done)
				require.Equal(t, "reply", snap.State.ModelReview.Decision)
				require.Equal(t, 30, snap.State.ModelReview.Usage.TotalTokens)
				require.Equal(t, "demo-faq-v1/hours", snap.State.Source)
			} else {
				require.Error(t, err)
				noEffect(t, b, "send_reply", "stream")
			}
			require.EqualValues(t, 1, b.model.calls.Load())
		})
	}
}

func fakeJudge(content string) *modelJudge {
	return &modelJudge{provider: "fake", model: "test-model", client: completionFunc(func(context.Context, *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		return &llm.CompletionResponse{Content: content, FinishReason: "stop"}, nil
	})}
}

func TestModelDecisionValidation(t *testing.T) {
	for _, content := range []string{
		"```json\n{}\n```", `null`, `{}`, `{"decision":"handoff","reason":"r"}`,
		`{"decision":"handoff","reason":"r","source":null}`,
		`{"decision":"handoff","reason":"r","source":"","command":"send"}`,
		`{"decision":"handoff","reason":"r","source":""} {}`,
		`{"decision":"restart","reason":"r","source":""}`,
		`{"decision":"reply","reason":"r","source":"invented"}`,
		`{"decision":"handoff","reason":" ","source":""}`,
		`{"decision":"handoff","reason":"r","source":"demo-faq-v1/hours"}`,
	} {
		_, err := decodeDecision("auto-reply", content)
		require.Error(t, err, content)
	}
	_, err := decodeDecision("auto-reply", `{"decision":"reply","reason":"matching FAQ","source":"demo-faq-v1/hours"}`)
	require.NoError(t, err)
}

func TestLiveModelCannotBypassDevOpsPolicy(t *testing.T) {
	ctx := context.Background()
	store, b := testStores(t)
	b.model = fakeJudge(`{"decision":"restart","reason":"incorrect recommendation","source":""}`)
	g, err := devopsGraph(ctx, store, b)
	require.NoError(t, err)
	s, err := g.Run(ctx, "r", devopsState{IncidentID: "i", Service: "demo-api", Healthy: true}, "")
	require.NoError(t, err)
	require.True(t, s.Done)
	require.Equal(t, "escalated", s.State.Outcome)
	noEffect(t, b, "restart_service", "i")
}

func TestLiveModelCannotBypassArticleBlock(t *testing.T) {
	ctx := context.Background()
	store, b := testStores(t)
	b.model = fakeJudge(`{"decision":"accept","reason":"incorrect recommendation","source":""}`)
	g, err := articleGraph(ctx, store, b)
	require.NoError(t, err)
	s, err := g.Run(ctx, "r", articleState{ArticleID: "a", Title: "DEMO_BLOCK", Body: strings.Repeat("文", 80)}, "")
	require.NoError(t, err)
	require.Equal(t, "rejected", s.State.Decision)
	noEffect(t, b, "publish_article", "a")
}

func TestLiveModelFailureProducesNoEffect(t *testing.T) {
	for _, mode := range []string{"network", "invalid", "truncated", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store, b := testStores(t)
			b.model = &modelJudge{provider: "fake", model: "test-model", client: completionFunc(func(ctx context.Context, _ *llm.CompletionRequest) (*llm.CompletionResponse, error) {
				switch mode {
				case "network":
					return nil, errors.New("provider unavailable")
				case "invalid":
					return &llm.CompletionResponse{Content: "not JSON", FinishReason: "stop"}, nil
				case "canceled":
					return nil, context.Canceled
				default:
					return &llm.CompletionResponse{Content: `{}`, FinishReason: "length"}, nil
				}
			})}
			g, err := replyGraph(ctx, store, b)
			require.NoError(t, err)
			_, err = g.Run(ctx, "r", replyState{MessageID: "m", Recipient: "demo-user", Message: "几点营业？"}, "")
			require.Error(t, err)
			noEffect(t, b, "send_reply", "m")
			require.EqualValues(t, 1, b.model.calls.Load())
		})
	}
}

func TestLiveDecisionCheckpointPreventsRepeatedModelCall(t *testing.T) {
	ctx := context.Background()
	store, b := testStores(t)
	b.model = fakeJudge(`{"decision":"reply","reason":"hours paraphrase","source":"demo-faq-v1/hours"}`)
	b.after = func() error { return errors.New("ambiguous send") }
	g, err := replyGraph(ctx, store, b)
	require.NoError(t, err)
	_, err = g.Run(ctx, "r", replyState{MessageID: "m", Recipient: "demo-user", Message: "几点开门？"}, "")
	require.ErrorContains(t, err, "ambiguous send")
	first := readEffect(t, b, "send_reply", "m")
	b.after = nil
	g, err = replyGraph(ctx, store, b)
	require.NoError(t, err)
	s, err := g.Run(ctx, "r", replyState{}, "")
	require.NoError(t, err)
	require.EqualValues(t, 1, b.model.calls.Load())
	require.Equal(t, first.Receipt, s.State.Receipt)
	require.Equal(t, "demo-faq-v1/hours", s.State.Source)
	require.Equal(t, "reply", s.State.ModelReview.Decision)
	// Offline or changed model cannot silently resume the live plan.
	b.model = nil
	offline, err := replyGraph(ctx, store, b)
	require.NoError(t, err)
	_, err = offline.Run(ctx, "r", replyState{}, "")
	require.Error(t, err)
}

func TestModelBudgetAndInputBound(t *testing.T) {
	m := fakeJudge(`{"decision":"handoff","reason":"unknown","source":""}`)
	_, err := m.judge(context.Background(), "auto-reply", strings.Repeat("x", 16001))
	require.Error(t, err)
	require.Zero(t, m.calls.Load())
	_, err = m.judge(context.Background(), "auto-reply", "question")
	require.NoError(t, err)
	_, err = m.judge(context.Background(), "auto-reply", "question")
	require.ErrorContains(t, err, "budget")
	require.EqualValues(t, 1, m.calls.Load())
}

func TestFailedDecisionPersistsUsageAndStopsPaidReplay(t *testing.T) {
	ctx := context.Background()
	store, b := testStores(t)
	b.model = &modelJudge{provider: "fake", model: "audit", client: completionFunc(func(context.Context, *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		return &llm.CompletionResponse{Content: "invalid-json", FinishReason: "stop", Usage: &interfaces.TokenUsage{TotalTokens: 73}}, nil
	})}
	graph, err := replyGraph(ctx, store, b)
	require.NoError(t, err)
	_, err = graph.Run(ctx, "failed", replyState{MessageID: "failed", Recipient: "demo-user", Message: "hours?"}, "")
	require.Error(t, err)
	require.Equal(t, "needs_attention", b.model.last.Load().Status)
	require.Equal(t, 73, b.model.last.Load().Usage.TotalTokens)
	require.NotEmpty(t, b.model.last.Load().ResponseHash)
	// A new model instance represents a restarted process. Its client must never run.
	b.model = &modelJudge{provider: "fake", model: "audit", client: completionFunc(func(context.Context, *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		t.Error("paid retry after persisted failure")
		return nil, errors.New("unexpected call")
	})}
	graph, err = replyGraph(ctx, store, b)
	require.NoError(t, err)
	_, err = graph.Run(ctx, "failed", replyState{}, "")
	require.ErrorIs(t, err, durable.ErrOperationAttention)
	require.Zero(t, b.model.calls.Load())
	require.Equal(t, 73, b.model.last.Load().Usage.TotalTokens)
	noEffect(t, b, "send_reply", "failed")
}
