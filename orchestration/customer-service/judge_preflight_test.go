package main

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 小批真实预检：只验证真实接口的技术链路（请求被接受、结束原因、用量、耗时、解析与 quote 校验），
// 不计算任何一致性指标，结果不并入正式评审。只有设置 CS_JUDGE_PREFLIGHT_DIR 时才运行。

// preflightPlan 是用户确认的固定范围：样本、顺序、模型、预算都不在运行中改动。
var preflightPlan = struct {
	IDs    []string
	Models []string
	Budget time.Duration
}{
	IDs:    []string{"calib-forget-01", "calib-update-04", "calib-premise-10", "calib-temporal-03", "calib-recall-02"},
	Models: []string{"deepseek", "gemini"},
	Budget: 30 * time.Minute,
}

type preflightManifest struct {
	CreatedAt  string             `json:"created_at"`
	Purpose    string             `json:"purpose"`
	Code       map[string]string  `json:"code"`
	Versions   map[string]string  `json:"versions"`
	Source     map[string]string  `json:"source"`
	ItemOrder  []string           `json:"item_order"`
	ModelOrder []string           `json:"model_order"`
	Models     []runModelManifest `json:"models"`
	Plan       map[string]any     `json:"plan"`
	StopRules  map[string]any     `json:"stop_rules"`
	Exclusions []string           `json:"exclusions"`
}

// runPreflight 是只有一段 calibration 样本、不保留参考标签的批次。
func runPreflight(ctx context.Context, dir string, man preflightManifest, models []runModel, items []refItem, budget time.Duration) (runSummary, error) {
	return runJudgePhases(ctx, dir, man, []runPhase{{Dataset: "calibration", Items: items}}, models, budget, false)
}

func buildPreflightManifest(code map[string]string, sourceSHA string, models []runModel) preflightManifest {
	n := len(preflightPlan.IDs)
	return preflightManifest{
		CreatedAt: time.Now().Format(time.RFC3339),
		Purpose:   "小批真实技术预检：只记录请求是否被接受、结束原因、用量、耗时、解析与 quote 校验；不计算 Accuracy、Recall、F1、κ，不并入正式评审结果。",
		Code:      code,
		Versions: map[string]string{"protocol": "judge-protocol/1", "rubric": judgeRubrics["forget"].Version,
			"prompt": judgePromptVersion, "schema": judgeSchemaVersion, "executor": judgeExecutorVersion, "record": judgeRecordVersion},
		Source:     map[string]string{"file": "judge-calibration.jsonl", "format": "calibration", "sha256": sourceSHA},
		ItemOrder:  preflightPlan.IDs,
		ModelOrder: preflightPlan.Models,
		Models:     modelManifests(models, (1+judgeMaxRetries)*judgePositions*n),
		Plan: map[string]any{
			"positions_per_item":              judgePositions,
			"max_retries_per_position":        judgeMaxRetries,
			"planned_positions_total":         judgePositions * n * len(models),
			"expected_http_requests_no_retry": judgePositions * n * len(models),
			"max_http_attempts_total":         (1 + judgeMaxRetries) * judgePositions * n * len(models),
			"attempt_timeout":                 judgeModelConfigs["deepseek"].Timeout.String(),
			"concurrency":                     1,
			"global_wall_clock_budget":        preflightPlan.Budget.String(),
			"budget_starts":                   "第一次真实生成请求之前开始计时",
		},
		StopRules: map[string]any{
			"blocking_http_status": blockingHTTPStatus,
			"on_blocking":          "该位置按协议用完重试并记录后，停止该模型的剩余预检，不换模型",
			"on_cancel_or_budget":  "标记 interrupted 或 budget_exhausted，保留已完成结果与已消耗尝试，未执行位置照实计数，不自动重启",
			"semantic_retry":       "不因语义答案不理想而重试",
			"no_changes":           "运行中不改 rubric、参数或校验规则",
		},
		Exclusions: []string{"参考标签、样本选择理由、failure_layer、数据集来源 ID 不进入提示词；结果中的参考标签字段置空",
			"不运行全量 Calibration／Validation、Final Holdout 或挑战集"},
	}
}

// TestJudgePreflight 执行真实预检。必须在已跟踪文件全部提交的工作区里运行，并显式给出新的输出目录：
//
//	CS_JUDGE_PREFLIGHT_DIR=<新目录> go test -run TestJudgePreflight -timeout 40m ./orchestration/customer-service/
func TestJudgePreflight(t *testing.T) {
	dir := requireRunDir(t, "CS_JUDGE_PREFLIGHT_DIR")
	code := codeState(t)

	src := filepath.Join(frozenLabelDir(t), "judge-calibration.jsonl")
	sha, err := fileSHA256(src)
	require.NoError(t, err)
	require.Equal(t, frozenLabels[0].sha256, sha, "冻结源文件被改动")
	all, err := loadLabelFile(src, "calibration")
	require.NoError(t, err)
	byID := map[string]refItem{}
	for _, it := range all {
		byID[it.In.ID] = it
	}
	var items []refItem
	for _, id := range preflightPlan.IDs {
		it, ok := byID[id]
		require.True(t, ok, id)
		items = append(items, it)
	}
	models := modelsFromEnv(t, preflightPlan.Models)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sum, err := runPreflight(ctx, dir, buildPreflightManifest(code, sha, models), models, items, preflightPlan.Budget)
	require.NoError(t, err)
	b, _ := json.MarshalIndent(sum, "", "  ")
	t.Logf("预检状态 %s，摘要见 %s\n%s", sum.Status, filepath.Join(dir, "summary.json"), b)
}
