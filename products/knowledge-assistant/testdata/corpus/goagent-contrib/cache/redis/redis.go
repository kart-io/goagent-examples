// Package redis 是基于 Redis 的模型调用缓存实现。
package redis

// Config 是缓存配置。
type Config struct {
	// Prefix 是全部键的前缀，默认 "goagent:cache:"。
	Prefix string
	// internalHelper 不应进入索引。
	secret string
}
