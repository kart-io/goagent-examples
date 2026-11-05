package support

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestJudgeRules(t *testing.T) {
	rules := DefaultRules()
	base := DisputeFacts{AmountCents: 3000, PaidCents: 9900, DeliveryDays: 3}
	cases := []struct {
		name   string
		facts  func(DisputeFacts) DisputeFacts
		reason string
	}{
		{"金额超过阈值", func(f DisputeFacts) DisputeFacts { f.AmountCents, f.PaidCents = 60000, 100000; return f }, "超过自动退款上限"},
		{"签收天数过长", func(f DisputeFacts) DisputeFacts { f.DeliveryDays = 8; return f }, "签收 8 天"},
		{"累计退款超比例", func(f DisputeFacts) DisputeFacts { f.RefundedCents = 6000; return f }, "累计退款"},
		{"同单多申请", func(f DisputeFacts) DisputeFacts { f.PriorRequests = 1; return f }, "此前已有 1 笔"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := rules.Judge(c.facts(base), false, "")
			require.True(t, v.Disputed)
			require.Len(t, v.Reasons, 1)
			require.Contains(t, v.Reasons[0], c.reason)
			require.Equal(t, "rules-v1", v.RuleVersion)
		})
	}
	t.Run("全部满足时无争议", func(t *testing.T) {
		v := rules.Judge(base, false, "")
		require.False(t, v.Disputed)
		require.Empty(t, v.Reasons)
	})
}

func TestJudgeAgentOnlyEscalates(t *testing.T) {
	rules := DefaultRules()
	v := rules.Judge(DisputeFacts{AmountCents: 3000, PaidCents: 9900, DeliveryDays: 1}, true, "客户称未收到货")
	require.True(t, v.Disputed, "Agent 可以把无争议升级为有争议")
	require.Equal(t, []string{"Agent 标记有争议：客户称未收到货"}, v.Reasons)

	v = rules.Judge(DisputeFacts{AmountCents: 60000, PaidCents: 100000, DeliveryDays: 1}, false, "")
	require.True(t, v.Disputed, "规则判为有争议时 Agent 未标记也进入审核")
}

func TestRulesValidate(t *testing.T) {
	require.NoError(t, DefaultRules().Validate())
	require.Error(t, DisputeRules{MaxRefundRatio: 1}.Validate(), "缺少版本号")
	require.Error(t, DisputeRules{Version: "v", MaxRefundRatio: 1.5}.Validate(), "比例超过 1")
}

func TestMemoryDirectory(t *testing.T) {
	ctx := context.Background()
	d := NewMemoryDirectory()
	d.SetLeader("agent-1", User{ID: "lead-1", Name: "组长"})
	d.SetGroup("finance", User{ID: "fin-1"}, User{ID: "fin-2"})
	leader, found, err := d.Leader(ctx, "agent-1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "lead-1", leader.ID)
	_, found, err = d.Leader(ctx, "nobody")
	require.NoError(t, err)
	require.False(t, found, "没有上级时 found 为 false")
	members, err := d.GroupMembers(ctx, "finance")
	require.NoError(t, err)
	require.Len(t, members, 2)

	d.Fail(errors.New("目录不可用"))
	_, err = d.GroupMembers(ctx, "finance")
	require.ErrorContains(t, err, "目录不可用")
}

func TestRecordersAndClock(t *testing.T) {
	ctx := context.Background()
	var n RecordingNotifier
	require.NoError(t, n.Notify(ctx, User{ID: "c1"}, Message{Kind: KindCustomerReply, IdempotencyKey: "k1"}))
	require.NoError(t, n.Notify(ctx, User{ID: "c1"}, Message{Kind: KindProgress, IdempotencyKey: "k2"}))
	require.Len(t, n.Sent(KindCustomerReply), 1)
	require.Len(t, n.Sent(""), 2)
	n.Fail(errors.New("发送失败"))
	require.Error(t, n.Notify(ctx, User{ID: "c1"}, Message{}))

	var h RecordingHandoff
	require.NoError(t, h.Transfer(ctx, "t1/final", HandoffContext{TicketID: "t1", Reason: ReasonMaxRounds}))
	require.Equal(t, ReasonMaxRounds, h.Transfers()[0].Context.Reason)

	start := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	c := NewManualClock(start)
	c.Advance(time.Hour)
	require.Equal(t, start.Add(time.Hour), c.Now())
}
