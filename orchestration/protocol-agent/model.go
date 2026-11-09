package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core/usage"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/llm/providers"
)

const liveMarker = "PROTOCOL-LIVE-OK"

type modelEvidence struct {
	Provider string                `json:"provider"`
	Model    string                `json:"model"`
	Usage    interfaces.TokenUsage `json:"usage"`
}

// countedModel 只在真正进入 Provider 时计数，持久回放不会增加计数；不配置自动重试。
type countedModel struct {
	llm.Client
	model string
	limit int64
	calls atomic.Int64
}

func (m *countedModel) CompleteStream(ctx context.Context, req *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for {
		old := m.calls.Load()
		if old >= m.limit {
			return nil, fmt.Errorf("真实模型请求已达到上限 %d", m.limit)
		}
		if m.calls.CompareAndSwap(old, old+1) {
			break
		}
	}
	copy := *req
	copy.Model = m.model
	copy.MaxTokens = 2048
	return m.Client.CompleteStream(ctx, &copy)
}

type modelBackend struct {
	provider, model string
	client          *countedModel
	generate        func(context.Context, any) (*builder.AgentOutput, error)
}

func newLiveModel(provider, model string, maxRequests int64) (*modelBackend, error) {
	if maxRequests < 1 || maxRequests > 20 {
		return nil, fmt.Errorf("max-model-requests 必须在1到20之间")
	}
	if model == "" {
		model = os.Getenv(strings.ToUpper(provider) + "_MODEL")
	}
	var client llm.Client
	var err error
	switch provider {
	case "deepseek":
		if model == "" {
			model = "deepseek-flash"
		}
		client, err = providers.NewDeepSeekWithOptions(llm.WithModel(model), llm.WithMaxTokens(2048), llm.WithTimeout(60*time.Second), llm.WithTemperature(0.2))
	case "gemini":
		if model == "" {
			model = "gemini-2.5-flash"
		}
		client, err = providers.NewGeminiWithOptions(llm.WithModel(model), llm.WithMaxTokens(2048), llm.WithTimeout(60*time.Second), llm.WithTemperature(0.2), llm.WithReasoning(usage.ReasoningConfig{BudgetSet: true, BudgetTokens: 512}))
	default:
		return nil, fmt.Errorf("真实模型案例支持 deepseek 或 gemini")
	}
	if err != nil {
		return nil, fmt.Errorf("模型初始化失败：%s", redactModelError(err.Error()))
	}
	return wrapModel(client, provider, model, maxRequests)
}

func wrapModel(client llm.Client, provider, model string, maxRequests int64) (*modelBackend, error) {
	counted := &countedModel{Client: client, model: model, limit: maxRequests}
	agent, err := builder.NewSimpleBuilder(counted).
		WithSystemPrompt("你是协议互操作验收助手。只根据合成输入，用中文给出一句简短摘要，不执行操作，不声称已真实发布。回答必须包含标记 " + liveMarker + "。").
		WithDurableScope("protocol-live-model").WithMaxIterations(1).WithMaxTokens(2048).WithTimeout(60 * time.Second).Build()
	if err != nil {
		return nil, err
	}
	return &modelBackend{provider: provider, model: model, client: counted, generate: agent.AsRunnable().Invoke}, nil
}

func (m *modelBackend) summarize(ctx context.Context, s reviewState) (reviewState, error) {
	out, err := m.generate(ctx, "这是协议案例合成任务。原始请求："+s.Message+"；人工补充："+s.Reply)
	if err != nil {
		return s, fmt.Errorf("真实模型调用失败：%s", redactModelError(err.Error()))
	}
	answer, ok := out.Result.(string)
	if !ok || !strings.Contains(answer, liveMarker) {
		return s, fmt.Errorf("真实模型结果缺少验收标记")
	}
	if out.TokenUsage == nil || out.TokenUsage.TotalTokens <= 0 {
		return s, fmt.Errorf("真实模型未返回有效token用量，不能作为验收通过")
	}
	s.Result = answer
	s.Model = &modelEvidence{Provider: m.provider, Model: m.model, Usage: *out.TokenUsage}
	return s, nil
}

func redactModelError(value string) string {
	for _, name := range []string{"DEEPSEEK_API_KEY", "GEMINI_API_KEY"} {
		if key := os.Getenv(name); key != "" {
			value = strings.ReplaceAll(value, key, "[REDACTED]")
		}
	}
	return value
}
