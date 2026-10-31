// 会话日志与重放：客服 Agent 查订单物流，对话、轮次与持久操作的快照记在同一份会话日志中。
// 进程在工具执行中退出后以同一执行键续写该轮；下一轮的历史从日志推导；最后把记录的运行当作回归测试重放：
// 全部回放、只让工具实跑（检查改过的工具是否改变结果）、从某次模型请求切开用当前模型重答。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync/atomic"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent-examples/orchestration/internal/livecase"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/core/session"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/testing/replay"
	"github.com/kart-io/goagent/tools"
)

// SessionID 是案例唯一的会话；执行键由它与轮次组成。
const SessionID = "support/C-1001"

// Order 是合成的订单物流状态。
type Order struct {
	ID      string `json:"order_id"`
	Status  string `json:"status"`
	Carrier string `json:"carrier,omitempty"`
}

// Orders 是记录时工具使用的数据。
var Orders = map[string]Order{
	"A100": {ID: "A100", Status: "已发货", Carrier: "顺丰 SF1234567"},
	"B200": {ID: "B200", Status: "待发货"},
}

// Questions 是两轮用户输入；第二轮依赖第一轮的上下文。
var Questions = []string{"我的订单 A100 到哪了？", "那 B200 呢？"}

// Statuses 是结论允许的取值。
var Statuses = []string{"已发货", "待发货", "已签收"}

const systemPrompt = `你是电商客服助手。回答订单物流问题前必须先调用 lookup_order 查询，只依据查询结果回答，不要猜测。
第一行只能是“结论：已发货”“结论：待发货”或“结论：已签收”，第二行一句说明。`

const orderSchema = `{"type":"object","properties":{"order_id":{"type":"string","description":"订单号，如 A100"}},"required":["order_id"]}`

// Hooks 用于测试注入：AfterLookup 在工具查到结果、工具检查点保存之前调用。
type Hooks struct{ AfterLookup func() }

func lookupTool(orders map[string]Order, runs *atomic.Int64, hooks Hooks) interfaces.Tool {
	return tools.NewBaseTool("lookup_order", "按订单号查询物流状态", orderSchema,
		func(_ context.Context, in *interfaces.ToolInput) (*interfaces.ToolOutput, error) {
			runs.Add(1)
			id, _ := in.Args["order_id"].(string)
			order, ok := orders[id]
			if hooks.AfterLookup != nil {
				hooks.AfterLookup()
			}
			if !ok {
				return &interfaces.ToolOutput{Success: false, Error: "订单不存在：" + id}, nil
			}
			data, err := json.Marshal(order)
			if err != nil {
				return nil, err
			}
			return &interfaces.ToolOutput{Success: true, Result: string(data)}, nil
		})
}

// watch 记下本次运行第一次真实模型请求的消息数，用来确认历史来自会话日志。
type watch struct {
	llm.ToolStreamClient
	first atomic.Int64
}

func (w *watch) CompleteToolsStream(ctx context.Context, req *llm.CompletionRequest, ts []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	w.first.CompareAndSwap(0, int64(len(req.Messages)))
	return w.ToolStreamClient.CompleteToolsStream(ctx, req, ts)
}

func newAgent(client llm.ToolStreamClient, log session.Store, tool interfaces.Tool) (*builder.ConfigurableAgent[any, *core.AgentState], error) {
	dispatched, err := livecase.DispatchTools([]interfaces.Tool{tool})
	if err != nil {
		return nil, err
	}
	return builder.NewSimpleBuilder(client).WithSystemPrompt(systemPrompt).WithSessionID(SessionID).WithSessionLog(log).
		WithDurableScope("support").WithTools(dispatched...).WithMaxIterations(4).Build()
}

// TurnResult 汇总一轮的运行。
type TurnResult struct {
	Answer, Conclusion string
	NewModelCalls      int64
	ToolRuns           int64
	// FirstRequestMessages 是本次第一次真实模型请求的消息数（含 system），0 表示没有请求模型。
	FirstRequestMessages int64
}

func run(ctx context.Context, client *livemodel.Client, log session.Store, tool interfaces.Tool, runs *atomic.Int64, input string) (TurnResult, error) {
	var out TurnResult
	w := &watch{ToolStreamClient: client}
	agent, err := newAgent(w, log, tool)
	if err != nil {
		return out, err
	}
	before := client.Calls()
	res, err := agent.AsRunnable().Invoke(ctx, input)
	out.NewModelCalls, out.ToolRuns, out.FirstRequestMessages = client.Calls()-before, runs.Load(), w.first.Load()
	if err != nil {
		return out, err
	}
	out.Answer = fmt.Sprint(res.Result)
	out.Conclusion, err = livecase.Decision(out.Answer, Statuses...)
	return out, err
}

// Ask 持久运行第 turn 轮（从 0 开始）。同一目录、同一轮再次调用时从日志恢复：已完成的模型回合与工具调用不重跑。
func Ask(ctx context.Context, client *livemodel.Client, dir string, turn int, hooks Hooks) (TurnResult, error) {
	if turn < 0 || turn >= len(Questions) {
		return TurnResult{}, fmt.Errorf("轮次 %d 超出范围", turn)
	}
	store, err := session.NewFileStore(dir)
	if err != nil {
		return TurnResult{}, err
	}
	ctx = durable.WithJournal(durable.WithExecutionKey(ctx, SessionID+"/turn/"+strconv.Itoa(turn)), session.DurableStore(store, SessionID))
	var runs atomic.Int64
	return run(ctx, client, store, lookupTool(Orders, &runs, hooks), &runs, Questions[turn])
}

// ReplayOptions 选择重放的轮次与实跑的边界。
type ReplayOptions struct {
	Turn int
	// Orders 是重放时工具使用的数据，nil 表示与记录时相同；用它模拟改过的工具实现。
	Orders    map[string]Order
	Cut, Live func(replay.Boundary) bool
}

// ReplayResult 是一次重放的结果与各边界的处理。
type ReplayResult struct {
	TurnResult
	Boundaries []replay.Result
	Diverged   *replay.Result `json:",omitempty"`
}

// Replay 用当前代码重放日志中第 opts.Turn 个已结束的轮次，只读原日志。
func Replay(ctx context.Context, client *livemodel.Client, dir string, opts ReplayOptions) (ReplayResult, error) {
	var out ReplayResult
	store, err := session.NewFileStore(dir)
	if err != nil {
		return out, err
	}
	rp := &replay.Replayer{Source: store, SessionID: SessionID, Cut: opts.Cut, Live: opts.Live}
	turns, err := rp.Turns(ctx)
	if err != nil {
		return out, err
	}
	if opts.Turn < 0 || opts.Turn >= len(turns) {
		return out, fmt.Errorf("日志中只有 %d 个已结束的轮次", len(turns))
	}
	ctx, log, report, err := rp.Prepare(ctx, turns[opts.Turn])
	if err != nil {
		return out, err
	}
	orders := opts.Orders
	if orders == nil {
		orders = Orders
	}
	var runs atomic.Int64
	out.TurnResult, err = run(ctx, client, log, lookupTool(orders, &runs, Hooks{}), &runs, turns[opts.Turn].Input)
	out.Boundaries = report.Results()
	if d, ok := report.Diverged(); ok {
		out.Diverged = &d
	}
	return out, err
}

// Export 把会话导出为 OpenAI 微调格式的一行 JSONL。
func Export(ctx context.Context, dir string, w io.Writer) (*session.Trajectory, error) {
	store, err := session.NewFileStore(dir)
	if err != nil {
		return nil, err
	}
	events, err := store.Read(ctx, SessionID, 0)
	if err != nil {
		return nil, err
	}
	traj, err := session.BuildTrajectory(events)
	if err != nil {
		return nil, err
	}
	return traj, traj.WriteOpenAI(w)
}

// Delivered 模拟改过的工具：A100 已签收。
func Delivered() map[string]Order {
	orders := map[string]Order{}
	for id, o := range Orders {
		orders[id] = o
	}
	a := orders["A100"]
	a.Status = "已签收"
	orders["A100"] = a
	return orders
}

func main() {
	dir := flag.String("dir", "", "会话日志目录")
	ask := flag.Int("ask", -1, "持久运行第 N 轮（0 或 1）")
	replayTurn := flag.Int("replay", -1, "重放日志中第 N 个已结束的轮次")
	liveTools := flag.Bool("live-tools", false, "重放时工具实跑")
	delivered := flag.Bool("delivered", false, "重放时使用改过的工具数据（A100 已签收）")
	cutModel := flag.Int("cut-model", -1, "重放时从第 N 次模型请求起实跑")
	export := flag.String("export", "", "把会话导出为 OpenAI 格式 JSONL 文件")
	flag.Parse()
	if *dir == "" || (*ask < 0 && *replayTurn < 0 && *export == "") {
		fmt.Fprintln(os.Stderr, "用法：session-replay -dir <目录> (-ask N | -replay N [-live-tools] [-delivered] [-cut-model N] | -export <文件>)")
		os.Exit(2)
	}
	if *export != "" {
		f, err := os.Create(*export)
		if err == nil {
			_, err = Export(context.Background(), *dir, f)
			err = errors.Join(err, f.Close())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	client, err := livemodel.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var out any
	if *ask >= 0 {
		out, err = Ask(context.Background(), client, *dir, *ask, Hooks{})
	} else {
		opts := ReplayOptions{Turn: *replayTurn}
		if *liveTools {
			opts.Live = replay.Kind("tool")
		}
		if *delivered {
			opts.Orders = Delivered()
		}
		if *cutModel >= 0 {
			opts.Cut = replay.ModelTurn(*cutModel)
		}
		out, err = Replay(context.Background(), client, *dir, opts)
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(data))
	if err != nil {
		fmt.Fprintln(os.Stderr, livemodel.Redact(err.Error()))
		os.Exit(1)
	}
}
