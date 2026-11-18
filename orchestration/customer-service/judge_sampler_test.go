package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/genai"

	"github.com/kart-io/goagent/constants"

	"github.com/kart-io/goagent-examples/internal/livemodel"
)

// 评审专用的模型适配器。不复用业务 Agent 的 livemodel 客户端：评审需要显式开启思考、省略不生效的采样参数，
// 而业务默认行为不能因此改变。DeepSeek 的 thinking 字段 go-openai 无法表达，所以直接走 HTTP；Gemini 用 genai SDK。
// 两者都不做内部重试，重试只由 judgeItem 按 judge-protocol/1 进行。

// judgeModelConfig 是一套评审模型配置；正式批次及其重试过程中不改动，结果里记录它的 hash。
type judgeModelConfig struct {
	Provider        string        `json:"provider"`
	Model           string        `json:"model"`
	API             string        `json:"api"`
	Thinking        string        `json:"thinking,omitempty"`         // DeepSeek：thinking.type
	ReasoningEffort string        `json:"reasoning_effort,omitempty"` // DeepSeek
	ThinkingLevel   string        `json:"thinking_level,omitempty"`   // Gemini：thinkingConfig.thinkingLevel
	IncludeThoughts bool          `json:"include_thoughts,omitempty"` // Gemini：思考摘要单独返回，便于存档
	TopP            *float64      `json:"top_p,omitempty"`
	MaxOutputTokens int           `json:"max_output_tokens"`
	Timeout         time.Duration `json:"timeout_ns"` // 每次尝试
	Concurrency     int           `json:"concurrency"`
}

func (c judgeModelConfig) hash() string {
	b, err := json.Marshal(c)
	if err != nil {
		panic(err) // 结构体只含基本类型，不会失败
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ptrTo[T any](v T) *T { return &v }

// judgeModelConfigs 是候选配置。输出上限 8192 是小批预检的候选值，尚未验证是否足够。
var judgeModelConfigs = map[string]judgeModelConfig{
	"deepseek": {Provider: "deepseek", Model: "deepseek-flash", API: "chat-completions-stream",
		Thinking: "enabled", ReasoningEffort: "high", TopP: ptrTo(1.0),
		MaxOutputTokens: 8192, Timeout: 120 * time.Second, Concurrency: 1},
	"gemini": {Provider: "gemini", Model: "gemini-3.5-flash-lite", API: "generate-content-stream",
		ThinkingLevel: string(genai.ThinkingLevelLow), IncludeThoughts: true,
		MaxOutputTokens: 8192, Timeout: 120 * time.Second, Concurrency: 1},
}

const auditUnavailable = "不可得"

type requestCounterKey struct{}

type usageProbeKey struct{}

// usageProbe 保存一次尝试的原始响应体：genai SDK 把缺失的 thoughtsTokenCount 解码成 0，只能从原始流判断服务端是否报告。
type usageProbe struct {
	mu  sync.Mutex
	buf []byte
}

const usageProbeLimit = 4 << 20

type probeBody struct {
	io.ReadCloser
	p *usageProbe
}

func (b probeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.p.mu.Lock()
	if len(b.p.buf)+n <= usageProbeLimit {
		b.p.buf = append(b.p.buf, p[:n]...)
	}
	b.p.mu.Unlock()
	return n, err
}

// reported 返回原始流里最后一个 usageMetadata 是否带了指定字段；与 SDK 一样以最后一个为准。
func (p *usageProbe) reported(field string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	var last map[string]json.RawMessage
	for _, line := range strings.Split(string(p.buf), "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		var ev struct {
			UsageMetadata map[string]json.RawMessage `json:"usageMetadata"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &ev) == nil && ev.UsageMetadata != nil {
			last = ev.UsageMetadata
		}
	}
	_, ok := last[field]
	return ok
}

// countingTransport 按 context 统计每次尝试实际发出的 HTTP 请求，用来核对审计记录与 SDK 是否偷偷重试。
type countingTransport struct{ next http.RoundTripper }

func (t countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if n, ok := r.Context().Value(requestCounterKey{}).(*atomic.Int32); ok {
		n.Add(1)
	}
	resp, err := t.next.RoundTrip(r)
	if p, ok := r.Context().Value(usageProbeKey{}).(*usageProbe); ok && err == nil {
		resp.Body = probeBody{ReadCloser: resp.Body, p: p}
	}
	return resp, err
}

// newJudgeModel 组装评审模型。HTTP 客户端不设整体超时，超时只由每次尝试的 context 控制，以免截断长 SSE。
// POST 请求不会被 net/http 自动重放。
func newJudgeModel(cfg judgeModelConfig, baseURL, apiKey string, rt http.RoundTripper) (judgeModel, error) {
	client := &http.Client{Transport: countingTransport{next: rt}}
	var sample judgeSampler
	switch cfg.Provider {
	case "deepseek":
		sample = deepSeekSampler(cfg, baseURL, apiKey, client)
	case "gemini":
		var err error
		if sample, err = geminiSampler(cfg, baseURL, apiKey, client); err != nil {
			return judgeModel{}, err
		}
	default:
		return judgeModel{}, fmt.Errorf("不支持的评审模型提供方 %q", cfg.Provider)
	}
	return judgeModel{Config: cfg, Sample: countRequests(sample)}, nil
}

// countRequests 给每次尝试挂一个独立计数器，结束后写入审计。
func countRequests(s judgeSampler) judgeSampler {
	return func(ctx context.Context, prompt string) (judgeRaw, error) {
		n := &atomic.Int32{}
		raw, err := s(context.WithValue(ctx, requestCounterKey{}, n), prompt)
		raw.Audit.Requests = int(n.Load())
		return raw, err
	}
}

// judgeModelFromEnv 从环境变量取凭据；错误信息不含密钥。
func judgeModelFromEnv(name string) (judgeModel, error) {
	cfg, ok := judgeModelConfigs[name]
	if !ok {
		return judgeModel{}, fmt.Errorf("未知评审模型 %q", name)
	}
	keyEnv, baseURL := constants.EnvGeminiAPIKey, ""
	if cfg.Provider == "deepseek" {
		keyEnv, baseURL = constants.EnvDeepSeekAPIKey, constants.DeepSeekBaseURL
		if v := os.Getenv(constants.EnvDeepSeekBaseURL); v != "" {
			baseURL = v
		}
	}
	key := os.Getenv(keyEnv)
	if key == "" {
		return judgeModel{}, fmt.Errorf("缺少评审模型凭据 %s", keyEnv)
	}
	return newJudgeModel(cfg, baseURL, key, http.DefaultTransport)
}

// redact 去掉错误信息中回显的密钥：既包括本次使用的密钥，也包括环境里已知的各家密钥。
func redact(s, apiKey string) string {
	if apiKey != "" {
		s = strings.ReplaceAll(s, apiKey, "[REDACTED]")
	}
	return livemodel.Redact(s)
}

func newAudit(model string) judgeAudit {
	return judgeAudit{RequestModel: model, ResponseModel: auditUnavailable, ModelVersion: auditUnavailable, SystemFingerprint: auditUnavailable, ResponseID: auditUnavailable, FinishReason: auditUnavailable}
}

// --- DeepSeek：OpenAI 兼容 Chat Completions 流式接口 ---

type deepSeekMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type deepSeekRequest struct {
	Model    string            `json:"model"`
	Messages []deepSeekMessage `json:"messages"`
	Thinking struct {
		Type string `json:"type"`
	} `json:"thinking"`
	ReasoningEffort string   `json:"reasoning_effort"`
	TopP            *float64 `json:"top_p,omitempty"`
	MaxTokens       int      `json:"max_tokens"`
	Stream          bool     `json:"stream"`
	StreamOptions   struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type deepSeekChunk struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	SystemFingerprint string `json:"system_fingerprint"`
	Choices           []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens            int `json:"prompt_tokens"`
		CompletionTokens        int `json:"completion_tokens"`
		TotalTokens             int `json:"total_tokens"`
		CompletionTokensDetails *struct {
			ReasoningTokens *int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func deepSeekBody(cfg judgeModelConfig, prompt string) deepSeekRequest {
	var req deepSeekRequest
	req.Model, req.ReasoningEffort, req.TopP, req.MaxTokens, req.Stream = cfg.Model, cfg.ReasoningEffort, cfg.TopP, cfg.MaxOutputTokens, true
	req.Messages = []deepSeekMessage{{Role: "user", Content: prompt}}
	req.Thinking.Type = cfg.Thinking
	req.StreamOptions.IncludeUsage = true
	return req
}

func deepSeekSampler(cfg judgeModelConfig, baseURL, apiKey string, client *http.Client) judgeSampler {
	url := strings.TrimRight(baseURL, "/") + "/chat/completions"
	return func(ctx context.Context, prompt string) (judgeRaw, error) {
		raw := judgeRaw{Audit: newAudit(cfg.Model)}
		body, err := json.Marshal(deepSeekBody(cfg, prompt))
		if err != nil {
			return raw, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return raw, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		resp, err := client.Do(req)
		if err != nil {
			return raw, errors.New(redact(err.Error(), apiKey))
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			raw.Audit.HTTPStatus = resp.StatusCode
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			return raw, &stageError{stage: stageCall, cause: causeHTTP, err: fmt.Errorf("HTTP %d：%s", resp.StatusCode, redact(string(msg), apiKey))}
		}
		var content, reasoning strings.Builder
		finish, done := "", false
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for !done && sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data:")
			if !ok {
				continue
			}
			if data = strings.TrimSpace(data); data == "[DONE]" {
				done = true
				continue
			}
			var ch deepSeekChunk
			if err := json.Unmarshal([]byte(data), &ch); err != nil {
				return raw, &stageError{stage: stageCall, cause: causeFinish, err: fmt.Errorf("流数据不是合法 JSON：%w", err)}
			}
			if ch.Error != nil {
				return raw, &stageError{stage: stageCall, cause: causeFinish, err: fmt.Errorf("流中返回错误：%s", ch.Error.Message)}
			}
			if ch.Model != "" {
				raw.Audit.ResponseModel = ch.Model
			}
			if ch.SystemFingerprint != "" {
				raw.Audit.SystemFingerprint = ch.SystemFingerprint
			}
			if ch.ID != "" {
				raw.Audit.ResponseID = ch.ID
			}
			for _, c := range ch.Choices {
				content.WriteString(c.Delta.Content)
				reasoning.WriteString(c.Delta.ReasoningContent)
				if c.FinishReason != nil && *c.FinishReason != "" {
					finish = *c.FinishReason
				}
			}
			if u := ch.Usage; u != nil {
				raw.Audit.Usage = &judgeUsage{Prompt: u.PromptTokens, Output: u.CompletionTokens, Total: u.TotalTokens}
				if d := u.CompletionTokensDetails; d != nil && d.ReasoningTokens != nil {
					raw.Audit.Usage.Reasoning, raw.Audit.Usage.ReasoningReported = *d.ReasoningTokens, true
				}
			}
		}
		raw.Content, raw.Reasoning = content.String(), reasoning.String()
		if finish != "" {
			raw.Audit.FinishReason = finish
		}
		if err := sc.Err(); err != nil {
			return raw, err
		}
		if !done {
			return raw, &stageError{stage: stageCall, cause: causeFinish, err: errors.New("流在 [DONE] 之前中断")}
		}
		return raw, finishError(finish, "stop", "length")
	}
}

// finishError 把结束原因映射为技术失败：截断单独记为 truncated，即使已收到的内容恰好能解析成 JSON 也不采用。
func finishError(finish, ok, truncated string) error {
	switch finish {
	case ok:
		return nil
	case truncated:
		return &stageError{stage: stageCall, cause: causeTruncated, err: fmt.Errorf("输出被截断（%s）", finish)}
	case "":
		return &stageError{stage: stageCall, cause: causeFinish, err: errors.New("流结束时没有结束原因")}
	default:
		return &stageError{stage: stageCall, cause: causeFinish, err: fmt.Errorf("非正常结束（%s）", finish)}
	}
}

// --- Gemini：genai SDK 的 GenerateContentStream ---

func geminiSampler(cfg judgeModelConfig, baseURL, apiKey string, client *http.Client) (judgeSampler, error) {
	gc, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey: apiKey, Backend: genai.BackendGeminiAPI, HTTPClient: client,
		HTTPOptions: genai.HTTPOptions{BaseURL: baseURL, RetryOptions: &genai.HTTPRetryOptions{Attempts: ptrTo(int32(1))}},
	})
	if err != nil {
		return nil, fmt.Errorf("创建 Gemini 评审客户端：%s", redact(err.Error(), apiKey))
	}
	// 不设置 Temperature、TopP（本代模型已弃用并忽略），也不设置旧的 ThinkingBudget。
	gcfg := &genai.GenerateContentConfig{
		MaxOutputTokens: int32(cfg.MaxOutputTokens),
		ThinkingConfig:  &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevel(cfg.ThinkingLevel), IncludeThoughts: cfg.IncludeThoughts},
	}
	return func(ctx context.Context, prompt string) (judgeRaw, error) {
		raw := judgeRaw{Audit: newAudit(cfg.Model)}
		probe := &usageProbe{}
		ctx = context.WithValue(ctx, usageProbeKey{}, probe)
		var content, reasoning strings.Builder
		finish := ""
		for resp, err := range gc.Models.GenerateContentStream(ctx, cfg.Model, genai.Text(prompt), gcfg) {
			if err != nil {
				raw.Content, raw.Reasoning = content.String(), reasoning.String()
				var apiErr genai.APIError
				if errors.As(err, &apiErr) {
					raw.Audit.HTTPStatus = apiErr.Code
					return raw, &stageError{stage: stageCall, cause: causeHTTP, err: fmt.Errorf("HTTP %d：%s", apiErr.Code, redact(apiErr.Message, apiKey))}
				}
				return raw, errors.New(redact(err.Error(), apiKey))
			}
			if resp.ModelVersion != "" {
				raw.Audit.ModelVersion = resp.ModelVersion
			}
			if resp.ResponseID != "" {
				raw.Audit.ResponseID = resp.ResponseID
			}
			if u := resp.UsageMetadata; u != nil {
				raw.Audit.Usage = &judgeUsage{Prompt: int(u.PromptTokenCount), Output: int(u.CandidatesTokenCount), Reasoning: int(u.ThoughtsTokenCount), Total: int(u.TotalTokenCount)}
			}
			if pf := resp.PromptFeedback; pf != nil && pf.BlockReason != "" {
				finish = "PROMPT_BLOCKED_" + string(pf.BlockReason)
			}
			for _, c := range resp.Candidates {
				if c.Content != nil {
					for _, p := range c.Content.Parts {
						if p.Thought {
							reasoning.WriteString(p.Text)
						} else {
							content.WriteString(p.Text)
						}
					}
				}
				if c.FinishReason != "" {
					finish = string(c.FinishReason)
				}
			}
		}
		raw.Content, raw.Reasoning = content.String(), reasoning.String()
		if finish != "" {
			raw.Audit.FinishReason = finish
		}
		if raw.Audit.Usage != nil {
			raw.Audit.Usage.ReasoningReported = probe.reported("thoughtsTokenCount")
		}
		if err := ctx.Err(); err != nil {
			return raw, err
		}
		return raw, finishError(finish, string(genai.FinishReasonStop), string(genai.FinishReasonMaxTokens))
	}, nil
}
