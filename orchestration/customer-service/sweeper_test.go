package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
	"github.com/kart-io/goagent/core/durable"
)

// restart 关闭并以同一目录重新打开宿主，模拟进程重启。
func (e *hostEnv) restart() {
	e.t.Helper()
	require.NoError(e.t, e.host.Close())
	e.open()
}

func (e *hostEnv) sweep() SweepReport {
	e.t.Helper()
	r, err := e.host.Sweep(context.Background())
	require.NoError(e.t, err)
	return r
}

const approveInput = `{"approved":true,"note":"核实无误","approver":"fin-1"}`

func TestSweepReviewReminderAndTimeout(t *testing.T) {
	e := newHostEnv(t, nil)
	r := e.message("m1", "c-1001", "订单 A1002 申请退款 800 元")
	require.Equal(t, SweepReport{}, e.sweep(), "未到提醒时间什么都不做")

	e.clock.Advance(48*time.Hour - 3*time.Hour)
	require.Equal(t, 1, e.sweep().Reminded)
	require.ElementsMatch(t, []string{"lead-1", "fin-1", "fin-2"}, recipients(e.sent(support.KindReminder)), "临近超时提醒每位审核人")
	require.Zero(t, e.sweep().Reminded, "提醒只发一次")

	e.clock.Advance(3 * time.Hour)
	require.Equal(t, 1, e.sweep().TimedOut)
	require.Equal(t, statusDelivered, e.row(r.TicketID).Status)
	require.Zero(t, e.refunds("A1002"), "超时按拒绝处理")
	replies := e.sent(support.KindCustomerReply)
	require.Len(t, replies, 1)
	require.Contains(t, replies[0].Message.Body, "审核超时")
	require.Len(t, e.sent(support.KindReminder), 3)
}

func TestSweepAskTimeout(t *testing.T) {
	e := newHostEnv(t, nil)
	r := e.message("m1", "c-1001", "我要退款 30 元")
	e.clock.Advance(47 * time.Hour)
	require.Zero(t, e.sweep().TimedOut)
	e.clock.Advance(time.Hour)
	require.Equal(t, 1, e.sweep().TimedOut)
	require.Equal(t, statusDelivered, e.row(r.TicketID).Status)
	require.Contains(t, e.sent(support.KindCustomerReply)[0].Message.Body, "客户未提供订单号", "追问超时按现有信息结束")
}

func TestAnswerAndTimeoutRace(t *testing.T) {
	timeout := json.RawMessage(`{"approved":false,"reason":"timeout","answered_at":"2026-11-07T08:00:00Z"}`)
	t.Run("先批准后超时", func(t *testing.T) {
		e := newHostEnv(t, nil)
		row := e.row(e.message("m1", "c-1001", "订单 A1002 申请退款 800 元").TicketID)
		a, err := e.host.respondReview(context.Background(), row, "fin-1", json.RawMessage(approveInput))
		require.NoError(t, err)
		require.False(t, a.Late)
		late, err := e.host.respondReview(context.Background(), row, "", timeout)
		require.NoError(t, err, "后到的超时按已有结果处理，不当作失败")
		require.True(t, late.Late)
		require.Equal(t, 1, e.refunds("A1002"), "结果以先到者为准")
		require.Zero(t, e.row(row.ID).Failures)
	})
	t.Run("先超时后批准", func(t *testing.T) {
		e := newHostEnv(t, nil)
		row := e.row(e.message("m1", "c-1001", "订单 A1002 申请退款 800 元").TicketID)
		_, err := e.host.respondReview(context.Background(), row, "", timeout)
		require.NoError(t, err)
		late, err := e.host.respondReview(context.Background(), row, "fin-1", json.RawMessage(approveInput))
		require.NoError(t, err)
		require.True(t, late.Late)
		require.Zero(t, e.refunds("A1002"))
		notices := e.sent(support.KindReviewResult)
		require.Len(t, notices, 1, "告知后到的审核人当前结果")
		require.Equal(t, "fin-1", notices[0].To.ID)
		require.Contains(t, notices[0].Message.Body, "审核超时，按拒绝处理（2026-11-07T08:00:00Z）")
	})
	t.Run("并发提交", func(t *testing.T) {
		e := newHostEnv(t, nil)
		row := e.row(e.message("m1", "c-1001", "订单 A1002 申请退款 800 元").TicketID)
		results := make([]AnswerResult, 2)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i, input := range []json.RawMessage{json.RawMessage(approveInput), timeout} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				approver := ""
				if i == 0 {
					approver = "fin-1"
				}
				results[i], errs[i] = e.host.respondReview(context.Background(), row, approver, input)
			}()
		}
		wg.Wait()
		require.NoError(t, errors.Join(errs...))
		require.NotEqual(t, results[0].Late, results[1].Late, "恰好一方后到")
		want := 0
		if results[1].Late {
			want = 1
		}
		require.Equal(t, want, e.refunds("A1002"), "结果以先到者为准")
		require.Len(t, e.sent(support.KindCustomerReply), 1)
	})
}

func TestSweepRunsQueuedMessageAsNewRound(t *testing.T) {
	e := newHostEnv(t, nil)
	e.store.arm(func(s *durable.Snapshot) bool { return s.Plan != "" }) // 首个检查点写入失败，运行停在 running
	_, err := e.host.Message(context.Background(), "c-1001", "agent-1", "m1", "订单 A1001 申请退款 30 元")
	require.ErrorIs(t, err, errInjected)
	q := e.message("m2", "c-1001", "我的包裹物流到哪了")
	require.Equal(t, "queued", q.Action, "运行中到达的消息记入待处理队列")

	e.clock.Advance(time.Minute)
	require.Zero(t, e.sweep().Advanced, "未超过停滞阈值不重新推进")
	e.clock.Advance(2 * time.Minute)
	require.Equal(t, 1, e.sweep().Advanced)
	replies := e.sent(support.KindCustomerReply)
	require.Len(t, replies, 2, "第一轮结束后，排队的消息作为新一轮咨询处理")
	require.Contains(t, replies[0].Message.Body, "已退款")
	require.Contains(t, replies[1].Message.Body, "配送途中")
	row := e.row(q.TicketID)
	require.Equal(t, 2, row.Round)
	require.Equal(t, q.TicketID+"#2", row.RunID)
	require.Equal(t, statusDelivered, row.Status)
}

func TestSweepRunFailedHandoff(t *testing.T) {
	e := newHostEnv(t, nil)
	r := e.message("m1", "c-1001", "订单 A1002 申请退款 800 元")
	_, err := e.host.ledger.db.Exec("DELETE FROM orders WHERE order_id = 'A1002'") // 执行操作的不可恢复错误
	require.NoError(t, err)
	_, err = e.host.Answer(context.Background(), r.TicketID, "fin-1", true, "")
	require.ErrorContains(t, err, "读取订单 A1002")
	require.Equal(t, statusRunning, e.row(r.TicketID).Status)
	for range 2 {
		e.clock.Advance(3 * time.Minute)
		_, _ = e.host.Sweep(context.Background())
	}
	require.Equal(t, statusDelivered, e.row(r.TicketID).Status)
	transfers := e.handoff.Transfers()
	require.Len(t, transfers, 1)
	require.Equal(t, support.ReasonRunFailed, transfers[0].Context.Reason)
	e.clock.Advance(3 * time.Minute)
	e.sweep()
	require.Len(t, e.handoff.Transfers(), 1, "不再自动重试，只发一条最终消息")
	require.Len(t, e.sent(support.KindHandoffNotice), 1)
	require.Empty(t, e.sent(support.KindCustomerReply))
}

func TestSweepRecoversAfterCrash(t *testing.T) {
	t.Run("回执已写入而执行记录停在 pending", func(t *testing.T) {
		e := newHostEnv(t, nil)
		e.store.arm(operationSaved("refund/execute", "completed"))
		_, err := e.host.Message(context.Background(), "c-1001", "agent-1", "m1", "订单 A1001 申请退款 30 元")
		require.ErrorIs(t, err, errInjected)
		require.Equal(t, 1, e.refunds("A1001"))

		e.restart()
		e.clock.Advance(3 * time.Minute)
		require.Equal(t, 1, e.sweep().Advanced)
		require.Equal(t, 1, e.refunds("A1001"), "Reconcile 补记，不重复退款")
		require.Zero(t, e.host.desk.executions.Load(), "执行操作没有重跑")
		require.Len(t, e.sent(support.KindCustomerReply), 1)

		e.restart()
		e.sweep()
		require.Len(t, e.sent(""), 1, "Sweeper 重启后不重复投递")
	})
	t.Run("登记写入而登记记录停在 pending", func(t *testing.T) {
		e := newHostEnv(t, nil)
		e.store.arm(operationSaved("refund/register", "completed"))
		_, err := e.host.Message(context.Background(), "c-1001", "agent-1", "m1", "订单 A1002 申请退款 800 元")
		require.ErrorIs(t, err, errInjected)

		e.restart()
		e.clock.Advance(3 * time.Minute)
		e.sweep()
		require.Zero(t, e.host.desk.registers.Load(), "Reconcile 补记，登记没有重跑")
		require.Len(t, e.sent(support.KindApprovalRequest), 3, "补记的判定仍为有争议，进入审核")
	})
}

func TestSweepRetriesApproverResolution(t *testing.T) {
	e := newHostEnv(t, nil)
	e.directory.Fail(errors.New("通讯录超时"))
	_, err := e.host.Message(context.Background(), "c-1001", "agent-1", "m1", "订单 A1002 申请退款 800 元")
	require.ErrorContains(t, err, "通讯录超时")
	row := e.row("tk-m1")
	require.Equal(t, statusWaitingReview, row.Status, "运行保持等待")
	require.Empty(t, e.sent(support.KindApprovalRequest))
	require.Len(t, e.sent(support.KindOpsAlert), 1, "告警运营")

	_, err = e.host.Sweep(context.Background())
	require.ErrorContains(t, err, "通讯录超时")
	require.Len(t, e.sent(support.KindOpsAlert), 1, "同一中断只告警一次")

	e.directory.Fail(nil)
	require.Equal(t, 1, e.sweep().Resolved)
	require.Len(t, e.sent(support.KindApprovalRequest), 3)
	require.Equal(t, row.Deadline, e.row("tk-m1").Deadline, "审核截止时间不受影响")
}

func TestSweepCompensatesUsageAlert(t *testing.T) {
	e := newHostEnv(t, func(c *HostConfig) { c.DisableUsageHook, c.UsageAlertAt = true, 2 })
	e.message("m1", "c-1001", "订单 A1002 申请退款 800 元")
	require.Empty(t, e.sent(support.KindUsageAlert), "关闭 UsageHook 后运行中不告警")
	e.sweep()
	require.Len(t, e.sent(support.KindUsageAlert), 1, "扫描读根预算统计补偿")
	e.sweep()
	require.Len(t, e.sent(support.KindUsageAlert), 1, "按幂等键去重")
}

func TestLateAnswerNotifiesCurrentResult(t *testing.T) {
	answer := func(e *hostEnv, ticketID, approver string, approved bool) AnswerResult {
		t.Helper()
		r, err := e.host.Answer(context.Background(), ticketID, approver, approved, "")
		require.NoError(t, err, "工单已结束时返回已处理结果，不返回错误")
		require.True(t, r.Late)
		return r
	}
	t.Run("工单已超时后作答", func(t *testing.T) {
		e := newHostEnv(t, nil)
		r := e.message("m1", "c-1001", "订单 A1002 申请退款 800 元")
		e.clock.Advance(48 * time.Hour)
		require.Equal(t, 1, e.sweep().TimedOut)
		got := answer(e, r.TicketID, "fin-1", true)
		require.Equal(t, "审核超时，按拒绝处理（2026-11-07T08:00:00Z）。", got.Result)
		notices := e.sent(support.KindReviewResult)
		require.Len(t, notices, 1, "当前结果通知一次")
		require.Equal(t, "fin-1", notices[0].To.ID)
		require.Contains(t, notices[0].Message.Body, "审核超时，按拒绝处理")
		require.Zero(t, e.refunds("A1002"), "后到的批准不生效")
	})
	t.Run("已被他人批准后作答", func(t *testing.T) {
		e := newHostEnv(t, nil)
		r := e.message("m1", "c-1001", "订单 A1002 申请退款 800 元")
		e.clock.Advance(time.Hour)
		_, err := e.host.Answer(context.Background(), r.TicketID, "fin-2", true, "核实无误")
		require.NoError(t, err)
		got := answer(e, r.TicketID, "lead-1", false)
		require.Equal(t, "fin-2 已于 2026-11-05T09:00:00Z 批准。", got.Result)
		notices := e.sent(support.KindReviewResult)
		require.Len(t, notices, 1)
		require.Equal(t, "lead-1", notices[0].To.ID)
		require.Equal(t, 1, e.refunds("A1002"))

		_, err = e.host.Answer(context.Background(), r.TicketID, "stranger", true, "")
		require.ErrorIs(t, err, ErrNotApprover, "不在名单中的人不会收到结果")
		require.Len(t, e.sent(support.KindReviewResult), 1)
	})
	t.Run("同一人重复作答不重复发", func(t *testing.T) {
		e := newHostEnv(t, nil)
		r := e.message("m1", "c-1001", "订单 A1002 申请退款 800 元")
		_, err := e.host.Answer(context.Background(), r.TicketID, "fin-2", false, "凭证不足")
		require.NoError(t, err)
		for range 2 {
			got := answer(e, r.TicketID, "fin-2", true)
			require.Equal(t, "fin-2 已于 2026-11-05T08:00:00Z 拒绝：凭证不足。", got.Result)
		}
		require.Len(t, e.sent(support.KindReviewResult), 1, "幂等键为工单 + 中断键 + 审核人")
		require.Zero(t, e.refunds("A1002"))
	})
	t.Run("没有审核的工单", func(t *testing.T) {
		e := newHostEnv(t, nil)
		r := e.message("m1", "c-1001", "订单 A1001 申请退款 30 元")
		_, err := e.host.Answer(context.Background(), r.TicketID, "fin-1", true, "")
		require.ErrorIs(t, err, ErrNotWaitingReview)
		require.Empty(t, e.sent(support.KindReviewResult))
	})
}
