package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 开发评审报告：只读一次运行目录里的 results.jsonl 与 summary.json，指标用执行器同一个 computeJudgeMetrics 计算，
// 报告另写 report.md，不改运行目录里的原始文件。只有设置 CS_JUDGE_DEVRUN_REPORT=<运行目录> 时才生成。

func loadRunRecords(dir string) ([]runRecord, error) {
	f, err := os.Open(filepath.Join(dir, "results.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var recs []runRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var r runRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, err
		}
		recs = append(recs, r)
	}
	return recs, sc.Err()
}

func fmtRatio(r ratio) string {
	if r.NA {
		return "N/A（分母 0）"
	}
	return fmt.Sprintf("%d/%d = %.1f%%（Wilson 95%% %.1f%%–%.1f%%）", r.K, r.N, 100*float64(r.K)/float64(r.N), 100*r.Lo, 100*r.Hi)
}

func fmtPoint(p *float64) string {
	if p == nil {
		return "N/A"
	}
	return fmt.Sprintf("%.3f", *p)
}

// attemptStats 是一组尝试的技术统计。
type attemptStats struct {
	Attempts, Requests, Retries, Truncated, Timeout, QuoteMismatch int
	ElapsedMS                                                      []int64
	Prompt, Output, Reasoning, ReasoningReported, UsageMissing     int
	Finish                                                         map[string]int
	ResponseModels, Versions, Fingerprints                         map[string]int
}

func collectStats(recs []runRecord) attemptStats {
	s := attemptStats{Finish: map[string]int{}, ResponseModels: map[string]int{}, Versions: map[string]int{}, Fingerprints: map[string]int{}}
	for _, r := range recs {
		for _, a := range r.Result.Attempts {
			s.Attempts++
			s.Requests += a.Audit.Requests
			if a.Attempt > 1 {
				s.Retries++
			}
			switch a.Cause {
			case causeTruncated:
				s.Truncated++
			case causeTimeout:
				s.Timeout++
			case causeQuote:
				s.QuoteMismatch++
			}
			s.ElapsedMS = append(s.ElapsedMS, a.ElapsedMS)
			s.Finish[a.Audit.FinishReason]++
			s.ResponseModels[a.Audit.ResponseModel]++
			s.Versions[a.Audit.ModelVersion]++
			s.Fingerprints[a.Audit.SystemFingerprint]++
			if u := a.Audit.Usage; u != nil {
				s.Prompt += u.Prompt
				s.Output += u.Output
				s.Reasoning += u.Reasoning
				if u.ReasoningReported {
					s.ReasoningReported++
				}
			} else {
				s.UsageMissing++
			}
		}
	}
	return s
}

func fmtCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, m[k]))
	}
	return strings.Join(parts, "，")
}

// caseEvidence 写出一条样本在某模型下的检查项票型与每个位置的引文；引文为空的不列。
func caseEvidence(b *strings.Builder, model string, res judgeItemResult) {
	fmt.Fprintf(b, "  - %s：最终 %s，票 %v，检查项 %s\n", model, res.Final, res.Votes, fmtCheckVotes(res.CheckVotes))
	for _, a := range res.Attempts {
		if a.Stage != "" {
			fmt.Fprintf(b, "    - 位置 %d 第 %d 次：技术失败 %s/%s\n", a.Position, a.Attempt, a.Stage, a.Cause)
			continue
		}
		var qs []string
		for _, ans := range a.Answers {
			q := ans.Quote
			if q == "" {
				q = "（空）"
			}
			qs = append(qs, fmt.Sprintf("%s=%s 引文「%s」", ans.ID, ans.Answer, q))
		}
		fmt.Fprintf(b, "    - 位置 %d：%s\n", a.Position, strings.Join(qs, "；"))
	}
}

func fmtCheckVotes(cv map[string]map[string]int) string {
	ids := make([]string, 0, len(cv))
	for id := range cv {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var parts []string
	for _, id := range ids {
		parts = append(parts, id+"{"+fmtCounts(cv[id])+"}")
	}
	return strings.Join(parts, " ")
}

func buildDevRunReport(sum runSummary, recs []runRecord, items map[string]refItem) string {
	var b strings.Builder
	b.WriteString("# 开发评审结果报告\n\n参考标签均为「模型复核参考标签」。Validation 是开发验证集，不是独立测试集。指标按 judge-protocol/1 §3、§5 由执行器的 computeJudgeMetrics 计算，Calibration 与 Validation 分开报告，不合并。\n\n")
	fmt.Fprintf(&b, "运行状态：**%s**，开始 %s，结束 %s，耗时 %.1f 分钟。\n\n", sum.Status, sum.StartedAt, sum.EndedAt, float64(sum.ElapsedMS)/60000)

	b.WriteString("## 1. 执行完成情况（按采样位置）\n\n| 模型 | 状态 | 数据集 | 计划 | 完成 | 失败 | 中断 | 未执行 | 尝试 | HTTP |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for _, m := range sum.Models {
		for _, ds := range []string{"calibration", "validation"} {
			c := m.ByDataset[ds]
			if c == nil {
				continue
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %d | %d | %d | %d | %d |\n", m.Name, m.Status, ds, c.Planned, c.Completed, c.Failed, c.Interrupted, c.Unexecuted, c.Attempts, c.Requests)
		}
		if m.StopReason != "" {
			fmt.Fprintf(&b, "\n%s 停止原因：%s\n\n", m.Name, m.StopReason)
		}
	}

	byKey := map[string][]runRecord{}
	for _, r := range recs {
		byKey[r.Dataset+"|"+r.Model] = append(byKey[r.Dataset+"|"+r.Model], r)
	}
	models := []string{}
	for _, m := range sum.Models {
		models = append(models, m.Name)
	}

	b.WriteString("\n## 2. 语义结果与指标（参考可评集合，正类为「答复有错」）\n\n")
	for _, ds := range []string{"calibration", "validation"} {
		for _, model := range models {
			var scored []judgeItemResult
			for _, r := range byKey[ds+"|"+model] {
				if r.Group == "scored" && r.Error == "" {
					scored = append(scored, r.Result)
				}
			}
			m := computeJudgeMetrics(scored)
			fmt.Fprintf(&b, "### %s / %s\n\n", ds, model)
			fmt.Fprintf(&b, "- 参考可评且已得出最终结果的条数 N = %d\n", m.N)
			fmt.Fprintf(&b, "- 二分类覆盖率 %s；弃权率 %s；infra 率 %s", fmtRatio(m.Coverage), fmtRatio(m.Abstain), fmtRatio(m.Infra))
			if len(m.InfraByStage) > 0 {
				fmt.Fprintf(&b, "；infra 失败阶段 %s", fmtCounts(m.InfraByStage))
			}
			fmt.Fprintf(&b, "\n- 混淆矩阵：TP=%d（参考错·判错） FN=%d（参考错·判对） FP=%d（参考对·判错） TN=%d（参考对·判对）\n", m.TP, m.FN, m.FP, m.TN)
			fmt.Fprintf(&b, "- 错误召回率 %s\n- 正确答复误判率 %s\n- 判错精确率 %s\n- Accuracy %s\n- F1 %s；κ %s（点估计）\n", fmtRatio(m.Recall), fmtRatio(m.FPR), fmtRatio(m.Precision), fmtRatio(m.Accuracy), fmtPoint(m.F1), fmtPoint(m.Kappa))
			fmt.Fprintf(&b, "- 端到端错误检出（分母为全部参考错误，含弃权与 infra）%s\n\n", fmtRatio(m.EndToEnd))
		}
	}
	b.WriteString("Validation 参考错误只有 1 条：其错误召回率与端到端检出的分母为 1，区间极宽，不能据此或据高 Accuracy 概括识别错误答复的能力。\n\n")

	b.WriteString("## 3. uncertain 样本（不进入语义指标）\n\n")
	for _, model := range models {
		for _, r := range byKey["validation|"+model] {
			if r.Group == "uncertain" {
				fmt.Fprintf(&b, "- %s / %s：参考 %s\n", r.Result.ID, model, r.Result.Ref)
				caseEvidence(&b, model, r.Result)
			}
		}
	}

	b.WriteString("\n## 4. 技术统计（按模型、数据集）\n\n| 模型 | 数据集 | 尝试 | HTTP | 重试 | 截断 | 超时 | quote_mismatch | 耗时 ms（中位/最大/合计） | usage 输入/输出/思考 | 思考 token 已报告 | usage 缺失 |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	var audits strings.Builder
	for _, model := range models {
		for _, ds := range []string{"calibration", "validation"} {
			s := collectStats(byKey[ds+"|"+model])
			if s.Attempts == 0 {
				fmt.Fprintf(&b, "| %s | %s | 0 | 0 | | | | | | | | |\n", model, ds)
				continue
			}
			ms := slices.Clone(s.ElapsedMS)
			slices.Sort(ms)
			var total int64
			for _, v := range ms {
				total += v
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %d | %d | %d | %d / %d / %d | %d / %d / %d | %d/%d | %d |\n", model, ds, s.Attempts, s.Requests, s.Retries, s.Truncated, s.Timeout, s.QuoteMismatch,
				ms[len(ms)/2], ms[len(ms)-1], total, s.Prompt, s.Output, s.Reasoning, s.ReasoningReported, s.Attempts, s.UsageMissing)
		}
		s := collectStats(append(append([]runRecord(nil), byKey["calibration|"+model]...), byKey["validation|"+model]...))
		fmt.Fprintf(&audits, "- %s 审计字段：结束原因 %s；返回模型 %s；模型版本 %s；system_fingerprint %s\n", model, fmtCounts(s.Finish), fmtCounts(s.ResponseModels), fmtCounts(s.Versions), fmtCounts(s.Fingerprints))
	}
	b.WriteString("\n" + audits.String() + "\n")

	b.WriteString("## 5. 与参考标签不一致的案例（参考可评集合，含弃权与 infra）\n\n")
	type key struct{ ds, id string }
	results := map[key]map[string]judgeItemResult{}
	var order []key
	for _, r := range recs {
		k := key{r.Dataset, r.Result.ID}
		if results[k] == nil {
			results[k] = map[string]judgeItemResult{}
			order = append(order, k)
		}
		results[k][r.Model] = r.Result
	}
	writeCase := func(k key) {
		it := items[k.ds+"|"+k.id]
		reply := []rune(it.In.Reply)
		if len(reply) > 400 {
			reply = append(reply[:400], []rune("……")...)
		}
		fmt.Fprintf(&b, "- **%s / %s**（场景 %s，参考 %s）\n  - 答复原文：%s\n", k.ds, k.id, it.Scenario, it.Ref, strings.ReplaceAll(string(reply), "\n", " "))
		for _, model := range models {
			if res, ok := results[k][model]; ok {
				caseEvidence(&b, model, res)
			}
		}
	}
	n := 0
	for _, k := range order {
		it := items[k.ds+"|"+k.id]
		if it.Ref != refCorrect && it.Ref != refWrong {
			continue
		}
		for _, model := range models {
			if res, ok := results[k][model]; ok && res.Final != it.Ref {
				fmt.Fprintf(&b, "%d. %s 判 %s\n", n+1, model, res.Final)
				n++
			}
		}
	}
	if n == 0 {
		b.WriteString("无。\n")
	}
	b.WriteString("\n逐条证据：\n\n")
	for _, k := range order {
		it := items[k.ds+"|"+k.id]
		if it.Ref != refCorrect && it.Ref != refWrong {
			continue
		}
		for _, model := range models {
			if res, ok := results[k][model]; ok && res.Final != it.Ref {
				writeCase(k)
				break
			}
		}
	}

	b.WriteString("\n## 6. 两模型最终判定不同的案例\n\n")
	m := 0
	for _, k := range order {
		if len(models) < 2 || len(results[k]) < 2 {
			continue
		}
		if results[k][models[0]].Final != results[k][models[1]].Final {
			writeCase(k)
			m++
		}
	}
	if m == 0 {
		b.WriteString("无。\n")
	}
	return b.String()
}

func TestJudgeDevRunReport(t *testing.T) {
	dir := os.Getenv("CS_JUDGE_DEVRUN_REPORT")
	if dir == "" {
		t.Skip("设置 CS_JUDGE_DEVRUN_REPORT=<运行目录> 才生成报告")
	}
	var sum runSummary
	b, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &sum))
	recs, err := loadRunRecords(dir)
	require.NoError(t, err)
	labels := frozenLabelDir(t)
	phases, _, err := devRunPhases(labels)
	require.NoError(t, err)
	items := map[string]refItem{}
	for _, ph := range phases {
		for _, it := range ph.Items {
			items[ph.Dataset+"|"+it.In.ID] = it
		}
	}
	for _, r := range recs {
		it, ok := items[r.Dataset+"|"+r.Result.ID]
		require.True(t, ok, r.Result.ID)
		require.Equal(t, it.Ref, r.Result.Ref, "结果里的参考标签必须与冻结文件一致")
	}
	require.NoError(t, writeExclusive(filepath.Join(dir, "report.md"), []byte(buildDevRunReport(sum, recs, items))))
}

func TestBuildDevRunReport(t *testing.T) {
	item := func(id, ref string) refItem {
		return refItem{In: judgeInput{ID: id, Reply: "答复-" + id}, Scenario: "forget", Ref: ref}
	}
	res := func(id, ref, final string) judgeItemResult {
		return judgeItemResult{ID: id, Ref: ref, Final: final, Votes: []string{final, final, final},
			Attempts: []judgeAttempt{{Position: 1, Attempt: 1, Answers: []checkAnswer{{ID: "F1", Quote: "引文-" + id, Answer: "是"}}}}}
	}
	items := map[string]refItem{}
	for _, it := range []refItem{item("same", refCorrect), item("miss", refWrong), item("split", refCorrect), item("unc", refUncertain)} {
		items["validation|"+it.In.ID] = it
	}
	recs := []runRecord{
		{Dataset: "validation", Group: "scored", Model: "a", Result: res("same", refCorrect, refCorrect)},
		{Dataset: "validation", Group: "scored", Model: "b", Result: res("same", refCorrect, refCorrect)},
		{Dataset: "validation", Group: "scored", Model: "a", Result: res("miss", refWrong, refCorrect)},
		{Dataset: "validation", Group: "scored", Model: "b", Result: res("miss", refWrong, refCorrect)},
		{Dataset: "validation", Group: "scored", Model: "a", Result: res("split", refCorrect, refCorrect)},
		{Dataset: "validation", Group: "scored", Model: "b", Result: res("split", refCorrect, verdictAbstain)},
		{Dataset: "validation", Group: "uncertain", Model: "a", Result: res("unc", refUncertain, refWrong)},
	}
	sum := runSummary{Status: runComplete, Models: []runModelSummary{{Name: "a"}, {Name: "b"}}}
	out := buildDevRunReport(sum, recs, items)
	section := func(title string) string {
		_, rest, ok := strings.Cut(out, title)
		require.True(t, ok, title)
		if i := strings.Index(rest, "\n## "); i >= 0 {
			rest = rest[:i]
		}
		return rest
	}
	cases := []struct {
		name, title string
		has, lacks  []string
	}{
		{"不一致案例含两模型漏判与弃权，不含一致样本", "## 5.", []string{"validation / miss", "validation / split", "引文-miss"}, []string{"validation / same", "unc"}},
		{"模型分歧只列判定不同的样本", "## 6.", []string{"validation / split"}, []string{"validation / miss", "validation / same"}},
		{"uncertain 单列且不进入语义指标", "## 3.", []string{"unc / a"}, []string{"split"}},
		{"Validation 指标分母只含参考可评样本", "### validation / a", []string{"N = 3", "TP=0", "FN=1", "TN=2"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := section(c.title)
			for _, h := range c.has {
				require.Contains(t, s, h)
			}
			for _, l := range c.lacks {
				require.NotContains(t, s, l)
			}
		})
	}
}
