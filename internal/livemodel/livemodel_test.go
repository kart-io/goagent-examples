package livemodel

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/kart-io/goagent/core"
	"github.com/kart-io/goagent/core/usage"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"

	shared "github.com/kart-io/goagent/constants"
	"github.com/stretchr/testify/require"
)

func TestConfigFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name, provider, deepseekModel, minimaxModel string
		want                                        Config
		ok                                          bool
	}{
		{"默认 deepseek", "", "", "", Config{"deepseek", "deepseek-flash"}, true},
		{"大小写与空白", "  MiniMax ", "", "", Config{"minimax", "MiniMax-M3.1-Flash"}, true},
		{"覆盖 deepseek 模型", "deepseek", "deepseek-pro", "", Config{"deepseek", "deepseek-pro"}, true},
		{"覆盖 minimax 模型", "minimax", "ignored", "MiniMax-M3", Config{"minimax", "MiniMax-M3"}, true},
		{"gemini 默认模型", "gemini", "", "", Config{"gemini", "gemini-2.5-flash"}, true},
		{"不支持的提供方", "openai", "", "", Config{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOAGENT_LIVE_PROVIDER", tc.provider)
			t.Setenv("DEEPSEEK_MODEL", tc.deepseekModel)
			t.Setenv("MINIMAX_MODEL", tc.minimaxModel)
			got, err := ConfigFromEnv()
			if !tc.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestMiniMaxRequiresKey(t *testing.T) {
	t.Setenv("MINIMAX_API_KEY", "")
	t.Setenv("MINIMAX_CN_API_KEY", "")
	_, err := New(Config{Provider: "minimax", Model: "m"})
	require.ErrorContains(t, err, "MINIMAX")
	t.Setenv("MINIMAX_API_KEY", "k")
	t.Setenv("MINIMAX_BASE_URL", "https://user:pw@example.com/v1")
	_, err = New(Config{Provider: "minimax", Model: "m"})
	require.ErrorContains(t, err, "MINIMAX_BASE_URL")
}

func TestRedact(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-secret-123")
	t.Setenv("GEMINI_API_KEY", "AIza-secret-456")
	require.Equal(t, "bad key [REDACTED] and [REDACTED]", Redact("bad key sk-secret-123 and AIza-secret-456"))
}

// recordingClient 记录收到的请求，并返回一个完成块。
type recordingClient struct {
	calls atomic.Int64
	seen  []*llm.CompletionRequest
}

func (r *recordingClient) Provider() shared.Provider { return "fake" }
func (r *recordingClient) CompleteStream(_ context.Context, req *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	r.calls.Add(1)
	r.seen = append(r.seen, req)
	ch := make(chan *llm.StreamChunk, 1)
	ch <- &llm.StreamChunk{Content: "ok", Done: true}
	close(ch)
	return ch, nil
}
func (r *recordingClient) CompleteToolsStream(ctx context.Context, req *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	return r.CompleteStream(ctx, req)
}

func TestWrapCountsAndFillsModel(t *testing.T) {
	inner := &recordingClient{}
	c := Wrap(inner, Config{Provider: "fake", Model: "m1"})
	require.Equal(t, core.StreamModeTokens, c.StreamMode(), "未声明能力时按逐 token 处理")
	req := &llm.CompletionRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}
	for _, call := range []func() (<-chan *llm.StreamChunk, error){
		func() (<-chan *llm.StreamChunk, error) { return c.CompleteStream(context.Background(), req) },
		func() (<-chan *llm.StreamChunk, error) { return c.CompleteToolsStream(context.Background(), req, nil) },
	} {
		ch, err := call()
		require.NoError(t, err)
		for range ch {
		}
	}
	require.Equal(t, int64(2), c.Calls())
	require.Equal(t, int64(2), inner.calls.Load())
	for _, r := range inner.seen {
		require.Equal(t, "m1", r.Model)
		require.Equal(t, MaxTokens, r.MaxTokens)
	}
	require.Empty(t, req.Model, "不修改调用方请求")
	explicit := &llm.CompletionRequest{Model: "m2", MaxTokens: 7, Messages: req.Messages}
	ch, err := c.CompleteStream(context.Background(), explicit)
	require.NoError(t, err)
	for range ch {
	}
	last := inner.seen[len(inner.seen)-1]
	require.Equal(t, "m2", last.Model)
	require.Equal(t, 7, last.MaxTokens)
}

func TestWrapKeepsDeclaredStreamMode(t *testing.T) {
	m := &minimax{}
	require.Equal(t, core.StreamModeFinal, Wrap(m, Config{}).StreamMode())
}

// Gemini 的思考 token 计入输出上限；不限制思考预算时，输出会以 MAX_TOKENS 截断。
func TestGeminiCapsThinkingBudget(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")
	c, err := New(Config{Provider: "gemini", Model: "gemini-2.5-flash"})
	require.NoError(t, err)
	p, ok := c.ToolStreamClient.(interface {
		ResolveReasoning(*llm.CompletionRequest) *usage.ReasoningConfig
	})
	require.True(t, ok)
	r := p.ResolveReasoning(&llm.CompletionRequest{})
	require.NotNil(t, r)
	require.Equal(t, usage.ReasoningConfig{BudgetTokens: GeminiThinkingBudget, BudgetSet: true}, *r)
	require.Less(t, GeminiThinkingBudget, MaxTokens)
}
