package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
	"github.com/kart-io/goagent/core/durable"
)

var errInjected = errors.New("注入的保存失败")

// faultStore 让匹配的一次 Save 失败，模拟“效果已发生、记录未落盘”时崩溃。
type faultStore struct {
	durable.Store
	mu    sync.Mutex
	match func(*durable.Snapshot) bool
	errv  error // 非空时以它代替 errInjected 返回，如模拟上限时间到期
}

func (s *faultStore) arm(match func(*durable.Snapshot) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.match = match
}

func (s *faultStore) Save(ctx context.Context, id string, snap *durable.Snapshot) error {
	s.mu.Lock()
	hit := s.match != nil && s.match(snap)
	if hit {
		s.match = nil
	}
	s.mu.Unlock()
	if hit && s.errv != nil {
		return s.errv
	}
	if hit {
		return errInjected
	}
	return s.Store.Save(ctx, id, snap)
}

// operationSaved 匹配操作 id 以 status 状态保存的记录。
func operationSaved(id, status string) func(*durable.Snapshot) bool {
	return func(s *durable.Snapshot) bool {
		var r durable.OperationRecord
		return s != nil && s.Version == "goagent-operation-v2" && json.Unmarshal(s.Data, &r) == nil && r.ID == id && r.Status == status
	}
}

var testStart = time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)

type deskEnv struct {
	t      *testing.T
	ledger *ledger
	desk   *refundDesk
	clock  *support.ManualClock
	store  *faultStore
}

func newDeskEnv(t *testing.T) *deskEnv {
	t.Helper()
	l, err := openLedger(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	inner, err := durable.NewFileStore(t.TempDir())
	require.NoError(t, err)
	clock := support.NewManualClock(testStart)
	return &deskEnv{t: t, ledger: l, clock: clock, store: &faultStore{Store: inner}, desk: newRefundDesk(l, clock, support.DefaultRules(), 48*time.Hour)}
}

// ctx 返回以 key 为执行键、带持久日志的上下文，相当于某次节点访问。
func (e *deskEnv) ctx(key string) context.Context {
	return durable.WithJournal(durable.WithExecutionKey(context.Background(), key), e.store)
}

func (e *deskEnv) register(key, ticket, customer string, p proposal) registration {
	e.t.Helper()
	reg, err := e.desk.registerOnce(e.ctx(key), ticket, customer, p)
	require.NoError(e.t, err)
	return reg
}

// allow 把 users 写为审核中断 key 的审核人名单。
func (e *deskEnv) allow(key, requestID string, users ...support.User) {
	e.t.Helper()
	require.NoError(e.t, saveApprovers(context.Background(), e.ledger.db, key, requestID, users))
}

func refundOf(order string, cents int64) proposal {
	return proposal{Action: "refund", OrderID: order, AmountCents: cents, Reason: "客户申请"}
}

func TestRegisterFixesVerdictAndSequence(t *testing.T) {
	e := newDeskEnv(t)
	small := e.register("visit-1", "t-1", "c-1001", refundOf("A1001", 3000))
	require.Empty(t, small.Rejected)
	require.False(t, small.Verdict.Disputed)
	require.EqualValues(t, 1, small.Sequence)
	require.Equal(t, "rules-v1", small.Verdict.RuleVersion)
	require.Equal(t, testStart.Add(48*time.Hour).Format(time.RFC3339), small.ReviewDeadline, "截止时间由宿主时钟在登记时生成")
	require.Equal(t, testStart.Add(49*time.Hour).Format(time.RFC3339Nano), small.Request.ValidUntil, "有效期为审核截止后 1 小时")

	large := e.register("visit-2", "t-1", "c-1001", refundOf("A1002", 80000))
	require.True(t, large.Verdict.Disputed)
	require.Contains(t, strings.Join(large.Verdict.Reasons, ";"), "超过自动退款上限")
	require.EqualValues(t, 2, large.Sequence)

	// 等待期间规则与订单事实变化：同一节点访问重入时回放，同一工单的重复咨询读到已固定的判定。
	e.desk.setRules(support.DisputeRules{Version: "rules-v2", MaxAutoAmountCents: 1000000, MaxDeliveryDays: 30, MaxRefundRatio: 1})
	_, err := e.ledger.db.Exec("UPDATE orders SET delivery_days = 9, version = 2 WHERE order_id = 'A1002'")
	require.NoError(t, err)
	replayed := e.register("visit-2", "t-1", "c-1001", refundOf("A1002", 80000))
	require.Equal(t, large, replayed)
	repeated := e.register("visit-3", "t-1", "c-1001", refundOf("A1002", 80000))
	require.Equal(t, large.Request.RequestID, repeated.Request.RequestID, "同一工单内的重复申请得到同一申请 ID")
	require.Equal(t, large.Verdict, repeated.Verdict, "判定已固定，不受新规则影响")
	require.EqualValues(t, 3, e.desk.registers.Load(), "回放不执行登记，重复咨询执行一次但不新增登记")
	var n int
	require.NoError(t, e.ledger.db.QueryRow("SELECT count(*) FROM refund_requests").Scan(&n))
	require.Equal(t, 2, n)
}

func TestRegisterRejectsForeignOrder(t *testing.T) {
	e := newDeskEnv(t)
	for _, order := range []string{"A2001", "A9999"} {
		reg := e.register("visit-"+order, "t-1", "c-1001", refundOf(order, 3000))
		require.Equal(t, "未找到该订单，请核对订单号", reg.Rejected, "他人订单与不存在的订单回复相同，不泄露订单是否存在")
	}
	var n int
	require.NoError(t, e.ledger.db.QueryRow("SELECT count(*) FROM refund_requests").Scan(&n))
	require.Zero(t, n, "不属于该客户的订单不登记")
}

func TestConcurrentSplitOnlyFirstMayAutoApprove(t *testing.T) {
	e := newDeskEnv(t)
	results := make([]registration, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, cents := range []int64{3000, 2000} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = e.desk.registerOnce(e.ctx("split-"+string(rune('a'+i))), "t-1", "c-1001", refundOf("A1001", cents))
		}()
	}
	wg.Wait()
	require.NoError(t, errors.Join(errs...))
	first, second := results[0], results[1]
	if first.Sequence > second.Sequence {
		first, second = second, first
	}
	require.EqualValues(t, 1, first.Sequence)
	require.False(t, first.Verdict.Disputed, "序号最小的一笔可以自动通过")
	require.EqualValues(t, 2, second.Sequence)
	require.True(t, second.Verdict.Disputed)
	require.Contains(t, second.Verdict.Reasons[0], "此前已有 1 笔")
}

func TestNewTicketIsNewRequestWithinPaidAmount(t *testing.T) {
	e := newDeskEnv(t)
	first := e.register("v-1", "t-1", "c-1001", refundOf("A1001", 6000))
	result, err := e.desk.executeOnce(e.ctx("v-1"), first.Request, systemActor)
	require.NoError(t, err)
	require.NotEmpty(t, result.Receipt)

	second := e.register("v-2", "t-2", "c-1001", refundOf("A1001", 6000))
	require.NotEqual(t, first.Request.RequestID, second.Request.RequestID, "新工单算新申请")
	require.True(t, second.Verdict.Disputed)
	reasons := strings.Join(second.Verdict.Reasons, ";")
	require.Contains(t, reasons, "此前已有 1 笔")
	require.Contains(t, reasons, "累计退款")

	e.allow("review-2", second.Request.RequestID, support.User{ID: "lead-1"})
	_, err = e.desk.executeOnce(withReviewKey(e.ctx("v-2"), "review-2"), second.Request, "lead-1")
	require.ErrorContains(t, err, "cumulative refund exceeds paid amount", "累计额度由 refund.Service 把关")
	count, total, err := e.ledger.refunds(context.Background(), "A1001")
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.EqualValues(t, 6000, total)
}

func TestAuthorizer(t *testing.T) {
	e := newDeskEnv(t)
	disputed := e.register("v-1", "t-1", "c-1001", refundOf("A1002", 80000))
	_, err := e.desk.executeOnce(e.ctx("v-sys"), disputed.Request, systemActor)
	require.ErrorContains(t, err, "系统主体只能批准无争议的申请")

	e.allow("review-1", disputed.Request.RequestID, support.User{ID: "lead-1"}, support.User{ID: "fin-1"})
	_, err = e.desk.executeOnce(withReviewKey(e.ctx("v-stranger"), "review-1"), disputed.Request, "stranger")
	require.ErrorContains(t, err, "不在申请")
	_, err = e.desk.executeOnce(withReviewKey(e.ctx("v-other-review"), "review-x"), disputed.Request, "lead-1")
	require.ErrorContains(t, err, "不在申请", "名单按中断键读取，其他中断的名单不适用")

	ok, err := e.desk.executeOnce(withReviewKey(e.ctx("v-lead"), "review-1"), disputed.Request, "lead-1")
	require.NoError(t, err)
	require.NotEmpty(t, ok.Receipt)
}

func TestApproveIdempotentPerActor(t *testing.T) {
	e := newDeskEnv(t)
	reg := e.register("v-1", "t-1", "c-1001", refundOf("A1002", 80000))
	e.allow("review-1", reg.Request.RequestID, support.User{ID: "lead-1"}, support.User{ID: "fin-1"})
	ctx := withReviewKey(context.Background(), "review-1")
	svc := e.desk.service()
	require.NoError(t, svc.Approve(ctx, reg.Request, "lead-1"))
	require.NoError(t, svc.Approve(ctx, reg.Request, "lead-1"), "同一主体重复批准只覆盖相同值")
	require.ErrorContains(t, svc.Approve(ctx, reg.Request, "fin-1"), "已由其他主体批准")
}

func TestRegisterCanceledBeforeCommitRerunsWithRealVerdict(t *testing.T) {
	e := newDeskEnv(t)
	ctx, cancel := context.WithCancel(e.ctx("v-1"))
	e.ledger.beforeCommit = func() error { cancel(); e.ledger.beforeCommit = nil; return context.Canceled }
	_, err := e.desk.registerOnce(ctx, "t-1", "c-1001", refundOf("A1002", 80000))
	require.ErrorIs(t, err, context.Canceled)
	_, found, err := e.ledger.request(context.Background(), requestID("t-1", "A1002", 80000))
	require.NoError(t, err)
	require.False(t, found, "事务提交前退出，申请与判定都没有写入")

	reg := e.register("v-1", "t-1", "c-1001", refundOf("A1002", 80000))
	require.True(t, reg.Verdict.Disputed, "重跑重新计算判定，不以零值补记")
	require.EqualValues(t, 2, e.desk.registers.Load())
}

func TestRegisterPendingReconcile(t *testing.T) {
	t.Run("申请与判定都存在时补记", func(t *testing.T) {
		e := newDeskEnv(t)
		e.store.arm(operationSaved("refund/register", "completed"))
		_, err := e.desk.registerOnce(e.ctx("v-1"), "t-1", "c-1001", refundOf("A1002", 80000))
		require.ErrorIs(t, err, errInjected)
		reg := e.register("v-1", "t-1", "c-1001", refundOf("A1002", 80000))
		require.True(t, reg.Verdict.Disputed)
		require.EqualValues(t, 1, e.desk.registers.Load(), "Reconcile 补记，登记没有重跑")
		r, err := durable.InspectOperation(e.ctx("v-1"), "refund/register")
		require.NoError(t, err)
		require.True(t, r.Reconciled)
	})
	t.Run("只有申请没有判定时报错", func(t *testing.T) {
		e := newDeskEnv(t)
		e.store.arm(operationSaved("refund/register", "completed"))
		_, err := e.desk.registerOnce(e.ctx("v-1"), "t-1", "c-1001", refundOf("A1002", 80000))
		require.ErrorIs(t, err, errInjected)
		// SQLite 事务是原子的，无法自然出现“有申请无判定”，这里直接删除判定来构造。
		_, err = e.ledger.db.Exec("DELETE FROM refund_verdicts")
		require.NoError(t, err)
		_, err = e.desk.registerOnce(e.ctx("v-1"), "t-1", "c-1001", refundOf("A1002", 80000))
		require.ErrorContains(t, err, "缺少争议判定", "重跑读不到判定时报错，绝不当作无争议")
		require.EqualValues(t, 2, e.desk.registers.Load())
	})
}

func TestExecuteReceiptFirstAfterCanceledRecord(t *testing.T) {
	e := newDeskEnv(t)
	reg := e.register("v-1", "t-1", "c-1001", refundOf("A1001", 3000))
	ctx, cancel := context.WithCancel(e.ctx("v-1"))
	e.ledger.afterRefund = func() { cancel(); e.ledger.afterRefund = nil }
	_, err := e.desk.executeOnce(ctx, reg.Request, systemActor)
	require.ErrorIs(t, err, context.Canceled)
	r, err := durable.InspectOperation(e.ctx("v-1"), "refund/execute")
	require.NoError(t, err)
	require.Equal(t, "failed", r.Status)
	require.Equal(t, "canceled", r.FailureKind)

	// 申请已过期、订单已变化：若先 Approve 会报过期或需重新审核而卡死，先查回执则直接返回。
	e.clock.Advance(72 * time.Hour)
	_, err = e.ledger.db.Exec("UPDATE orders SET version = 2 WHERE order_id = 'A1001'")
	require.NoError(t, err)
	result, err := e.desk.executeOnce(e.ctx("v-1"), reg.Request, systemActor)
	require.NoError(t, err)
	require.NotEmpty(t, result.Receipt)
	require.False(t, result.OrderChanged)
	count, _, err := e.ledger.refunds(context.Background(), "A1001")
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.EqualValues(t, 2, e.desk.executions.Load())
}

func TestExecutePendingReconcileAndOrderChange(t *testing.T) {
	e := newDeskEnv(t)
	reg := e.register("v-1", "t-1", "c-1001", refundOf("A1001", 3000))
	e.store.arm(operationSaved("refund/execute", "completed"))
	_, err := e.desk.executeOnce(e.ctx("v-1"), reg.Request, systemActor)
	require.ErrorIs(t, err, errInjected)
	result, err := e.desk.executeOnce(e.ctx("v-1"), reg.Request, systemActor)
	require.NoError(t, err)
	require.NotEmpty(t, result.Receipt)
	require.EqualValues(t, 1, e.desk.executions.Load(), "Reconcile 按回执补记，不重复退款")

	changed := e.register("v-2", "t-1", "c-1001", refundOf("A1003", 2000))
	require.True(t, changed.Verdict.Disputed, "签收 20 天超过 7 天")
	_, err = e.ledger.db.Exec("UPDATE orders SET version = 2 WHERE order_id = 'A1003'")
	require.NoError(t, err)
	e.allow("review-2", changed.Request.RequestID, support.User{ID: "lead-1"})
	out, err := e.desk.executeOnce(withReviewKey(e.ctx("v-2"), "review-2"), changed.Request, "lead-1")
	require.NoError(t, err, "订单变化是业务结果，不是错误")
	require.True(t, out.OrderChanged)
	count, _, err := e.ledger.refunds(context.Background(), "A1003")
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestRegisterRetriesAfterNonCanceledFailure(t *testing.T) {
	cases := []struct {
		name, kind string
		run        func(e *deskEnv) error
	}{
		{"执行失败", "execution", func(e *deskEnv) error {
			e.ledger.beforeCommit = func() error { e.ledger.beforeCommit = nil; return errors.New("database is locked") }
			_, err := e.desk.registerOnce(e.ctx("v-1"), "t-1", "c-1001", refundOf("A1002", 80000))
			return err
		}},
		{"上限时间到期", "deadline", func(e *deskEnv) error {
			ctx, cancel := context.WithTimeout(e.ctx("v-1"), 50*time.Millisecond)
			defer cancel()
			e.ledger.beforeCommit = func() error { e.ledger.beforeCommit = nil; <-ctx.Done(); return ctx.Err() }
			_, err := e.desk.registerOnce(ctx, "t-1", "c-1001", refundOf("A1002", 80000))
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDeskEnv(t)
			require.Error(t, c.run(e))
			r, err := durable.InspectOperation(e.ctx("v-1"), "refund/register")
			require.NoError(t, err)
			require.Equal(t, "failed", r.Status)
			require.Equal(t, c.kind, r.FailureKind)

			reg := e.register("v-1", "t-1", "c-1001", refundOf("A1002", 80000))
			require.True(t, reg.Verdict.Disputed, "重跑登记得到真实判定")
			require.EqualValues(t, 2, e.desk.registers.Load(), "非取消类失败同样在尝试次数内重跑")
		})
	}
}
