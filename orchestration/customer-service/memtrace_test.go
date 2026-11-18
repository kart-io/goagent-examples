package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent/llm"
)

// memoryTrace 把记忆抽取与整理的每次模型调用（系统提示开头、输入、回复或错误）追加到 path，
// 用来区分「抽取没有给出候选」与「整理判为 noop」。
type memoryTrace struct {
	llm.Client
	path string
	mu   sync.Mutex
}

type memoryTraceEntry struct {
	System string `json:"system"`
	Input  string `json:"input"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (c *memoryTrace) CompleteStream(ctx context.Context, req *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	entry := memoryTraceEntry{}
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			entry.System = truncateRunes(m.Content, 20)
		case "user":
			entry.Input = m.Content // 修复 JSON 的重试会追加 user 消息，取最后一条
		}
	}
	in, err := c.Client.CompleteStream(ctx, req)
	if err != nil {
		entry.Error = err.Error()
		c.write(entry)
		return nil, err
	}
	out := make(chan *llm.StreamChunk)
	go func() {
		defer close(out)
		var deltas strings.Builder
		for ch := range in {
			if ch != nil {
				deltas.WriteString(ch.Delta)
				switch {
				case ch.Error != nil:
					entry.Error = ch.Error.Error()
					c.write(entry)
				case ch.Done:
					entry.Output = ch.Content
					if entry.Output == "" {
						entry.Output = deltas.String()
					}
					c.write(entry)
				}
			}
			select {
			case out <- ch:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (c *memoryTrace) write(e memoryTraceEntry) {
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	f, err := os.OpenFile(c.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

func TestMemoryTrace(t *testing.T) {
	cases := []struct {
		name, system, user string
	}{
		{"抽取", "你从一轮对话中抽取值得跨会话长期记住的信息", "对话时间：2026-11-18 09:00:00\n用户：我的充电器用的时候发烫"},
		{"整理", "你维护长期记忆。给定一条新信息", "新信息：用户反馈充电器发烫\n\n已有记忆：\n- id=a：用户反馈耳机断连\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "memory-llm.jsonl")
			c := &memoryTrace{Client: offlineModel(), path: path}
			resp, err := llm.Chat(context.Background(), c, []llm.Message{llm.SystemMessage(tc.system), llm.UserMessage(tc.user)})
			require.NoError(t, err)

			f, err := os.Open(path)
			require.NoError(t, err)
			defer f.Close()
			var got []memoryTraceEntry
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				var e memoryTraceEntry
				require.NoError(t, json.Unmarshal(sc.Bytes(), &e))
				got = append(got, e)
			}
			require.Len(t, got, 1, "每次调用一行")
			require.True(t, strings.HasPrefix(tc.system, strings.TrimSuffix(got[0].System, "…")), "系统提示开头：%q", got[0].System)
			require.Equal(t, tc.user, got[0].Input)
			require.Equal(t, resp.Content, got[0].Output, "记下的回复与调用方收到的一致")
			require.NotEmpty(t, got[0].Output)
		})
	}
}
