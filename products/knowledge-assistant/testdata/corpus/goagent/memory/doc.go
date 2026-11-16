// Package memory 提供智能体的长期记忆能力。记忆按命名空间隔离，支持写入、检索与删除。
package memory

// Put 向指定命名空间写入一条记忆，相同键重复写入会覆盖旧值。
func Put(namespace []string, key, value string) error { return nil }

// Search 在指定命名空间内按语义检索记忆，最多返回 topK 条。
func Search(namespace []string, query string, topK int) ([]string, error) { return nil, nil }

func internalHelper() {}
