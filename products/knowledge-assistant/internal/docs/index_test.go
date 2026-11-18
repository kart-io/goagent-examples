package docs

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/qdrant/go-client/qdrant"

	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/interfaces"

	"github.com/kart-io/goagent-contrib/retrieval"

	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/embed"
)

type env struct {
	addr  string
	alias string
	cli   *qdrant.Client
	pool  *pgxpool.Pool
	idx   *Index
}

func newEnv(t *testing.T) *env {
	t.Helper()
	addr, pgURL := os.Getenv("KA_TEST_QDRANT_ADDR"), os.Getenv("KA_TEST_PG_URL")
	if addr == "" || pgURL == "" {
		t.Skip("需经 scripts/test.sh 运行")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		t.Fatalf("连接 PG: %v", err)
	}
	t.Cleanup(pool.Close)
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("解析 KA_TEST_QDRANT_ADDR %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("解析 KA_TEST_QDRANT_ADDR 端口 %q: %v", portStr, err)
	}
	cli, err := qdrant.NewClient(&qdrant.Config{Host: host, Port: port})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	alias := fmt.Sprintf("t_%08x", rand.Uint32())
	idx, err := NewIndex(ctx, addr, alias, embed.NewHash(3072), pool)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	e := &env{addr: addr, alias: alias, cli: cli, pool: pool, idx: idx}
	t.Cleanup(func() {
		if err := idx.Close(); err != nil {
			t.Errorf("关闭 Index: %v", err)
		}
		for _, name := range e.collections(t) {
			if err := cli.DeleteCollection(ctx, name); err != nil {
				t.Errorf("清理集合 %s: %v", name, err)
			}
		}
		if err := cli.Close(); err != nil {
			t.Errorf("关闭客户端: %v", err)
		}
	})
	return e
}

// collections 返回以本测试 alias 为前缀的集合（含与 alias 同名的集合）。
func (e *env) collections(t *testing.T) []string {
	t.Helper()
	all, err := e.cli.ListCollections(context.Background())
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	var out []string
	for _, n := range all {
		if n == e.alias || strings.HasPrefix(n, e.alias+"_") {
			out = append(out, n)
		}
	}
	return out
}

func (e *env) count(t *testing.T, collection string) uint64 {
	t.Helper()
	n, err := e.cli.Count(context.Background(), &qdrant.CountPoints{CollectionName: collection})
	if err != nil {
		t.Fatalf("Count %s: %v", collection, err)
	}
	return n
}

func TestRetrieverRequiresAlias(t *testing.T) {
	e := newEnv(t)
	_, err := e.idx.Retriever(context.Background(), 4)
	if err == nil {
		t.Fatal("alias 不存在时应报错")
	}
	if code := agentErrors.GetCode(err); code != agentErrors.CodeNotFound {
		t.Errorf("错误码 = %v，期望 CodeNotFound（%v）", code, err)
	}
	if !strings.Contains(err.Error(), "先运行 ka ingest") {
		t.Errorf("错误信息缺少提示: %v", err)
	}
	if got := e.collections(t); len(got) != 0 {
		t.Errorf("不应隐式创建集合，实际有 %v", got)
	}
}

func TestRebuildSwitchesAlias(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chunks := mustCollect(t)

	first, err := e.idx.Rebuild(ctx, "k1", chunks)
	if err != nil {
		t.Fatalf("Rebuild k1: %v", err)
	}
	if cur, err := e.idx.Current(ctx); err != nil || cur != first {
		t.Fatalf("Current = %q, %v；期望 %q", cur, err, first)
	}

	r, err := e.idx.Retriever(ctx, 4)
	if err != nil {
		t.Fatalf("Retriever: %v", err)
	}
	var _ interfaces.DocumentRetriever = r
	docs, err := r.GetRelevantDocuments(ctx, "pgstore 怎么建表")
	if err != nil {
		t.Fatalf("GetRelevantDocuments: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("没有检索结果")
	}
	src, _ := docs[0].Metadata["source"].(string)
	if !strings.HasPrefix(src, "goagent-contrib:durable/pgstore/") {
		t.Errorf("首条 source = %q，期望前缀 goagent-contrib:durable/pgstore/", src)
	}
	if !strings.HasPrefix(docs[0].PageContent, "来源："+src+"\n") {
		t.Errorf("首条正文缺少来源行: %q", docs[0].PageContent)
	}

	second, err := e.idx.Rebuild(ctx, "k2", chunks)
	if err != nil {
		t.Fatalf("Rebuild k2: %v", err)
	}
	if second == first {
		t.Fatal("不同 key 应得到不同集合名")
	}
	if cur, err := e.idx.Current(ctx); err != nil || cur != second {
		t.Fatalf("Current = %q, %v；期望 %q", cur, err, second)
	}
	if exists, err := e.cli.CollectionExists(ctx, first); err != nil || exists {
		t.Errorf("旧集合 %s 应已删除: exists=%v err=%v", first, exists, err)
	}
}

func TestRebuildIsIdempotent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chunks := mustCollect(t)

	for _, k := range []string{"k1", "k2"} {
		if _, err := e.idx.Rebuild(ctx, k, chunks); err != nil {
			t.Fatalf("Rebuild %s: %v", k, err)
		}
	}
	second, err := e.idx.Current(ctx)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	full := e.count(t, second)
	if full != uint64(len(chunks)) {
		t.Fatalf("点数 = %d，期望 %d", full, len(chunks))
	}

	// 删掉一个点：重放若重写，点数会恢复。
	pts, err := e.cli.Scroll(ctx, &qdrant.ScrollPoints{CollectionName: second, Limit: qdrant.PtrOf(uint32(1))})
	if err != nil || len(pts) != 1 {
		t.Fatalf("Scroll: %v, %d", err, len(pts))
	}
	if _, err := e.cli.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: second,
		Wait:           qdrant.PtrOf(true),
		Points:         qdrant.NewPointsSelector(pts[0].Id),
	}); err != nil {
		t.Fatalf("删除点: %v", err)
	}

	third, err := e.idx.Rebuild(ctx, "k2", chunks)
	if err != nil {
		t.Fatalf("重放 Rebuild k2: %v", err)
	}
	if third != second {
		t.Errorf("重放返回 %q，期望 %q", third, second)
	}
	if got := e.collections(t); len(got) != 1 {
		t.Errorf("alias 前缀集合 = %v，期望只剩 1 个", got)
	}
	if got := e.count(t, second); got != full-1 {
		t.Errorf("重放后点数 = %d，期望保持 %d（不应重写）", got, full-1)
	}
}

func TestRebuildRecoversPartialCollection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chunks := mustCollect(t)

	// 模拟写入后、切 alias 前崩溃：新集合存在但只有部分点，alias 不指向它。
	name := e.alias + "_" + keyHash("k1")
	if err := e.cli.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: name,
		VectorsConfig:  qdrant.NewVectorsConfig(&qdrant.VectorParams{Size: 3072, Distance: qdrant.Distance_Cosine}),
	}); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	got, err := e.idx.Rebuild(ctx, "k1", chunks)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if got != name {
		t.Fatalf("返回 %q，期望 %q", got, name)
	}
	if n := e.count(t, name); n != uint64(len(chunks)) {
		t.Errorf("点数 = %d，期望 %d", n, len(chunks))
	}
	if cur, err := e.idx.Current(ctx); err != nil || cur != name {
		t.Errorf("Current = %q, %v", cur, err)
	}
}

func TestRetrieverReusesStore(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.idx.Rebuild(ctx, "k1", mustCollect(t)); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if _, err := e.idx.Retriever(ctx, 4); err != nil {
		t.Fatalf("Retriever: %v", err)
	}
	first := e.idx.store
	if first == nil {
		t.Fatal("首次调用后应缓存 store")
	}
	for range 3 {
		r, err := e.idx.Retriever(ctx, 4)
		if err != nil {
			t.Fatalf("Retriever: %v", err)
		}
		if r.VectorStore != interfaces.VectorStore(first) {
			t.Fatal("检索器应复用同一个 store")
		}
	}
	if e.idx.store != first {
		t.Error("store 被重建")
	}
	// alias 切换后复用的 store 仍能检索到新集合。
	if _, err := e.idx.Rebuild(ctx, "k2", mustCollect(t)); err != nil {
		t.Fatalf("Rebuild k2: %v", err)
	}
	r, err := e.idx.Retriever(ctx, 4)
	if err != nil {
		t.Fatalf("Retriever: %v", err)
	}
	if docs, err := r.GetRelevantDocuments(ctx, "pgstore 怎么建表"); err != nil || len(docs) == 0 {
		t.Fatalf("alias 切换后检索: %d 条, %v", len(docs), err)
	}
	if err := e.idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if e.idx.store != nil {
		t.Error("Close 后应释放缓存的 store")
	}
}

func TestRebuildRejectsEmptyChunks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	live, err := e.idx.Rebuild(ctx, "k1", mustCollect(t))
	if err != nil {
		t.Fatalf("Rebuild k1: %v", err)
	}
	for _, chunks := range [][]Chunk{nil, {}} {
		_, err := e.idx.Rebuild(ctx, "k2", chunks)
		if !agentErrors.IsCode(err, agentErrors.CodeInvalidInput) {
			t.Fatalf("错误 = %v，期望 CodeInvalidInput", err)
		}
	}
	if cur, err := e.idx.Current(ctx); err != nil || cur != live {
		t.Errorf("Current = %q, %v；线上索引应不变，期望 %q", cur, err, live)
	}
	if got := e.collections(t); len(got) != 1 || got[0] != live {
		t.Errorf("集合 = %v，期望只有 %s", got, live)
	}
}

func TestDropStaleOnlyOwnCollections(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// 另一个 alias（以本 alias 加下划线为前缀）的集合不能被误删。
	other := e.alias + "_foo_aaaaaaaaaaaa"
	if err := e.cli.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: other,
		VectorsConfig:  qdrant.NewVectorsConfig(&qdrant.VectorParams{Size: 3072, Distance: qdrant.Distance_Cosine}),
	}); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	for _, k := range []string{"k1", "k2"} {
		if _, err := e.idx.Rebuild(ctx, k, mustCollect(t)); err != nil {
			t.Fatalf("Rebuild %s: %v", k, err)
		}
	}
	if exists, err := e.cli.CollectionExists(ctx, other); err != nil || !exists {
		t.Errorf("他人集合 %s 被误删: exists=%v err=%v", other, exists, err)
	}
}

// 模拟锁被绕过的并发重建：A 切换 alias 之后、清理之前，B 把 alias 切到了自己的集合。
// A 的清理必须放过 alias 当前指向的集合，否则线上索引被删，之后所有任务都会失败。
func TestDropStaleKeepsCurrentAfterConcurrentSwitch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mine, err := e.idx.Rebuild(ctx, "k1", mustCollect(t))
	if err != nil {
		t.Fatalf("Rebuild k1: %v", err)
	}
	theirs := e.alias + "_" + keyHash("k2")
	if err := e.cli.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: theirs,
		VectorsConfig:  qdrant.NewVectorsConfig(&qdrant.VectorParams{Size: 3072, Distance: qdrant.Distance_Cosine}),
	}); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if err := e.cli.UpdateAliases(ctx, []*qdrant.AliasOperations{
		qdrant.NewAliasDelete(e.alias), qdrant.NewAliasCreate(e.alias, theirs),
	}); err != nil {
		t.Fatalf("外部切换 alias: %v", err)
	}

	if err := e.idx.dropStale(ctx, mine); err != nil {
		t.Fatalf("dropStale: %v", err)
	}
	if exists, err := e.cli.CollectionExists(ctx, theirs); err != nil || !exists {
		t.Fatalf("alias 当前指向的 %s 被删除: exists=%v err=%v", theirs, exists, err)
	}
	if cur, err := e.idx.Current(ctx); err != nil || cur != theirs {
		t.Errorf("Current = %q, %v；期望 %q", cur, err, theirs)
	}
}

// 两个进程（各自的 Index）同时以不同的键重建：锁让它们依次执行，都成功，最终 alias 指向完整的集合且只剩它一个。
func TestConcurrentRebuildsSerialized(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chunks := mustCollect(t)
	other, err := NewIndex(ctx, e.addr, e.alias, embed.NewHash(3072), e.pool)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })

	tests := []struct {
		idx *Index
		key string
	}{
		{e.idx, "k1"},
		{other, "k2"},
	}
	names := make([]string, len(tests))
	errs := make([]error, len(tests))
	var wg sync.WaitGroup
	for i, tt := range tests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			names[i], errs[i] = tt.idx.Rebuild(ctx, tt.key, chunks)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Rebuild %s: %v", tests[i].key, err)
		}
	}
	cur, err := e.idx.Current(ctx)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cur != names[0] && cur != names[1] {
		t.Fatalf("Current = %q，期望是 %v 之一", cur, names)
	}
	if n := e.count(t, cur); n != uint64(len(chunks)) {
		t.Errorf("当前集合点数 = %d，期望 %d", n, len(chunks))
	}
	if got := e.collections(t); len(got) != 1 || got[0] != cur {
		t.Errorf("集合 = %v，期望只剩 %s", got, cur)
	}
}

// 锁被另一方持有时 Rebuild 必须等待，释放后才继续：证明串行化来自锁，而不是两次调用碰巧错开。
// 等待期间不得在数据库里排队占连接（排队的重建再多也不占满连接池）。
func TestRebuildWaitsForLock(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	old := lockPoll
	lockPoll = 50 * time.Millisecond
	t.Cleanup(func() { lockPoll = old })
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, rebuildLock); err != nil {
		t.Fatalf("持锁: %v", err)
	}

	chunks := mustCollect(t)
	done := make(chan error, 1)
	go func() {
		_, err := e.idx.Rebuild(ctx, "k1", chunks)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("锁被占用时 Rebuild 不应完成: %v", err)
	case <-time.After(10 * lockPoll):
	}
	var waiting int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND classid = 0 AND objid = $1 AND objsubid = 1 AND NOT granted`, rebuildLock).Scan(&waiting); err != nil {
		t.Fatalf("查询 pg_locks: %v", err)
	}
	if waiting != 0 {
		t.Fatalf("等锁时不应在数据库里阻塞占连接，实际有 %d 个等待者", waiting)
	}
	if got := e.collections(t); len(got) != 0 {
		t.Fatalf("等锁期间不应写集合，实际有 %v", got)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("释放锁: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("释放锁后 Rebuild 60 秒内未完成")
	}
	if _, err := e.idx.Current(ctx); err != nil {
		t.Errorf("Current: %v", err)
	}
}

// gateEmbedder 在首次嵌入时停住，让测试在重建持锁写入期间动手。
type gateEmbedder struct {
	retrieval.Embedder
	once             sync.Once
	entered, release chan struct{}
}

func (g *gateEmbedder) wait() {
	g.once.Do(func() { close(g.entered) })
	<-g.release
}

func (g *gateEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	g.wait()
	return g.Embedder.Embed(ctx, text)
}

func (g *gateEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	g.wait()
	return g.Embedder.EmbedBatch(ctx, texts)
}

// 持锁连接在写入途中断开（锁随之释放）时，重建必须放弃切换 alias，否则会与拿到锁的另一个重建同时切换。
func TestRebuildAbortsWhenLockLost(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	gate := &gateEmbedder{Embedder: embed.NewHash(3072), entered: make(chan struct{}), release: make(chan struct{})}
	idx, err := NewIndex(ctx, e.addr, e.alias, gate, e.pool)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	done := make(chan error, 1)
	go func() {
		_, err := idx.Rebuild(ctx, "k1", mustCollect(t))
		done <- err
	}()
	select {
	case <-gate.entered:
	case err := <-done:
		t.Fatalf("Rebuild 未进入写入就返回: %v", err)
	}
	var killed int
	if err := e.pool.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_locks
		WHERE locktype = 'advisory' AND classid = 0 AND objid = $1 AND objsubid = 1 AND granted`, rebuildLock).Scan(&killed); err != nil {
		t.Fatalf("断开持锁连接: %v", err)
	}
	if killed != 1 {
		t.Fatalf("断开了 %d 个持锁连接，期望 1", killed)
	}
	close(gate.release)
	if err := <-done; err == nil {
		t.Fatal("失锁后 Rebuild 应返回错误")
	}
	if cur, err := e.idx.Current(ctx); !agentErrors.IsCode(err, agentErrors.CodeNotFound) {
		t.Fatalf("失锁后不应切换 alias，Current = %q, %v", cur, err)
	}
}

func TestContentKey(t *testing.T) {
	base := []Chunk{{ID: "a", Source: "repo:x.md", Text: "正文"}}
	key := ContentKey("hash-v1@3072", base)
	tests := []struct {
		name   string
		model  string
		chunks []Chunk
		same   bool
	}{
		{"相同输入", "hash-v1@3072", []Chunk{{ID: "a", Source: "repo:x.md", Text: "正文"}}, true},
		{"换嵌入模型", "gemini@3072", base, false},
		{"正文变化", "hash-v1@3072", []Chunk{{ID: "a", Source: "repo:x.md", Text: "正文2"}}, false},
		{"来源变化", "hash-v1@3072", []Chunk{{ID: "a", Source: "repo:y.md", Text: "正文"}}, false},
		{"字段边界移动", "hash-v1@3072", []Chunk{{ID: "a", Source: "repo:x.md正", Text: "文"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContentKey(tt.model, tt.chunks) == key; got != tt.same {
				t.Errorf("与基准键相同 = %v，期望 %v", got, tt.same)
			}
		})
	}
}
