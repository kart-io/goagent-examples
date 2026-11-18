package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/kart-io/goagent/codec/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 真实评审批次的通用执行器，小批预检与开发评审共用：先独占创建目录并写清单，再按「数据集 → 模型 → 样本」的固定顺序串行评审，
// 每个样本结束（或被中止）就追加一行结果；阻断、中止、超出预算时保留已完成结果与已消耗尝试，未执行位置照实计数。

// blockingHTTPStatus 是鉴权、权限、模型不存在、参数不支持、余额或配额类阻断：该位置按协议用完重试后，停止该模型剩余的全部样本。
var blockingHTTPStatus = map[int]string{
	400: "请求参数不被接受",
	401: "鉴权失败",
	402: "余额不足",
	403: "无权限",
	404: "模型或接口不存在",
	429: "配额或限流",
}

// 运行状态。
const (
	runComplete    = "complete"
	runBlocked     = "blocked"
	runInterrupted = "interrupted"
	runBudget      = "budget_exhausted"
)

type runModel struct {
	Name  string
	Model judgeModel
	// Endpoint 只记录接口位置，不含凭据。
	Endpoint string
}

// runPhase 是批次中的一段：同一数据集的样本依次交给每个模型。
type runPhase struct {
	Dataset string
	Items   []refItem
}

type runModelManifest struct {
	Name           string           `json:"name"`
	Config         judgeModelConfig `json:"config"`
	ConfigHash     string           `json:"config_hash"`
	Endpoint       string           `json:"endpoint"`
	OmittedParams  []string         `json:"omitted_params"`
	MaxHTTPAttempt int              `json:"max_http_attempts"`
}

type runItemSummary struct {
	Dataset     string         `json:"dataset"`
	Group       string         `json:"group"`
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

// runCounts 是按采样位置的去向统计；Planned 恒等于其余四项之和。
type runCounts struct {
	Planned     int            `json:"planned_positions"`
	Completed   int            `json:"completed_positions"`
	Failed      int            `json:"failed_positions"`
	Interrupted int            `json:"interrupted_positions"`
	Unexecuted  int            `json:"unexecuted_positions"`
	Attempts    int            `json:"attempts"`
	Requests    int            `json:"http_requests"`
	Failures    map[string]int `json:"failures"`
}

func (c *runCounts) add(is runItemSummary) {
	if c.Failures == nil {
		c.Failures = map[string]int{}
	}
	c.Planned += judgePositions
	c.Completed += is.Completed
	c.Failed += is.Failed
	c.Interrupted += is.Interrupted
	c.Unexecuted += is.Unexecuted
	c.Attempts += is.Attempts
	c.Requests += is.Requests
	for k, v := range is.Failures {
		c.Failures[k] += v
	}
}

type runModelSummary struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	StopReason string `json:"stop_reason,omitempty"`
	runCounts
	ByDataset map[string]*runCounts `json:"by_dataset"`
	Items     []runItemSummary      `json:"items"`
}

type runSummary struct {
	Status    string            `json:"status"`
	StartedAt string            `json:"started_at"`
	EndedAt   string            `json:"ended_at"`
	ElapsedMS int64             `json:"elapsed_ms"`
	Models    []runModelSummary `json:"models"`
}

// runRecord 是 results.jsonl 的一行：一个模型对一个样本的完整执行链。
type runRecord struct {
	Dataset string          `json:"dataset"`
	Group   string          `json:"group"` // scored 进入指标；uncertain 只记票型
	Model   string          `json:"model"`
	Result  judgeItemResult `json:"result"`
	Error   string          `json:"error,omitempty"`
}

func itemGroup(it refItem) string {
	if it.Ref == refUncertain {
		return "uncertain"
	}
	return "scored"
}

// summarizeItem 按采样位置统计：有合法结果为完成，重试用完为失败，被中止时正在进行的为中断，其余为未执行。
// aborted 表示 judgeItem 因中止返回：最后一次尝试若未成功，它所在的位置是被打断的，即使次数已用满也不算失败。
func summarizeItem(res judgeItemResult, aborted bool) runItemSummary {
	s := runItemSummary{ID: res.ID, Final: res.Final, Votes: res.Votes, Agree: res.Agree, Attempts: len(res.Attempts), Failures: map[string]int{}}
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

// runJudgePhases 执行一个批次。keepRef 为假时结果里的参考标签置空（预检不算指标）；参考标签任何时候都不进入提示词。
func runJudgePhases(ctx context.Context, dir string, manifest any, phases []runPhase, models []runModel, budget time.Duration, keepRef bool) (runSummary, error) {
	for _, pm := range models {
		if pm.Model.Config.Concurrency != 1 {
			return runSummary{}, fmt.Errorf("%s 的评审并发须为 1，配置为 %d", pm.Name, pm.Model.Config.Concurrency)
		}
	}
	for _, ph := range phases {
		for _, it := range ph.Items {
			if it.Ref == refNoReply {
				return runSummary{}, fmt.Errorf("%s 没有待评答复，不能进入批次", it.In.ID)
			}
			if _, ok := judgeRubrics[it.Scenario]; !ok {
				return runSummary{}, fmt.Errorf("%s: 场景 %q 没有 rubric", it.In.ID, it.Scenario)
			}
		}
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return runSummary{}, fmt.Errorf("运行目录必须是新目录：%w", err)
	}
	mb, err := json.Marshal(manifest, json.Indent("  "))
	if err != nil {
		return runSummary{}, err
	}
	if err := writeExclusive(filepath.Join(dir, "manifest.json"), append(mb, '\n')); err != nil {
		return runSummary{}, err
	}
	results, err := os.OpenFile(filepath.Join(dir, "results.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return runSummary{}, err
	}
	defer results.Close()

	start := time.Now()
	bctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	ms := make([]runModelSummary, len(models))
	for i, pm := range models {
		ms[i] = runModelSummary{Name: pm.Name, Status: runComplete, ByDataset: map[string]*runCounts{}}
	}
	record := func(i int, ph runPhase, it refItem, is runItemSummary) {
		is.Dataset, is.Group, is.ID = ph.Dataset, itemGroup(it), it.In.ID
		if ms[i].ByDataset[ph.Dataset] == nil {
			ms[i].ByDataset[ph.Dataset] = &runCounts{}
		}
		ms[i].ByDataset[ph.Dataset].add(is)
		ms[i].runCounts.add(is)
		ms[i].Items = append(ms[i].Items, is)
	}
	for _, ph := range phases {
		for i, pm := range models {
			for _, it := range ph.Items {
				if ms[i].Status != runComplete || bctx.Err() != nil {
					if ms[i].Status == runComplete {
						ms[i].Status, ms[i].StopReason = stopStatus(bctx), "运行在本模型的剩余样本开始前已中止"
					}
					record(i, ph, it, runItemSummary{Unexecuted: judgePositions, Failures: map[string]int{}})
					continue
				}
				res, jerr := judgeItem(bctx, pm.Model, judgeRubrics[it.Scenario], it)
				if !keepRef {
					res.Ref = ""
				}
				rec := runRecord{Dataset: ph.Dataset, Group: itemGroup(it), Model: pm.Name, Result: res}
				if jerr != nil {
					rec.Error = jerr.Error()
					ms[i].Status, ms[i].StopReason = stopStatus(bctx), fmt.Sprintf("在 %s/%s 处中止：%v", ph.Dataset, it.In.ID, jerr)
				}
				line, err := json.Marshal(rec)
				if err != nil {
					return runSummary{}, err
				}
				if _, err := results.Write(append(line, '\n')); err != nil {
					return runSummary{}, err
				}
				if err := results.Sync(); err != nil {
					return runSummary{}, err
				}
				record(i, ph, it, summarizeItem(res, jerr != nil))
				if res.Final == verdictInfra && len(res.Attempts) > 0 {
					code := res.Attempts[len(res.Attempts)-1].Audit.HTTPStatus
					if reason, ok := blockingHTTPStatus[code]; ok {
						ms[i].Status, ms[i].StopReason = runBlocked, fmt.Sprintf("HTTP %d %s（%s/%s）", code, reason, ph.Dataset, it.In.ID)
					}
				}
			}
		}
	}
	sum := runSummary{Status: runComplete, StartedAt: start.Format(time.RFC3339), Models: ms}
	for _, m := range ms {
		switch {
		case m.Status == runInterrupted || m.Status == runBudget:
			sum.Status = m.Status
		case m.Status == runBlocked && sum.Status == runComplete:
			sum.Status = runBlocked
		}
	}
	end := time.Now()
	sum.EndedAt, sum.ElapsedMS = end.Format(time.RFC3339), end.Sub(start).Milliseconds()
	sb, err := json.Marshal(sum, json.Indent("  "))
	if err != nil {
		return sum, err
	}
	return sum, writeExclusive(filepath.Join(dir, "summary.json"), append(sb, '\n'))
}

// stopStatus 区分预算耗尽与外部中断；两者都不能算完整完成。
func stopStatus(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return runBudget
	}
	return runInterrupted
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

var runEndpoints = map[string]string{
	"deepseek": "DEEPSEEK_BASE_URL 或默认 https://api.deepseek.com/v1，POST /chat/completions（流式）",
	"gemini":   "genai SDK 默认 Gemini API 地址，v1beta models/{model}:streamGenerateContent?alt=sse",
}

func modelManifests(models []runModel, maxAttempts int) []runModelManifest {
	var out []runModelManifest
	for _, pm := range models {
		out = append(out, runModelManifest{Name: pm.Name, Config: pm.Model.Config, ConfigHash: pm.Model.Config.hash(),
			Endpoint: pm.Endpoint, OmittedParams: omittedParams[pm.Model.Config.Provider], MaxHTTPAttempt: maxAttempts})
	}
	return out
}

// requireRunDir 要求输出目录尚不存在，不覆盖已有结果。
func requireRunDir(t *testing.T, env string) string {
	t.Helper()
	dir := os.Getenv(env)
	if dir == "" {
		t.Skipf("设置 %s 才发出真实评审请求", env)
	}
	_, err := os.Stat(dir)
	require.True(t, os.IsNotExist(err), "运行目录必须不存在，不覆盖已有结果")
	return dir
}

// codeState 记录实际代码提交；当前仓库已跟踪文件必须全部提交，清单才能对应到确切代码。
func codeState(t *testing.T) map[string]string {
	t.Helper()
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
	return code
}

func modelsFromEnv(t *testing.T, names []string) []runModel {
	t.Helper()
	var models []runModel
	for _, name := range names {
		m, err := judgeModelFromEnv(name)
		require.NoError(t, err)
		models = append(models, runModel{Name: name, Model: m, Endpoint: runEndpoints[name]})
	}
	return models
}
