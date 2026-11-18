package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/kart-io/goagent/core"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
)

// 生成阶段默认的全局上限：历史 90 次重复的被测请求 1,317 次、旧评审约 90 次，留约四成余量。
const (
	defaultGenerationMaxHTTP = 2000
	defaultGenerationMinutes = 90
)

var errGenerationBudget = errors.New("生成预算已用完")

// generationBudget 是一次生成的全局 HTTP 上限，被测客户端与旧评审共用。
// 每个请求先计数再判断，放行的请求总数不超过 limit；过了截止时间不再放行新请求，已发出的请求照常结束。
// 嵌入向量走 Gemini SDK，不经过这里，不计入。
type generationBudget struct {
	limit    int64
	deadline time.Time
	issued   atomic.Int64
	refused  atomic.Int64
}

func newGenerationBudget(limit int64, d time.Duration) *generationBudget {
	return &generationBudget{limit: limit, deadline: time.Now().Add(d)}
}

// generationBudgetFromEnv 读取 CS_SCENARIO_MAX_HTTP 与 CS_SCENARIO_MINUTES，未设置时用默认值。
func generationBudgetFromEnv() (*generationBudget, error) {
	vals := map[string]int64{"CS_SCENARIO_MAX_HTTP": defaultGenerationMaxHTTP, "CS_SCENARIO_MINUTES": defaultGenerationMinutes}
	for name := range vals {
		v := os.Getenv(name)
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%s 应为正整数，实际 %q", name, v)
		}
		vals[name] = n
	}
	return newGenerationBudget(vals["CS_SCENARIO_MAX_HTTP"], time.Duration(vals["CS_SCENARIO_MINUTES"])*time.Minute), nil
}

func (b *generationBudget) take() error {
	if !time.Now().Before(b.deadline) {
		b.refused.Add(1)
		return fmt.Errorf("%w：已到截止时间", errGenerationBudget)
	}
	if b.issued.Add(1) > b.limit {
		b.issued.Add(-1)
		b.refused.Add(1)
		return fmt.Errorf("%w：已放行 %d 次 HTTP", errGenerationBudget, b.limit)
	}
	return nil
}

// exhausted 报告是否已不能再放行请求；新的重复据此跳过，不开始运行。
func (b *generationBudget) exhausted() bool {
	return !time.Now().Before(b.deadline) || b.issued.Load() >= b.limit
}

// cappedClient 在真实客户端之前扣全局预算；refused 只记本客户端被拒的次数，用于判断这一次重复是否受预算影响。
type cappedClient struct {
	llm.ToolStreamClient
	budget  *generationBudget
	refused atomic.Int64
}

func (c *cappedClient) take() error {
	err := c.budget.take()
	if err != nil {
		c.refused.Add(1)
	}
	return err
}

func (c *cappedClient) CompleteStream(ctx context.Context, req *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	if err := c.take(); err != nil {
		return nil, err
	}
	return c.ToolStreamClient.CompleteStream(ctx, req)
}

func (c *cappedClient) CompleteToolsStream(ctx context.Context, req *llm.CompletionRequest, tools []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	if err := c.take(); err != nil {
		return nil, err
	}
	return c.ToolStreamClient.CompleteToolsStream(ctx, req, tools)
}

// StreamMode 透传内层能力，livemodel.Wrap 据此决定流式模式。
func (c *cappedClient) StreamMode() core.StreamMode {
	if s, ok := c.ToolStreamClient.(core.StreamCapability); ok {
		return s.StreamMode()
	}
	return core.StreamModeTokens
}

// generationAccount 是一次生成的调用核算，写入输出目录的 generation.json。
type generationAccount struct {
	MaxHTTP     int64  `json:"max_http"`
	Deadline    string `json:"deadline"`
	HTTPIssued  int64  `json:"http_issued"`
	HTTPRefused int64  `json:"http_refused"`
	SUTCalls    int64  `json:"sut_calls_in_results"`
	JudgeCalls  int64  `json:"old_judge_calls_in_results"`
	Repeats     int    `json:"repeats"`
	BudgetHit   int    `json:"repeats_affected_by_budget"`
	Exhausted   bool   `json:"budget_exhausted"`
	Note        string `json:"note"`
}

func accountGeneration(b *generationBudget, rs []scenarioResult) generationAccount {
	a := generationAccount{MaxHTTP: b.limit, Deadline: b.deadline.Format(time.RFC3339), HTTPIssued: b.issued.Load(),
		HTTPRefused: b.refused.Load(), Repeats: len(rs),
		Note: "http_issued 是被测客户端与旧评审实际放行的请求总数，包含结果写出后仍在后台进行的请求；嵌入向量请求不经过此上限，未计入"}
	for _, r := range rs {
		a.SUTCalls += r.ModelCalls
		a.JudgeCalls += r.JudgeCalls
		if r.Budget != "" {
			a.BudgetHit++
		}
	}
	a.Exhausted = a.HTTPRefused > 0 || a.BudgetHit > 0
	return a
}
