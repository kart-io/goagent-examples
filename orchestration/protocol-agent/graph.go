package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/kart-io/goagent/codec/json"
	"github.com/kart-io/goagent/constants"
	"github.com/kart-io/goagent/core/durable"
	mg "github.com/kart-io/goagent/management"
	"github.com/kart-io/goagent/management/adapters"
	"github.com/kart-io/goagent/workflow"
)

type reviewState struct {
	Scenario string         `json:"scenario"`
	Message  string         `json:"message"`
	Reply    string         `json:"reply,omitempty"`
	Decision string         `json:"decision,omitempty"`
	Result   string         `json:"result,omitempty"`
	Model    *modelEvidence `json:"model_evidence,omitempty"`
}

// 使用真实 DurableGraph；模型后端可选，审批由宿主决定，不进行外部业务写入。
func reviewFactory(model *modelBackend) mg.Factory {
	return adapters.GraphFactory[reviewState]{
		Check: func(context.Context, mg.Version) error { return nil },
		Initial: func(raw json.RawMessage) (reviewState, error) {
			var s reviewState
			err := json.Unmarshal(raw, &s, json.Strict)
			if err == nil && s.Scenario != "review" && s.Scenario != "echo" && s.Scenario != "cancel" {
				err = fmt.Errorf("scenario 必须为 review、echo 或 cancel")
			}
			// 审批结论只能来自管理审批，不能由初始业务输入设置。
			if s.Reply != "" || s.Decision != "" || s.Result != "" || s.Model != nil {
				err = fmt.Errorf("初始输入只允许 scenario 和 message")
			}
			return s, err
		},
		BuildGraph: func(ctx context.Context, _ mg.Version, r mg.Resources) (*workflow.DurableGraph[reviewState], func(context.Context) error, error) {
			g := workflow.NewGraph[reviewState]()
			nodes := []struct {
				name string
				run  func(context.Context, reviewState) (reviewState, error)
			}{
				{"route", func(_ context.Context, s reviewState) (reviewState, error) { return s, nil }},
				{"ask", func(ctx context.Context, s reviewState) (reviewState, error) {
					raw, err := durable.AwaitInput(ctx, "review-note", json.RawMessage(`{"prompt":"请补充发布说明"}`), func(raw json.RawMessage) error {
						var answer string
						if json.Unmarshal(raw, &answer) != nil || strings.TrimSpace(answer) == "" {
							return fmt.Errorf("发布说明必须是非空字符串")
						}
						return nil
					})
					if err != nil {
						return s, err
					}
					err = json.Unmarshal(raw, &s.Reply)
					return s, err
				}},
				{"publish", func(ctx context.Context, s reviewState) (reviewState, error) {
					switch s.Decision {
					case "approve":
						if model != nil {
							return model.summarize(ctx, s)
						}
						s.Result = "模拟发布完成：" + s.Message + "；说明：" + s.Reply
					case "reject":
						s.Result = "用户拒绝，未执行模拟发布"
					default:
						return s, fmt.Errorf("缺少明确审批决定")
					}
					return s, nil
				}},
				{"echo", func(ctx context.Context, s reviewState) (reviewState, error) {
					if model != nil {
						return model.summarize(ctx, s)
					}
					s.Result = "远端 Agent 已处理：" + s.Message
					return s, nil
				}},
				{"slow", func(ctx context.Context, s reviewState) (reviewState, error) { <-ctx.Done(); return s, ctx.Err() }},
			}
			for _, node := range nodes {
				if err := g.AddNode(node.name, node.run); err != nil {
					return nil, nil, err
				}
			}
			if err := g.AddBranch("route", func(_ context.Context, s reviewState) (string, error) {
				switch s.Scenario {
				case "review":
					return "ask", nil
				case "cancel":
					return "slow", nil
				default:
					return "echo", nil
				}
			}, "ask", "slow", "echo"); err != nil {
				return nil, nil, err
			}
			for _, edge := range [][2]string{{constants.WorkflowStart, "route"}, {"ask", "publish"}, {"publish", constants.WorkflowEnd}, {"echo", constants.WorkflowEnd}, {"slow", constants.WorkflowEnd}} {
				if err := g.AddEdge(edge[0], edge[1]); err != nil {
					return nil, nil, err
				}
			}
			compiled, err := g.CompileDurable(ctx, workflow.DurableConfig{Store: r.Journal, Version: "protocol-demo-v1", ApprovalNodes: []string{"publish"}}, workflow.WithResumeHandler(func(_ context.Context, _ string, snap *workflow.GraphSnapshot[reviewState], raw json.RawMessage) (reviewState, error) {
				s := snap.State
				var answer mg.ApprovalAnswer
				if json.Unmarshal(raw, &answer) != nil || (answer.Decision != "approve" && answer.Decision != "reject") {
					return s, fmt.Errorf("无效审批回应")
				}
				s.Decision = answer.Decision
				return s, nil
			}))
			return compiled, nil, err
		},
	}
}
