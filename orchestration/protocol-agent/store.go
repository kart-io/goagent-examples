package main

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kart-io/goagent/codec/json"
	agentErrors "github.com/kart-io/goagent/errors"
	mg "github.com/kart-io/goagent/management"
)

type docKey struct{ kind, tenant, id string }
type documents map[docKey]mg.Document

// 串行事务和深拷贝保证失败写入不可见，读取结果不会泄漏可变引用。
// demoStore 仅用于单进程演示，退出即丢失；不作为生产持久存储。
// 存储合约参考 goagent/management/internal/managementtest，保留事务、CAS及可见范围。
type demoStore struct {
	mu   sync.Mutex
	docs documents
}
type transaction struct{ docs documents }

func newDemoStore() *demoStore { return &demoStore{docs: documents{}} }

func clone(d mg.Document) mg.Document { d.Data = slices.Clone(d.Data); return d }
func key(d mg.Document) docKey        { return docKey{d.Kind, d.TenantID, d.ID} }

func validKind(kind string) error {
	if !slices.Contains(mg.Kinds, kind) {
		return agentErrors.ErrManagementInvalid
	}
	return nil
}
func validDocument(d mg.Document) error {
	if err := validKind(d.Kind); err != nil {
		return err
	}
	var m mg.Meta
	if json.Unmarshal(d.Data, &m) != nil || d.Revision < 1 || m.ID != d.ID || m.TenantID != d.TenantID || m.Revision != d.Revision {
		return agentErrors.ErrManagementInvalid
	}
	return nil
}

func (s *demoStore) WithinTx(ctx context.Context, fn func(mg.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	copy := documents{}
	for k, d := range s.docs {
		copy[k] = clone(d)
	}
	if err := fn(&transaction{copy}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.docs = copy
	return nil
}
func (s *demoStore) Get(ctx context.Context, kind, tenant, id string) (mg.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return (&transaction{s.docs}).Get(ctx, kind, tenant, id)
}
func (s *demoStore) List(ctx context.Context, kind string, selection mg.Selection) ([]mg.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return (&transaction{s.docs}).List(ctx, kind, selection)
}
func (tx *transaction) Get(ctx context.Context, kind, tenant, id string) (mg.Document, error) {
	if err := validKind(kind); err != nil {
		return mg.Document{}, err
	}
	if err := ctx.Err(); err != nil {
		return mg.Document{}, err
	}
	d, ok := tx.docs[docKey{kind, tenant, id}]
	if !ok {
		return mg.Document{}, agentErrors.ErrManagementNotFound
	}
	return clone(d), nil
}
func (tx *transaction) Insert(ctx context.Context, d mg.Document) error {
	if err := validDocument(d); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := tx.docs[key(d)]; ok {
		return agentErrors.ErrManagementConflict
	}
	tx.docs[key(d)] = clone(d)
	return nil
}
func (tx *transaction) CAS(ctx context.Context, d mg.Document, expected int64) error {
	if err := validDocument(d); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	old, ok := tx.docs[key(d)]
	if !ok || old.Revision != expected {
		return agentErrors.ErrManagementConflict
	}
	d.CreatedAt = old.CreatedAt
	tx.docs[key(d)] = clone(d)
	return nil
}
func (tx *transaction) Delete(ctx context.Context, kind, tenant, id string, expected int64) error {
	if err := validKind(kind); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	k := docKey{kind, tenant, id}
	old, ok := tx.docs[k]
	if !ok || old.Revision != expected {
		return agentErrors.ErrManagementConflict
	}
	delete(tx.docs, k)
	return nil
}
func (tx *transaction) List(ctx context.Context, kind string, selection mg.Selection) ([]mg.Document, error) {
	if err := validKind(kind); err != nil {
		return nil, err
	}
	if selection.TenantID == "" && !selection.Visibility.All {
		return nil, agentErrors.ErrManagementForbidden
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q, v := selection.Query, selection.Visibility
	if q.Limit > 10001 {
		return nil, agentErrors.ErrManagementInvalid
	}
	if q.Limit <= 0 {
		q.Limit = 201
	}
	var cursorTime time.Time
	var cursorID string
	if q.Cursor != "" {
		parts := strings.SplitN(q.Cursor, "|", 2)
		if len(parts) != 2 {
			return nil, agentErrors.ErrManagementInvalid
		}
		var err error
		cursorTime, err = time.Parse(time.RFC3339Nano, parts[0])
		if err != nil {
			return nil, agentErrors.ErrManagementInvalid
		}
		cursorID = parts[1]
	}
	out := []mg.Document{}
	for _, d := range tx.docs {
		if d.Kind != kind || (selection.TenantID != "" && d.TenantID != selection.TenantID) {
			continue
		}
		var fields struct {
			AgentID   string `json:"agent_id"`
			SessionID string `json:"session_id"`
			TaskID    string `json:"task_id"`
			State     string `json:"state"`
			Status    string `json:"status"`
		}
		if err := json.Unmarshal(d.Data, &fields); err != nil {
			return nil, err
		}
		if q.AgentID != "" && fields.AgentID != q.AgentID || q.SessionID != "" && fields.SessionID != q.SessionID || q.TaskID != "" && fields.TaskID != q.TaskID {
			continue
		}
		state := fields.State
		if state == "" {
			state = fields.Status
		}
		if len(q.Statuses) > 0 && !slices.Contains(q.Statuses, state) {
			continue
		}
		if !q.CreatedFrom.IsZero() && d.CreatedAt.Before(q.CreatedFrom) || !q.CreatedTo.IsZero() && d.CreatedAt.After(q.CreatedTo) {
			continue
		}
		// 自身 ID 只用于可见范围；查询字段仍按 JSON 中的父资源关系匹配。
		if kind == "agents" {
			fields.AgentID = d.ID
		}
		if kind == "sessions" {
			fields.SessionID = d.ID
		}
		if !v.All && !slices.Contains(v.ResourceIDs, d.ID) && !slices.Contains(v.AgentIDs, fields.AgentID) && !slices.Contains(v.SessionIDs, fields.SessionID) {
			continue
		}
		if q.Cursor != "" && (d.CreatedAt.Before(cursorTime) || d.CreatedAt.Equal(cursorTime) && d.ID <= cursorID) {
			continue
		}
		out = append(out, clone(d))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

var _ mg.Store = (*demoStore)(nil)
