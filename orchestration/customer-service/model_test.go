package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent/llm"
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
	tests := []struct {
		name string
		req  *llm.CompletionRequest
		text string
		call string
	}{
		{"通用成员收到记住请求先调用 memory_save", memoryRequest(general, ticketJSON("请记住：我偏好电话联系")), "", `{"content":"我偏好电话联系","kind":"fact","pinned":true}`},
		{"memory_save 结果返回后确认", memoryRequest(general, ticketJSON("请记住：我偏好电话联系"),
			llm.Message{Role: "assistant"}, llm.Message{Role: "tool", ToolCallID: "mem-1", Content: "ok"}), "好的，已记住。", ""},
		{"通用成员普通咨询回复不变", memoryRequest(general, ticketJSON("我的包裹到哪了")), "您好，包裹已在配送途中，预计 1 至 2 天送达；账单与发票可在订单详情页下载。", ""},
		{"抽取：只退回原支付方式", memoryRequest(extract, "用户：退款请只退回原支付方式"), `{"memories":[{"kind":"fact","content":"客户要求退款只退回原支付方式","keywords":[],"entities":[],"importance":0.8}]}`, ""},
		{"抽取：邮件或短信", memoryRequest(extract, "用户：邮件或短信都可以"), `{"memories":[{"kind":"fact","content":"客户接受邮件或短信联系","keywords":[],"entities":[],"importance":0.8}]}`, ""},
		{"抽取：先说住杭州又改口得到两条", memoryRequest(extract, "用户：先说住杭州又改口"), `{"memories":[{"kind":"fact","content":"客户住在杭州","keywords":[],"entities":[],"importance":0.8},{"kind":"fact","content":"客户不住在杭州","keywords":[],"entities":[],"importance":0.8}]}`, ""},
		{"抽取：坏回复返回非 JSON", memoryRequest(extract, "用户：抽取坏回复"), "这不是 JSON", ""},
		{"抽取：无关对话为空", memoryRequest(extract, "用户：你好"), `{"memories":[]}`, ""},
		{"整理：邮件候选更新", memoryRequest(reconcile, reconcileUser("客户接受邮件或短信联系", "- id=m1：客户偏好邮件联系")), `{"op":"update","target":"m1","content":"客户接受邮件或短信联系"}`, ""},
		{"整理：否定杭州候选解析出 target", memoryRequest(reconcile, reconcileUser("客户不住在杭州", "- id=a1：客户要求退款只退回原支付方式", "- id=b-2：客户住在杭州")), `{"op":"invalidate","target":"b-2"}`, ""},
		{"整理：无候选新增", memoryRequest(reconcile, reconcileUser("客户不住在杭州")), `{"op":"add"}`, ""},
		{"整理：邮件新信息但候选无邮件则新增", memoryRequest(reconcile, reconcileUser("客户接受邮件或短信联系", "- id=m1：客户住在杭州")), `{"op":"add"}`, ""},
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
			if tt.name == "抽取：坏回复返回非 JSON" {
				require.False(t, json.Valid([]byte(got.Content)))
				return
			}
			if json.Valid([]byte(tt.text)) {
				require.JSONEq(t, tt.text, got.Content)
				return
			}
			require.Equal(t, tt.text, got.Content)
		})
	}
}
