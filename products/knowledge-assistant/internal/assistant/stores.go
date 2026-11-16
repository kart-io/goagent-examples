// Package assistant 装配存储、管理面 Factory 与按用户隔离的知识助手 Agent。
package assistant

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/kart-io/goagent-contrib/cache/redis"
	"github.com/kart-io/goagent-contrib/durable/pgstore"
	mgpg "github.com/kart-io/goagent-contrib/management/pgstore"
	"github.com/kart-io/goagent-contrib/memory/pgvector"
	"github.com/kart-io/goagent-contrib/retrieval"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/memory"

	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/config"
	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/embed"
)

// migrateLock 是本产品专用的锁号，不与 contrib 示例（80261109）和 mgpg（80261108）的迁移锁共用，免得互相串行。
const migrateLock = 80261116

// cacheTTL 须不超过 memory 默认的 EraseWithin（5 分钟），否则 EraseRequired 构建失败。
const cacheTTL = 4 * time.Minute

// Migrate 建齐产品所需的表，可重复执行；多实例并发启动时由 advisory 锁串行。
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLock); err != nil {
			return err
		}
		schemas := []struct{ table, ddl string }{
			{"goagent_durable_schema", pgstore.Schema + pgstore.SessionSchema},
			{"goagent_memory_vectors_schema", pgvector.Schema},
		}
		for _, s := range schemas {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, s.table).Scan(&exists); err != nil {
				return err
			}
			if exists {
				continue
			}
			if _, err := tx.Exec(ctx, s.ddl); err != nil {
				return err
			}
		}
		// 只追加：测试与验收据此断言审批工具恰好执行一次。
		_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS ka_tool_runs(
			execution_key text NOT NULL,
			tool text NOT NULL,
			tenant text NOT NULL,
			at timestamptz NOT NULL DEFAULT now())`)
		return err
	})
	if err != nil {
		return err
	}
	return mgpg.Migrate(ctx, pool)
}

// Stores 是进程共享的全部存储句柄。
type Stores struct {
	Pool     *pgxpool.Pool
	Journal  *pgstore.Store
	Sessions *pgstore.SessionStore
	Manage   *mgpg.Store
	Memory   *memory.Memory
	Cache    builder.ModelCache
	Redis    goredis.UniversalClient
}

// Open 连接已迁移的数据库与 Redis 并装配长期记忆；model 仅在 c.Model=="live" 时用于记忆抽取。
func Open(ctx context.Context, c config.Config, model llm.Client, e retrieval.Embedder) (_ *Stores, err error) {
	s := &Stores{}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.Close(context.WithoutCancel(ctx)))
		}
	}()
	if s.Pool, err = pgxpool.New(ctx, c.PGURL); err != nil {
		return nil, err
	}
	if s.Journal, err = pgstore.New(ctx, pgstore.Config{Pool: s.Pool}); err != nil {
		return nil, err
	}
	if s.Sessions, err = pgstore.NewSessionStore(ctx, s.Journal); err != nil {
		return nil, err
	}
	if s.Manage, err = mgpg.New(ctx, s.Pool); err != nil {
		return nil, err
	}
	s.Redis = goredis.NewClient(&goredis.Options{Addr: c.RedisAddr})
	rc, err := redis.New(ctx, redis.Config{Client: s.Redis, Prefix: c.CachePrefix})
	if err != nil {
		return nil, err
	}
	s.Cache = builder.ModelCache{Cache: rc, TTL: cacheTTL, Erase: builder.EraseRequired, TagKey: c.CacheTagKey}
	idx, err := pgvector.New(ctx, pgvector.Config{Pool: s.Pool})
	if err != nil {
		return nil, err
	}
	mc := memory.Config{
		Durable:    s.Journal,
		Index:      idx,
		Embedder:   e,
		EmbedModel: embed.ModelID(e),
		OnErase:    s.Cache.Eraser(),
	}
	// script 模式没有真实模型，不做抽取。
	if c.Model == "live" {
		mc.Model, mc.Sessions = model, s.Sessions
	}
	if s.Memory, err = memory.New(mc); err != nil {
		return nil, err
	}
	return s, nil
}

// Close 释放连接；可对部分装配的 Stores 调用。
func (s *Stores) Close(ctx context.Context) error {
	var errs []error
	if s.Journal != nil {
		errs = append(errs, s.Journal.Close(ctx))
	}
	if s.Redis != nil {
		errs = append(errs, s.Redis.Close())
	}
	if s.Pool != nil {
		s.Pool.Close()
	}
	return errors.Join(errs...)
}
