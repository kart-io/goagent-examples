package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent-examples/internal/livemodel/livemodeltest"
	"github.com/kart-io/goagent-examples/orchestration/internal/demomodel"
	"github.com/kart-io/goagent-examples/orchestration/internal/livecase"
	"github.com/kart-io/goagent-examples/orchestration/internal/livecase/livecasetest"
	"github.com/kart-io/goagent/core/session"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/testing/replay"
	"github.com/stretchr/testify/require"
)

var orderID = regexp.MustCompile(`[A-Z]\d{3}`)

// scriptedClient 先按用户消息中的订单号调用 lookup_order，再按工具结果给出结论。
func scriptedClient() *livemodel.Client {
	var calls atomic.Int64
	return livemodel.Wrap(&demomodel.Model{Calls: &calls, Respond: func(r *llm.CompletionRequest) llm.StreamChunk {
		last := r.Messages[len(r.Messages)-1]
		if last.Role == "tool" {
			for _, s := range Statuses {
				if strings.Contains(last.Content, s) {
					text := "结论：" + s + "\n以查询结果为准。"
					return llm.StreamChunk{Content: text, Delta: text}
				}
			}
			return llm.StreamChunk{Content: "结论：待发货\n没有查到。", Delta: "结论：待发货\n没有查到。"}
		}
		id := orderID.FindString(last.Content)
		call := demomodel.Call("call-"+id, "lookup_order")
		call.Function.Arguments = fmt.Sprintf(`{"order_id":%q}`, id)
		return llm.StreamChunk{ToolCalls: []llm.ToolCall{call}}
	}}, livemodel.Config{Provider: "scripted", Model: "v1"})
}

const childDir, childStep, childScripted = "GOAGENT_SESSION_REPLAY_DIR", "GOAGENT_SESSION_REPLAY_STEP", "GOAGENT_SESSION_REPLAY_SCRIPTED"

// childMain 执行一步：ask0-crash（工具查到结果后退出）、ask0（恢复第一轮）、ask1（第二轮）。
func childMain() {
	client := scriptedClient()
	if os.Getenv(childScripted) != "1" {
		var err error
		if client, err = livemodel.FromEnv(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	turn, hooks := 0, Hooks{}
	switch os.Getenv(childStep) {
	case "ask0-crash":
		hooks.AfterLookup = func() { livecase.ExitWithResult(TurnResult{NewModelCalls: client.Calls()}) }
	case "ask1":
		turn = 1
	}
	out, err := Ask(context.Background(), client, os.Getenv(childDir), turn, hooks)
	if err != nil {
		fmt.Fprintln(os.Stderr, livemodel.Redact(err.Error()))
		os.Exit(2)
	}
	livecase.ExitWithResult(out)
}

func modes(r ReplayResult) map[string]replay.Mode {
	out := map[string]replay.Mode{}
	for _, b := range r.Boundaries {
		out[b.ID] = b.Mode
	}
	return out
}

func count(r ReplayResult, kind string) int {
	n := 0
	for _, b := range r.Boundaries {
		if b.Kind == kind {
			n++
		}
	}
	return n
}

// logBytes 读出会话日志目录中的全部 JSONL，用来确认重放不写原日志。
func logBytes(t *testing.T, dir string) []byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	return data
}

func exercise(t *testing.T, client *livemodel.Client, testName string, scripted bool) {
	dir := t.TempDir()
	env := func(step string) []string {
		e := []string{childDir + "=" + dir, childStep + "=" + step}
		if scripted {
			e = append(e, childScripted+"=1")
		}
		return e
	}
	ctx := context.Background()

	// 第一轮：工具查到结果后、检查点保存前进程退出，日志中留下未结束的轮次。
	var crashed TurnResult
	livecasetest.Result(t, livecasetest.Child(t, testName, env("ask0-crash")...), &crashed)
	require.Empty(t, crashed.Answer)
	require.Equal(t, int64(1), crashed.NewModelCalls)
	store, err := session.NewFileStore(dir)
	require.NoError(t, err)
	events, err := store.Read(ctx, SessionID, 0)
	require.NoError(t, err)
	_, open := session.OpenTurn(events)
	require.True(t, open, "退出时这一轮还没有结束")

	// 新进程以同一执行键续写：第一次模型回合从日志回放，工具检查点没保存所以重跑一次。
	var first TurnResult
	livecasetest.Result(t, livecasetest.Child(t, testName, env("ask0")...), &first)
	t.Logf("第一轮：%+v", first)
	require.Equal(t, "已发货", first.Conclusion)
	require.GreaterOrEqual(t, first.ToolRuns, int64(1))

	// 第二轮在另一个进程：历史从会话日志推导，第一次请求就带着第一轮的对话。
	var second TurnResult
	livecasetest.Result(t, livecasetest.Child(t, testName, env("ask1")...), &second)
	t.Logf("第二轮：%+v", second)
	require.Equal(t, "待发货", second.Conclusion)
	require.Greater(t, second.FirstRequestMessages, int64(4), "system、第一轮至少 4 条消息与本轮输入")

	// 导出：模型最终看到的两轮对话。
	var buf bytes.Buffer
	traj, err := Export(ctx, dir, &buf)
	require.NoError(t, err)
	var exported struct {
		Messages []struct{ Role, Content string } `json:"messages"`
		Tools    []json.RawMessage                `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &exported))
	require.Equal(t, "system", exported.Messages[0].Role)
	users := 0
	for _, m := range exported.Messages {
		if m.Role == "user" {
			users++
		}
	}
	require.Equal(t, 2, users, "中断的那次运行没有留下重复的用户消息")
	require.Equal(t, second.Answer, exported.Messages[len(exported.Messages)-1].Content)
	require.Len(t, exported.Tools, 1)
	require.Equal(t, 2, traj.Turns)
	require.Equal(t, []session.ToolStat{{Name: "lookup_order", Calls: int(first.ToolRuns + second.ToolRuns)}}, traj.Stats, "中断前那次执行没有结果，不计入")

	before := logBytes(t, dir)

	// 全部回放：模型与工具都不调用，答复与记录相同。
	full, err := Replay(ctx, client, dir, ReplayOptions{Turn: 0})
	require.NoError(t, err)
	require.Zero(t, full.NewModelCalls+full.ToolRuns)
	require.Equal(t, first.Answer, full.Answer)
	require.Nil(t, full.Diverged)
	require.Equal(t, int64(count(full, "model")), crashed.NewModelCalls+first.NewModelCalls, "两个进程合计的请求数等于记录的模型回合数")
	again, err := Replay(ctx, client, dir, ReplayOptions{Turn: 1})
	require.NoError(t, err)
	require.Zero(t, again.NewModelCalls+again.ToolRuns)
	require.Equal(t, second.Answer, again.Answer)

	// 工具实跑、结果不变：没有分歧，模型继续回放。
	same, err := Replay(ctx, client, dir, ReplayOptions{Turn: 0, Live: replay.Kind("tool")})
	require.NoError(t, err)
	require.Equal(t, int64(count(same, "tool")), same.ToolRuns)
	require.Zero(t, same.NewModelCalls)
	require.Nil(t, same.Diverged)

	// 工具改了实现（A100 变为已签收）：在工具边界分歧，之后的模型回合用当前模型实跑。
	changed, err := Replay(ctx, client, dir, ReplayOptions{Turn: 0, Live: replay.Kind("tool"), Orders: Delivered()})
	require.NoError(t, err)
	t.Logf("工具改动后：%s", changed.Answer)
	require.NotNil(t, changed.Diverged)
	require.Equal(t, "tool", changed.Diverged.Kind)
	require.Contains(t, string(changed.Diverged.Recorded), "已发货")
	require.Contains(t, string(changed.Diverged.Value), "已签收")
	require.GreaterOrEqual(t, changed.NewModelCalls, int64(1))
	require.Equal(t, "已签收", changed.Conclusion)
	require.Equal(t, replay.Replayed, modes(changed)["model/turn/0"])

	// 从第二轮的第二次模型请求切开：查询回放，只用当前模型重答。
	cut, err := Replay(ctx, client, dir, ReplayOptions{Turn: 1, Cut: replay.ModelTurn(1)})
	require.NoError(t, err)
	require.Zero(t, cut.ToolRuns)
	require.GreaterOrEqual(t, cut.NewModelCalls, int64(1))
	require.Equal(t, "待发货", cut.Conclusion)
	require.Equal(t, replay.Replayed, modes(cut)["model/turn/0"])
	require.Equal(t, replay.Live, modes(cut)["model/turn/1"])

	require.Equal(t, before, logBytes(t, dir), "重放只读原日志")
}

func TestScriptedSessionReplay(t *testing.T) {
	if os.Getenv(childDir) != "" {
		childMain()
	}
	client := scriptedClient()
	exercise(t, client, "TestScriptedSessionReplay", true)
}

func TestLiveSessionReplay(t *testing.T) {
	if os.Getenv(childDir) != "" {
		childMain()
	}
	client := livemodeltest.Require(t)
	t.Logf("模型：%s/%s", client.Live.Provider, client.Live.Model)
	exercise(t, client, "TestLiveSessionReplay", false)
}
