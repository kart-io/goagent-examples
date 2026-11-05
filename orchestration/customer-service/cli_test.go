package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// cli 执行一条命令，返回最后一个 JSON 结果与之前打印的通知行。
func cli(t *testing.T, args ...string) (map[string]any, []map[string]any, error) {
	t.Helper()
	var out bytes.Buffer
	err := execute(context.Background(), args, &out)
	dec := json.NewDecoder(&out)
	var docs []map[string]any
	for dec.More() {
		var v map[string]any
		require.NoError(t, dec.Decode(&v))
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
