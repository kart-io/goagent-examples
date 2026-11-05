package support

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func ids(users []User) []string {
	out := make([]string, len(users))
	for i, u := range users {
		out[i] = u.ID
	}
	return out
}

func demoResolver(t *testing.T) (RuleResolver, *MemoryDirectory) {
	t.Helper()
	cfg, err := ParseApproverConfig([]byte(DefaultApproverConfig))
	require.NoError(t, err)
	d := NewMemoryDirectory()
	d.SetLeader("agent-1", User{ID: "lead-1"})
	d.SetGroup("finance-reviewers", User{ID: "fin-1"}, User{ID: "lead-1"}, User{ID: "agent-1"})
	return RuleResolver{Directory: d, Config: cfg}, d
}

func TestResolveCombinesAndDeduplicates(t *testing.T) {
	r, _ := demoResolver(t)
	ctx := context.Background()
	small, err := r.Resolve(ctx, ApprovalRequest{Submitter: "agent-1", AmountCents: 3000})
	require.NoError(t, err)
	require.Equal(t, []string{"lead-1"}, ids(small), "低于财务阈值时只有上级")

	large, err := r.Resolve(ctx, ApprovalRequest{Submitter: "agent-1", AmountCents: 80000})
	require.NoError(t, err)
	require.Equal(t, []string{"lead-1", "fin-1"}, ids(large), "上级与财务组合并去重，并排除提交人本人")
}

func TestResolveSkipsMissingLeaderOnly(t *testing.T) {
	r, _ := demoResolver(t)
	ctx := context.Background()
	got, err := r.Resolve(ctx, ApprovalRequest{Submitter: "agent-2", AmountCents: 80000})
	require.NoError(t, err)
	require.Equal(t, []string{"fin-1", "lead-1", "agent-1"}, ids(got), "没有上级时只跳过 submitter_leader；agent-1 不是提交人，不被排除")

	got, err = r.Resolve(ctx, ApprovalRequest{Submitter: "", AmountCents: 3000})
	require.NoError(t, err)
	require.Equal(t, []string{"ou_fallback"}, ids(got), "提交人为空且无其他策略时用兜底人")
}

func TestResolveSelfApprovalUsesFallback(t *testing.T) {
	cfg := ApproverConfig{
		Rules:           []ApproverRule{{Strategies: []Strategy{{Kind: StrategyGroup, GroupID: "finance-reviewers"}}}},
		FallbackUserIDs: []string{"fin-1", "ou_fallback"},
	}
	d := NewMemoryDirectory()
	d.SetGroup("finance-reviewers", User{ID: "fin-1"})
	got, err := RuleResolver{Directory: d, Config: cfg}.Resolve(context.Background(), ApprovalRequest{Submitter: "fin-1", AmountCents: 1})
	require.NoError(t, err)
	require.Equal(t, []string{"ou_fallback"}, ids(got), "提交人在财务组中时被排除，排除后为空用兜底人，兜底人同样排除提交人")
}

func TestResolveFailures(t *testing.T) {
	r, d := demoResolver(t)
	ctx := context.Background()
	d.Fail(errors.New("目录超时"))
	_, err := r.Resolve(ctx, ApprovalRequest{Submitter: "agent-1", AmountCents: 3000})
	require.ErrorContains(t, err, "目录超时")

	empty := RuleResolver{Directory: NewMemoryDirectory(), Config: ApproverConfig{Rules: []ApproverRule{{Strategies: []Strategy{{Kind: StrategySubmitterLeader}}}}}}
	_, err = empty.Resolve(ctx, ApprovalRequest{Submitter: "agent-9", AmountCents: 3000})
	require.ErrorIs(t, err, ErrNoApprover)
}

func TestParseApproverConfigRejectsInvalid(t *testing.T) {
	for name, doc := range map[string]string{
		"缺少 approvers": `{}`,
		"未知字段":         `{"approvers":{"rules":[],"extra":1}}`,
		"组缺少 ID":       `{"approvers":{"rules":[{"min_amount_cents":0,"strategies":[{"kind":"group"}]}]}}`,
		"固定缺少用户":       `{"approvers":{"rules":[{"min_amount_cents":0,"strategies":[{"kind":"fixed"}]}]}}`,
		"未知策略":         `{"approvers":{"rules":[{"min_amount_cents":0,"strategies":[{"kind":"ceo"}]}]}}`,
		"负金额":          `{"approvers":{"rules":[{"min_amount_cents":-1,"strategies":[{"kind":"submitter_leader"}]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseApproverConfig([]byte(doc))
			require.Error(t, err)
		})
	}
	cfg, err := ParseApproverConfig([]byte(`{"approvers":{"rules":[{"min_amount_cents":0,"strategies":[{"kind":"fixed","user_ids":["u1"]}]}]}}`))
	require.NoError(t, err)
	got, err := RuleResolver{Directory: NewMemoryDirectory(), Config: cfg}.Resolve(context.Background(), ApprovalRequest{AmountCents: 1})
	require.NoError(t, err)
	require.Equal(t, []string{"u1"}, ids(got))
}
