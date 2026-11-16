package embed

import (
	"context"
	"fmt"

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

// EmbedBatch 逐条调用 Embed：provider 没有批量接口（FINDINGS F6）。
func (g *Gemini) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := g.p.Embed(ctx, t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}
