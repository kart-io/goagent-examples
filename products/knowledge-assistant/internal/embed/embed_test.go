package embed

import (
	"context"
	"errors"
	"math"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	b, err := h.Embed(context.Background(), "pgstore 怎么建表")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 3072 || !reflect.DeepEqual(a, b) {
		t.Fatalf("向量不确定或维度错误: len=%d", len(a))
	}
}

func TestHashSimilarity(t *testing.T) {
	h := NewHash(3072)
	ctx := context.Background()
	embed := func(s string) []float32 {
		v, err := h.Embed(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	q := embed("pgstore 怎么建表")
	near := embed("durable/pgstore 建表：执行 Schema")
	far := embed("Redis 缓存前缀")
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
		one, err := h.Embed(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
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
	model := os.Getenv("GEMINI_EMBEDDING_MODEL")
	if model == "" {
		model = "gemini-embedding-001"
	}
	if id := ModelID(g); !strings.HasPrefix(id, model+"@") {
		t.Fatalf("ModelID=%q", id)
	}
}

func TestEmbedAll(t *testing.T) {
	boom := errors.New("boom")
	texts := []string{"a", "bb", "ccc", "dddd", "eeeee", "ffffff", "ggggggg", "hhhhhhhh", "iiiiiiiii"}
	tests := []struct {
		name    string
		failOn  string
		wantErr error
	}{
		{"按输入顺序返回", "", nil},
		{"任一失败即返回错误", "eeeee", boom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const limit = 3
			var running, peak atomic.Int32
			embed := func(ctx context.Context, s string) ([]float32, error) {
				n := running.Add(1)
				defer running.Add(-1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				time.Sleep(5 * time.Millisecond)
				if s == tt.failOn {
					return nil, boom
				}
				return []float32{float32(len(s))}, ctx.Err()
			}
			got, err := embedAll(context.Background(), texts, limit, embed)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tt.wantErr)
			}
			if p := peak.Load(); p > limit || p < 2 {
				t.Errorf("并发峰值 %d，期望在 2..%d", p, limit)
			}
			if tt.wantErr != nil {
				return
			}
			for i, s := range texts {
				if len(got[i]) != 1 || got[i][0] != float32(len(s)) {
					t.Fatalf("第 %d 条结果 %v 与输入 %q 不对应", i, got[i], s)
				}
			}
		})
	}
}
