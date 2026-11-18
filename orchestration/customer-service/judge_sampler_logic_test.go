package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent/constants"
)

// 这里的测试只连本地 httptest 服务，不发出任何外部请求。请求体由真实适配器（DeepSeek 的 HTTP 实现、Gemini 的 genai SDK）生成。

const testJudgeKey = "sk-judge-test-SECRET"

var (
	forgetPassJSON = `{"checks":[{"id":"Q1","quote":"","answer":"否"},{"id":"Q2","quote":"我这边没有记录","answer":"是"}]}`
	forgetFailJSON = `{"checks":[{"id":"Q1","quote":"","answer":"是"},{"id":"Q2","quote":"","answer":"否"}]}`
)

// sentinelForgetItem 用带哨兵字段的 validation 行经 loadLabels 加载，证明白名单之外的字段到不了请求。
func sentinelForgetItem(t *testing.T) refItem {
	t.Helper()
	forget := liveScenarios[len(liveScenarios)-2]
	line := fmt.Sprintf(`{"id":"val-forget-99","source_run":"SENTINEL_RUN","scenario":"forget","repeat":1,"judge_kind":"abstain","question":%q,"expect":%q,"reply":"我这边没有记录","label":"对","evidence":["SENTINEL_EVIDENCE"],"reason":"SENTINEL_REASON","label_method":"SENTINEL_METHOD","old_judge_event":{"note":"SENTINEL_OLD_JUDGE"},"judge_reason":"SENTINEL_JUDGE_REASON"}`, forget.question, forget.expect)
	items, err := loadLabels(strings.NewReader(line), "validation")
	require.NoError(t, err)
	require.Len(t, items, 1)
	return items[0]
}

type capturedRequest struct {
	Method, Path, Query string
	Header              http.Header
	Body                []byte
}

// fakeLLM 是本地模型服务：第 i 个请求交给 steps[i]（超出后重复最后一个），每个请求原样记录。
type fakeLLM struct {
	srv   *httptest.Server
	mu    sync.Mutex
	reqs  []capturedRequest
	steps []http.HandlerFunc
}

func newFakeLLM(t *testing.T, steps ...http.HandlerFunc) *fakeLLM {
	f := &fakeLLM{steps: steps}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		i := len(f.reqs)
		f.reqs = append(f.reqs, capturedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
		f.mu.Unlock()
		f.steps[min(i, len(f.steps)-1)](w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLLM) requests() []capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedRequest(nil), f.reqs...)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		fmt.Fprintf(w, "data: %s\n\n", e)
		w.(http.Flusher).Flush()
	}
}

// 各提供方的流式响应构造与审计期望，测试按表遍历两个提供方。
type providerFixture struct {
	name string
	cfg  judgeModelConfig
	base func(srv string) string
	// stream 返回一个完整响应：reasoning 与 content 分块下发，finish 为结束原因（空表示不给结束原因）。
	stream func(reasoning string, content []string, finish string) http.HandlerFunc
	// partial 下发一段内容后挂起，直到请求 context 结束，再把结束信号写入 released。
	partial                   func(content string, started, released chan<- struct{}) http.HandlerFunc
	stop, truncated, filtered string
	maxTokens                 func(body map[string]any) any
	responseModel             string
}

var providerFixtures = []providerFixture{
	{
		name: "deepseek", cfg: judgeModelConfigs["deepseek"],
		base: func(srv string) string { return srv },
		stream: func(reasoning string, content []string, finish string) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				head := map[string]any{"id": "ds-resp-1", "model": "deepseek-flash", "system_fingerprint": "fp_test"}
				ev := []string{mustJSON(merge(head, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": reasoning}}}}))}
				for _, c := range content {
					ev = append(ev, mustJSON(merge(head, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": c}}}})))
				}
				if finish != "" {
					ev = append(ev, mustJSON(merge(head, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": finish}}})))
				}
				ev = append(ev, mustJSON(merge(head, map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 900, "completion_tokens": 300, "total_tokens": 1200, "completion_tokens_details": map[string]any{"reasoning_tokens": 250}}})), "[DONE]")
				writeSSE(w, ev...)
			}
		},
		partial: func(content string, started, released chan<- struct{}) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				writeSSE(w, mustJSON(map[string]any{"id": "ds-partial", "model": "deepseek-flash", "choices": []any{map[string]any{"delta": map[string]any{"content": content}}}}))
				sendSignal(started)
				<-r.Context().Done()
				sendSignal(released)
			}
		},
		stop: "stop", truncated: "length", filtered: "content_filter",
		maxTokens:     func(b map[string]any) any { return b["max_tokens"] },
		responseModel: "deepseek-flash",
	},
	{
		name: "gemini", cfg: judgeModelConfigs["gemini"],
		base: func(srv string) string { return srv + "/" },
		stream: func(reasoning string, content []string, finish string) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				head := map[string]any{"modelVersion": "gemini-3.5-flash-lite-001", "responseId": "gm-resp-1"}
				part := func(p map[string]any, finish string) string {
					c := map[string]any{"content": map[string]any{"role": "model", "parts": []any{p}}}
					if finish != "" {
						c["finishReason"] = finish
					}
					return mustJSON(merge(head, map[string]any{"candidates": []any{c}}))
				}
				ev := []string{part(map[string]any{"text": reasoning, "thought": true}, "")}
				for i, c := range content {
					f := ""
					if i == len(content)-1 {
						f = finish
					}
					ev = append(ev, part(map[string]any{"text": c}, f))
				}
				ev = append(ev, mustJSON(merge(head, map[string]any{"usageMetadata": map[string]any{"promptTokenCount": 900, "candidatesTokenCount": 50, "thoughtsTokenCount": 250, "totalTokenCount": 1200}})))
				writeSSE(w, ev...)
			}
		},
		partial: func(content string, started, released chan<- struct{}) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				writeSSE(w, mustJSON(map[string]any{"responseId": "gm-partial", "candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": content}}}}}}))
				sendSignal(started)
				<-r.Context().Done()
				sendSignal(released)
			}
		},
		stop: "STOP", truncated: "MAX_TOKENS", filtered: "SAFETY",
		maxTokens:     func(b map[string]any) any { return b["generationConfig"].(map[string]any)["maxOutputTokens"] },
		responseModel: auditUnavailable,
	},
}

func merge(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func sendSignal(ch chan<- struct{}) {
	if ch != nil {
		ch <- struct{}{}
	}
}

func (p providerFixture) model(t *testing.T, f *fakeLLM, timeout time.Duration) judgeModel {
	t.Helper()
	cfg := p.cfg
	if timeout > 0 {
		cfg.Timeout = timeout
	}
	m, err := newJudgeModel(cfg, p.base(f.srv.URL), testJudgeKey, http.DefaultTransport)
	require.NoError(t, err)
	return m
}

// requireAuditMatchesServer 核对审计记录与服务端实际收到的请求一一对应：每次尝试恰好一个请求。
func requireAuditMatchesServer(t *testing.T, res judgeItemResult, f *fakeLLM) {
	t.Helper()
	require.Len(t, f.requests(), len(res.Attempts), "服务端请求数等于尝试次数")
	for _, a := range res.Attempts {
		require.Equal(t, 1, a.Audit.Requests, "位置 %d 第 %d 次尝试", a.Position, a.Attempt)
	}
}

func requestBody(t *testing.T, r capturedRequest) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(r.Body, &body))
	return body
}

// 抓取两个适配器发出的完整请求，与 testdata 中脱敏后的样例逐字节比对；设置 UPDATE_JUDGE_GOLDEN=1 重新生成。
func TestJudgeRequestGolden(t *testing.T) {
	item := sentinelForgetItem(t)
	r := judgeRubrics["forget"]
	checks := map[string]func(t *testing.T, body map[string]any){
		"deepseek": func(t *testing.T, b map[string]any) {
			require.Equal(t, "deepseek-flash", b["model"])
			require.Equal(t, map[string]any{"type": "enabled"}, b["thinking"])
			require.Equal(t, "high", b["reasoning_effort"])
			require.Equal(t, 1.0, b["top_p"])
			require.Equal(t, 8192.0, b["max_tokens"])
			require.Equal(t, true, b["stream"])
			require.Equal(t, map[string]any{"include_usage": true}, b["stream_options"])
			require.NotContains(t, b, "temperature", "思考模式下 temperature 不生效，评审适配器不发送")
		},
		"gemini": func(t *testing.T, b map[string]any) {
			gc := b["generationConfig"].(map[string]any)
			require.Equal(t, 8192.0, gc["maxOutputTokens"])
			require.Equal(t, map[string]any{"thinkingLevel": "LOW", "includeThoughts": true}, gc["thinkingConfig"], "只发 thinkingLevel，不发旧的 thinkingBudget")
			for _, k := range []string{"temperature", "topP", "topK"} {
				require.NotContains(t, gc, k)
			}
		},
	}
	for _, p := range providerFixtures {
		t.Run(p.name, func(t *testing.T) {
			f := newFakeLLM(t, p.stream("思考", []string{forgetPassJSON}, p.stop))
			res, err := judgeItem(context.Background(), p.model(t, f, 0), r, item)
			require.NoError(t, err)
			require.Equal(t, refCorrect, res.Final)
			reqs := f.requests()
			require.Len(t, reqs, judgePositions)
			for _, rq := range reqs {
				require.Equal(t, string(reqs[0].Body), string(rq.Body), "三次采样请求完全相同，互不共享上下文")
			}
			raw := string(reqs[0].Body)
			for _, want := range []string{item.In.Reply, item.In.Question, r.Background, judgeRubricRules, r.Checks[0].Ask, r.Checks[1].Ask} {
				require.Contains(t, raw, mustJSON(want)[1:len(mustJSON(want))-1])
			}
			for _, leak := range []string{"SENTINEL", "val-forget-99", "abstain", "\"对\"", "label"} {
				require.NotContains(t, raw, leak)
			}
			require.NotContains(t, raw, "Want")
			checks[p.name](t, requestBody(t, reqs[0]))

			golden := goldenRequest(t, reqs[0])
			path := filepath.Join("testdata", "judge-requests", p.name+".json")
			if os.Getenv("UPDATE_JUDGE_GOLDEN") == "1" {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, golden, 0o644))
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, string(want), string(golden))
			require.NotContains(t, string(golden), testJudgeKey)
		})
	}
}

// goldenRequest 把抓到的请求整理成可存档的样例：只保留与鉴权、格式相关的请求头，密钥替换为 [REDACTED]，主机名不记录。
func goldenRequest(t *testing.T, r capturedRequest) []byte {
	t.Helper()
	headers := map[string]string{}
	for _, h := range []string{"Authorization", "X-Goog-Api-Key", "Content-Type", "Accept"} {
		if v := r.Header.Get(h); v != "" {
			require.False(t, h != "Authorization" && h != "X-Goog-Api-Key" && strings.Contains(v, testJudgeKey))
			headers[h] = strings.ReplaceAll(v, testJudgeKey, "[REDACTED]")
		}
	}
	require.True(t, r.Header.Get("Authorization") == "Bearer "+testJudgeKey || r.Header.Get("X-Goog-Api-Key") == testJudgeKey, "密钥经请求头发送")
	var body any
	require.NoError(t, json.Unmarshal(r.Body, &body))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(map[string]any{"method": r.Method, "path": r.Path, "query": r.Query, "headers": headers, "body": body}))
	return buf.Bytes()
}

// 思考内容里放一个相反结论的 JSON，最终内容分块下发：裁决只取最终内容，思考内容单独存档。
func TestJudgeSamplerContentReasoningSplit(t *testing.T) {
	item := sentinelForgetItem(t)
	for _, p := range providerFixtures {
		t.Run(p.name, func(t *testing.T) {
			half := len(forgetPassJSON) / 2
			f := newFakeLLM(t, p.stream("先想想："+forgetFailJSON, []string{forgetPassJSON[:half], forgetPassJSON[half:]}, p.stop))
			res, err := judgeItem(context.Background(), p.model(t, f, 0), judgeRubrics["forget"], item)
			require.NoError(t, err)
			require.Equal(t, refCorrect, res.Final)
			requireAuditMatchesServer(t, res, f)
			a := res.Attempts[0]
			require.Equal(t, forgetPassJSON, a.Content)
			require.Equal(t, "先想想："+forgetFailJSON, a.Reasoning)
			require.Equal(t, p.cfg.Model, a.Audit.RequestModel)
			require.Equal(t, p.responseModel, a.Audit.ResponseModel)
			require.NotEqual(t, auditUnavailable, a.Audit.ModelVersion)
			require.NotEqual(t, auditUnavailable, a.Audit.ResponseID)
			require.Equal(t, p.stop, a.Audit.FinishReason)
			require.NotNil(t, a.Audit.Usage)
			require.Equal(t, judgeUsage{Prompt: 900, Output: a.Audit.Usage.Output, Reasoning: 250, Total: 1200}, *a.Audit.Usage)
		})
	}
}

// 截断即使内容恰好是合法 JSON 也记为技术失败并按协议重试；重试的输出上限不变。
func TestJudgeSamplerTruncation(t *testing.T) {
	item := sentinelForgetItem(t)
	for _, p := range providerFixtures {
		t.Run(p.name, func(t *testing.T) {
			tests := []struct {
				name     string
				steps    func() []http.HandlerFunc
				final    string
				requests int
			}{
				{"截断后重试成功", func() []http.HandlerFunc {
					return []http.HandlerFunc{p.stream("", []string{forgetPassJSON}, p.truncated), p.stream("", []string{forgetPassJSON}, p.stop)}
				}, refCorrect, 4},
				{"三次都截断为 infra", func() []http.HandlerFunc {
					return []http.HandlerFunc{p.stream("", []string{forgetPassJSON}, p.truncated)}
				}, verdictInfra, 3},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					f := newFakeLLM(t, tt.steps()...)
					res, err := judgeItem(context.Background(), p.model(t, f, 0), judgeRubrics["forget"], item)
					require.NoError(t, err)
					require.Equal(t, tt.final, res.Final)
					requireAuditMatchesServer(t, res, f)
					require.Len(t, res.Attempts, tt.requests)
					a := res.Attempts[0]
					require.Equal(t, [2]string{stageCall, causeTruncated}, [2]string{a.Stage, a.Cause})
					require.Equal(t, forgetPassJSON, a.Content, "截断时已收到的内容留档，但不解析")
					require.Nil(t, a.Answers)
					require.Equal(t, p.truncated, a.Audit.FinishReason)
					for _, rq := range f.requests() {
						require.Equal(t, 8192.0, p.maxTokens(requestBody(t, rq)), "重试不自动提高输出上限")
					}
				})
			}
		})
	}
}

// 非 200、过滤、缺结束原因等非正常结束：每次尝试只发一个请求（SDK 内部不重试），重试上限内耗尽后为 infra。
func TestJudgeSamplerAbnormalEnd(t *testing.T) {
	item := sentinelForgetItem(t)
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			fmt.Fprintf(w, `{"error":{"code":%d,"message":"echo key %s","status":"UNAVAILABLE"}}`, code, testJudgeKey)
		}
	}
	for _, p := range providerFixtures {
		t.Run(p.name, func(t *testing.T) {
			tests := []struct {
				name  string
				step  http.HandlerFunc
				cause string
			}{
				{"503", status(http.StatusServiceUnavailable), causeHTTP},
				{"429", status(http.StatusTooManyRequests), causeHTTP},
				{"内容过滤", p.stream("", []string{forgetPassJSON}, p.filtered), causeFinish},
				{"缺少结束原因", p.stream("", []string{forgetPassJSON}, ""), causeFinish},
			}
			if p.name == "deepseek" {
				tests = append(tests, struct {
					name  string
					step  http.HandlerFunc
					cause string
				}{"流在 DONE 之前结束", func(w http.ResponseWriter, _ *http.Request) {
					writeSSE(w, mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": forgetPassJSON}, "finish_reason": "stop"}}}))
				}, causeFinish})
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					f := newFakeLLM(t, tt.step)
					res, err := judgeItem(context.Background(), p.model(t, f, 0), judgeRubrics["forget"], item)
					require.NoError(t, err)
					require.Equal(t, verdictInfra, res.Final)
					require.Equal(t, stageCall, res.InfraStage)
					require.Len(t, res.Attempts, 1+judgeMaxRetries)
					requireAuditMatchesServer(t, res, f)
					for _, a := range res.Attempts {
						require.Equal(t, tt.cause, a.Cause, a.Error)
						require.NotContains(t, a.Error, testJudgeKey, "错误信息不含密钥")
					}
				})
			}
		})
	}
}

// 每次尝试有独立的超时 context：第一次在流中途超时，已收到的内容留档；重试拿到新的、未过期的 context 并成功。
// 服务端确认超时请求的连接被释放。
func TestJudgeSamplerAttemptTimeout(t *testing.T) {
	item := sentinelForgetItem(t)
	for _, p := range providerFixtures {
		t.Run(p.name, func(t *testing.T) {
			released := make(chan struct{}, 1)
			f := newFakeLLM(t, p.partial(`{"checks":[`, nil, released), p.stream("", []string{forgetPassJSON}, p.stop))
			m := p.model(t, f, 300*time.Millisecond)
			type entry struct {
				err      error
				deadline time.Time
			}
			var entries []entry
			inner := m.Sample
			m.Sample = func(ctx context.Context, prompt string) (judgeRaw, error) {
				d, _ := ctx.Deadline()
				entries = append(entries, entry{ctx.Err(), d})
				return inner(ctx, prompt)
			}
			res, err := judgeItem(context.Background(), m, judgeRubrics["forget"], item)
			require.NoError(t, err)
			require.Equal(t, refCorrect, res.Final)
			requireAuditMatchesServer(t, res, f)
			require.Len(t, res.Attempts, 4)
			a := res.Attempts[0]
			require.Equal(t, [2]string{stageCall, causeTimeout}, [2]string{a.Stage, a.Cause})
			require.Equal(t, `{"checks":[`, a.Content)
			require.Len(t, entries, 4)
			for i, e := range entries {
				require.NoError(t, e.err, "第 %d 次尝试开始时 context 未过期", i+1)
				if i > 0 {
					require.True(t, e.deadline.After(entries[i-1].deadline), "每次尝试的截止时间各自独立")
				}
			}
			select {
			case <-released:
			case <-time.After(5 * time.Second):
				t.Fatal("超时后服务端请求未结束")
			}
		})
	}
}

// 父 context 取消立即结束流式读取并释放连接；judgeItem 返回取消错误而不是 infra，也不再发起新请求。
func TestJudgeSamplerParentCancel(t *testing.T) {
	item := sentinelForgetItem(t)
	for _, p := range providerFixtures {
		t.Run(p.name, func(t *testing.T) {
			started, released := make(chan struct{}, 1), make(chan struct{}, 1)
			f := newFakeLLM(t, p.partial(`{"checks":[`, started, released))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-started
				cancel()
			}()
			begin := time.Now()
			res, err := judgeItem(ctx, p.model(t, f, 0), judgeRubrics["forget"], item)
			require.ErrorIs(t, err, context.Canceled)
			require.Less(t, time.Since(begin), 10*time.Second, "不等 120 秒超时")
			require.Empty(t, res.Final)
			require.Len(t, f.requests(), 1)
			select {
			case <-released:
			case <-time.After(5 * time.Second):
				t.Fatal("取消后服务端请求未结束")
			}
		})
	}
}

// 凭据只从环境变量读取；缺失时报变量名，不输出值。DeepSeek 的 base URL 可由环境变量指向本地服务。
func TestJudgeModelFromEnv(t *testing.T) {
	for _, tt := range []struct{ name, env string }{{"deepseek", constants.EnvDeepSeekAPIKey}, {"gemini", constants.EnvGeminiAPIKey}} {
		t.Run(tt.name+" 缺凭据", func(t *testing.T) {
			t.Setenv(tt.env, "")
			_, err := judgeModelFromEnv(tt.name)
			require.ErrorContains(t, err, tt.env)
		})
	}
	t.Run("未知模型", func(t *testing.T) {
		_, err := judgeModelFromEnv("gpt")
		require.Error(t, err)
	})
	t.Run("deepseek 走环境变量", func(t *testing.T) {
		p := providerFixtures[0]
		f := newFakeLLM(t, p.stream("", []string{forgetPassJSON}, p.stop))
		t.Setenv(constants.EnvDeepSeekAPIKey, testJudgeKey)
		t.Setenv(constants.EnvDeepSeekBaseURL, f.srv.URL)
		m, err := judgeModelFromEnv("deepseek")
		require.NoError(t, err)
		res, err := judgeItem(context.Background(), m, judgeRubrics["forget"], sentinelForgetItem(t))
		require.NoError(t, err)
		require.Equal(t, refCorrect, res.Final)
		require.Equal(t, "Bearer "+testJudgeKey, f.requests()[0].Header.Get("Authorization"))
	})
}

func TestJudgeModelConfigs(t *testing.T) {
	require.Len(t, judgeModelConfigs, 2)
	hashes := map[string]bool{}
	for name, c := range judgeModelConfigs {
		require.Equal(t, name, c.Provider)
		require.Equal(t, 8192, c.MaxOutputTokens)
		require.Equal(t, 120*time.Second, c.Timeout)
		require.Equal(t, 1, c.Concurrency)
		require.Equal(t, c.hash(), c.hash())
		require.Len(t, c.hash(), 64)
		hashes[c.hash()] = true
	}
	require.Len(t, hashes, 2)
	ds, gm := judgeModelConfigs["deepseek"], judgeModelConfigs["gemini"]
	require.Equal(t, [4]any{"deepseek-flash", "enabled", "high", 1.0}, [4]any{ds.Model, ds.Thinking, ds.ReasoningEffort, *ds.TopP})
	require.Equal(t, [3]any{"gemini-3.5-flash-lite", "LOW", (*float64)(nil)}, [3]any{gm.Model, gm.ThinkingLevel, gm.TopP})
	changed := ds
	changed.MaxOutputTokens = 16384
	require.NotEqual(t, ds.hash(), changed.hash(), "改任何参数都会改变配置 hash")
}
