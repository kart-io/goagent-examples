package embed

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/kart-io/goagent-contrib/retrieval"
	"github.com/kart-io/goagent/llm/providers"
)

var _ retrieval.Embedder = (*Gemini)(nil)

// Gemini 把 GeminiProvider 适配为 retrieval.Embedder。
type Gemini struct {
	p     *providers.GeminiProvider
	model string
	dims  int
}

// NewGemini 读取 GEMINI_API_KEY、GEMINI_EMBEDDING_MODEL（默认 gemini-embedding-001），
// 并嵌入一次探测文本以确定维度。
func NewGemini(ctx context.Context) (*Gemini, error) {
	p, err := providers.NewGeminiWithOptions()
	if err != nil {
		return nil, fmt.Errorf("创建 Gemini 提供方失败: %w", err)
	}
	g := &Gemini{p: p, model: p.Config.EmbeddingModel}
	probe, err := p.Embed(ctx, "维度探测")
	if err != nil {
		return nil, fmt.Errorf("探测嵌入维度失败: %w", err)
	}
	g.dims = len(probe)
	return g, nil
}

func (g *Gemini) Dimensions() int { return g.dims }

func (g *Gemini) Embed(ctx context.Context, text string) ([]float32, error) {
	return g.p.Embed(ctx, text)
}

// EmbedBatch 并发逐条调用 Embed：provider 没有批量接口（FINDINGS F6），完整语料约 5500 块，
// 串行每条约 0.5–0.9 秒，一次 ingest 要一小时以上。
func (g *Gemini) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	return embedAll(ctx, texts, geminiParallel, g.p.Embed)
}

// geminiParallel 是单批并发上限；实测 10 并发约 10 条/秒，未触发限流。
const geminiParallel = 8

// embedAll 以至多 limit 个并发调用 embed，结果按输入顺序返回，任一失败即取消其余调用。
func embedAll(ctx context.Context, texts []string, limit int, embed func(context.Context, string) ([]float32, error)) ([][]float32, error) {
	out := make([][]float32, len(texts))
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(limit)
	for i, t := range texts {
		g.Go(func() error {
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
