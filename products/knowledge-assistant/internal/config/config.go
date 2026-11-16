// Package config 从环境变量加载并校验服务配置。
package config

import (
	"encoding/hex"
	"fmt"
	"os"
)

// minTagKeyBytes 是缓存标签 HMAC 密钥的最小长度，短于此值的密钥抗暴力枚举能力不足。
const minTagKeyBytes = 32

type Config struct {
	PGURL       string
	RedisAddr   string
	QdrantAddr  string // gRPC host:port
	DocsAlias   string
	Listen      string
	Worker      bool
	CacheTagKey []byte
	CachePrefix string
	Model       string // live | script
	Embedder    string // gemini | hash
	CorpusDir   string // 其下每个子目录名即仓库名
}

// FromEnv 从进程环境变量加载配置。
func FromEnv() (Config, error) {
	return FromLookup(os.LookupEnv)
}

// FromLookup 通过注入的查找函数加载配置，便于测试。
func FromLookup(get func(string) (string, bool)) (Config, error) {
	val := func(name, def string) string {
		if v, ok := get(name); ok && v != "" {
			return v
		}
		return def
	}

	var c Config
	required := []struct {
		name string
		dst  *string
	}{
		{"KA_PG_URL", &c.PGURL},
		{"KA_REDIS_ADDR", &c.RedisAddr},
		{"KA_QDRANT_ADDR", &c.QdrantAddr},
	}
	for _, r := range required {
		if *r.dst = val(r.name, ""); *r.dst == "" {
			return Config{}, fmt.Errorf("缺少环境变量 %s", r.name)
		}
	}

	c.DocsAlias = val("KA_DOCS_ALIAS", "ka_docs")
	c.Listen = val("KA_LISTEN", "127.0.0.1:8081")
	c.Worker = val("WORKER", "") == "1"
	c.CachePrefix = val("KA_CACHE_PREFIX", "ka:cache:")
	c.CorpusDir = val("KA_CORPUS_DIR", "/corpus")

	enums := []struct {
		name    string
		def     string
		allowed []string
		dst     *string
	}{
		{"KA_MODEL", "live", []string{"live", "script"}, &c.Model},
		{"KA_EMBEDDER", "gemini", []string{"gemini", "hash"}, &c.Embedder},
	}
	for _, e := range enums {
		v := val(e.name, e.def)
		if !contains(e.allowed, v) {
			return Config{}, fmt.Errorf("环境变量 %s 取值 %q 非法，可选 %v", e.name, v, e.allowed)
		}
		*e.dst = v
	}

	raw := val("KA_CACHE_TAG_KEY", "")
	if raw == "" {
		return Config{}, fmt.Errorf("缺少环境变量 KA_CACHE_TAG_KEY")
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return Config{}, fmt.Errorf("环境变量 KA_CACHE_TAG_KEY 不是合法 hex: %w", err)
	}
	if len(key) < minTagKeyBytes {
		return Config{}, fmt.Errorf("环境变量 KA_CACHE_TAG_KEY 解码后需至少 %d 字节，实际 %d", minTagKeyBytes, len(key))
	}
	c.CacheTagKey = key
	return c, nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
