package embed

import (
	"context"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
)

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func TestHashDeterministic(t *testing.T) {
	h := NewHash(3072)
	a, err := h.Embed(context.Background(), "pgstore 怎么建表")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := h.Embed(context.Background(), "pgstore 怎么建表")
	if len(a) != 3072 || !reflect.DeepEqual(a, b) {
		t.Fatalf("向量不确定或维度错误: len=%d", len(a))
	}
}

func TestHashSimilarity(t *testing.T) {
	h := NewHash(3072)
	ctx := context.Background()
	q, _ := h.Embed(ctx, "pgstore 怎么建表")
	near, _ := h.Embed(ctx, "durable/pgstore 建表：执行 Schema")
	far, _ := h.Embed(ctx, "Redis 缓存前缀")
	if cosine(q, near) <= cosine(q, far) {
		t.Fatalf("相近文本相似度 %.3f 应大于无关文本 %.3f", cosine(q, near), cosine(q, far))
	}
}

func TestHashEmptyNotZero(t *testing.T) {
	v, err := NewHash(3072).Embed(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range v {
		if x != 0 {
			return
		}
	}
	t.Fatal("空文本向量不应全零")
}

func TestHashBatch(t *testing.T) {
	h := NewHash(3072)
	ctx := context.Background()
	texts := []string{"pgstore 建表", "", "Redis 缓存前缀"}
	batch, err := h.EmbedBatch(ctx, texts)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range texts {
		one, _ := h.Embed(ctx, s)
		if !reflect.DeepEqual(batch[i], one) {
			t.Fatalf("第 %d 条批量与单条结果不一致", i)
		}
	}
}

func TestModelIDHash(t *testing.T) {
	if got := ModelID(NewHash(3072)); got != "hash-v1@3072" {
		t.Fatalf("got %q", got)
	}
}

func TestGeminiLive(t *testing.T) {
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("未设置 GEMINI_API_KEY")
	}
	g, err := NewGemini(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if g.Dimensions() <= 0 {
		t.Fatalf("维度应大于 0: %d", g.Dimensions())
	}
	if id := ModelID(g); !strings.HasPrefix(id, "gemini-embedding-001@") {
		t.Fatalf("ModelID=%q", id)
	}
}
