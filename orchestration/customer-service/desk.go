package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/workflow"
	"github.com/kart-io/goagent/workflow/refund"
)

// systemActor 是无争议申请的批准主体，只能批准判定为无争议的申请。
const systemActor = "system:auto-policy"

// validityAfterReview 是审核截止后申请仍然有效的时长。
const validityAfterReview = time.Hour

// proposal 是退款 Agent 输出的结构化提议；Agent 只能把无争议升级为有争议。
type proposal struct {
	Action        string `json:"action"` // refund 或 no_refund
	OrderID       string `json:"order_id"`
	AmountCents   int64  `json:"amount_cents"`
	Reason        string `json:"reason"`
	AgentDisputed bool   `json:"agent_disputed"`
	AgentNote     string `json:"agent_note"`
}

// registration 是登记操作的结果，随操作记录固定。Rejected 非空表示业务拒绝，未登记。
type registration struct {
	Rejected       string          `json:"rejected,omitempty"`
	Request        refund.Request  `json:"request"`
	Verdict        support.Verdict `json:"verdict"`
	Sequence       int64           `json:"sequence"`
	ReviewDeadline string          `json:"review_deadline"`
}

// execution 是执行操作的结果。OrderChanged 为真表示审核期间订单事实变化，需重新申请。
type execution struct {
	Receipt      string `json:"receipt,omitempty"`
	OrderChanged bool   `json:"order_changed,omitempty"`
}

type reviewKeyContext struct{}

// withReviewKey 把本次退款的审核中断键交给 Authorizer，用于读取该中断持久化的审核人名单。
func withReviewKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, reviewKeyContext{}, key)
}

func reviewKey(ctx context.Context) string {
	key, _ := ctx.Value(reviewKeyContext{}).(string)
	return key
}

// refundDesk 组合账本、refund.Service 与争议规则，提供登记与执行两个持久操作的执行体。
type refundDesk struct {
	ledger    *ledger
	clock     support.Clock
	reviewTTL time.Duration

	mu    sync.Mutex
	rules support.DisputeRules

	registers, executions atomic.Int32 // 实际执行次数，回放不计
}

func newRefundDesk(l *ledger, clock support.Clock, rules support.DisputeRules, reviewTTL time.Duration) *refundDesk {
	return &refundDesk{ledger: l, clock: clock, rules: rules, reviewTTL: reviewTTL}
}

// setRules 替换争议规则；只作用于之后的登记。
func (d *refundDesk) setRules(r support.DisputeRules) { d.mu.Lock(); defer d.mu.Unlock(); d.rules = r }

func (d *refundDesk) currentRules() support.DisputeRules {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rules
}

// requestID 由工单、订单与金额派生业务申请 ID：同一工单内的重复申请落到同一 ID，新工单算新申请。
func requestID(ticketID, orderID string, amountCents int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%d", ticketID, orderID, amountCents)))
	return "rf-" + hex.EncodeToString(sum[:])[:24]
}

func (d *refundDesk) service() *refund.Service {
	return &refund.Service{
		Store:      d.ledger,
		Now:        d.clock.Now,
		Authorizer: refund.AuthorizeFunc(d.authorize),
		Policy: func(_ context.Context, _ refund.Request, o refund.Order) error {
			if o.DeliveryDays > 30 {
				return errors.New("签收超过 30 天，不在售后期内")
			}
			return nil
		},
	}
}

// authorize 在 refund.Service 的事务内被调用，只能经同一事务查询：账本是单连接，另开连接会死锁。
func (d *refundDesk) authorize(ctx context.Context, actor string, req refund.Request) error {
	tx := d.ledger.activeTx()
	if tx == nil {
		return errors.New("授权必须在账本事务内调用")
	}
	if actor == systemActor {
		var data []byte
		if err := tx.QueryRowContext(ctx, "SELECT verdict_json FROM refund_verdicts WHERE request_id = ?", req.RequestID).Scan(&data); err != nil {
			return fmt.Errorf("读取申请 %s 的判定: %w", req.RequestID, err)
		}
		var v support.Verdict
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		if v.Disputed {
			return errors.New("系统主体只能批准无争议的申请")
		}
		return nil
	}
	id, users, found, err := approvers(ctx, tx, reviewKey(ctx))
	if err != nil {
		return err
	}
	if !found || id != req.RequestID || !containsUser(users, actor) {
		return fmt.Errorf("%s 不在申请 %s 的审核人名单中", actor, req.RequestID)
	}
	return nil
}

// register 校验订单归属，登记申请，并经 OnRegister 在同一事务内写入判定与登记序号。
// 重跑时 Register 返回已有申请且不调用钩子，判定只能从账本读取；缺失时报错，不以零值“无争议”放行。
func (d *refundDesk) register(ctx context.Context, ticketID, customerID string, p proposal) (json.RawMessage, error) {
	d.registers.Add(1)
	o, err := d.ledger.order(ctx, p.OrderID)
	if errors.Is(err, refund.ErrNotFound) || (err == nil && o.CustomerID != customerID) {
		return json.Marshal(registration{Rejected: "未找到该订单，请核对订单号"})
	}
	if err != nil {
		return nil, err
	}
	id := requestID(ticketID, p.OrderID, p.AmountCents)
	deadline := d.clock.Now().Add(d.reviewTTL)
	req := refund.Request{
		RequestID: id, OrderID: p.OrderID, AmountCents: p.AmountCents, Currency: o.Currency, PaidCents: o.PaidCents,
		DeliveryDays: o.DeliveryDays, OrderVersion: o.Version, ValidUntil: deadline.Add(validityAfterReview).UTC().Format(time.RFC3339Nano),
	}
	rules := d.currentRules()
	svc := d.service()
	svc.OnRegister = func(ctx context.Context, tx refund.Transaction, r refund.Request) error {
		lt, ok := tx.(ledgerTx)
		if !ok {
			return errors.New("账本事务类型不符")
		}
		_, err := lt.registerVerdict(ctx, r, func(f support.DisputeFacts) support.Verdict {
			return rules.Judge(f, p.AgentDisputed, p.AgentNote)
		})
		return err
	}
	registered, err := svc.Register(ctx, req)
	if err != nil {
		return nil, err
	}
	reg, found, err := d.load(ctx, registered)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("申请 %s 已登记但缺少争议判定", id)
	}
	return json.Marshal(reg)
}

// load 组装已登记申请的判定、序号与审核截止时间。
func (d *refundDesk) load(ctx context.Context, req refund.Request) (registration, bool, error) {
	v, seq, found, err := d.ledger.verdict(ctx, req.RequestID)
	if err != nil || !found {
		return registration{}, false, err
	}
	until, err := time.Parse(time.RFC3339Nano, req.ValidUntil)
	if err != nil {
		return registration{}, false, err
	}
	return registration{Request: req, Verdict: v, Sequence: seq, ReviewDeadline: until.Add(-validityAfterReview).Format(time.RFC3339)}, true, nil
}

// reconcileRegister 只有申请与判定都已存在时才补记完成，否则交给重跑。
func (d *refundDesk) reconcileRegister(id string) func(context.Context) (json.RawMessage, bool, error) {
	return func(ctx context.Context) (json.RawMessage, bool, error) {
		req, found, err := d.ledger.request(ctx, id)
		if err != nil || !found {
			return nil, false, err
		}
		reg, found, err := d.load(ctx, req)
		if err != nil || !found {
			return nil, false, err
		}
		data, err := json.Marshal(reg)
		return data, err == nil, err
	}
}

// execute 先按申请 ID 查回执：Approve 在查回执之前就校验有效期与订单事实，退款已生效后先 Approve 会卡死。
// 订单事实变化时把“需重新申请”作为业务结果返回；否则以 actor 批准并退款。
func (d *refundDesk) execute(ctx context.Context, req refund.Request, actor string) (json.RawMessage, error) {
	d.executions.Add(1)
	if receipt, ok, err := d.ledger.receipt(ctx, req.RequestID); err != nil || ok {
		if err != nil {
			return nil, err
		}
		return json.Marshal(execution{Receipt: receipt})
	}
	o, err := d.ledger.order(ctx, req.OrderID)
	if err != nil {
		return nil, fmt.Errorf("读取订单 %s: %w", req.OrderID, err)
	}
	if o.Version != req.OrderVersion || o.PaidCents != req.PaidCents || o.Currency != req.Currency || o.DeliveryDays != req.DeliveryDays {
		return json.Marshal(execution{OrderChanged: true})
	}
	svc := d.service()
	if err := svc.Approve(ctx, req, actor); err != nil {
		return nil, err
	}
	receipt, err := svc.Refund(ctx, workflow.DurableKey(ctx), req)
	if err != nil {
		return nil, err
	}
	return json.Marshal(execution{Receipt: receipt})
}

// reconcileExecute 按申请 ID 查回执，存在则补记完成。
func (d *refundDesk) reconcileExecute(id string) func(context.Context) (json.RawMessage, bool, error) {
	return func(ctx context.Context) (json.RawMessage, bool, error) {
		receipt, ok, err := d.ledger.receipt(ctx, id)
		if err != nil || !ok {
			return nil, false, err
		}
		data, err := json.Marshal(execution{Receipt: receipt})
		return data, err == nil, err
	}
}

// registerOnce 以持久操作登记：意图只含申请 ID 与提议，不含规则阈值或时间。登记按申请 ID 幂等，
// 中断（pending 未核对到、或被取消）后可安全重跑。
func (d *refundDesk) registerOnce(ctx context.Context, ticketID, customerID string, p proposal) (registration, error) {
	id := requestID(ticketID, p.OrderID, p.AmountCents)
	intent, err := json.Marshal(struct {
		RequestID string   `json:"request_id"`
		Proposal  proposal `json:"proposal"`
	}{id, p})
	if err != nil {
		return registration{}, err
	}
	policy := durable.OperationPolicy{MaxAttempts: 3, RetryInterrupted: true, Reconcile: d.reconcileRegister(id)}
	record, err := durable.Do(ctx, "refund/register", intent, policy, func(ctx context.Context) (json.RawMessage, error) {
		return d.register(ctx, ticketID, customerID, p)
	})
	if err != nil {
		return registration{}, err
	}
	var reg registration
	return reg, json.Unmarshal(record.Value, &reg)
}

// executeOnce 以持久操作执行退款。退款可能已提交而调用返回错误（记录为 failed），RetryFailed 让续跑重跑 run，
// 由 run 先查回执确认；记录停在 pending 时先经 Reconcile 按回执补记。
func (d *refundDesk) executeOnce(ctx context.Context, req refund.Request, actor string) (execution, error) {
	intent, err := json.Marshal(struct {
		RequestID string `json:"request_id"`
		Approver  string `json:"approver"`
	}{req.RequestID, actor})
	if err != nil {
		return execution{}, err
	}
	policy := durable.OperationPolicy{MaxAttempts: 3, RetryFailed: true, Reconcile: d.reconcileExecute(req.RequestID)}
	record, err := durable.Do(ctx, "refund/execute", intent, policy, func(ctx context.Context) (json.RawMessage, error) {
		return d.execute(ctx, req, actor)
	})
	if err != nil {
		return execution{}, err
	}
	var out execution
	return out, json.Unmarshal(record.Value, &out)
}
