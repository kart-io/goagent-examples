package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
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
		ORDER BY 7, 2, 3`, customerID, channelNotify, customerID)
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
