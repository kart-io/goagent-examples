package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent/memory"
)

type judgeKind string

const (
	judgeGeneric  judgeKind = "generic"
	judgeUpdate   judgeKind = "update"
	judgeTemporal judgeKind = "temporal"
	judgeAbstain  judgeKind = "abstain"
)

const judgeFormat = "只输出判定：第一行只写“对”或“错”，第二行用一句话给出理由。"

// judgeCriteria 是各题型的中文判定口径，改写自 memeval.JudgePrompt。
var judgeCriteria = map[judgeKind]string{
	judgeGeneric:  "如果答复包含标准答案，判“对”；与标准答案等价，或给出了得到标准答案的全部中间步骤，也判“对”；只包含答案所需信息的一部分，判“错”。",
	judgeUpdate:   "如果答复包含标准答案，判“对”；答复中同时提到旧信息与更新后的答案时，只要以更新后的答案为准，仍判“对”；否则判“错”。",
	judgeTemporal: "如果答复包含标准答案，判“对”；与标准答案等价或给出全部中间步骤也判“对”；只含一部分信息判“错”。若问题问的是天数、周数、月数等，答复相差一天不扣分（例如标准答案 18 天而答复 19 天，仍判“对”）。",
	judgeAbstain:  "这是一道无法回答的问题，说明栏写明了原因。如果答复正确指出无法回答（例如说信息不全、已有信息与所问不符），或请对方提供所需信息，判“对”；若答复编造了具体答案，判“错”。",
}

func judgePrompt(kind judgeKind, question, expect, reply string) string {
	criteria, ok := judgeCriteria[kind]
	if !ok {
		criteria = judgeCriteria[judgeGeneric]
	}
	expectLabel := "标准答案"
	if kind == judgeAbstain {
		expectLabel = "说明"
	}
	return fmt.Sprintf("下面给出一个问题、%s和一段模型答复，请判断答复是否正确。\n%s\n%s\n\n问题：%s\n\n%s：%s\n\n模型答复：%s",
		expectLabel, criteria, judgeFormat, question, expectLabel, expect, reply)
}

var verdictWords = map[string]bool{"对": true, "正确": true, "是": true, "错": false, "错误": false, "否": false}

// parseVerdict 取首个非空行开头的连续文字作判定词，其余内容拼成一行理由。
func parseVerdict(out string) (bool, string, error) {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		runes := []rune(strings.TrimLeftFunc(line, func(r rune) bool { return !unicode.IsLetter(r) }))
		n := 0
		for n < len(runes) && unicode.IsLetter(runes[n]) {
			n++
		}
		word := string(runes[:n])
		ok, known := verdictWords[word]
		if !known {
			return false, "", fmt.Errorf("无法解析评审判定: %q", line)
		}
		rest := append([]string{strings.TrimSpace(strings.TrimLeftFunc(string(runes[n:]), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }))}, lines[i+1:]...)
		var parts []string
		for _, s := range rest {
			if s = strings.TrimSpace(s); s != "" {
				parts = append(parts, s)
			}
		}
		return ok, strings.Join(parts, " "), nil
	}
	return false, "", fmt.Errorf("评审输出为空")
}

type scenarioResult struct {
	Scenario    string           `json:"scenario"`
	Repeat      int              `json:"repeat"`
	Customers   []string         `json:"customers"`
	Tickets     []ticketEvidence `json:"tickets"`
	Question    string           `json:"question"`
	Expect      string           `json:"expect"`
	Reply       string           `json:"reply"`
	RecallBlock string           `json:"recall_block"`
	Records     []memory.Record  `json:"records"`
	StateOK     bool             `json:"state_ok"`
	StateNote   string           `json:"state_note"`
	Invariant   bool             `json:"invariant"`
	JudgeOK     bool             `json:"judge_ok"`
	JudgeReason string           `json:"judge_reason"`
	Infra       string           `json:"infra"`
	ModelCalls  int64            `json:"model_calls"`
	Seconds     float64          `json:"seconds"`
}

type scenarioRow struct {
	Scenario                                 string
	N, Infra, StatePass, JudgePass, BothPass int
	Lo, Hi                                   float64
}

// wilson 与 memory/memeval 的同名函数同式（z=1.96）；n=0 时返回 0, 0。
func wilson(k, n float64) (float64, float64) {
	if n == 0 {
		return 0, 0
	}
	const z = 1.96
	p := k / n
	d := 1 + z*z/n
	c := p + z*z/(2*n)
	h := z * math.Sqrt(p*(1-p)/n+z*z/(4*n*n))
	return max((c-h)/d, 0), min((c+h)/d, 1)
}

func summarizeScenarios(rs []scenarioResult) []scenarioRow {
	byName := map[string]*scenarioRow{}
	for _, r := range rs {
		row := byName[r.Scenario]
		if row == nil {
			row = &scenarioRow{Scenario: r.Scenario}
			byName[r.Scenario] = row
		}
		if r.Infra != "" {
			row.Infra++
			continue
		}
		row.N++
		if r.StateOK {
			row.StatePass++
		}
		if r.JudgeOK {
			row.JudgePass++
		}
		if r.StateOK && r.JudgeOK {
			row.BothPass++
		}
	}
	rows := make([]scenarioRow, 0, len(byName))
	for _, row := range byName {
		row.Lo, row.Hi = wilson(float64(row.BothPass), float64(row.N))
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Scenario < rows[j].Scenario })
	return rows
}

func scenarioMarkdown(rows []scenarioRow) string {
	var b strings.Builder
	b.WriteString("| 场景 | 有效次数 | infra | 状态通过 | 评审通过 | 两层通过 | 通过率 | 95% 区间 |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		rate := 0.0
		if r.N > 0 {
			rate = float64(r.BothPass) / float64(r.N)
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %.1f%% | %.1f%% - %.1f%% |\n",
			r.Scenario, r.N, r.Infra, r.StatePass, r.JudgePass, r.BothPass, rate*100, r.Lo*100, r.Hi*100)
	}
	return b.String()
}

func scenarioOutDir() (string, error) {
	base := os.Getenv("CS_SCENARIO_OUT")
	if base == "" {
		base = "/tmp/cs-scenarios"
	}
	dir := filepath.Join(base, time.Now().Format("20060102-150405"))
	return dir, os.MkdirAll(dir, 0o755)
}

func TestJudgePrompt(t *testing.T) {
	tests := []struct {
		kind judgeKind
		want []string
	}{
		{judgeGeneric, []string{"包含标准答案"}},
		{judgeUpdate, []string{"以更新后的答案为准"}},
		{judgeTemporal, []string{"相差一天不扣分"}},
		{judgeAbstain, []string{"无法回答", "请对方提供"}},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			got := judgePrompt(tt.kind, "问题甲", "答案乙", "答复丙")
			for _, w := range append(tt.want, "问题甲", "答案乙", "答复丙", "第一行只写“对”或“错”") {
				require.Contains(t, got, w)
			}
		})
	}
}

func TestParseVerdict(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		ok         bool
		wantErr    bool
		wantReason string
	}{
		{"对加理由", "对\n答复给出了上海浦东", true, false, "答复给出了上海浦东"},
		{"错带句号", "错。\n答复仍说北京", false, false, "答复仍说北京"},
		{"正确同行理由", "  正确：理由", true, false, "理由"},
		{"否", "否", false, false, ""},
		{"前导空行", "\n\n错误\n理由一\n理由二", false, false, "理由一 理由二"},
		{"无法判定", "我认为……", false, true, ""},
		{"空", "", false, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason, err := parseVerdict(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.ok, ok)
			require.Equal(t, tt.wantReason, reason)
		})
	}
}

func TestScenarioSummary(t *testing.T) {
	rs := []scenarioResult{
		{Scenario: "update", StateOK: true, JudgeOK: true},
		{Scenario: "update", StateOK: true, JudgeOK: false},
		{Scenario: "update", Infra: "deepseek: 429"},
		{Scenario: "abstain", StateOK: true, JudgeOK: true},
	}
	rows := summarizeScenarios(rs)
	require.Len(t, rows, 2)
	require.Equal(t, "abstain", rows[0].Scenario)
	u := rows[1]
	require.Equal(t, "update", u.Scenario)
	require.Equal(t, []int{2, 1, 2, 1, 1}, []int{u.N, u.Infra, u.StatePass, u.JudgePass, u.BothPass})
	require.InDelta(t, 0.0945, u.Lo, 0.001)
	require.InDelta(t, 0.9055, u.Hi, 0.001)

	empty := summarizeScenarios([]scenarioResult{{Scenario: "x", Infra: "e"}})
	require.Equal(t, []float64{0, 0}, []float64{empty[0].Lo, empty[0].Hi})
}

func TestScenarioMarkdown(t *testing.T) {
	md := scenarioMarkdown([]scenarioRow{{Scenario: "update", N: 2, Infra: 1, StatePass: 2, JudgePass: 1, BothPass: 1, Lo: 0.0945, Hi: 0.9055}})
	require.Contains(t, md, "| 场景 | 有效次数 | infra | 状态通过 | 评审通过 | 两层通过 | 通过率 | 95% 区间 |")
	require.Contains(t, md, "| update | 2 | 1 | 2 | 1 | 1 | 50.0% | 9.4% - 90.5% |")
}

func TestScenarioOutDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("CS_SCENARIO_OUT", base)
	dir, err := scenarioOutDir()
	require.NoError(t, err)
	require.Equal(t, base, filepath.Dir(dir))
	require.Regexp(t, `^\d{8}-\d{6}$`, filepath.Base(dir))
	st, err := os.Stat(dir)
	require.NoError(t, err)
	require.True(t, st.IsDir())
}
