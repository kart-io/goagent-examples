package support

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNoApprover 表示解析结果为空且没有可用的兜底审核人；宿主告警运营并在下次扫描时重试。
var ErrNoApprover = errors.New("没有可用的审核人")

// 审核人策略。
const (
	StrategySubmitterLeader = "submitter_leader"
	StrategyGroup           = "group"
	StrategyFixed           = "fixed"
)

// Strategy 是一种审核人来源：提交人的上级、用户组成员或固定用户。
type Strategy struct {
	Kind    string   `json:"kind"`
	GroupID string   `json:"group_id,omitempty"`
	UserIDs []string `json:"user_ids,omitempty"`
}

// ApproverRule 在退款金额不低于 MinAmountCents 时生效，多条规则的结果合并去重。
type ApproverRule struct {
	MinAmountCents int64      `json:"min_amount_cents"`
	Strategies     []Strategy `json:"strategies"`
}

// ApproverConfig 是审核人解析配置。
type ApproverConfig struct {
	Rules           []ApproverRule `json:"rules"`
	FallbackUserIDs []string       `json:"fallback_user_ids"`
}

// DefaultApproverConfig 是演示配置：所有争议退款交提交人的上级，5 万分及以上再加财务组。
const DefaultApproverConfig = `{
  "approvers": {
    "rules": [
      {"min_amount_cents": 0,     "strategies": [{"kind": "submitter_leader"}]},
      {"min_amount_cents": 50000, "strategies": [{"kind": "group", "group_id": "finance-reviewers"}]}
    ],
    "fallback_user_ids": ["ou_fallback"]
  }
}`

// ParseApproverConfig 严格解析 {"approvers": {...}}，拒绝未知字段与无效策略。
func ParseApproverConfig(data []byte) (ApproverConfig, error) {
	var doc struct {
		Approvers *ApproverConfig `json:"approvers"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return ApproverConfig{}, fmt.Errorf("解析审核人配置: %w", err)
	}
	if doc.Approvers == nil {
		return ApproverConfig{}, errors.New("审核人配置缺少 approvers")
	}
	for i, r := range doc.Approvers.Rules {
		if r.MinAmountCents < 0 || len(r.Strategies) == 0 {
			return ApproverConfig{}, fmt.Errorf("第 %d 条审核规则需要非负金额与至少一个策略", i+1)
		}
		for _, s := range r.Strategies {
			switch {
			case s.Kind == StrategySubmitterLeader:
			case s.Kind == StrategyGroup && s.GroupID != "":
			case s.Kind == StrategyFixed && len(s.UserIDs) > 0:
			default:
				return ApproverConfig{}, fmt.Errorf("第 %d 条审核规则的策略 %q 无效或缺少参数", i+1, s.Kind)
			}
		}
	}
	return *doc.Approvers, nil
}

// RuleResolver 按配置依次解析、去重并排除提交人；结果为空时使用兜底审核人（同样排除提交人）。
type RuleResolver struct {
	Directory Directory
	Config    ApproverConfig
}

func (r RuleResolver) Resolve(ctx context.Context, req ApprovalRequest) ([]User, error) {
	var out []User
	seen := map[string]bool{}
	add := func(u User) {
		if u.ID == "" || u.ID == req.Submitter || seen[u.ID] {
			return
		}
		seen[u.ID] = true
		out = append(out, u)
	}
	for _, rule := range r.Config.Rules {
		if req.AmountCents < rule.MinAmountCents {
			continue
		}
		for _, s := range rule.Strategies {
			switch s.Kind {
			case StrategySubmitterLeader:
				if req.Submitter == "" {
					continue
				}
				leader, found, err := r.Directory.Leader(ctx, req.Submitter)
				if err != nil {
					return nil, fmt.Errorf("查询 %s 的上级: %w", req.Submitter, err)
				}
				if found {
					add(leader)
				}
			case StrategyGroup:
				members, err := r.Directory.GroupMembers(ctx, s.GroupID)
				if err != nil {
					return nil, fmt.Errorf("查询用户组 %s: %w", s.GroupID, err)
				}
				for _, m := range members {
					add(m)
				}
			case StrategyFixed:
				for _, id := range s.UserIDs {
					add(User{ID: id})
				}
			default:
				return nil, fmt.Errorf("未知的审核人策略 %q", s.Kind)
			}
		}
	}
	if len(out) == 0 {
		for _, id := range r.Config.FallbackUserIDs {
			add(User{ID: id})
		}
	}
	if len(out) == 0 {
		return nil, ErrNoApprover
	}
	return out, nil
}
