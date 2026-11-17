package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent/core/durable"

	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
)

func (e *hostEnv) conversation(customer string) []ConversationEntry {
	e.t.Helper()
	got, err := e.host.Conversation(context.Background(), customer)
	require.NoError(e.t, err)
	return got
}

func TestConversation(t *testing.T) {
	notices := map[string]bool{
		string(support.KindCustomerReply): true, string(support.KindProgress): true, string(support.KindHandoffNotice): true,
	}
	t.Run("退款与回复交错", func(t *testing.T) {
		e := newHostEnv(t, nil)
		e.message("m1", "c-1001", "订单 A1001 申请退款 30 元")
		got := e.conversation("c-1001")
		require.GreaterOrEqual(t, len(got), 2)
		require.Equal(t, "customer", got[0].Role)
		require.Equal(t, "m1", got[0].MessageID)
		require.Equal(t, "订单 A1001 申请退款 30 元", got[0].Text)
		var service int
		for i, en := range got {
			require.NotEmpty(t, en.At)
			if i > 0 {
				require.GreaterOrEqual(t, en.At, got[i-1].At, "时间不降序")
			}
			if en.Role == "service" {
				service++
				require.NotEmpty(t, en.Kind)
				require.NotEmpty(t, en.Status)
				require.False(t, en.Manual)
			}
		}
		require.Positive(t, service)
	})
	t.Run("多轮第一轮不丢", func(t *testing.T) {
		e := newHostEnv(t, nil)
		e.store.arm(func(s *durable.Snapshot) bool { return s.Plan != "" })
		_, err := e.host.Message(context.Background(), "c-1001", "agent-1", "m1", "订单 A1001 申请退款 30 元")
		require.ErrorIs(t, err, errInjected)
		require.Equal(t, "queued", e.message("m2", "c-1001", "我的包裹物流到哪了").Action)
		var ids []string
		for _, en := range e.conversation("c-1001") {
			if en.Role == "customer" {
				ids = append(ids, en.MessageID)
			}
		}
		require.Equal(t, []string{"m1", "m2"}, ids)
	})
	t.Run("转人工只见告知", func(t *testing.T) {
		e := newHostEnv(t, nil)
		e.message("m1", "c-1001", "乱码乱码")
		transfers := e.handoff.Transfers()
		require.Len(t, transfers, 1)
		var service int
		for _, en := range e.conversation("c-1001") {
			if en.Role != "service" {
				continue
			}
			service++
			require.True(t, notices[en.Kind], en.Kind)
			require.NotEqual(t, transfers[0].Context.Summary, en.Text, "不泄露转人工摘要")
		}
		require.Positive(t, service)
	})
	t.Run("隔离", func(t *testing.T) {
		e := newHostEnv(t, nil)
		r1 := e.message("m1", "c-1001", "订单 A1002 申请退款 800 元") // 争议：审核人含 fin-1
		r2 := e.message("m2", "c-2002", "订单 A2001 申请退款 99 元")  // 审核人为 lead-1
		require.Equal(t, statusWaitingReview, r1.Status)
		require.Equal(t, statusWaitingReview, r2.Status)
		reviewer := func(ticketID, id string) int {
			var n int
			require.NoError(t, e.host.ledger.db.QueryRow(
				"SELECT count(*) FROM outbox WHERE ticket_id = ? AND json_extract(recipient_json, '$.ID') = ?", ticketID, id).Scan(&n))
			return n
		}
		require.Positive(t, reviewer(r1.TicketID, "fin-1"), "发件箱确有发给 fin-1 的记录")
		require.Positive(t, reviewer(r2.TicketID, "lead-1"), "发件箱确有发给审核人的记录")
		for customer, ticket := range map[string]string{"c-1001": r1.TicketID, "c-2002": r2.TicketID} {
			got := e.conversation(customer)
			require.NotEmpty(t, got)
			for _, en := range got {
				require.Equal(t, ticket, en.TicketID)
				require.NotEqual(t, string(support.KindApprovalRequest), en.Kind, "不含审核人的通知")
			}
		}
	})
	t.Run("无数据", func(t *testing.T) {
		e := newHostEnv(t, nil)
		got := e.conversation("c-none")
		require.NotNil(t, got)
		require.Empty(t, got)
	})
}

func TestLedgerV1Rejected(t *testing.T) {
	e := newHostEnv(t, nil)
	e.message("m1", "c-1001", "订单 A1001 申请退款 30 元")
	require.NoError(t, e.host.Close())
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(e.dir, "customer-service.sqlite"))
	require.NoError(t, err)
	_, err = db.Exec("PRAGMA user_version = 1")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.ErrorContains(t, e.tryOpen(), "账本结构不兼容")
}
