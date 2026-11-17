package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
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

func TestPinnedSurvivesUpdate(t *testing.T) {
	e, _ := newMemoryEnv(t)
	c, cctx := newCustomer()

	e.message(msgID(), c, "请记住：只用邮件联系我")
	replies := e.sent(support.KindCustomerReply)
	require.Len(t, replies, 1)
	require.Equal(t, "好的，已记住。", replies[0].Message.Body)
	before := pinned(t, e, cctx)
	require.Len(t, before, 1, "memory_save 钉住了客户要求记住的内容")

	e.message(msgID(), c, "邮件或短信都可以")
	e.sweep()
	after := pinned(t, e, cctx)
	require.Len(t, after, 1, "合并更新后仍只有一条钉住记录")
	require.Equal(t, "客户接受邮件或短信联系", after[0].Content)

	old, err := e.host.memory.Memory.Get(cctx, before[0].ID)
	require.NoError(t, err)
	require.False(t, old.Valid())
	require.Equal(t, after[0].ID, old.SupersededBy, "旧记录指向取代它的新记录")
}

func TestSameTurnNegateNoPanic(t *testing.T) {
	e, _ := newMemoryEnv(t)
	c, cctx := newCustomer()

	e.message(msgID(), c, "先说住杭州又改口")
	r, err := e.host.Sweep(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, r.MemoryJobs, 1)

	got := recallContents(t, e, cctx, "杭州")
	require.Contains(t, got, "客户不住在杭州")
	require.NotContains(t, got, "客户住在杭州", "同一轮被否定的记录不再召回")
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

func TestSweepMemoryFailure(t *testing.T) {
	e, _ := newMemoryEnv(t)
	c, _ := newCustomer()

	e.notifier.Fail(errors.New("通知服务不可用"))
	r := e.message(msgID(), c, "抽取坏回复")
	require.Empty(t, e.sent(support.KindCustomerReply), "回复留在发件箱")

	// 抽取每次扫描失败一次，最后一次尝试失败之前不报错。
	for range constants.DefaultMemoryJobAttempts - 1 {
		e.sweep()
	}
	e.notifier.Fail(nil)
	_, err := e.host.Sweep(context.Background())
	require.Error(t, err, "抽取重试耗尽时扫描返回错误")
	require.Contains(t, err.Error(), r.TicketID)
	require.Len(t, e.sent(support.KindCustomerReply), 1, "同一次扫描照常投递了发件箱")
	require.Equal(t, statusDelivered, e.row(r.TicketID).Status)
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
