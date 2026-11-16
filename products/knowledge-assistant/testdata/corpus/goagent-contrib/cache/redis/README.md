# cache/redis

基于 Redis 的缓存实现，实现 `cache.Bytes` 接口，可用于多实例共享缓存。

## 键前缀

所有键带统一前缀，便于按前缀整体清理，不要在业务代码里直接删除 Redis 键。
