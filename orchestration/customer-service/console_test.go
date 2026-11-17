package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/multiagent"

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
		for _, staff := range []string{"lead-1", "fin-1"} {
			require.Empty(t, e.conversation(staff), "审核人 ID 充当客户 ID 也看不到他人通知")
		}
	})
	t.Run("无数据", func(t *testing.T) {
		e := newHostEnv(t, nil)
		got := e.conversation("c-none")
		require.NotNil(t, got)
		require.Empty(t, got)
	})
}

func TestMessageIDTakenByOtherCustomer(t *testing.T) {
	e := newHostEnv(t, nil)
	r1 := e.message("m1", "c-1001", "订单 A1002 申请退款 800 元")
	before := e.outboxCount()
	_, err := e.host.Message(context.Background(), "c-2002", "web", "m1", "订单 A2001 申请退款 99 元")
	require.ErrorIs(t, err, errMessageIDTaken)
	require.Equal(t, before, e.outboxCount(), "不触碰他人工单")
	require.Empty(t, e.conversation("c-2002"))
	own, err := e.host.Tickets(context.Background(), "c-2002")
	require.NoError(t, err)
	require.Empty(t, own)
	require.Equal(t, r1.TicketID, e.conversation("c-1001")[0].TicketID)
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

func (e *hostEnv) outboxCount() int {
	e.t.Helper()
	var n int
	require.NoError(e.t, e.host.ledger.db.QueryRow("SELECT count(*) FROM outbox").Scan(&n))
	return n
}

func (e *hostEnv) needsHuman(customer, ticketID string) bool {
	e.t.Helper()
	got, err := e.host.Tickets(context.Background(), customer)
	require.NoError(e.t, err)
	for _, s := range got {
		if s.TicketID == ticketID {
			return s.NeedsHuman
		}
	}
	e.t.Fatalf("列表中没有工单 %s", ticketID)
	return false
}

func manualReplies(ds []support.Delivery) []support.Delivery {
	var out []support.Delivery
	for _, d := range ds {
		if d.Message.Title == "人工客服" {
			out = append(out, d)
		}
	}
	return out
}

func TestReply(t *testing.T) {
	ctx := context.Background()
	t.Run("转人工后可回复", func(t *testing.T) {
		e := newHostEnv(t, nil)
		tk := e.message("m1", "c-1001", "乱码乱码")
		require.Equal(t, statusDelivered, tk.Status)
		require.True(t, e.needsHuman("c-1001", tk.TicketID))
		res, err := e.host.Reply(ctx, tk.TicketID, "r1", "您好，我是人工客服")
		require.NoError(t, err)
		require.Equal(t, tk.TicketID+"/manual/r1", res.Key)
		sent := manualReplies(e.sent(support.KindCustomerReply))
		require.Len(t, sent, 1)
		require.Equal(t, "您好，我是人工客服", sent[0].Message.Body)
		conv := e.conversation("c-1001")
		require.True(t, conv[len(conv)-1].Manual)
		require.False(t, e.needsHuman("c-1001", tk.TicketID))
	})
	t.Run("重复提交只投递一次", func(t *testing.T) {
		e := newHostEnv(t, nil)
		tk := e.message("m1", "c-1001", "乱码乱码")
		for range 2 {
			_, err := e.host.Reply(ctx, tk.TicketID, "r1", "您好，我是人工客服")
			require.NoError(t, err)
		}
		require.Len(t, manualReplies(e.sent(support.KindCustomerReply)), 1)
	})
	t.Run("未转人工拒绝", func(t *testing.T) {
		e := newHostEnv(t, nil)
		tk := e.message("m1", "c-1001", "订单 A1001 申请退款 30 元")
		before := e.outboxCount()
		_, err := e.host.Reply(ctx, tk.TicketID, "r1", "您好")
		require.ErrorIs(t, err, errNotHandedOff)
		require.Equal(t, before, e.outboxCount())
		require.False(t, e.needsHuman("c-1001", tk.TicketID))
	})
	t.Run("工单不存在", func(t *testing.T) {
		e := newHostEnv(t, nil)
		e.message("m1", "c-1001", "乱码乱码")
		before := e.outboxCount()
		_, err := e.host.Reply(ctx, "tk-none", "r1", "您好")
		var nf ticketNotFoundError
		require.True(t, errors.As(err, &nf))
		require.EqualError(t, err, "工单 tk-none 不存在")
		require.Equal(t, before, e.outboxCount())
	})
	t.Run("空回复 ID 或内容", func(t *testing.T) {
		e := newHostEnv(t, nil)
		tk := e.message("m1", "c-1001", "乱码乱码")
		before := e.outboxCount()
		for _, c := range [][2]string{{"", "您好"}, {"r1", ""}} {
			_, err := e.host.Reply(ctx, tk.TicketID, c[0], c[1])
			require.Error(t, err)
		}
		require.Equal(t, before, e.outboxCount())
	})
	t.Run("运行失败也算转人工", func(t *testing.T) {
		e := newHostEnv(t, nil)
		r := e.message("m1", "c-1001", "订单 A1002 申请退款 800 元")
		_, err := e.host.ledger.db.Exec("DELETE FROM orders WHERE order_id = 'A1002'")
		require.NoError(t, err)
		_, err = e.host.Answer(ctx, r.TicketID, "fin-1", true, "")
		require.Error(t, err)
		for range 2 {
			e.clock.Advance(3 * time.Minute)
			_, _ = e.host.Sweep(ctx)
		}
		var status string
		require.NoError(t, e.host.ledger.db.QueryRow("SELECT status FROM tickets WHERE ticket_id = ?", r.TicketID).Scan(&status))
		require.Equal(t, statusDelivered, status)
		require.True(t, e.needsHuman("c-1001", r.TicketID))
		_, err = e.host.Reply(ctx, r.TicketID, "r1", "人工跟进")
		require.NoError(t, err)
		require.False(t, e.needsHuman("c-1001", r.TicketID))
	})
}

func TestTicketsAndTurns(t *testing.T) {
	ctx := context.Background()
	e := newHostEnv(t, nil)
	a := e.message("m1", "c-1001", "订单 A1001 申请退款 30 元")
	b := e.message("m2", "c-2002", "订单 A2001 申请退款 99 元")
	all, err := e.host.Tickets(ctx, "")
	require.NoError(t, err)
	require.Len(t, all, 2)
	only, err := e.host.Tickets(ctx, "c-2002")
	require.NoError(t, err)
	require.Len(t, only, 1)
	require.Equal(t, b.TicketID, only[0].TicketID)
	require.Equal(t, "c-2002", only[0].CustomerID)

	turns, err := e.host.AgentTurns(ctx, a.TicketID)
	require.NoError(t, err)
	require.NotEmpty(t, turns)
	require.True(t, slices.ContainsFunc(turns, func(tn multiagent.Turn) bool { return tn.Agent == triageName }))

	_, err = e.host.AgentTurns(ctx, "tk-none")
	var nf ticketNotFoundError
	require.True(t, errors.As(err, &nf))
}

func TestTicketsOrderAndEmpty(t *testing.T) {
	ctx := context.Background()
	e := newHostEnv(t, nil)
	got, err := e.host.Tickets(ctx, "")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)

	first := e.message("m1", "c-1001", "订单 A1001 申请退款 30 元")
	e.clock.Advance(time.Minute)
	second := e.message("m2", "c-2002", "订单 A2001 申请退款 99 元")
	got, err = e.host.Tickets(ctx, "")
	require.NoError(t, err)
	require.Equal(t, []string{second.TicketID, first.TicketID}, []string{got[0].TicketID, got[1].TicketID})
}
