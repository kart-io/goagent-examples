package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	agentErrors "github.com/kart-io/goagent/errors"
	mg "github.com/kart-io/goagent/management"

	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/assistant"
	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/chat"
	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/config"
	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/docs"
	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/embed"
)

const corpus = "../../testdata/corpus"

func TestActorFromRequest(t *testing.T) {
	tests := []struct {
		name   string
		header string // 空表示不带 Authorization
		want   mg.Actor
		err    error
	}{
		{"无头", "", mg.Actor{}, agentErrors.ErrManagementUnauthenticated},
		{"空名", "Bearer ", mg.Actor{}, agentErrors.ErrManagementUnauthenticated},
		{"含空格", "Bearer a b", mg.Actor{}, agentErrors.ErrManagementUnauthenticated},
		{"路径穿越", "Bearer ../x", mg.Actor{}, agentErrors.ErrManagementUnauthenticated},
		{"65字节", "Bearer " + strings.Repeat("a", 65), mg.Actor{}, agentErrors.ErrManagementUnauthenticated},
		{"Basic", "Basic u1", mg.Actor{}, agentErrors.ErrManagementUnauthenticated},
		{"合法", "Bearer u1", mg.Actor{TenantID: "u1", Subject: "u1"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}
			got, err := ActorFromRequest(r)
			if !errors.Is(err, tt.err) || (tt.err == nil && err != nil) {
				t.Fatalf("错误 = %v，期望 %v", err, tt.err)
			}
			if got != tt.want {
				t.Errorf("Actor = %+v，期望 %+v", got, tt.want)
			}
		})
	}
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	c := config.Config{
		PGURL:       os.Getenv("KA_TEST_PG_URL"),
		RedisAddr:   os.Getenv("KA_TEST_REDIS_ADDR"),
		QdrantAddr:  os.Getenv("KA_TEST_QDRANT_ADDR"),
		CachePrefix: fmt.Sprintf("ka:test:%08x:", rand.Uint32()),
		CacheTagKey: []byte(strings.Repeat("k", 32)),
		Model:       "script",
		Embedder:    "hash",
		CorpusDir:   corpus,
	}
	if c.PGURL == "" || c.RedisAddr == "" || c.QdrantAddr == "" {
		t.Skip("需经 scripts/test.sh 运行")
	}
	return c
}

// newServer 迁移、装配存储与索引，返回挂着 NewHandler 的测试服务器；worker 为真时另起 Worker。
// 不跑任务的测试不起 Worker：Worker.Run 在认领槽位途中被取消会留下无人释放的槽位（见 Task 7 报告 FINDING）。
func newServer(t *testing.T, worker bool) *httptest.Server {
	t.Helper()
	c := testConfig(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, c.PGURL)
	if err != nil {
		t.Fatalf("连接 PG: %v", err)
	}
	err = assistant.Migrate(ctx, pool)
	pool.Close()
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	e := embed.NewHash(3072)
	model := chat.NewScript()
	st, err := assistant.Open(ctx, c, model, e)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(context.Background()); err != nil {
			t.Errorf("关闭 Stores: %v", err)
		}
	})
	idx, err := docs.NewIndex(ctx, c.QdrantAddr, fmt.Sprintf("w_%08x", rand.Uint32()), e)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	chunks, err := docs.Collect(corpus)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if _, err := idx.Rebuild(ctx, "init", chunks); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	svc, err := mg.New(mg.Config{
		Store:         st.Manage,
		Sessions:      st.Sessions,
		HistoryReader: st.Manage,
		Authorizer:    mg.TenantAuthorizer{},
		Factories: map[string]mg.Factory{assistant.RunnerKind: assistant.Factory(assistant.Deps{
			Stores: st, Model: model, Index: idx, CorpusDir: corpus, Embedder: e,
		})},
		Report: func(stage string, err error) { t.Logf("管理诊断：%s %v", stage, err) },
	})
	if err != nil {
		t.Fatalf("mg.New: %v", err)
	}
	h, err := NewHandler(svc, st, idx)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	if !worker {
		t.Cleanup(srv.Close)
		return srv
	}
	w := mg.NewWorker(svc)
	wctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(wctx) }()
	// 关闭顺序与 cmd/ka 一致：先停 Worker 接新活，再关 HTTP，最后等 Worker 退出。
	t.Cleanup(func() {
		w.BeginShutdown()
		cancel()
		srv.Close()
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		_ = w.Shutdown(sctx)
		// 槽位被同库的其他 Worker 占用时 Run 立即返回 ErrManagementOwner，表现为任务一直不动。
		if err := <-done; err != nil {
			t.Errorf("Worker 退出：%v", err)
		}
	})
	return srv
}

// tenant 生成本次运行独有的用户名，避免与复用库里的历史数据互相干扰。
func tenant(prefix string) string { return prefix + "-" + uuid.NewString() }

// call 以 user 的身份请求 base+path；out 非 nil 时解码响应体。返回状态码。
func call(t *testing.T, base, user, method, path string, body, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求: %v", err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+user)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	if out != nil && res.StatusCode < 300 && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("解码 %s %s 响应 %s: %v", method, path, raw, err)
		}
	}
	if res.StatusCode >= 300 {
		t.Logf("%s %s → %d %s", method, path, res.StatusCode, raw)
	}
	return res.StatusCode
}

func waitFor(t *testing.T, what string, ok func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		done, err := ok()
		if err != nil {
			t.Fatalf("等待%s: %v", what, err)
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待%s超时", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ask 只经 HTTP 走完页面的对话流程：确保 Agent、建会话、提交任务并等到终态。
func ask(t *testing.T, base, user, input string) mg.Task {
	t.Helper()
	var agent struct {
		AgentID string `json:"agent_id"`
		Version string `json:"version"`
	}
	if code := call(t, base, user, "POST", "/api/agent", nil, &agent); code != http.StatusOK || agent.AgentID == "" {
		t.Fatalf("POST /api/agent = %d %+v", code, agent)
	}
	var se mg.Session
	if code := call(t, base, user, "POST", "/v1/sessions", map[string]any{"agent_id": agent.AgentID}, &se); code != http.StatusAccepted {
		t.Fatalf("建会话 = %d", code)
	}
	waitFor(t, "会话就绪", func() (bool, error) {
		call(t, base, user, "GET", "/v1/sessions/"+se.ID, nil, &se)
		return se.Status == "ready", nil
	})
	var task mg.Task
	if code := call(t, base, user, "POST", "/v1/tasks", map[string]any{"session_id": se.ID, "input": input}, &task); code != http.StatusAccepted {
		t.Fatalf("提交任务 = %d", code)
	}
	waitFor(t, "任务结束", func() (bool, error) {
		call(t, base, user, "GET", "/v1/tasks/"+task.ID, nil, &task)
		if task.State == mg.Attention {
			return false, fmt.Errorf("任务停在 %s（%s）", task.State, task.ErrorKind)
		}
		return task.State.Terminal(), nil
	})
	return task
}

type memories struct {
	Pinned  []map[string]any `json:"pinned"`
	Matches []map[string]any `json:"matches"`
}

// findMemory 返回 matches 中正文含 text 的记录 ID，没有则为空。
func findMemory(m memories, text string) string {
	for _, r := range m.Matches {
		if c, _ := r["content"].(string); strings.Contains(c, text) {
			id, _ := r["id"].(string)
			return id
		}
	}
	return ""
}

func TestMemoryRoutes(t *testing.T) {
	srv := newServer(t, true)
	users := map[string]string{"u1": tenant("u1"), "u2": tenant("u2")}
	const fact = "我偏好中文示例"
	task := ask(t, srv.URL, users["u1"], "记住："+fact)
	if task.State != mg.Succeeded {
		t.Fatalf("写入记忆任务 = %s（%s）", task.State, task.ErrorKind)
	}
	search := "/api/memories?q=" + url.QueryEscape("中文示例")
	var before memories
	call(t, srv.URL, users["u1"], "GET", search, nil, &before)
	id := findMemory(before, fact)
	if id == "" {
		t.Fatalf("matches 不含刚写入的记忆：%+v", before)
	}

	tests := []struct {
		name     string
		user     string
		method   string
		wantCode int
		wantHit  *bool // 仅 GET：matches 是否仍含该记忆
	}{
		{"u2 删不到 u1 的记忆", "u2", "DELETE", http.StatusNotFound, nil},
		{"u2 删除无效后仍在", "u1", "GET", http.StatusOK, ptr(true)},
		{"u1 删除", "u1", "DELETE", http.StatusNoContent, nil},
		{"删除后查不到", "u1", "GET", http.StatusOK, ptr(false)},
		{"u2 再删仍 404", "u2", "DELETE", http.StatusNotFound, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := "/api/memories/" + id
			if tt.method == "GET" {
				path = search
			}
			var got memories
			if code := call(t, srv.URL, users[tt.user], tt.method, path, nil, &got); code != tt.wantCode {
				t.Fatalf("%s %s = %d，期望 %d", tt.method, path, code, tt.wantCode)
			}
			if tt.wantHit != nil && (findMemory(got, fact) != "") != *tt.wantHit {
				t.Errorf("matches 含该记忆 = %v，期望 %v；%+v", !*tt.wantHit, *tt.wantHit, got)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestSources(t *testing.T) {
	srv := newServer(t, false)
	user := tenant("s1")
	tests := []struct {
		name   string
		q      string
		minLen int
	}{
		{"命中语料", "pgstore 怎么建表", 1},
		{"空查询", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []struct {
				Source  string  `json:"source"`
				Score   float64 `json:"score"`
				Excerpt string  `json:"excerpt"`
			}
			if code := call(t, srv.URL, user, "GET", "/api/sources?q="+url.QueryEscape(tt.q), nil, &got); code != http.StatusOK {
				t.Fatalf("GET /api/sources = %d", code)
			}
			if len(got) < tt.minLen || len(got) > 4 {
				t.Fatalf("返回 %d 条，期望 [%d,4]", len(got), tt.minLen)
			}
			for _, s := range got {
				if !strings.Contains(s.Source, ":") || s.Excerpt == "" || strings.HasPrefix(s.Excerpt, "来源：") {
					t.Errorf("来源条目不合要求：%+v", s)
				}
			}
		})
	}
}

func TestStaticIndex(t *testing.T) {
	h := staticHandler()
	tests := []struct {
		path, want string
	}{
		{"/", "<title>团队知识助手</title>"},
		{"/app.js", "/api/memories"},
		{"/style.css", "body"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tt.want) {
				t.Errorf("GET %s = %d，正文不含 %q", tt.path, rec.Code, tt.want)
			}
		})
	}
}

// 未认证请求在触及依赖前就被拒绝，因此空 handler 即可验证。
func TestAPIUnauthenticated(t *testing.T) {
	mux := http.NewServeMux()
	productRoutes(mux, &handler{})
	routes := []struct{ method, path string }{
		{"POST", "/api/agent"},
		{"GET", "/api/memories?q=x"},
		{"DELETE", "/api/memories/abc"},
		{"GET", "/api/sources?q=x"},
	}
	tokens := []struct{ name, header string }{
		{"无令牌", ""},
		{"非法令牌", "Bearer ../x"},
		{"Basic", "Basic u1"},
	}
	for _, rt := range routes {
		for _, tk := range tokens {
			t.Run(rt.method+" "+rt.path+" "+tk.name, func(t *testing.T) {
				req := httptest.NewRequest(rt.method, rt.path, nil)
				if tk.header != "" {
					req.Header.Set("Authorization", tk.header)
				}
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"unauthenticated"`) {
					t.Errorf("状态 = %d，正文 %s，期望 401 unauthenticated", rec.Code, rec.Body)
				}
			})
		}
	}
}

func TestProblemMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"记忆不存在", agentErrors.New(agentErrors.CodeStoreNotFound, "x"), http.StatusNotFound, "not_found"},
		{"通用不存在", agentErrors.New(agentErrors.CodeNotFound, "x"), http.StatusNotFound, "not_found"},
		{"管理面不存在", agentErrors.ErrManagementNotFound, http.StatusNotFound, "not_found"},
		{"无权限", agentErrors.ErrManagementForbidden, http.StatusForbidden, "forbidden"},
		{"冲突", agentErrors.ErrManagementConflict, http.StatusConflict, "conflict"},
		{"依赖不可用", fmt.Errorf("包装：%w", agentErrors.ErrManagementUnavailable), http.StatusServiceUnavailable, "unavailable"},
		{"容量已满", agentErrors.ErrManagementBusy, http.StatusServiceUnavailable, "busy"},
		{"参数无效", agentErrors.ErrManagementInvalid, http.StatusBadRequest, "invalid_request"},
		{"未知错误", errors.New("boom"), http.StatusInternalServerError, "internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := authed(func(http.ResponseWriter, *http.Request, mg.Actor) error { return tt.err })
			req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
			req.Header.Set("Authorization", "Bearer u1")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			var body struct {
				Error struct{ Code, Message string } `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != tt.want || body.Error.Code != tt.code {
				t.Errorf("状态 = %d、code = %q，期望 %d、%q", rec.Code, body.Error.Code, tt.want, tt.code)
			}
			if strings.Contains(body.Error.Message, "ingest") || strings.Contains(body.Error.Message, "boom") {
				t.Errorf("通用映射泄漏了不相关或内部信息：%q", body.Error.Message)
			}
		})
	}
}
