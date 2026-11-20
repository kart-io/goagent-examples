package embed

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kart-io/goagent-contrib/retrieval"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/llm/providers"
)

var _ retrieval.Embedder = (*Gemini)(nil)

// Gemini 把 GeminiProvider 适配为 retrieval.Embedder。
type Gemini struct {
	p        *providers.GeminiProvider
	model    string
	dims     int
	parallel int
}

// NewGemini 读取 GEMINI_API_KEY、GEMINI_EMBEDDING_MODEL（默认 gemini-embedding-001），
// 并嵌入一次探测文本以确定维度。parallel 是 EmbedBatch 的并发上限。
func NewGemini(ctx context.Context, parallel int) (*Gemini, error) {
	p, err := providers.NewGeminiWithOptions(llm.WithTimeout(60 * time.Second))
	if err != nil {
		return nil, fmt.Errorf("创建 Gemini 提供方失败: %w", err)
	}
	g := &Gemini{p: p, model: p.Config.EmbeddingModel, parallel: parallel}
	probe, err := withRetry(p.Embed, defaultRetry)(ctx, "维度探测")
	if err != nil {
		return nil, fmt.Errorf("探测嵌入维度失败: %w", err)
	}
	g.dims = len(probe)
	return g, nil
}

func (g *Gemini) Dimensions() int { return g.dims }

func (g *Gemini) Embed(ctx context.Context, text string) ([]float32, error) {
	return withRetry(g.p.Embed, defaultRetry)(ctx, text)
}

// EmbedBatch 并发逐条调用 Embed：provider 没有批量接口（FINDINGS F6），完整语料约 4200 块，
// 串行每条约 0.5–0.9 秒，一次 ingest 要一小时左右。
func (g *Gemini) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	return embedAll(ctx, texts, g.parallel, withRetry(g.p.Embed, defaultRetry))
}

// retryPolicy 是单条嵌入的重试参数。provider 固定只发一次请求（FINDINGS F25），
// 而 embedAll 遇错即取消整批，不重试的话一次 429 就会让整个 ingest 失败。
type retryPolicy struct {
	attempts      int
	base, max     time.Duration
	maxRetryAfter time.Duration // 服务端要求等待更久时放弃，交给下次 ingest
}

var defaultRetry = retryPolicy{attempts: 5, base: time.Second, max: 30 * time.Second, maxRetryAfter: time.Minute}

// withRetry 对暂时性失败做有上限的指数退避重试（全抖动），服务端给出 Retry-After 时按它等待。
func withRetry(embed func(context.Context, string) ([]float32, error), p retryPolicy) func(context.Context, string) ([]float32, error) {
	return func(ctx context.Context, text string) ([]float32, error) {
		for attempt := 1; ; attempt++ {
			v, err := embed(ctx, text)
			if err == nil || attempt >= p.attempts || !retryable(ctx, err) {
				return v, err
			}
			delay := rand.N(min(p.base<<(attempt-1), p.max) + 1)
			if pe := (*llm.ProviderError)(nil); errors.As(err, &pe) && pe.RetryAfter > 0 {
				if pe.RetryAfter > p.maxRetryAfter {
					return nil, err
				}
				delay = pe.RetryAfter
			}
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, errors.Join(err, ctx.Err())
			case <-t.C:
			}
		}
	}
}

// retryable 沿用 ProviderError.Retryable（429、5xx、暂时性连接错误）。provider 给每次请求套了自己的超时，
// 这种超时在 Retryable 里归为不可重试，但调用方 ctx 仍有效时它只是单次请求慢，值得再试。
func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var pe *llm.ProviderError
	if !errors.As(err, &pe) {
		return false
	}
	return pe.Retryable() || errors.Is(pe.Err, context.DeadlineExceeded)
}

// embedAll 以至多 limit 个并发调用 embed，结果按输入顺序返回，任一失败即取消其余调用。
func embedAll(ctx context.Context, texts []string, limit int, embed func(context.Context, string) ([]float32, error)) ([][]float32, error) {
	out := make([][]float32, len(texts))
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(limit)
	for i, t := range texts {
		g.Go(func() error {
			// errgroup 取消后仍会运行排队中的函数，这里不再发起新请求。
			if err := ctx.Err(); err != nil {
				return err
			}
			v, err := embed(ctx, t)
			out[i] = v
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}
