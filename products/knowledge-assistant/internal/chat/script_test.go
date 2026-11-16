package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/kart-io/goagent/llm"
)

func TestScriptRules(t *testing.T) {
	tests := []struct {
		name     string
		msgs     []llm.Message
		finish   string
		toolName string
		toolArgs string
		text     string
		contains string
	}{
		{name: "记住", msgs: []llm.Message{{Role: "user", Content: "记住：周会改到周三"}},
			finish: "tool_calls", toolName: "memory_save", toolArgs: `{"content":"周会改到周三","kind":"fact"}`},
		{name: "清空", msgs: []llm.Message{{Role: "user", Content: "请清空我的记忆"}},
			finish: "tool_calls", toolName: "purge_my_memory", toolArgs: `{}`},
		{name: "重建", msgs: []llm.Message{{Role: "user", Content: "重建文档索引"}},
			finish: "tool_calls", toolName: "rebuild_docs_index", toolArgs: `{}`},
		{name: "回显", msgs: []llm.Message{
			{Role: "system", Content: "来源：goagent:memory#godoc"}, {Role: "user", Content: "怎么用"}},
			finish: "stop", contains: "来源：goagent:memory#godoc"},
		{name: "工具结果", msgs: []llm.Message{{Role: "user", Content: "清空我的记忆"}, {Role: "tool", Content: "已清空 3 条"}},
			finish: "stop", text: "工具结果：已清空 3 条"},
		{name: "拒绝", msgs: []llm.Message{{Role: "user", Content: "清空我的记忆"}, {Role: "tool", Content: "审批人已拒绝此操作"}},
			finish: "stop", text: "用户拒绝，未清空"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ch, err := NewScript().CompleteToolsStream(context.Background(), &llm.CompletionRequest{Messages: tc.msgs}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var last *llm.StreamChunk
			for c := range ch {
				last = c
			}
			if last == nil || last.FinishReason != tc.finish || !last.Done {
				t.Fatalf("终止块不符：%+v", last)
			}
			if last.Usage == nil || last.Usage.TotalTokens != 5 {
				t.Fatalf("用量不符：%+v", last.Usage)
			}
			if tc.toolName != "" {
				if len(last.ToolCalls) != 1 || last.ToolCalls[0].ID == "" ||
					last.ToolCalls[0].Function.Name != tc.toolName || last.ToolCalls[0].Function.Arguments != tc.toolArgs {
					t.Fatalf("工具调用不符：%+v", last.ToolCalls)
				}
			}
			if tc.text != "" && last.Content != tc.text {
				t.Fatalf("回答 %q，期望 %q", last.Content, tc.text)
			}
			if tc.contains != "" && !strings.Contains(last.Content, tc.contains) {
				t.Fatalf("回答 %q 不含 %q", last.Content, tc.contains)
			}
		})
	}
}

func TestScriptCallIDDeterministic(t *testing.T) {
	ids := func(n int) string {
		msgs := make([]llm.Message, n)
		msgs[n-1] = llm.Message{Role: "user", Content: "重建文档索引"}
		ch, _ := NewScript().CompleteStream(context.Background(), &llm.CompletionRequest{Messages: msgs})
		var id string
		for c := range ch {
			if len(c.ToolCalls) > 0 {
				id = c.ToolCalls[0].ID
			}
		}
		return id
	}
	if ids(1) != ids(1) || ids(1) == ids(3) || ids(1) == "" {
		t.Fatal("工具调用 ID 须可重放且随消息数变化")
	}
}
