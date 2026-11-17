package main

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent/memory"
)

func TestHashEmbedder(t *testing.T) {
	e := hashEmbedder{dims: 256}
	ctx := context.Background()
	vec := func(s string) []float32 {
		v, err := e.Embed(ctx, s)
		require.NoError(t, err)
		return v
	}
	a, a2, b := vec("退款到账时间"), vec("退款到账时间"), vec("发票下载")

	tests := []struct {
		name string
		ok   bool
	}{
		{"同文本向量相等", slicesEqual(a, a2)},
		{"不同文本向量不等", !slicesEqual(a, b)},
		{"维度为 256", len(a) == 256 && len(b) == 256},
		{"向量非零", norm(a) > 0 && norm(b) > 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { require.True(t, tt.ok) })
	}
}

func slicesEqual(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func norm(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

func TestOpenMemory(t *testing.T) {
	pg, qd := os.Getenv("CS_MEMORY_PG_URL"), os.Getenv("CS_MEMORY_QDRANT_ADDR")
	if pg == "" || qd == "" {
		t.Skip("未设置 CS_MEMORY_PG_URL 与 CS_MEMORY_QDRANT_ADDR，请经 scripts/with-stores.sh 运行")
	}
	ctx := context.Background()
	cfg := MemoryConfig{PGURL: pg, QdrantAddr: qd}
	customer := "c-" + uuid.NewString()
	nsCtx := memory.WithNamespace(ctx, memoryNS(customer)...)
	const content = "客户偏好用支付宝接收退款"

	s, err := openMemory(ctx, cfg, offlineModel())
	require.NoError(t, err)
	t.Cleanup(s.Close)
	rec, err := s.Memory.Remember(nsCtx, memory.Record{ID: uuid.NewString(), Kind: memory.KindFact, Content: content, Importance: 0.8, Pinned: true})
	require.NoError(t, err)

	pinned := func(m *memory.Memory) []string {
		recs, err := m.Pinned(nsCtx)
		require.NoError(t, err)
		ids := make([]string, 0, len(recs))
		for _, r := range recs {
			ids = append(ids, r.ID)
		}
		return ids
	}
	t.Run("写入后置顶可读", func(t *testing.T) {
		require.Equal(t, []string{rec.ID}, pinned(s.Memory))
	})
	t.Run("同内容召回命中", func(t *testing.T) {
		got, err := s.Memory.Recall(nsCtx, memory.Query{Text: content, K: 5})
		require.NoError(t, err)
		ids := make([]string, 0, len(got))
		for _, g := range got {
			ids = append(ids, g.ID)
		}
		require.Contains(t, ids, rec.ID)
	})
	t.Run("重新打开后置顶仍在", func(t *testing.T) {
		s.Close()
		s2, err := openMemory(ctx, cfg, offlineModel())
		require.NoError(t, err)
		defer s2.Close()
		require.Equal(t, []string{rec.ID}, pinned(s2.Memory))
	})
}
