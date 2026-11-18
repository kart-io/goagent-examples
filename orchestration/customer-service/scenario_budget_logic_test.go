package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	shared "github.com/kart-io/goagent/constants"
	"github.com/kart-io/goagent/core"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
)

// countingClient 记录真正到达底层的请求数，代表实际发出的 HTTP。
type countingClient struct{ hits atomic.Int64 }

func (c *countingClient) stream() (<-chan *llm.StreamChunk, error) {
	c.hits.Add(1)
	ch := make(chan *llm.StreamChunk)
	close(ch)
	return ch, nil
}
func (c *countingClient) CompleteStream(context.Context, *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	return c.stream()
}
func (c *countingClient) CompleteToolsStream(context.Context, *llm.CompletionRequest, []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	return c.stream()
}
func (c *countingClient) Provider() shared.Provider   { return shared.Provider("fake") }
func (c *countingClient) StreamMode() core.StreamMode { return core.StreamModeFinal }

func TestGenerationBudgetCap(t *testing.T) {
	tests := []struct {
		name        string
		limit       int64
		after       time.Duration
		sut, judge  int
		wantHits    int64
		wantRefused int64
		exhausted   bool
	}{
		{"未到上限全部放行", 100, time.Hour, 30, 10, 40, 0, false},
		{"恰好到上限", 40, time.Hour, 30, 10, 40, 0, true},
		{"并发超出上限只放行上限数", 25, time.Hour, 30, 10, 25, 15, true},
		{"过了截止时间一律拒绝", 100, -time.Second, 30, 10, 0, 40, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newGenerationBudget(tt.limit, tt.after)
			inner := &countingClient{}
			sut := &cappedClient{ToolStreamClient: inner, budget: b}
			judge := &cappedClient{ToolStreamClient: inner, budget: b}
			sutClient := livemodel.Wrap(sut, livemodel.Config{Provider: "deepseek", Model: "deepseek-flash"})
			judgeClient := livemodel.Wrap(judge, livemodel.Config{Provider: "deepseek", Model: "deepseek-flash"})
			var wg sync.WaitGroup
			for i := range tt.sut + tt.judge {
				wg.Go(func() {
					if i < tt.sut {
						_, _ = sutClient.CompleteToolsStream(context.Background(), &llm.CompletionRequest{}, nil)
						return
					}
					_, _ = llm.Complete(context.Background(), judgeClient, &llm.CompletionRequest{})
				})
			}
			wg.Wait()
			require.Equal(t, tt.wantHits, inner.hits.Load(), "实际发出的请求数")
			require.Equal(t, tt.wantHits, b.issued.Load())
			require.Equal(t, tt.wantRefused, b.refused.Load())
			require.Equal(t, tt.wantRefused, sut.refused.Load()+judge.refused.Load(), "被拒次数按客户端分开记录")
			require.Equal(t, int64(tt.sut+tt.judge), sutClient.Calls()+judgeClient.Calls(), "Calls 统计的是逻辑请求，包括被拒的")
			require.Equal(t, tt.exhausted, b.exhausted())
		})
	}
}

func TestCappedClientKeepsStreamMode(t *testing.T) {
	c := livemodel.Wrap(&cappedClient{ToolStreamClient: &countingClient{}, budget: newGenerationBudget(1, time.Hour)}, livemodel.Config{})
	require.Equal(t, core.StreamModeFinal, c.StreamMode())
}

func TestGenerationBudgetFromEnv(t *testing.T) {
	tests := []struct {
		name, maxHTTP, minutes string
		wantLimit              int64
		errSub                 string
	}{
		{"默认值", "", "", defaultGenerationMaxHTTP, ""},
		{"自定义", "1500", "60", 1500, ""},
		{"非法上限", "0", "", 0, "CS_SCENARIO_MAX_HTTP"},
		{"非法时长", "", "x", 0, "CS_SCENARIO_MINUTES"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CS_SCENARIO_MAX_HTTP", tt.maxHTTP)
			t.Setenv("CS_SCENARIO_MINUTES", tt.minutes)
			b, err := generationBudgetFromEnv()
			if tt.errSub != "" {
				require.ErrorContains(t, err, tt.errSub)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantLimit, b.limit)
		})
	}
}

func TestAccountGeneration(t *testing.T) {
	b := newGenerationBudget(10, time.Hour)
	b.issued.Store(9)
	rs := []scenarioResult{
		{Scenario: "recall", ModelCalls: 5, JudgeCalls: 1, Reply: "答复", JudgeInfra: "超时"},
		{Scenario: "recall", ModelCalls: 3, JudgeCalls: 0, Budget: "未开始：生成预算已用完"},
	}
	a := accountGeneration(b, rs)
	require.Equal(t, int64(8), a.SUTCalls)
	require.Equal(t, int64(1), a.JudgeCalls)
	require.Equal(t, int64(9), a.HTTPIssued)
	require.Equal(t, 1, a.BudgetHit)
	require.True(t, a.Exhausted)

	rows := summarizeScenarios(rs)
	require.Equal(t, 2, rows[0].Infra, "旧评审失败与预算跳过都不进入旧评审通过率分母")
	require.Equal(t, "答复", rs[0].Reply, "旧评审失败不清空答复")
}
