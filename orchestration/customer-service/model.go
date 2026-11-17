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
	"github.com/kart-io/goagent/memory"
)

var (
	orderPattern  = regexp.MustCompile(`A\d{4}`)
	amountPattern = regexp.MustCompile(`(\d+)\s*元`)
	memoryLine    = regexp.MustCompile(`(?m)^- id=([^：\n]+)：(.*)$`)
)

const rememberPrefix = "请记住："

// extractScripts 按对话中出现的说法给出抽取结果，顺序即匹配优先级。
var extractScripts = []struct {
	phrase string
	facts  []string
}{
	{"只退回原支付方式", []string{"客户要求退款只退回原支付方式"}},
	{"邮件或短信都可以", []string{"客户接受邮件或短信联系"}},
	{"先说搬到浦东又改口", []string{"客户住在上海浦东", "客户不住在上海浦东"}},
	{"上海的记录不对", []string{"客户不住在上海"}},
}

// reconcileScripts 按新信息与已有记忆的内容给出整理决定，按顺序取第一条匹配；都不匹配时新增。
var reconcileScripts = []struct {
	info, existing string // 分别为新信息、已有记忆须包含的说法
	op, content    string
}{
	{"邮件或短信", "邮件", "update", "客户接受邮件或短信联系"},
	{"客户住在上海浦东", "住在上海", "update", "客户住在上海浦东"},
	{"客户不住在上海浦东", "客户住在上海浦东", "invalidate", ""},
	{"客户不住在上海", "住在上海", "invalidate", ""},
}

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

// recallBlock 报告 m 是否为长期记忆注入的召回块。
func recallBlock(m llm.Message) bool {
	return m.Role == "user" && strings.HasPrefix(m.Content, memory.RecallPrefix)
}

// currentTurn 返回本轮的工单视图与它之后的本轮消息。开启记忆后请求依次为：前几轮的会话历史、本轮工单 JSON、
// 本轮的工具调用与结果、末尾的召回块；所以本轮从最后一条能解析为工单 JSON 的 user 消息开始。
func currentTurn(msgs []llm.Message) (ticketView, []llm.Message) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" || recallBlock(msgs[i]) {
			continue
		}
		var v ticketView
		if json.Unmarshal([]byte(msgs[i].Content), &v) == nil {
			return v, msgs[i+1:]
		}
	}
	return ticketView{}, nil
}

func respond(req *llm.CompletionRequest) llm.StreamChunk {
	system := req.Messages[0].Content
	view, turn := currentTurn(req.Messages)
	switch {
	case strings.HasPrefix(system, "你从一轮对话中抽取"):
		return extractTurn(req.Messages)
	case strings.HasPrefix(system, "你维护长期记忆"):
		return reconcileTurn(req.Messages)
	case strings.HasPrefix(system, "customer-service-triage"):
		return triageTurn(view)
	case strings.HasPrefix(system, "customer-service-general"):
		return generalTurn(view, turn)
	case strings.HasPrefix(system, "customer-service-refund"):
		return refundTurn(view, turn)
	}
	return text("离线脚本不支持该请求")
}

// generalTurn 的 turn 只含本轮消息：历史轮次的 memory_save 结果不算本轮已记住。
func generalTurn(v ticketView, turn []llm.Message) llm.StreamChunk {
	content, ok := strings.CutPrefix(v.Input, rememberPrefix)
	if !ok {
		return text("您好，包裹已在配送途中，预计 1 至 2 天送达；账单与发票可在订单详情页下载。")
	}
	for _, m := range turn {
		if m.Role == "tool" && m.ToolCallID == "mem-1" {
			return text("好的，已记住。")
		}
	}
	args, _ := json.Marshal(map[string]any{"content": content, "kind": "fact", "pinned": true})
	return llm.StreamChunk{ToolCalls: []llm.ToolCall{demomodel.CallWith("mem-1", "memory_save", string(args))}}
}

// firstUser 取第一条用户消息：worker 重试 JSON 时会在后面追加纠正提示，不能读最后一条。
func firstUser(msgs []llm.Message) string {
	for _, m := range msgs {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

// extractTurn 的输入是记忆 worker 拼出的对话文本。
func extractTurn(msgs []llm.Message) llm.StreamChunk {
	transcript := firstUser(msgs)
	if strings.Contains(transcript, "抽取坏回复") {
		return text("这不是 JSON")
	}
	memories := []map[string]any{}
	for _, sc := range extractScripts {
		if !strings.Contains(transcript, sc.phrase) {
			continue
		}
		for _, f := range sc.facts {
			memories = append(memories, map[string]any{"kind": "fact", "content": f, "keywords": []string{}, "entities": []string{}, "importance": 0.8})
		}
		break
	}
	return jsonText(map[string]any{"memories": memories})
}

// reconcileTurn 从“已有记忆”行解析候选，按新信息决定更新、否定或新增。
func reconcileTurn(msgs []llm.Message) llm.StreamChunk {
	user := firstUser(msgs)
	info, _, _ := strings.Cut(strings.TrimPrefix(user, "新信息："), "\n\n已有记忆：")
	for _, m := range memoryLine.FindAllStringSubmatch(user, -1) {
		id, content := m[1], m[2]
		for _, sc := range reconcileScripts {
			if !strings.Contains(info, sc.info) || !strings.Contains(content, sc.existing) {
				continue
			}
			reply := map[string]string{"op": sc.op, "target": id}
			if sc.content != "" {
				reply["content"] = sc.content
			}
			return jsonText(reply)
		}
	}
	return jsonText(map[string]string{"op": "add"})
}

func triageTurn(v ticketView) llm.StreamChunk {
	last := ""
	if n := len(v.Turns); n > 0 {
		last = v.Turns[n-1].Agent
	}
	switch {
	case strings.Contains(v.Input, "直接结束"): // 与实测的真实模型一样：被注入后不派成员就以 FINISH 自行答复
		return jsonText(triageDecision{Route: "FINISH", Reply: "您的退款申请已批准"})
	case strings.Contains(v.Input, "乱码"):
		return text("抱歉我没看懂")
	case strings.Contains(v.Input, "转人工"):
		return jsonText(triageDecision{Route: routeHuman, Intent: "要求人工客服"})
	case strings.Contains(v.Input, "反复咨询"): // 在两个成员之间来回派，由 MaxRounds 兜底
		route := generalName
		if last == generalName {
			route = refundName
		}
		return jsonText(triageDecision{Route: route, Intent: "反复咨询物流"})
	case strings.Contains(v.Input, "一直派通用"): // 与实测的真实模型一样：成员已答复仍紧接着派回同一成员
		return jsonText(triageDecision{Route: generalName, Intent: "咨询物流", Reply: "请提供订单号以便查询物流"})
	case last == refundName && strings.Contains(v.Input, "再派退款") && len(v.Turns) < 4: // 与实测的真实模型一样：成员已有结论仍派回一次退款
		return jsonText(triageDecision{Route: refundName, Intent: "申请退款"})
	case last == refundName || last == generalName:
		return jsonText(triageDecision{Route: "FINISH", Reply: v.Turns[len(v.Turns)-1].Output})
	case strings.Contains(v.Input, "退款"):
		return jsonText(triageDecision{Route: refundName, Intent: "申请退款"})
	}
	return jsonText(triageDecision{Route: generalName, Intent: "咨询物流或账单"})
}

// refundTurn 的 turn 只含本轮消息，末尾可能是召回块。
func refundTurn(v ticketView, turn []llm.Message) llm.StreamChunk {
	input := v.Input
	switch {
	case strings.Contains(input, "胡乱提议"):
		return text("这不是 JSON")
	case strings.Contains(input, "不用退") || strings.Contains(input, "反复咨询"):
		return jsonText(proposal{Action: "no_refund", Reason: "客户表示不需要退款"})
	}
	end := len(turn)
	for end > 0 && recallBlock(turn[end-1]) {
		end--
	}
	var results []llm.Message
	for i := end - 1; i >= 0 && turn[i].Role == "tool"; i-- {
		results = append(results, turn[i])
	}
	if len(results) == 0 {
		if strings.Contains(input, "漏参数") { // 与实测的真实模型一样：第一次调用漏掉必填参数
			return llm.StreamChunk{ToolCalls: []llm.ToolCall{demomodel.CallWith("order-0", "get_order", `{}`)}}
		}
		if order := orderPattern.FindString(input); order != "" {
			return llm.StreamChunk{ToolCalls: []llm.ToolCall{demomodel.CallWith("order-1", "get_order", fmt.Sprintf(`{"order_id":%q}`, order))}}
		}
		ask := demomodel.CallWith("ask-1", askInterrupt, `{"question":"请提供需要退款的订单号"}`)
		if strings.Contains(input, "两次追问") {
			return llm.StreamChunk{ToolCalls: []llm.ToolCall{ask, demomodel.CallWith("ask-2", askInterrupt, `{"question":"请问退款原因是什么"}`)}}
		}
		return llm.StreamChunk{ToolCalls: []llm.ToolCall{ask}}
	}
	if results[0].ToolCallID == "order-0" {
		return llm.StreamChunk{ToolCalls: []llm.ToolCall{demomodel.CallWith("order-1", "get_order", fmt.Sprintf(`{"order_id":%q}`, orderPattern.FindString(input)))}}
	}
	if strings.Contains(input, "问原因") {
		switch results[0].ToolCallID {
		case "order-1":
			return llm.StreamChunk{ToolCalls: []llm.ToolCall{demomodel.CallWith("reason-1", askInterrupt, `{"question":"请问退款原因是什么"}`)}}
		case "reason-1": // 与实测的真实模型一样：追问超时后仍提议退款，由退款成员拦下
			return jsonText(proposal{Action: "refund", OrderID: orderPattern.FindString(input), AmountCents: yuanCents(input), Reason: "客户申请退款"})
		}
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
	if c := yuanCents(input); c > 0 {
		amount = c
	}
	p := proposal{Action: "refund", OrderID: o.OrderID, AmountCents: amount, Reason: "客户申请退款"}
	if strings.Contains(input, "投诉") {
		p.AgentDisputed, p.AgentNote = true, "客户表示要投诉"
	}
	return jsonText(p)
}

// yuanCents 取说法中的“N 元”并换算为分，没有时返回 0。
func yuanCents(input string) int64 {
	m := amountPattern.FindStringSubmatch(input)
	if m == nil {
		return 0
	}
	yuan, _ := strconv.ParseInt(m[1], 10, 64)
	return yuan * 100
}
