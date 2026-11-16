package docs

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/interfaces"

	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/embed"
)

type env struct {
	addr  string
	alias string
	cli   *qdrant.Client
	idx   *Index
}

func newEnv(t *testing.T) *env {
	t.Helper()
	addr := os.Getenv("KA_TEST_QDRANT_ADDR")
	if addr == "" {
		t.Skip("需经 scripts/test.sh 运行")
	}
	ctx := context.Background()
	host, port := "127.0.0.1", 0
	if _, err := fmt.Sscanf(addr[strings.LastIndex(addr, ":")+1:], "%d", &port); err != nil {
		t.Fatalf("解析 KA_TEST_QDRANT_ADDR %q: %v", addr, err)
	}
	cli, err := qdrant.NewClient(&qdrant.Config{Host: host, Port: port})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	alias := fmt.Sprintf("t_%08x", rand.Uint32())
	idx, err := NewIndex(ctx, addr, alias, embed.NewHash(3072))
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	e := &env{addr: addr, alias: alias, cli: cli, idx: idx}
	t.Cleanup(func() {
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
