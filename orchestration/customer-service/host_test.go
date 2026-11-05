package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
	"github.com/kart-io/goagent/core/durable"
)

// hostEnv 是宿主测试环境：内存组织架构、记录型通知与转人工、可控时钟与可注入故障的运行存储。
type hostEnv struct {
	t         *testing.T
	dir       string
	clock     *support.ManualClock
	directory *support.MemoryDirectory
	notifier  *support.RecordingNotifier
	handoff   *support.RecordingHandoff
	store     *faultStore
	mutate    func(*HostConfig)
	host      *Host
}

func newHostEnv(t *testing.T, mutate func(*HostConfig)) *hostEnv {
	t.Helper()
	d := support.NewMemoryDirectory()
	d.SetLeader("agent-1", support.User{ID: "lead-1", Name: "组长"})
	d.SetGroup("finance-reviewers", support.User{ID: "fin-1", Name: "财务一"}, support.User{ID: "fin-2", Name: "财务二"})
	e := &hostEnv{t: t, dir: t.TempDir(), clock: support.NewManualClock(testStart), directory: d,
		notifier: &support.RecordingNotifier{}, handoff: &support.RecordingHandoff{}, mutate: mutate}
	e.open()
	t.Cleanup(func() { _ = e.host.Close() })
	return e
}

// open 以同一目录打开宿主；重复调用模拟进程重启，运行存储换成新的故障包装。
func (e *hostEnv) open() {
	e.t.Helper()
	inner, err := durable.NewFileStore(filepath.Join(e.dir, "runs"))
	require.NoError(e.t, err)
	e.store = &faultStore{Store: inner}
	cfg, err := support.ParseApproverConfig([]byte(support.DefaultApproverConfig))
	require.NoError(e.t, err)
	hc := HostConfig{Dir: e.dir, Clock: e.clock, Resolver: support.RuleResolver{Directory: e.directory, Config: cfg},
		Notifier: e.notifier, Handoff: e.handoff, Operator: support.User{ID: "ops-1"}, Store: e.store}
	if e.mutate != nil {
		e.mutate(&hc)
	}
	e.host, err = OpenHost(context.Background(), hc)
	require.NoError(e.t, err)
}

func (e *hostEnv) message(id, customer, text string) MessageResult {
	e.t.Helper()
	r, err := e.host.Message(context.Background(), customer, "agent-1", id, text)
	require.NoError(e.t, err)
	return r
}

func (e *hostEnv) row(ticketID string) ticketRow {
	e.t.Helper()
	r, err := e.host.ticket(context.Background(), e.host.ledger.db, ticketID)
	require.NoError(e.t, err)
	return r
}

func (e *hostEnv) sent(kind support.MessageKind) []support.Delivery { return e.notifier.Sent(kind) }

func recipients(ds []support.Delivery) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.To.ID
	}
	return out
}

func (e *hostEnv) refunds(order string) int {
	e.t.Helper()
	n, _, err := e.host.ledger.refunds(context.Background(), order)
	require.NoError(e.t, err)
	return n
}

func TestHostDirectRefund(t *testing.T) {
	e := newHostEnv(t, nil)
	r := e.message("m1", "c-1001", "订单 A1001 申请退款 30 元")
	require.Equal(t, "started", r.Action)
	require.Equal(t, statusDelivered, r.Status)
	replies := e.sent(support.KindCustomerReply)
	require.Len(t, replies, 1, "只发一条客户回复")
	require.Equal(t, "c-1001", replies[0].To.ID)
	require.Contains(t, replies[0].Message.Body, "已退款 3000 分")
	require.Empty(t, e.sent(support.KindApprovalRequest))
	require.Equal(t, 1, e.refunds("A1001"))
}

func TestHostDisputedApproval(t *testing.T) {
	e := newHostEnv(t, nil)
	r := e.message("m1", "c-1001", "订单 A1002 申请退款 800 元")
	require.Equal(t, statusWaitingReview, r.Status)
	require.ElementsMatch(t, []string{"lead-1", "fin-1", "fin-2"}, recipients(e.sent(support.KindApprovalRequest)), "上级与财务组成员各收到一条审核消息")
	row := e.row(r.TicketID)
	require.Equal(t, reviewInterrupt, row.WaitID)
	require.Equal(t, testStart.Add(e.host.cfg.ReviewTTL).Format("2006-01-02T15:04:05Z07:00"), row.Deadline, "截止时间取中断 Info 的 review_deadline")

	_, err := e.host.Answer(context.Background(), r.TicketID, "stranger", true, "")
	require.ErrorIs(t, err, ErrNotApprover, "非审核人作答被拒绝")
	require.Equal(t, statusWaitingReview, e.row(r.TicketID).Status, "运行保持等待")

	a, err := e.host.Answer(context.Background(), r.TicketID, "fin-2", true, "核实无误")
	require.NoError(t, err)
	require.Equal(t, statusDelivered, a.Status)
	require.Equal(t, 1, e.refunds("A1002"), "任一人批准后退款一次")
	require.Len(t, e.sent(support.KindCustomerReply), 1)

	late, err := e.host.Answer(context.Background(), r.TicketID, "lead-1", false, "")
	require.NoError(t, err, "工单已结束时返回已处理结果，不返回错误")
	require.True(t, late.Late)
	require.Equal(t, 1, e.refunds("A1002"))
}

func TestHostSelfApprovalExcluded(t *testing.T) {
	e := newHostEnv(t, nil)
	r, err := e.host.Message(context.Background(), "c-1001", "fin-1", "m1", "订单 A1002 申请退款 800 元")
	require.NoError(t, err)
	require.Equal(t, statusWaitingReview, r.Status)
	require.Equal(t, []string{"fin-2"}, recipients(e.sent(support.KindApprovalRequest)), "提交人在财务组中时不在审核人名单里")
	_, err = e.host.Answer(context.Background(), r.TicketID, "fin-1", true, "")
	require.ErrorIs(t, err, ErrNotApprover, "自审自批被拒绝")
}

func TestHostMessageWhileWaitingReview(t *testing.T) {
	e := newHostEnv(t, nil)
	r := e.message("m1", "c-1001", "订单 A1002 申请退款 800 元")
	calls := e.host.cfg.Client.Calls()
	p := e.message("m2", "c-1001", "进度怎么样了")
	require.Equal(t, "progress", p.Action)
	require.Equal(t, r.TicketID, p.TicketID, "归入同一工单")
	require.Equal(t, statusWaitingReview, p.Status)
	progress := e.sent(support.KindProgress)
	require.Len(t, progress, 1)
	require.Contains(t, progress[0].Message.Body, "审核中")
	require.Equal(t, calls, e.host.cfg.Client.Calls(), "不新开运行")

	dup := e.message("m2", "c-1001", "进度怎么样了")
	require.Equal(t, "duplicate", dup.Action)
	require.Len(t, e.sent(support.KindProgress), 1, "同一消息 ID 只处理一次")
}

func TestHostAskCustomer(t *testing.T) {
	e := newHostEnv(t, nil)
	r := e.message("m1", "c-1001", "我要退款 30 元")
	require.Equal(t, statusWaitingCustomer, r.Status)
	questions := e.sent(support.KindCustomerQuestion)
	require.Len(t, questions, 1)
	require.Equal(t, "请提供需要退款的订单号", questions[0].Message.Body)

	a := e.message("m2", "c-1001", "订单号是 A1001")
	require.Equal(t, "answered", a.Action)
	require.Equal(t, statusDelivered, a.Status)
	require.Equal(t, 1, e.refunds("A1001"))
	require.Len(t, e.sent(support.KindCustomerReply), 1)
}

func TestHostHandoff(t *testing.T) {
	cases := []struct {
		name, input string
		mutate      func(*HostConfig)
		reason      support.HandoffReason
	}{
		{"分流无效", "乱码乱码", nil, support.ReasonInvalidRoute},
		{"轮数超限", "反复咨询物流", nil, support.ReasonMaxRounds},
		{"请求上限", "反复咨询物流", func(c *HostConfig) { c.MaxModelRequests = 3 }, support.ReasonMaxRequests},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newHostEnv(t, c.mutate)
			r := e.message("m1", "c-1001", c.input)
			require.Equal(t, statusDelivered, r.Status)
			transfers := e.handoff.Transfers()
			require.Len(t, transfers, 1, "只转一次人工")
			h := transfers[0].Context
			require.Equal(t, c.reason, h.Reason)
			require.Equal(t, r.TicketID, h.TicketID)
			require.NotEmpty(t, h.Solutions, "交接上下文至少含一个方案")
			require.Contains(t, h.Summary, c.input)
			if c.reason != support.ReasonInvalidRoute {
				require.Equal(t, "反复咨询物流", h.Intent)
			}
			require.Len(t, e.sent(support.KindHandoffNotice), 1, "告知客户已转人工")
			require.Empty(t, e.sent(support.KindCustomerReply), "每种只发一条最终消息")
		})
	}
}

func TestHostUsageAlertOnce(t *testing.T) {
	e := newHostEnv(t, func(c *HostConfig) { c.UsageAlertAt = 2 })
	e.message("m1", "c-1001", "反复咨询物流")
	alerts := e.sent(support.KindUsageAlert)
	require.Len(t, alerts, 1, "超阈值只告警一次")
	require.Equal(t, "ops-1", alerts[0].To.ID)
}

func TestHostOutboxRetriedAfterNotifierFailure(t *testing.T) {
	e := newHostEnv(t, nil)
	e.notifier.Fail(errors.New("IM 不可用"))
	r := e.message("m1", "c-1001", "订单 A1001 申请退款 30 元")
	require.Equal(t, statusDone, r.Status, "最终消息未投递前不记为 delivered")
	e.notifier.Fail(nil)
	e.host.deliver(context.Background())
	e.host.deliver(context.Background())
	replies := e.sent(support.KindCustomerReply)
	require.Len(t, replies, 1, "投递成功后不再重发")
	require.True(t, strings.HasSuffix(replies[0].Message.IdempotencyKey, "/final"))
	require.Equal(t, statusDelivered, e.row(r.TicketID).Status)
}
