//go:build linux

// Package e2e 以两个 ka serve 子进程跑规格第 6 节的离线验收场景。
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/qdrant/go-client/qdrant"
	goredis "github.com/redis/go-redis/v9"

	mg "github.com/kart-io/goagent/management"

	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/docs"
	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/embed"
)

const (
	tagKey     = "6b612d6532652d7461672d6b65792d3332627974657321212121212121212121" // 固定 32 字节
	sourceHint = "goagent-contrib:durable/pgstore/"
	pollLimit  = 60 * time.Second
)

// bin 是 TestMain 编译出的 ka；为空表示缺少外部服务，各测试自行跳过。
var bin string

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if os.Getenv("KA_TEST_PG_URL") == "" {
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "ka-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "创建临时目录：", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	bin = filepath.Join(dir, "ka")
	build := exec.Command("go", "build", "-o", bin, "./cmd/ka")
	build.Dir = ".."
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "编译 ka：", err)
		return 1
	}
	return m.Run()
}

// suite 是一次验收运行共享的环境：同一库、同一 Redis 前缀与文档 alias。
type suite struct {
	env    []string
	alias  string
	prefix string
	pool   *pgxpool.Pool
	redis  *goredis.Client
	a, b   *proc
	root   *testing.T // 子进程的清理挂在顶层测试上，场景 4 在子测试里启动的 b2 要供后续场景使用
}

func newSuite(t *testing.T) *suite {
	t.Helper()
	pg, rd, qd := os.Getenv("KA_TEST_PG_URL"), os.Getenv("KA_TEST_REDIS_ADDR"), os.Getenv("KA_TEST_QDRANT_ADDR")
	if bin == "" || pg == "" || rd == "" || qd == "" {
		t.Skip("需经 scripts/test.sh 运行")
	}
	corpus, err := filepath.Abs("../testdata/corpus")
	if err != nil {
		t.Fatalf("语料路径：%v", err)
	}
	tag := fmt.Sprintf("%08x", rand.Uint32())
	s := &suite{alias: "e2e_" + tag, prefix: "ka:e2e:" + tag + ":", root: t}
	s.env = append(os.Environ(),
		"KA_PG_URL="+pg, "KA_REDIS_ADDR="+rd, "KA_QDRANT_ADDR="+qd,
		"KA_MODEL=script", "KA_EMBEDDER=hash", "KA_CORPUS_DIR="+corpus,
		"KA_DOCS_ALIAS="+s.alias, "KA_CACHE_PREFIX="+s.prefix, "KA_CACHE_TAG_KEY="+tagKey,
		"KA_LISTEN=127.0.0.1:0",
	)
	if s.pool, err = pgxpool.New(context.Background(), pg); err != nil {
		t.Fatalf("连接 PG：%v", err)
	}
	t.Cleanup(s.pool.Close)
	s.redis = goredis.NewClient(&goredis.Options{Addr: rd})
	t.Cleanup(func() { _ = s.redis.Close() })
	return s
}

// command 运行一次性子命令，返回合并输出；退出码非 0 时测试失败。
func (s *suite) command(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = s.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ka %s 失败：%v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// proc 是一个 ka serve 子进程。
type proc struct {
	name   string
	base   string
	cmd    *exec.Cmd
	done   chan error
	stdout *syncBuffer
	stderr *syncBuffer
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// live 记录所有启动过的子进程，任务失败时一并输出它们的 stderr 供定位。
var live struct {
	mu    sync.Mutex
	procs []*proc
}

var startedLine = regexp.MustCompile(`已启动：(http://\S+)（`)

// start 以子进程运行 ka serve，等 GET / 返回 200 才返回；进程在测试结束时经 SIGTERM 优雅退出。
func (s *suite) start(t *testing.T, name string, worker bool) *proc {
	t.Helper()
	w := "0"
	if worker {
		w = "1"
	}
	p := &proc{name: name, cmd: exec.Command(bin, "serve"), done: make(chan error, 1), stdout: &syncBuffer{}, stderr: &syncBuffer{}}
	p.cmd.Env = append(slices.Clone(s.env), "WORKER="+w)
	p.cmd.Stderr = p.stderr
	// go test 超时被杀时子进程随之退出，否则 WORKER=1 的孤儿进程会一直占着槽位。
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("%s stdout：%v", name, err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("启动 %s：%v", name, err)
	}
	addr := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			_, _ = fmt.Fprintln(p.stdout, sc.Text())
			if m := startedLine.FindStringSubmatch(sc.Text()); m != nil {
				addr <- m[1]
			}
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	go func() { p.done <- p.cmd.Wait() }()
	live.mu.Lock()
	live.procs = append(live.procs, p)
	live.mu.Unlock()
	s.root.Cleanup(func() { p.Stop(s.root) })

	select {
	case p.base = <-addr:
	case err := <-p.done:
		p.done <- err
		t.Fatalf("%s 启动即退出：%v\n%s", name, err, p.stderr)
	case <-time.After(pollLimit):
		t.Fatalf("%s 未输出监听地址\n%s", name, p.stderr)
	}
	waitFor(t, name+" 首页可访问", func() (bool, error) {
		res, err := http.Get(p.base + "/")
		if err != nil {
			return false, nil
		}
		_ = res.Body.Close()
		return res.StatusCode == http.StatusOK, nil
	})
	return p
}

func (p *proc) exited() bool {
	select {
	case err := <-p.done:
		p.done <- err
		return true
	default:
		return false
	}
}

// Kill 发 SIGKILL，模拟实例崩溃，不走优雅退出。
func (p *proc) Kill(t *testing.T) {
	t.Helper()
	if p.exited() {
		return
	}
	_ = p.cmd.Process.Kill()
	err := <-p.done
	p.done <- err
}

// Stop 发 SIGTERM 并等待退出；优雅退出的退出码必须为 0，否则 Worker 槽位可能没有释放。
func (p *proc) Stop(t *testing.T) {
	t.Helper()
	if p.exited() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-p.done:
		p.done <- err
		if err != nil {
			t.Errorf("%s 优雅退出失败：%v\n%s", p.name, err, p.stderr)
		}
	case <-time.After(30 * time.Second):
		_ = p.cmd.Process.Kill()
		t.Errorf("%s 30 秒内未退出，已强杀\n%s", p.name, p.stderr)
	}
}

func dumpProcs(t *testing.T) {
	t.Helper()
	live.mu.Lock()
	defer live.mu.Unlock()
	for _, p := range live.procs {
		t.Logf("==== %s stderr ====\n%s", p.name, p.stderr)
	}
}

func waitFor(t *testing.T, what string, ok func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(pollLimit)
	for {
		done, err := ok()
		if err != nil {
			dumpProcs(t)
			t.Fatalf("等待%s：%v", what, err)
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			dumpProcs(t)
			t.Fatalf("等待%s超时", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// call 以 user 身份请求 base+path，out 非 nil 时解码 2xx 响应体；返回状态码与原始响应体。
func call(t *testing.T, base, user, method, path string, body, out any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求：%v", err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		t.Fatalf("构造请求：%v", err)
	}
	req.Header.Set("Authorization", "Bearer "+user)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	res, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s：%v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	if out != nil && res.StatusCode < 300 && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("解码 %s %s 响应 %s：%v", method, path, raw, err)
		}
	}
	return res.StatusCode, raw
}

func mustCall(t *testing.T, base, user, method, path string, body, out any, want int) {
	t.Helper()
	if code, raw := call(t, base, user, method, path, body, out); code != want {
		t.Fatalf("%s %s = %d %s，期望 %d", method, path, code, raw, want)
	}
}

// ask 走页面的对话流程：确保 Agent、建会话并轮询到 ready、提交任务，轮询到终态或 waiting_approval。
func ask(t *testing.T, base, user, text string) mg.Task {
	t.Helper()
	var agent struct {
		AgentID string `json:"agent_id"`
	}
	mustCall(t, base, user, "POST", "/api/agent", nil, &agent, http.StatusOK)
	var se mg.Session
	mustCall(t, base, user, "POST", "/v1/sessions", map[string]any{"agent_id": agent.AgentID}, &se, http.StatusAccepted)
	waitFor(t, "会话就绪", func() (bool, error) {
		mustCall(t, base, user, "GET", "/v1/sessions/"+se.ID, nil, &se, http.StatusOK)
		return se.Status == "ready", nil
	})
	var task mg.Task
	mustCall(t, base, user, "POST", "/v1/tasks", map[string]any{"session_id": se.ID, "input": text}, &task, http.StatusAccepted)
	return waitTask(t, base, user, task.ID, func(task mg.Task) bool {
		return task.State.Terminal() || task.State == mg.WaitingApproval
	})
}

// waitTask 轮询任务直到 done 成立；任务 failed 或需人工处理时输出完整状态与子进程 stderr。
func waitTask(t *testing.T, base, user, id string, done func(mg.Task) bool) mg.Task {
	t.Helper()
	var task mg.Task
	waitFor(t, "任务 "+id, func() (bool, error) {
		mustCall(t, base, user, "GET", "/v1/tasks/"+id, nil, &task, http.StatusOK)
		if task.State == mg.Failed || task.State == mg.Attention {
			raw, _ := json.MarshalIndent(task, "", "  ")
			return false, fmt.Errorf("任务停在 %s（ErrorKind=%s）\n%s", task.State, task.ErrorKind, raw)
		}
		return done(task), nil
	})
	return task
}

func wantState(t *testing.T, task mg.Task, want mg.TaskState) {
	t.Helper()
	if task.State != want {
		raw, _ := json.MarshalIndent(task, "", "  ")
		dumpProcs(t)
		t.Fatalf("任务状态 %s，期望 %s\n%s", task.State, want, raw)
	}
}

// resultText 把任务结果还原成文本：结果通常是 JSON 字符串。
func resultText(task mg.Task) string {
	var s string
	if json.Unmarshal(task.Result, &s) == nil {
		return s
	}
	return string(task.Result)
}

// pendingApproval 返回任务唯一的待审批项及原始 JSON。
func pendingApproval(t *testing.T, base, user, taskID string) (mg.Approval, string) {
	t.Helper()
	var page mg.Page[mg.Approval]
	code, raw := call(t, base, user, "GET", "/v1/approvals?status=pending&task_id="+url.QueryEscape(taskID), nil, &page)
	if code != http.StatusOK || len(page.Items) != 1 {
		t.Fatalf("待审批 = %d %s，期望恰好 1 条", code, raw)
	}
	return page.Items[0], string(raw)
}

func decide(t *testing.T, base, user string, ap mg.Approval, decision string) {
	t.Helper()
	mustCall(t, base, user, "POST", "/v1/approvals/"+ap.ID+"/decision",
		map[string]any{"expected_revision": ap.Revision, "decision": decision, "reason": "e2e"}, nil, http.StatusOK)
}

// tenant 生成本次运行独有的用户名：库可能被复用，固定用户名会读到历史数据。
func tenant(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

type memories struct {
	Pinned  []map[string]any `json:"pinned"`
	Matches []map[string]any `json:"matches"`
}

// findMemory 返回置顶或召回结果中正文含 text 的记录 ID，没有则为空。
func findMemory(t *testing.T, base, user, q, text string) string {
	t.Helper()
	var m memories
	mustCall(t, base, user, "GET", "/api/memories?q="+url.QueryEscape(q), nil, &m, http.StatusOK)
	for _, r := range slices.Concat(m.Pinned, m.Matches) {
		if c, _ := r["content"].(string); strings.Contains(c, text) {
			id, _ := r["id"].(string)
			return id
		}
	}
	return ""
}

func remember(t *testing.T, base, user, fact string) {
	t.Helper()
	wantState(t, ask(t, base, user, "记住："+fact), mg.Succeeded)
}

// snapshotReads 返回 durable 快照表的累计扫描次数。统计由各后端异步上报，有秒级延迟，只用来判断是否增长。
func (s *suite) snapshotReads(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := s.pool.QueryRow(context.Background(),
		`SELECT coalesce(seq_scan, 0) + coalesce(idx_scan, 0) FROM pg_stat_user_tables WHERE relname = 'goagent_durable_snapshot'`).Scan(&n); err != nil {
		t.Fatalf("读取 durable 快照表统计：%v", err)
	}
	return n
}

func (s *suite) toolRuns(t *testing.T, tool, user string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ka_tool_runs WHERE tool=$1 AND tenant=$2`, tool, user).Scan(&n); err != nil {
		t.Fatalf("统计 ka_tool_runs：%v", err)
	}
	return n
}

func (s *suite) cacheKeys(t *testing.T) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	iter := s.redis.Scan(context.Background(), 0, s.prefix+"*", 100).Iterator()
	for iter.Next(context.Background()) {
		keys[iter.Val()] = true
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("SCAN Redis：%v", err)
	}
	return keys
}

// approve 让 user 在 p 上提交 text，确认进入待审批后按 decision 处理，返回结束后的任务与待审批项原始 JSON。
func approve(t *testing.T, p *proc, user, text, decision string) (mg.Task, string) {
	t.Helper()
	task := ask(t, p.base, user, text)
	wantState(t, task, mg.WaitingApproval)
	ap, raw := pendingApproval(t, p.base, user, task.ID)
	decide(t, p.base, user, ap, decision)
	return waitTask(t, p.base, user, task.ID, func(task mg.Task) bool { return task.State.Terminal() }), raw
}

func TestAcceptance(t *testing.T) {
	s := newSuite(t)
	s.command(t, "migrate")
	t.Log(s.command(t, "ingest"))
	s.a = s.start(t, "a", true)
	s.b = s.start(t, "b", false)
	// 放在任何任务之前：此时只有记忆维护会读 durable 快照表，读次数增长即说明维护在运行。
	t.Run("TestMemoryMaintenanceRuns", func(t *testing.T) {
		for _, p := range []*proc{s.a, s.b} {
			if !strings.Contains(p.stdout.String(), "memory=maintenance") {
				t.Errorf("%s 未以记忆维护模式启动：%s", p.name, p.stdout)
			}
		}
		before := s.snapshotReads(t)
		waitFor(t, "记忆维护读取 durable 快照", func() (bool, error) {
			return s.snapshotReads(t) > before, nil
		})
	})
	// 先确认 a 已认领 Worker 槽位且能执行任务，之后才可能对它发信号（见 Task 7 FINDING：认领途中取消会泄漏槽位）。
	wantState(t, ask(t, s.a.base, tenant("probe"), "你好"), mg.Succeeded)

	t.Run("TestRAGCitesSources", func(t *testing.T) {
		task := ask(t, s.a.base, tenant("u1"), "pgstore 怎么建表")
		wantState(t, task, mg.Succeeded)
		if got := resultText(task); !strings.Contains(got, sourceHint) {
			t.Errorf("结果不含来源 %q：\n%s", sourceHint, got)
		}
	})

	t.Run("TestMemoryIsolatedPerUser", func(t *testing.T) {
		u1, u2 := tenant("u1"), tenant("u2")
		const fact = "我们用 pgstore"
		remember(t, s.a.base, u1, fact)
		tests := []struct {
			user string
			want bool
		}{
			{u1, true},
			{u2, false},
		}
		for _, tt := range tests {
			t.Run(tt.user, func(t *testing.T) {
				task := ask(t, s.b.base, tt.user, "存储选型用什么")
				wantState(t, task, mg.Succeeded)
				if got := resultText(task); strings.Contains(got, fact) != tt.want {
					t.Errorf("回显含 %q = %v，期望 %v：\n%s", fact, !tt.want, tt.want, got)
				}
			})
		}
	})

	t.Run("TestForgetAcrossInstancesErasesCache", func(t *testing.T) {
		u3 := tenant("u3")
		const fact = "部署平台是 k8s"
		remember(t, s.a.base, u3, fact)
		before := s.cacheKeys(t)
		wantState(t, ask(t, s.a.base, u3, "部署平台用什么"), mg.Succeeded)
		var added []string
		for k := range s.cacheKeys(t) {
			if !before[k] {
				added = append(added, k)
			}
		}
		if len(added) == 0 {
			t.Fatalf("提问后 Redis 中没有新增 %s* 键，模型缓存未生效", s.prefix)
		}
		t.Logf("新增缓存键 %d 个：%v", len(added), added)

		id := findMemory(t, s.b.base, u3, "部署平台", fact)
		if id == "" {
			t.Fatalf("经 b 查不到 a 写入的记忆")
		}
		mustCall(t, s.b.base, u3, "DELETE", "/api/memories/"+url.PathEscape(id), nil, nil, http.StatusNoContent)
		if findMemory(t, s.a.base, u3, "部署平台", fact) != "" {
			t.Errorf("经 b 删除后 a 仍能查到该记忆")
		}
		// 条目键（e:）承载缓存内容，必须全部删除。条目同时挂着会话标签，该标签的令牌（g:）与成员集合（t:）
		// 按 cache/redis 协议不随记忆标签删除，只含摘要不含内容，到 TTL 自行过期；这里只要求它们不是永久键。
		ctx := context.Background()
		entries := 0
		for _, k := range added {
			if !strings.HasPrefix(k, s.prefix+"e:") {
				if !strings.HasPrefix(k, s.prefix+"g:") && !strings.HasPrefix(k, s.prefix+"t:") {
					t.Errorf("新增键 %s 既非条目（e:）也非标签令牌（g:）或成员集合（t:）", k)
					continue
				}
				ttl, err := s.redis.PTTL(ctx, k).Result()
				if err != nil {
					t.Fatalf("PTTL %s：%v", k, err)
				}
				// 不存在时 PTTL 为 -2，未设过期时间为 -1（go-redis 原样返回为纳秒数）。
				if ttl == -1 {
					t.Errorf("标签键 %s 没有过期时间", k)
				}
				continue
			}
			entries++
			n, err := s.redis.Exists(ctx, k).Result()
			if err != nil {
				t.Fatalf("EXISTS %s：%v", k, err)
			}
			if n != 0 {
				t.Errorf("缓存条目 %s 在删除记忆后仍存在", k)
			}
		}
		if entries == 0 {
			t.Errorf("新增键中没有缓存条目（%se:*）：%v", s.prefix, added)
		}
	})

	t.Run("TestApprovalSurvivesKillAndFailover", func(t *testing.T) {
		u1 := tenant("u1")
		const fact = "值班表在共享盘"
		remember(t, s.a.base, u1, fact)
		if findMemory(t, s.a.base, u1, "值班表", fact) == "" {
			t.Fatalf("清空前经 a 查不到刚写入的记忆，后续“清空后为空”的断言没有意义")
		}
		task := ask(t, s.a.base, u1, "清空我的记忆")
		wantState(t, task, mg.WaitingApproval)

		s.a.Kill(t)
		t.Log(s.command(t, "recover-worker", "-reason", "e2e", "-confirm-stopped"))
		s.b.Stop(t)
		s.b = s.start(t, "b2", true)
		wantState(t, ask(t, s.b.base, tenant("probe"), "你好"), mg.Succeeded)

		ap, raw := pendingApproval(t, s.b.base, u1, task.ID)
		t.Logf("GET /v1/approvals?status=pending 原始响应：%s", raw)
		decide(t, s.b.base, u1, ap, "approve")
		task = waitTask(t, s.b.base, u1, task.ID, func(task mg.Task) bool { return task.State.Terminal() })
		wantState(t, task, mg.Succeeded)

		if n := s.toolRuns(t, "purge", u1); n != 1 {
			t.Errorf("该用户的 purge 执行 %d 次，期望 1", n)
		}
		if id := findMemory(t, s.b.base, u1, "值班表", fact); id != "" {
			t.Errorf("清空后仍能召回记忆 %s", id)
		}
	})

	t.Run("TestRejectKeepsMemory", func(t *testing.T) {
		u4 := tenant("u4")
		const fact = "周报周五交"
		remember(t, s.b.base, u4, fact)
		task, raw := approve(t, s.b, u4, "清空我的记忆", "reject")
		t.Logf("GET /v1/approvals?status=pending 原始响应：%s", raw)
		wantState(t, task, mg.Succeeded)
		if got := resultText(task); !strings.Contains(got, "用户拒绝，未清空") {
			t.Errorf("结果不含拒绝说明：\n%s", got)
		}
		if findMemory(t, s.b.base, u4, "周报", fact) == "" {
			t.Errorf("拒绝后记忆不可召回")
		}
		if n := s.toolRuns(t, "purge", u4); n != 0 {
			t.Errorf("拒绝后 purge 仍执行了 %d 次", n)
		}
	})

	t.Run("TestRebuildIndexViaApproval", func(t *testing.T) {
		ctx := context.Background()
		qd := os.Getenv("KA_TEST_QDRANT_ADDR")
		idx, err := docs.NewIndex(ctx, qd, s.alias, embed.NewHash(3072), s.pool)
		if err != nil {
			t.Fatalf("NewIndex：%v", err)
		}
		defer func() { _ = idx.Close() }()
		old, err := idx.Current(ctx)
		if err != nil {
			t.Fatalf("Current：%v", err)
		}

		u1 := tenant("u1")
		task, _ := approve(t, s.b, u1, "重建文档索引", "approve")
		wantState(t, task, mg.Succeeded)

		cur, err := idx.Current(ctx)
		if err != nil {
			t.Fatalf("Current：%v", err)
		}
		if cur == old {
			t.Errorf("重建后 alias 仍指向 %s", old)
		}
		if exists := collectionExists(t, qd, old); exists {
			t.Errorf("旧集合 %s 未删除", old)
		}
		task = ask(t, s.b.base, u1, "pgstore 怎么建表")
		wantState(t, task, mg.Succeeded)
		if got := resultText(task); !strings.Contains(got, sourceHint) {
			t.Errorf("切换后结果不含来源 %q：\n%s", sourceHint, got)
		}
	})

	live.mu.Lock()
	for _, p := range live.procs {
		if strings.Contains(p.stderr.String(), "记忆维护：") {
			t.Errorf("%s 的记忆维护报错", p.name)
		}
	}
	live.mu.Unlock()

	if t.Failed() {
		dumpProcs(t)
	}
}

func collectionExists(t *testing.T, addr, name string) bool {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("Qdrant 地址 %q 无效：%v", addr, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("Qdrant 端口 %q 无效：%v", port, err)
	}
	cli, err := qdrant.NewClient(&qdrant.Config{Host: host, Port: p})
	if err != nil {
		t.Fatalf("Qdrant 客户端：%v", err)
	}
	defer func() { _ = cli.Close() }()
	exists, err := cli.CollectionExists(context.Background(), name)
	if err != nil {
		t.Fatalf("CollectionExists：%v", err)
	}
	return exists
}
