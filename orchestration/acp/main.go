// ACP Agent：以 Agent Client Protocol 经 stdio 运行，可在 Zed 等编辑器中使用。
// 提供读文件与执行命令两个工具，执行命令前经编辑器审批。默认使用离线模型；GOAGENT_LIVE=1 时使用真实模型。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/kart-io/goagent-contrib/acp"
	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent-examples/orchestration/internal/demomodel"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/harness/env"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/tools"
)

// demoModel 先读 README，再执行 ls，最后用读到的内容作答。
func demoModel() llm.ToolStreamClient { return scriptedModel(false, nil) }

// scriptedModel 的 badFirst 为真时先以不合法参数调用一次，用于验证参数错误交还模型；
// onRequest 非空时收到每次请求中的 tool 消息。
func scriptedModel(badFirst bool, onRequest func(toolMessages []string)) llm.ToolStreamClient {
	var calls atomic.Int64
	return &demomodel.Model{Calls: &calls, Respond: func(r *llm.CompletionRequest) llm.StreamChunk {
		var results []string
		for _, m := range r.Messages {
			if m.Role == "tool" {
				results = append(results, m.Content)
			}
		}
		if onRequest != nil {
			onRequest(results)
		}
		step := len(results)
		if badFirst {
			if step == 0 {
				c := demomodel.Call("c0", "run_command")
				c.Function.Arguments = `{"argv":"ls"}`
				return llm.StreamChunk{ToolCalls: []llm.ToolCall{c}}
			}
			step--
			results = results[1:]
		}
		switch step {
		case 0:
			c := demomodel.Call("c1", "read_file")
			c.Function.Arguments = `{"path":"README.md"}`
			return llm.StreamChunk{ToolCalls: []llm.ToolCall{c}}
		case 1:
			c := demomodel.Call("c2", "run_command")
			c.Function.Arguments = `{"argv":["ls"]}`
			return llm.StreamChunk{ToolCalls: []llm.ToolCall{c}}
		default:
			answer := "项目说明：" + strings.TrimSpace(results[0])
			return llm.StreamChunk{Content: answer, Delta: answer}
		}
	}}
}

func readFile(fs env.FileSystem) interfaces.Tool {
	return tools.NewBaseTool("read_file", "读取工作区内的文本文件", `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`,
		func(ctx context.Context, in *interfaces.ToolInput) (*interfaces.ToolOutput, error) {
			path, _ := in.Args["path"].(string)
			data, _, err := fs.ReadFile(ctx, path)
			if err != nil {
				return tools.Rejected(err.Error(), nil), nil
			}
			return &interfaces.ToolOutput{Success: true, Result: string(data)}, nil
		})
}

func runCommand(exec env.Executor) interfaces.Tool {
	return tools.NewBaseTool("run_command", "在工作区内执行命令（不经 shell）", `{"type":"object","properties":{"argv":{"type":"array","items":{"type":"string"},"minItems":1}},"required":["argv"]}`,
		func(ctx context.Context, in *interfaces.ToolInput) (*interfaces.ToolOutput, error) {
			raw, _ := in.Args["argv"].([]any)
			argv := make([]string, 0, len(raw))
			for _, a := range raw {
				argv = append(argv, fmt.Sprint(a))
			}
			if len(argv) == 0 {
				return tools.Rejected("argv 不能为空", nil), nil
			}
			res, err := exec.Run(ctx, env.Command{Argv: argv})
			if err != nil {
				return nil, err
			}
			return &interfaces.ToolOutput{Success: true, Result: fmt.Sprintf("退出码 %d\n%s", res.ExitCode, res.Output)}, nil
		})
}

// askBeforeCommand 要求执行命令前审批，本会话允许放行全部命令。
func askBeforeCommand(_ context.Context, c *tools.Call) (tools.PreDecision, error) {
	if c.Tool != "run_command" {
		return tools.PreDecision{}, nil
	}
	return tools.PreDecision{Kind: tools.PreAsk, Feedback: "将在工作区执行命令", Grant: commandGrant{}}, nil
}

type commandGrant struct{}

func (commandGrant) Describe() string           { return "本会话内的全部命令" }
func (commandGrant) Matches(c *tools.Call) bool { return c.Tool == "run_command" }

func newServer(model llm.ToolStreamClient) *acp.Server {
	kinds := map[string]acpsdk.ToolKind{"read_file": acpsdk.ToolKindRead, "run_command": acpsdk.ToolKindExecute}
	return acp.NewServer(func(_ context.Context, s *acp.Session) (acp.Runner, error) {
		p := s.Pipeline()
		p.Pre = append(p.Pre, askBeforeCommand)
		reg := tools.NewRegistry()
		for _, t := range []interfaces.Tool{readFile(s.FileSystem()), runCommand(s.Executor())} {
			if err := reg.Register(t); err != nil {
				return nil, err
			}
		}
		d, err := tools.NewDispatcher(reg, tools.DispatchConfig{Pipeline: p, ReturnArgumentErrors: true})
		if err != nil {
			return nil, err
		}
		agent, err := builder.NewSimpleBuilder(model).
			WithSystemPrompt("你是编码助手，工作区为 " + s.Cwd() + "。").
			WithSessionID(s.ID()).
			WithTools(d.Tools()...).
			WithMaxIterations(6).
			Build()
		if err != nil {
			return nil, err
		}
		return agent, nil
	}, acp.WithTerminalCapabilities(env.Capabilities{Network: true}), acp.WithAgentInfo("goagent-acp-example", "0.1.0"), acp.WithToolKind(func(tool string) acpsdk.ToolKind {
		if k, ok := kinds[tool]; ok {
			return k
		}
		return acpsdk.ToolKindOther
	}))
}

func main() {
	model := demoModel()
	if os.Getenv("GOAGENT_LIVE") == "1" {
		client, err := livemodel.FromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		model = client
	}
	if err := newServer(model).Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
