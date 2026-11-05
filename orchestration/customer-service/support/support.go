// Package support 定义客服案例的业务接口与可替换实现：争议规则、组织架构、审核人解析、通知、转人工与时钟。
// 这些能力属于业务方，不进入 GoAgent 内核；默认实现全部在内存中，飞书实现见 support/feishu。
package support

import (
	"context"
	"sync"
	"time"
)

// User 是组织架构中的一个人，ID 为飞书 open_id 或演示账号。
type User struct{ ID, Name string }

// Directory 是组织架构查询。found 为 false 表示没有上级。
type Directory interface {
	Leader(ctx context.Context, userID string) (leader User, found bool, err error)
	GroupMembers(ctx context.Context, groupID string) ([]User, error)
}

// ApprovalRequest 是解析审核人的输入，Submitter 为提交工单的客服账号。
type ApprovalRequest struct {
	RequestID, Submitter string
	AmountCents          int64
	Verdict              Verdict
}

// ApproverResolver 解析一笔有争议退款的审核人；返回空列表表示没有可用审核人。
type ApproverResolver interface {
	Resolve(ctx context.Context, req ApprovalRequest) ([]User, error)
}

// MessageKind 区分发件箱中的消息用途。
type MessageKind string

const (
	KindApprovalRequest  MessageKind = "approval_request"
	KindReminder         MessageKind = "reminder"
	KindCustomerReply    MessageKind = "customer_reply"
	KindCustomerQuestion MessageKind = "customer_question"
	KindProgress         MessageKind = "progress"
	KindHandoffNotice    MessageKind = "handoff_notice"
	KindUsageAlert       MessageKind = "usage_alert"
	KindOpsAlert         MessageKind = "ops_alert"
	KindReviewResult     MessageKind = "review_result" // 告知后到的审核人当前结果
)

// Message 是一条待投递的消息。IdempotencyKey 是发件箱键，接收方据此去重。
type Message struct {
	Kind           MessageKind
	IdempotencyKey string
	Title, Body    string
}

// Notifier 投递消息；投递为至少一次，实现应按 IdempotencyKey 去重。
type Notifier interface {
	Notify(ctx context.Context, to User, m Message) error
}

// Solution 是已给出或可选的处理方案。
type Solution struct {
	Title, Detail    string
	RequiresApproval bool
}

// HandoffReason 说明转人工的原因。
type HandoffReason string

const (
	ReasonInvalidRoute HandoffReason = "invalid_route"
	ReasonMaxRounds    HandoffReason = "max_rounds"
	ReasonMaxRequests  HandoffReason = "max_requests"
	ReasonRunFailed    HandoffReason = "run_failed"
)

// HandoffContext 是交给人工客服的结构化上下文。
type HandoffContext struct {
	TicketID, CustomerID string
	Intent               string
	Solutions            []Solution
	Reason               HandoffReason
	Summary              string
}

// Handoff 把工单转给人工；渠道取决于 IM 系统，实现应按 idempotencyKey 去重。
type Handoff interface {
	Transfer(ctx context.Context, idempotencyKey string, h HandoffContext) error
}

// Clock 是宿主时钟；登记时间、截止时间与扫描都取自它，测试用 ManualClock 推进。
type Clock interface{ Now() time.Time }

// SystemClock 返回当前 UTC 时间。
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// ManualClock 是可推进的测试时钟，并发安全。
type ManualClock struct {
	mu  sync.Mutex
	now time.Time
}

func NewManualClock(now time.Time) *ManualClock { return &ManualClock{now: now.UTC()} }

func (c *ManualClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

// Advance 把时钟向前推进 d。
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set 把时钟设为 t。
func (c *ManualClock) Set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.now = t.UTC() }
