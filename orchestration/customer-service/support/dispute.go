package support

import (
	"errors"
	"fmt"
)

// DisputeRules 是业务配置的确定性争议规则；Version 随阈值变化而提升，并写入判定。
type DisputeRules struct {
	Version            string
	MaxAutoAmountCents int64
	MaxDeliveryDays    int
	MaxRefundRatio     float64
}

// Verdict 是登记时计算并固定的争议判定。
type Verdict struct {
	Disputed    bool
	Reasons     []string
	RuleVersion string
}

// DisputeFacts 是登记事务内读到的订单事实。PriorRequests 是该订单在本申请之前已登记的申请数（任何状态）。
type DisputeFacts struct {
	AmountCents, PaidCents, RefundedCents int64
	DeliveryDays, PriorRequests           int
}

// DefaultRules 是演示用的争议规则。
func DefaultRules() DisputeRules {
	return DisputeRules{Version: "rules-v1", MaxAutoAmountCents: 50000, MaxDeliveryDays: 7, MaxRefundRatio: 0.8}
}

// Validate 拒绝无法给出确定结果的规则配置。
func (r DisputeRules) Validate() error {
	if r.Version == "" || r.MaxAutoAmountCents < 0 || r.MaxDeliveryDays < 0 || r.MaxRefundRatio <= 0 || r.MaxRefundRatio > 1 {
		return errors.New("争议规则需要版本号、非负阈值与 (0, 1] 内的退款比例")
	}
	return nil
}

// Judge 计算“规则判定 OR Agent 升级”：Agent 只能把无争议升级为有争议，不能降级。
func (r DisputeRules) Judge(f DisputeFacts, agentDisputed bool, agentNote string) Verdict {
	v := Verdict{RuleVersion: r.Version}
	if f.AmountCents > r.MaxAutoAmountCents {
		v.Reasons = append(v.Reasons, fmt.Sprintf("金额 %d 分超过自动退款上限 %d 分", f.AmountCents, r.MaxAutoAmountCents))
	}
	if f.DeliveryDays > r.MaxDeliveryDays {
		v.Reasons = append(v.Reasons, fmt.Sprintf("签收 %d 天超过 %d 天", f.DeliveryDays, r.MaxDeliveryDays))
	}
	if float64(f.RefundedCents+f.AmountCents) > float64(f.PaidCents)*r.MaxRefundRatio {
		v.Reasons = append(v.Reasons, fmt.Sprintf("累计退款 %d 分超过支付额的 %.0f%%", f.RefundedCents+f.AmountCents, r.MaxRefundRatio*100))
	}
	if f.PriorRequests > 0 {
		v.Reasons = append(v.Reasons, fmt.Sprintf("该订单此前已有 %d 笔退款申请", f.PriorRequests))
	}
	if agentDisputed {
		v.Reasons = append(v.Reasons, "Agent 标记有争议："+agentNote)
	}
	v.Disputed = len(v.Reasons) > 0
	return v
}
