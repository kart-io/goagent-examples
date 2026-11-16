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

	"github.com/kart-io/goagent/llm"
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
	g, err := NewGemini(context.Background(), 2)
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
		name      string
		limit     int
		failOn    string
		wantErr   error
		wantCalls int32 // 0 表示不检查
		wantPeak  bool
	}{
		{name: "按输入顺序返回且并发受限", limit: 3, wantPeak: true},
		// 并发为 1 时失败后的调度确定：失败那条之后不应再有调用。
		{name: "失败后不再发起调用", limit: 1, failOn: "ccc", wantErr: boom, wantCalls: 3},
		{name: "并发下失败仍返回错误", limit: 3, failOn: "eeeee", wantErr: boom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var running, peak, calls atomic.Int32
			embed := func(ctx context.Context, s string) ([]float32, error) {
				calls.Add(1)
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
			got, err := embedAll(context.Background(), texts, tt.limit, embed)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tt.wantErr)
			}
			if tt.wantCalls != 0 && calls.Load() != tt.wantCalls {
				t.Errorf("调用 %d 次，期望 %d 次", calls.Load(), tt.wantCalls)
			}
			if tt.wantErr != nil {
				if got != nil {
					t.Fatalf("失败时应返回 nil，实际 %v", got)
				}
				return
			}
			if p := peak.Load(); tt.wantPeak && (p > int32(tt.limit) || p < 2) {
				t.Errorf("并发峰值 %d，期望在 2..%d", p, tt.limit)
			}
			for i, s := range texts {
				if len(got[i]) != 1 || got[i][0] != float32(len(s)) {
					t.Fatalf("第 %d 条结果 %v 与输入 %q 不对应", i, got[i], s)
				}
			}
		})
	}
}

func TestWithRetry(t *testing.T) {
	pe := func(code int, err error) error { return &llm.ProviderError{StatusCode: code, Err: err} }
	upstream := errors.New("upstream")
	tests := []struct {
		name      string
		errs      []error // 第 i 次调用返回 errs[i]，用完后返回成功
		wantErr   bool
		wantCalls int
	}{
		{"429 两次后成功", []error{pe(429, upstream), pe(429, upstream)}, false, 3},
		{"503 后成功", []error{pe(503, upstream)}, false, 2},
		{"单次请求超时后成功", []error{pe(0, context.DeadlineExceeded)}, false, 2},
		{"400 不重试", []error{pe(400, upstream)}, true, 1},
		{"额度耗尽不重试", []error{&llm.ProviderError{StatusCode: 429, QuotaExhausted: true, Err: upstream}}, true, 1},
		{"非提供方错误不重试", []error{upstream}, true, 1},
		{"重试次数用尽", []error{pe(500, upstream), pe(500, upstream), pe(500, upstream), pe(500, upstream)}, true, 3},
		{"Retry-After 过长放弃", []error{&llm.ProviderError{StatusCode: 429, RetryAfter: time.Hour, Err: upstream}}, true, 1},
	}
	policy := retryPolicy{attempts: 3, base: time.Millisecond, max: 4 * time.Millisecond, maxRetryAfter: time.Second}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			embed := func(context.Context, string) ([]float32, error) {
				calls++
				if calls <= len(tt.errs) {
					return nil, tt.errs[calls-1]
				}
				return []float32{1}, nil
			}
			v, err := withRetry(embed, policy)(context.Background(), "x")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v，期望出错 %v", err, tt.wantErr)
			}
			if !tt.wantErr && len(v) != 1 {
				t.Fatalf("成功时应返回向量，实际 %v", v)
			}
			if calls != tt.wantCalls {
				t.Fatalf("调用 %d 次，期望 %d 次", calls, tt.wantCalls)
			}
		})
	}
}

func TestWithRetryStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	embed := func(context.Context, string) ([]float32, error) {
		calls++
		cancel()
		return nil, &llm.ProviderError{StatusCode: 503, Err: errors.New("upstream")}
	}
	policy := retryPolicy{attempts: 5, base: time.Hour, max: time.Hour, maxRetryAfter: time.Hour}
	if _, err := withRetry(embed, policy)(ctx, "x"); err == nil || calls != 1 {
		t.Fatalf("取消后应立即返回错误且不再调用：err=%v calls=%d", err, calls)
	}
}
