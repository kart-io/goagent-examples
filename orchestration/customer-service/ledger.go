package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/workflow/refund"
)

// ledger 是本地 SQLite 库：退款账本（实现 refund.Store）、争议判定与登记序号，以及宿主的工单表与发件箱。
// 它是模拟账本，不转移资金。单连接加即时写事务，跨进程也按库串行化，满足 refund.Store 按订单串行化的要求。
type ledger struct {
	db *sql.DB

	mu       sync.Mutex // 串行化 Transaction，保护 active 与 refunded
	active   *sql.Tx    // 当前 refund 事务，供 Authorizer 在同一事务内查询
	refunded bool       // 当前事务写入了退款

	beforeCommit func() error // 测试钩子：refund 事务提交前调用，返回错误时回滚
	afterRefund  func()       // 测试钩子：退款事务提交后调用
}

type order struct {
	refund.Order
	CustomerID string
}

func openLedger(dir string) (*ledger, error) {
	path, err := filepath.Abs(filepath.Join(dir, "customer-service.sqlite"))
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL&_txlock=immediate"}
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	l := &ledger{db: db}
	if err = l.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return l, nil
}

func (l *ledger) Close() error { return l.db.Close() }

const schemaVersion = 1

func (l *ledger) initialize() error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var version, tables int
	if err = tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == schemaVersion {
		return tx.Commit()
	}
	if err = tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table'").Scan(&tables); err != nil {
		return err
	}
	if version != 0 || tables != 0 {
		return errors.New("账本结构不兼容，请保留旧数据并换用新的演示目录")
	}
	_, err = tx.Exec(`
		CREATE TABLE orders (order_id TEXT PRIMARY KEY, customer_id TEXT NOT NULL, paid_cents INTEGER NOT NULL,
		 currency TEXT NOT NULL, delivery_days INTEGER NOT NULL, version INTEGER NOT NULL);
		INSERT INTO orders VALUES
		 ('A1001','c-1001',9900,'CNY',3,1),
		 ('A1002','c-1001',200000,'CNY',2,1),
		 ('A1003','c-1001',9900,'CNY',20,1),
		 ('A2001','c-2002',9900,'CNY',1,1);
		CREATE TABLE refund_requests (request_id TEXT PRIMARY KEY, order_id TEXT NOT NULL, state_json BLOB NOT NULL,
		 approved_by TEXT NOT NULL DEFAULT '', approved_at TEXT NOT NULL DEFAULT '');
		CREATE TABLE refunds (request_id TEXT PRIMARY KEY, execution_key TEXT NOT NULL, order_id TEXT NOT NULL,
		 amount_cents INTEGER NOT NULL CHECK(amount_cents > 0), currency TEXT NOT NULL, receipt_id TEXT NOT NULL UNIQUE);
		CREATE TABLE refund_verdicts (request_id TEXT PRIMARY KEY, verdict_json BLOB NOT NULL);
		CREATE TABLE refund_sequence (seq INTEGER PRIMARY KEY, request_id TEXT NOT NULL UNIQUE, order_id TEXT NOT NULL);
		CREATE TABLE review_approvers (interrupt_key TEXT PRIMARY KEY, request_id TEXT NOT NULL, approvers_json BLOB NOT NULL);
		CREATE TABLE tickets (ticket_id TEXT PRIMARY KEY, customer_id TEXT NOT NULL, submitter TEXT NOT NULL,
		 round INTEGER NOT NULL, run_id TEXT NOT NULL, input TEXT NOT NULL, status TEXT NOT NULL,
		 wait_key TEXT NOT NULL DEFAULT '', wait_id TEXT NOT NULL DEFAULT '', deadline TEXT NOT NULL DEFAULT '',
		 last_progress TEXT NOT NULL, failures INTEGER NOT NULL DEFAULT 0);
		CREATE INDEX tickets_customer ON tickets(customer_id, status);
		CREATE TABLE ticket_messages (message_id TEXT PRIMARY KEY, ticket_id TEXT NOT NULL, seq INTEGER NOT NULL,
		 text TEXT NOT NULL, handled INTEGER NOT NULL DEFAULT 0);
		CREATE TABLE outbox (idempotency_key TEXT PRIMARY KEY, ticket_id TEXT NOT NULL, channel TEXT NOT NULL,
		 recipient_json BLOB NOT NULL, payload_json BLOB NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
		 attempts INTEGER NOT NULL DEFAULT 0, seq INTEGER NOT NULL);
		CREATE TABLE events (event_key TEXT PRIMARY KEY, ticket_id TEXT NOT NULL);
		PRAGMA user_version = 1;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Transaction 实现 refund.Store：回调出错回滚，成功后提交。事务期间 active 指向它，供 Authorizer 读取。
func (l *ledger) Transaction(ctx context.Context, f func(refund.Transaction) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	l.active, l.refunded = tx, false
	defer func() { l.active = nil }()
	if err = f(ledgerTx{Tx: tx, l: l}); err != nil {
		return err
	}
	if l.beforeCommit != nil {
		if err = l.beforeCommit(); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if l.refunded && l.afterRefund != nil {
		l.afterRefund()
	}
	return nil
}

// activeTx 返回当前 refund 事务；只在 Transaction 的回调内（含其中调用的 Authorizer）有效。
func (l *ledger) activeTx() *sql.Tx { return l.active }

type ledgerTx struct {
	*sql.Tx
	l *ledger
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return agentErrors.ErrRefundNotFound
	}
	return err
}

func readOrder(ctx context.Context, q rowQuerier, id string) (order, error) {
	var o order
	err := q.QueryRowContext(ctx, "SELECT customer_id, paid_cents, currency, delivery_days, version FROM orders WHERE order_id = ?", id).
		Scan(&o.CustomerID, &o.PaidCents, &o.Currency, &o.DeliveryDays, &o.Version)
	return o, notFound(err)
}

func (t ledgerTx) Order(ctx context.Context, id string) (refund.Order, error) {
	o, err := readOrder(ctx, t.Tx, id)
	return o.Order, err
}

func (t ledgerTx) Request(ctx context.Context, id string) (refund.Request, string, error) {
	var data []byte
	var actor string
	var req refund.Request
	err := t.QueryRowContext(ctx, "SELECT state_json, approved_by FROM refund_requests WHERE request_id = ?", id).Scan(&data, &actor)
	if err == nil {
		err = json.Unmarshal(data, &req)
	}
	return req, actor, notFound(err)
}

func (t ledgerTx) Receipt(ctx context.Context, id string) (string, error) {
	var receipt string
	err := t.QueryRowContext(ctx, "SELECT receipt_id FROM refunds WHERE request_id = ?", id).Scan(&receipt)
	return receipt, notFound(err)
}

func (t ledgerTx) TotalRefunded(ctx context.Context, orderID string) (int64, error) {
	var amount int64
	err := t.QueryRowContext(ctx, "SELECT COALESCE(SUM(amount_cents),0) FROM refunds WHERE order_id = ?", orderID).Scan(&amount)
	return amount, err
}

func (t ledgerTx) InsertRequest(ctx context.Context, req refund.Request) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = t.ExecContext(ctx, "INSERT INTO refund_requests(request_id, order_id, state_json) VALUES (?,?,?)", req.RequestID, req.OrderID, data)
	return err
}

// Approve 对同一申请与主体幂等；不同主体再次批准时报错。
func (t ledgerTx) Approve(ctx context.Context, id, actor string, at time.Time) error {
	res, err := t.ExecContext(ctx, "UPDATE refund_requests SET approved_by = ?, approved_at = ? WHERE request_id = ? AND approved_by IN ('', ?)",
		actor, at.Format(time.RFC3339Nano), id, actor)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("申请 %s 已由其他主体批准", id)
	}
	return nil
}

func (t ledgerTx) InsertRefund(ctx context.Context, key string, req refund.Request, receipt string) error {
	_, err := t.ExecContext(ctx, "INSERT INTO refunds VALUES (?,?,?,?,?,?)", req.RequestID, key, req.OrderID, req.AmountCents, req.Currency, receipt)
	if err == nil {
		t.l.refunded = true
	}
	return err
}

// registerVerdict 在登记事务内分配单调递增的登记序号，按该订单此前的登记数与当前事实计算判定并写入。
func (t ledgerTx) registerVerdict(ctx context.Context, req refund.Request, judge func(support.DisputeFacts) support.Verdict) (support.Verdict, error) {
	var prior int
	if err := t.QueryRowContext(ctx, "SELECT count(*) FROM refund_sequence WHERE order_id = ?", req.OrderID).Scan(&prior); err != nil {
		return support.Verdict{}, err
	}
	refunded, err := t.TotalRefunded(ctx, req.OrderID)
	if err != nil {
		return support.Verdict{}, err
	}
	if _, err = t.ExecContext(ctx, "INSERT INTO refund_sequence(seq, request_id, order_id) VALUES ((SELECT COALESCE(MAX(seq),0)+1 FROM refund_sequence),?,?)", req.RequestID, req.OrderID); err != nil {
		return support.Verdict{}, err
	}
	v := judge(support.DisputeFacts{AmountCents: req.AmountCents, PaidCents: req.PaidCents, RefundedCents: refunded, DeliveryDays: req.DeliveryDays, PriorRequests: prior})
	data, err := json.Marshal(v)
	if err != nil {
		return support.Verdict{}, err
	}
	_, err = t.ExecContext(ctx, "INSERT INTO refund_verdicts(request_id, verdict_json) VALUES (?,?)", req.RequestID, data)
	return v, err
}

// order 读取订单（事务外）。
func (l *ledger) order(ctx context.Context, id string) (order, error) {
	return readOrder(ctx, l.db, id)
}

// verdict 读取已固定的判定与登记序号；found 为 false 表示缺少任一记录。
func (l *ledger) verdict(ctx context.Context, requestID string) (v support.Verdict, seq int64, found bool, err error) {
	var data []byte
	err = l.db.QueryRowContext(ctx, `SELECT v.verdict_json, s.seq FROM refund_verdicts v JOIN refund_sequence s ON s.request_id = v.request_id
		WHERE v.request_id = ?`, requestID).Scan(&data, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return v, 0, false, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &v)
	}
	return v, seq, err == nil, err
}

// request 读取已登记的申请（事务外）。
func (l *ledger) request(ctx context.Context, id string) (refund.Request, bool, error) {
	var data []byte
	err := l.db.QueryRowContext(ctx, "SELECT state_json FROM refund_requests WHERE request_id = ?", id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return refund.Request{}, false, nil
	}
	var req refund.Request
	if err == nil {
		err = json.Unmarshal(data, &req)
	}
	return req, err == nil, err
}

// receipt 读取已提交的退款回执（事务外）。
func (l *ledger) receipt(ctx context.Context, id string) (string, bool, error) {
	var receipt string
	err := l.db.QueryRowContext(ctx, "SELECT receipt_id FROM refunds WHERE request_id = ?", id).Scan(&receipt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return receipt, err == nil, err
}

// refunds 返回该订单的退款笔数与累计金额。
func (l *ledger) refunds(ctx context.Context, orderID string) (count int, total int64, err error) {
	err = l.db.QueryRowContext(ctx, "SELECT count(*), COALESCE(SUM(amount_cents),0) FROM refunds WHERE order_id = ?", orderID).Scan(&count, &total)
	return
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// saveApprovers 在首次观察到审核中断时写入审核人名单；已存在时保留原名单。
func saveApprovers(ctx context.Context, q execer, interruptKey, requestID string, users []support.User) error {
	data, err := json.Marshal(users)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, "INSERT OR IGNORE INTO review_approvers VALUES (?,?,?)", interruptKey, requestID, data)
	return err
}

// approvers 读取审核中断的审核人名单。
func approvers(ctx context.Context, q rowQuerier, interruptKey string) (requestID string, users []support.User, found bool, err error) {
	var data []byte
	err = q.QueryRowContext(ctx, "SELECT request_id, approvers_json FROM review_approvers WHERE interrupt_key = ?", interruptKey).Scan(&requestID, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &users)
	}
	return requestID, users, err == nil, err
}

func containsUser(users []support.User, id string) bool {
	return slices.ContainsFunc(users, func(u support.User) bool { return u.ID == id })
}
