package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/workflow"
)

// graphEnv 在 deskEnv 之上装配持久图与根预算，不经宿主直接运行一个工单。
type graphEnv struct {
	*deskEnv
	budgetDir string
	graph     *workflow.DurableGraph[Ticket]
}

func newGraphEnv(t *testing.T) *graphEnv {
	t.Helper()
	e := &graphEnv{deskEnv: newDeskEnv(t), budgetDir: t.TempDir()}
	g, err := newGraph(context.Background(), e.store, offlineModel(), e.desk)
	require.NoError(t, err)
	e.graph = g
	return e
}

// runCtx 返回带根预算的上下文；RootKey 必须等于运行 ID。
func (e *graphEnv) runCtx(runID string) context.Context {
	e.t.Helper()
	store, err := durable.NewFileStore(e.budgetDir)
	require.NoError(e.t, err)
	c, err := budget.NewDurable(budget.Config{RootKey: runID, Version: graphVersion, MaxModelRequests: 40}, store)
	require.NoError(e.t, err)
	return budget.WithController(context.Background(), c)
}

func (e *graphEnv) start(runID, customer, input string) (*workflow.GraphSnapshot[Ticket], error) {
	return e.graph.Run(e.runCtx(runID), runID, Ticket{TicketID: "t-" + runID, CustomerID: customer, Submitter: "agent-1", Input: input}, "")
}

func (e *graphEnv) resume(runID, key, input string) (*workflow.GraphSnapshot[Ticket], error) {
	return e.graph.Resume(e.runCtx(runID), runID, workflow.ResumeRequest{Key: key, Input: json.RawMessage(input)})
}

// waitAt 运行到唯一的中断并返回它。
func (e *graphEnv) waitAt(runID, customer, input, id string) workflow.InterruptPoint {
	e.t.Helper()
	snap, err := e.start(runID, customer, input)
	require.ErrorIs(e.t, err, durable.ErrApprovalRequired)
	require.Len(e.t, snap.Interrupts, 1)
	require.Equal(e.t, id, snap.Interrupts[0].ID)
	return snap.Interrupts[0]
}

func (e *graphEnv) refundCount(order string) int {
	e.t.Helper()
	n, _, err := e.ledger.refunds(context.Background(), order)
	require.NoError(e.t, err)
	return n
}

func TestGraphDirectRefund(t *testing.T) {
	e := newGraphEnv(t)
	snap, err := e.start("r1", "c-1001", "订单 A1001 申请退款 30 元")
	require.NoError(t, err)
	require.True(t, snap.Done)
	require.Empty(t, snap.Interrupts, "无争议时不出现审核中断")
	require.Equal(t, "refunded", snap.State.Refund.Status)
	require.Contains(t, finalReply(snap.State), "已退款 3000 分")
	require.Equal(t, 1, e.refundCount("A1001"))
	var actor string
	require.NoError(t, e.ledger.db.QueryRow("SELECT approved_by FROM refund_requests").Scan(&actor))
	require.Equal(t, systemActor, actor, "无争议时由系统主体批准")
}

func TestGraphDisputedReview(t *testing.T) {
	approve := `{"approved":true,"note":"核实无误","approver":"lead-1"}`
	t.Run("批准后退款一次", func(t *testing.T) {
		e := newGraphEnv(t)
		point := e.waitAt("r1", "c-1001", "订单 A1002 申请退款 800 元", reviewInterrupt)
		var info reviewInfo
		require.NoError(t, json.Unmarshal(point.Info, &info))
		require.Equal(t, "A1002", info.OrderID)
		require.EqualValues(t, 80000, info.AmountCents)
		require.Equal(t, "agent-1", info.Submitter)
		require.Contains(t, strings.Join(info.Reasons, ";"), "超过自动退款上限")
		require.Equal(t, testStart.Add(48*time.Hour).Format(time.RFC3339), info.ReviewDeadline)
		require.Zero(t, e.refundCount("A1002"), "等待期间不退款")

		_, err := e.resume("r1", point.Key, `{"approved":true,"note":"x","approver":"system:auto-policy"}`)
		var rejected *workflow.ResumeRejectedError
		require.ErrorAs(t, err, &rejected, "人工审核不能以系统主体批准")

		e.allow(point.Key, info.RequestID, support.User{ID: "lead-1"})
		snap, err := e.resume("r1", point.Key, approve)
		require.NoError(t, err)
		require.True(t, snap.Done)
		require.Equal(t, "refunded", snap.State.Refund.Status)
		require.Equal(t, 1, e.refundCount("A1002"))
		require.EqualValues(t, 1, e.desk.registers.Load(), "登记从日志回放")
	})
	t.Run("拒绝与超时不退款并给出替代方案", func(t *testing.T) {
		for name, input := range map[string]string{
			"declined": `{"approved":false,"note":"凭证不足","approver":"lead-1"}`,
			"timeout":  `{"approved":false,"reason":"timeout"}`,
		} {
			t.Run(name, func(t *testing.T) {
				e := newGraphEnv(t)
				point := e.waitAt("r1", "c-1001", "订单 A1002 申请退款 800 元", reviewInterrupt)
				snap, err := e.resume("r1", point.Key, input)
				require.NoError(t, err)
				require.Equal(t, name, snap.State.Refund.Status)
				require.Zero(t, e.refundCount("A1002"))
				last := snap.State.Solutions[len(snap.State.Solutions)-1]
				require.Equal(t, "补充凭证后重新申请", last.Title)
				require.True(t, last.RequiresApproval)
			})
		}
	})
	t.Run("Agent 标记有争议时进入审核", func(t *testing.T) {
		e := newGraphEnv(t)
		point := e.waitAt("r1", "c-1001", "订单 A1001 申请退款 30 元，不处理我就投诉", reviewInterrupt)
		var info reviewInfo
		require.NoError(t, json.Unmarshal(point.Info, &info))
		require.Equal(t, []string{"Agent 标记有争议：客户表示要投诉"}, info.Reasons)
	})
	t.Run("审核期间订单变化", func(t *testing.T) {
		e := newGraphEnv(t)
		point := e.waitAt("r1", "c-1001", "订单 A1002 申请退款 800 元", reviewInterrupt)
		var info reviewInfo
		require.NoError(t, json.Unmarshal(point.Info, &info))
		e.allow(point.Key, info.RequestID, support.User{ID: "lead-1"})
		_, err := e.ledger.db.Exec("UPDATE orders SET version = 2 WHERE order_id = 'A1002'")
		require.NoError(t, err)
		snap, err := e.resume("r1", point.Key, approve)
		require.NoError(t, err)
		require.True(t, snap.Done, "业务结果不是节点错误，运行正常结束")
		require.Equal(t, "order_changed", snap.State.Refund.Status)
		require.Contains(t, finalReply(snap.State), "订单已变化，需重新申请")
		require.Zero(t, e.refundCount("A1002"))
	})
}

func TestGraphForeignOrderAndNoRefund(t *testing.T) {
	cases := []struct{ name, input, status, reply string }{
		{"他人订单", "订单 A2001 申请退款 30 元", "no_refund", "未找到该订单"},
		{"无需退款", "订单 A1001 退款的事不用退了", "no_refund", "客户表示不需要退款"},
		{"提议无效", "订单 A1001 申请退款，胡乱提议", "no_refund", "退款提议无法解析"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newGraphEnv(t)
			snap, err := e.start("r1", "c-1001", c.input)
			require.NoError(t, err)
			require.True(t, snap.Done)
			require.Equal(t, c.status, snap.State.Refund.Status)
			require.Contains(t, finalReply(snap.State), c.reply)
			var n int
			require.NoError(t, e.ledger.db.QueryRow("SELECT count(*) FROM refund_requests").Scan(&n))
			require.Zero(t, n, "不登记")
			require.Zero(t, e.desk.registers.Load())
		})
	}
}

func TestGraphAskCustomer(t *testing.T) {
	t.Run("客户回复后续跑", func(t *testing.T) {
		e := newGraphEnv(t)
		point := e.waitAt("r1", "c-1001", "我要退款 30 元", askInterrupt)
		require.JSONEq(t, `{"question":"请提供需要退款的订单号"}`, string(point.Info))
		_, err := e.resume("r1", point.Key, `{"answer":""}`)
		var rejected *workflow.ResumeRejectedError
		require.ErrorAs(t, err, &rejected, "空回答保持等待")
		snap, err := e.resume("r1", point.Key, `{"answer":"订单号是 A1001"}`)
		require.NoError(t, err)
		require.Equal(t, "refunded", snap.State.Refund.Status)
		require.Equal(t, 1, e.refundCount("A1001"))
	})
	t.Run("追问超时按现有信息结束", func(t *testing.T) {
		e := newGraphEnv(t)
		point := e.waitAt("r1", "c-1001", "我要退款 30 元", askInterrupt)
		snap, err := e.resume("r1", point.Key, `{"timeout":true}`)
		require.NoError(t, err)
		require.True(t, snap.Done)
		require.Equal(t, "no_refund", snap.State.Refund.Status)
		require.Contains(t, finalReply(snap.State), "客户未提供订单号")
	})
	t.Run("同一时刻只允许一个未回答的追问", func(t *testing.T) {
		e := newGraphEnv(t)
		point := e.waitAt("r1", "c-1001", "我要退款 30 元，两次追问", askInterrupt)
		snap, err := e.resume("r1", point.Key, `{"answer":"A1001"}`)
		require.NoError(t, err)
		require.Equal(t, "refunded", snap.State.Refund.Status)
	})
}

func TestGraphGeneralFallbackAndRounds(t *testing.T) {
	t.Run("通用咨询", func(t *testing.T) {
		e := newGraphEnv(t)
		snap, err := e.start("r1", "c-1001", "我的包裹物流到哪了")
		require.NoError(t, err)
		require.Equal(t, []string{triageName, generalName, triageName}, agentsOf(snap.State))
		require.Contains(t, finalReply(snap.State), "配送途中")
		require.Equal(t, "咨询答复", snap.State.Solutions[0].Title)
	})
	t.Run("分流无效经 Fallback", func(t *testing.T) {
		e := newGraphEnv(t)
		snap, err := e.start("r1", "c-1001", "乱码乱码")
		require.NoError(t, err)
		require.True(t, snap.Done)
		require.Equal(t, OutcomeHandoff, snap.State.Outcome)
		require.Empty(t, snap.State.Route)
	})
	t.Run("轮数超限返回 ErrMaxSteps", func(t *testing.T) {
		e := newGraphEnv(t)
		snap, err := e.start("r1", "c-1001", "反复咨询物流")
		require.ErrorIs(t, err, workflow.ErrMaxSteps)
		require.Equal(t, maxRounds, strings.Count(strings.Join(agentsOf(snap.State), ","), generalName))
	})
}

func agentsOf(t Ticket) []string {
	out := make([]string, len(t.Turns))
	for i, turn := range t.Turns {
		out[i] = turn.Agent
	}
	return out
}
