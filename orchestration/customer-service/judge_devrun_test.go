package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 正式开发评审：在冻结的 Calibration 与 Validation 上用固定候选版本跑一轮，结果只用于开发分析。
// 只有设置 CS_JUDGE_DEVRUN_DIR 时才发真实请求。

// devRunPlan 是用户确认的批次范围：先两模型跑完 Calibration，再两模型跑 Validation；没有待评答复的样本不发送。
var devRunPlan = struct {
	Datasets []struct{ Name, File string }
	Models   []string
	Budget   time.Duration
}{
	Datasets: []struct{ Name, File string }{
		{"calibration", "judge-calibration.jsonl"},
		{"validation", "judge-validation.jsonl"},
	},
	Models: []string{"deepseek", "gemini"},
	Budget: 90 * time.Minute,
}

type devDatasetManifest struct {
	Name      string   `json:"name"`
	File      string   `json:"file"`
	SHA256    string   `json:"sha256"`
	Scored    []string `json:"scored_ids"`
	Uncertain []string `json:"uncertain_ids"`
	Skipped   []string `json:"skipped_no_reply_ids"`
}

type devRunManifest struct {
	CreatedAt  string               `json:"created_at"`
	Purpose    string               `json:"purpose"`
	Code       map[string]string    `json:"code"`
	Versions   map[string]string    `json:"versions"`
	Frozen     map[string]string    `json:"frozen_sha256"`
	Datasets   []devDatasetManifest `json:"datasets"`
	Order      string               `json:"order"`
	ModelOrder []string             `json:"model_order"`
	Models     []runModelManifest   `json:"models"`
	Plan       map[string]any       `json:"plan"`
	StopRules  map[string]any       `json:"stop_rules"`
	Exclusions []string             `json:"exclusions"`
}

// devRunPhases 按文件顺序装配两段样本，没有待评答复的样本单独列出、不进入批次。
func devRunPhases(dir string) ([]runPhase, map[string][]string, error) {
	var phases []runPhase
	skipped := map[string][]string{}
	for _, ds := range devRunPlan.Datasets {
		items, err := loadLabelFile(filepath.Join(dir, ds.File), ds.Name)
		if err != nil {
			return nil, nil, err
		}
		ph := runPhase{Dataset: ds.Name}
		for _, it := range items {
			if it.Ref == refNoReply {
				skipped[ds.Name] = append(skipped[ds.Name], it.In.ID)
				continue
			}
			ph.Items = append(ph.Items, it)
		}
		phases = append(phases, ph)
	}
	return phases, skipped, nil
}

// verifyFrozen 按 FROZEN.sha256 逐个核对冻结目录里的文件，返回文件名 → SHA256。
func verifyFrozen(dir string) (map[string]string, error) {
	f, err := os.Open(filepath.Join(dir, "FROZEN.sha256"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		want, name, ok := strings.Cut(strings.TrimSpace(sc.Text()), "  ")
		if !ok {
			continue
		}
		got, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if got != want {
			return nil, fmt.Errorf("冻结文件 %s 的 SHA256 不符：%s ≠ %s", name, got, want)
		}
		out[name] = got
	}
	return out, sc.Err()
}

func buildDevRunManifest(code, frozen map[string]string, phases []runPhase, skipped map[string][]string, models []runModel) devRunManifest {
	perModel := 0
	var datasets []devDatasetManifest
	for i, ph := range phases {
		d := devDatasetManifest{Name: ph.Dataset, File: devRunPlan.Datasets[i].File, SHA256: frozen[devRunPlan.Datasets[i].File], Skipped: skipped[ph.Dataset]}
		for _, it := range ph.Items {
			if itemGroup(it) == "uncertain" {
				d.Uncertain = append(d.Uncertain, it.In.ID)
			} else {
				d.Scored = append(d.Scored, it.In.ID)
			}
		}
		datasets = append(datasets, d)
		perModel += judgePositions * len(ph.Items)
	}
	maxPerModel := (1 + judgeMaxRetries) * perModel
	return devRunManifest{
		CreatedAt: time.Now().Format(time.RFC3339),
		Purpose:   "固定候选版本的开发评审：结果只用于开发分析，不作为独立测试集结论；两模型的比较对应各自声明的配置，不是推理预算相等的受控比较。",
		Code:      code,
		Versions: map[string]string{"protocol": "judge-protocol/1", "rubric": judgeRubrics["forget"].Version,
			"prompt": judgePromptVersion, "schema": judgeSchemaVersion, "executor": judgeExecutorVersion, "record": judgeRecordVersion},
		Frozen:     frozen,
		Datasets:   datasets,
		Order:      "数据集依次为 calibration、validation；每个数据集内模型依次为 deepseek、gemini；样本按冻结文件顺序；全局并发 1",
		ModelOrder: devRunPlan.Models,
		Models:     modelManifests(models, maxPerModel),
		Plan: map[string]any{
			"positions_per_item":              judgePositions,
			"max_retries_per_position":        judgeMaxRetries,
			"planned_positions_per_model":     perModel,
			"planned_positions_total":         perModel * len(models),
			"expected_http_requests_no_retry": perModel * len(models),
			"max_http_attempts_per_model":     maxPerModel,
			"max_http_attempts_total":         maxPerModel * len(models),
			"attempt_timeout":                 judgeModelConfigs["deepseek"].Timeout.String(),
			"max_output_tokens":               judgeModelConfigs["deepseek"].MaxOutputTokens,
			"concurrency":                     1,
			"global_wall_clock_budget":        devRunPlan.Budget.String(),
			"budget_starts":                   "第一次真实生成请求之前开始计时；独立于预检，不沿用预检计数",
		},
		StopRules: map[string]any{
			"blocking_http_status": blockingHTTPStatus,
			"on_blocking":          "该位置按协议用完重试并记录后，停止该模型剩余的全部样本（含后续数据集），另一模型继续",
			"on_cancel_or_budget":  "标记 interrupted 或 budget_exhausted，保留已完成、失败、中断、未执行状态，不自动重启、不重置预算",
			"semantic_retry":       "合法的无法判定、票型不一致或结果不理想都不重试",
			"exhausted_position":   "任一位置技术重试耗尽，该样本记为 infra，不用剩余票补成多数",
			"no_changes":           "批次内不改 rubric、参数或校验规则",
		},
		Exclusions: []string{"参考标签、样本选择理由、failure_layer、数据集来源 ID 不进入提示词；参考标签只在结果里供统计",
			"uncertain 保留原参考标签，只记录三票与聚合，不进入语义指标",
			"不复用预检的票；不运行 Final Holdout 或挑战集"},
	}
}

// TestJudgeDevRun 执行正式开发评审。必须在已跟踪文件全部提交的工作区里运行：
//
//	CS_JUDGE_DEVRUN_DIR=<新目录> go test -run TestJudgeDevRun -timeout 100m ./orchestration/customer-service/
func TestJudgeDevRun(t *testing.T) {
	dir := requireRunDir(t, "CS_JUDGE_DEVRUN_DIR")
	code := codeState(t)
	labels := frozenLabelDir(t)
	frozen, err := verifyFrozen(labels)
	require.NoError(t, err)
	require.Equal(t, frozenLabels[0].sha256, frozen[frozenLabels[0].file])
	require.Equal(t, frozenLabels[1].sha256, frozen[frozenLabels[1].file])
	require.Contains(t, frozen, "protocol-v1.md")
	phases, skipped, err := devRunPhases(labels)
	require.NoError(t, err)
	models := modelsFromEnv(t, devRunPlan.Models)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sum, err := runJudgePhases(ctx, dir, buildDevRunManifest(code, frozen, phases, skipped, models), phases, models, devRunPlan.Budget, true)
	require.NoError(t, err)
	b, _ := json.MarshalIndent(sum.Models, "", "  ")
	t.Logf("开发评审状态 %s，摘要见 %s\n%.4000s", sum.Status, filepath.Join(dir, "summary.json"), b)
}
