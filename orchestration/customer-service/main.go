// Command customer-service 演示客服编排第一期：分流、争议退款人工审核、追问、超时与转人工。
// 默认离线运行（脚本化模型、内存组织架构、打印型通知），退款写入本地模拟账本，不转移资金。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
	"github.com/kart-io/goagent-examples/orchestration/customer-service/support/feishu"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := execute(ctx, os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, livemodel.Redact(err.Error()))
		os.Exit(1)
	}
}

const usage = `用法：customer-service <message|answer|sweep|show> -dir 目录 [选项]
  message -customer 客户ID -text 内容 [-id 消息ID] [-submitter 客服账号]
  answer  -ticket 工单ID -approver 审核人 (-approve | -reject) [-note 说明]
  sweep   执行一次扫描（-now 指定时间可演示超时）
  show    -ticket 工单ID`

// printer 把通知与转人工打印为 JSON 行，代替真实 IM 渠道。
type printer struct{ w io.Writer }

func (p printer) Notify(_ context.Context, to support.User, m support.Message) error {
	return json.NewEncoder(p.w).Encode(map[string]string{"event": "notify", "to": to.ID, "kind": string(m.Kind), "key": m.IdempotencyKey, "title": m.Title, "body": m.Body})
}

func (p printer) Transfer(_ context.Context, key string, h support.HandoffContext) error {
	return json.NewEncoder(p.w).Encode(map[string]any{"event": "handoff", "key": key, "context": h})
}

// demoDirectory 是演示组织架构：客服 agent-1 的上级为 lead-1，财务审核组为 fin-1、fin-2。
func demoDirectory() *support.MemoryDirectory {
	d := support.NewMemoryDirectory()
	d.SetLeader("agent-1", support.User{ID: "lead-1", Name: "客服组长"})
	d.SetGroup("finance-reviewers", support.User{ID: "fin-1", Name: "财务一"}, support.User{ID: "fin-2", Name: "财务二"})
	return d
}

func execute(ctx context.Context, args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	cmd := args[0]
	flags := flag.NewFlagSet("customer-service "+cmd, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("dir", "", "持久目录：账本、工单、发件箱、检查点与预算")
	at := flags.String("now", "", "宿主时钟（RFC3339），默认当前时间")
	live := flags.Bool("live", false, "显式启用真实模型（会产生费用）；业务仍为模拟")
	approverFile := flags.String("approvers", "", "审核人配置 JSON 文件，默认使用内置演示配置")
	customer := flags.String("customer", "", "客户 ID")
	text := flags.String("text", "", "客户消息")
	messageID := flags.String("id", "", "消息 ID，默认由时间生成；重复投递同一 ID 只处理一次")
	submitter := flags.String("submitter", "agent-1", "提交工单的客服账号")
	ticket := flags.String("ticket", "", "工单 ID")
	approver := flags.String("approver", "", "作答人；演示中视为已认证，不是真实认证")
	approve := flags.Bool("approve", false, "批准")
	reject := flags.Bool("reject", false, "拒绝")
	note := flags.String("note", "", "审核说明")
	if err := flags.Parse(args[1:]); err != nil {
		return fmt.Errorf("%w\n%s", err, usage)
	}
	if *dir == "" || flags.NArg() > 0 {
		return errors.New(usage)
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	cfg := HostConfig{Dir: *dir, Notifier: printer{w}, Handoff: printer{w}, Operator: support.User{ID: "ops-1"}}
	if *at != "" {
		now, err := time.Parse(time.RFC3339, *at)
		if err != nil {
			return fmt.Errorf("-now 需要 RFC3339 时间: %w", err)
		}
		cfg.Clock = support.NewManualClock(now)
	}
	if *live {
		client, err := livemodel.FromEnv()
		if err != nil {
			return err
		}
		cfg.Client, cfg.LockTimeout = client, 10*time.Minute
	}
	rawApprovers := []byte(support.DefaultApproverConfig)
	if *approverFile != "" {
		data, err := os.ReadFile(*approverFile)
		if err != nil {
			return err
		}
		rawApprovers = data
	}
	approverCfg, err := support.ParseApproverConfig(rawApprovers)
	if err != nil {
		return err
	}
	var directory support.Directory = demoDirectory()
	fs, err := feishu.FromEnv()
	if err != nil {
		return err
	}
	if fs != nil {
		directory = fs
	}
	cfg.Resolver = support.RuleResolver{Directory: directory, Config: approverCfg}
	h, err := OpenHost(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	var out any
	switch cmd {
	case "message":
		if *customer == "" || *text == "" {
			return errors.New("message 需要 -customer 与 -text")
		}
		if *messageID == "" {
			*messageID = fmt.Sprintf("m%d", h.now().UnixNano())
		}
		out, err = h.Message(ctx, *customer, *submitter, *messageID, *text)
	case "answer":
		if *ticket == "" || *approver == "" || *approve == *reject {
			return errors.New("answer 需要 -ticket、-approver，以及 -approve 或 -reject 之一")
		}
		out, err = h.Answer(ctx, *ticket, *approver, *approve, *note)
	case "sweep":
		out, err = h.Sweep(ctx)
	case "show":
		if *ticket == "" {
			return errors.New("show 需要 -ticket")
		}
		out, err = h.Show(ctx, *ticket)
	default:
		return fmt.Errorf("未知命令 %q\n%s", cmd, usage)
	}
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// TicketView 是 show 的输出：工单状态、等待的中断键、审核人与发件箱。
type TicketView struct {
	TicketID   string         `json:"ticket_id"`
	CustomerID string         `json:"customer_id"`
	Status     string         `json:"status"`
	RunID      string         `json:"run_id"`
	Round      int            `json:"round"`
	WaitID     string         `json:"wait_id,omitempty"`
	WaitKey    string         `json:"wait_key,omitempty"`
	Deadline   string         `json:"deadline,omitempty"`
	Approvers  []support.User `json:"approvers,omitempty"`
	Outbox     []OutboxView   `json:"outbox"`
}

// OutboxView 是一条发件箱记录。
type OutboxView struct {
	Key      string `json:"key"`
	Channel  string `json:"channel"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
}

// Show 读取工单状态；只读，不推进运行。
func (h *Host) Show(ctx context.Context, ticketID string) (TicketView, error) {
	row, err := h.ticket(ctx, h.ledger.db, ticketID)
	if err != nil {
		return TicketView{}, err
	}
	v := TicketView{TicketID: row.ID, CustomerID: row.CustomerID, Status: row.Status, RunID: row.RunID, Round: row.Round, WaitID: row.WaitID, WaitKey: row.WaitKey, Deadline: row.Deadline}
	if row.WaitKey != "" {
		if _, v.Approvers, _, err = approvers(ctx, h.ledger.db, row.WaitKey); err != nil {
			return TicketView{}, err
		}
	}
	rows, err := h.ledger.db.QueryContext(ctx, "SELECT idempotency_key, channel, status, attempts FROM outbox WHERE ticket_id = ? ORDER BY seq", ticketID)
	if err != nil {
		return TicketView{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var o OutboxView
		if err := rows.Scan(&o.Key, &o.Channel, &o.Status, &o.Attempts); err != nil {
			return TicketView{}, err
		}
		v.Outbox = append(v.Outbox, o)
	}
	return v, rows.Err()
}
