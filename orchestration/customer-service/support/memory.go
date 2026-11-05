package support

import (
	"context"
	"slices"
	"sync"
)

// MemoryDirectory 是内存组织架构，Err 非空时所有查询返回它，用于演示目录故障。
type MemoryDirectory struct {
	mu      sync.Mutex
	leaders map[string]User
	groups  map[string][]User
	err     error
}

func NewMemoryDirectory() *MemoryDirectory {
	return &MemoryDirectory{leaders: map[string]User{}, groups: map[string][]User{}}
}

// SetLeader 设置 userID 的上级。
func (d *MemoryDirectory) SetLeader(userID string, leader User) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.leaders[userID] = leader
}

// SetGroup 设置用户组成员。
func (d *MemoryDirectory) SetGroup(groupID string, members ...User) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.groups[groupID] = slices.Clone(members)
}

// Fail 让之后的查询返回 err；传 nil 恢复。
func (d *MemoryDirectory) Fail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.err = err
}

func (d *MemoryDirectory) Leader(_ context.Context, userID string) (User, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return User{}, false, d.err
	}
	u, ok := d.leaders[userID]
	return u, ok, nil
}

func (d *MemoryDirectory) GroupMembers(_ context.Context, groupID string) ([]User, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	return slices.Clone(d.groups[groupID]), nil
}

// Delivery 是一次投递记录。
type Delivery struct {
	To      User
	Message Message
}

// RecordingNotifier 记录每次投递（含重复投递），测试据此断言消息只发一次。
type RecordingNotifier struct {
	mu   sync.Mutex
	sent []Delivery
	err  error
}

// Fail 让之后的投递返回 err；传 nil 恢复。
func (n *RecordingNotifier) Fail(err error) { n.mu.Lock(); defer n.mu.Unlock(); n.err = err }

func (n *RecordingNotifier) Notify(_ context.Context, to User, m Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.err != nil {
		return n.err
	}
	n.sent = append(n.sent, Delivery{To: to, Message: m})
	return nil
}

// Sent 返回 kind 类型的投递；kind 为空时返回全部。
func (n *RecordingNotifier) Sent(kind MessageKind) []Delivery {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []Delivery
	for _, d := range n.sent {
		if kind == "" || d.Message.Kind == kind {
			out = append(out, d)
		}
	}
	return out
}

// Transfer 是一次转人工记录。
type Transfer struct {
	Key     string
	Context HandoffContext
}

// RecordingHandoff 记录每次转人工（含重复调用）。
type RecordingHandoff struct {
	mu        sync.Mutex
	transfers []Transfer
}

func (h *RecordingHandoff) Transfer(_ context.Context, key string, c HandoffContext) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.transfers = append(h.transfers, Transfer{Key: key, Context: c})
	return nil
}

// Transfers 返回全部转人工记录。
func (h *RecordingHandoff) Transfers() []Transfer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.transfers)
}
