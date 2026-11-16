package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kart-io/goagent/constants"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
)

// Script 是离线确定性模型：按最后一条消息的内容选择工具调用或回答，
// 让 e2e 与测试不依赖外部模型。llm.Client 在 v0.54.1 只有 CompleteStream 与
// Provider，Builder 不会调用 Complete/Chat，因此无需内嵌接口。
type Script struct{}

// NewScript 创建脚本模型。
func NewScript() *Script { return &Script{} }

func (*Script) Provider() constants.Provider { return constants.ProviderOpenAI }

func (s *Script) CompleteStream(_ context.Context, r *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	return s.reply(r), nil
}

func (s *Script) CompleteToolsStream(_ context.Context, r *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	return s.reply(r), nil
}

// scriptTools 按顺序匹配用户消息；新增一种工具只需加一行。
var scriptTools = []struct {
	match func(string) bool
	name  string
	args  func(string) string
}{
	{func(u string) bool { return strings.HasPrefix(u, "记住：") }, constants.ToolMemorySave,
		func(u string) string {
			b, _ := json.Marshal(map[string]string{"content": strings.TrimPrefix(u, "记住："), "kind": "fact"})
			return string(b)
		}},
	{func(u string) bool { return strings.Contains(u, "清空我的记忆") }, "purge_my_memory", func(string) string { return `{}` }},
	{func(u string) bool { return strings.Contains(u, "重建文档索引") }, "rebuild_docs_index", func(string) string { return `{}` }},
}

func (*Script) reply(r *llm.CompletionRequest) <-chan *llm.StreamChunk {
	usage := &llm.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}
	chunk := &llm.StreamChunk{Done: true, FinishReason: "stop", Usage: usage}
	msgs := r.Messages
	var lastUser string
	var others []string
	for _, m := range msgs {
		switch m.Role {
		case "user":
			lastUser = m.Content
		case "tool":
		default:
			others = append(others, m.Content)
		}
	}

	switch {
	case len(msgs) > 0 && msgs[len(msgs)-1].Role == "tool":
		last := msgs[len(msgs)-1].Content
		chunk.Content = "工具结果：" + last
		if strings.Contains(last, "拒绝") {
			chunk.Content = "用户拒绝，未清空"
		}
	default:
		chunk.Content = "回显：\n" + strings.Join(others, "\n")
		for _, t := range scriptTools {
			if t.match(lastUser) {
				// 消息数随对话单调增长，既唯一又可重放。
				call := llm.ToolCall{ID: fmt.Sprintf("script-call-%d", len(msgs)), Type: "function"}
				call.Function.Name = t.name
				call.Function.Arguments = t.args(lastUser)
				chunk = &llm.StreamChunk{Done: true, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{call}, Usage: usage}
				break
			}
		}
	}
	chunk.Delta = chunk.Content
	ch := make(chan *llm.StreamChunk, 1)
	ch <- chunk
	close(ch)
	return ch
}

var _ llm.ToolStreamClient = (*Script)(nil)
