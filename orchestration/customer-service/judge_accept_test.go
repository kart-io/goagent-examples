package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 最终验收入口：用候选 C1 在冻结的 Final Holdout 或挑战集上各评审一次，按冻结的整数容许数逐模型判定。
// 入口只做加载、隔离、指纹核对、编排和统计；评审规则、采样与停止规则全部沿用 judgeItem 与 runJudgePhases，不在这里改。

// 两种验收数据的标签取值。字段沿用 validation 格式（label、judge_kind），没有待评答复在 Holdout 里写作 no_reply。
func init() {
	for name, refs := range map[string]map[string]string{
		"accept-holdout":   {"对": refCorrect, "错": refWrong, "uncertain": refUncertain, "no_reply": refNoReply},
		"accept-challenge": {"对": refCorrect, "错": refWrong},
	} {
		f := labelFormats["validation"]
		f.refs = refs
		labelFormats[name] = f
	}
}

const (
	acceptThresholdsFile = "acceptance-thresholds.json"
	acceptPlanFile       = "final-acceptance-plan.md"
)

var acceptModels = []string{"deepseek", "gemini"}

// acceptSet 声明一种验收数据：冻结目录里的文件、每个场景的条数、用户批准的两模型合计上限与通过标准。
type acceptSet struct {
	Format       string // labelFormats 中的格式名
	Data, Pairs  string // Pairs 为空表示没有配对文件
	PerScenario  int
	Budget       time.Duration
	MaxPositions int
	MaxHTTP      int
	Criteria     []acceptCriterion
}

var acceptSets = map[string]acceptSet{
	"holdout": {Format: "accept-holdout", Data: "holdout-v1.jsonl", PerScenario: 10, Budget: 45 * time.Minute, MaxPositions: 540, MaxHTTP: 1620, Criteria: holdoutCriteria},
	"challenge": {Format: "accept-challenge", Data: "challenge-v1.jsonl", Pairs: "challenge-pairs-v1.json", PerScenario: 8, Budget: 40 * time.Minute,
		MaxPositions: 432, MaxHTTP: 1296, Criteria: challengeCriteria},
}

// acceptBase 是冻结时就确定的样本计数；N = Nc + Ne，uncertain 与没有待评答复不计入 N。容许数只由它决定。
type acceptBase struct {
	N, Nc, Ne, Uncertain, NoReply int
	PerScenario                   map[string]scenarioBase
}

type scenarioBase struct{ Correct, Wrong, Uncertain, NoReply int }

func baseOf(items []refItem) acceptBase {
	b := acceptBase{PerScenario: map[string]scenarioBase{}}
	for _, it := range items {
		s := b.PerScenario[it.Scenario]
		switch it.Ref {
		case refCorrect:
			b.Nc++
			s.Correct++
		case refWrong:
			b.Ne++
			s.Wrong++
		case refUncertain:
			b.Uncertain++
			s.Uncertain++
		case refNoReply:
			b.NoReply++
			s.NoReply++
		}
		b.PerScenario[it.Scenario] = s
	}
	b.N = b.Nc + b.Ne
	return b
}

// acceptTally 是一个模型在一组数据上的结果。端到端失败把弃权与 infra 计入失败，但它们不是语义上的误判或漏判：
// 混淆矩阵、Accuracy、F1、κ 只在二元判定子集上计算（computeJudgeMetrics）。
type acceptTally struct {
	ErrFail  int `json:"error_reply_e2e_failures"`   // 参考错：漏判 + 弃权 + infra
	CorrFail int `json:"correct_reply_e2e_failures"` // 参考对：误判 + 弃权 + infra
	FN       int `json:"fn"`
	FP       int `json:"fp"`
	AbstainW int `json:"abstain_on_wrong"`
	AbstainC int `json:"abstain_on_correct"`
	InfraW   int `json:"infra_on_wrong"`
	InfraC   int `json:"infra_on_correct"`
	InfraAll int `json:"infra_all"` // 含 uncertain 样本
	Unstable int `json:"unstable"`  // 可评样本中三票不是同一单票结果的条数
	// FailedScenarios 是挑战集 C4 未达标的场景：任一侧端到端失败超过 1 条。
	FailedScenarios []string                  `json:"failed_scenarios"`
	PerScenario     map[string]*scenarioTally `json:"per_scenario"`
}

type scenarioTally struct {
	ErrFail  int `json:"error_reply_e2e_failures"`
	CorrFail int `json:"correct_reply_e2e_failures"`
	Unstable int `json:"unstable"`
}

// stableVotes：三个位置都有合法单票且三票相同才算稳定，三票都是「无法判定」也算稳定（但仍是端到端失败）；infra 或缺票不算。
func stableVotes(res judgeItemResult) bool {
	if res.Final == verdictInfra || len(res.Votes) != judgePositions {
		return false
	}
	for _, v := range res.Votes[1:] {
		if v != res.Votes[0] {
			return false
		}
	}
	return true
}

func tallyOf(items []refItem, results map[string]judgeItemResult) acceptTally {
	t := acceptTally{PerScenario: map[string]*scenarioTally{}}
	for _, it := range items {
		res := results[it.In.ID]
		if it.Ref == refNoReply {
			continue
		}
		if res.Final == verdictInfra {
			t.InfraAll++
		}
		if it.Ref == refUncertain {
			continue
		}
		s := t.PerScenario[it.Scenario]
		if s == nil {
			s = &scenarioTally{}
			t.PerScenario[it.Scenario] = s
		}
		if !stableVotes(res) {
			t.Unstable++
			s.Unstable++
		}
		if res.Final == it.Ref {
			continue
		}
		if it.Ref == refWrong {
			t.ErrFail++
			s.ErrFail++
			*map[string]*int{refCorrect: &t.FN, verdictAbstain: &t.AbstainW, verdictInfra: &t.InfraW}[res.Final]++
		} else {
			t.CorrFail++
			s.CorrFail++
			*map[string]*int{refWrong: &t.FP, verdictAbstain: &t.AbstainC, verdictInfra: &t.InfraC}[res.Final]++
		}
	}
	for _, name := range slices.Sorted(maps.Keys(t.PerScenario)) {
		if s := t.PerScenario[name]; s.ErrFail > 1 || s.CorrFail > 1 {
			t.FailedScenarios = append(t.FailedScenarios, name)
		}
	}
	return t
}

// acceptCriterion 是一条通过标准：Value 是观测到的失败数，Limit 由冻结时的计数算出整数容许数，na 为真表示不适用。
type acceptCriterion struct {
	ID, Desc string
	Value    func(acceptTally) int
	Limit    func(acceptBase) (limit int, na bool)
}

func fixedLimit(n int) func(acceptBase) (int, bool) {
	return func(acceptBase) (int, bool) { return n, false }
}

var holdoutCriteria = []acceptCriterion{
	{"H1", "参考对的端到端失败（误判 + 弃权 + infra）≤ ⌊0.03 × Nc⌋",
		func(t acceptTally) int { return t.CorrFail }, func(b acceptBase) (int, bool) { return b.Nc * 3 / 100, false }},
	{"H2", "参考错的端到端失败（漏判 + 弃权 + infra）：Ne = 0 不适用；1 ≤ Ne < 10 为 0；否则 ≤ ⌊0.1 × Ne⌋",
		func(t acceptTally) int { return t.ErrFail }, func(b acceptBase) (int, bool) {
			switch {
			case b.Ne == 0:
				return 0, true
			case b.Ne < 10:
				return 0, false
			}
			return b.Ne / 10, false
		}},
	{"H3", "infra（含 uncertain 样本）≤ 2", func(t acceptTally) int { return t.InfraAll }, fixedLimit(2)},
	{"H4", "可评样本三票不稳定 ≤ ⌊0.1 × N⌋", func(t acceptTally) int { return t.Unstable }, func(b acceptBase) (int, bool) { return b.N / 10, false }},
}

var challengeCriteria = []acceptCriterion{
	{"C1", "错误答复的端到端失败 ≤ 2", func(t acceptTally) int { return t.ErrFail }, fixedLimit(2)},
	{"C2", "正确答复的端到端失败 ≤ 2", func(t acceptTally) int { return t.CorrFail }, fixedLimit(2)},
	{"C3", "两侧端到端失败合计 ≤ 3", func(t acceptTally) int { return t.ErrFail + t.CorrFail }, fixedLimit(3)},
	{"C4", "每个场景两侧端到端失败各 ≤ 1（未达标场景数 ≤ 0）", func(t acceptTally) int { return len(t.FailedScenarios) }, fixedLimit(0)},
	{"C5", "三票不稳定 ≤ 7", func(t acceptTally) int { return t.Unstable }, fixedLimit(7)},
}

// holdoutSufficient 是 H0：没有待评答复 ≤ 9 且 Nc ≥ 60。不满足时 Holdout 记为样本不足，不运行评审。
func holdoutSufficient(b acceptBase) error {
	if b.NoReply > 9 || b.Nc < 60 {
		return fmt.Errorf("H0 不满足：无待评答复 %d 条（上限 9），Nc = %d（下限 60），Holdout 样本不足", b.NoReply, b.Nc)
	}
	return nil
}

// acceptThresholds 是冻结在数据目录里的计数与整数容许数；入口重新计算并要求与冻结文件完全一致。
type acceptThresholds struct {
	Set    string          `json:"set"`
	Base   acceptBase      `json:"base"`
	Limits map[string]*int `json:"limits"` // nil 表示不适用
}

func thresholdsFor(set string, items []refItem) acceptThresholds {
	b := baseOf(items)
	th := acceptThresholds{Set: set, Base: b, Limits: map[string]*int{}}
	for _, c := range acceptSets[set].Criteria {
		if n, na := c.Limit(b); !na {
			th.Limits[c.ID] = &n
		} else {
			th.Limits[c.ID] = nil
		}
	}
	return th
}

// challengePair 只解码统计需要的字段；案例编号、形态、错误类型、预期答案留在文件里供审阅，执行器不读。
type challengePair struct {
	Pair     string `json:"pair"`
	Scenario string `json:"scenario"`
	Correct  string `json:"correct_id"`
	Wrong    string `json:"wrong_id"`
}

func loadPairs(path string, items []refItem) ([]challengePair, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pairs []challengePair
	if err := json.Unmarshal(b, &pairs); err != nil {
		return nil, fmt.Errorf("配对文件不是合法 JSON：%w", err)
	}
	byID := map[string]refItem{}
	for _, it := range items {
		byID[it.In.ID] = it
	}
	used := map[string]bool{}
	for _, p := range pairs {
		for _, side := range []struct{ id, ref string }{{p.Correct, refCorrect}, {p.Wrong, refWrong}} {
			it, ok := byID[side.id]
			switch {
			case !ok:
				return nil, fmt.Errorf("配对 %s 引用了不存在的样本 %q", p.Pair, side.id)
			case used[side.id]:
				return nil, fmt.Errorf("样本 %s 出现在多个配对里", side.id)
			case it.Ref != side.ref || it.Scenario != p.Scenario:
				return nil, fmt.Errorf("配对 %s 的 %s 标签或场景不符：%s/%s", p.Pair, side.id, it.Ref, it.Scenario)
			}
			used[side.id] = true
		}
	}
	if len(used) != len(items) {
		return nil, fmt.Errorf("配对覆盖 %d 条，数据集有 %d 条", len(used), len(items))
	}
	return pairs, nil
}

// acceptInput 是一次验收评审装配好的输入；prepareAcceptance 在发出任何请求之前完成全部核对。
type acceptInput struct {
	Set       string
	Spec      acceptSet
	Frozen    map[string]string
	Items     []refItem
	Pairs     []challengePair
	Th        acceptThresholds
	Candidate candidateFingerprint
	Phase     runPhase
	Skipped   []string
}

func prepareAcceptance(set, dataDir string, models []runModel) (acceptInput, error) {
	spec, ok := acceptSets[set]
	if !ok {
		return acceptInput{}, fmt.Errorf("未知的验收数据 %q，只支持 holdout 或 challenge", set)
	}
	in := acceptInput{Set: set, Spec: spec}
	var err error
	if in.Frozen, err = verifyFrozen(dataDir); err != nil {
		return in, err
	}
	for _, f := range []string{spec.Data, spec.Pairs, acceptThresholdsFile, acceptPlanFile} {
		if _, ok := in.Frozen[f]; f != "" && !ok {
			return in, fmt.Errorf("%s 不在 FROZEN.sha256 里", f)
		}
	}
	if in.Items, err = loadLabelFile(filepath.Join(dataDir, spec.Data), spec.Format); err != nil {
		return in, err
	}
	b := baseOf(in.Items)
	for _, sc := range liveScenarios {
		s := b.PerScenario[sc.name]
		if n := s.Correct + s.Wrong + s.Uncertain + s.NoReply; n != spec.PerScenario {
			return in, fmt.Errorf("场景 %s 有 %d 条，应为 %d 条", sc.name, n, spec.PerScenario)
		}
	}
	if spec.Pairs != "" {
		if in.Pairs, err = loadPairs(filepath.Join(dataDir, spec.Pairs), in.Items); err != nil {
			return in, err
		}
	}
	if set == "holdout" {
		if err := holdoutSufficient(b); err != nil {
			return in, err
		}
	}
	in.Th = thresholdsFor(set, in.Items)
	raw, err := os.ReadFile(filepath.Join(dataDir, acceptThresholdsFile))
	if err != nil {
		return in, err
	}
	var frozenTh acceptThresholds
	if err := json.Unmarshal(raw, &frozenTh); err != nil {
		return in, fmt.Errorf("%s 不是合法 JSON：%w", acceptThresholdsFile, err)
	}
	if !reflect.DeepEqual(frozenTh, in.Th) {
		return in, fmt.Errorf("冻结的计数或容许数与数据重新计算的结果不一致")
	}
	in.Phase = runPhase{Dataset: set}
	for _, it := range in.Items {
		if it.Ref == refNoReply {
			in.Skipped = append(in.Skipped, it.In.ID)
			continue
		}
		in.Phase.Items = append(in.Phase.Items, it)
	}
	if pos := judgePositions * len(in.Phase.Items) * len(models); pos > spec.MaxPositions || (1+judgeMaxRetries)*pos > spec.MaxHTTP {
		return in, fmt.Errorf("计划 %d 个位置、最多 %d 次 HTTP，超出批准的 %d 个位置、%d 次 HTTP", pos, (1+judgeMaxRetries)*pos, spec.MaxPositions, spec.MaxHTTP)
	}
	if in.Candidate, err = verifyCandidate(candidateC1, models); err != nil {
		return in, err
	}
	return in, nil
}

type acceptManifest struct {
	CreatedAt  string               `json:"created_at"`
	Purpose    string               `json:"purpose"`
	Set        string               `json:"set"`
	Code       map[string]string    `json:"code"`
	Candidate  candidateFingerprint `json:"candidate_fingerprint"`
	Frozen     map[string]string    `json:"frozen_sha256"`
	Thresholds acceptThresholds     `json:"thresholds"`
	Scored     []string             `json:"scored_ids"`
	Uncertain  []string             `json:"uncertain_ids"`
	Skipped    []string             `json:"skipped_no_reply_ids"`
	ModelOrder []string             `json:"model_order"`
	Models     []runModelManifest   `json:"models"`
	Plan       map[string]any       `json:"plan"`
	StopRules  map[string]any       `json:"stop_rules"`
	Exclusions []string             `json:"exclusions"`
}

func buildAcceptManifest(code map[string]string, in acceptInput, models []runModel) acceptManifest {
	perModel := judgePositions * len(in.Phase.Items)
	maxPerModel := (1 + judgeMaxRetries) * perModel
	m := acceptManifest{
		CreatedAt:  time.Now().Format(time.RFC3339),
		Purpose:    "候选 C1 最终验收：每个评审模型单独报告、单独判定；本数据集只评审这一次，看到结果后不调参。",
		Set:        in.Set,
		Code:       code,
		Candidate:  in.Candidate,
		Frozen:     in.Frozen,
		Thresholds: in.Th,
		Skipped:    in.Skipped,
		ModelOrder: acceptModels,
		Models:     modelManifests(models, maxPerModel),
		Plan: map[string]any{
			"positions_per_item":          judgePositions,
			"max_retries_per_position":    judgeMaxRetries,
			"planned_positions_per_model": perModel,
			"planned_positions_total":     perModel * len(models),
			"max_http_attempts_total":     maxPerModel * len(models),
			"approved_max_positions":      in.Spec.MaxPositions,
			"approved_max_http":           in.Spec.MaxHTTP,
			"concurrency":                 1,
			"global_wall_clock_budget":    in.Spec.Budget.String(),
			"budget_starts":               "第一次真实请求之前开始计时",
		},
		StopRules: map[string]any{
			"blocking_http_status": blockingHTTPStatus,
			"on_blocking":          "该位置按协议用完重试并记录后，停止该模型剩余的全部样本，另一模型继续",
			"on_cancel_or_budget":  "标记 interrupted 或 budget_exhausted，保留各位置状态；不自动重启，不重置预算",
			"exhausted_position":   "任一位置技术重试耗尽，该样本记为 infra 并跳过剩余位置；infra 是终态，计入端到端失败，任何续跑都不补这些位置",
			"completion":           "每条计划样本都有终态（对、错、abstain、infra）才算完成；只有因阻断、预算或取消而没有终态的样本使模型未完成",
			"continuation":         "续跑须用户另行批准，继承每个位置已用尝试、各模型与全局 HTTP 已用量和累计运行时间；新目录不重置预算，已完成位置和 infra 终态样本不重跑",
		},
		Exclusions: []string{"参考标签、配对关系、案例编号、形态、错误类型、预期答案、样本 ID 都不进入提示词；参考标签只在结果里供统计",
			"uncertain 只记录三票与聚合，不进入 N；没有待评答复的样本不发送",
			"Holdout 生成阶段的旧评审输出不在验收数据里"},
	}
	for _, it := range in.Phase.Items {
		if itemGroup(it) == "uncertain" {
			m.Uncertain = append(m.Uncertain, it.In.ID)
		} else {
			m.Scored = append(m.Scored, it.In.ID)
		}
	}
	return m
}

// 验收结论。
const (
	acceptPass       = "通过"
	acceptFail       = "未通过"
	acceptIncomplete = "未完成"
)

type criterionResult struct {
	ID    string `json:"id"`
	Desc  string `json:"desc"`
	Value int    `json:"value"`
	Limit *int   `json:"limit"` // nil 表示不适用
	Pass  *bool  `json:"pass"`  // nil 表示不适用
}

type acceptModelReport struct {
	Name       string            `json:"name"`
	RunStatus  string            `json:"run_status"`
	StopReason string            `json:"stop_reason,omitempty"`
	Verdict    string            `json:"verdict"`
	NoTerminal []string          `json:"items_without_terminal_state"`
	Tally      *acceptTally      `json:"tally,omitempty"`
	Criteria   []criterionResult `json:"criteria,omitempty"`
	Metrics    *judgeMetrics     `json:"binary_subset_metrics,omitempty"`
	PairsBoth  *int              `json:"pairs_both_right,omitempty"`
}

type acceptReport struct {
	Set        string              `json:"set"`
	Thresholds acceptThresholds    `json:"thresholds"`
	Models     []acceptModelReport `json:"models"`
}

// evaluateAcceptance 按模型判定。模型未完成时只报告状态计数，不给语义指标。
func evaluateAcceptance(in acceptInput, sum runSummary, recs []runRecord) acceptReport {
	rep := acceptReport{Set: in.Set, Thresholds: in.Th}
	for _, ms := range sum.Models {
		mr := acceptModelReport{Name: ms.Name, RunStatus: ms.Status, StopReason: ms.StopReason}
		results := map[string]judgeItemResult{}
		for _, r := range recs {
			if r.Model == ms.Name && r.Result.Final != "" {
				results[r.Result.ID] = r.Result
			}
		}
		for _, it := range in.Phase.Items {
			if _, ok := results[it.In.ID]; !ok {
				mr.NoTerminal = append(mr.NoTerminal, it.In.ID)
			}
		}
		if len(mr.NoTerminal) > 0 {
			mr.Verdict = acceptIncomplete
			rep.Models = append(rep.Models, mr)
			continue
		}
		t := tallyOf(in.Items, results)
		mr.Tally, mr.Verdict = &t, acceptPass
		for _, c := range in.Spec.Criteria {
			cr := criterionResult{ID: c.ID, Desc: c.Desc, Value: c.Value(t), Limit: in.Th.Limits[c.ID]}
			if cr.Limit != nil {
				ok := cr.Value <= *cr.Limit
				cr.Pass = &ok
				if !ok {
					mr.Verdict = acceptFail
				}
			}
			mr.Criteria = append(mr.Criteria, cr)
		}
		var scored []judgeItemResult
		for _, it := range in.Items {
			if res, ok := results[it.In.ID]; ok {
				res.Ref = it.Ref
				scored = append(scored, res)
			}
		}
		m := computeJudgeMetrics(scored)
		mr.Metrics = &m
		if in.Pairs != nil {
			both := 0
			for _, p := range in.Pairs {
				if results[p.Correct].Final == refCorrect && results[p.Wrong].Final == refWrong {
					both++
				}
			}
			mr.PairsBoth = &both
		}
		rep.Models = append(rep.Models, mr)
	}
	return rep
}

// firstAttemptStrict 统计第一次尝试即合法、且全部非空引文逐字匹配的位置数。
func firstAttemptStrict(recs []runRecord) (strict, positions int) {
	for _, r := range recs {
		for _, a := range r.Result.Attempts {
			if a.Attempt != 1 {
				continue
			}
			positions++
			if a.Stage != "" {
				continue
			}
			ok := true
			for _, q := range a.Quotes {
				ok = ok && q.Method == quoteExact
			}
			if ok {
				strict++
			}
		}
	}
	return strict, positions
}

func fmtLimit(l *int) string {
	if l == nil {
		return "不适用"
	}
	return fmt.Sprint(*l)
}

// buildAcceptReport 写出单个数据集的验收报告；两组数据与两个模型都分别报告，不合并。
func buildAcceptReport(in acceptInput, rep acceptReport, recs []runRecord) string {
	var b strings.Builder
	th := rep.Thresholds
	fmt.Fprintf(&b, "# 候选 C1 最终验收报告：%s\n\n", rep.Set)
	fmt.Fprintf(&b, "N = %d（Nc = %d，Ne = %d），uncertain %d 条，无待评答复 %d 条。uncertain 与无待评答复不计入 N。\n\n",
		th.Base.N, th.Base.Nc, th.Base.Ne, th.Base.Uncertain, th.Base.NoReply)
	b.WriteString("口径：端到端失败把弃权与 infra 计为失败，但它们不是语义上的误判或漏判；混淆矩阵、Accuracy、F1、κ 只在二元判定子集上计算。" +
		"Wilson 区间按协议报告；挑战集是配对构造、场景固定，Holdout 场景也固定，区间没有为这些结构做调整。" +
		"通过表示达到预设的运行门槛，不是以 95% 置信度证明总体正确率达到 95%。\n\n")
	for _, mr := range rep.Models {
		mrecs := slices.DeleteFunc(slices.Clone(recs), func(r runRecord) bool { return r.Model != mr.Name })
		fmt.Fprintf(&b, "## %s：%s\n\n运行状态 %s", mr.Name, mr.Verdict, mr.RunStatus)
		if mr.StopReason != "" {
			fmt.Fprintf(&b, "（%s）", mr.StopReason)
		}
		b.WriteString("\n\n")
		if mr.Verdict == acceptIncomplete {
			fmt.Fprintf(&b, "没有终态的样本 %d 条：%s。按方案只报告状态计数，不给语义指标。\n\n", len(mr.NoTerminal), strings.Join(mr.NoTerminal, "、"))
			continue
		}
		b.WriteString("| 标准 | 内容 | 观测 | 容许 | 结论 |\n|---|---|---|---|---|\n")
		for _, c := range mr.Criteria {
			verdict := "不适用"
			if c.Pass != nil {
				verdict = map[bool]string{true: "达标", false: "未达标"}[*c.Pass]
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %s | %s |\n", c.ID, c.Desc, c.Value, fmtLimit(c.Limit), verdict)
		}
		t, m := mr.Tally, mr.Metrics
		fmt.Fprintf(&b, "\n- 参考错：端到端失败 %d（漏判 %d，弃权 %d，infra %d）；检出率 %s\n", t.ErrFail, t.FN, t.AbstainW, t.InfraW, fmtRatio(m.EndToEnd))
		fmt.Fprintf(&b, "- 参考对：端到端失败 %d（误判 %d，弃权 %d，infra %d）；判对 %s\n", t.CorrFail, t.FP, t.AbstainC, t.InfraC, fmtRatio(newRatio(th.Base.Nc-t.CorrFail, th.Base.Nc)))
		fmt.Fprintf(&b, "- 全部 N：端到端正确 %s\n", fmtRatio(newRatio(th.Base.N-t.ErrFail-t.CorrFail, th.Base.N)))
		fmt.Fprintf(&b, "- 二元判定子集：TP %d，FN %d，FP %d，TN %d；覆盖 %s；Accuracy %s；F1 %s；κ %s\n",
			m.TP, m.FN, m.FP, m.TN, fmtRatio(m.Coverage), fmtRatio(m.Accuracy), fmtPoint(m.F1), fmtPoint(m.Kappa))
		fmt.Fprintf(&b, "- 三票稳定：不稳定 %d 条（infra 与缺票计为不稳定；三票都是无法判定计为稳定的弃权）\n", t.Unstable)
		if mr.PairsBoth != nil {
			fmt.Fprintf(&b, "- 配对两侧都判对：%d/%d（只报告）\n", *mr.PairsBoth, len(in.Pairs))
		}
		b.WriteString("\n| 场景 | 参考对 | 参考错 | uncertain | 无待评答复 | 对侧失败 | 错侧失败 | 不稳定 |\n|---|---|---|---|---|---|---|---|\n")
		for _, sc := range liveScenarios {
			sb, st := th.Base.PerScenario[sc.name], t.PerScenario[sc.name]
			if st == nil {
				st = &scenarioTally{}
			}
			fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d | %d |\n", sc.name, sb.Correct, sb.Wrong, sb.Uncertain, sb.NoReply, st.CorrFail, st.ErrFail, st.Unstable)
		}
		s := collectStats(mrecs)
		strict, firsts := firstAttemptStrict(mrecs)
		fmt.Fprintf(&b, "\n技术统计（只报告）：尝试 %d 次，HTTP %d 次，重试 %d 次，quote_mismatch %d 次，加粗容忍引文 %d 段，首次尝试严格逐字合规 %d/%d，思考 token 已报告 %d/%d 次。\n\n",
			s.Attempts, s.Requests, s.Retries, s.QuoteMismatch, s.BoldQuotes, strict, firsts, s.ReasoningReported, s.Attempts)
		for _, sec := range []struct {
			title string
			pick  func(refItem, judgeItemResult) bool
		}{
			{"未判对或三票不稳定的可评样本", func(it refItem, res judgeItemResult) bool {
				return it.Ref != refUncertain && (res.Final != it.Ref || !stableVotes(res))
			}},
			{"uncertain 样本（只记票型，不进入 N）", func(it refItem, _ judgeItemResult) bool { return it.Ref == refUncertain }},
		} {
			fmt.Fprintf(&b, "%s：\n\n", sec.title)
			for _, it := range in.Phase.Items {
				for _, r := range mrecs {
					if r.Result.ID == it.In.ID && sec.pick(it, r.Result) {
						fmt.Fprintf(&b, "- %s（%s，参考 %s）\n", it.In.ID, it.Scenario, it.Ref)
						caseEvidence(&b, mr.Name, r.Result)
					}
				}
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// runAcceptance 完成一次验收评审：先核对，再执行，最后在运行目录里另写 acceptance.json 与 acceptance.md。
func runAcceptance(ctx context.Context, set, dataDir, runDir string, code map[string]string, models []runModel) (acceptReport, error) {
	in, err := prepareAcceptance(set, dataDir, models)
	if err != nil {
		return acceptReport{}, err
	}
	sum, err := runJudgePhases(ctx, runDir, buildAcceptManifest(code, in, models), []runPhase{in.Phase}, models, in.Spec.Budget, true)
	if err != nil {
		return acceptReport{}, err
	}
	recs, err := loadRunRecords(runDir)
	if err != nil {
		return acceptReport{}, err
	}
	rep := evaluateAcceptance(in, sum, recs)
	jb, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return rep, err
	}
	return rep, errors.Join(writeExclusive(filepath.Join(runDir, "acceptance.json"), append(jb, '\n')),
		writeExclusive(filepath.Join(runDir, "acceptance.md"), []byte(buildAcceptReport(in, rep, recs))))
}

// TestJudgeAcceptance 执行一次最终验收评审。必须在已跟踪文件全部提交的工作区里运行：
//
//	CS_JUDGE_ACCEPT_SET=holdout|challenge CS_JUDGE_ACCEPT_DATA=<冻结目录> CS_JUDGE_ACCEPT_DIR=<新目录> \
//	  go test -run TestJudgeAcceptance -timeout 60m ./orchestration/customer-service/
func TestJudgeAcceptance(t *testing.T) {
	dir := requireRunDir(t, "CS_JUDGE_ACCEPT_DIR")
	set, data := os.Getenv("CS_JUDGE_ACCEPT_SET"), os.Getenv("CS_JUDGE_ACCEPT_DATA")
	require.NotEmpty(t, set, "需要 CS_JUDGE_ACCEPT_SET")
	require.NotEmpty(t, data, "需要 CS_JUDGE_ACCEPT_DATA")
	code := codeState(t)
	models := modelsFromEnv(t, acceptModels)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep, err := runAcceptance(ctx, set, data, dir, code, models)
	require.NoError(t, err)
	for _, m := range rep.Models {
		t.Logf("%s：%s（运行状态 %s）", m.Name, m.Verdict, m.RunStatus)
	}
}
