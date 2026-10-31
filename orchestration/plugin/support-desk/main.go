// 客服端到端：订单、退款政策与脱敏插件组成持久运行的客服 Agent。进程在工具结果记入日志后、下一次模型请求前崩溃，
// 重启后回放已完成的模型回合与工具结果；脱敏插件升级版本后，旧运行不能恢复。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/kart-io/goagent-examples/orchestration/internal/demomodel"
	"github.com/kart-io/goagent-examples/orchestration/plugin/internal/shop"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/plugin"
	"github.com/kart-io/goagent/tools"
)

const crashCode = 3

type options struct {
	Dir, RedactVersion  string
	CrashAfterFirstStep bool
}

type result struct {
	Answer       string
	ModelCalls   int64
	OrderLookups int32
}

// crashProbe 的 PreStep 身份固定，两次运行的意图一致；crash 为真时在第二次模型请求前（工具结果已记入日志）退出。
// 模型请求已发出而结果未落盘会停在 pending/ambiguous 而不再重发，所以崩溃点只能放在请求之前。
func crashProbe(crash bool) plugin.Plugin {
	return shop.Static("crash-probe", "1.0.0", plugin.Contribution{
		PreSteps: []plugin.PreStep{{Identity: "before-answer", Hook: func(_ context.Context, s *builder.Step) (builder.PreStepResult, error) {
			if crash && s.Index > 0 {
				os.Exit(crashCode)
			}
			return builder.PreStepResult{}, nil
		}}},
	})
}

func run(ctx context.Context, opts options) (result, error) {
	store, err := durable.NewFileStore(opts.Dir)
	if err != nil {
		return result{}, err
	}
	ctx = durable.WithJournal(durable.WithExecutionKey(ctx, "ticket/1001"), store)

	var lookups atomic.Int32
	host, err := plugin.NewHost(shop.Orders(&lookups), shop.RefundPolicy(), shop.Redact(opts.RedactVersion), crashProbe(opts.CrashAfterFirstStep))
	if err != nil {
		return result{}, err
	}
	if err := host.Start(ctx); err != nil {
		return result{}, err
	}
	defer func() { _ = host.Stop(context.Background()) }()

	var calls atomic.Int64
	model := &demomodel.Model{Calls: &calls, Respond: func(r *llm.CompletionRequest) llm.StreamChunk {
		var evidence string
		for _, m := range r.Messages {
			if m.Role == "tool" {
				evidence = m.Content
			}
		}
		if evidence == "" {
			return llm.StreamChunk{ToolCalls: []llm.ToolCall{demomodel.Call("c1", "lookup_order")}}
		}
		return llm.StreamChunk{Content: "订单 A100 签收 5 天，符合 7 天无理由退款。" + strings.TrimSpace(evidence)}
	}}
	b := builder.NewSimpleBuilder(model).WithSystemPrompt("你是电商客服。").WithDurableScope("support-desk").WithMaxIterations(4)
	if err := plugin.Install(ctx, b, host, plugin.Dispatch(tools.DispatchConfig{MaxConcurrency: 1})); err != nil {
		return result{}, err
	}
	agent, err := b.Build()
	if err != nil {
		return result{}, err
	}
	out, err := agent.AsRunnable().Invoke(ctx, "我是 13800138000，订单 A100 想退款")
	if err != nil {
		return result{}, err
	}
	return result{Answer: fmt.Sprint(out.Result), ModelCalls: calls.Load(), OrderLookups: lookups.Load()}, nil
}

func main() {
	dir := "support-desk-journal"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	out, err := run(context.Background(), options{Dir: dir, RedactVersion: "1.0.0"})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("回答：%s\n本进程模型请求 %d 次，订单查询 %d 次（重复运行同一目录时均为 0）\n", out.Answer, out.ModelCalls, out.OrderLookups)
}
