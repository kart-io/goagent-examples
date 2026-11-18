package main

import (
	"context"
	"github.com/kart-io/goagent/codec/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func intp(n int) *int { return &n }

func TestAcceptThresholdBoundaries(t *testing.T) {
	limit := func(id string, b acceptBase) *int {
		for _, c := range append(holdoutCriteria, challengeCriteria...) {
			if c.ID == id {
				if n, na := c.Limit(b); !na {
					return &n
				}
				return nil
			}
		}
		t.Fatalf("没有标准 %s", id)
		return nil
	}
	tests := []struct {
		id   string
		base acceptBase
		want *int
	}{
		{"H1", acceptBase{Nc: 33}, intp(0)},
		{"H1", acceptBase{Nc: 34}, intp(1)},
		{"H1", acceptBase{Nc: 66}, intp(1)},
		{"H1", acceptBase{Nc: 67}, intp(2)},
		{"H1", acceptBase{Nc: 90}, intp(2)},
		{"H2", acceptBase{Ne: 0}, nil},
		{"H2", acceptBase{Ne: 1}, intp(0)},
		{"H2", acceptBase{Ne: 9}, intp(0)},
		{"H2", acceptBase{Ne: 10}, intp(1)},
		{"H2", acceptBase{Ne: 19}, intp(1)},
		{"H2", acceptBase{Ne: 20}, intp(2)},
		{"H3", acceptBase{}, intp(2)},
		{"H4", acceptBase{N: 79}, intp(7)},
		{"H4", acceptBase{N: 80}, intp(8)},
		{"C1", acceptBase{}, intp(2)},
		{"C2", acceptBase{}, intp(2)},
		{"C3", acceptBase{}, intp(3)},
		{"C4", acceptBase{}, intp(0)},
		{"C5", acceptBase{}, intp(7)},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%+v", tt.id, tt.base), func(t *testing.T) {
			require.Equal(t, tt.want, limit(tt.id, tt.base))
		})
	}
}

func TestHoldoutSufficient(t *testing.T) {
	tests := []struct {
		name string
		base acceptBase
		ok   bool
	}{
		{"边界内", acceptBase{Nc: 60, NoReply: 9}, true},
		{"无待评答复超限", acceptBase{Nc: 80, NoReply: 10}, false},
		{"参考对不足", acceptBase{Nc: 59}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.ok, holdoutSufficient(tt.base) == nil)
		})
	}
}

func TestStableVotes(t *testing.T) {
	tests := []struct {
		name string
		res  judgeItemResult
		want bool
	}{
		{"三票相同", judgeItemResult{Final: refCorrect, Votes: []string{refCorrect, refCorrect, refCorrect}}, true},
		{"三票都无法判定是稳定的弃权", judgeItemResult{Final: verdictAbstain, Votes: []string{voteUnknown, voteUnknown, voteUnknown}}, true},
		{"2:1", judgeItemResult{Final: refWrong, Votes: []string{refWrong, refWrong, refCorrect}}, false},
		{"infra 前两票相同也不算", judgeItemResult{Final: verdictInfra, Votes: []string{refCorrect, refCorrect}}, false},
		{"缺票", judgeItemResult{Final: refCorrect, Votes: []string{refCorrect, refCorrect}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, stableVotes(tt.res))
		})
	}
}

func votes3(v string) []string { return []string{v, v, v} }

func TestTallyOf(t *testing.T) {
	item := func(id, sc, ref string) refItem { return refItem{In: judgeInput{ID: id}, Scenario: sc, Ref: ref} }
	items := []refItem{
		item("w-tp", "recall", refWrong), item("w-fn", "recall", refWrong), item("w-ab", "recall", refWrong), item("w-inf", "multi", refWrong),
		item("c-tn", "recall", refCorrect), item("c-fp", "multi", refCorrect), item("c-ab", "multi", refCorrect), item("c-inf", "multi", refCorrect),
		item("unc", "forget", refUncertain), item("nr", "forget", refNoReply),
	}
	results := map[string]judgeItemResult{
		"w-tp":  {Final: refWrong, Votes: votes3(refWrong)},
		"w-fn":  {Final: refCorrect, Votes: []string{refCorrect, refCorrect, refWrong}},
		"w-ab":  {Final: verdictAbstain, Votes: votes3(voteUnknown)},
		"w-inf": {Final: verdictInfra, Votes: []string{refWrong}},
		"c-tn":  {Final: refCorrect, Votes: votes3(refCorrect)},
		"c-fp":  {Final: refWrong, Votes: votes3(refWrong)},
		"c-ab":  {Final: verdictAbstain, Votes: []string{voteUnknown, refCorrect, voteUnknown}},
		"c-inf": {Final: verdictInfra},
		"unc":   {Final: verdictInfra},
	}
	got := tallyOf(items, results)
	require.Equal(t, 3, got.ErrFail, "漏判、弃权、infra 都是错侧端到端失败")
	require.Equal(t, 3, got.CorrFail)
	require.Equal(t, [6]int{1, 1, 1, 1, 1, 1}, [6]int{got.FN, got.FP, got.AbstainW, got.AbstainC, got.InfraW, got.InfraC})
	require.Equal(t, 3, got.InfraAll, "uncertain 的 infra 计入技术可靠性")
	require.Equal(t, 4, got.Unstable, "w-fn 2:1、w-inf、c-ab、c-inf；三票无法判定算稳定；uncertain 不计")
	require.Equal(t, []string{"multi", "recall"}, got.FailedScenarios, "recall 错侧失败 2 条，multi 对侧失败 3 条")
	require.Equal(t, 2, got.PerScenario["recall"].ErrFail)
	require.NotContains(t, got.PerScenario, "forget", "uncertain 与无待评答复不进入场景失败计数")
}

// acceptFixture 在临时目录里写一份可通过核对的冻结数据，mutate 可在写 FROZEN.sha256 之前改动文件。
type acceptFixture struct {
	rows  []map[string]any
	pairs []map[string]any
}

const (
	markWrong   = "【演示-错】"
	markAbstain = "【演示-弃】"
	leakToken   = "机密案例说明"
)

func newAcceptFixture(set string) acceptFixture {
	var f acceptFixture
	n := 0
	for _, sc := range liveScenarios {
		for i := range acceptSets[set].PerScenario {
			n++
			label, reply := "对", fmt.Sprintf("这是 %s 场景的第 %d 条答复。", sc.name, i+1)
			id := fmt.Sprintf("hold-%s-%02d", sc.name, i+1)
			if set == "challenge" {
				id = fmt.Sprintf("chal-%03d", n)
				if i%2 == 1 {
					label, reply = "错", reply+markWrong
				}
			} else {
				switch {
				case sc.name == "recall" && i == 0:
					label, reply = "错", reply+markWrong
				case sc.name == "forget" && i == 0:
					label = "uncertain"
				case sc.name == "pinchain" && i == 0:
					label, reply = "no_reply", ""
				}
			}
			f.rows = append(f.rows, map[string]any{"id": id, "scenario": sc.name, "question": sc.question, "expect": sc.expect,
				"reply": reply, "judge_kind": string(sc.kind), "label": label, "note": leakToken})
			if set == "challenge" && i%2 == 1 {
				prev := f.rows[len(f.rows)-2]
				f.pairs = append(f.pairs, map[string]any{"pair": fmt.Sprintf("P%02d", len(f.pairs)+1), "scenario": sc.name,
					"correct_id": prev["id"], "wrong_id": id, "form": leakToken, "failure": leakToken, "expected": map[string]string{"Q1": leakToken}})
			}
		}
	}
	return f
}

func (f acceptFixture) write(t *testing.T, set string, mutate func(dir string)) string {
	t.Helper()
	dir := t.TempDir()
	var lines []string
	for _, r := range f.rows {
		b, err := json.Marshal(r)
		require.NoError(t, err)
		lines = append(lines, string(b))
	}
	spec := acceptSets[set]
	files := map[string][]byte{spec.Data: []byte(strings.Join(lines, "\n") + "\n"), acceptPlanFile: []byte("# 方案\n")}
	items, err := loadLabels(strings.NewReader(string(files[spec.Data])), spec.Format)
	require.NoError(t, err)
	th, err := json.Marshal(thresholdsFor(set, items), json.Indent("  "))
	require.NoError(t, err)
	files[acceptThresholdsFile] = th
	if spec.Pairs != "" {
		pb, err := json.Marshal(f.pairs)
		require.NoError(t, err)
		files[spec.Pairs] = pb
	}
	for name, b := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), b, 0o644))
	}
	if mutate != nil {
		mutate(dir)
	}
	var sums []string
	for name := range files {
		s, err := fileSHA256(filepath.Join(dir, name))
		require.NoError(t, err)
		sums = append(sums, s+"  "+name)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "FROZEN.sha256"), []byte(strings.Join(sums, "\n")+"\n"), 0o644))
	return dir
}

// candidateFakes 用候选配置包装假采样器：按提示词里的背景认出 rubric，按答复里的标记给出错票或无法判定，其余给对票；记下全部提示词。
func candidateFakes(prompts *[]string, mu *sync.Mutex) []runModel {
	sampler := func(_ context.Context, prompt string) (judgeRaw, error) {
		mu.Lock()
		*prompts = append(*prompts, prompt)
		mu.Unlock()
		for _, r := range judgeRubrics {
			if !strings.Contains(prompt, "背景："+r.Background+"\n") {
				continue
			}
			var checks []checkAnswer
			for i, c := range r.Checks {
				ans := c.Want
				switch {
				case strings.Contains(prompt, markWrong) && i == 0:
					ans = map[string]string{"是": "否", "否": "是"}[c.Want]
				case strings.Contains(prompt, markAbstain):
					ans = voteUnknown
				}
				checks = append(checks, checkAnswer{ID: c.ID, Answer: ans})
			}
			b, _ := json.Marshal(map[string]any{"checks": checks})
			return judgeRaw{Content: string(b), Audit: judgeAudit{Requests: 1}}, nil
		}
		return judgeRaw{}, fmt.Errorf("认不出 rubric")
	}
	var models []runModel
	for _, name := range acceptModels {
		models = append(models, runModel{Name: name, Model: judgeModel{Config: judgeModelConfigs[name], Sample: sampler}})
	}
	return models
}

func TestRunAcceptanceOffline(t *testing.T) {
	for _, set := range []string{"challenge", "holdout"} {
		t.Run(set, func(t *testing.T) {
			f := newAcceptFixture(set)
			data := f.write(t, set, nil)
			var prompts []string
			var mu sync.Mutex
			run := filepath.Join(t.TempDir(), "run")
			rep, err := runAcceptance(context.Background(), set, data, run, map[string]string{"examples_commit": "c"}, candidateFakes(&prompts, &mu))
			require.NoError(t, err)

			want := map[string]int{"challenge": 72 * 3 * 2, "holdout": 89 * 3 * 2}[set]
			require.Len(t, prompts, want, "没有待评答复的样本不发送")
			for _, p := range prompts {
				for _, leak := range []string{"hold-", "chal-", leakToken, "P01", "uncertain", "no_reply", "\"label\""} {
					require.NotContains(t, p, leak)
				}
			}
			for _, m := range rep.Models {
				require.Equal(t, acceptPass, m.Verdict, m.Name)
				require.Zero(t, m.Tally.ErrFail+m.Tally.CorrFail)
			}
			if set == "challenge" {
				require.Equal(t, 36, *rep.Models[0].PairsBoth)
			} else {
				require.Equal(t, acceptBase{N: 88, Nc: 87, Ne: 1, Uncertain: 1, NoReply: 1}, acceptBase{N: rep.Thresholds.Base.N,
					Nc: rep.Thresholds.Base.Nc, Ne: rep.Thresholds.Base.Ne, Uncertain: rep.Thresholds.Base.Uncertain, NoReply: rep.Thresholds.Base.NoReply})
				require.Equal(t, map[string]*int{"H1": intp(2), "H2": intp(0), "H3": intp(2), "H4": intp(8)}, rep.Thresholds.Limits)
			}
			for _, name := range []string{"manifest.json", "results.jsonl", "summary.json", "acceptance.json", "acceptance.md"} {
				require.FileExists(t, filepath.Join(run, name))
			}
			man, err := os.ReadFile(filepath.Join(run, "manifest.json"))
			require.NoError(t, err)
			require.Contains(t, string(man), candidateC1.RubricsAndPrompts)
			require.NotContains(t, string(man), leakToken, "清单只记 ID 与哈希，不带配对说明")
		})
	}
}

func TestPrepareAcceptanceRejects(t *testing.T) {
	models := candidateFakes(new([]string), &sync.Mutex{})
	rewrite := func(name string, edit func(string) string) func(string) {
		return func(dir string) {
			p := filepath.Join(dir, name)
			b, err := os.ReadFile(p)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(p, []byte(edit(string(b))), 0o644))
		}
	}
	insufficient := newAcceptFixture("holdout")
	for i := range 10 {
		insufficient.rows[i]["label"], insufficient.rows[i]["reply"] = "no_reply", ""
	}
	tests := []struct {
		name   string
		set    string
		data   func(t *testing.T) string
		models []runModel
		errSub string
	}{
		{"未知数据集", "dev", func(t *testing.T) string { return t.TempDir() }, models, "未知的验收数据"},
		{"冻结后被改", "challenge", func(t *testing.T) string {
			dir := newAcceptFixture("challenge").write(t, "challenge", nil)
			rewrite(acceptPlanFile, func(s string) string { return s + "改" })(dir)
			return dir
		}, models, "SHA256 不符"},
		{"容许数被改", "challenge", func(t *testing.T) string {
			return newAcceptFixture("challenge").write(t, "challenge", rewrite(acceptThresholdsFile, func(s string) string {
				return strings.Replace(s, `"C3": 3`, `"C3": 4`, 1)
			}))
		}, models, "容许数与数据重新计算的结果不一致"},
		{"配对标签不符", "challenge", func(t *testing.T) string {
			f := newAcceptFixture("challenge")
			f.pairs[0]["correct_id"], f.pairs[0]["wrong_id"] = f.pairs[0]["wrong_id"], f.pairs[0]["correct_id"]
			return f.write(t, "challenge", nil)
		}, models, "标签或场景不符"},
		{"场景条数不符", "challenge", func(t *testing.T) string {
			f := newAcceptFixture("challenge")
			f.rows = f.rows[:71]
			f.pairs = f.pairs[:35]
			return f.write(t, "challenge", nil)
		}, models, "应为 8 条"},
		{"H0 样本不足", "holdout", func(t *testing.T) string { return insufficient.write(t, "holdout", nil) }, models, "H0 不满足"},
		{"模型配置不是候选", "holdout", func(t *testing.T) string { return newAcceptFixture("holdout").write(t, "holdout", nil) },
			[]runModel{{Name: "deepseek", Model: fakeModel(nil)}}, "配置哈希不符"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := prepareAcceptance(tt.set, tt.data(t), tt.models)
			require.ErrorContains(t, err, tt.errSub)
		})
	}
}

func TestEvaluateAcceptance(t *testing.T) {
	f := newAcceptFixture("challenge")
	data := f.write(t, "challenge", nil)
	in, err := prepareAcceptance("challenge", data, candidateFakes(new([]string), &sync.Mutex{}))
	require.NoError(t, err)
	recsWith := func(model string, override map[string]judgeItemResult) []runRecord {
		var recs []runRecord
		for _, it := range in.Phase.Items {
			res, ok := override[it.In.ID]
			if !ok {
				res = judgeItemResult{Final: it.Ref, Votes: votes3(it.Ref)}
			}
			res.ID = it.In.ID
			recs = append(recs, runRecord{Model: model, Result: res})
		}
		return recs
	}
	wrongIDs := []string{}
	for _, p := range in.Pairs {
		wrongIDs = append(wrongIDs, p.Wrong)
	}
	missed := func(n int) map[string]judgeItemResult {
		o := map[string]judgeItemResult{}
		// 每个场景最多漏 1 条，避免 C4 先于 C1 触发。
		for i := 0; i < n; i++ {
			o[wrongIDs[i*4]] = judgeItemResult{Final: refCorrect, Votes: votes3(refCorrect)}
		}
		return o
	}
	both := func(n int) map[string]judgeItemResult {
		o := missed(n)
		for i := 0; i < n; i++ {
			o[in.Pairs[i*4+3].Correct] = judgeItemResult{Final: refWrong, Votes: votes3(refWrong)}
		}
		return o
	}
	tests := []struct {
		name    string
		recs    []runRecord
		verdict string
		failed  []string
	}{
		{"全对", recsWith("a", nil), acceptPass, nil},
		{"漏 2 条在边界内", recsWith("a", missed(2)), acceptPass, nil},
		{"漏 3 条只有 C1 未达标", recsWith("a", missed(3)), acceptFail, []string{"C1"}},
		{"两侧各 2 条只有 C3 未达标", recsWith("a", both(2)), acceptFail, []string{"C3"}},
		{"一条没有终态即未完成", recsWith("a", map[string]judgeItemResult{wrongIDs[0]: {}}), acceptIncomplete, nil},
		{"infra 是终态，计入失败但不使模型未完成", recsWith("a", map[string]judgeItemResult{wrongIDs[0]: {Final: verdictInfra}}), acceptPass, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep := evaluateAcceptance(in, runSummary{Models: []runModelSummary{{Name: "a", Status: runComplete}}}, tt.recs)
			m := rep.Models[0]
			require.Equal(t, tt.verdict, m.Verdict)
			var failed []string
			for _, c := range m.Criteria {
				if c.Pass != nil && !*c.Pass {
					failed = append(failed, c.ID)
				}
			}
			require.Equal(t, tt.failed, failed)
			if tt.verdict == acceptIncomplete {
				require.Nil(t, m.Tally, "未完成时不给语义指标")
				require.Equal(t, []string{wrongIDs[0]}, m.NoTerminal)
			}
		})
	}
}
