package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var testRubric = judgeRubric{
	Version:    "test/1",
	Background: "客户此前要求删除备用电话。",
	Checks: []judgeCheck{
		{ID: "F1", Ask: "答复是否给出了备用电话号码？", Want: "否"},
		{ID: "F2", Ask: "答复是否说明没有记录或已删除？", Want: "是"},
	},
}

func checksJSON(f1, f2 string) string {
	return fmt.Sprintf(`{"checks":[{"id":"F1","quote":"","answer":%q},{"id":"F2","quote":"没有记录","answer":%q}]}`, f1, f2)
}

var (
	outPass    = checksJSON("否", "是")
	outFail    = checksJSON("否", "否")
	outUnknown = checksJSON("否", voteUnknown)
)

// scripted 按调用顺序返回预设输出；以 "ERR:" 开头的条目模拟调用失败。
func scripted(outs ...string) (judgeSampler, *int) {
	calls := 0
	return func(_ context.Context, _ string) (judgeRaw, error) {
		if calls >= len(outs) {
			return judgeRaw{}, errors.New("脚本耗尽")
		}
		o := outs[calls]
		calls++
		if rest, ok := strings.CutPrefix(o, "ERR:"); ok {
			return judgeRaw{}, errors.New(rest)
		}
		return judgeRaw{Content: o, Reasoning: "推理" + o}, nil
	}, &calls
}

func forgetItem(ref string) refItem {
	return refItem{In: judgeInput{ID: "x", Question: "问", Expect: "说明", Reply: "我这边没有记录", Kind: judgeAbstain}, Scenario: "forget", Ref: ref}
}

func TestJudgeItemAggregation(t *testing.T) {
	tests := []struct {
		name       string
		outs       []string
		final      string
		votes      []string
		agree      bool
		calls      int
		infraStage string
	}{
		{"三票都对", []string{outPass, outPass, outPass}, refCorrect, []string{"对", "对", "对"}, true, 3, ""},
		{"两错一对", []string{outFail, outPass, outFail}, refWrong, []string{"错", "对", "错"}, false, 3, ""},
		{"对错无法判定各一票为弃权", []string{outPass, outFail, outUnknown}, verdictAbstain, []string{"对", "错", voteUnknown}, false, 3, ""},
		{"两票无法判定为弃权", []string{outUnknown, outUnknown, outPass}, verdictAbstain, []string{voteUnknown, voteUnknown, "对"}, false, 3, ""},
		{"三票无法判定不重试", []string{outUnknown, outUnknown, outUnknown}, verdictAbstain, []string{voteUnknown, voteUnknown, voteUnknown}, true, 3, ""},
		{"明确不满足压过无法判定", []string{checksJSON("是", voteUnknown), outFail, outPass}, refWrong, []string{"错", "错", "对"}, false, 3, ""},
		{"技术失败重试后成功", []string{"ERR:超时", "", outPass, outPass, outPass}, refCorrect, []string{"对", "对", "对"}, true, 5, ""},
		{"第二个位置重试两次后成功", []string{outFail, "不是 JSON", `{"checks":[]}`, outFail, outPass}, refWrong, []string{"错", "错", "对"}, false, 5, ""},
		{"某位置重试耗尽为 infra", []string{outPass, "ERR:a", "ERR:b", ""}, verdictInfra, nil, false, 4, stageEmpty},
		{"首个位置三次解析失败即停止", []string{"x", "y", "z"}, verdictInfra, nil, false, 3, stageParse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, calls := scripted(tt.outs...)
			res := judgeItem(context.Background(), s, testRubric, forgetItem(refCorrect))
			require.Equal(t, tt.final, res.Final)
			require.Equal(t, tt.votes, res.Votes)
			require.Equal(t, tt.agree, res.Agree)
			require.Equal(t, tt.calls, *calls, "调用次数")
			require.Equal(t, tt.infraStage, res.InfraStage)
			require.Len(t, res.Attempts, tt.calls, "每次尝试（含失败）都留有记录")
			require.Equal(t, judgeVersions{Rubric: "test/1", Prompt: judgePromptVersion, Schema: judgeSchemaVersion, Executor: judgeExecutorVersion}, res.Versions)
			for _, a := range res.Attempts {
				require.LessOrEqual(t, a.Attempt, 1+judgeMaxRetries)
				if a.Stage == "" {
					require.NotEmpty(t, a.Answers)
					require.Equal(t, "推理"+a.Content, a.Reasoning, "保留 reasoning_content")
				}
			}
		})
	}
}

func TestJudgeItemCheckVotes(t *testing.T) {
	s, _ := scripted(outPass, outFail, outPass)
	res := judgeItem(context.Background(), s, testRubric, forgetItem(refCorrect))
	require.Equal(t, map[string]map[string]int{"F1": {"否": 3}, "F2": {"是": 2, "否": 1}}, res.CheckVotes)
	require.Equal(t, 0, res.QuoteMissing)

	s, _ = scripted(outPass, outPass, outPass)
	item := forgetItem(refCorrect)
	item.In.Reply = "答复里没有这段引文"
	require.Equal(t, 3, judgeItem(context.Background(), s, testRubric, item).QuoteMissing, "引文不在答复中只记录，不判失败")
}

// 逐项多数两项都满足，但每票按整票规则各自合成：(满足,不满足)、(不满足,满足)、(满足,满足) 得错、错、对，最终为错。
// F1 的 Want 为「否」：答「否」是满足，答「是」才是不满足。
func TestJudgeItemWholeVoteMajority(t *testing.T) {
	s, _ := scripted(checksJSON("否", "否"), checksJSON("是", "是"), outPass)
	res := judgeItem(context.Background(), s, testRubric, forgetItem(refWrong))
	require.Equal(t, []string{refWrong, refWrong, refCorrect}, res.Votes)
	require.Equal(t, refWrong, res.Final)
	require.Equal(t, map[string]map[string]int{"F1": {"否": 2, "是": 1}, "F2": {"否": 1, "是": 2}}, res.CheckVotes, "逐项多数只作诊断")
	require.False(t, res.Agree)
}

func TestJudgeSamplesShareNoContext(t *testing.T) {
	var prompts []string
	s := func(_ context.Context, p string) (judgeRaw, error) {
		prompts = append(prompts, p)
		return judgeRaw{Content: outPass}, nil
	}
	judgeItem(context.Background(), s, testRubric, forgetItem(refCorrect))
	require.Len(t, prompts, 3)
	for _, p := range prompts {
		require.Equal(t, prompts[0], p, "三次采样拿到相同的独立提示词，没有前一次的输出")
		require.NotContains(t, p, "推理")
	}
}

func TestParseChecks(t *testing.T) {
	tests := []struct {
		name, out, stage string
	}{
		{"合法", outPass, ""},
		{"代码围栏", "```json\n" + outPass + "\n```", ""},
		{"无法判定合法", outUnknown, ""},
		{"空", "  \n", stageEmpty},
		{"不是 JSON", "对", stageParse},
		{"尾随内容", outPass + " {}", stageParse},
		{"未知字段", `{"checks":[],"verdict":"对"}`, stageParse},
		{"缺少检查项", `{"checks":[{"id":"F1","quote":"","answer":"否"}]}`, stageValidate},
		{"重复检查项", `{"checks":[{"id":"F1","quote":"","answer":"否"},{"id":"F1","quote":"","answer":"否"},{"id":"F2","quote":"","answer":"是"}]}`, stageValidate},
		{"未知检查项", `{"checks":[{"id":"F1","quote":"","answer":"否"},{"id":"F2","quote":"","answer":"是"},{"id":"F9","quote":"","answer":"是"}]}`, stageValidate},
		{"非法答案", checksJSON("否", "对"), stageValidate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseChecks(tt.out, testRubric)
			if tt.stage == "" {
				require.NoError(t, err)
				require.Len(t, got, 2)
				return
			}
			var se *stageError
			require.True(t, errors.As(err, &se), "%v", err)
			require.Equal(t, tt.stage, se.stage)
		})
	}
}

func TestChecklistPrompt(t *testing.T) {
	in := judgeInput{ID: "val-forget-01", Question: "问题甲", Expect: "说明乙", Reply: "答复丙", Kind: judgeAbstain}
	p := checklistPrompt(testRubric, in)
	for _, w := range []string{testRubric.Background, "问题甲", "说明乙", "答复丙", "F1", testRubric.Checks[0].Ask, "F2", testRubric.Checks[1].Ask, "无法判定", `"checks"`} {
		require.Contains(t, p, w)
	}
	require.NotContains(t, p, "val-forget-01", "样本 ID 不进入提示词")
	require.NotContains(t, p, "Want")
}

// 每个非白名单字段都填哨兵值，加载后构造的提示词里一个也不能出现。
func TestJudgeInputIsolation(t *testing.T) {
	forget := liveScenarios[len(liveScenarios)-2]
	require.Equal(t, "forget", forget.name)
	tests := []struct {
		format, line string
	}{
		{"calibration", `{"id":"calib-forget-01","source_run":"SENTINEL_RUN","scenario":"forget","repeat":1,"question":%q,"expect":%q,"reply":"我这边没有记录","auto_state_ok":true,"auto_judge_ok":false,"human_verdict":"SENTINEL_HUMAN","category":"SENTINEL_CATEGORY","failure_layer":"SENTINEL_LAYER","reply_label":"错","evidence":["SENTINEL_EVIDENCE"],"reason":"SENTINEL_REASON"}`},
		{"validation", `{"id":"val-forget-01","source_run":"SENTINEL_RUN","scenario":"forget","repeat":1,"judge_kind":"abstain","question":%q,"expect":%q,"reply":"我这边没有记录","label":"uncertain","evidence":["SENTINEL_EVIDENCE"],"reason":"SENTINEL_REASON","label_method":"SENTINEL_METHOD","old_judge_event":{"note":"SENTINEL_OLD_JUDGE"},"judge_reason":"SENTINEL_JUDGE_REASON"}`},
	}
	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			items, err := loadLabels(strings.NewReader(fmt.Sprintf(tt.line, forget.question, forget.expect)), tt.format)
			require.NoError(t, err)
			require.Len(t, items, 1)
			p := checklistPrompt(testRubric, items[0].In)
			require.NotContains(t, p, "SENTINEL")
			for _, leak := range []string{"uncertain", "错", "abstain", "forget", "calib-", "val-"} {
				require.NotContains(t, p, leak)
			}
		})
	}
}

func TestLoadLabelsErrors(t *testing.T) {
	sc := liveScenarios[0] // recall
	row := func(extra string) string {
		return fmt.Sprintf(`{"id":"a","scenario":"recall","question":%q,"expect":%q,"reply":"答复"%s}`, sc.question, sc.expect, extra)
	}
	tests := []struct {
		name, format, body, want string
	}{
		{"重复 ID", "calibration", row(`,"reply_label":"对"`) + "\n" + row(`,"reply_label":"错"`), "重复"},
		{"非法标签", "calibration", row(`,"reply_label":"uncertain"`), "非法标签"},
		{"缺标签", "calibration", row(""), "非法标签"},
		{"judge_kind 不符", "validation", row(`,"judge_kind":"update","label":"对"`), "judge_kind"},
		{"有答复却标 infra", "validation", row(`,"judge_kind":"generic","label":"infra"`), "答复是否为空"},
		{"场景未知", "calibration", `{"id":"a","scenario":"nope","question":"q","expect":"e","reply":"r","reply_label":"对"}`, "未知场景"},
		{"问题与场景定义不符", "calibration", `{"id":"a","scenario":"recall","question":"别的问题","expect":"e","reply":"r","reply_label":"对"}`, "不一致"},
		{"缺 reply", "calibration", fmt.Sprintf(`{"id":"a","scenario":"recall","question":%q,"expect":%q,"reply_label":"对"}`, sc.question, sc.expect), "缺少"},
		{"未知格式", "holdout", row(`,"label":"对"`), "未知"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadLabels(strings.NewReader(tt.body), tt.format)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

// 冻结基线（judge-protocol/1，2026-11-18）；加载前先核对 SHA256，防止误用被改动的标签。
var frozenLabels = []struct {
	file, format, sha256 string
	n                    int
	refs                 map[string]int
}{
	{"judge-calibration.jsonl", "calibration", "ea8c5f7cd0b478675ae4ce907d0b9265b0706ad86e838fd81164f4eb1679392e", 45,
		map[string]int{refCorrect: 28, refWrong: 17}},
	{"judge-validation.jsonl", "validation", "c6f9ce0f9c56bd626c271480daf7d4d3b6413620519be02c9a6f22f64009de54", 90,
		map[string]int{refCorrect: 86, refWrong: 1, refUncertain: 2, refNoReply: 1}},
}

func frozenLabelDir(t *testing.T) string {
	dir := os.Getenv("CS_JUDGE_LABELS")
	if dir == "" {
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		dir = filepath.Join(home, "cs-eval-archive", "judge-labels-2026-11-18")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("冻结标签目录不可用（%v）；设置 CS_JUDGE_LABELS 指定位置", err)
	}
	return dir
}

func TestLoadFrozenLabels(t *testing.T) {
	dir := frozenLabelDir(t)
	for _, fl := range frozenLabels {
		t.Run(fl.format, func(t *testing.T) {
			path := filepath.Join(dir, fl.file)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			sum := sha256.Sum256(data)
			require.Equal(t, fl.sha256, hex.EncodeToString(sum[:]), "冻结文件被改动")
			items, err := loadLabelFile(path, fl.format)
			require.NoError(t, err)
			require.Len(t, items, fl.n)
			got := map[string]int{}
			for _, it := range items {
				got[it.Ref]++
			}
			require.Equal(t, fl.refs, got)
		})
	}
}

// 恒判「对」在 Validation 参考可评集合（对 86、错 1）上的基线，见 protocol-v1.md §5。
func TestJudgeMetricsConstantBaseline(t *testing.T) {
	var rs []judgeItemResult
	for i := 0; i < 86; i++ {
		rs = append(rs, judgeItemResult{Ref: refCorrect, Final: refCorrect})
	}
	rs = append(rs,
		judgeItemResult{Ref: refWrong, Final: refCorrect},
		judgeItemResult{Ref: refUncertain, Final: refWrong},
		judgeItemResult{Ref: refUncertain, Final: verdictAbstain},
		judgeItemResult{Ref: refNoReply},
	)
	m := computeJudgeMetrics(rs)
	require.Equal(t, 87, m.N, "uncertain 与无待评答复不进入参考可评集合")
	require.Equal(t, ratio{K: 86, N: 87}, stripCI(m.Accuracy))
	require.InDelta(t, 0.9885, float64(m.Accuracy.K)/float64(m.Accuracy.N), 1e-4)
	require.Equal(t, ratio{K: 0, N: 1}, stripCI(m.Recall))
	require.True(t, m.Precision.NA, "没有判错时精确率为 N/A")
	require.NotNil(t, m.Kappa)
	require.InDelta(t, 0, *m.Kappa, 1e-12)
	require.NotNil(t, m.F1)
	require.Zero(t, *m.F1)
	require.Equal(t, ratio{K: 87, N: 87}, stripCI(m.Coverage))
}

func stripCI(r ratio) ratio { return ratio{K: r.K, N: r.N, NA: r.NA} }

func TestJudgeMetricsDenominators(t *testing.T) {
	rs := []judgeItemResult{
		{Ref: refWrong, Final: refWrong},                         // TP
		{Ref: refWrong, Final: refWrong},                         // TP
		{Ref: refWrong, Final: refCorrect},                       // FN
		{Ref: refWrong, Final: verdictAbstain},                   // 弃权的参考错误
		{Ref: refWrong, Final: verdictInfra, InfraStage: "call"}, // infra 的参考错误
		{Ref: refCorrect, Final: refWrong},                       // FP
		{Ref: refCorrect, Final: refCorrect},                     // TN
		{Ref: refCorrect, Final: refCorrect},                     // TN
		{Ref: refCorrect, Final: refCorrect},                     // TN
		{Ref: refCorrect, Final: verdictInfra, InfraStage: "parse"},
		{Ref: refUncertain, Final: refWrong},
	}
	m := computeJudgeMetrics(rs)
	tests := []struct {
		name string
		got  ratio
		k, n int
	}{
		{"二分类覆盖率", m.Coverage, 7, 10},
		{"弃权率", m.Abstain, 1, 10},
		{"infra 率", m.Infra, 2, 10},
		{"错误召回率", m.Recall, 2, 3},
		{"正确答复误判率", m.FPR, 1, 4},
		{"判错精确率", m.Precision, 2, 3},
		{"Accuracy", m.Accuracy, 5, 7},
		{"端到端错误检出", m.EndToEnd, 2, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.False(t, tt.got.NA)
			require.Equal(t, [2]int{tt.k, tt.n}, [2]int{tt.got.K, tt.got.N})
			p := float64(tt.k) / float64(tt.n)
			require.True(t, tt.got.Lo <= p && p <= tt.got.Hi, "Wilson 区间包含点估计")
		})
	}
	require.Equal(t, map[string]int{"call": 1, "parse": 1}, m.InfraByStage)
	require.InDelta(t, 4.0/6.0, *m.F1, 1e-12, "F1 = 2TP/(2TP+FP+FN)")
	// po = 5/7；pe = (3·3 + 4·4)/49 = 25/49；κ = (35/49 − 25/49)/(24/49) = 10/24。
	require.InDelta(t, 10.0/24.0, *m.Kappa, 1e-12)
}

func TestJudgeMetricsNA(t *testing.T) {
	m := computeJudgeMetrics([]judgeItemResult{{Ref: refWrong, Final: refWrong}, {Ref: refWrong, Final: refWrong}})
	require.True(t, m.FPR.NA, "单类子集没有参考对，误判率为 N/A")
	require.Nil(t, m.Kappa, "p_e = 1 时 κ 为 N/A")
	require.Equal(t, [2]int{2, 2}, [2]int{m.Recall.K, m.Recall.N})

	empty := computeJudgeMetrics(nil)
	require.True(t, empty.Coverage.NA && empty.Accuracy.NA && empty.EndToEnd.NA)
	require.Nil(t, empty.F1)
	require.Nil(t, empty.Kappa)
}

// rubric/1 的离线一致性：覆盖全部场景、检查项 ID 唯一且为中性编号、Want 合法，并按 Want 的极性合成单票。
func TestRubricV1(t *testing.T) {
	require.Len(t, judgeRubrics, len(liveScenarios))
	for _, sc := range liveScenarios {
		t.Run(sc.name, func(t *testing.T) {
			r, ok := judgeRubrics[sc.name]
			require.True(t, ok, "场景缺少 rubric")
			require.Equal(t, "rubric/1", r.Version)
			require.NotEmpty(t, r.Background)
			require.NotEmpty(t, r.Checks)
			satisfied := make([]checkAnswer, len(r.Checks))
			for i, c := range r.Checks {
				require.Equal(t, fmt.Sprintf("Q%d", i+1), c.ID, "检查项按顺序中性编号")
				require.Contains(t, []string{"是", "否"}, c.Want)
				require.NotEmpty(t, c.Ask)
				satisfied[i] = checkAnswer{ID: c.ID, Answer: c.Want}
			}
			require.Equal(t, refCorrect, sampleVote(r, satisfied), "全部满足")
			flip := map[string]string{"是": "否", "否": "是"}
			for i := range r.Checks {
				for _, tt := range []struct{ answer, vote string }{{flip[r.Checks[i].Want], refWrong}, {voteUnknown, voteUnknown}} {
					answers := append([]checkAnswer(nil), satisfied...)
					answers[i].Answer = tt.answer
					require.Equal(t, tt.vote, sampleVote(r, answers), "%s 答 %s", r.Checks[i].ID, tt.answer)
				}
			}
		})
	}
}

// 用全部冻结样本构造 rubric/1 提示词：检查项与答复齐全，样本 ID 与参考标签不进入提示词。
func TestRubricV1FrozenPrompts(t *testing.T) {
	dir := frozenLabelDir(t)
	total := 0
	for _, fl := range frozenLabels {
		items, err := loadLabelFile(filepath.Join(dir, fl.file), fl.format)
		require.NoError(t, err)
		for _, it := range items {
			r := judgeRubrics[it.Scenario]
			p := checklistPrompt(r, it.In)
			require.Contains(t, p, it.In.Reply)
			for _, c := range r.Checks {
				require.Contains(t, p, "- "+c.ID+"："+c.Ask)
			}
			require.NotContains(t, p, it.In.ID)
			for _, leak := range []string{"calib-", "val-", "uncertain", "no_reply", "reply_label", "Want"} {
				require.NotContains(t, p, leak, it.In.ID)
			}
			total++
		}
	}
	require.Equal(t, 135, total)
}
