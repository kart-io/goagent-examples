package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func preflightItems(n int) []refItem {
	var items []refItem
	for i := range n {
		it := forgetItem(refCorrect)
		it.In.ID = "p" + string(rune('1'+i))
		it.In.Reply = "我这边没有记录"
		items = append(items, it)
	}
	return items
}

// stepSampler 每次调用记一次请求，并交给 step 决定返回什么。
func stepSampler(calls *int, step func(ctx context.Context, n int) (judgeRaw, error)) judgeSampler {
	return func(ctx context.Context, _ string) (judgeRaw, error) {
		*calls++
		raw, err := step(ctx, *calls)
		raw.Audit.Requests = 1
		return raw, err
	}
}

func okStep(context.Context, int) (judgeRaw, error) { return judgeRaw{Content: forgetPassJSON}, nil }

func statusStep(code int) func(context.Context, int) (judgeRaw, error) {
	return func(context.Context, int) (judgeRaw, error) {
		return judgeRaw{Audit: judgeAudit{HTTPStatus: code}}, errors.New("HTTP 错误")
	}
}

func blockUntilDone(ctx context.Context, _ int) (judgeRaw, error) {
	<-ctx.Done()
	return judgeRaw{}, ctx.Err()
}

func readPreflight(t *testing.T, dir string) (runSummary, []runRecord) {
	t.Helper()
	var sum runSummary
	b, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &sum))
	b, err = os.ReadFile(filepath.Join(dir, "results.jsonl"))
	require.NoError(t, err)
	var recs []runRecord
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var r runRecord
		require.NoError(t, json.Unmarshal([]byte(line), &r))
		recs = append(recs, r)
	}
	return sum, recs
}

type preflightCounts struct {
	Status                                     string
	Completed, Failed, Interrupted, Unexecuted int
	Requests                                   int
}

func countsOf(m runModelSummary) preflightCounts {
	return preflightCounts{m.Status, m.Completed, m.Failed, m.Interrupted, m.Unexecuted, m.Requests}
}

func TestRunPreflight(t *testing.T) {
	tests := []struct {
		name       string
		stepA      func(context.Context, int) (judgeRaw, error)
		stepB      func(context.Context, int) (judgeRaw, error)
		budget     time.Duration
		cancelAt   int // 模型 A 第几次调用时取消父 context；0 表示不取消
		wantStatus string
		wantA      preflightCounts
		wantB      preflightCounts
		wantReason string
		wantRecs   int
	}{
		{name: "全部完成", stepA: okStep, stepB: okStep, budget: time.Minute, wantStatus: runComplete,
			wantA: preflightCounts{runComplete, 6, 0, 0, 0, 6}, wantB: preflightCounts{runComplete, 6, 0, 0, 0, 6}, wantRecs: 4},
		{name: "鉴权失败只停该模型", stepA: statusStep(401), stepB: okStep, budget: time.Minute, wantStatus: runBlocked,
			wantA: preflightCounts{runBlocked, 0, 1, 0, 5, 3}, wantB: preflightCounts{runComplete, 6, 0, 0, 0, 6}, wantReason: "HTTP 401", wantRecs: 3},
		{name: "服务端 503 不算阻断", stepA: statusStep(503), stepB: okStep, budget: time.Minute, wantStatus: runComplete,
			wantA: preflightCounts{runComplete, 0, 2, 0, 4, 6}, wantB: preflightCounts{runComplete, 6, 0, 0, 0, 6}, wantRecs: 4},
		{name: "预算耗尽保留已消耗尝试", stepA: blockUntilDone, stepB: okStep, budget: 50 * time.Millisecond, wantStatus: runBudget,
			wantA: preflightCounts{runBudget, 0, 0, 1, 5, 1}, wantB: preflightCounts{runBudget, 0, 0, 0, 6, 0}, wantRecs: 1},
		{name: "父 context 取消记为中断", stepA: okStep, stepB: okStep, budget: time.Minute, cancelAt: 4, wantStatus: runInterrupted,
			wantA: preflightCounts{runInterrupted, 4, 0, 0, 2, 4}, wantB: preflightCounts{runInterrupted, 0, 0, 0, 6, 0}, wantRecs: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "run")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			manifestSeen := false
			var callsA, callsB int
			stepA := tt.stepA
			a := stepSampler(&callsA, func(c context.Context, n int) (judgeRaw, error) {
				if n == 1 {
					_, err := os.Stat(filepath.Join(dir, "manifest.json"))
					manifestSeen = err == nil
				}
				raw, err := stepA(c, n)
				if n == tt.cancelAt {
					cancel()
				}
				return raw, err
			})
			b := stepSampler(&callsB, tt.stepB)
			models := []runModel{{Name: "a", Model: fakeModel(a)}, {Name: "b", Model: fakeModel(b)}}
			items := preflightItems(2)

			sum, err := runPreflight(ctx, dir, buildPreflightManifest(map[string]string{"examples_commit": "test"}, "sha", models), models, items, tt.budget)
			require.NoError(t, err)
			require.True(t, manifestSeen, "第一次请求前清单必须已落盘")
			require.Equal(t, tt.wantStatus, sum.Status)
			require.Equal(t, tt.wantA, countsOf(sum.Models[0]))
			require.Equal(t, tt.wantB, countsOf(sum.Models[1]))
			require.Equal(t, callsA, sum.Models[0].Requests)
			require.Equal(t, callsB, sum.Models[1].Requests)
			for _, m := range sum.Models {
				require.Equal(t, m.Planned, m.Completed+m.Failed+m.Interrupted+m.Unexecuted, "计划位置必须全部有去向")
				require.Equal(t, 6, m.Planned)
			}
			require.Contains(t, sum.Models[0].StopReason, tt.wantReason)

			disk, recs := readPreflight(t, dir)
			require.Equal(t, sum.Status, disk.Status)
			require.Len(t, recs, tt.wantRecs)
			for _, r := range recs {
				require.Empty(t, r.Result.Ref, "参考标签不能写入预检结果")
			}
		})
	}
}

// 计数规则：中止时最后一个未成功的位置算中断，即使已用满三次尝试。
func TestSummarizeItem(t *testing.T) {
	ok := func(pos, n int) judgeAttempt { return judgeAttempt{Position: pos, Attempt: n} }
	bad := func(pos, n int) judgeAttempt {
		return judgeAttempt{Position: pos, Attempt: n, Stage: stageCall, Cause: causeTimeout}
	}
	tests := []struct {
		name     string
		attempts []judgeAttempt
		aborted  bool
		want     [4]int // 完成、失败、中断、未执行
	}{
		{"未开始", nil, true, [4]int{0, 0, 0, 3}},
		{"重试后完成", []judgeAttempt{ok(1, 1), bad(2, 1), ok(2, 2), ok(3, 1)}, false, [4]int{3, 0, 0, 0}},
		{"位置耗尽", []judgeAttempt{ok(1, 1), bad(2, 1), bad(2, 2), bad(2, 3)}, false, [4]int{1, 1, 0, 1}},
		{"第三次尝试被中止", []judgeAttempt{bad(1, 1), bad(1, 2), bad(1, 3)}, true, [4]int{0, 0, 1, 2}},
		{"成功后被中止", []judgeAttempt{ok(1, 1), ok(2, 1)}, true, [4]int{2, 0, 0, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := summarizeItem(judgeItemResult{Attempts: tt.attempts}, tt.aborted)
			require.Equal(t, tt.want, [4]int{s.Completed, s.Failed, s.Interrupted, s.Unexecuted})
			require.Equal(t, len(tt.attempts), s.Attempts)
		})
	}
}

// 已有目录（哪怕是空的）也拒绝运行，且不发出任何请求。
func TestRunPreflightRefusesExistingDir(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	models := []runModel{{Name: "a", Model: fakeModel(stepSampler(&calls, okStep))}}
	_, err := runPreflight(context.Background(), dir, preflightManifest{}, models, preflightItems(1), time.Minute)
	require.Error(t, err)
	require.Zero(t, calls)
}

func TestPreflightManifest(t *testing.T) {
	ds, err := newJudgeModel(judgeModelConfigs["deepseek"], "http://127.0.0.1:1", "k", nil)
	require.NoError(t, err)
	gm, err := newJudgeModel(judgeModelConfigs["gemini"], "http://127.0.0.1:1", "k", nil)
	require.NoError(t, err)
	models := []runModel{{Name: "deepseek", Model: ds}, {Name: "gemini", Model: gm}}
	man := buildPreflightManifest(map[string]string{"examples_commit": "c"}, "s", models)

	require.Equal(t, []string{"calib-forget-01", "calib-update-04", "calib-premise-10", "calib-temporal-03", "calib-recall-02"}, man.ItemOrder)
	require.Equal(t, map[string]string{"protocol": "judge-protocol/1", "rubric": "rubric/3", "prompt": judgePromptVersion,
		"schema": judgeSchemaVersion, "executor": "judge-executor/4", "record": "judge-record/2"}, man.Versions)
	require.Equal(t, 30, man.Plan["planned_positions_total"])
	require.Equal(t, 30, man.Plan["expected_http_requests_no_retry"])
	require.Equal(t, 90, man.Plan["max_http_attempts_total"])
	require.Equal(t, "30m0s", man.Plan["global_wall_clock_budget"])
	require.Equal(t, "2m0s", man.Plan["attempt_timeout"])
	for _, m := range man.Models {
		require.Equal(t, 45, m.MaxHTTPAttempt)
		require.Equal(t, judgeModelConfigs[m.Name].hash(), m.ConfigHash)
	}
	b, err := json.Marshal(man)
	require.NoError(t, err)
	require.NotContains(t, string(b), `"k"`, "清单不能带凭据")
}

// 两段批次：阻断状态跨数据集保持，被阻断模型在后续数据集的样本全部未执行；另一模型照常完成。正式批次保留参考标签供统计。
func TestRunJudgePhasesAcrossDatasets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	var callsA, callsB int
	models := []runModel{{Name: "a", Model: fakeModel(stepSampler(&callsA, statusStep(403)))}, {Name: "b", Model: fakeModel(stepSampler(&callsB, okStep))}}
	val := preflightItems(2)
	val[1].Ref = refUncertain
	phases := []runPhase{{Dataset: "calibration", Items: preflightItems(2)}, {Dataset: "validation", Items: val}}

	sum, err := runJudgePhases(context.Background(), dir, map[string]string{"k": "v"}, phases, models, time.Minute, true)
	require.NoError(t, err)
	require.Equal(t, runBlocked, sum.Status)
	require.Equal(t, preflightCounts{runBlocked, 0, 1, 0, 11, 3}, countsOf(sum.Models[0]))
	require.Equal(t, preflightCounts{runComplete, 12, 0, 0, 0, 12}, countsOf(sum.Models[1]))
	require.Equal(t, 6, sum.Models[0].ByDataset["validation"].Unexecuted)
	require.Equal(t, 6, sum.Models[1].ByDataset["validation"].Completed)

	_, recs := readPreflight(t, dir)
	require.Len(t, recs, 5)
	var got []string
	for _, r := range recs {
		if r.Model == "b" {
			got = append(got, r.Dataset+"/"+r.Group+"/"+r.Result.Ref)
		}
	}
	require.Equal(t, []string{"calibration/scored/对", "calibration/scored/对", "validation/scored/对", "validation/uncertain/uncertain"}, got, "正式批次保留参考标签")
}

// 不合规的批次在创建目录、发请求之前就被拒绝。
func TestRunJudgePhasesRejects(t *testing.T) {
	noReply := preflightItems(1)
	noReply[0].Ref = refNoReply
	badScenario := preflightItems(1)
	badScenario[0].Scenario = "nope"
	tests := []struct {
		name        string
		items       []refItem
		concurrency int
		want        string
	}{
		{"没有待评答复", noReply, 1, "没有待评答复"},
		{"没有 rubric", badScenario, 1, "没有 rubric"},
		{"并发不为 1", preflightItems(1), 2, "并发"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "run")
			calls := 0
			m := fakeModel(stepSampler(&calls, okStep))
			m.Config.Concurrency = tt.concurrency
			_, err := runJudgePhases(context.Background(), dir, nil, []runPhase{{Dataset: "d", Items: tt.items}}, []runModel{{Name: "a", Model: m}}, time.Minute, true)
			require.ErrorContains(t, err, tt.want)
			require.Zero(t, calls)
			_, statErr := os.Stat(dir)
			require.True(t, os.IsNotExist(statErr))
		})
	}
}

func TestVerifyFrozen(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte("x\n"), 0o644))
	sum, err := fileSHA256(filepath.Join(dir, "a.jsonl"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "FROZEN.sha256"), []byte(sum+"  a.jsonl\n"), 0o644))
	got, err := verifyFrozen(dir)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"a.jsonl": sum}, got)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte("y\n"), 0o644))
	_, err = verifyFrozen(dir)
	require.ErrorContains(t, err, "不符")
}

// 用真实冻结数据核对开发评审的范围与预算：804 个计划位置、最多 2412 次 HTTP 尝试。
func TestDevRunManifest(t *testing.T) {
	labels := frozenLabelDir(t)
	frozen, err := verifyFrozen(labels)
	require.NoError(t, err)
	phases, skipped, err := devRunPhases(labels)
	require.NoError(t, err)
	ds, err := newJudgeModel(judgeModelConfigs["deepseek"], "http://127.0.0.1:1", "k", nil)
	require.NoError(t, err)
	gm, err := newJudgeModel(judgeModelConfigs["gemini"], "http://127.0.0.1:1", "k", nil)
	require.NoError(t, err)
	models := []runModel{{Name: "deepseek", Model: ds}, {Name: "gemini", Model: gm}}
	man := buildDevRunManifest(map[string]string{"examples_commit": "c"}, frozen, phases, skipped, models)

	require.Equal(t, []string{"calibration", "validation"}, []string{phases[0].Dataset, phases[1].Dataset})
	require.Len(t, man.Datasets[0].Scored, 45)
	require.Empty(t, man.Datasets[0].Uncertain)
	require.Len(t, man.Datasets[1].Scored, 87)
	require.Equal(t, []string{"val-forget-01", "val-recall-06"}, man.Datasets[1].Uncertain)
	require.Equal(t, []string{"val-pinchain-06"}, man.Datasets[1].Skipped)
	require.Equal(t, frozenLabels[0].sha256, man.Datasets[0].SHA256)
	require.Equal(t, frozenLabels[1].sha256, man.Datasets[1].SHA256)
	require.Equal(t, "e93cd7e2f6be29135b5b4f07dfe117a6305bd2c87d22cc22617cdfb05448acd2", man.Frozen["protocol-v1.md"])
	for k, v := range map[string]int{"planned_positions_per_model": 402, "planned_positions_total": 804, "expected_http_requests_no_retry": 804,
		"max_http_attempts_per_model": 1206, "max_http_attempts_total": 2412, "max_output_tokens": 8192} {
		require.Equal(t, v, man.Plan[k], k)
	}
	require.Equal(t, "1h30m0s", man.Plan["global_wall_clock_budget"])
	require.Equal(t, map[string]string{"protocol": "judge-protocol/1", "rubric": "rubric/3", "prompt": "checklist-prompt/2",
		"schema": "checks-schema/2", "executor": "judge-executor/4", "record": "judge-record/2"}, man.Versions)
	for _, m := range man.Models {
		require.Equal(t, 1206, m.MaxHTTPAttempt)
		require.Equal(t, judgeModelConfigs[m.Name].hash(), m.ConfigHash)
	}
	b, err := json.Marshal(man)
	require.NoError(t, err)
	require.NotContains(t, string(b), `"k"`, "清单不能带凭据")
}
