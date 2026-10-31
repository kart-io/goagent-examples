// 钩子类插件：手机号脱敏（PreStep）、限流重试（RequestError）、答案校验续跑（TurnStopping）、审计中间件。
// 默认使用离线模型；GOAGENT_LIVE=1 时使用真实模型（见 internal/livemodel）。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent-examples/orchestration/internal/demomodel"
	"github.com/kart-io/goagent-examples/orchestration/plugin/internal/shop"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core/middleware"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/plugin"
)

const question = "我是 13800138000，订单什么时候到？"

type result struct {
	Answer                      string
	Retries, Continued, Audited int32
}

// flaky 让前 failures 次请求以 503 失败，模拟上游限流。
type flaky struct {
	llm.ToolStreamClient
	failures int32
	n        atomic.Int32
}

func (f *flaky) CompleteToolsStream(ctx context.Context, r *llm.CompletionRequest, ts []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	if f.n.Add(1) <= f.failures {
		return nil, &llm.ProviderError{StatusCode: 503, Err: errors.New("服务暂时不可用")}
	}
	return f.ToolStreamClient.CompleteToolsStream(ctx, r, ts)
}

// demoModel 先调用订单工具，第一次作答故意不给订单号；seen 报告模型是否见过完整手机号。
func demoModel() (llm.ToolStreamClient, func() bool) {
	var calls atomic.Int64
	var sawPhone atomic.Bool
	var answers atomic.Int32
	m := &demomodel.Model{Calls: &calls, Respond: func(r *llm.CompletionRequest) llm.StreamChunk {
		hasTool := false
		for _, msg := range r.Messages {
			if strings.Contains(msg.Content, "13800138000") {
				sawPhone.Store(true)
			}
			hasTool = hasTool || msg.Role == "tool"
		}
		if !hasTool {
			return llm.StreamChunk{ToolCalls: []llm.ToolCall{demomodel.Call("c1", "lookup_order")}}
		}
		if answers.Add(1) == 1 {
			return llm.StreamChunk{Content: "您的订单已发货。"}
		}
		return llm.StreamChunk{Content: "订单 A100 已发货，预计明天送达。"}
	}}
	return m, sawPhone.Load
}

var orderID = regexp.MustCompile(`A\d+`)

type audit struct {
	*middleware.BaseMiddleware
	calls *atomic.Int32
}

func (a *audit) OnBefore(ctx context.Context, r *middleware.MiddlewareRequest) (*middleware.MiddlewareRequest, error) {
	a.calls.Add(1)
	return a.BaseMiddleware.OnBefore(ctx, r)
}

func run(ctx context.Context, model llm.ToolStreamClient) (result, error) {
	var retries, continued, audited atomic.Int32
	retry := builder.RetryTransient(2, func(int) time.Duration { return 10 * time.Millisecond })
	host, err := plugin.NewHost(
		shop.Orders(nil),
		shop.Redact("1.0.0"),
		shop.Static("retry", "1.0.0", plugin.Contribution{
			MaxRecoveries: 2,
			RequestErrors: []builder.RequestErrorHook{func(ctx context.Context, f *builder.RequestFailure) (builder.RequestErrorDecision, error) {
				d, err := retry(ctx, f)
				if d.Action == builder.RequestRetry {
					retries.Add(1)
				}
				return d, err
			}},
		}),
		shop.Static("verify", "1.0.0", plugin.Contribution{
			TurnStopping: []builder.TurnStoppingHook{func(_ context.Context, s *builder.Stopping) (string, error) {
				if orderID.MatchString(s.Answer) {
					return "", nil
				}
				continued.Add(1)
				return "回答中必须给出订单号。", nil
			}},
		}),
		shop.Static("audit", "1.0.0", plugin.Contribution{
			Middleware: []middleware.Middleware{&audit{BaseMiddleware: middleware.NewBaseMiddleware("audit"), calls: &audited}},
		}),
	)
	if err != nil {
		return result{}, err
	}
	if err := host.Start(ctx); err != nil {
		return result{}, err
	}
	defer func() { _ = host.Stop(context.Background()) }()
	b := builder.NewSimpleBuilder(model).WithSystemPrompt("你是电商客服。").WithMaxIterations(6)
	if err := plugin.Install(ctx, b, host); err != nil {
		return result{}, err
	}
	agent, err := b.Build()
	if err != nil {
		return result{}, err
	}
	out, err := agent.AsRunnable().Invoke(ctx, question)
	if err != nil {
		return result{}, err
	}
	return result{Answer: fmt.Sprint(out.Result), Retries: retries.Load(), Continued: continued.Load(), Audited: audited.Load()}, nil
}

func main() {
	ctx := context.Background()
	model, seen := demoModel()
	var client llm.ToolStreamClient = &flaky{ToolStreamClient: model, failures: 1}
	if os.Getenv("GOAGENT_LIVE") == "1" {
		live, err := livemodel.FromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		client, seen = live, func() bool { return false }
	}
	out, err := run(ctx, client)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("重试 %d 次，续跑 %d 次，审计 %d 次，模型见过完整手机号：%v\n回答：%s\n", out.Retries, out.Continued, out.Audited, seen(), out.Answer)
}
