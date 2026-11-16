// Package embed 提供产品使用的嵌入器：线上用 Gemini，离线测试用确定性哈希。
package embed

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"unicode"

	"github.com/kart-io/goagent-contrib/retrieval"
)

var _ retrieval.Embedder = (*Hash)(nil)

// Hash 是无需网络的确定性嵌入器，只用于离线测试，不代表真实语义。
type Hash struct {
	dims int
}

func NewHash(dims int) *Hash {
	if dims <= 0 {
		panic("embed: 哈希嵌入器维度必须大于 0")
	}
	return &Hash{dims: dims}
}

func (h *Hash) Dimensions() int { return h.dims }

func (h *Hash) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v := make([]float32, h.dims)
	tokens := tokenize(text)
	for _, tok := range tokens {
		sum := fnv.New32a()
		_, _ = sum.Write([]byte(tok))
		s := sum.Sum32()
		// 桶号取自对维度取模，符号取自最高位。
		sign := float32(1)
		if s>>31 == 1 {
			sign = -1
		}
		v[int(s%uint32(h.dims))] += sign
	}
	// memory.NormalizeVector 拒绝零向量。
	if len(tokens) == 0 {
		v[0] = 1
	}
	return v, nil
}

func (h *Hash) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := h.Embed(ctx, t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// tokenize 产出小写 ASCII 词与相邻汉字二元组；仅汉字组二元组，假名、谚文等不处理。
func tokenize(text string) []string {
	var tokens []string
	var word []rune
	var prevCJK rune
	flush := func() {
		if len(word) > 0 {
			tokens = append(tokens, string(word))
			word = word[:0]
		}
	}
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_':
			word = append(word, r)
			prevCJK = 0
		case unicode.Is(unicode.Han, r):
			flush()
			if prevCJK != 0 {
				tokens = append(tokens, string([]rune{prevCJK, r}))
			}
			prevCJK = r
		default:
			flush()
			prevCJK = 0
		}
	}
	flush()
	return tokens
}

// ModelID 生成写入 memory.Config.EmbedModel 的标识，维度变化时旧向量不会被混用。
func ModelID(e retrieval.Embedder) string {
	switch v := e.(type) {
	case *Hash:
		return fmt.Sprintf("hash-v1@%d", v.dims)
	case *Gemini:
		return fmt.Sprintf("%s@%d", v.model, v.dims)
	default:
		return fmt.Sprintf("unknown@%d", e.Dimensions())
	}
}
