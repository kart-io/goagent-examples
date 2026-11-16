// Package web 提供知识助手的 HTTP 入口：管理 API、产品接口与静态页面。
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"regexp"
	"strings"

	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/interfaces"
	mg "github.com/kart-io/goagent/management"
	"github.com/kart-io/goagent/management/httpapi"
	"github.com/kart-io/goagent/memory"

	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/assistant"
	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/docs"
)

//go:embed static
var static embed.FS

// userName 同时用作租户与记忆命名空间，限定字符集，防止用户名里混入路径或分隔符。
var userName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

const (
	recallK    = 20
	sourcesK   = 4
	excerptLen = 200 // 摘录按 rune 截断，够页面判断来源相关性即可
)

// ActorFromRequest 把 "Bearer <用户名>" 解析为租户与主体都是该用户名的 Actor。
// 这是演示用的身份方案：令牌就是用户名，没有真实认证。
func ActorFromRequest(r *http.Request) (mg.Actor, error) {
	name, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !userName.MatchString(name) {
		return mg.Actor{}, agentErrors.ErrManagementUnauthenticated
	}
	return mg.Actor{TenantID: name, Subject: name}, nil
}

// NewHandler 组装全部路由；/v1/ 原样交给 management 的 httpapi。
func NewHandler(s *mg.Service, st *assistant.Stores, idx *docs.Index) (http.Handler, error) {
	api, err := httpapi.New(s, httpapi.Config{ActorFromRequest: ActorFromRequest})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", api)
	productRoutes(mux, &handler{svc: s, mem: st.Memory, idx: idx})
	mux.Handle("/", staticHandler())
	return mux, nil
}

// productRoutes 挂载 /api/*；鉴权先于处理函数执行，未认证请求不会触及依赖。
func productRoutes(mux *http.ServeMux, h *handler) {
	mux.HandleFunc("POST /api/agent", authed(h.ensureAgent))
	mux.HandleFunc("GET /api/memories", authed(h.memories))
	mux.HandleFunc("DELETE /api/memories/{id}", authed(h.forget))
	mux.HandleFunc("GET /api/sources", authed(h.sources))
}

// staticHandler 提供嵌入的页面，不依赖任何存储。
func staticHandler() http.Handler {
	page, err := fs.Sub(static, "static")
	if err != nil {
		panic(err) // "static" 由 go:embed 保证存在
	}
	return http.FileServer(http.FS(page))
}

type handler struct {
	svc *mg.Service
	mem *memory.Memory
	idx *docs.Index
}

func authed(fn func(http.ResponseWriter, *http.Request, mg.Actor) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who, err := ActorFromRequest(r)
		if err == nil {
			err = fn(w, r, who)
		}
		if err != nil {
			problem(w, r, err)
		}
	}
}

func (h *handler) ensureAgent(w http.ResponseWriter, r *http.Request, who mg.Actor) error {
	agent, err := assistant.EnsureAgent(r.Context(), h.svc, who)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{"agent_id": agent.ID, "version": agent.PublishedVersion})
	return nil
}

// memories 返回置顶记忆和按 q 召回的记忆。memory 没有列举接口（FINDINGS F4），只能这样拼出"我的记忆"。
func (h *handler) memories(w http.ResponseWriter, r *http.Request, who mg.Actor) error {
	ctx := memory.WithNamespace(r.Context(), who.TenantID)
	pinned, err := h.mem.Pinned(ctx)
	if err != nil {
		return err
	}
	matches := []memory.Scored{}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		if matches, err = h.mem.Recall(ctx, memory.Query{Text: q, K: recallK}); err != nil {
			return err
		}
	}
	if pinned == nil {
		pinned = []memory.Record{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pinned": pinned, "matches": matches})
	return nil
}

func (h *handler) forget(w http.ResponseWriter, r *http.Request, who mg.Actor) error {
	if err := h.mem.Forget(memory.WithNamespace(r.Context(), who.TenantID), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

type source struct {
	Source  string  `json:"source"`
	Score   float64 `json:"score"`
	Excerpt string  `json:"excerpt"`
}

func (h *handler) sources(w http.ResponseWriter, r *http.Request, _ mg.Actor) error {
	out := []source{}
	// 空查询直接返回：Gemini 嵌入拒绝空文本。
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, out)
		return nil
	}
	ret, err := h.idx.Retriever(r.Context(), sourcesK)
	if agentErrors.IsCode(err, agentErrors.CodeNotFound) {
		writeProblem(w, http.StatusNotFound, "index_missing", "文档索引不存在，请先运行 ka ingest")
		return nil
	}
	if err != nil {
		return err
	}
	found, err := ret.Retrieve(r.Context(), interfaces.RetrievalRequest{Query: q})
	if err != nil {
		return err
	}
	for _, d := range found {
		src, _ := d.Metadata["source"].(string)
		out = append(out, source{Source: src, Score: d.Score, Excerpt: excerpt(d.PageContent)})
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// excerpt 去掉写给模型看的首行"来源："，页面已单独展示来源。
func excerpt(text string) string {
	if first, rest, ok := strings.Cut(text, "\n"); ok && strings.HasPrefix(first, "来源：") {
		text = rest
	}
	text = strings.TrimSpace(text)
	if r := []rune(text); len(r) > excerptLen {
		text = string(r[:excerptLen]) + "…"
	}
	return text
}

// problems 按顺序匹配错误；响应形状与 httpapi 一致，页面可统一处理。
var problems = []struct {
	match   func(error) bool
	status  int
	code    string
	message string
}{
	{is(agentErrors.ErrManagementUnauthenticated), http.StatusUnauthorized, "unauthenticated", "需要认证"},
	{is(agentErrors.ErrManagementForbidden), http.StatusForbidden, "forbidden", "没有操作权限"},
	{is(agentErrors.ErrManagementNotFound), http.StatusNotFound, "not_found", "资源不存在"},
	{code(agentErrors.CodeStoreNotFound), http.StatusNotFound, "not_found", "资源不存在"},
	{code(agentErrors.CodeNotFound), http.StatusNotFound, "not_found", "资源不存在"},
	{is(agentErrors.ErrManagementUnavailable), http.StatusServiceUnavailable, "unavailable", "依赖暂不可用"},
	{is(agentErrors.ErrManagementBusy), http.StatusServiceUnavailable, "busy", "容量已满，请稍后重试"},
	{is(agentErrors.ErrManagementConflict), http.StatusConflict, "conflict", "修订号、状态或幂等内容冲突"},
	{is(agentErrors.ErrManagementInvalid), http.StatusBadRequest, "invalid_request", "请求参数无效"},
	{code(agentErrors.CodeInvalidInput), http.StatusBadRequest, "invalid_request", "请求参数无效"},
}

func is(target error) func(error) bool { return func(err error) bool { return errors.Is(err, target) } }

func code(c agentErrors.ErrorCode) func(error) bool {
	return func(err error) bool { return agentErrors.IsCode(err, c) }
}

func problem(w http.ResponseWriter, r *http.Request, err error) {
	status, c, msg := http.StatusInternalServerError, "internal", "服务内部错误"
	for _, p := range problems {
		if p.match(err) {
			status, c, msg = p.status, p.code, p.message
			break
		}
	}
	if status == http.StatusInternalServerError {
		log.Printf("%s %s 失败：%v", r.Method, r.URL.Path, err)
	}
	writeProblem(w, status, c, msg)
}

func writeProblem(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("写响应失败：%v", err)
	}
}
