package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/memory"
)

// ticketEvidence 记录一张工单走到终态的过程。
type ticketEvidence struct {
	ID        string `json:"ticket_id"`
	Status    string `json:"final_status"`
	FollowUps int    `json:"follow_ups"`
	Approvals int    `json:"approvals"`
}

// driveTicket 发一条新消息开启工单，并把它推到终态；不调用 require，错误交给调用方记录。
func driveTicket(ctx context.Context, e *hostEnv, customer, text string) (ticketEvidence, error) {
	r, err := e.host.Message(ctx, customer, "agent-1", msgID(), text)
	if err != nil {
		return ticketEvidence{}, err
	}
	ev := ticketEvidence{ID: r.TicketID}
	if r.Action != "started" {
		return ev, fmt.Errorf("新消息应开启新工单，实际 %s（工单 %s）", r.Action, r.TicketID)
	}
	for ev.FollowUps+ev.Approvals < 4 {
		row, err := e.host.ticket(ctx, e.host.ledger.db, r.TicketID)
		if err != nil {
			return ev, fmt.Errorf("读取工单 %s：%w", r.TicketID, err)
		}
		switch row.Status {
		case statusWaitingCustomer:
			if ev.FollowUps >= 2 {
				return ev, fmt.Errorf("工单 %s 追问超过 2 次仍未结束", r.TicketID)
			}
			if _, err := e.host.Message(ctx, customer, "agent-1", msgID(), "没有其他补充，按原支付方式退回即可"); err != nil {
				return ev, fmt.Errorf("工单 %s 补答：%w", r.TicketID, err)
			}
			ev.FollowUps++
		case statusWaitingReview:
			var err error
			for _, approver := range []string{"lead-1", "fin-1"} {
				if _, err = e.host.Answer(ctx, r.TicketID, approver, true, "核实无误"); err == nil {
					break
				}
			}
			if err != nil {
				return ev, fmt.Errorf("工单 %s 审核：%w", r.TicketID, err)
			}
			ev.Approvals++
		default:
			ev.Status = row.Status
		}
		if ev.Status != "" {
			break
		}
	}
	if ev.Status == "" {
		row, err := e.host.ticket(ctx, e.host.ledger.db, r.TicketID)
		if err != nil {
			return ev, fmt.Errorf("读取工单 %s：%w", r.TicketID, err)
		}
		ev.Status = row.Status
	}
	switch ev.Status {
	case statusDone, statusHandoff, statusFailed, statusDelivered:
		return ev, nil
	}
	return ev, fmt.Errorf("工单 %s 未到终态：%s", r.TicketID, ev.Status)
}

// validRecords 取出 ctx 命名空间下的全部有效记录；空文本召回只按新近性排序。
func validRecords(ctx context.Context, m *memory.Memory) ([]memory.Record, error) {
	got, err := m.Recall(ctx, memory.Query{K: 1000})
	if err != nil {
		return nil, err
	}
	out := make([]memory.Record, len(got))
	for i, s := range got {
		out[i] = s.Record
	}
	return out, nil
}

const settleAttempts = 5

// settle 反复 Sweep，直到这些工单各轮的抽取任务都结束（完成、失败或丢弃）；返回最后一次扫描报告。
func settle(ctx context.Context, e *hostEnv, cctx context.Context, tickets []string) (SweepReport, error) {
	var report SweepReport
	var lastErr error
	var unfinished []string
	for range settleAttempts {
		var err error
		if report, err = e.host.Sweep(ctx); err != nil {
			lastErr = err
			continue
		}
		lastErr = nil
		if unfinished, err = unfinishedJobs(ctx, e, cctx, tickets); err != nil {
			lastErr = err
			continue
		}
		if len(unfinished) == 0 {
			return report, nil
		}
	}
	if lastErr != nil {
		return report, fmt.Errorf("settle 在 %d 次尝试后仍出错：%w", settleAttempts, lastErr)
	}
	return report, fmt.Errorf("settle 在 %d 次扫描后仍有未结束的抽取任务：%v", settleAttempts, unfinished)
}

func unfinishedJobs(ctx context.Context, e *hostEnv, cctx context.Context, tickets []string) ([]string, error) {
	var out []string
	for _, tk := range tickets {
		for _, scope := range []string{refundName, generalName} {
			keys, err := sessionJobKeys(ctx, e, memorySession(tk, scope))
			if errors.Is(err, agentErrors.ErrSessionNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			for _, key := range keys {
				j, ok, err := e.host.memory.Memory.Job(cctx, key)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
				switch j.Status {
				case memory.JobDone, memory.JobFailed, memory.JobDropped:
				default:
					out = append(out, key)
				}
			}
		}
	}
	return out, nil
}

func TestMemStateHelpers(t *testing.T) {
	e, _ := newMemoryEnv(t)
	c, cctx := newCustomer()
	r := e.message(msgID(), c, "订单 A1001 申请退款 30 元，只退回原支付方式")
	_, err := settle(context.Background(), e, cctx, []string{r.TicketID})
	require.NoError(t, err)
	recs, err := validRecords(cctx, e.host.memory.Memory)
	require.NoError(t, err)
	require.Contains(t, contents(recs, func(r memory.Record) string { return r.Content }), "客户要求退款只退回原支付方式")
	_, other := newCustomer()
	empty, err := validRecords(other, e.host.memory.Memory)
	require.NoError(t, err)
	require.Empty(t, empty)
}
