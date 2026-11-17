package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/qdrant/go-client/qdrant"

	cq "github.com/kart-io/goagent-contrib/retrieval/qdrant"

	"github.com/kart-io/goagent-contrib/retrieval"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/interfaces"
)

var _ interfaces.DocumentRetriever = (*retrieval.VectorStoreRetriever)(nil)

const writeBatch = 100

// rebuildLock 串行化同一数据库上的全部重建：Worker 每个任务一个 goroutine，两个审批同时通过、
// 或与 compose 每次 up 都会跑的 ingest 重叠时，重建会并发执行，后者的集合可能被前者的清理删掉。
// 不与迁移锁（assistant.migrateLock = 80261116）共用，免得耗时数分钟的重建挡住实例启动时的迁移。
const rebuildLock = 80261117

// lockPoll 是等锁时的重试间隔。等待方不持连接，排队的重建再多也不会占满连接池。
var lockPoll = 2 * time.Second

var errLockBusy = agentErrors.New(agentErrors.CodeConflict, "重建索引锁被占用")

// ContentKey 是重建的幂等键：由嵌入模型与全部文本块决定。ingest 与审批触发的重建共用它，
// 语料未变时谁先重建都得到同一集合，后来者不再重新嵌入；换嵌入模型则必定重建。
func ContentKey(modelID string, chunks []Chunk) string {
	h := sha256.New()
	fmt.Fprintf(h, "%q\x00", modelID)
	for _, c := range chunks {
		fmt.Fprintf(h, "%q\x00%q\x00%q\x00", c.ID, c.Source, c.Text)
	}
	return "content-" + hex.EncodeToString(h.Sum(nil))
}

// Index 是以 Qdrant alias 对外的文档索引：每次重建写入新集合，再原子切换 alias。
type Index struct {
	addr, alias string
	embedder    retrieval.Embedder
	client      *qdrant.Client
	pool        *pgxpool.Pool // 只用于 rebuildLock；放进构造参数，让每条重建路径都绕不开锁

	mu    sync.Mutex
	store *cq.QdrantVectorStore // 绑定 alias 名的检索 store，懒创建后复用；alias 切换后仍有效
}

// NewIndex 连接 Qdrant（addr 形如 host:6334，gRPC）；pool 指向各实例共用的 Postgres，Rebuild 在其上取 advisory 锁。
func NewIndex(ctx context.Context, addr, alias string, e retrieval.Embedder, pool *pgxpool.Pool) (*Index, error) {
	if pool == nil {
		return nil, agentErrors.New(agentErrors.CodeInvalidConfig, "文档索引需要 Postgres 连接池以串行化重建")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, agentErrors.Wrap(err, agentErrors.CodeInvalidConfig, "Qdrant 地址格式应为 host:port")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, agentErrors.Wrap(err, agentErrors.CodeInvalidConfig, "Qdrant 端口无效")
	}
	cli, err := qdrant.NewClient(&qdrant.Config{Host: host, Port: port})
	if err != nil {
		return nil, agentErrors.Wrap(err, agentErrors.CodeInternal, "创建 Qdrant 客户端失败")
	}
	if _, err := cli.HealthCheck(ctx); err != nil {
		_ = cli.Close()
		return nil, agentErrors.Wrap(err, agentErrors.CodeInternal, "连接 Qdrant 失败")
	}
	return &Index{addr: addr, alias: alias, embedder: e, client: cli, pool: pool}, nil
}

// Close 关闭内部客户端。
func (x *Index) Close() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	var storeErr error
	if x.store != nil {
		storeErr = x.store.Close()
		x.store = nil
	}
	if err := x.client.Close(); err != nil {
		return err
	}
	return storeErr
}

func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// Current 返回 alias 当前指向的集合；alias 不存在时返回 CodeNotFound。
func (x *Index) Current(ctx context.Context) (string, error) {
	aliases, err := x.client.ListAliases(ctx)
	if err != nil {
		return "", agentErrors.Wrap(err, agentErrors.CodeInternal, "列出 Qdrant alias 失败")
	}
	for _, a := range aliases {
		if a.AliasName == x.alias {
			return a.CollectionName, nil
		}
	}
	return "", agentErrors.New(agentErrors.CodeNotFound, fmt.Sprintf("索引 %s 不存在，请先运行 ka ingest", x.alias))
}

// Rebuild 把 chunks 写入由 key 决定的新集合并切换 alias，返回新集合名。
// 同一 key 重放是幂等的：已是当前集合时不重写，只清理残留旧集合。
// 并发调用（含其它进程）经 rebuildLock 依次执行；事务级锁在提交、回滚或连接断开时都会释放，进程崩溃也不会留下死锁。
func (x *Index) Rebuild(ctx context.Context, key string, chunks []Chunk) (string, error) {
	if len(chunks) == 0 {
		return "", agentErrors.New(agentErrors.CodeInvalidInput, "语料为空，拒绝重建索引（避免用空集合替换线上索引）")
	}
	for {
		var name string
		err := pgx.BeginFunc(ctx, x.pool, func(tx pgx.Tx) error {
			var got bool
			if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, rebuildLock).Scan(&got); err != nil {
				return agentErrors.Wrap(err, agentErrors.CodeInternal, "获取重建索引锁失败")
			}
			if !got {
				return errLockBusy
			}
			var err error
			name, err = x.rebuild(ctx, tx, key, chunks)
			return err
		})
		if !errors.Is(err, errLockBusy) {
			return name, err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// rebuild 在持锁事务 tx 内执行。锁随连接断开而释放，所以切换 alias 前先确认连接还在，
// 免得失锁后与另一个重建同时切换；断开发生在确认之后的窗口仍靠 dropStale 不删线上集合兜底。
func (x *Index) rebuild(ctx context.Context, tx pgx.Tx, key string, chunks []Chunk) (string, error) {
	next := x.alias + "_" + keyHash(key)
	cur := ""
	switch c, err := x.Current(ctx); {
	case err == nil:
		cur = c
	case !agentErrors.IsCode(err, agentErrors.CodeNotFound):
		return "", err
	}

	if cur != next {
		if err := x.write(ctx, next, chunks); err != nil {
			return "", err
		}
		ops := []*qdrant.AliasOperations{qdrant.NewAliasCreate(x.alias, next)}
		if cur != "" {
			ops = append([]*qdrant.AliasOperations{qdrant.NewAliasDelete(x.alias)}, ops...)
		}
		if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
			return "", agentErrors.Wrap(err, agentErrors.CodeInternal, "重建索引锁已丢失，放弃切换 alias")
		}
		if err := x.client.UpdateAliases(ctx, ops); err != nil {
			return "", agentErrors.Wrap(err, agentErrors.CodeInternal, "切换 Qdrant alias 失败")
		}
	}
	if err := x.dropStale(ctx, next); err != nil {
		return "", err
	}
	return next, nil
}

// write 重新创建集合再写入；崩溃遗留的半成品集合直接丢弃，保证集合内容完整。
func (x *Index) write(ctx context.Context, name string, chunks []Chunk) error {
	exists, err := x.client.CollectionExists(ctx, name)
	if err != nil {
		return agentErrors.Wrap(err, agentErrors.CodeInternal, "检查集合是否存在失败")
	}
	if exists {
		if err := x.client.DeleteCollection(ctx, name); err != nil {
			return agentErrors.Wrap(err, agentErrors.CodeInternal, "删除残留集合失败")
		}
	}
	store, err := cq.NewQdrantVectorStore(ctx, cq.QdrantConfig{
		URL: x.addr, CollectionName: name, VectorSize: x.embedder.Dimensions(), Embedder: x.embedder,
	})
	if err != nil {
		return err
	}
	defer store.Close()

	for i := 0; i < len(chunks); i += writeBatch {
		end := min(i+writeBatch, len(chunks))
		batch := make([]*interfaces.Document, 0, end-i)
		for _, c := range chunks[i:end] {
			batch = append(batch, &interfaces.Document{
				ID:          c.ID,
				PageContent: c.Text,
				Metadata:    map[string]interface{}{"source": c.Source},
			})
		}
		if err := store.AddDocuments(ctx, batch); err != nil {
			return err
		}
	}
	return nil
}

func (x *Index) dropStale(ctx context.Context, keep string) error {
	names, err := x.client.ListCollections(ctx)
	if err != nil {
		return agentErrors.Wrap(err, agentErrors.CodeInternal, "列出 Qdrant 集合失败")
	}
	mine := regexp.MustCompile("^" + regexp.QuoteMeta(x.alias) + "_[0-9a-f]{12}$")
	for _, n := range names {
		if n == keep || !mine.MatchString(n) {
			continue
		}
		// 每次删除前重读 alias：即使锁被绕过、alias 已被别的重建切走，也不删线上集合。
		cur, err := x.Current(ctx)
		if err != nil && !agentErrors.IsCode(err, agentErrors.CodeNotFound) {
			return err
		}
		if n == cur {
			continue
		}
		if err := x.client.DeleteCollection(ctx, n); err != nil {
			return agentErrors.Wrap(err, agentErrors.CodeInternal, "删除旧集合 "+n+" 失败")
		}
	}
	return nil
}

// Retriever 返回基于 alias 的检索器。先校验 alias，因为 NewQdrantVectorStore 会对不存在的名字隐式建集合。
func (x *Index) Retriever(ctx context.Context, topK int) (*retrieval.VectorStoreRetriever, error) {
	if _, err := x.Current(ctx); err != nil {
		return nil, err
	}
	store, err := x.aliasStore(ctx)
	if err != nil {
		return nil, err
	}
	return retrieval.NewVectorStoreRetriever(store, retrieval.RetrieverConfig{TopK: topK}), nil
}

func (x *Index) aliasStore(ctx context.Context) (*cq.QdrantVectorStore, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.store != nil {
		return x.store, nil
	}
	store, err := cq.NewQdrantVectorStore(ctx, cq.QdrantConfig{
		URL: x.addr, CollectionName: x.alias, VectorSize: x.embedder.Dimensions(), Embedder: x.embedder,
	})
	if err != nil {
		return nil, err
	}
	x.store = store
	return store, nil
}
