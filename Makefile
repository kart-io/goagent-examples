.PHONY: work test test-remote vet

# 建立本地工作区，与同级目录的 goagent 联动编译
work:
	test -f go.work || (go work init . ../goagent && go work edit -go=1.25.0)

# 使用 go.work（存在时）运行全部离线测试
test:
	go test -race -count=1 ./...

# 只依赖 go.mod 中固定的 goagent 版本
test-remote:
	GOWORK=off go test -race -count=1 ./...

vet:
	go vet ./...
