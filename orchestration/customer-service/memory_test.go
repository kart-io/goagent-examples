package main

import (
	"context"
	"github.com/kart-io/goagent/codec/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
	"github.com/kart-io/goagent-examples/orchestration/internal/demomodel"
	"github.com/kart-io/goagent/constants"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/memory"
)

// requestLog 记录离线模型收到的每个请求，测试据此检查召回块是否注入。
type requestLog struct {
	mu   sync.Mutex
	reqs [][]llm.Message
}

func (l *requestLog) model() *livemodel.Client {
	var calls atomic.Int64
	return livemodel.Wrap(&demomodel.Model{Calls: &calls, Respond: func(req *llm.CompletionRequest) llm.StreamChunk {
		l.mu.Lock()
		l.reqs = append(l.reqs, slices.Clone(req.Messages))
		l.mu.Unlock()
		return respond(req)
	}}, livemodel.Config{Provider: "scripted", Model: "customer-service-transcript"})
}

// userMessages 返回系统提示以 system 开头的请求中的全部用户消息。
func (l *requestLog) userMessages(system string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, msgs := range l.reqs {
		if len(msgs) == 0 || !strings.HasPrefix(msgs[0].Content, system) {
			continue
		}
		for _, m := range msgs {
			if m.Role == "user" {
				out = append(out, m.Content)
			}
		}
	}
	return out
}

// newMemoryEnv 打开接入长期记忆的宿主；缺少存储环境变量时跳过。
func newMemoryEnv(t *testing.T) (*hostEnv, *requestLog) {
	t.Helper()
	pg, qd := os.Getenv("CS_MEMORY_PG_URL"), os.Getenv("CS_MEMORY_QDRANT_ADDR")
	if pg == "" || qd == "" {
		t.Skipf("未设置 CS_MEMORY_PG_URL 与 CS_MEMORY_QDRANT_ADDR，请经 scripts/with-stores.sh 运行")
	}
	log := &requestLog{}
	client := log.model()
	e := newHostEnv(t, func(c *HostConfig) {
		c.Client, c.Memory = client, &MemoryConfig{PGURL: pg, QdrantAddr: qd}
	})
	return e, log
}

// newCustomer 返回唯一的客户 ID 及绑定其命名空间的上下文：各测试共用一个 Postgres 与 Qdrant。
func newCustomer() (string, context.Context) {
	id := "c-" + uuid.NewString()
	return id, memory.WithNamespace(context.Background(), memoryNS(id)...)
}

// msgID 返回唯一的消息 ID：工单 ID 由它派生，会话日志按工单 ID 存于共用的 Postgres。
func msgID() string { return "m-" + uuid.NewString() }

func contents[T any](items []T, content func(T) string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = content(it)
	}
	return out
}

func recallContents(t *testing.T, e *hostEnv, ctx context.Context, text string) []string {
	t.Helper()
	got, err := e.host.memory.Memory.Recall(ctx, memory.Query{Text: text, K: 10})
	require.NoError(t, err)
	return contents(got, func(s memory.Scored) string { return s.Content })
}

func pinned(t *testing.T, e *hostEnv, ctx context.Context) []memory.Record {
	t.Helper()
	recs, err := e.host.memory.Memory.Pinned(ctx)
	require.NoError(t, err)
	return recs
}

func TestMemoryAcrossTickets(t *testing.T) {
	e, log := newMemoryEnv(t)
	c, cctx := newCustomer()
	_, dctx := newCustomer()

	e.message(msgID(), c, "订单 A1001 申请退款 30 元，只退回原支付方式")
	r := e.sweep()
	require.GreaterOrEqual(t, r.MemoryJobs, 1, "扫描处理了退款成员登记的抽取任务")
	require.Contains(t, recallContents(t, e, cctx, "只退回原支付方式"), "客户要求退款只退回原支付方式")
	e.restart()

	got := e.message(msgID(), c, "我的包裹到哪了，原支付方式能开发票吗")
	require.Equal(t, statusDelivered, got.Status)
	recalled := slices.ContainsFunc(log.userMessages("customer-service-general"), func(s string) bool {
		return strings.HasPrefix(s, memory.RecallPrefix) && strings.Contains(s, "只退回原支付方式")
	})
	require.True(t, recalled, "新工单的通用成员请求带上了上一张工单记住的偏好")

	t.Run("其他客户读不到", func(t *testing.T) {
		require.Empty(t, recallContents(t, e, dctx, "只退回原支付方式"))
		require.Empty(t, pinned(t, e, dctx))
	})
}

// rememberPinned 让客户经 memory_save 钉住一条记录并返回它。
func rememberPinned(t *testing.T, e *hostEnv, ctx context.Context, customer, text string) memory.Record {
	t.Helper()
	e.message(msgID(), customer, text)
	replies := e.sent(support.KindCustomerReply)
	require.Equal(t, "好的，已记住。", replies[len(replies)-1].Message.Body)
	recs := pinned(t, e, ctx)
	require.Len(t, recs, 1, "memory_save 钉住了客户要求记住的内容")
	return recs[0]
}

// sweepTicket 发一条消息后扫描，要求扫描无错且该工单通用成员本轮的抽取任务完成。
func sweepTicket(t *testing.T, e *hostEnv, ctx context.Context, customer, text string) {
	t.Helper()
	r := e.message(msgID(), customer, text)
	_, err := e.host.Sweep(context.Background())
	require.NoError(t, err)
	keys, err := sessionJobKeys(context.Background(), e, memorySession(r.TicketID, generalName))
	require.NoError(t, err)
	require.Len(t, keys, 1)
	j, ok, err := e.host.memory.Memory.Job(ctx, keys[0])
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, memory.JobDone, j.Status, j.Error)
}

// 钉住记录被整理取代时，取代者继承钉住（goagent 47e1f5f5 的 update 与 invalidate 两个分支）。
func TestPinnedInheritedBySuccessor(t *testing.T) {
	tests := []struct {
		name, remember, next, successor string
	}{
		{name: "合并更新", remember: "请记住：只用邮件联系我", next: "邮件或短信都可以", successor: "客户接受邮件或短信联系"},
		{name: "否定", remember: "请记住：我住在上海", next: "上海的记录不对", successor: "客户不住在上海"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := newMemoryEnv(t)
			c, cctx := newCustomer()
			before := rememberPinned(t, e, cctx, c, tt.remember)

			sweepTicket(t, e, cctx, c, tt.next)
			after := pinned(t, e, cctx)
			require.Len(t, after, 1, "取代后仍只有一条钉住记录")
			require.Equal(t, tt.successor, after[0].Content)

			old, err := e.host.memory.Memory.Get(cctx, before.ID)
			require.NoError(t, err)
			require.False(t, old.Valid())
			require.Equal(t, after[0].ID, old.SupersededBy, "旧记录指向取代它的新记录")
		})
	}
}

// 同一轮中候选 A 合并更新已有钉住记录 x，候选 B 又否定了 A 的计划记录：A 不再创建，x 改由 B 取代，钉住随之转移给 B。
// 被否定的计划记录本身须取代已有记录才会走到 goagent 49dd0983 修复的路径，修复前提交时空指针 panic。
func TestSameTurnNegateOfSupersedingPlan(t *testing.T) {
	e, _ := newMemoryEnv(t)
	c, cctx := newCustomer()
	x := rememberPinned(t, e, cctx, c, "请记住：我住在上海")

	sweepTicket(t, e, cctx, c, "先说搬到浦东又改口")
	after := pinned(t, e, cctx)
	require.Len(t, after, 1)
	negator := after[0]
	require.Equal(t, "客户不住在上海浦东", negator.Content, "否定者继承了 x 的钉住")

	old, err := e.host.memory.Memory.Get(cctx, x.ID)
	require.NoError(t, err)
	require.False(t, old.Valid())
	require.Equal(t, negator.ID, old.SupersededBy, "x 改由否定者取代")
	require.NotContains(t, recallContents(t, e, cctx, "上海浦东"), "客户住在上海浦东", "被否定的计划记录没有创建")
}

func TestFollowUpKeepsNamespace(t *testing.T) {
	e, _ := newMemoryEnv(t)
	c, _ := newCustomer()

	r := e.message(msgID(), c, "我要退款 30 元")
	require.Equal(t, statusWaitingCustomer, r.Status)
	e.restart()

	a, err := e.host.Message(context.Background(), c, "agent-1", msgID(), "订单号是 A1001")
	require.NoError(t, err, "续跑时宿主按工单行重新绑定同一命名空间")
	require.Equal(t, "answered", a.Action)
	require.Equal(t, statusDelivered, a.Status)
}

// sessionJobKeys 按 builder 的规则（会话 ID/turn/start 序号）拼出会话中各轮的抽取任务 Key；goagent 没有导出构造方式。
func sessionJobKeys(ctx context.Context, e *hostEnv, sessionID string) ([]string, error) {
	events, err := e.host.memory.Sessions.Read(ctx, sessionID, 0)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, ev := range events {
		if ev.Type == constants.SessionEventTurnStart {
			keys = append(keys, sessionID+"/"+strconv.FormatInt(ev.Seq, 10))
		}
	}
	return keys, nil
}

func TestSweepMemoryFailure(t *testing.T) {
	e, _ := newMemoryEnv(t)
	c, cctx := newCustomer()

	e.notifier.Fail(errors.New("通知服务不可用"))
	r := e.message(msgID(), c, "抽取坏回复")
	require.Empty(t, e.sent(support.KindCustomerReply), "回复留在发件箱")
	e.notifier.Fail(nil)

	keys, err := sessionJobKeys(context.Background(), e, memorySession(r.TicketID, generalName))
	require.NoError(t, err)
	require.Len(t, keys, 1)
	job := func() memory.Job {
		j, ok, err := e.host.memory.Memory.Job(cctx, keys[0])
		require.NoError(t, err)
		require.True(t, ok, "通用成员的轮次登记了抽取任务")
		return j
	}

	report := e.sweep()
	require.GreaterOrEqual(t, report.MemoryJobs, 1)
	require.Len(t, e.sent(support.KindCustomerReply), 1, "第一次扫描在抽取之前投递了发件箱")
	require.Equal(t, statusDelivered, e.row(r.TicketID).Status)

	for i := 1; i < constants.DefaultMemoryJobAttempts && job().Status != memory.JobFailed; i++ {
		e.sweep()
	}
	j := job()
	require.Equal(t, memory.JobFailed, j.Status, "抽取按 JobAttempts 重试耗尽后标记失败")
	require.Equal(t, constants.DefaultMemoryJobAttempts, j.Attempts)
	require.Len(t, e.sent(support.KindCustomerReply), 1, "抽取失败不影响投递，也不重复投递")
}

func TestRefundRecallsMemory(t *testing.T) {
	e, log := newMemoryEnv(t)
	c, _ := newCustomer()
	for _, order := range []string{"A7001", "A7002"} {
		_, err := e.host.ledger.db.Exec("INSERT INTO orders VALUES (?,?,9900,'CNY',3,1)", order, c)
		require.NoError(t, err)
	}

	e.message(msgID(), c, "订单 A7001 申请退款 30 元，只退回原支付方式")
	require.Equal(t, 1, e.refunds("A7001"))
	e.sweep()

	r := e.message(msgID(), c, "订单 A7002 申请退款 30 元")
	require.Equal(t, statusDelivered, r.Status)
	require.Equal(t, 1, e.refunds("A7002"), "召回块在末尾时退款成员仍读到本轮的 get_order 结果并提议退款")
	replies := e.sent(support.KindCustomerReply)
	require.Len(t, replies, 2)
	require.Contains(t, replies[1].Message.Body, "已退款 3000 分")
	recalled := slices.ContainsFunc(log.userMessages("customer-service-refund"), func(s string) bool {
		return strings.HasPrefix(s, memory.RecallPrefix) && strings.Contains(s, "只退回原支付方式")
	})
	require.True(t, recalled, "第二张工单的退款成员请求带上了召回块")
}

func TestNoMemoryUnchanged(t *testing.T) {
	t.Setenv("CS_MEMORY_PG_URL", "postgres://invalid:1/none")
	t.Setenv("CS_MEMORY_QDRANT_ADDR", "invalid:1")
	e := newHostEnv(t, nil)
	require.Nil(t, e.host.memory, "未配置记忆时不连接 Postgres 与 Qdrant")

	r := e.message("m1", "c-1001", "订单 A1001 申请退款 30 元")
	require.Equal(t, statusDelivered, r.Status)
	report := e.sweep()
	require.Equal(t, SweepReport{}, report)
	data, err := json.Marshal(report)
	require.NoError(t, err)
	require.JSONEq(t, `{"advanced":0,"resolved":0,"reminded":0,"timed_out":0}`, string(data), "报告不出现记忆字段")
}
