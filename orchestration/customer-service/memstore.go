package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"net"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	qc "github.com/qdrant/go-client/qdrant"

	"github.com/kart-io/goagent-contrib/durable/pgstore"
	"github.com/kart-io/goagent-contrib/memory/qdrant"
	"github.com/kart-io/goagent/core/session"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/llm/providers"
	"github.com/kart-io/goagent/memory"
)

const (
	memoryApp = "customer-service"
	// 与 contrib 示例、ka 产品的迁移锁号错开，互不串行。
	memoryMigrateLock = 80261117
	hashDims          = 256
)

// MemoryConfig 是长期记忆的存储与嵌入配置；Live 为真时用 Gemini 嵌入，否则用离线哈希嵌入。
type MemoryConfig struct {
	PGURL, QdrantAddr string
	Live              bool
	Model             llm.Client // 抽取与整理用的模型，nil 时用宿主的模型
}

// memoryStores 持有长期记忆及其底层连接。
type memoryStores struct {
	Memory   *memory.Memory
	Sessions session.Store
	close    func()
}

// Close 释放连接，可重复调用。
func (s *memoryStores) Close() { s.close() }

// memoryNS 是客户的记忆命名空间，只由宿主依据工单行的 CustomerID 绑定。
func memoryNS(customerID string) []string { return []string{memoryApp, customerID} }

// migrateMemory 建齐 pgstore 的表，可重复执行；多实例并发启动时由 advisory 锁串行。
func migrateMemory(ctx context.Context, pool *pgxpool.Pool) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, memoryMigrateLock); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass('goagent_durable_schema') IS NOT NULL`).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		_, err := tx.Exec(ctx, pgstore.Schema+pgstore.SessionSchema)
		return err
	})
}

// openMemory 连接 Postgres 与 Qdrant 并装配长期记忆；model 用于记忆抽取，可为 nil（不抽取）。
func openMemory(ctx context.Context, c MemoryConfig, model llm.Client) (_ *memoryStores, err error) {
	var cleanups []func()
	closeAll := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	defer func() {
		if err != nil {
			closeAll()
		}
	}()

	pool, err := pgxpool.New(ctx, c.PGURL)
	if err != nil {
		return nil, fmt.Errorf("连接 Postgres 失败: %w", err)
	}
	cleanups = append(cleanups, pool.Close)
	if err = migrateMemory(ctx, pool); err != nil {
		return nil, fmt.Errorf("迁移记忆表失败: %w", err)
	}
	journal, err := pgstore.New(ctx, pgstore.Config{Pool: pool})
	if err != nil {
		return nil, err
	}
	cleanups = append(cleanups, func() { _ = journal.Close(context.WithoutCancel(ctx)) })
	sessions, err := pgstore.NewSessionStore(ctx, journal)
	if err != nil {
		return nil, err
	}

	host, portStr, err := net.SplitHostPort(c.QdrantAddr)
	if err != nil {
		return nil, fmt.Errorf("解析 Qdrant 地址（须为 host:port）: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("解析 Qdrant 端口: %w", err)
	}
	client, err := qc.NewClient(&qc.Config{Host: host, Port: port, SkipCompatibilityCheck: true})
	if err != nil {
		return nil, fmt.Errorf("连接 Qdrant 失败: %w", err)
	}
	cleanups = append(cleanups, func() { _ = client.Close() })
	index, err := qdrant.New(ctx, qdrant.Config{Client: client, Prefix: "cs_memory"})
	if err != nil {
		return nil, err
	}

	embedder, embedModel, err := newEmbedder(ctx, c.Live)
	if err != nil {
		return nil, err
	}
	mem, err := memory.New(memory.Config{
		Durable: journal, Index: index, Embedder: embedder, EmbedModel: embedModel,
		Model: model, Sessions: sessions,
	})
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return &memoryStores{Memory: mem, Sessions: sessions, close: func() { once.Do(closeAll) }}, nil
}

// newEmbedder 返回嵌入客户端及其模型标识，标识是索引键的一部分。
func newEmbedder(ctx context.Context, live bool) (llm.EmbeddingClient, string, error) {
	if !live {
		return hashEmbedder{dims: hashDims}, fmt.Sprintf("hash/%d", hashDims), nil
	}
	p, err := providers.NewGeminiWithOptions()
	if err != nil {
		return nil, "", fmt.Errorf("创建 Gemini 提供方失败: %w", err)
	}
	probe, err := p.Embed(ctx, "维度探测")
	if err != nil {
		return nil, "", fmt.Errorf("探测嵌入维度失败: %w", err)
	}
	return p, fmt.Sprintf("gemini/%s/%d", p.Config.EmbeddingModel, len(probe)), nil
}

// hashEmbedder 把字符二元组哈希累加到固定维度，确定且非零，只供离线演示与测试，不具语义。
type hashEmbedder struct{ dims int }

func (h hashEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if h.dims <= 0 {
		return nil, errors.New("嵌入维度须为正")
	}
	runes := []rune(text)
	v := make([]float32, h.dims)
	add := func(gram string) {
		f := fnv.New32a()
		_, _ = f.Write([]byte(gram))
		v[f.Sum32()%uint32(h.dims)]++
	}
	for i := 0; i+1 < len(runes); i++ {
		add(string(runes[i : i+2]))
	}
	if len(runes) == 1 {
		add(string(runes))
	}
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return nil, errors.New("文本为空，无法嵌入")
	}
	n := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= n
	}
	return v, nil
}
