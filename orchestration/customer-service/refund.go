package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
	"github.com/kart-io/goagent/core"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/multiagent"
	"github.com/kart-io/goagent/tools"
	"github.com/kart-io/goagent/workflow"
	"github.com/kart-io/goagent/workflow/refund"
)

// sessionCleanupTimeout 是关闭退款 Agent 会话的上限，与 goagent multiagent 的会话清理时限一致。
const sessionCleanupTimeout = 5 * time.Second

// 中断 ID：退款审核与向客户追问。
const (
	reviewInterrupt = "refund-review"
	askInterrupt    = "ask_customer"
)

type customerContext struct{}

// withCustomer 由退款成员注入当前工单的客户 ID；get_order 只从这里取，不信任模型参数。
func withCustomer(ctx context.Context, customerID string) context.Context {
	return context.WithValue(ctx, customerContext{}, customerID)
}

func customerFrom(ctx context.Context) string {
	id, _ := ctx.Value(customerContext{}).(string)
	return id
}

// orderTool 只查询当前客户名下的订单；不属于该客户时与不存在一样返回“未找到”，不泄露他人订单。
func orderTool(l *ledger) (interfaces.Tool, error) {
	return tools.NewFunctionToolBuilder("get_order").WithDescription("查询当前客户名下的订单；只读").
		WithArgsSchema(`{"type":"object","properties":{"order_id":{"type":"string"}},"required":["order_id"],"additionalProperties":false}`).
		WithFunction(func(ctx context.Context, args map[string]any) (any, error) {
			id, _ := args["order_id"].(string)
			o, err := l.order(ctx, id)
			if errors.Is(err, refund.ErrNotFound) || (err == nil && o.CustomerID != customerFrom(ctx)) {
				return "未找到该订单", nil
			}
			if err != nil {
				return nil, err
			}
			return map[string]any{"order_id": id, "paid_cents": o.PaidCents, "currency": o.Currency, "delivery_days": o.DeliveryDays}, nil
		}).Build()
}

// askGate 保证同一时刻只有一个未回答的追问：等待中的追问不释放，回答后才释放。
// timedOut 记录本次会话是否有追问超时；续跑时工具整体重新执行，按固定的回应重建。
type askGate struct {
	mu       sync.Mutex
	pending  bool
	timedOut bool
}

func (g *askGate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending {
		return false
	}
	g.pending = true
	return true
}

func (g *askGate) answered(timeout bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pending = false
	g.timedOut = g.timedOut || timeout
}

func (g *askGate) anyTimedOut() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.timedOut }

// askAnswer 是追问的回应：客户回复 {"answer": "..."}，超时 {"timeout": true}。
type askAnswer struct {
	Answer  string `json:"answer,omitempty"`
	Timeout bool   `json:"timeout,omitempty"`
}

// validateAsk 是纯函数：格式不对时保持等待。
func validateAsk(raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var a askAnswer
	if err := dec.Decode(&a); err != nil || (a.Answer == "") == !a.Timeout {
		return errors.New("追问回应应为 {answer} 或 {timeout: true}")
	}
	return nil
}

// askTool 是可中断工具：调用 durable.AwaitInput 暂停，续跑时整体重新执行。只允许一次尝试，
// 崩溃留下的 pending 无法自动恢复，由宿主按运行失败转人工。
func askTool(gate *askGate) (interfaces.Tool, error) {
	return tools.NewFunctionToolBuilder(askInterrupt).WithDescription("向客户追问一个问题并等待回答").
		WithArgsSchema(`{"type":"object","properties":{"question":{"type":"string"}},"required":["question"],"additionalProperties":false}`).
		WithInterruptible("ask-customer-v1").
		WithFunction(func(ctx context.Context, args map[string]any) (any, error) {
			if !gate.enter() {
				return "已有未回答的追问，请等待客户回复后再问", nil
			}
			question, _ := args["question"].(string)
			info, err := json.Marshal(map[string]string{"question": question})
			if err != nil {
				return nil, err
			}
			raw, err := durable.AwaitInput(ctx, askInterrupt, info, validateAsk)
			if err != nil {
				return nil, err
			}
			var a askAnswer
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, err
			}
			gate.answered(a.Timeout)
			if a.Timeout {
				return "客户未在时限内回复，本次不能办理退款，请输出 no_refund 并说明原因", nil
			}
			return "客户回复：" + a.Answer, nil
		}).Build()
}

// reviewInput 是审核回应；approver 由宿主认证后填入，reason 为 timeout 表示等待超时按拒绝处理。
// answered_at 由宿主在构造输入时写入，随中断输入一起固定，供告知后到的审核人“何时”处理。
type reviewInput struct {
	Approved   bool   `json:"approved"`
	Note       string `json:"note,omitempty"`
	Approver   string `json:"approver,omitempty"`
	Reason     string `json:"reason,omitempty"`
	AnsweredAt string `json:"answered_at,omitempty"`
}

// validateReview 是纯函数：格式错误、批准却没有审核人或以系统主体批准时保持等待。
func validateReview(raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in reviewInput
	if err := dec.Decode(&in); err != nil || (in.Reason != "" && in.Reason != "timeout") || (in.Approved && in.Reason != "") {
		return errors.New("审核输入应为 {approved, note, approver} 或超时输入")
	}
	if in.Approved && (in.Approver == "" || in.Approver == systemActor) {
		return errors.New("批准时必须提供人工审核人")
	}
	return nil
}

// reviewInfo 是审核中断的 Info，经快照暴露给宿主，不含凭证。
type reviewInfo struct {
	RequestID      string   `json:"request_id"`
	OrderID        string   `json:"order_id"`
	AmountCents    int64    `json:"amount_cents"`
	Currency       string   `json:"currency"`
	Submitter      string   `json:"submitter"`
	Reasons        []string `json:"reasons"`
	ReviewDeadline string   `json:"review_deadline"`
}

// modelJSON 取模型输出中的 JSON：纯 JSON 原样返回；否则取恰好一个 Markdown 代码块的内容
// （部分模型会把 JSON 放进代码块，或先写说明再给代码块）。是纯函数，重入时结果相同。
func modelJSON(text string) string {
	body := strings.TrimSpace(text)
	if strings.HasPrefix(body, "{") {
		return body
	}
	parts := strings.Split(body, "```")
	if len(parts) != 3 {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(parts[1]), "json"))
}

// parseProposal 是对已回放模型输出的纯函数，重入时结果相同；内容严格校验，不接受未知字段或尾随内容。
func parseProposal(text string) (proposal, bool) {
	dec := json.NewDecoder(strings.NewReader(modelJSON(text)))
	dec.DisallowUnknownFields()
	var p proposal
	if err := dec.Decode(&p); err != nil || dec.Decode(&struct{}{}) != io.EOF || (p.Action != "refund" && p.Action != "no_refund") {
		return proposal{}, false
	}
	return p, true
}

// refundMember 是退款成员：提议 → 登记（固定判定）→ 有争议时等待审核 → 执行（先查回执）。
// 资金相关的每段都由持久操作固定，节点重入时回放。
func refundMember(desk *refundDesk, client *livemodel.Client) multiagent.Step[Ticket] {
	return multiagent.Step[Ticket]{Name: refundName, Run: core.NewRunnableFunc(func(ctx context.Context, t Ticket) (_ Ticket, err error) {
		done := func(text string, rc RefundCase, s support.Solution) (Ticket, error) {
			t = appendTurn(t, refundName, text)
			t.Refund = &rc
			return addSolution(t, s), nil
		}
		if t.Refund != nil { // 本轮已有结论时分流又派回退款：不重新登记或审核，否则已超时拒绝的申请可能被再次批准
			return appendTurn(t, refundName, "本工单的退款已有结论（"+t.Refund.Status+"），不再重复处理"), nil
		}
		ctx = withCustomer(ctx, t.CustomerID)
		gate := &askGate{}
		factory := agentFactory(client, "refund-agent", refundPrompt, 6, func() ([]interfaces.Tool, error) {
			order, err := orderTool(desk.ledger)
			if err != nil {
				return nil, err
			}
			ask, err := askTool(gate)
			if err != nil {
				return nil, err
			}
			return []interfaces.Tool{order, ask}, nil
		})
		owned, err := factory(ctx)
		if err != nil {
			return t, err
		}
		defer func() {
			cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
			defer cancel()
			if cerr := owned.Close(cleanCtx); cerr != nil { // 只在关闭出错时合并，不改变等待错误的原样返回
				err = errors.Join(err, cerr)
			}
		}()
		input, err := render(t)
		if err != nil {
			return t, err
		}
		out, err := owned.Runnable.Invoke(ctx, input)
		if err != nil {
			return t, err // 追问等待也从这里返回，图据此暂停
		}
		p, ok := parseProposal(resultText(out))
		switch {
		case !ok:
			return done("退款提议无法解析，未登记", RefundCase{Status: "no_refund"}, support.Solution{Title: "人工核实退款诉求", Detail: "系统无法理解本次退款诉求，建议由人工客服核实"})
		case p.Action == "refund" && gate.anyTimedOut(): // 客户没回答追问时不凭不完整的信息退款，不依赖模型遵守提示
			return done("客户未在时限内回复追问，未登记退款", RefundCase{Status: "no_refund", OrderID: p.OrderID},
				support.Solution{Title: "人工跟进退款诉求", Detail: "客户未在时限内回复追问，本次未办理退款，可由人工客服联系客户核实"})
		case p.Action == "no_refund" || p.OrderID == "" || p.AmountCents <= 0:
			return done("未登记退款："+p.Reason, RefundCase{Status: "no_refund", OrderID: p.OrderID}, support.Solution{Title: "无需退款", Detail: p.Reason})
		}
		reg, err := desk.registerOnce(ctx, t.TicketID, t.CustomerID, p)
		if err != nil {
			return t, err
		}
		if reg.Rejected != "" {
			return done(reg.Rejected, RefundCase{Status: "rejected", OrderID: p.OrderID}, support.Solution{Title: "核对订单号", Detail: reg.Rejected})
		}
		rc := RefundCase{RequestID: reg.Request.RequestID, OrderID: p.OrderID, AmountCents: p.AmountCents, Reasons: reg.Verdict.Reasons}
		actor := systemActor
		if reg.Verdict.Disputed {
			key := durable.ChildKey(workflow.DurableKey(ctx), "interrupt/"+reviewInterrupt)
			ctx = withReviewKey(ctx, key)
			info, err := json.Marshal(reviewInfo{RequestID: rc.RequestID, OrderID: rc.OrderID, AmountCents: rc.AmountCents, Currency: reg.Request.Currency,
				Submitter: t.Submitter, Reasons: reg.Verdict.Reasons, ReviewDeadline: reg.ReviewDeadline})
			if err != nil {
				return t, err
			}
			raw, err := durable.AwaitInput(ctx, reviewInterrupt, info, validateReview)
			if err != nil {
				return t, err
			}
			var in reviewInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return t, err
			}
			alternative := support.Solution{Title: "补充凭证后重新申请", Detail: "可补充订单问题的照片或说明，由人工客服重新提交", RequiresApproval: true}
			switch {
			case !in.Approved && in.Reason == "timeout":
				rc.Status = "timeout"
				return done("审核超时，按拒绝处理，未退款", rc, alternative)
			case !in.Approved:
				rc.Status = "declined"
				return done("审核未通过："+in.Note, rc, alternative)
			}
			actor = in.Approver
		}
		result, err := desk.executeOnce(ctx, reg.Request, actor)
		if err != nil {
			return t, err
		}
		if result.OrderChanged {
			rc.Status = "order_changed"
			return done("订单已变化，需重新申请", rc, support.Solution{Title: "重新申请退款", Detail: "订单信息在审核期间发生变化，请重新发起退款申请"})
		}
		rc.Status, rc.Receipt = "refunded", result.Receipt
		return done(fmt.Sprintf("已退款 %d 分，回执 %s", p.AmountCents, result.Receipt), rc, support.Solution{Title: "已退款", Detail: "退款已提交，原路退回"})
	})}
}
