// 资源与依赖：库存插件在 Start 中打开库存并发布为共享资源，定价插件声明依赖并取出使用；
// 审计插件启动失败时，已启动的插件按逆序停止。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent-examples/orchestration/internal/demomodel"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/plugin"
	"github.com/kart-io/goagent/tools"
)

type result struct {
	Answer          string
	Events          []string
	InventoryClosed bool
}

type events struct {
	mu   sync.Mutex
	list []string
}

func (e *events) add(s string) { e.mu.Lock(); defer e.mu.Unlock(); e.list = append(e.list, s) }
func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.list...)
}

// inventory 模拟需要打开与关闭的连接。
type inventory struct {
	stock  map[string]int
	closed atomic.Bool
}

func (i *inventory) Stock(sku string) (int, error) {
	if i.closed.Load() {
		return 0, errors.New("库存连接已关闭")
	}
	return i.stock[sku], nil
}

type inventoryPlugin struct {
	ev  *events
	inv *inventory
}

func (*inventoryPlugin) Name() string    { return "inventory" }
func (*inventoryPlugin) Version() string { return "1.0.0" }
func (p *inventoryPlugin) Start(_ context.Context, env *plugin.Env) error {
	p.inv = &inventory{stock: map[string]int{"SKU-1": 12}}
	p.ev.add("打开库存")
	return env.Provide("inventory.store", p.inv)
}
func (p *inventoryPlugin) Stop(context.Context) error {
	p.inv.closed.Store(true)
	p.ev.add("关闭库存")
	return nil
}
func (p *inventoryPlugin) Contribute(context.Context, *plugin.Env) (plugin.Contribution, error) {
	return plugin.Contribution{Tools: []interfaces.Tool{tools.NewBaseTool("check_stock", "查询 SKU-1 库存", `{"type":"object","properties":{},"additionalProperties":false}`,
		func(context.Context, *interfaces.ToolInput) (*interfaces.ToolOutput, error) {
			n, err := p.inv.Stock("SKU-1")
			if err != nil {
				return nil, err
			}
			return &interfaces.ToolOutput{Success: true, Result: fmt.Sprintf("SKU-1 库存 %d", n)}, nil
		})}}, nil
}

type pricingPlugin struct{ ev *events }

func (*pricingPlugin) Name() string       { return "pricing" }
func (*pricingPlugin) Version() string    { return "1.0.0" }
func (*pricingPlugin) Requires() []string { return []string{"inventory"} }
func (p *pricingPlugin) Start(_ context.Context, env *plugin.Env) error {
	if _, err := plugin.Lookup[*inventory](env.Resources(), "inventory.store"); err != nil {
		return err
	}
	p.ev.add("定价就绪")
	return nil
}
func (p *pricingPlugin) Stop(context.Context) error { p.ev.add("定价停止"); return nil }
func (p *pricingPlugin) Contribute(_ context.Context, env *plugin.Env) (plugin.Contribution, error) {
	inv, err := plugin.Lookup[*inventory](env.Resources(), "inventory.store")
	if err != nil {
		return plugin.Contribution{}, err
	}
	return plugin.Contribution{Tools: []interfaces.Tool{tools.NewBaseTool("quote_price", "按库存给出 SKU-1 报价", `{"type":"object","properties":{},"additionalProperties":false}`,
		func(context.Context, *interfaces.ToolInput) (*interfaces.ToolOutput, error) {
			n, err := inv.Stock("SKU-1")
			if err != nil {
				return nil, err
			}
			price := 99
			if n < 5 {
				price = 129
			}
			return &interfaces.ToolOutput{Success: true, Result: fmt.Sprintf("SKU-1 单价 %d 元", price)}, nil
		})}}, nil
}

// auditLog 依赖定价；fail 为 true 时模拟日志服务不可达。
type auditLog struct{ fail bool }

func (*auditLog) Name() string       { return "audit-log" }
func (*auditLog) Version() string    { return "1.0.0" }
func (*auditLog) Requires() []string { return []string{"pricing"} }
func (a *auditLog) Start(context.Context, *plugin.Env) error {
	if a.fail {
		return errors.New("日志服务不可达")
	}
	return nil
}
func (*auditLog) Stop(context.Context) error { return nil }
func (*auditLog) Contribute(context.Context, *plugin.Env) (plugin.Contribution, error) {
	return plugin.Contribution{}, nil
}

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
			return llm.StreamChunk{ToolCalls: []llm.ToolCall{demomodel.Call("c1", "check_stock"), demomodel.Call("c2", "quote_price")}}
		}
		return llm.StreamChunk{Content: fmt.Sprint(results)}
	}}
}

func run(ctx context.Context, model llm.ToolStreamClient, failAudit bool) (result, error) {
	ev := &events{}
	inv := &inventoryPlugin{ev: ev}
	// 注册顺序与依赖顺序相反，Host 按依赖排序。
	host, err := plugin.NewHost(&auditLog{fail: failAudit}, &pricingPlugin{ev: ev}, inv)
	if err != nil {
		return result{}, err
	}
	if err := host.Start(ctx); err != nil {
		return result{Events: ev.all(), InventoryClosed: inv.inv != nil && inv.inv.closed.Load()}, err
	}
	b := builder.NewSimpleBuilder(model).WithSystemPrompt("你是商品顾问。").WithMaxIterations(4)
	if err := plugin.Install(ctx, b, host); err != nil {
		return result{}, errors.Join(err, host.Stop(context.Background()))
	}
	agent, err := b.Build()
	if err != nil {
		return result{}, errors.Join(err, host.Stop(context.Background()))
	}
	out, err := agent.AsRunnable().Invoke(ctx, "SKU-1 还有货吗，多少钱？")
	// 先停用 Agent（本例中运行已结束），再停止 Host。
	stopErr := host.Stop(context.Background())
	if err = errors.Join(err, stopErr); err != nil {
		return result{}, err
	}
	return result{Answer: fmt.Sprint(out.Result), Events: ev.all(), InventoryClosed: inv.inv.closed.Load()}, nil
}

func main() {
	var model llm.ToolStreamClient = demoModel()
	if os.Getenv("GOAGENT_LIVE") == "1" {
		live, err := livemodel.FromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		model = live
	}
	out, err := run(context.Background(), model, false)
	fmt.Printf("回答：%s\n启停：%v\n错误：%v\n", out.Answer, out.Events, err)
	out, err = run(context.Background(), model, true)
	fmt.Printf("审计插件启动失败时的启停：%v\n错误：%v\n", out.Events, err)
}
