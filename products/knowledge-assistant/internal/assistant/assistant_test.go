package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kart-io/goagent/interfaces"
	mg "github.com/kart-io/goagent/management"
	"github.com/kart-io/goagent/memory"

	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/chat"
	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/config"
	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/docs"
	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/embed"
)

const corpus = "../../testdata/corpus"

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

func migrate(t *testing.T, c config.Config) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, c.PGURL)
	if err != nil {
		t.Fatalf("连接 PG: %v", err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
}

type env struct {
	stores *Stores
	svc    *mg.Service
	deps   Deps
}

// newEnv 迁移、装配存储、重建一次测试专用 alias 的文档索引；worker 为真时另起 Worker。
// 不跑任务的测试不起 Worker：Worker.Run 在认领槽位途中被取消会留下无人释放的槽位（FINDINGS F19）。
func newEnv(t *testing.T, worker bool) *env {
	t.Helper()
	c := testConfig(t)
	ctx := context.Background()
	migrate(t, c)

	e := embed.NewHash(3072)
	model := chat.NewScript()
	s, err := Open(ctx, c, model, e)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Errorf("关闭 Stores: %v", err)
		}
	})

	idx, err := docs.NewIndex(ctx, c.QdrantAddr, fmt.Sprintf("t_%08x", rand.Uint32()), e, s.Pool)
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

	deps := Deps{Stores: s, Model: model, Index: idx, CorpusDir: corpus, Embedder: e}
	svc, err := mg.New(mg.Config{
		Store:         s.Manage,
		Sessions:      s.Sessions,
		HistoryReader: s.Manage,
		Authorizer:    mg.TenantAuthorizer{},
		Factories:     map[string]mg.Factory{RunnerKind: Factory(deps)},
		Report:        func(stage string, err error) { t.Logf("管理诊断：%s %v", stage, err) },
	})
	if err != nil {
		t.Fatalf("mg.New: %v", err)
	}
	if !worker {
		return &env{stores: s, svc: svc, deps: deps}
	}
	w := mg.NewWorker(svc)
	wctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(wctx) }()
	t.Cleanup(func() {
		w.BeginShutdown()
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		if err := w.Shutdown(sctx); err != nil {
			t.Errorf("Worker 关闭：%v", err)
		}
		// 槽位被同库的其他 Worker 占用时 Run 立即返回 ErrManagementOwner，表现为任务一直不动。
		if err := <-done; err != nil {
			t.Errorf("Worker 退出：%v", err)
		}
	})
	return &env{stores: s, svc: svc, deps: deps}
}

func actor(name string) mg.Actor { return mg.Actor{TenantID: name, Subject: name} }

// tenant 生成本次运行独有的租户，测试之间及复用的库里互不干扰。
func tenant(prefix string) string { return prefix + "-" + uuid.NewString() }

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

// submit 为 a 确保 Agent、建会话并提交 input，返回任务。
func (e *env) submit(t *testing.T, a mg.Actor, input string) mg.Task {
	t.Helper()
	ctx := context.Background()
	agent, err := EnsureAgent(ctx, e.svc, a)
	if err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	se, err := e.svc.CreateSession(ctx, a, mg.CreateSessionRequest{IdempotencyKey: uuid.NewString(), AgentID: agent.ID})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitFor(t, "会话就绪", func() (bool, error) {
		se, err = e.svc.GetSession(ctx, a, se.ID)
		return se.Status == "ready", err
	})
	raw, _ := json.Marshal(input)
	task, err := e.svc.SubmitTask(ctx, a, mg.SubmitTaskRequest{IdempotencyKey: uuid.NewString(), SessionID: se.ID, Input: raw})
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	return task
}

func (e *env) waitState(t *testing.T, a mg.Actor, id string, want mg.TaskState) mg.Task {
	t.Helper()
	var task mg.Task
	waitFor(t, "任务进入 "+string(want), func() (bool, error) {
		var err error
		task, err = e.svc.GetTask(context.Background(), a, id)
		if err != nil {
			return false, err
		}
		if task.State == want {
			return true, nil
		}
		if task.State.Terminal() || task.State == mg.Attention {
			return false, fmt.Errorf("任务停在 %s（%s）%s", task.State, task.ErrorKind, task.Result)
		}
		return false, nil
	})
	return task
}

func TestMigrateTwice(t *testing.T) {
	c := testConfig(t)
	for i := range 2 {
		t.Run(fmt.Sprintf("第%d次", i+1), func(t *testing.T) { migrate(t, c) })
	}
}

func TestEnsureAgentIdempotent(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	ids := map[string]string{}
	tenants := map[string]string{"ea1": tenant("ea1"), "ea2": tenant("ea2")}
	tests := []struct {
		actor string
		same  string // 非空时期望与该 actor 先前得到的 ID 相同
		other string // 非空时期望与该 actor 的 ID 不同
	}{
		{actor: "ea1"},
		{actor: "ea1", same: "ea1"},
		{actor: "ea2", other: "ea1"},
	}
	for i, tt := range tests {
		t.Run(fmt.Sprintf("%d_%s", i, tt.actor), func(t *testing.T) {
			agent, err := EnsureAgent(ctx, e.svc, actor(tenants[tt.actor]))
			if err != nil {
				t.Fatalf("EnsureAgent: %v", err)
			}
			if agent.ID == "" || agent.PublishedVersion != AgentVersion {
				t.Fatalf("Agent = %+v，期望已发布 %s", agent, AgentVersion)
			}
			if tt.same != "" && agent.ID != ids[tt.same] {
				t.Errorf("重复调用得到 %s，期望 %s", agent.ID, ids[tt.same])
			}
			if tt.other != "" && agent.ID == ids[tt.other] {
				t.Errorf("不同租户得到相同 Agent %s", agent.ID)
			}
			ids[tt.actor] = agent.ID
		})
	}
}

func recall(t *testing.T, m *memory.Memory, ns, text string) []memory.Scored {
	t.Helper()
	got, err := m.Recall(memory.WithNamespace(context.Background(), ns), memory.Query{Text: text, K: 5})
	if err != nil {
		t.Fatalf("Recall(%s): %v", ns, err)
	}
	return got
}

func TestNamespaceBoundInRun(t *testing.T) {
	e := newEnv(t, true)
	ns := map[string]string{"u1": tenant("u1"), "u2": tenant("u2")}
	u1 := actor(ns["u1"])
	task := e.submit(t, u1, "记住：我们用 pgstore")
	e.waitState(t, u1, task.ID, mg.Succeeded)

	tests := []struct {
		ns   string
		want bool
	}{
		{"u1", true},
		{"u2", false},
	}
	for _, tt := range tests {
		t.Run(tt.ns, func(t *testing.T) {
			got := recall(t, e.stores.Memory, ns[tt.ns], "pgstore")
			hit := false
			for _, s := range got {
				hit = hit || strings.Contains(s.Content, "我们用 pgstore")
			}
			if hit != tt.want {
				t.Errorf("命名空间 %s 命中 = %v，期望 %v；结果 %+v", tt.ns, hit, tt.want, got)
			}
		})
	}
}

func purgeRuns(t *testing.T, e *env, tenant string) int {
	t.Helper()
	var n int
	if err := e.stores.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ka_tool_runs WHERE tool='purge' AND tenant=$1`, tenant).Scan(&n); err != nil {
		t.Fatalf("统计 ka_tool_runs: %v", err)
	}
	return n
}

func TestPurgeNeedsApproval(t *testing.T) {
	e := newEnv(t, true)
	ctx := context.Background()
	tenant := tenant("u1")
	u1 := actor(tenant)
	if _, err := e.stores.Memory.Remember(memory.WithNamespace(ctx, tenant), memory.Record{
		Content: "部署在 k8s", Kind: memory.KindFact, Importance: 0.5, Pinned: true,
	}); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	task := e.submit(t, u1, "清空我的记忆")
	e.waitState(t, u1, task.ID, mg.WaitingApproval)
	page, err := e.svc.ListApprovals(ctx, u1, mg.Query{TaskID: task.ID, Statuses: []string{"pending"}})
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("待审批 = %d 条，期望 1", len(page.Items))
	}
	ap := page.Items[0]
	if _, err := e.svc.DecideApproval(ctx, u1, mg.DecisionRequest{
		IdempotencyKey: "d-" + ap.ID, ApprovalID: ap.ID, ExpectedRevision: ap.Revision, Decision: "approve", Reason: "本人要求",
	}); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	e.waitState(t, u1, task.ID, mg.Succeeded)

	if runs := purgeRuns(t, e, tenant); runs != 1 {
		t.Errorf("purge 执行 %d 次，期望 1", runs)
	}
	pinned, err := e.stores.Memory.Pinned(memory.WithNamespace(ctx, tenant))
	if err != nil {
		t.Fatalf("Pinned: %v", err)
	}
	if len(pinned) != 0 {
		t.Errorf("Pinned 应为空，实际 %+v", pinned)
	}
	if got := recall(t, e.stores.Memory, tenant, "k8s"); len(got) != 0 {
		t.Errorf("Recall 应为空，实际 %+v", got)
	}
}

// 审批触发的重建以语料内容为键：语料未变时不同任务重放得到同一集合，之后的 ingest 也不必重新嵌入。
func TestRebuildToolKeyedByContent(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	var rebuild approvalTool
	for _, tool := range e.deps.approvalTools() {
		if tool.name == "rebuild_docs_index" {
			rebuild = tool
		}
	}
	first, err := rebuild.run(ctx, &interfaces.ToolInput{ExecutionKey: "exec-1"})
	if err != nil {
		t.Fatalf("第一次重建: %v", err)
	}
	second, err := rebuild.run(ctx, &interfaces.ToolInput{ExecutionKey: "exec-2"})
	if err != nil {
		t.Fatalf("第二次重建: %v", err)
	}
	if first != second {
		t.Fatalf("语料未变时两次重建的集合不同: %s / %s", first, second)
	}
	if cur, err := e.deps.Index.Current(ctx); err != nil || cur != first {
		t.Fatalf("Current = %q, %v，期望 %s", cur, err, first)
	}
}
