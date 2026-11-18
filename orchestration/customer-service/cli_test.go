package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kart-io/goagent/codec/json"

	"github.com/stretchr/testify/require"
)

// cli 执行一条命令，返回最后一个 JSON 结果与之前打印的通知行。
func cli(t *testing.T, args ...string) (map[string]any, []map[string]any, error) {
	t.Helper()
	var out bytes.Buffer
	err := execute(context.Background(), args, &out)
	dec := json.NewDecoder(&out)
	var docs []map[string]any
	for dec.PeekKind() != 0 { // 0 表示输入结束
		var v map[string]any
		require.NoError(t, json.DecodeNext(dec, &v))
		docs = append(docs, v)
	}
	if len(docs) == 0 {
		return nil, nil, err
	}
	return docs[len(docs)-1], docs[:len(docs)-1], err
}

func TestCLIDisputedRefundFlow(t *testing.T) {
	t.Setenv("FEISHU_APP_ID", "")
	dir := t.TempDir()
	res, events, err := cli(t, "message", "-dir", dir, "-now", "2026-11-05T08:00:00Z", "-id", "m1", "-customer", "c-1001", "-text", "订单 A1002 申请退款 800 元")
	require.NoError(t, err)
	require.Equal(t, "waiting_review", res["status"])
	require.Len(t, events, 3, "上级与财务组两人各一条审核消息")

	show, _, err := cli(t, "show", "-dir", dir, "-ticket", "tk-m1")
	require.NoError(t, err)
	require.Equal(t, "refund-review", show["wait_id"])
	require.Len(t, show["approvers"], 3)

	_, _, err = cli(t, "answer", "-dir", dir, "-ticket", "tk-m1", "-approver", "stranger", "-approve")
	require.ErrorIs(t, err, ErrNotApprover)

	res, events, err = cli(t, "answer", "-dir", dir, "-now", "2026-11-05T09:00:00Z", "-ticket", "tk-m1", "-approver", "fin-1", "-approve", "-note", "核实无误")
	require.NoError(t, err)
	require.Equal(t, "delivered", res["status"])
	require.Len(t, events, 1)
	require.Equal(t, "customer_reply", events[0]["kind"])
	require.Contains(t, events[0]["body"], "已退款 80000 分")
}

func TestCLISweepTimeout(t *testing.T) {
	t.Setenv("FEISHU_APP_ID", "")
	dir := t.TempDir()
	_, _, err := cli(t, "message", "-dir", dir, "-now", "2026-11-05T08:00:00Z", "-id", "m1", "-customer", "c-1001", "-text", "订单 A1001 申请退款 30 元，我要投诉")
	require.NoError(t, err)
	res, events, err := cli(t, "sweep", "-dir", dir, "-now", "2026-11-07T08:00:00Z")
	require.NoError(t, err)
	require.EqualValues(t, 1, res["timed_out"])
	require.Len(t, events, 1)
	require.Contains(t, events[0]["body"], "审核超时")
}

func TestCLIRejectsInvalidUsage(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{},
		{"message", "-customer", "c-1001", "-text", "x"},
		{"message", "-dir", dir, "-text", "x"},
		{"answer", "-dir", dir, "-ticket", "tk-1", "-approver", "fin-1"},
		{"answer", "-dir", dir, "-ticket", "tk-1", "-approver", "fin-1", "-approve", "-reject"},
		{"show", "-dir", dir},
		{"nope", "-dir", dir},
		{"sweep", "-dir", dir, "-now", "明天"},
	} {
		_, _, err := cli(t, args...)
		require.Error(t, err, strings.Join(args, " "))
	}
}

func TestCLIServeRejectsBusyAddr(t *testing.T) {
	t.Setenv("FEISHU_APP_ID", "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	done := make(chan error, 1)
	go func() {
		_, _, err := cli(t, "serve", "-dir", t.TempDir(), "-addr", ln.Addr().String())
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("端口被占用时 serve 没有返回")
	}
}

func TestCLIMemoryRequiresStores(t *testing.T) {
	t.Setenv("FEISHU_APP_ID", "")
	t.Setenv("CS_MEMORY_PG_URL", "")
	t.Setenv("CS_MEMORY_QDRANT_ADDR", "")
	const want = "-memory 需要 -pg 与 -qdrant（或环境变量 CS_MEMORY_PG_URL、CS_MEMORY_QDRANT_ADDR）"
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"缺 pg 与 qdrant", []string{"-memory"}},
		{"缺 pg", []string{"-memory", "-qdrant", "127.0.0.1:1"}},
		{"缺 qdrant", []string{"-memory", "-pg", "postgres://x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"message", "-dir", t.TempDir(), "-customer", "c-1", "-text", "你好"}, tc.args...)
			_, _, err := cli(t, args...)
			require.EqualError(t, err, want)
		})
	}
}

func TestCLIMemoryShow(t *testing.T) {
	t.Setenv("FEISHU_APP_ID", "")
	pg, qd := os.Getenv("CS_MEMORY_PG_URL"), os.Getenv("CS_MEMORY_QDRANT_ADDR")
	if pg == "" || qd == "" {
		t.Skipf("未设置 CS_MEMORY_PG_URL 与 CS_MEMORY_QDRANT_ADDR，请经 scripts/with-stores.sh 运行")
	}
	dir, customer := t.TempDir(), "c-"+uuid.NewString()
	mem := []string{"-memory", "-pg", pg, "-qdrant", qd}
	_, _, err := cli(t, append([]string{"message", "-dir", dir, "-id", msgID(), "-customer", customer, "-text", "请记住：只用邮件联系我"}, mem...)...)
	require.NoError(t, err)

	res, _, err := cli(t, append([]string{"memory", "-dir", dir, "-customer", customer}, mem...)...)
	require.NoError(t, err)
	require.Len(t, res["pinned"], 1)
	require.Equal(t, []any{}, res["matches"])
	oldID := res["pinned"].([]any)[0].(map[string]any)["id"].(string)

	_, _, err = cli(t, "memory", "-dir", dir, "-customer", customer)
	require.Error(t, err, "未给 -memory 时 memory 命令应报错")

	t.Run("-id 看得到被取代的旧记录", func(t *testing.T) {
		_, _, err := cli(t, append([]string{"message", "-dir", dir, "-id", msgID(), "-customer", customer, "-text", "邮件或短信都可以"}, mem...)...)
		require.NoError(t, err)
		_, _, err = cli(t, append([]string{"sweep", "-dir", dir}, mem...)...)
		require.NoError(t, err)
		res, _, err := cli(t, append([]string{"memory", "-dir", dir, "-customer", customer}, mem...)...)
		require.NoError(t, err)
		require.Len(t, res["pinned"], 1)
		newID := res["pinned"].([]any)[0].(map[string]any)["id"].(string)
		require.NotEqual(t, oldID, newID)

		old, _, err := cli(t, append([]string{"memory", "-dir", dir, "-customer", customer, "-id", oldID}, mem...)...)
		require.NoError(t, err)
		require.Equal(t, oldID, old["id"])
		require.Equal(t, newID, old["superseded_by"], "旧记录指向继承钉住的新记录")
		require.NotEmpty(t, old["invalid_at"])
	})

	t.Run("-id 不跨客户", func(t *testing.T) {
		_, _, err := cli(t, append([]string{"memory", "-dir", dir, "-customer", "c-" + uuid.NewString(), "-id", oldID}, mem...)...)
		require.Error(t, err)
	})

	t.Run("-id 与 -q 二选一", func(t *testing.T) {
		_, _, err := cli(t, append([]string{"memory", "-dir", dir, "-customer", customer, "-id", oldID, "-q", "邮件"}, mem...)...)
		require.EqualError(t, err, "memory 的 -id 与 -q 只能二选一")
	})
}

// 通知与转人工逐行输出，每行恰好一个 JSON 对象，便于按行消费（与命令结果分开）。
func TestCLIEventsOnePerLine(t *testing.T) {
	t.Setenv("FEISHU_APP_ID", "")
	var out bytes.Buffer
	err := execute(context.Background(), []string{"message", "-dir", t.TempDir(), "-now", "2026-11-05T08:00:00Z", "-id", "m1", "-customer", "c-1001", "-text", "订单 A1002 申请退款 800 元"}, &out)
	require.NoError(t, err)
	lines := strings.Split(out.String(), "\n")
	for _, line := range lines[:3] {
		var v map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &v), "每条通知独占一行：%q", line)
		require.Equal(t, "notify", v["event"])
	}
}
