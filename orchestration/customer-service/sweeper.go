package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
	"github.com/kart-io/goagent/constants"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/memory"
)

// SweepReport 汇总一次扫描做了什么。
type SweepReport struct {
	Advanced int `json:"advanced"`  // 重新推进的工单：停滞的 running，或以待确认回答续跑的追问
	Resolved int `json:"resolved"`  // 重新解析审核人的工单
	Reminded int `json:"reminded"`  // 发出提醒的工单
	TimedOut int `json:"timed_out"` // 以超时输入回应的工单
	// MemoryJobs 是本次处理的记忆抽取任务数（含失败与丢弃），MemoryDropped 是其间丢弃的任务数；未接入记忆时省略。
	MemoryJobs    int   `json:"memory_jobs,omitempty"`
	MemoryDropped int64 `json:"memory_dropped,omitempty"`
}

// tickets 读取指定状态的工单；先读完再处理，单连接下边遍历边写会死锁。
func (h *Host) tickets(ctx context.Context, statuses ...string) ([]ticketRow, error) {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",")
	args := make([]any, len(statuses))
	for i, s := range statuses {
		args[i] = s
	}
	rows, err := h.ledger.db.QueryContext(ctx, "SELECT "+ticketColumns+" FROM tickets WHERE status IN ("+marks+") ORDER BY ticket_id", args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ticketRow
	for rows.Next() {
		r, err := scanTicket(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Sweep 扫描未结束的工单：补偿用量告警，重试审核人解析，发提醒，按时限超时回应，
// 重新推进停滞的 running 工单（宿主中途退出），最后投递发件箱。时间取自宿主时钟。
func (h *Host) Sweep(ctx context.Context) (SweepReport, error) {
	var report SweepReport
	rows, err := h.tickets(ctx, statusRunning, statusWaitingReview, statusWaitingCustomer, statusDone, statusHandoff, statusFailed)
	if err != nil {
		return report, err
	}
	now := h.now()
	var errs []error
	for _, row := range rows {
		if err := h.compensateUsage(ctx, row); err != nil {
			errs = append(errs, fmt.Errorf("工单 %s 用量补偿: %w", row.ID, err))
		}
		if err := h.sweepTicket(ctx, row, now, &report); err != nil {
			errs = append(errs, fmt.Errorf("工单 %s: %w", row.ID, err))
		}
	}
	h.deliver(ctx)
	if h.memory != nil { // 抽取要调用模型，放在投递之后：抽取失败或变慢不耽误客户收到回复
		errs = append(errs, h.sweepMemory(ctx, &report))
	}
	return report, errors.Join(errs...)
}

// memoryScopes 是接入记忆的成员作用域，与 newGraph 一致。
var memoryScopes = []string{generalName, refundAgentScope}

// sweepMemory 处理待处理的记忆抽取任务。任务失败只记入任务，不作为 ProcessPending 的错误返回，
// 所以按本账本各工单的会话查出重试耗尽的任务，每个只报告一次。
func (h *Host) sweepMemory(ctx context.Context, report *SweepReport) error {
	m := h.memory.Memory
	dropped := m.DroppedJobs()
	n, err := m.ProcessPending(ctx)
	report.MemoryJobs, report.MemoryDropped = n, m.DroppedJobs()-dropped
	if err != nil {
		err = fmt.Errorf("记忆抽取: %w", err)
	}
	return errors.Join(err, h.memoryFailures(ctx))
}

// memoryFailures 报告本账本工单中新近失败的抽取任务；任务 Key 为 会话 ID/轮次起始序号。
func (h *Host) memoryFailures(ctx context.Context) error {
	rows, err := h.ledger.db.QueryContext(ctx, "SELECT ticket_id, customer_id FROM tickets ORDER BY ticket_id")
	if err != nil {
		return err
	}
	type owner struct{ ticket, customer string }
	var owners []owner
	for rows.Next() {
		var o owner
		if err := rows.Scan(&o.ticket, &o.customer); err != nil {
			_ = rows.Close()
			return err
		}
		owners = append(owners, o)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var errs []error
	for _, o := range owners {
		nsCtx := memory.WithNamespace(ctx, memoryNS(o.customer)...)
		for _, scope := range memoryScopes {
			id := memorySession(o.ticket, scope)
			events, err := h.memory.Sessions.Read(ctx, id, 0)
			if errors.Is(err, agentErrors.ErrSessionNotFound) {
				continue
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, ev := range events {
				if ev.Type != constants.SessionEventTurnStart {
					continue
				}
				key := id + "/" + strconv.FormatInt(ev.Seq, 10)
				job, ok, err := h.memory.Memory.Job(nsCtx, key)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				if !ok || job.Status != memory.JobFailed {
					continue
				}
				var first bool
				err = h.write(ctx, func(tx *sql.Tx) (err error) {
					first, err = claim(ctx, tx, "memory/failed/"+key, o.ticket)
					return err
				})
				if err == nil && first {
					err = fmt.Errorf("工单 %s 的记忆抽取任务 %s 重试 %d 次后失败: %s", o.ticket, key, job.Attempts, job.Error)
				}
				if err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

func (h *Host) sweepTicket(ctx context.Context, row ticketRow, now time.Time, report *SweepReport) error {
	switch row.Status {
	case statusRunning:
		last, err := time.Parse(time.RFC3339Nano, row.LastProgress)
		if err != nil || now.Sub(last) < h.cfg.StaleAfter {
			return err
		}
		report.Advanced++
		return h.advance(ctx, row, nil)
	case statusWaitingCustomer:
		answered, err := h.answerCustomer(ctx, row) // 客户已回答但上次续跑失败：先以回答续跑，再看超时
		if answered {
			report.Advanced++
		}
		if answered || err != nil {
			return err
		}
		readCtx, cancel := context.WithTimeout(ctx, h.cfg.LockTimeout)
		_, fixed, err := durable.InterruptAnswer(readCtx, h.cfg.Store, row.WaitKey, askInterrupt)
		cancel()
		if err != nil {
			return err
		}
		if fixed {
			// 回答已固定而工单仍停在等待追问（推进到上限时间或被取消，未写回）：停滞后重推，不按超时处理。
			last, err := time.Parse(time.RFC3339Nano, row.LastProgress)
			if err != nil || now.Sub(last) < h.cfg.StaleAfter {
				return err
			}
			report.Advanced++
			return h.advance(ctx, row, nil)
		}
		deadline, err := time.Parse(time.RFC3339, row.Deadline)
		if err != nil || now.Before(deadline) {
			return err
		}
		report.TimedOut++
		_, err = h.resume(ctx, row, json.RawMessage(`{"timeout":true}`))
		return err
	case statusWaitingReview:
		return h.sweepReview(ctx, row, now, report)
	}
	return nil
}

// sweepReview 处理审核等待：审核人未解析时重试，临近截止提醒审核人，到期以超时输入回应。
// 截止时间不受解析失败影响：审核人始终未解析时到期同样按超时处理，超时输入不需要审核人名单。
func (h *Host) sweepReview(ctx context.Context, row ticketRow, now time.Time, report *SweepReport) error {
	deadline, err := time.Parse(time.RFC3339, row.Deadline)
	if err != nil {
		return err
	}
	timeout := func() error {
		report.TimedOut++
		input, err := json.Marshal(reviewInput{Reason: "timeout", AnsweredAt: now.Format(time.RFC3339)})
		if err != nil {
			return err
		}
		_, err = h.respondReview(ctx, row, "", input)
		return err
	}
	done, err := claimed(ctx, h.ledger.db, row.WaitKey)
	if err != nil {
		return err
	}
	if !done {
		if !now.Before(deadline) {
			return timeout()
		}
		report.Resolved++
		readCtx, cancel := context.WithTimeout(ctx, h.cfg.LockTimeout)
		defer cancel()
		snap, err := h.graph.Snapshot(readCtx, row.RunID)
		if err != nil {
			return err
		}
		for _, p := range snap.Interrupts {
			if p.Key == row.WaitKey {
				return h.wait(ctx, row, p)
			}
		}
		return fmt.Errorf("运行 %s 不再等待中断 %s", row.RunID, row.WaitKey)
	}
	readCtx, cancel := context.WithTimeout(ctx, h.cfg.LockTimeout)
	_, answered, err := durable.InterruptAnswer(readCtx, h.cfg.Store, row.WaitKey, reviewInterrupt)
	cancel()
	if err != nil {
		return err
	}
	if answered {
		// 回答已固定而工单仍停在等待：作答方在写回前中断，停滞后重推；未停滞则交给进行中的作答。
		last, err := time.Parse(time.RFC3339Nano, row.LastProgress)
		if err != nil || now.Sub(last) < h.cfg.StaleAfter {
			return err
		}
		report.Advanced++
		return h.advance(ctx, row, nil)
	}
	if !now.Before(deadline) {
		return timeout()
	}
	if now.Before(deadline.Add(-h.cfg.RemindBefore)) {
		return nil
	}
	_, users, found, err := approvers(ctx, h.ledger.db, row.WaitKey)
	if err != nil || !found {
		return err
	}
	return h.write(ctx, func(tx *sql.Tx) error {
		first, err := claim(ctx, tx, row.WaitKey+"/reminder", row.ID)
		if err != nil || !first {
			return err
		}
		report.Reminded++
		for _, u := range users {
			body := "退款审核将于 " + row.Deadline + " 超时，超时按拒绝处理。"
			if err := notify(ctx, tx, row.ID, u, support.KindReminder, row.WaitKey+"/reminder/"+u.ID, "审核即将超时", body); err != nil {
				return err
			}
		}
		return nil
	})
}

// compensateUsage 以与运行完全相同的预算配置打开根预算，读取累计统计补偿 UsageHook 的遗漏；按幂等键去重。
func (h *Host) compensateUsage(ctx context.Context, row ticketRow) error {
	readCtx, cancel := context.WithTimeout(ctx, h.cfg.LockTimeout)
	defer cancel()
	c, err := budget.NewDurable(budget.Config{RootKey: row.RunID, Version: graphVersion, MaxModelRequests: h.cfg.MaxModelRequests}, h.budgets)
	if err != nil {
		return err
	}
	snap, err := c.Snapshot(readCtx)
	if err != nil {
		return err
	}
	return h.alertUsage(ctx, row, snap.ReservedRequests)
}
