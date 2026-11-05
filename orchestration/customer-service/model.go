package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent-examples/orchestration/internal/demomodel"
	"github.com/kart-io/goagent/llm"
)

var (
	orderPattern  = regexp.MustCompile(`A\d{4}`)
	amountPattern = regexp.MustCompile(`(\d+)\s*元`)
)

// offlineModel 是有限的脚本化对话，不是自然语言模型：按 README 中的固定说法回答。
// 真实 Builder、工具调度、持久日志与用量统计照常运行，只替换了传输层。
func offlineModel() *livemodel.Client {
	var calls atomic.Int64
	return livemodel.Wrap(&demomodel.Model{Calls: &calls, Respond: respond}, livemodel.Config{Provider: "scripted", Model: "customer-service-transcript"})
}

func text(s string) llm.StreamChunk { return llm.StreamChunk{Content: s, Delta: s} }

func jsonText(v any) llm.StreamChunk {
	data, _ := json.Marshal(v)
	return text(string(data))
}

func respond(req *llm.CompletionRequest) llm.StreamChunk {
	system := req.Messages[0].Content
	var view ticketView
	for _, m := range req.Messages {
		if m.Role == "user" {
			_ = json.Unmarshal([]byte(m.Content), &view)
			break
		}
	}
	switch {
	case strings.HasPrefix(system, "customer-service-triage"):
		return triageTurn(view)
	case strings.HasPrefix(system, "customer-service-general"):
		return text("您好，包裹已在配送途中，预计 1 至 2 天送达；账单与发票可在订单详情页下载。")
	case strings.HasPrefix(system, "customer-service-refund"):
		return refundTurn(view, req.Messages)
	}
	return text("离线脚本不支持该请求")
}

func triageTurn(v ticketView) llm.StreamChunk {
	last := ""
	if n := len(v.Turns); n > 0 {
		last = v.Turns[n-1].Agent
	}
	switch {
	case strings.Contains(v.Input, "乱码"):
		return text("抱歉我没看懂")
	case strings.Contains(v.Input, "反复咨询"):
		return jsonText(triageDecision{Route: generalName, Intent: "反复咨询物流"})
	case last == refundName || last == generalName:
		return jsonText(triageDecision{Route: "FINISH", Reply: v.Turns[len(v.Turns)-1].Output})
	case strings.Contains(v.Input, "退款"):
		return jsonText(triageDecision{Route: refundName, Intent: "申请退款"})
	}
	return jsonText(triageDecision{Route: generalName, Intent: "咨询物流或账单"})
}

func refundTurn(v ticketView, msgs []llm.Message) llm.StreamChunk {
	input := v.Input
	switch {
	case strings.Contains(input, "胡乱提议"):
		return text("这不是 JSON")
	case strings.Contains(input, "不用退"):
		return jsonText(proposal{Action: "no_refund", Reason: "客户表示不需要退款"})
	}
	var results []llm.Message
	for i := len(msgs) - 1; i >= 0 && msgs[i].Role == "tool"; i-- {
		results = append(results, msgs[i])
	}
	if len(results) == 0 {
		if order := orderPattern.FindString(input); order != "" {
			return llm.StreamChunk{ToolCalls: []llm.ToolCall{demomodel.CallWith("order-1", "get_order", fmt.Sprintf(`{"order_id":%q}`, order))}}
		}
		ask := demomodel.CallWith("ask-1", askInterrupt, `{"question":"请提供需要退款的订单号"}`)
		if strings.Contains(input, "两次追问") {
			return llm.StreamChunk{ToolCalls: []llm.ToolCall{ask, demomodel.CallWith("ask-2", askInterrupt, `{"question":"请问退款原因是什么"}`)}}
		}
		return llm.StreamChunk{ToolCalls: []llm.ToolCall{ask}}
	}
	asked := false
	for _, r := range results {
		if !strings.HasPrefix(r.ToolCallID, "ask-") {
			continue
		}
		asked = true
		if order := orderPattern.FindString(r.Content); order != "" {
			return llm.StreamChunk{ToolCalls: []llm.ToolCall{demomodel.CallWith("order-2", "get_order", fmt.Sprintf(`{"order_id":%q}`, order))}}
		}
	}
	if asked {
		return jsonText(proposal{Action: "no_refund", Reason: "客户未提供订单号，无法办理退款"})
	}
	last := results[0]
	if strings.Contains(last.Content, "未找到") {
		return jsonText(proposal{Action: "no_refund", Reason: "未找到该订单，请核对订单号"})
	}
	var o struct {
		OrderID   string `json:"order_id"`
		PaidCents int64  `json:"paid_cents"`
	}
	_ = json.Unmarshal([]byte(last.Content), &o)
	amount := o.PaidCents
	if m := amountPattern.FindStringSubmatch(input); m != nil {
		yuan, _ := strconv.ParseInt(m[1], 10, 64)
		amount = yuan * 100
	}
	p := proposal{Action: "refund", OrderID: o.OrderID, AmountCents: amount, Reason: "客户申请退款"}
	if strings.Contains(input, "投诉") {
		p.AgentDisputed, p.AgentNote = true, "客户表示要投诉"
	}
	return jsonText(p)
}
