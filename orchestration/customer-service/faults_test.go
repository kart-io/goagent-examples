package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	qc "github.com/qdrant/go-client/qdrant"
	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent-examples/orchestration/internal/demomodel"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/memory"
)

// notePattern 取“记下：”之后到行尾或引号为止的文本：对话里的工单视图是 JSON，输入以引号结束。
var notePattern = regexp.MustCompile(`记下：([^"\n]+)`)

// faultModel 包装离线脚本：抽取请求的对话含“记下：”时把其后的文本作为一条事实；整理请求先回调 onReconcile（可为 nil）。
// 故障测试的事实放在测试里，不改 model.go 的离线话术表。
func faultModel(onReconcile func()) *livemodel.Client { return faultModelFor("", onReconcile) }

// faultModelFor 与 faultModel 相同，但只在整理请求含 token 时回调 onReconcile：共用存储中其他命名空间的整理照常应答。
func faultModelFor(token string, onReconcile func()) *livemodel.Client {
	var calls atomic.Int64
	return livemodel.Wrap(&demomodel.Model{Calls: &calls, Respond: func(req *llm.CompletionRequest) llm.StreamChunk {
		system := req.Messages[0].Content
		switch {
		case strings.HasPrefix(system, "你从一轮对话中抽取"):
			if m := notePattern.FindStringSubmatch(firstUser(req.Messages)); m != nil {
				return jsonText(map[string]any{"memories": []map[string]any{
					{"kind": "fact", "content": m[1], "keywords": []string{}, "entities": []string{}, "importance": 0.8},
				}})
			}
		case strings.HasPrefix(system, "你维护长期记忆") && onReconcile != nil && strings.Contains(firstUser(req.Messages), token):
			onReconcile()
		}
		return respond(req)
	}}, livemodel.Config{Provider: "scripted", Model: "customer-service-faults"})
}

// faultStores 返回存储地址；缺少时跳过。
func faultStores(t *testing.T) (pg, qd string) {
	t.Helper()
	pg, qd = os.Getenv("CS_MEMORY_PG_URL"), os.Getenv("CS_MEMORY_QDRANT_ADDR")
	if pg == "" || qd == "" {
		t.Skipf("未设置 CS_MEMORY_PG_URL 与 CS_MEMORY_QDRANT_ADDR，请经 scripts/with-stores.sh 运行")
	}
	return pg, qd
}

// newFaultEnvAt 在 dir 打开接入长期记忆的宿主；client 为 nil 时用 faultModel(nil)。
func newFaultEnvAt(t *testing.T, dir string, client *livemodel.Client) *hostEnv {
	t.Helper()
	pg, qd := faultStores(t)
	if client == nil {
		client = faultModel(nil)
	}
	return newHostEnvAt(t, dir, func(c *HostConfig) {
		c.Client, c.Memory = client, &MemoryConfig{PGURL: pg, QdrantAddr: qd}
	})
}

func countContent(recs []memory.Record, content string) int {
	n := 0
	for _, r := range recs {
		if r.Content == content {
			n++
		}
	}
	return n
}

func recordContents(recs []memory.Record) []string {
	return contents(recs, func(r memory.Record) string { return r.Content })
}

func uniqueTickets(rs ...MessageResult) []string {
	var out []string
	for _, r := range rs {
		if r.TicketID != "" && !slices.Contains(out, r.TicketID) {
			out = append(out, r.TicketID)
		}
	}
	return out
}

// TestMemoryFaultCrashChild 只作为 crash 子测试的子进程运行：含 CS_FAULT_CHILD_TOKEN 的整理请求到达时创建标记文件并永久阻塞，等待父进程 SIGKILL。
func TestMemoryFaultCrashChild(t *testing.T) {
	dir := os.Getenv("CS_FAULT_CHILD_DIR")
	if dir == "" {
		t.Skip("仅由 TestMemoryFaults/crash 以子进程方式运行")
	}
	mark, token := os.Getenv("CS_FAULT_CHILD_MARK"), os.Getenv("CS_FAULT_CHILD_TOKEN")
	if token == "" {
		t.Fatal("缺少 CS_FAULT_CHILD_TOKEN")
	}
	e := newFaultEnvAt(t, dir, faultModelFor(token, func() {
		_ = os.WriteFile(mark, nil, 0o600)
		select {}
	}))
	_, err := e.host.Sweep(context.Background())
	t.Fatalf("整理应阻塞直到被终止，Sweep 却返回：%v", err)
}

// TestMemoryFaults 在真实 Postgres 与 Qdrant 上跑确定性的故障与并发场景；子测试串行：
// pg-outage 暂停共用容器，index-loss 删除共用集合，所以 index-loss 放在最后。
func TestMemoryFaults(t *testing.T) {
	faultStores(t)
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"concurrent", faultConcurrent},
		{"replay", faultReplay},
		{"crash", faultCrash},
		{"pg-outage", faultPGOutage},
		{"ns-concurrent", faultNSConcurrent},
		{"index-loss", faultIndexLoss},
	} {
		t.Run(tc.name, tc.run)
	}
}

// 同一客户同时提交两条消息、同时扫描：两条事实各恰好一条。
func faultConcurrent(t *testing.T) {
	e := newFaultEnvAt(t, t.TempDir(), nil)
	c, cctx := newCustomer()
	texts := []string{"记下：会员等级是金卡", "记下：常用快递是顺丰"}

	results, errs := make([]MessageResult, len(texts)), make([]error, len(texts))
	var wg sync.WaitGroup
	for i, text := range texts {
		wg.Go(func() { results[i], errs[i] = e.host.Message(context.Background(), c, "agent-1", msgID(), text) })
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "消息 %q", texts[i])
		t.Logf("消息 %q：%+v", texts[i], results[i])
	}

	sweepErrs := make([]error, 2)
	for i := range sweepErrs {
		wg.Go(func() { _, sweepErrs[i] = e.host.Sweep(context.Background()) })
	}
	wg.Wait()
	for i, err := range sweepErrs {
		require.NoError(t, err, "第 %d 个并发 Sweep", i+1)
	}

	_, err := settle(context.Background(), e, cctx, uniqueTickets(results...))
	require.NoError(t, err)
	recs, err := validRecords(cctx, e.host.memory.Memory)
	require.NoError(t, err)
	for _, fact := range []string{"会员等级是金卡", "常用快递是顺丰"} {
		require.Equal(t, 1, countContent(recs, fact), "事实 %q 应恰好 1 条，有效记录：%q", fact, recordContents(recs))
	}
}

// 处理完成后再扫描三次、同一消息重复投递：记录数不变，重复投递不开新工单。
func faultReplay(t *testing.T) {
	e := newFaultEnvAt(t, t.TempDir(), nil)
	c, cctx := newCustomer()
	id, text := msgID(), "记下：生日是三月"

	r := e.message(id, c, text)
	_, err := settle(context.Background(), e, cctx, []string{r.TicketID})
	require.NoError(t, err)
	recs, err := validRecords(cctx, e.host.memory.Memory)
	require.NoError(t, err)
	n := len(recs)
	require.Equal(t, 1, countContent(recs, "生日是三月"), "有效记录：%q", recordContents(recs))

	for i := range 3 {
		_, err := e.host.Sweep(context.Background())
		require.NoError(t, err, "第 %d 次重复扫描", i+1)
	}
	dup, err := e.host.Message(context.Background(), c, "agent-1", id, text)
	require.NoError(t, err)
	require.NotEqual(t, "started", dup.Action, "重复投递不开新工单")
	_, err = settle(context.Background(), e, cctx, uniqueTickets(r, dup))
	require.NoError(t, err)

	recs, err = validRecords(cctx, e.host.memory.Memory)
	require.NoError(t, err)
	require.Len(t, recs, n, "重放后记录数不变，有效记录：%q", recordContents(recs))
}

const (
	crashMarkWait  = 60 * time.Second
	crashSettleMax = 3 * time.Minute
	crashSweepGap  = 2 * time.Second
	// crashSweepTimeout 限定单次 Sweep：死去子进程持有的 worker 租约（pgstore 默认 30 秒）过期前 Lock 会一直等待。
	crashSweepTimeout = 45 * time.Second
)

// 整理进行中 SIGKILL 宿主进程，重启后再扫描：任务完成，钉住转给取代者，没有悬空的取代关系。
func faultCrash(t *testing.T) {
	e := newFaultEnvAt(t, t.TempDir(), nil)
	c, cctx := newCustomer()
	// token 写进钉住记录，本任务的整理请求在“已有记忆”中带上它，子进程只在这次整理中阻塞。
	token := "T" + uuid.NewString()[:8]
	before := rememberPinned(t, e, cctx, c, "请记住：只用邮件联系我（"+token+"）")
	r := e.message(msgID(), c, "邮件或短信都可以")
	keys, err := sessionJobKeys(context.Background(), e, memorySession(r.TicketID, generalName))
	require.NoError(t, err)
	require.Len(t, keys, 1)
	job := func() memory.Job {
		t.Helper()
		j, ok, err := e.host.memory.Memory.Job(cctx, keys[0])
		require.NoError(t, err)
		require.True(t, ok, "该轮登记了抽取任务")
		return j
	}
	// 子进程打开同一目录的 SQLite 账本，父进程须先关闭宿主。
	require.NoError(t, e.host.Close())

	mark := filepath.Join(t.TempDir(), "reconciling")
	var out bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run", "^TestMemoryFaultCrashChild$", "-test.count", "1", "-test.v", "-test.timeout", "5m")
	cmd.Env = append(os.Environ(), "CS_FAULT_CHILD_DIR="+e.dir, "CS_FAULT_CHILD_MARK="+mark, "CS_FAULT_CHILD_TOKEN="+token)
	cmd.Stdout, cmd.Stderr = &out, &out
	require.NoError(t, cmd.Start())
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = cmd.Process.Kill()
			<-exited
		}
	})

	deadline := time.Now().Add(crashMarkWait)
	for {
		if _, err := os.Stat(mark); err == nil {
			break
		}
		select {
		case err := <-exited:
			killed = true
			t.Fatalf("子进程在整理前退出：%v\n%s", err, out.String())
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s 内子进程未进入整理\n%s", crashMarkWait, out.String())
		}
	}
	require.NoError(t, cmd.Process.Kill())
	<-exited
	killed = true
	t.Logf("子进程已在整理中被 SIGKILL")

	e.open()
	j := job()
	require.NotEqual(t, memory.JobDone, j.Status, "子进程应在本任务的整理中被终止，任务不应已完成")
	t.Logf("重启后任务状态：%s，尝试 %d 次", j.Status, j.Attempts)

	start := time.Now()
	for sweeps := 1; ; sweeps++ {
		ctx, cancel := context.WithTimeout(context.Background(), crashSweepTimeout)
		_, err := e.host.Sweep(ctx)
		cancel()
		if err != nil {
			t.Logf("第 %d 次 Sweep（%s）返回错误：%v", sweeps, time.Since(start).Round(time.Second), err)
		}
		if j = job(); j.Status != memory.JobPending {
			t.Logf("第 %d 次 Sweep（%s）后任务状态：%s，尝试 %d 次", sweeps, time.Since(start).Round(time.Second), j.Status, j.Attempts)
			break
		}
		if time.Since(start) > crashSettleMax {
			break
		}
		time.Sleep(crashSweepGap)
	}
	require.Equal(t, memory.JobDone, j.Status, "崩溃后 %s 内任务未完成：状态 %s，尝试 %d 次，错误 %q", crashSettleMax, j.Status, j.Attempts, j.Error)

	after := pinned(t, e, cctx)
	require.Len(t, after, 1, "钉住记录恰好 1 条：%q", recordContents(after))
	require.Equal(t, "客户接受邮件或短信联系", after[0].Content)
	old, err := e.host.memory.Memory.Get(cctx, before.ID)
	require.NoError(t, err)
	require.False(t, old.Valid(), "原钉住记录已失效")
	require.Equal(t, after[0].ID, old.SupersededBy, "原钉住记录指向取代者")

	touched, err := validRecords(cctx, e.host.memory.Memory)
	require.NoError(t, err)
	for _, rec := range append(touched, old, after[0]) {
		if rec.InvalidReason != memory.ReasonSuperseded {
			continue
		}
		_, err := e.host.memory.Memory.Get(cctx, rec.SupersededBy)
		require.NoError(t, err, "记录 %s（%q）的取代者 %s 应存在", rec.ID, rec.Content, rec.SupersededBy)
	}
}

func docker(args ...string) error {
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker %s：%w：%s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// 扫描期间暂停 Postgres：扫描报错而非静默成功；恢复后重试完成，事实恰好一条。
func faultPGOutage(t *testing.T) {
	name := os.Getenv("CS_MEMORY_PG_CONTAINER")
	if name == "" {
		t.Skip("未设置 CS_MEMORY_PG_CONTAINER，请经 scripts/with-stores.sh 运行")
	}
	e := newFaultEnvAt(t, t.TempDir(), nil)
	c, cctx := newCustomer()
	r := e.message(msgID(), c, "记下：发票抬头是个人")

	require.NoError(t, docker("pause", name))
	t.Cleanup(func() { _ = docker("unpause", name) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan error, 1)
	go func() {
		_, err := e.host.Sweep(ctx)
		done <- err
	}()
	var sweepErr error
	select {
	case sweepErr = <-done:
	case <-time.After(30 * time.Second):
		t.Errorf("Postgres 暂停时 Sweep 超过上下文期限 10 秒后 20 秒仍未返回")
		require.NoError(t, docker("unpause", name))
		sweepErr = <-done
	}
	cancel()
	require.NoError(t, docker("unpause", name))
	t.Logf("暂停期间 Sweep：%v", sweepErr)
	require.Error(t, sweepErr, "Postgres 暂停时 Sweep 应返回错误")

	_, err := settle(context.Background(), e, cctx, []string{r.TicketID})
	require.NoError(t, err, "恢复后 settle 应成功")
	recs, err := validRecords(cctx, e.host.memory.Memory)
	require.NoError(t, err)
	require.Equal(t, 1, countContent(recs, "发票抬头是个人"), "有效记录：%q", recordContents(recs))
}

const nsCustomers = 5

// 多个客户并发提交与扫描：每个命名空间只含自己的事实。
func faultNSConcurrent(t *testing.T) {
	e := newFaultEnvAt(t, t.TempDir(), nil)
	type customer struct {
		id     string
		ctx    context.Context
		result MessageResult
		err    error
	}
	cs := make([]customer, nsCustomers)
	for i := range cs {
		cs[i].id, cs[i].ctx = newCustomer()
	}
	var wg sync.WaitGroup
	for i := range cs {
		wg.Go(func() {
			cs[i].result, cs[i].err = e.host.Message(context.Background(), cs[i].id, "agent-1", msgID(), fmt.Sprintf("记下：暗号是 K%d", i))
			if cs[i].err != nil {
				return
			}
			if _, err := e.host.Sweep(context.Background()); err != nil {
				t.Logf("客户 %d 的 Sweep 返回错误：%v", i, err)
			}
		})
	}
	wg.Wait()
	for i, cu := range cs {
		require.NoError(t, cu.err, "客户 %d 的消息", i)
	}
	for i, cu := range cs {
		_, err := settle(context.Background(), e, cu.ctx, []string{cu.result.TicketID})
		require.NoError(t, err, "客户 %d", i)
	}
	for i, cu := range cs {
		recs, err := validRecords(cu.ctx, e.host.memory.Memory)
		require.NoError(t, err)
		require.Equal(t, []string{fmt.Sprintf("暗号是 K%d", i)}, recordContents(recs), "客户 %d 的命名空间只含自己的暗号", i)
	}
}

// noPanic 运行 fn：panic 记为失败，返回错误只写入日志。
func noPanic(t *testing.T, what string, fn func() error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("%s panic：%v", what, p)
		}
	}()
	if err := fn(); err != nil {
		t.Logf("%s 返回错误（可接受）：%v", what, err)
		return
	}
	t.Logf("%s 成功", what)
}

func recallIDs(ctx context.Context, m *memory.Memory, text string) ([]string, error) {
	got, err := m.Recall(ctx, memory.Query{Text: text, K: 10})
	return contents(got, func(s memory.Scored) string { return s.ID }), err
}

func qdrantClient(addr string) (*qc.Client, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	return qc.NewClient(&qc.Config{Host: host, Port: port, SkipCompatibilityCheck: true})
}

// deleteMemoryCollections 删除本示例前缀（cs_memory）下的全部 Qdrant 集合，返回删除的名称。
func deleteMemoryCollections(ctx context.Context, client *qc.Client) ([]string, error) {
	names, err := client.ListCollections(ctx)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, name := range names {
		if !strings.HasPrefix(name, "cs_memory_") {
			continue
		}
		if err := client.DeleteCollection(ctx, name); err != nil {
			return deleted, err
		}
		deleted = append(deleted, name)
	}
	return deleted, nil
}

// 删除 Qdrant 集合后召回与收消息不崩溃；Reindex 后集合重建且点数不少于有效记录数，召回的记录 ID 集合与故障前相同。
// 召回还按字符二元组打分，没有集合时也能命中，所以须直接检查集合与点数。
func faultIndexLoss(t *testing.T) {
	_, qd := faultStores(t)
	if os.Getenv("CS_MEMORY_QDRANT_CONTAINER") == "" {
		t.Skip("未设置 CS_MEMORY_QDRANT_CONTAINER，请经 scripts/with-stores.sh 运行：本场景删除 cs_memory_* 集合，只能用于脚本启动的 Qdrant")
	}
	client, err := qdrantClient(qd)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	e := newFaultEnvAt(t, t.TempDir(), nil)
	c, cctx := newCustomer()
	var rs []MessageResult
	for _, text := range []string{"记下：常住城市是杭州", "记下：公司在杭州滨江", "记下：杭州收货地址在西湖区", "记下：偏好晚上联系"} {
		rs = append(rs, e.message(msgID(), c, text))
	}
	_, err = settle(context.Background(), e, cctx, uniqueTickets(rs...))
	require.NoError(t, err)
	m := e.host.memory.Memory
	a, err := recallIDs(cctx, m, "杭州")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(a), 2, "故障前召回到多条记录：%v", a)

	deleted, err := deleteMemoryCollections(context.Background(), client)
	require.NoError(t, err)
	require.NotEmpty(t, deleted, "删除了本示例的集合")
	t.Logf("已删除集合：%v", deleted)

	noPanic(t, "删除集合后 Recall", func() error {
		got, err := recallIDs(cctx, m, "杭州")
		t.Logf("删除集合后召回 ID：%v", got)
		return err
	})
	noPanic(t, "删除集合后 Message", func() error {
		r, err := e.host.Message(context.Background(), c, "agent-1", msgID(), "我的包裹到哪了")
		t.Logf("删除集合后消息结果：%+v", r)
		return err
	})

	require.NoError(t, m.Reindex(cctx, true))
	collection := fmt.Sprintf("cs_memory_d%d", hashDims)
	exists, err := client.CollectionExists(context.Background(), collection)
	require.NoError(t, err)
	require.True(t, exists, "Reindex 后集合 %s 已重建", collection)
	points, err := client.Count(context.Background(), &qc.CountPoints{CollectionName: collection, Exact: new(true)})
	require.NoError(t, err)
	recs, err := validRecords(cctx, m)
	require.NoError(t, err)
	t.Logf("Reindex 后集合 %s 有 %d 个点，有效记录 %d 条", collection, points, len(recs))
	require.GreaterOrEqual(t, points, uint64(len(recs)), "Reindex 后点数不少于有效记录数")
	b, err := recallIDs(cctx, m, "杭州")
	require.NoError(t, err)
	require.ElementsMatch(t, a, b, "Reindex 后召回的记录 ID 集合与故障前相同")
}
