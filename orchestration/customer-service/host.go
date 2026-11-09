package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/workflow"
)

// 工单状态。状态转换与对应的发件箱记录在同一事务内写入。
const (
	statusRunning         = "running"
	statusWaitingReview   = "waiting_review"
	statusWaitingCustomer = "waiting_customer"
	statusDone            = "done"
	statusHandoff         = "handoff"
	statusFailed          = "failed"
	statusDelivered       = "delivered"
)

// 发件箱通道：notify 经 Notifier 投递，handoff 经 Handoff 转人工。
const (
	channelNotify  = "notify"
	channelHandoff = "handoff"
)

// maxFailures 是同一运行连续推进失败的上限，达到后按运行失败转人工。
const maxFailures = 3

// answerPending 是 ticket_messages.handled 的取值之一：追问的回答已收到、尚未固定到中断。
// 0 为排队（本轮结束后作为新一轮咨询），1 为已处理。
const answerPending = 2

// ErrNotApprover 表示作答人不在该审核中断持久化的审核人名单中；运行保持等待。
var ErrNotApprover = errors.New("作答人不是该工单的审核人")

// ErrNotWaitingReview 表示工单从未进入过退款审核，没有可作答或可告知结果的审核。
var ErrNotWaitingReview = errors.New("工单没有退款审核")

// ErrReviewNotReady 表示审核回答尚未固定而工单暂不在等待审核（如推进失败后记为 running）；可稍后重试，不通知、不占用幂等键。
var ErrReviewNotReady = errors.New("审核暂不可作答，请稍后重试")

// HostConfig 是宿主配置；零值字段使用默认值。
type HostConfig struct {
	Dir              string
	Client           *livemodel.Client // nil 时使用离线脚本模型
	Clock            support.Clock     // nil 时使用系统时钟
	Resolver         support.ApproverResolver
	Notifier         support.Notifier
	Handoff          support.Handoff
	Operator         support.User // 运营告警的接收人
	Rules            support.DisputeRules
	ReviewTTL        time.Duration // 审核时限，默认 48 小时
	AskTTL           time.Duration // 追问时限，默认 48 小时
	RemindBefore     time.Duration // 审核临近超时的提醒提前量，默认 4 小时
	StaleAfter       time.Duration // running 超过该时长没有进展视为宿主中途退出，默认 2 分钟
	LockTimeout      time.Duration // 每个工单读取快照或推进运行的上限，默认 30 秒
	MaxModelRequests int           // 单个运行的模型请求上限，默认 40
	UsageAlertAt     int           // 用量告警阈值（模型请求数），默认 20
	DisableUsageHook bool          // 关闭 UsageHook，只靠扫描补偿
	Store            durable.Store // 可选：运行检查点与操作日志的存储，默认 Dir/runs
	InspectOnly      bool          // 只用于 show 等只读查看：不检查目录绑定的模型，不得推进运行
}

func (c *HostConfig) defaults() error {
	if c.Dir == "" || c.Resolver == nil || c.Notifier == nil || c.Handoff == nil || c.Operator.ID == "" {
		return errors.New("宿主需要目录、审核人解析、通知、转人工与运营接收人")
	}
	if c.Client == nil {
		c.Client = offlineModel()
	}
	if c.Clock == nil {
		c.Clock = support.SystemClock{}
	}
	if c.Rules.Version == "" {
		c.Rules = support.DefaultRules()
	}
	if err := c.Rules.Validate(); err != nil {
		return err
	}
	set := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	set(&c.ReviewTTL, 48*time.Hour)
	set(&c.AskTTL, 48*time.Hour)
	set(&c.RemindBefore, 4*time.Hour)
	set(&c.StaleAfter, 2*time.Minute)
	set(&c.LockTimeout, 30*time.Second)
	if c.MaxModelRequests <= 0 {
		c.MaxModelRequests = 40
	}
	if c.UsageAlertAt <= 0 {
		c.UsageAlertAt = 20
	}
	return nil
}

// Host 负责一切对外投递、超时与作答认证；图只做决定与等待，因为节点重入时节点内发消息会重复。
type Host struct {
	cfg     HostConfig
	ledger  *ledger
	desk    *refundDesk
	graph   *workflow.DurableGraph[Ticket]
	budgets durable.Store

	deliverMu sync.Mutex // 进程内串行化发件箱投递
}

func OpenHost(ctx context.Context, cfg HostConfig) (*Host, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	if !cfg.InspectOnly {
		if err := bindModel(cfg.Dir, cfg.Client.Live); err != nil {
			return nil, err
		}
	}
	l, err := openLedger(cfg.Dir)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Host, error) { _ = l.Close(); return nil, err }
	if cfg.Store == nil {
		if cfg.Store, err = durable.NewFileStore(filepath.Join(cfg.Dir, "runs")); err != nil {
			return fail(err)
		}
	}
	budgets, err := durable.NewFileStore(filepath.Join(cfg.Dir, "budget"))
	if err != nil {
		return fail(err)
	}
	desk := newRefundDesk(l, cfg.Clock, cfg.Rules, cfg.ReviewTTL)
	g, err := newGraph(ctx, cfg.Store, cfg.Client, desk)
	if err != nil {
		return fail(err)
	}
	return &Host{cfg: cfg, ledger: l, desk: desk, graph: g, budgets: budgets}, nil
}

func (h *Host) Close() error { return h.ledger.Close() }

func (h *Host) now() time.Time { return h.cfg.Clock.Now().UTC() }

// ticketRow 是 tickets 表的一行，它同时是运行索引：durable.Store 不能枚举运行。
type ticketRow struct {
	ID, CustomerID, Submitter string
	Round                     int
	RunID, Input, Status      string
	WaitKey, WaitID           string
	Deadline, LastProgress    string
	Failures                  int
}

const ticketColumns = "ticket_id, customer_id, submitter, round, run_id, input, status, wait_key, wait_id, deadline, last_progress, failures"

func scanTicket(scan func(...any) error) (ticketRow, error) {
	var r ticketRow
	err := scan(&r.ID, &r.CustomerID, &r.Submitter, &r.Round, &r.RunID, &r.Input, &r.Status, &r.WaitKey, &r.WaitID, &r.Deadline, &r.LastProgress, &r.Failures)
	return r, err
}

func (h *Host) ticket(ctx context.Context, q rowQuerier, id string) (ticketRow, error) {
	r, err := scanTicket(q.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM tickets WHERE ticket_id = ?", id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("工单 %s 不存在", id)
	}
	return r, err
}

// write 在一个即时写事务内执行 fn。
func (h *Host) write(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := h.ledger.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// claim 记录一次性事件，返回是否首次记录；与同一事务内的动作同生共灭。
func claim(ctx context.Context, tx *sql.Tx, key, ticketID string) (bool, error) {
	res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO events VALUES (?,?)", key, ticketID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func claimed(ctx context.Context, q rowQuerier, key string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE event_key = ?", key).Scan(&n)
	return n > 0, err
}

// enqueue 写入发件箱；同一幂等键只写一次。
func enqueue(ctx context.Context, tx *sql.Tx, ticketID, channel string, to support.User, payload any, key string) error {
	recipient, err := json.Marshal(to)
	if err != nil {
		return err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO outbox(idempotency_key, ticket_id, channel, recipient_json, payload_json, seq)
		VALUES (?,?,?,?,?,(SELECT COALESCE(MAX(seq),0)+1 FROM outbox))`, key, ticketID, channel, recipient, data)
	return err
}

func notify(ctx context.Context, tx *sql.Tx, ticketID string, to support.User, kind support.MessageKind, key, title, body string) error {
	return enqueue(ctx, tx, ticketID, channelNotify, to, support.Message{Kind: kind, IdempotencyKey: key, Title: title, Body: body}, key)
}

// MessageResult 是一次客户消息的处理结果。
type MessageResult struct {
	TicketID string `json:"ticket_id"`
	Status   string `json:"status"`
	Action   string `json:"action"` // started、progress、answered、queued、duplicate
}

// Message 投递一条客户消息：没有未结束工单时新建工单并运行；等待审核时归入工单并自动回复进度；
// 等待追问时作为追问的回答续跑；运行中时记入待处理队列，运行结束后作为新一轮咨询。
func (h *Host) Message(ctx context.Context, customerID, submitter, messageID, text string) (MessageResult, error) {
	if customerID == "" || messageID == "" || text == "" {
		return MessageResult{}, errors.New("消息需要客户 ID、消息 ID 与内容")
	}
	var row ticketRow
	var action string
	err := h.write(ctx, func(tx *sql.Tx) error {
		var ticketID string
		var prev int
		err := tx.QueryRowContext(ctx, "SELECT ticket_id, handled FROM ticket_messages WHERE message_id = ?", messageID).Scan(&ticketID, &prev)
		if err == nil {
			action = "duplicate"
			row, err = h.ticket(ctx, tx, ticketID)
			if err == nil && prev == answerPending && row.Status == statusWaitingCustomer {
				action = "answered" // 上次续跑失败、回答未固定：重投时再次续跑
			}
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		row, err = scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM tickets WHERE customer_id = ? AND status IN (?,?,?)",
			customerID, statusRunning, statusWaitingReview, statusWaitingCustomer).Scan)
		now := h.now().Format(time.RFC3339Nano)
		if errors.Is(err, sql.ErrNoRows) {
			row = ticketRow{ID: "tk-" + messageID, CustomerID: customerID, Submitter: submitter, Round: 1, Input: text, Status: statusRunning, LastProgress: now}
			row.RunID = row.ID + "#1"
			if _, err = tx.ExecContext(ctx, "INSERT INTO tickets("+ticketColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?)", row.ID, row.CustomerID, row.Submitter,
				row.Round, row.RunID, row.Input, row.Status, "", "", "", now, 0); err != nil {
				return err
			}
			action = "started"
		} else if err != nil {
			return err
		}
		handled := 1
		switch {
		case action == "started":
		case row.Status == statusWaitingReview:
			action = "progress"
			body := "您的退款申请正在审核中，预计 " + row.Deadline + " 前给出结果，结果出来后会第一时间告诉您。"
			if err = notify(ctx, tx, row.ID, support.User{ID: row.CustomerID}, support.KindProgress, messageID, "审核进度", body); err != nil {
				return err
			}
		case row.Status == statusWaitingCustomer:
			action, handled = "answered", answerPending
		default:
			action, handled = "queued", 0
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO ticket_messages VALUES (?,?,(SELECT COALESCE(MAX(seq),0)+1 FROM ticket_messages),?,?)", messageID, row.ID, text, handled)
		return err
	})
	if err != nil {
		return MessageResult{}, err
	}
	switch action {
	case "started":
		err = h.advance(ctx, row, nil)
	case "answered":
		_, err = h.answerCustomer(ctx, row)
	}
	h.deliver(ctx)
	out, e := h.ticket(ctx, h.ledger.db, row.ID)
	return MessageResult{TicketID: row.ID, Status: out.Status, Action: action}, errors.Join(err, e)
}

// budgetContext 打开该运行的根预算：RootKey 必须等于运行 ID，配置在运行生命周期内不变。
func (h *Host) budgetContext(ctx context.Context, row ticketRow) (context.Context, error) {
	c, err := budget.NewDurable(budget.Config{RootKey: row.RunID, Version: graphVersion, MaxModelRequests: h.cfg.MaxModelRequests}, h.budgets)
	if err != nil {
		return nil, err
	}
	ctx = budget.WithController(ctx, c)
	if !h.cfg.DisableUsageHook {
		ctx = budget.WithUsageHook(ctx, func(hookCtx context.Context, e budget.UsageEvent) {
			if e.HasStatistics {
				_ = h.alertUsage(hookCtx, row, e.Statistics.Root.ReservedRequests)
			}
		})
	}
	return ctx, nil
}

// alertUsage 在模型请求数达到阈值时告警运营一次。
func (h *Host) alertUsage(ctx context.Context, row ticketRow, requests int) error {
	if requests < h.cfg.UsageAlertAt {
		return nil
	}
	key := fmt.Sprintf("%s/usage/%d", row.RunID, h.cfg.UsageAlertAt)
	return h.write(ctx, func(tx *sql.Tx) error {
		first, err := claim(ctx, tx, key, row.ID)
		if err != nil || !first {
			return err
		}
		body := fmt.Sprintf("工单 %s 的运行 %s 已发出 %d 次模型请求，达到告警阈值 %d。", row.ID, row.RunID, requests, h.cfg.UsageAlertAt)
		return notify(ctx, tx, row.ID, h.cfg.Operator, support.KindUsageAlert, key, "用量告警", body)
	})
}

// advance 推进一次运行（resume 为 nil 时用 Run 续跑），并按结果更新工单。
func (h *Host) advance(ctx context.Context, row ticketRow, resume *workflow.ResumeRequest) error {
	snap, err := h.step(ctx, row, resume)
	return h.observe(ctx, row, snap, err)
}

// step 执行一次 Run 或 Resume。每个工单有上限时间（含等待运行锁），超时跳过、下次再试。
func (h *Host) step(ctx context.Context, row ticketRow, resume *workflow.ResumeRequest) (*workflow.GraphSnapshot[Ticket], error) {
	ctx, cancel := context.WithTimeout(ctx, h.cfg.LockTimeout)
	defer cancel()
	runCtx, err := h.budgetContext(ctx, row)
	if err != nil {
		return nil, err
	}
	if resume != nil {
		return h.graph.Resume(runCtx, row.RunID, *resume)
	}
	return h.graph.Run(runCtx, row.RunID, Ticket{TicketID: row.ID, CustomerID: row.CustomerID, Submitter: row.Submitter, Input: row.Input}, "")
}

// resume 回应工单当前等待的中断。另一方（作答或超时）先到时返回 late 为真，不计为失败，也不改写工单。
func (h *Host) resume(ctx context.Context, row ticketRow, input json.RawMessage) (late bool, err error) {
	snap, err := h.step(ctx, row, &workflow.ResumeRequest{Key: row.WaitKey, Input: input})
	if errors.Is(err, agentErrors.ErrApprovalMismatch) || errors.Is(err, agentErrors.ErrInterruptAnswered) {
		return true, nil
	}
	return false, h.observe(ctx, row, snap, err)
}

// answerCustomer 以最早一条待确认的回答回应追问；没有待确认的回答时返回 false。
// 回答固定（续跑成功、本次后到，或续跑失败但中断已记下回答）后，序号不晚于它的待确认回答都记为已处理：
// 中断只采用第一个被接受的回答，与后到的回答一样不再生效；续跑期间为新追问到达的回答不受影响。
// 回答未固定时保持待确认，供重投或扫描重试。
func (h *Host) answerCustomer(ctx context.Context, row ticketRow) (bool, error) {
	var text string
	var seq int64
	err := h.ledger.db.QueryRowContext(ctx, "SELECT text, seq FROM ticket_messages WHERE ticket_id = ? AND handled = ? ORDER BY seq LIMIT 1",
		row.ID, answerPending).Scan(&text, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	input, err := json.Marshal(askAnswer{Answer: text})
	if err != nil {
		return true, err
	}
	late, runErr := h.resume(ctx, row, input)
	fixed := late || runErr == nil
	if !fixed {
		readCtx, cancel := context.WithTimeout(ctx, h.cfg.LockTimeout)
		_, answered, e := durable.InterruptAnswer(readCtx, h.cfg.Store, row.WaitKey, askInterrupt)
		cancel()
		if e != nil {
			return true, errors.Join(runErr, e)
		}
		fixed = answered
	}
	if fixed {
		_, err = h.ledger.db.ExecContext(ctx, "UPDATE ticket_messages SET handled = 1 WHERE ticket_id = ? AND handled = ? AND seq <= ?", row.ID, answerPending, seq)
	}
	return true, errors.Join(runErr, err)
}

// observe 把一次推进的结果写回工单表：等待、结束、超限转人工或计入失败。
func (h *Host) observe(ctx context.Context, row ticketRow, snap *workflow.GraphSnapshot[Ticket], runErr error) error {
	state := func() (Ticket, error) {
		if snap != nil {
			return snap.State, nil
		}
		readCtx, cancel := context.WithTimeout(ctx, h.cfg.LockTimeout)
		defer cancel()
		s, err := h.graph.Snapshot(readCtx, row.RunID)
		if err != nil {
			return Ticket{}, err
		}
		return s.State, nil
	}
	finishWith := func(reason support.HandoffReason, status string) error {
		t, err := state()
		if err != nil {
			return errors.Join(runErr, err)
		}
		return h.finish(ctx, row, t, reason, status)
	}
	switch {
	case runErr == nil && snap != nil && snap.Done:
		return h.finish(ctx, row, snap.State, "", "")
	case errors.Is(runErr, agentErrors.ErrApprovalRequired) && snap != nil && len(snap.Interrupts) > 0:
		return h.wait(ctx, row, snap.Interrupts[0])
	case errors.Is(runErr, agentErrors.ErrWorkflowMaxSteps):
		return finishWith(support.ReasonMaxRounds, statusHandoff)
	case errors.Is(runErr, agentErrors.ErrModelBudgetExceeded):
		return finishWith(support.ReasonMaxRequests, statusHandoff)
	case errors.Is(runErr, agentErrors.ErrOperationAttention):
		return finishWith(support.ReasonRunFailed, statusFailed)
	case errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled):
		return runErr
	case runErr == nil:
		return fmt.Errorf("运行 %s 停在未知状态", row.RunID)
	}
	row.Failures++
	if row.Failures >= maxFailures {
		return errors.Join(runErr, finishWith(support.ReasonRunFailed, statusFailed))
	}
	// 记为 running：已固定的中断回答之后失败时，扫描以 Run 续跑；回答尚未固定时，续跑会重新回到等待。
	err := h.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE tickets SET status = ?, failures = ?, last_progress = ? WHERE ticket_id = ? AND run_id = ?",
			statusRunning, row.Failures, h.now().Format(time.RFC3339Nano), row.ID, row.RunID)
		return err
	})
	return errors.Join(runErr, err)
}

// wait 登记等待：首次观察到中断时解析审核人并发审核消息，或把问题发给客户。
// 审核人解析失败时告警运营、运行保持等待，事件不记录，下次扫描重试；截止时间仍取中断 Info。
// 解析失败不作为错误返回：工单已正确进入等待审核，运营告警即是信号。
func (h *Host) wait(ctx context.Context, row ticketRow, p workflow.InterruptPoint) error {
	status, deadline := statusWaitingCustomer, h.now().Add(h.cfg.AskTTL).Format(time.RFC3339)
	var info reviewInfo
	if p.ID == reviewInterrupt {
		if err := json.Unmarshal(p.Info, &info); err != nil {
			return err
		}
		status, deadline = statusWaitingReview, info.ReviewDeadline
	} else if row.WaitKey == p.Key && row.Deadline != "" {
		deadline = row.Deadline
	}
	done, err := claimed(ctx, h.ledger.db, p.Key)
	if err != nil {
		return err
	}
	var users []support.User
	var resolveErr error
	if !done && p.ID == reviewInterrupt {
		users, resolveErr = h.cfg.Resolver.Resolve(ctx, support.ApprovalRequest{RequestID: info.RequestID, Submitter: info.Submitter, AmountCents: info.AmountCents,
			Verdict: support.Verdict{Disputed: true, Reasons: info.Reasons}})
	}
	err = h.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE tickets SET status = ?, wait_key = ?, wait_id = ?, deadline = ?, last_progress = ?, failures = 0 WHERE ticket_id = ? AND run_id = ?",
			status, p.Key, p.ID, deadline, h.now().Format(time.RFC3339Nano), row.ID, row.RunID); err != nil {
			return err
		}
		if done {
			return nil
		}
		if resolveErr != nil {
			alert := fmt.Sprintf("工单 %s 的退款审核无法解析审核人：%v。运行保持等待，下次扫描重试。", row.ID, resolveErr)
			return notify(ctx, tx, row.ID, h.cfg.Operator, support.KindOpsAlert, p.Key+"/no-approver", "审核人解析失败", alert)
		}
		first, err := claim(ctx, tx, p.Key, row.ID)
		if err != nil || !first {
			return err // 并发时另一方已登记名单并发出消息，不再通知未持久化名单里的人
		}
		if p.ID != reviewInterrupt {
			// 待确认回答只会在工单等待追问时写入，此刻还在的都属于更早的追问（续跑成功但未记为已处理），不能用于新追问。
			if _, err := tx.ExecContext(ctx, "UPDATE ticket_messages SET handled = 1 WHERE ticket_id = ? AND handled = ?", row.ID, answerPending); err != nil {
				return err
			}
			var q struct {
				Question string `json:"question"`
			}
			if err := json.Unmarshal(p.Info, &q); err != nil {
				return err
			}
			return notify(ctx, tx, row.ID, support.User{ID: row.CustomerID}, support.KindCustomerQuestion, p.Key, "需要您补充信息", q.Question)
		}
		if err := saveApprovers(ctx, tx, p.Key, info.RequestID, users); err != nil {
			return err
		}
		body := reviewBody(info)
		for _, u := range users {
			if err := notify(ctx, tx, row.ID, u, support.KindApprovalRequest, p.Key+"/"+u.ID, "退款审核", body); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// finish 结束本轮：按 Outcome 只发一条最终消息（reply 回复客户；handoff 转人工并告知客户）。
// 本轮运行期间有排队的消息且结果为回复时，开启新一轮咨询，运行 ID 随轮次变化，根预算随之更新。
func (h *Host) finish(ctx context.Context, row ticketRow, t Ticket, reason support.HandoffReason, status string) error {
	if reason == "" && t.Outcome == OutcomeHandoff {
		reason, status = support.ReasonInvalidRoute, statusHandoff
	}
	if status == "" {
		status = statusDone
	}
	final := row.RunID + "/final"
	customer := support.User{ID: row.CustomerID}
	var next *ticketRow
	err := h.write(ctx, func(tx *sql.Tx) error {
		current, err := h.ticket(ctx, tx, row.ID)
		if err != nil || current.RunID != row.RunID {
			return err
		}
		if reason == "" {
			if err = notify(ctx, tx, row.ID, customer, support.KindCustomerReply, final, "客服回复", finalReply(t)); err != nil {
				return err
			}
		} else {
			if err = enqueue(ctx, tx, row.ID, channelHandoff, customer, handoffContext(row, t, reason), final); err != nil {
				return err
			}
			if err = notify(ctx, tx, row.ID, customer, support.KindHandoffNotice, final+"/notice", "已转人工", "您的问题已转交人工客服，稍后会有专人联系您。"); err != nil {
				return err
			}
		}
		now := h.now().Format(time.RFC3339Nano)
		var texts []string
		if status == statusDone {
			rows, err := tx.QueryContext(ctx, "SELECT text FROM ticket_messages WHERE ticket_id = ? AND handled = 0 ORDER BY seq", row.ID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var s string
				if err = rows.Scan(&s); err != nil {
					_ = rows.Close()
					return err
				}
				texts = append(texts, s)
			}
			if err = errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
		}
		if len(texts) == 0 {
			_, err = tx.ExecContext(ctx, "UPDATE tickets SET status = ?, wait_key = '', wait_id = '', deadline = '', last_progress = ? WHERE ticket_id = ?", status, now, row.ID)
			return err
		}
		n := row
		n.Round++
		n.RunID, n.Input, n.Status, n.Failures = fmt.Sprintf("%s#%d", row.ID, n.Round), strings.Join(texts, "\n"), statusRunning, 0
		next = &n
		if _, err = tx.ExecContext(ctx, "UPDATE ticket_messages SET handled = 1 WHERE ticket_id = ? AND handled = 0", row.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE tickets SET round = ?, run_id = ?, input = ?, status = ?, wait_key = '', wait_id = '', deadline = '', last_progress = ?, failures = 0 WHERE ticket_id = ?",
			n.Round, n.RunID, n.Input, n.Status, now, row.ID)
		return err
	})
	if err != nil || next == nil {
		return err
	}
	return h.advance(ctx, *next, nil)
}

// handoffContext 构造交接上下文；没有任何方案时补一条人工跟进，保证交接至少含一个方案。
func handoffContext(row ticketRow, t Ticket, reason support.HandoffReason) support.HandoffContext {
	solutions := t.Solutions
	if len(solutions) == 0 {
		solutions = []support.Solution{{Title: "人工客服跟进", Detail: "由人工客服核实后答复客户"}}
	}
	summary := "客户：" + row.Input
	for _, turn := range t.Turns {
		if turn.Agent != triageName {
			summary += "\n[" + turn.Agent + "]: " + turn.Output
		}
	}
	return support.HandoffContext{TicketID: row.ID, CustomerID: row.CustomerID, Intent: t.Intent, Solutions: solutions, Reason: reason, Summary: summary}
}

type outboxRow struct {
	Key, TicketID, Channel string
	Recipient, Payload     []byte
}

// deliver 投递发件箱中未完成的消息，至少一次；接收方按幂等键去重。失败时保留，下次扫描重试。
// 本轮全部发出后，已结束的工单记为 delivered。
func (h *Host) deliver(ctx context.Context) {
	h.deliverMu.Lock()
	defer h.deliverMu.Unlock()
	rows, err := h.ledger.db.QueryContext(ctx, "SELECT idempotency_key, ticket_id, channel, recipient_json, payload_json FROM outbox WHERE status = 'pending' ORDER BY seq")
	if err != nil {
		return
	}
	var pending []outboxRow
	for rows.Next() {
		var r outboxRow
		if rows.Scan(&r.Key, &r.TicketID, &r.Channel, &r.Recipient, &r.Payload) == nil {
			pending = append(pending, r)
		}
	}
	_ = rows.Close()
	for _, r := range pending {
		var to support.User
		err := json.Unmarshal(r.Recipient, &to)
		if err == nil && r.Channel == channelHandoff {
			var hc support.HandoffContext
			if err = json.Unmarshal(r.Payload, &hc); err == nil {
				err = h.cfg.Handoff.Transfer(ctx, r.Key, hc)
			}
		} else if err == nil {
			var m support.Message
			if err = json.Unmarshal(r.Payload, &m); err == nil {
				err = h.cfg.Notifier.Notify(ctx, to, m)
			}
		}
		status := "sent"
		if err != nil {
			status = "pending"
		}
		_, _ = h.ledger.db.ExecContext(ctx, "UPDATE outbox SET status = ?, attempts = attempts + 1 WHERE idempotency_key = ?", status, r.Key)
	}
	_, _ = h.ledger.db.ExecContext(ctx, `UPDATE tickets SET status = 'delivered' WHERE status IN (?,?,?)
		AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.ticket_id = tickets.ticket_id AND o.status = 'pending')`, statusDone, statusHandoff, statusFailed)
}

// AnswerResult 是一次审核作答的结果。Late 为真表示审核已有结果（超时、其他审核人或本人先前作答），
// 本次作答未生效、不返回错误，Result 是当前结果，并已经发件箱通知该审核人。
type AnswerResult struct {
	TicketID string `json:"ticket_id"`
	Status   string `json:"status"`
	Late     bool   `json:"late,omitempty"`
	Result   string `json:"result,omitempty"`
}

// Answer 以已认证的主体作答：确认其在该中断持久化的审核人名单中，再构造输入回应中断。
// 第一期只有命令行作答，approver 视为已认证（演示用，不是真实认证）。
func (h *Host) Answer(ctx context.Context, ticketID, approver string, approved bool, note string) (AnswerResult, error) {
	row, err := h.ticket(ctx, h.ledger.db, ticketID)
	if err != nil {
		return AnswerResult{}, err
	}
	if row.Status != statusWaitingReview {
		return h.late(ctx, row, approver)
	}
	_, users, found, err := approvers(ctx, h.ledger.db, row.WaitKey)
	if err != nil {
		return AnswerResult{}, err
	}
	if !found || !containsUser(users, approver) {
		return AnswerResult{TicketID: ticketID, Status: row.Status}, ErrNotApprover
	}
	input, err := json.Marshal(reviewInput{Approved: approved, Note: note, Approver: approver, AnsweredAt: h.now().Format(time.RFC3339)})
	if err != nil {
		return AnswerResult{}, err
	}
	return h.respondReview(ctx, row, approver, input)
}

// respondReview 回应审核中断。作答与超时并发时，后到者收到 ErrApprovalMismatch 或 ErrInterruptAnswered，按“已有结果”处理。
func (h *Host) respondReview(ctx context.Context, row ticketRow, approver string, input json.RawMessage) (AnswerResult, error) {
	late, err := h.resume(ctx, row, input)
	if late {
		return h.late(ctx, row, approver)
	}
	h.deliver(ctx)
	out, e := h.ticket(ctx, h.ledger.db, row.ID)
	return AnswerResult{TicketID: row.ID, Status: out.Status}, errors.Join(err, e)
}

// late 处理后到的作答：工单已结束（超时、其他审核人或本人先前作答）或并发时另一方先到。
// 作答人须在该工单最近一次审核的名单中；当前结果经发件箱告知他，幂等键为“工单 + 中断键 + 审核人”，
// 重复作答不重复发。approver 为空（超时等系统输入）时只返回结果，不发通知。
func (h *Host) late(ctx context.Context, row ticketRow, approver string) (AnswerResult, error) {
	key, users, found, err := lastReview(ctx, h.ledger.db, row.ID)
	if err != nil {
		return AnswerResult{}, err
	}
	if !found {
		return AnswerResult{TicketID: row.ID, Status: row.Status}, ErrNotWaitingReview
	}
	if approver != "" && !containsUser(users, approver) {
		return AnswerResult{TicketID: row.ID, Status: row.Status}, ErrNotApprover
	}
	if row.Status == statusRunning || row.Status == statusWaitingCustomer {
		readCtx, cancel := context.WithTimeout(ctx, h.cfg.LockTimeout)
		_, answered, e := durable.InterruptAnswer(readCtx, h.cfg.Store, key, reviewInterrupt)
		cancel()
		if e != nil {
			return AnswerResult{}, e
		}
		if !answered {
			return AnswerResult{TicketID: row.ID, Status: row.Status}, ErrReviewNotReady
		}
	}
	result, err := h.reviewResult(ctx, key)
	if err != nil {
		return AnswerResult{}, err
	}
	if approver != "" {
		err = h.write(ctx, func(tx *sql.Tx) error {
			return notify(ctx, tx, row.ID, support.User{ID: approver}, support.KindReviewResult, row.ID+"/"+key+"/review-result/"+approver,
				"该退款审核已有结果", "本次作答未生效。"+result)
		})
		h.deliver(ctx)
	}
	out, e := h.ticket(ctx, h.ledger.db, row.ID)
	return AnswerResult{TicketID: row.ID, Status: out.Status, Late: true, Result: result}, errors.Join(err, e)
}

// lastReview 读取工单最近一次审核中断的键与审核人名单；名单在首次观察到中断时写入，事件表记录其所属工单。
func lastReview(ctx context.Context, q rowQuerier, ticketID string) (key string, users []support.User, found bool, err error) {
	var data []byte
	err = q.QueryRowContext(ctx, `SELECT r.interrupt_key, r.approvers_json FROM review_approvers r JOIN events e ON e.event_key = r.interrupt_key
		WHERE e.ticket_id = ? ORDER BY r.rowid DESC LIMIT 1`, ticketID).Scan(&key, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &users)
	}
	return key, users, err == nil, err
}

// reviewResult 读取审核中断已固定的输入，描述谁、何时、批准还是拒绝；超时写“审核超时，按拒绝处理”。
func (h *Host) reviewResult(ctx context.Context, key string) (string, error) {
	readCtx, cancel := context.WithTimeout(ctx, h.cfg.LockTimeout)
	defer cancel()
	raw, answered, err := durable.InterruptAnswer(readCtx, h.cfg.Store, key, reviewInterrupt)
	if err != nil {
		return "", err
	}
	if !answered {
		return "该审核已结束，没有审核结论。", nil
	}
	var in reviewInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", err
	}
	switch {
	case in.Reason == "timeout":
		return fmt.Sprintf("审核超时，按拒绝处理（%s）。", in.AnsweredAt), nil
	case in.Approved:
		return fmt.Sprintf("%s 已于 %s 批准。", in.Approver, in.AnsweredAt), nil
	}
	return fmt.Sprintf("%s 已于 %s 拒绝：%s。", in.Approver, in.AnsweredAt, in.Note), nil
}

// reviewBody 是审核请求正文；争议原因可能来自 Agent 说明，去掉其末尾标点再接句号。
func reviewBody(info reviewInfo) string {
	reasons := strings.TrimRight(strings.Join(info.Reasons, "；"), "。.；; ")
	return fmt.Sprintf("订单 %s 申请退款 %d 分（%s），争议原因：%s。请在 %s 前作答。", info.OrderID, info.AmountCents, info.Currency, reasons, info.ReviewDeadline)
}

// bindModel 把目录绑定到首次打开时的模型配置。持久恢复会回放已记录的模型调用，换模型后续跑旧运行
// 只会得到 goagent 的“operation intent or attempt policy changed”，因此在打开时就以可读的错误拒绝。
func bindModel(dir string, cfg livemodel.Config) error {
	want := cfg.Provider + "/" + cfg.Model
	path := filepath.Join(dir, "model")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_, err = f.WriteString(want)
		return errors.Join(err, f.Close())
	}
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	got, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(got) != want {
		return fmt.Errorf("目录 %s 的工单以模型 %s 运行，本次为 %s；同一目录的 message、answer、sweep 须使用相同的 -live 与模型配置", dir, got, want)
	}
	return nil
}
