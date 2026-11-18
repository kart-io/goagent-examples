package main

import (
	"context"
	"database/sql"
	"github.com/kart-io/goagent/codec/json"
	"errors"
	"os"
	"strings"

	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
	"github.com/kart-io/goagent/multiagent"
)

// ConversationEntry 是对话中的一条：客户消息或发给客户的通知。
type ConversationEntry struct {
	TicketID  string `json:"ticket_id"`
	Role      string `json:"role"`                 // "customer" 或 "service"
	MessageID string `json:"message_id,omitempty"` // 仅客户消息
	Kind      string `json:"kind,omitempty"`       // 仅通知：support.MessageKind
	Title     string `json:"title,omitempty"`
	Text      string `json:"text"`
	At        string `json:"at"`               // created_at
	Status    string `json:"status,omitempty"` // 仅通知：outbox.status
	Manual    bool   `json:"manual,omitempty"` // 幂等键含 "/manual/"
}

// Conversation 返回该客户全部工单的客户消息与发给客户的通知，按写入时间升序；
// 时间相同时客户消息在前，同类按各自表的 seq 升序。
func (h *Host) Conversation(ctx context.Context, customerID string) ([]ConversationEntry, error) {
	rows, err := h.ledger.db.QueryContext(ctx, `
		SELECT ticket_id, 0 AS rank, seq, message_id, '' AS payload, text, created_at, '' AS status, '' AS idem FROM (
		  SELECT m.ticket_id, m.seq, m.message_id, m.text, m.created_at
		  FROM ticket_messages m JOIN tickets t ON t.ticket_id = m.ticket_id WHERE t.customer_id = ?)
		UNION ALL
		SELECT ticket_id, 1, seq, '', CAST(payload_json AS TEXT), '', created_at, status, idempotency_key FROM outbox
		 WHERE channel = ? AND json_extract(recipient_json, '$.ID') = ?
		   AND ticket_id IN (SELECT ticket_id FROM tickets WHERE customer_id = ?)
		ORDER BY 7, 2, 3`, customerID, channelNotify, customerID, customerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ConversationEntry{}
	for rows.Next() {
		var (
			e                           ConversationEntry
			rank, seq                   int
			messageID, payload, idemKey string
		)
		if err = rows.Scan(&e.TicketID, &rank, &seq, &messageID, &payload, &e.Text, &e.At, &e.Status, &idemKey); err != nil {
			return nil, err
		}
		if rank == 0 {
			e.Role, e.MessageID = "customer", messageID
		} else {
			var m support.Message
			if err = json.Unmarshal([]byte(payload), &m); err != nil {
				return nil, err
			}
			e.Role, e.Kind, e.Title, e.Text = "service", string(m.Kind), m.Title, m.Body
			e.Manual = strings.Contains(idemKey, "/manual/")
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// errNotHandedOff 表示工单从未转过人工，不接受人工回复。
var errNotHandedOff = errors.New("工单未转人工，不能人工回复")

// TicketSummary 是工单列表中的一行。
type TicketSummary struct {
	TicketID     string `json:"ticket_id"`
	CustomerID   string `json:"customer_id"`
	Status       string `json:"status"`
	Round        int    `json:"round"`
	LastProgress string `json:"last_progress"`
	NeedsHuman   bool   `json:"needs_human"`
}

// Tickets 按 last_progress 降序返回工单；customerID 为空时返回全部。
// needs_human：转过人工，且最后一条 handoff 记录之后没有人工回复。
func (h *Host) Tickets(ctx context.Context, customerID string) ([]TicketSummary, error) {
	rows, err := h.ledger.db.QueryContext(ctx, `
		SELECT t.ticket_id, t.customer_id, t.status, t.round, t.last_progress,
		  COALESCE((SELECT MAX(o.seq) FROM outbox o WHERE o.ticket_id = t.ticket_id AND o.channel = ?1) > COALESCE(
		    (SELECT MAX(o.seq) FROM outbox o WHERE o.ticket_id = t.ticket_id AND o.channel = ?2
		       AND instr(o.idempotency_key, t.ticket_id || '/manual/') = 1), 0), 0)
		FROM tickets t WHERE ?3 = '' OR t.customer_id = ?3
		ORDER BY t.last_progress DESC, t.ticket_id DESC`, channelHandoff, channelNotify, customerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []TicketSummary{}
	for rows.Next() {
		var s TicketSummary
		if err = rows.Scan(&s.TicketID, &s.CustomerID, &s.Status, &s.Round, &s.LastProgress, &s.NeedsHuman); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AgentTurns 返回工单当前运行检查点中各 Agent 的处理轮次；运行尚无检查点时为空。
func (h *Host) AgentTurns(ctx context.Context, ticketID string) ([]multiagent.Turn, error) {
	row, err := h.ticket(ctx, h.ledger.db, ticketID)
	if err != nil {
		return nil, err
	}
	readCtx, cancel := context.WithTimeout(ctx, h.cfg.LockTimeout)
	defer cancel()
	snap, err := h.graph.Snapshot(readCtx, row.RunID)
	if errors.Is(err, os.ErrNotExist) {
		return []multiagent.Turn{}, nil
	}
	if err != nil {
		return nil, err
	}
	return append([]multiagent.Turn{}, snap.State.Turns...), nil
}

// ReplyResult 是一次人工回复的结果。
type ReplyResult struct {
	TicketID string `json:"ticket_id"`
	Key      string `json:"key"`
}

// Reply 写入一条人工回复并投递；同一 replyID 重复调用不重复写入。
func (h *Host) Reply(ctx context.Context, ticketID, replyID, text string) (ReplyResult, error) {
	if replyID == "" || text == "" {
		return ReplyResult{}, errors.New("人工回复需要回复 ID 与内容")
	}
	key := ticketID + "/manual/" + replyID
	err := h.write(ctx, func(tx *sql.Tx) error {
		row, err := h.ticket(ctx, tx, ticketID)
		if err != nil {
			return err
		}
		var n int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM outbox WHERE ticket_id = ? AND channel = ?", ticketID, channelHandoff).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return errNotHandedOff
		}
		return notify(ctx, tx, ticketID, support.User{ID: row.CustomerID}, support.KindCustomerReply, key, "人工客服", text)
	})
	if err != nil {
		return ReplyResult{}, err
	}
	h.deliver(ctx)
	return ReplyResult{TicketID: ticketID, Key: key}, nil
}
