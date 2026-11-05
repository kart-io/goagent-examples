package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
)

// SweepReport 汇总一次扫描做了什么。
type SweepReport struct {
	Advanced int `json:"advanced"`  // 重新推进的 running 工单
	Resolved int `json:"resolved"`  // 重新解析审核人的工单
	Reminded int `json:"reminded"`  // 发出提醒的工单
	TimedOut int `json:"timed_out"` // 以超时输入回应的工单
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
	return report, errors.Join(errs...)
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
