// 基础与组合：订单插件与物流插件装进同一个 Agent；两个插件提供同名工具时 Install 报错。
// 默认使用离线模型；GOAGENT_LIVE=1 时使用真实模型（见 internal/livemodel）。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent-examples/orchestration/internal/demomodel"
	"github.com/kart-io/goagent-examples/orchestration/plugin/internal/shop"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/plugin"
)

type result struct {
	Answer       string
	Plugins      []string
	OrderLookups int32
}

// demoModel 第一轮同时调用两个工具，第二轮用工具结果作答。
func demoModel() llm.ToolStreamClient {
	var calls atomic.Int64
	return &demomodel.Model{Calls: &calls, Respond: func(r *llm.CompletionRequest) llm.StreamChunk {
		var results []string
		for _, m := range r.Messages {
			if m.Role == "tool" {
				results = append(results, m.Content)
			}
		}
		if len(results) == 0 {
			return llm.StreamChunk{ToolCalls: []llm.ToolCall{demomodel.Call("c1", "lookup_order"), demomodel.Call("c2", "track_parcel")}}
		}
		return llm.StreamChunk{Content: "订单 A100 已发货；" + strings.Join(results, "；")}
	}}
}

func run(ctx context.Context, model llm.ToolStreamClient) (result, error) {
	var lookups atomic.Int32
	host, err := plugin.NewHost(shop.Orders(&lookups), shop.Shipping())
	if err != nil {
		return result{}, err
	}
	if err := host.Start(ctx); err != nil {
		return result{}, err
	}
	defer func() { _ = host.Stop(context.Background()) }()

	b := builder.NewSimpleBuilder(model).WithSystemPrompt("你是电商客服。").WithMaxIterations(4)
	if err := plugin.Install(ctx, b, host); err != nil {
		return result{}, err
	}
	agent, err := b.Build()
	if err != nil {
		return result{}, err
	}
	out, err := agent.AsRunnable().Invoke(ctx, "我的订单 A100 到哪了？")
	if err != nil {
		return result{}, err
	}
	return result{Answer: fmt.Sprint(out.Result), Plugins: host.Plugins(), OrderLookups: lookups.Load()}, nil
}

// conflict 演示两个插件提供同名工具：Install 在写入 Builder 前报错。
func conflict(ctx context.Context) error {
	host, err := plugin.NewHost(shop.Orders(nil), shop.Static("orders-copy", "1.0.0", plugin.Contribution{
		Tools: []interfaces.Tool{shop.Tool("lookup_order", "另一份订单数据", nil)},
	}))
	if err != nil {
		return err
	}
	if err := host.Start(ctx); err != nil {
		return err
	}
	defer func() { _ = host.Stop(context.Background()) }()
	return plugin.Install(ctx, builder.NewSimpleBuilder(demoModel()), host)
}

func main() {
	ctx := context.Background()
	var model llm.ToolStreamClient = demoModel()
	if os.Getenv("GOAGENT_LIVE") == "1" {
		client, err := livemodel.FromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		model = client
	}
	out, err := run(ctx, model)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("插件：%v\n订单查询次数：%d\n回答：%s\n", out.Plugins, out.OrderLookups, out.Answer)
	fmt.Println("同名工具冲突：", conflict(ctx))
}
