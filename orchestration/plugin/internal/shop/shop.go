// Package shop 是插件案例共用的电商插件：订单、物流、退款政策与手机号脱敏。
package shop

import (
	"context"
	"regexp"
	"sync/atomic"

	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/plugin"
	"github.com/kart-io/goagent/tools"
)

type static struct {
	name, version string
	c             plugin.Contribution
}

func (s *static) Name() string    { return s.name }
func (s *static) Version() string { return s.version }
func (s *static) Contribute(context.Context, *plugin.Env) (plugin.Contribution, error) {
	return s.c, nil
}

// Static 返回固定贡献的插件。
func Static(name, version string, c plugin.Contribution) plugin.Plugin {
	return &static{name: name, version: version, c: c}
}

// Tool 返回无参数工具，执行时把 runs 加一（runs 可为 nil）。
func Tool(name, result string, runs *atomic.Int32) interfaces.Tool {
	return tools.NewBaseTool(name, name, `{"type":"object","properties":{},"additionalProperties":false}`, func(context.Context, *interfaces.ToolInput) (*interfaces.ToolOutput, error) {
		if runs != nil {
			runs.Add(1)
		}
		return &interfaces.ToolOutput{Success: true, Result: result}, nil
	})
}

func text(s string) func(builder.PromptScope) string {
	return func(builder.PromptScope) string { return s }
}

// Orders 提供订单查询工具与使用说明。
func Orders(runs *atomic.Int32) plugin.Plugin {
	return Static("orders", "1.0.0", plugin.Contribution{
		Tools:          []interfaces.Tool{Tool("lookup_order", "订单 A100：已发货，金额 199 元，下单 5 天", runs)},
		PromptSections: []builder.PromptSection{{Name: "orders", Text: text("回答订单问题前先调用 lookup_order，回答中给出订单号。")}},
	})
}

// Shipping 提供物流查询工具。
func Shipping() plugin.Plugin {
	return Static("shipping", "1.0.0", plugin.Contribution{
		Tools: []interfaces.Tool{Tool("track_parcel", "包裹在上海转运中心，预计明天送达", nil)},
	})
}

// RefundPolicy 只提供退款政策提示词。
func RefundPolicy() plugin.Plugin {
	return Static("refund-policy", "1.0.0", plugin.Contribution{
		PromptSections: []builder.PromptSection{{Name: "refund-policy", Order: 1, Text: text("签收 7 天内可无理由退款。")}},
	})
}

var phone = regexp.MustCompile(`1[3-9]\d{9}`)

// MaskPhone 把手机号中间四位替换为星号。
func MaskPhone(s string) string {
	return phone.ReplaceAllStringFunc(s, func(p string) string { return p[:3] + "****" + p[7:] })
}

// Redact 在每次模型请求前脱敏用户消息中的手机号。version 进入持久身份。
func Redact(version string) plugin.Plugin {
	return Static("redact", version, plugin.Contribution{
		PreSteps: []plugin.PreStep{{Identity: "mask-phone", Hook: func(_ context.Context, s *builder.Step) (builder.PreStepResult, error) {
			for i, m := range s.Messages {
				if m.Role == "user" {
					s.Messages[i].Content = MaskPhone(m.Content)
				}
			}
			return builder.PreStepResult{}, nil
		}}},
	})
}
