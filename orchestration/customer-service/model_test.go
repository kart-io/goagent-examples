package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent-examples/orchestration/internal/demomodel"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/memory"
)

func memoryRequest(system, user string, extra ...llm.Message) *llm.CompletionRequest {
	return &llm.CompletionRequest{Messages: append([]llm.Message{llm.SystemMessage(system), llm.UserMessage(user)}, extra...)}
}

func ticketJSON(input string) string {
	data, _ := json.Marshal(ticketView{Input: input})
	return string(data)
}

func TestOfflineMemoryScripts(t *testing.T) {
	const (
		extract   = "你从一轮对话中抽取值得跨会话长期记住的信息"
		reconcile = "你维护长期记忆。给定一条新信息"
		general   = "customer-service-general 通用成员"
	)
	reconcileUser := func(info string, lines ...string) string {
		s := "新信息：" + info + "\n\n已有记忆：\n"
		for _, l := range lines {
			s += l + "\n"
		}
		return s
	}
	retry := func(system, user string) *llm.CompletionRequest {
		return memoryRequest(system, user, llm.Message{Role: "assistant", Content: "这不是 JSON"},
			llm.UserMessage("上面的回复不是合法 JSON（invalid character）。按原要求重新输出完整、合法的 JSON，只输出 JSON。"))
	}
	const (
		extractFact = `{"memories":[{"kind":"fact","content":"%s","keywords":[],"entities":[],"importance":0.8}]}`
		saveArgs    = `{"content":"我偏好电话联系","kind":"fact","pinned":true}`
		generalText = "您好，包裹已在配送途中，预计 1 至 2 天送达；账单与发票可在订单详情页下载。"
	)
	tests := []struct {
		name     string
		req      *llm.CompletionRequest
		want     string
		wantJSON bool
		call     string
	}{
		{name: "通用成员收到记住请求先调用 memory_save", req: memoryRequest(general, ticketJSON("请记住：我偏好电话联系")), call: saveArgs},
		{name: "memory_save 结果返回后确认", req: memoryRequest(general, ticketJSON("请记住：我偏好电话联系"),
			llm.Message{Role: "assistant"}, llm.Message{Role: "tool", ToolCallID: "mem-1", Content: "ok"}), want: "好的，已记住。"},
		{name: "通用成员普通咨询回复不变", req: memoryRequest(general, ticketJSON("我的包裹到哪了")), want: generalText},
		{name: "抽取：只退回原支付方式", req: memoryRequest(extract, "用户：退款请只退回原支付方式"), want: fmt.Sprintf(extractFact, "客户要求退款只退回原支付方式"), wantJSON: true},
		{name: "抽取：邮件或短信", req: memoryRequest(extract, "用户：邮件或短信都可以"), want: fmt.Sprintf(extractFact, "客户接受邮件或短信联系"), wantJSON: true},
		{name: "抽取：先说搬到浦东又改口得到两条", req: memoryRequest(extract, "用户：先说搬到浦东又改口"), want: `{"memories":[{"kind":"fact","content":"客户住在上海浦东","keywords":[],"entities":[],"importance":0.8},{"kind":"fact","content":"客户不住在上海浦东","keywords":[],"entities":[],"importance":0.8}]}`, wantJSON: true},
		{name: "抽取：上海的记录不对", req: memoryRequest(extract, "用户：上海的记录不对"), want: fmt.Sprintf(extractFact, "客户不住在上海"), wantJSON: true},
		{name: "抽取：坏回复返回非 JSON", req: memoryRequest(extract, "用户：抽取坏回复"), want: "这不是 JSON"},
		{name: "抽取：重试请求仍返回非 JSON", req: retry(extract, "用户：抽取坏回复"), want: "这不是 JSON"},
		{name: "抽取：重试请求仍按首条用户消息匹配", req: retry(extract, "用户：邮件或短信都可以"), want: fmt.Sprintf(extractFact, "客户接受邮件或短信联系"), wantJSON: true},
		{name: "抽取：无关对话为空", req: memoryRequest(extract, "用户：你好"), want: `{"memories":[]}`, wantJSON: true},
		{name: "整理：邮件候选更新", req: memoryRequest(reconcile, reconcileUser("客户接受邮件或短信联系", "- id=m1：客户偏好邮件联系")), want: `{"op":"update","target":"m1","content":"客户接受邮件或短信联系"}`, wantJSON: true},
		{name: "整理：浦东候选更新已有的上海记录", req: memoryRequest(reconcile, reconcileUser("客户住在上海浦东", "- id=a1：客户要求退款只退回原支付方式", "- id=x-1：我住在上海")), want: `{"op":"update","target":"x-1","content":"客户住在上海浦东"}`, wantJSON: true},
		{name: "整理：否定浦东候选解析出计划记录 target", req: memoryRequest(reconcile, reconcileUser("客户不住在上海浦东", "- id=k/3-0：客户住在上海浦东")), want: `{"op":"invalidate","target":"k/3-0"}`, wantJSON: true},
		{name: "整理：否定上海候选", req: memoryRequest(reconcile, reconcileUser("客户不住在上海", "- id=x-1：我住在上海")), want: `{"op":"invalidate","target":"x-1"}`, wantJSON: true},
		{name: "整理：重试请求仍解析首条用户消息", req: retry(reconcile, reconcileUser("客户不住在上海浦东", "- id=b-2：客户住在上海浦东")), want: `{"op":"invalidate","target":"b-2"}`, wantJSON: true},
		{name: "整理：无候选新增", req: memoryRequest(reconcile, reconcileUser("客户不住在上海浦东")), want: `{"op":"add"}`, wantJSON: true},
		{name: "整理：邮件新信息但候选无邮件则新增", req: memoryRequest(reconcile, reconcileUser("客户接受邮件或短信联系", "- id=m1：客户住在上海")), want: `{"op":"add"}`, wantJSON: true},
		{name: "整理：否定新信息但候选不是被否定的内容则新增", req: memoryRequest(reconcile, reconcileUser("客户不住在上海浦东", "- id=m1：客户偏好邮件联系")), want: `{"op":"add"}`, wantJSON: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := respond(tt.req)
			if tt.call != "" {
				require.Len(t, got.ToolCalls, 1)
				require.Equal(t, "memory_save", got.ToolCalls[0].Function.Name)
				require.JSONEq(t, tt.call, string(got.ToolCalls[0].Function.Arguments))
				return
			}
			require.Empty(t, got.ToolCalls)
			require.Equal(t, tt.wantJSON, json.Valid([]byte(got.Content)))
			if tt.wantJSON {
				require.JSONEq(t, tt.want, got.Content)
				return
			}
			require.Equal(t, tt.want, got.Content)
		})
	}
}

// TestOfflineScriptsCurrentTurn 覆盖开启记忆后的请求形态：前面有上一轮的会话历史，末尾有召回块。
func TestOfflineScriptsCurrentTurn(t *testing.T) {
	const (
		refund  = "customer-service-refund：退款专员"
		general = "customer-service-general：通用成员"
	)
	recall := llm.UserMessage(memory.RecallPrefix + "以下是过去对话中记下的信息\n- 客户要求退款只退回原支付方式")
	call := func(id, name, args string) llm.Message {
		return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{demomodel.CallWith(id, name, args)}}
	}
	result := func(id, content string) llm.Message {
		return llm.Message{Role: "tool", ToolCallID: id, Content: content}
	}
	order := func(id string) string { return fmt.Sprintf(`{"order_id":%q,"paid_cents":9900}`, id) }
	// history 是同一会话上一轮完整的退款对话。
	history := []llm.Message{
		llm.UserMessage(ticketJSON("订单 A1001 申请退款 30 元")),
		call("order-1", "get_order", `{"order_id":"A1001"}`), result("order-1", order("A1001")),
		{Role: "assistant", Content: `{"action":"refund","order_id":"A1001","amount_cents":3000,"reason":"客户申请退款"}`},
	}
	msgs := func(system string, parts ...[]llm.Message) *llm.CompletionRequest {
		out := []llm.Message{llm.SystemMessage(system)}
		for _, p := range parts {
			out = append(out, p...)
		}
		return &llm.CompletionRequest{Messages: out}
	}
	one := func(m ...llm.Message) []llm.Message { return m }
	tests := []struct {
		name     string
		req      *llm.CompletionRequest
		call     string // 期望的工具调用 名称:参数；为空时期望文本
		contains string
	}{
		{name: "退款：末尾召回块不遮住本轮的订单结果", req: msgs(refund, one(llm.UserMessage(ticketJSON("订单 A1002 申请退款 30 元")),
			call("order-1", "get_order", `{"order_id":"A1002"}`), result("order-1", order("A1002")), recall)), contains: `"order_id":"A1002"`},
		{name: "退款：本轮只有召回块时先查本轮订单", req: msgs(refund, one(llm.UserMessage(ticketJSON("订单 A1002 申请退款 30 元")), recall)),
			call: `get_order:{"order_id":"A1002"}`},
		{name: "退款：上一轮的订单结果不算本轮", req: msgs(refund, history, one(llm.UserMessage(ticketJSON("订单 A1003 申请退款 30 元")), recall)),
			call: `get_order:{"order_id":"A1003"}`},
		{name: "退款：有历史时按本轮订单提议", req: msgs(refund, history, one(llm.UserMessage(ticketJSON("订单 A1003 申请退款 30 元")),
			call("order-1", "get_order", `{"order_id":"A1003"}`), result("order-1", order("A1003")), recall)), contains: `"order_id":"A1003"`},
		{name: "通用：上一轮记住过不算本轮已记住", req: msgs(general, one(llm.UserMessage(ticketJSON("请记住：只用邮件联系我")),
			call("mem-1", "memory_save", `{}`), result("mem-1", "ok"), llm.Message{Role: "assistant", Content: "好的，已记住。"},
			llm.UserMessage(ticketJSON("请记住：周末别打电话")), recall)), call: `memory_save:{"content":"周末别打电话","kind":"fact","pinned":true}`},
		{name: "通用：本轮 memory_save 结果之后有召回块仍确认", req: msgs(general, one(llm.UserMessage(ticketJSON("请记住：只用邮件联系我")),
			call("mem-1", "memory_save", `{}`), result("mem-1", "ok"), recall)), contains: "好的，已记住。"},
		{name: "通用：按本轮输入而不是历史输入答复", req: msgs(general, one(llm.UserMessage(ticketJSON("请记住：只用邮件联系我")),
			llm.Message{Role: "assistant", Content: "好的，已记住。"}, llm.UserMessage(ticketJSON("我的包裹到哪了")), recall)), contains: "包裹已在配送途中"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := respond(tt.req)
			if tt.call != "" {
				name, args, _ := strings.Cut(tt.call, ":")
				require.Len(t, got.ToolCalls, 1)
				require.Equal(t, name, got.ToolCalls[0].Function.Name)
				require.JSONEq(t, args, got.ToolCalls[0].Function.Arguments)
				return
			}
			require.Empty(t, got.ToolCalls)
			require.Contains(t, got.Content, tt.contains)
		})
	}
}
