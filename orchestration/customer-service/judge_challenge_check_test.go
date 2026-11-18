package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kart-io/goagent/codec/json"

	"github.com/stretchr/testify/require"
)

// challengeDraft 是挑战集草稿的一行：答复连同预期的逐检查项答案与引文，只用于构造一致性检查，不进入评审输入。
type challengeDraft struct {
	Case     string               `json:"case"`
	Side     string               `json:"side"` // correct 或 wrong
	Scenario string               `json:"scenario"`
	Label    string               `json:"label"`
	Reply    string               `json:"reply"`
	Expected map[string][2]string `json:"expected"` // 检查项 ID → {答案, quote}
}

type corpusReply struct {
	Source string `json:"source"`
	ID     string `json:"id"`
	Reply  string `json:"reply"`
}

func readJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for n := 1; sc.Scan(); n++ {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v, json.Strict); err != nil {
			return nil, fmt.Errorf("%s 第 %d 行：%w", filepath.Base(path), n, err)
		}
		out = append(out, v)
	}
	return out, sc.Err()
}

// checkChallengeDraft 做构造一致性检查与逐字去重，返回全部问题；空切片表示通过。
// 构造一致性只说明预期答案、引文与 rubric/4 的口径一致：引文按 executor/5 的规则（parseChecks → matchQuote）必须能匹配，
// 预期答案按单票规则必须得到该条标签。它不是独立金标准。
func checkChallengeDraft(rows []challengeDraft, corpus []corpusReply) []string {
	var probs []string
	seen := map[string]string{}
	for _, c := range corpus {
		seen[strings.TrimSpace(c.Reply)] = c.Source + "/" + c.ID
	}
	for _, dc := range rubricDevCases {
		seen[strings.TrimSpace(dc.reply)] = "rubricDevCases/" + dc.id
	}
	sides := map[string]map[string]bool{}
	perScenario := map[string]int{}
	for _, row := range rows {
		key := row.Case + "/" + row.Side
		r, ok := judgeRubrics[row.Scenario]
		if !ok {
			probs = append(probs, key+"：未知场景 "+row.Scenario)
			continue
		}
		want := map[string]string{"correct": refCorrect, "wrong": refWrong}[row.Side]
		if want == "" || row.Label != want {
			probs = append(probs, fmt.Sprintf("%s：side 与 label 不一致（%s，%s）", key, row.Side, row.Label))
		}
		if sides[row.Case] == nil {
			sides[row.Case] = map[string]bool{}
		}
		if sides[row.Case][row.Side] {
			probs = append(probs, key+"：重复")
		}
		sides[row.Case][row.Side] = true
		perScenario[row.Scenario]++

		var body struct {
			Checks []checkAnswer `json:"checks"`
		}
		for _, c := range r.Checks {
			a := row.Expected[c.ID]
			body.Checks = append(body.Checks, checkAnswer{ID: c.ID, Answer: a[0], Quote: a[1]})
		}
		if len(row.Expected) != len(r.Checks) {
			probs = append(probs, key+"：预期答案的检查项数与 rubric 不符")
		}
		answers, _, err := parseChecks(mustJSON(body), r, row.Reply)
		if err != nil {
			probs = append(probs, key+"：预期答案或引文未通过输出校验："+err.Error())
		} else if v := sampleVote(r, answers); v != row.Label {
			probs = append(probs, fmt.Sprintf("%s：预期答案按单票规则得到 %s，标签是 %s", key, v, row.Label))
		}

		reply := strings.TrimSpace(row.Reply)
		if src, dup := seen[reply]; dup {
			probs = append(probs, key+"：与已有答复逐字相同（"+src+"），须重写")
		}
		seen[reply] = "challenge/" + key
	}
	for c, s := range sides {
		if !s["correct"] || !s["wrong"] {
			probs = append(probs, c+"：缺少正确或错误答复")
		}
	}
	for name := range judgeRubrics {
		if perScenario[name] != 8 {
			probs = append(probs, fmt.Sprintf("场景 %s 有 %d 条，应为 8 条", name, perScenario[name]))
		}
	}
	return probs
}

// TestChallengeConstruction 对 CS_CHALLENGE_DRAFT 目录下的 draft.jsonl 做构造一致性检查与去重；
// corpus.jsonl 是开发数据与 Holdout 的全部答复。未设置时跳过。
func TestChallengeConstruction(t *testing.T) {
	dir := os.Getenv("CS_CHALLENGE_DRAFT")
	if dir == "" {
		t.Skip("设置 CS_CHALLENGE_DRAFT 以检查挑战集草稿")
	}
	rows, err := readJSONL[challengeDraft](filepath.Join(dir, "draft.jsonl"))
	require.NoError(t, err)
	corpus, err := readJSONL[corpusReply](filepath.Join(dir, "corpus.jsonl"))
	require.NoError(t, err)
	require.Len(t, rows, 72)
	probs := checkChallengeDraft(rows, corpus)
	require.Empty(t, probs, strings.Join(probs, "\n"))
	t.Logf("72 条通过构造一致性检查与去重，比较语料 %d 条（另加 rubricDevCases %d 条）", len(corpus), len(rubricDevCases))
}

func TestCheckChallengeDraft(t *testing.T) {
	ok := func(side, label, reply string) challengeDraft {
		return challengeDraft{Case: "temporal-1", Side: side, Scenario: "temporal", Label: label, Reply: reply,
			Expected: map[string][2]string{"Q1": {map[string]string{refCorrect: "是", refWrong: "否"}[label], ""}}}
	}
	good := []challengeDraft{ok("correct", refCorrect, "最早是耳机断连。"), ok("wrong", refWrong, "最早是充电器发烫。")}
	tests := []struct {
		name   string
		mutate func([]challengeDraft) []challengeDraft
		corpus []corpusReply
		want   string
	}{
		{"一致", func(r []challengeDraft) []challengeDraft { return r }, nil, ""},
		{"引文不是原文", func(r []challengeDraft) []challengeDraft {
			r[0].Expected = map[string][2]string{"Q1": {"是", "最早反馈的是耳机断连"}}
			return r
		}, nil, "未通过输出校验"},
		{"预期答案与标签不符", func(r []challengeDraft) []challengeDraft {
			r[1].Expected = map[string][2]string{"Q1": {"是", ""}}
			return r
		}, nil, "单票规则得到 对"},
		{"与开发答复相同", func(r []challengeDraft) []challengeDraft { return r },
			[]corpusReply{{Source: "validation", ID: "val-1", Reply: " 最早是耳机断连。\n"}}, "validation/val-1"},
		{"与开发用例相同", func(r []challengeDraft) []challengeDraft {
			r[1].Reply = rubricDevCases[0].reply
			r[1].Expected = map[string][2]string{"Q1": {"否", ""}}
			return r
		}, nil, "rubricDevCases/"},
		{"挑战集内重复", func(r []challengeDraft) []challengeDraft {
			r[1].Case = "temporal-2"
			return append(r, ok("wrong", refWrong, "最早是耳机断连。"))
		}, nil, "challenge/temporal-1/correct"},
		{"side 与标签不符", func(r []challengeDraft) []challengeDraft {
			r[0].Side = "wrong"
			return r
		}, nil, "side 与 label 不一致"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := tt.mutate(append([]challengeDraft(nil), good...))
			probs := strings.Join(checkChallengeDraft(rows, tt.corpus), "\n")
			// 测试数据只覆盖 temporal 一个场景，场景条数的问题与本用例无关。
			var rest []string
			for _, p := range strings.Split(probs, "\n") {
				if p != "" && !strings.HasPrefix(p, "场景 ") {
					rest = append(rest, p)
				}
			}
			if tt.want == "" {
				require.Empty(t, rest)
				return
			}
			require.Contains(t, strings.Join(rest, "\n"), tt.want)
		})
	}
}
