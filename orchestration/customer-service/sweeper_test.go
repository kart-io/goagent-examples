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
	require.NoError(t, err, "解析失败以运营告警为信号，不作为错误返回")
	row := e.row("tk-m1")
	require.Equal(t, statusWaitingReview, row.Status, "运行保持等待")
	require.Empty(t, e.sent(support.KindApprovalRequest))
	require.Len(t, e.sent(support.KindOpsAlert), 1, "告警运营")

	require.Equal(t, 1, e.sweep().Resolved, "解析仍失败时继续等待")
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

func TestSweepRecoversFixedAnswerStuckInWaitingReview(t *testing.T) {
	e := newHostEnv(t, nil)
	r := e.message("m1", "c-1001", "订单 A1002 申请退款 800 元")
	e.store.arm(operationSaved("refund/execute", "pending"))
	_, err := e.host.Answer(context.Background(), r.TicketID, "fin-1", true, "核实无误")
	require.ErrorIs(t, err, errInjected)
	remindersBefore := len(e.sent(support.KindReminder))
	_, err = e.host.ledger.db.Exec("UPDATE tickets SET status = ? WHERE ticket_id = ?", statusWaitingReview, r.TicketID) // 回答已固定，宿主在写回工单前崩溃
	require.NoError(t, err)

	e.restart()
	e.clock.Advance(time.Minute)
	rep := e.sweep()
	require.Zero(t, rep.Advanced+rep.Reminded+rep.TimedOut, "回答已固定但未停滞：既不提醒也不超时")
	e.clock.Advance(2 * time.Minute)
	require.Equal(t, 1, e.sweep().Advanced)
	e.clock.Advance(24 * time.Hour)
	rep = e.sweep()
	require.Zero(t, rep.Reminded+rep.TimedOut)

	require.Equal(t, statusDelivered, e.row(r.TicketID).Status)
	require.Equal(t, 1, e.refunds("A1002"))
	require.Len(t, e.sent(support.KindCustomerReply), 1)
	require.Len(t, e.sent(support.KindReminder), remindersBefore, "回答固定后不再提醒")
}

func TestSweepReviewTimesOutWhileApproverUnresolved(t *testing.T) {
	e := newHostEnv(t, nil)
	e.directory.Fail(errors.New("通讯录超时"))
	r, err := e.host.Message(context.Background(), "c-1001", "agent-1", "m1", "订单 A1002 申请退款 800 元")
	require.NoError(t, err, "工单已正确进入等待审核并告警运营，解析失败不作为错误返回给消息调用方")
	require.Equal(t, statusWaitingReview, r.Status)
	require.Len(t, e.sent(support.KindOpsAlert), 1)

	e.clock.Advance(47 * time.Hour)
	rep := e.sweep()
	require.Equal(t, 1, rep.Resolved, "截止前继续重试解析")
	require.Zero(t, rep.TimedOut)

	e.clock.Advance(time.Hour)
	rep = e.sweep()
	require.Equal(t, 1, rep.TimedOut, "审核截止时间不受解析失败影响，到期按超时拒绝")
	require.Zero(t, rep.Resolved)
	require.Equal(t, statusDelivered, e.row(r.TicketID).Status)
	require.Zero(t, e.refunds("A1002"))
	replies := e.sent(support.KindCustomerReply)
	require.Len(t, replies, 1)
	require.Contains(t, replies[0].Message.Body, "审核超时")
	require.Empty(t, e.sent(support.KindApprovalRequest))

	e.clock.Advance(time.Hour)
	require.Equal(t, SweepReport{}, e.sweep())
	require.Len(t, e.sent(support.KindCustomerReply), 1, "超时回复只发一次")
}

func TestSweepRetriesFailedRegisterWithoutHandoff(t *testing.T) {
	e := newHostEnv(t, nil)
	e.host.ledger.beforeCommit = func() error { e.host.ledger.beforeCommit = nil; return errors.New("database is locked") }
	_, err := e.host.Message(context.Background(), "c-1001", "agent-1", "m1", "订单 A1002 申请退款 800 元")
	require.ErrorContains(t, err, "database is locked")
	require.Equal(t, statusRunning, e.row("tk-m1").Status)

	e.clock.Advance(3 * time.Minute)
	require.Equal(t, 1, e.sweep().Advanced)
	require.Equal(t, statusWaitingReview, e.row("tk-m1").Status, "登记重跑成功，照常进入审核")
	require.Len(t, e.sent(support.KindApprovalRequest), 3)
	require.Empty(t, e.handoff.Transfers(), "不因登记失败按 run_failed 转人工")
}

func TestCustomerAnswerSurvivesResumeFailure(t *testing.T) {
	setup := func(t *testing.T) (*hostEnv, string) {
		e := newHostEnv(t, nil)
		r := e.message("m1", "c-1001", "我要退款 30 元")
		require.Equal(t, statusWaitingCustomer, r.Status)
		e.store.arm(func(s *durable.Snapshot) bool { return s != nil }) // 续跑的第一次保存失败
		_, err := e.host.Message(context.Background(), "c-1001", "agent-1", "m2", "订单号是 A1001")
		require.ErrorIs(t, err, errInjected)
		require.Zero(t, e.refunds("A1001"))
		return e, r.TicketID
	}
	finished := func(t *testing.T, e *hostEnv, ticketID string) {
		require.Equal(t, statusDelivered, e.row(ticketID).Status)
		require.Equal(t, 1, e.refunds("A1001"), "回答没有丢失，退款恰好一次")
		replies := e.sent(support.KindCustomerReply)
		require.Len(t, replies, 1)
		require.Contains(t, replies[0].Message.Body, "已退款")
		require.Len(t, e.sent(support.KindCustomerQuestion), 1, "不重复追问")

		again := e.message("m2", "c-1001", "订单号是 A1001")
		require.Equal(t, "duplicate", again.Action, "回答已处理后重投不再生效")
		require.Equal(t, 1, e.refunds("A1001"))
		e.clock.Advance(3 * time.Minute)
		e.sweep()
		require.Len(t, e.sent(support.KindCustomerReply), 1, "回答不会被当作新一轮咨询")
	}
	t.Run("扫描以未处理的回答续跑", func(t *testing.T) {
		e, id := setup(t)
		e.clock.Advance(3 * time.Minute)
		e.sweep() // 停滞的 running 重新推进，回到等待追问
		e.sweep() // 等待追问且有未处理的回答：以它续跑
		finished(t, e, id)
	})
	t.Run("回答已固定后续跑失败", func(t *testing.T) {
		e := newHostEnv(t, nil)
		r := e.message("m1", "c-1001", "我要退款 30 元")
		e.store.arm(operationSaved("model/turn/1", "pending")) // 追问已记下回答，之后的模型调用记录保存失败
		_, err := e.host.Message(context.Background(), "c-1001", "agent-1", "m2", "订单号是 A1001")
		require.ErrorIs(t, err, errInjected)
		var handled int
		require.NoError(t, e.host.ledger.db.QueryRow("SELECT handled FROM ticket_messages WHERE message_id = 'm2'").Scan(&handled))
		require.Equal(t, 1, handled, "中断已记下回答，消息记为已处理")
		e.clock.Advance(3 * time.Minute)
		e.sweep() // 以 Run 续跑已固定的回答
		finished(t, e, r.TicketID)
	})
	t.Run("重投同一消息续跑", func(t *testing.T) {
		e, id := setup(t)
		e.clock.Advance(3 * time.Minute)
		e.sweep()
		r := e.message("m2", "c-1001", "订单号是 A1001")
		require.Equal(t, "answered", r.Action, "未处理的回答重投时再次续跑")
		finished(t, e, id)
	})
}

func TestPendingAnswerBoundToItsAsk(t *testing.T) {
	pending := func(t *testing.T, e *hostEnv, id, ticketID, text string) {
		_, err := e.host.ledger.db.Exec("INSERT INTO ticket_messages VALUES (?, ?, (SELECT COALESCE(MAX(seq),0)+1 FROM ticket_messages), ?, ?)",
			id, ticketID, text, answerPending)
		require.NoError(t, err)
	}
	handled := func(t *testing.T, e *hostEnv, id string) int {
		var n int
		require.NoError(t, e.host.ledger.db.QueryRow("SELECT handled FROM ticket_messages WHERE message_id = ?", id).Scan(&n))
		return n
	}
	t.Run("遗留的待确认回答不用于新追问", func(t *testing.T) {
		e := newHostEnv(t, nil)
		r := e.message("m1", "c-1001", "我要退款 30 元")
		row := e.row(r.TicketID)
		snap, err := e.host.graph.Snapshot(context.Background(), row.RunID)
		require.NoError(t, err)
		require.Len(t, snap.Interrupts, 1)
		// 构造：上一个追问已续跑成功但没来得及记为已处理，留下待确认回答；运行随后发起的追问被首次观察到。
		_, err = e.host.ledger.db.Exec("DELETE FROM events WHERE event_key = ?", row.WaitKey)
		require.NoError(t, err)
		pending(t, e, "old", r.TicketID, "订单号是 A1003")
		require.NoError(t, e.host.wait(context.Background(), row, snap.Interrupts[0]))
		require.Equal(t, 1, handled(t, e, "old"), "首次登记新追问时，此前的待确认回答记为已处理")

		e.message("m2", "c-1001", "订单号是 A1001")
		require.Zero(t, e.refunds("A1003"), "旧回答不被用于新追问")
		require.Equal(t, 1, e.refunds("A1001"))
	})
	t.Run("续跑期间到达的新回答不被吞掉", func(t *testing.T) {
		e := newHostEnv(t, nil)
		r := e.message("m1", "c-1001", "我要退款 30 元")
		var once sync.Once
		var insertErr error
		e.store.arm(func(*durable.Snapshot) bool { // 续跑的第一次保存时插入一条更晚的待确认回答
			once.Do(func() {
				_, insertErr = e.host.ledger.db.Exec("INSERT INTO ticket_messages VALUES ('fresh', ?, (SELECT COALESCE(MAX(seq),0)+1 FROM ticket_messages), '订单号是 A1003', ?)",
					r.TicketID, answerPending)
			})
			return false
		})
		e.message("m2", "c-1001", "订单号是 A1001")
		require.NoError(t, insertErr)
		require.Equal(t, 1, handled(t, e, "m2"))
		require.Equal(t, answerPending, handled(t, e, "fresh"), "只把序号不晚于所用回答的待确认消息记为已处理")
	})
}
