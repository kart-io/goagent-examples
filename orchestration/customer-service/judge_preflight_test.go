package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
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

// preflightBlockingStatus 是鉴权、权限、模型不存在、参数不支持、余额或配额类阻断：该位置按协议用完重试后，停止该模型的剩余预检。
var preflightBlockingStatus = map[int]string{
	400: "请求参数不被接受",
	401: "鉴权失败",
	402: "余额不足",
	403: "无权限",
	404: "模型或接口不存在",
	429: "配额或限流",
}

// 运行状态。
const (
	preflightComplete    = "complete"
	preflightBlocked     = "blocked"
	preflightInterrupted = "interrupted"
	preflightBudget      = "budget_exhausted"
	preflightNotStarted  = "not_started"
)

type preflightModel struct {
	Name  string
	Model judgeModel
	// Endpoint 只记录接口位置，不含凭据。
	Endpoint string
}

type preflightModelManifest struct {
	Name           string           `json:"name"`
	Config         judgeModelConfig `json:"config"`
	ConfigHash     string           `json:"config_hash"`
	Endpoint       string           `json:"endpoint"`
	OmittedParams  []string         `json:"omitted_params"`
	MaxHTTPAttempt int              `json:"max_http_attempts"`
}

type preflightManifest struct {
	CreatedAt  string                   `json:"created_at"`
	Purpose    string                   `json:"purpose"`
	Code       map[string]string        `json:"code"`
	Versions   map[string]string        `json:"versions"`
	Source     map[string]string        `json:"source"`
	ItemOrder  []string                 `json:"item_order"`
	ModelOrder []string                 `json:"model_order"`
	Models     []preflightModelManifest `json:"models"`
	Plan       map[string]any           `json:"plan"`
	StopRules  map[string]any           `json:"stop_rules"`
	Exclusions []string                 `json:"exclusions"`
}

type preflightItemSummary struct {
	ID          string         `json:"id"`
	Final       string         `json:"final,omitempty"`
	Votes       []string       `json:"votes"`
	Agree       bool           `json:"agree"`
	Completed   int            `json:"completed_positions"`
	Failed      int            `json:"failed_positions"`
	Interrupted int            `json:"interrupted_positions"`
	Unexecuted  int            `json:"unexecuted_positions"`
	Attempts    int            `json:"attempts"`
	Requests    int            `json:"http_requests"`
	Failures    map[string]int `json:"failures"` // stage/cause → 次数
}

type preflightModelSummary struct {
	Name        string                 `json:"name"`
	Status      string                 `json:"status"`
	StopReason  string                 `json:"stop_reason,omitempty"`
	Planned     int                    `json:"planned_positions"`
	Completed   int                    `json:"completed_positions"`
	Failed      int                    `json:"failed_positions"`
	Interrupted int                    `json:"interrupted_positions"`
	Unexecuted  int                    `json:"unexecuted_positions"`
	Attempts    int                    `json:"attempts"`
	Requests    int                    `json:"http_requests"`
	Failures    map[string]int         `json:"failures"`
	Items       []preflightItemSummary `json:"items"`
}

type preflightSummary struct {
	Status    string                  `json:"status"`
	StartedAt string                  `json:"started_at"`
	EndedAt   string                  `json:"ended_at"`
	ElapsedMS int64                   `json:"elapsed_ms"`
	Models    []preflightModelSummary `json:"models"`
}

// preflightRecord 是 results.jsonl 的一行：一个模型对一个样本的完整执行链。参考标签已去掉，避免预检结果被拿去算指标。
type preflightRecord struct {
	Model  string          `json:"model"`
	Result judgeItemResult `json:"result"`
	Error  string          `json:"error,omitempty"`
}

// summarizeItem 按采样位置统计：有合法结果为完成，重试用完为失败，被中止时正在进行的为中断，其余为未执行。
// aborted 表示 judgeItem 因中止返回：最后一次尝试若未成功，它所在的位置是被打断的，即使次数已用满也不算失败。
func summarizeItem(res judgeItemResult, aborted bool) preflightItemSummary {
	s := preflightItemSummary{ID: res.ID, Final: res.Final, Votes: res.Votes, Agree: res.Agree, Attempts: len(res.Attempts), Failures: map[string]int{}}
	byPos := map[int][]judgeAttempt{}
	for _, a := range res.Attempts {
		byPos[a.Position] = append(byPos[a.Position], a)
		s.Requests += a.Audit.Requests
		if a.Stage != "" {
			s.Failures[strings.TrimSuffix(a.Stage+"/"+a.Cause, "/")]++
		}
	}
	for pos := 1; pos <= judgePositions; pos++ {
		as := byPos[pos]
		switch {
		case len(as) == 0:
			s.Unexecuted++
		case as[len(as)-1].Stage == "":
			s.Completed++
		case aborted && pos == res.Attempts[len(res.Attempts)-1].Position:
			s.Interrupted++
		case len(as) == 1+judgeMaxRetries:
			s.Failed++
		default:
			s.Interrupted++
		}
	}
	return s
}

func addFailures(dst, src map[string]int) {
	for k, v := range src {
		dst[k] += v
	}
}

// runPreflight 先独占创建目录并写入清单，再按模型顺序、样本顺序串行评审；每完成（或中止）一个样本就追加一行结果。
// 中止、超出预算或阻断时保留已完成的结果与已消耗的尝试，未执行的位置照实计数，最后写 summary.json。
func runPreflight(ctx context.Context, dir string, man preflightManifest, models []preflightModel, items []refItem, budget time.Duration) (preflightSummary, error) {
	if err := os.Mkdir(dir, 0o755); err != nil {
		return preflightSummary{}, fmt.Errorf("预检目录必须是新目录：%w", err)
	}
	mb, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return preflightSummary{}, err
	}
	if err := writeExclusive(filepath.Join(dir, "manifest.json"), append(mb, '\n')); err != nil {
		return preflightSummary{}, err
	}
	results, err := os.OpenFile(filepath.Join(dir, "results.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return preflightSummary{}, err
	}
	defer results.Close()

	start := time.Now()
	bctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	sum := preflightSummary{Status: preflightComplete, StartedAt: start.Format(time.RFC3339)}
	for _, pm := range models {
		ms := preflightModelSummary{Name: pm.Name, Status: preflightComplete, Planned: judgePositions * len(items), Failures: map[string]int{}}
		for _, it := range items {
			if ms.Status != preflightComplete || bctx.Err() != nil {
				if ms.Status == preflightComplete {
					ms.Status, ms.StopReason = stopStatus(bctx), "运行在本模型开始前已中止"
				}
				ms.Items = append(ms.Items, preflightItemSummary{ID: it.In.ID, Unexecuted: judgePositions, Failures: map[string]int{}})
				continue
			}
			res, jerr := judgeItem(bctx, pm.Model, judgeRubrics[it.Scenario], it)
			res.Ref = ""
			rec := preflightRecord{Model: pm.Name, Result: res}
			if jerr != nil {
				rec.Error = jerr.Error()
				ms.Status, ms.StopReason = stopStatus(bctx), fmt.Sprintf("在 %s 处中止：%v", it.In.ID, jerr)
			}
			line, err := json.Marshal(rec)
			if err != nil {
				return sum, err
			}
			if _, err := results.Write(append(line, '\n')); err != nil {
				return sum, err
			}
			if err := results.Sync(); err != nil {
				return sum, err
			}
			is := summarizeItem(res, jerr != nil)
			ms.Items = append(ms.Items, is)
			if res.Final == verdictInfra && len(res.Attempts) > 0 {
				if reason, ok := preflightBlockingStatus[res.Attempts[len(res.Attempts)-1].Audit.HTTPStatus]; ok {
					ms.Status, ms.StopReason = preflightBlocked, fmt.Sprintf("HTTP %d %s", res.Attempts[len(res.Attempts)-1].Audit.HTTPStatus, reason)
				}
			}
		}
		for _, is := range ms.Items {
			ms.Completed += is.Completed
			ms.Failed += is.Failed
			ms.Interrupted += is.Interrupted
			ms.Unexecuted += is.Unexecuted
			ms.Attempts += is.Attempts
			ms.Requests += is.Requests
			addFailures(ms.Failures, is.Failures)
		}
		sum.Models = append(sum.Models, ms)
		if ms.Status == preflightInterrupted || ms.Status == preflightBudget {
			sum.Status = ms.Status
		} else if ms.Status == preflightBlocked && sum.Status == preflightComplete {
			sum.Status = preflightBlocked
		}
	}
	end := time.Now()
	sum.EndedAt, sum.ElapsedMS = end.Format(time.RFC3339), end.Sub(start).Milliseconds()
	sb, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return sum, err
	}
	return sum, writeExclusive(filepath.Join(dir, "summary.json"), append(sb, '\n'))
}

// stopStatus 区分预算耗尽与外部中断；两者都不能算完整完成。
func stopStatus(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return preflightBudget
	}
	return preflightInterrupted
}

func writeExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// gitState 返回 HEAD 与已跟踪文件是否有未提交改动。
func gitState(dir string) (head string, dirty bool, err error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", false, err
	}
	st, err := exec.Command("git", "-C", dir, "status", "--porcelain", "--untracked-files=no").Output()
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(string(out)), len(strings.TrimSpace(string(st))) > 0, nil
}

func fileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

// omittedParams 是各提供方的评审适配器明确不发送的参数。
var omittedParams = map[string][]string{
	"deepseek": {"temperature（思考模式下不生效）", "presence_penalty", "frequency_penalty"},
	"gemini":   {"temperature", "topP", "topK（本代模型已弃用并忽略）", "thinkingBudget（改用 thinkingLevel）"},
}

func buildPreflightManifest(code map[string]string, sourceSHA string, models []preflightModel) preflightManifest {
	man := preflightManifest{
		CreatedAt: time.Now().Format(time.RFC3339),
		Purpose:   "小批真实技术预检：只记录请求是否被接受、结束原因、用量、耗时、解析与 quote 校验；不计算 Accuracy、Recall、F1、κ，不并入正式评审结果。",
		Code:      code,
		Versions: map[string]string{"protocol": "judge-protocol/1", "rubric": judgeRubrics["forget"].Version,
			"prompt": judgePromptVersion, "schema": judgeSchemaVersion, "executor": judgeExecutorVersion},
		Source:     map[string]string{"file": "judge-calibration.jsonl", "format": "calibration", "sha256": sourceSHA},
		ItemOrder:  preflightPlan.IDs,
		ModelOrder: preflightPlan.Models,
		Plan: map[string]any{
			"positions_per_item":              judgePositions,
			"max_retries_per_position":        judgeMaxRetries,
			"planned_positions_total":         judgePositions * len(preflightPlan.IDs) * len(models),
			"expected_http_requests_no_retry": judgePositions * len(preflightPlan.IDs) * len(models),
			"max_http_attempts_total":         (1 + judgeMaxRetries) * judgePositions * len(preflightPlan.IDs) * len(models),
			"attempt_timeout":                 judgeModelConfigs["deepseek"].Timeout.String(),
			"concurrency":                     1,
			"global_wall_clock_budget":        preflightPlan.Budget.String(),
			"budget_starts":                   "第一次真实生成请求之前开始计时",
		},
		StopRules: map[string]any{
			"blocking_http_status": preflightBlockingStatus,
			"on_blocking":          "该位置按协议用完重试并记录后，停止该模型的剩余预检，不换模型",
			"on_cancel_or_budget":  "标记 interrupted 或 budget_exhausted，保留已完成结果与已消耗尝试，未执行位置照实计数，不自动重启",
			"semantic_retry":       "不因语义答案不理想而重试",
			"no_changes":           "运行中不改 rubric、参数或校验规则",
		},
		Exclusions: []string{"参考标签、样本选择理由、failure_layer、数据集来源 ID 不进入提示词；结果中的参考标签字段置空",
			"不运行全量 Calibration／Validation、Final Holdout 或挑战集"},
	}
	for _, pm := range models {
		man.Models = append(man.Models, preflightModelManifest{Name: pm.Name, Config: pm.Model.Config, ConfigHash: pm.Model.Config.hash(),
			Endpoint: pm.Endpoint, OmittedParams: omittedParams[pm.Model.Config.Provider], MaxHTTPAttempt: (1 + judgeMaxRetries) * judgePositions * len(preflightPlan.IDs)})
	}
	return man
}

// TestJudgePreflight 执行真实预检。必须在已跟踪文件全部提交的工作区里运行，并显式给出新的输出目录：
//
//	CS_JUDGE_PREFLIGHT_DIR=<新目录> go test -run TestJudgePreflight -timeout 40m ./orchestration/customer-service/
func TestJudgePreflight(t *testing.T) {
	dir := os.Getenv("CS_JUDGE_PREFLIGHT_DIR")
	if dir == "" {
		t.Skip("设置 CS_JUDGE_PREFLIGHT_DIR 才运行真实预检")
	}
	_, err := os.Stat(dir)
	require.True(t, os.IsNotExist(err), "预检目录必须不存在，不覆盖已有结果")

	repo, err := filepath.Abs("../..")
	require.NoError(t, err)
	head, dirty, err := gitState(repo)
	require.NoError(t, err)
	require.False(t, dirty, "已跟踪文件有未提交改动，清单无法对应到确切提交")
	code := map[string]string{"examples_commit": head, "go_version": runtime.Version()}
	for _, mod := range []string{"../goagent", "../goagent-contrib-mem-qdrant"} {
		if h, d, err := gitState(filepath.Join(repo, mod)); err == nil {
			code[strings.TrimPrefix(mod, "../")+"_commit"] = fmt.Sprintf("%s（已跟踪文件有改动：%v）", h, d)
		}
	}

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

	endpoints := map[string]string{"deepseek": "DEEPSEEK_BASE_URL 或默认 https://api.deepseek.com/v1，POST /chat/completions（流式）",
		"gemini": "genai SDK 默认 Gemini API 地址，v1beta models/{model}:streamGenerateContent?alt=sse"}
	var models []preflightModel
	for _, name := range preflightPlan.Models {
		m, err := judgeModelFromEnv(name)
		require.NoError(t, err)
		models = append(models, preflightModel{Name: name, Model: m, Endpoint: endpoints[name]})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sum, err := runPreflight(ctx, dir, buildPreflightManifest(code, sha, models), models, items, preflightPlan.Budget)
	require.NoError(t, err)
	b, _ := json.MarshalIndent(sum, "", "  ")
	t.Logf("预检状态 %s，摘要见 %s\n%s", sum.Status, filepath.Join(dir, "summary.json"), b)
}
