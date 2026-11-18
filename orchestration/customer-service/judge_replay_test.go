package main

import (
	"github.com/kart-io/goagent/codec/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 离线回放：用当前执行器的输出校验重新解析某次运行存下的模型原始输出，只比较每次尝试的校验结果，不重算票、不改判样本，
// 也不发任何模型请求。原运行目录只读，回放结果写进新目录。只有同时设置 CS_JUDGE_REPLAY_SRC 与 CS_JUDGE_REPLAY_DIR 时运行。

type replayAttempt struct {
	Dataset   string       `json:"dataset"`
	Model     string       `json:"model"`
	ID        string       `json:"id"`
	Position  int          `json:"position"`
	Attempt   int          `json:"attempt"`
	OldStage  string       `json:"old_stage"`
	OldCause  string       `json:"old_cause"`
	NewStage  string       `json:"new_stage"`
	NewCause  string       `json:"new_cause"`
	NewError  string       `json:"new_error,omitempty"`
	NewQuotes []quoteMatch `json:"new_quote_matches,omitempty"`
}

// replayParse 只回放有模型输出、且旧记录不是调用阶段失败的尝试：调用失败没有可解析的完整输出。
func replayParse(rec runRecord, items map[string]refItem) ([]replayAttempt, error) {
	it, ok := items[rec.Dataset+"|"+rec.Result.ID]
	if !ok {
		return nil, fmt.Errorf("冻结数据中没有 %s/%s", rec.Dataset, rec.Result.ID)
	}
	r := judgeRubrics[it.Scenario]
	var out []replayAttempt
	for _, a := range rec.Result.Attempts {
		if a.Stage == stageCall {
			continue
		}
		ra := replayAttempt{Dataset: rec.Dataset, Model: rec.Model, ID: rec.Result.ID, Position: a.Position, Attempt: a.Attempt, OldStage: a.Stage, OldCause: a.Cause}
		_, quotes, err := parseChecks(a.Content, r, it.In.Reply)
		var se *stageError
		if errors.As(err, &se) {
			ra.NewStage, ra.NewCause, ra.NewError = se.stage, se.cause, err.Error()
		}
		ra.NewQuotes = quotes
		out = append(out, ra)
	}
	return out, nil
}

func TestJudgeQuoteReplay(t *testing.T) {
	src, dst := os.Getenv("CS_JUDGE_REPLAY_SRC"), os.Getenv("CS_JUDGE_REPLAY_DIR")
	if src == "" || dst == "" {
		t.Skip("设置 CS_JUDGE_REPLAY_SRC=<原运行目录> 与 CS_JUDGE_REPLAY_DIR=<新目录> 才回放")
	}
	recs, err := loadRunRecords(src)
	require.NoError(t, err)
	srcResults, err := fileSHA256(filepath.Join(src, "results.jsonl"))
	require.NoError(t, err)
	phases, _, err := devRunPhases(frozenLabelDir(t))
	require.NoError(t, err)
	items := map[string]refItem{}
	for _, ph := range phases {
		for _, it := range ph.Items {
			items[ph.Dataset+"|"+it.In.ID] = it
		}
	}

	type key struct{ Model, Old, New, Method string }
	counts := map[key]int{}
	var lines []byte
	for _, rec := range recs {
		ras, err := replayParse(rec, items)
		require.NoError(t, err)
		for _, ra := range ras {
			if ra.OldStage == "" {
				require.Empty(t, ra.NewStage, "旧版接受的输出在新版也必须接受：%s/%s 位置 %d", ra.Model, ra.ID, ra.Position)
				for _, q := range ra.NewQuotes {
					require.Equal(t, quoteExact, q.Method, "旧版接受的引文都应逐字匹配")
				}
			}
			method := quoteExact
			for _, q := range ra.NewQuotes {
				if q.Method == quoteBold {
					method = quoteBold
				}
			}
			if ra.NewStage != "" {
				method = ""
			}
			counts[key{ra.Model, ra.OldStage + "/" + ra.OldCause, ra.NewStage + "/" + ra.NewCause, method}]++
			b, err := json.Marshal(ra)
			require.NoError(t, err)
			lines = append(append(lines, b...), '\n')
		}
	}
	require.NoError(t, os.Mkdir(dst, 0o755))
	require.NoError(t, writeExclusive(filepath.Join(dst, "replay.jsonl"), lines))
	var table []map[string]any
	for k, n := range counts {
		table = append(table, map[string]any{"model": k.Model, "old": k.Old, "new": k.New, "quote_method": k.Method, "attempts": n})
	}
	sum, err := json.Marshal(map[string]any{
		"created_at":     time.Now().Format(time.RFC3339),
		"purpose":        "只回放输出校验，不重算票、不改判样本、不发模型请求；新版结果另行通过开发回归记录",
		"source_dir":     src,
		"source_results": srcResults,
		"versions":       map[string]string{"executor": judgeExecutorVersion, "record": judgeRecordVersion, "schema": judgeSchemaVersion},
		"code":           codeState(t),
		"counts":         table,
	}, json.Indent("  "))
	require.NoError(t, err)
	require.NoError(t, writeExclusive(filepath.Join(dst, "summary.json"), sum))
	t.Logf("%s", sum)
}
