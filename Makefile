.PHONY: work test test-remote vet

# 建立本地工作区，与同级目录的 goagent 联动编译
# CONTRIB 非空时把 contrib 工作树的 durable/pgstore 与 memory/qdrant 一并加入工作区
CONTRIB ?=
work:
	test -f go.work || go work init . ../goagent
ifeq ($(CONTRIB),)
	go work edit -go=1.26.0
else
	go work use $(CONTRIB)/durable/pgstore $(CONTRIB)/memory/qdrant
	go work edit -go=1.27.0
endif

# 使用 go.work（存在时）运行全部离线测试
test:
	go test -race -count=1 ./...

# 只依赖 go.mod 中固定的 goagent 版本
test-remote:
	GOWORK=off go test -race -count=1 ./...

vet:
	go vet ./...
