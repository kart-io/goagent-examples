package config

import (
	"strings"
	"testing"
)

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func minimalEnv(over map[string]string) map[string]string {
	m := map[string]string{
		"KA_PG_URL":        "postgres://x",
		"KA_REDIS_ADDR":    "127.0.0.1:6379",
		"KA_QDRANT_ADDR":   "127.0.0.1:6334",
		"KA_CACHE_TAG_KEY": strings.Repeat("ab", 32),
	}
	for k, v := range over {
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return m
}

func TestFromLookup(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(t *testing.T, c Config)
	}{
		{name: "缺少 PG 地址", env: minimalEnv(map[string]string{"KA_PG_URL": ""}), wantErr: "KA_PG_URL"},
		{name: "缺少 Redis 地址", env: minimalEnv(map[string]string{"KA_REDIS_ADDR": ""}), wantErr: "KA_REDIS_ADDR"},
		{name: "缺少 Qdrant 地址", env: minimalEnv(map[string]string{"KA_QDRANT_ADDR": ""}), wantErr: "KA_QDRANT_ADDR"},
		{name: "缺少标签密钥", env: minimalEnv(map[string]string{"KA_CACHE_TAG_KEY": ""}), wantErr: "KA_CACHE_TAG_KEY"},
		{name: "标签密钥非法 hex", env: minimalEnv(map[string]string{"KA_CACHE_TAG_KEY": "zz"}), wantErr: "KA_CACHE_TAG_KEY"},
		{name: "标签密钥仅 31 字节", env: minimalEnv(map[string]string{"KA_CACHE_TAG_KEY": strings.Repeat("ab", 31)}), wantErr: "KA_CACHE_TAG_KEY"},
		{name: "非法模型", env: minimalEnv(map[string]string{"KA_MODEL": "foo"}), wantErr: "KA_MODEL"},
		{name: "非法嵌入器", env: minimalEnv(map[string]string{"KA_EMBEDDER": "foo"}), wantErr: "KA_EMBEDDER"},
		{name: "嵌入并发非数字", env: minimalEnv(map[string]string{"KA_EMBED_PARALLEL": "x"}), wantErr: "KA_EMBED_PARALLEL"},
		{name: "嵌入并发为 0", env: minimalEnv(map[string]string{"KA_EMBED_PARALLEL": "0"}), wantErr: "KA_EMBED_PARALLEL"},
		{name: "嵌入并发超上限", env: minimalEnv(map[string]string{"KA_EMBED_PARALLEL": "65"}), wantErr: "KA_EMBED_PARALLEL"},
		{
			name: "最小合法输入取默认值",
			env:  minimalEnv(nil),
			check: func(t *testing.T, c Config) {
				if c.DocsAlias != "ka_docs" || c.Listen != "127.0.0.1:8081" || c.Worker ||
					c.Model != "live" || c.Embedder != "gemini" ||
					c.CachePrefix != "ka:cache:" || c.CorpusDir != "/corpus" || len(c.CacheTagKey) != 32 || c.EmbedParallel != 8 {
					t.Fatalf("默认值不符: %+v", c)
				}
			},
		},
		{
			name: "WORKER=1 为真",
			env:  minimalEnv(map[string]string{"WORKER": "1"}),
			check: func(t *testing.T, c Config) {
				if !c.Worker {
					t.Fatal("Worker 应为真")
				}
			},
		},
		{
			name: "覆盖默认值",
			env:  minimalEnv(map[string]string{"KA_MODEL": "script", "KA_EMBEDDER": "hash", "KA_LISTEN": ":9", "KA_EMBED_PARALLEL": "3"}),
			check: func(t *testing.T, c Config) {
				if c.Model != "script" || c.Embedder != "hash" || c.Listen != ":9" || c.EmbedParallel != 3 {
					t.Fatalf("覆盖未生效: %+v", c)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := FromLookup(lookupFrom(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("期望错误含 %q，实际 %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			tt.check(t, c)
		})
	}
}
