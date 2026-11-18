package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 真实开发批次 2026-11-18-01 的两条答复：pinchain-05 含加粗，isolate-06 无加粗、Gemini 引文漏了「为您」。
const (
	replyPinchain05 = "根据记录，您此前明确要求后续只通过**邮件**联系您，不使用短信或电话等其他渠道。因此我们会按这个偏好在需要时用邮件与您沟通。"
	replyIsolate06  = "您好，我这边没有查到您此前提供过会员号，目前也没有相关记录可以告诉您。如果方便，请提供注册手机号或订单号，我们可为您进一步核实。"
)

func TestMatchQuote(t *testing.T) {
	tests := []struct {
		name, reply, quote string
		method, source     string // method 为空表示必须拒绝
	}{
		{"逐字匹配优先", replyPinchain05, "不使用短信或电话等其他渠道", quoteExact, "不使用短信或电话等其他渠道"},
		{"含定界符的逐字引文仍按逐字匹配", replyPinchain05, "只通过**邮件**联系您", quoteExact, "只通过**邮件**联系您"},
		{"忽略加粗定界符", replyPinchain05, "后续只通过邮件联系您", quoteBold, "后续只通过**邮件**联系您"},
		{"引文从加粗内部开始", "偏好是**仅通过邮件**接收通知", "邮件接收通知", quoteBold, "邮件**接收通知"},
		{"引文在加粗内部结束", "偏好是**仅通过邮件**接收通知", "偏好是仅通过", quoteBold, "偏好是**仅通过"},
		{"多段加粗", "地址由**北京朝阳**改为**上海浦东**。", "北京朝阳改为上海浦东", quoteBold, "北京朝阳**改为**上海浦东"},
		{"漏字仍拒绝（isolate-06 历史引文）", replyIsolate06, "我这边没有查到您此前提供过会员号，目前也没有相关记录可以告诉您。如果方便，请提供注册手机号或订单号，我们可进一步核实。", "", ""},
		{"删除正文拒绝", "您的耳机是**蓝色**的", "您的耳机是蓝的", "", ""},
		{"改写标点拒绝", "耳机是**蓝色**，没有红色记录", "耳机是蓝色,没有红色记录", "", ""},
		{"拼接片段拒绝", "耳机是**蓝色**。另外没有红色记录", "蓝色没有红色记录", "", ""},
		{"代码片段里的星号不是加粗", "命令写作 `**x**` 即可", "命令写作 `x` 即可", "", ""},
		{"未闭合的星号不是加粗", "注意**蓝色耳机的记录", "注意蓝色耳机", "", ""},
		{"三星号不按加粗忽略", "这是***蓝色***耳机", "这是*蓝色*耳机", "", ""},
		{"下划线加粗不忽略", "这是__蓝色__耳机", "这是蓝色耳机", "", ""},
		{"不清洗其他 Markdown", "**蓝色**\n\n- 没有红色记录", "蓝色 没有红色记录", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := matchQuote(tt.reply, tt.quote)
			if tt.method == "" {
				require.False(t, ok, "%+v", m)
				return
			}
			require.True(t, ok)
			require.Equal(t, tt.method, m.Method)
			require.Equal(t, tt.source, m.Source)
			require.Equal(t, tt.reply[m.Start:m.End], m.Source, "区间映射回连续原文")
			if m.Method == quoteBold {
				require.Equal(t, tt.quote, strings.ReplaceAll(m.Source, "**", ""), "与引文只差定界符")
			}
		})
	}
}

// Gemini 在开发批次中对 pinchain-05 的原始输出：judge-executor/4 判 quote_mismatch，judge-executor/5 应接受并留下匹配依据。
func TestParseChecksQuoteEvidence(t *testing.T) {
	out := "```json\n{\"checks\":[{\"id\":\"Q1\",\"quote\":\"后续只通过邮件联系您\",\"answer\":\"是\"},{\"id\":\"Q2\",\"quote\":\"不使用短信或电话等其他渠道\",\"answer\":\"否\"}]}\n```"
	answers, quotes, err := parseChecks(out, judgeRubrics["pinchain"], replyPinchain05)
	require.NoError(t, err)
	require.Equal(t, "后续只通过邮件联系您", answers[0].Quote, "保留模型原始引文")
	require.Equal(t, []string{"Q1/" + quoteBold + "/后续只通过**邮件**联系您", "Q2/" + quoteExact + "/不使用短信或电话等其他渠道"},
		[]string{quotes[0].ID + "/" + quotes[0].Method + "/" + quotes[0].Source, quotes[1].ID + "/" + quotes[1].Method + "/" + quotes[1].Source})

	_, quotes, err = parseChecks(`{"checks":[{"id":"Q1","quote":"","answer":"否"},{"id":"Q2","quote":"我们可进一步核实","answer":"是"}]}`, judgeRubrics["isolate"], replyIsolate06)
	var se *stageError
	require.ErrorAs(t, err, &se)
	require.Equal(t, causeQuote, se.cause)
	require.Nil(t, quotes)
}
