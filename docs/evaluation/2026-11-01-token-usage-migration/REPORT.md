# Token 统计案例迁移

日期：2026-11-01。案例代码已从 GoAgent 的 examples/agent-usage 整组迁入本仓库 [agent-usage](../../../agent-usage/README.md)，包括原离线示例、完整任务、单 Agent／子 Agent、真实 Provider 验证、多平台与错误／取消／回放，以及共享 helper 和测试。

## 依赖与运行

本分支依赖同级 GoAgent 当前开发代码。go.mod 仍固定 v0.28.0，新增 Token 明细及推理接口尚未发布，没有虚构新版本号，也没有增加旧接口分支或复制核心实现。

```sh
make work
go test -race ./agent-usage/...
go run ./agent-usage/task
go run ./agent-usage/agent -mode child
go run ./agent-usage
```

生成的 go.work / go.work.sum 被 git 忽略；框架发布后应统一升级 go.mod 再执行 test-remote。当前不能宣称只克隆本仓库、仅依赖固定旧版本即可运行所有案例。

所有 helper 导入已改为本模块路径。GoAgent 原目录仅保留 README 迁移入口；核心实现、API 使用文档、历史实际调用日志留在 GoAgent。迁入 README 中跨仓库设计／历史证据链接指向 GoAgent，当前核心使用文档链接指向本仓库 main/agent-usage。历史执行日志和命令未改写；当前命令以迁入后的 README 为准。

迁移前最新证据摘要覆盖的 7 个 Go 文件，反向归一导入路径后均与原摘要一致；原有跟踪的 main.go 按字节保持不变。其它迁入文件通过案例测试和编译验证。

## 既有案例接入修正

全仓库验证先暴露了与当前内核的既有不一致，未用跳过测试处理：

- 移除 internal/livemodel 和 workflow/business-cases 中已不存在的 WithRetryCount(0)；当前 Provider 不隐式重试，由 Agent 控制恢复。
- localize 的预算 RootKey 与图 RunID 统一为 notice-1，保持恢复和术语策略变更时的意图校验。
- SQL-review 多张工单保留独立图／持久键，调用 Agent 时显式使用宿主批次预算的观测根身份；没有关闭身份校验，也没有更换或重跑图的持久操作键，批次额度上限保持不变。

两处业务案例原有失败、恢复、编辑后重审等回归均通过。目标仓库已有的需求／调研文档与 README 段落保留。

## 验证

全部退出码为 0：

| 检查 | 结果 |
| --- | --- |
| 迁入后 go test -race ./agent-usage/... | 通过 |
| go build ./agent-usage/... | 通过 |
| go test -race ./...（本仓库，关闭真实模型 opt-in） | 通过，见 [examples-race.log](examples-race.log) |
| 两个修正案例、共享客户端、业务案例的 race 回归 | 通过，见 [integration-race.log](integration-race.log) |
| GoAgent go test ./... | 通过，见 [core-suite.log](core-suite.log) |
| 新目录运行 task / agent -mode child / 原离线示例 | 分别验证 18、17 Token 及回放仍为18、live=5/replay=2 |

本次迁移没有新增付费模型调用；上一轮真实模型证据保留在 GoAgent，不能把本次离线运行称为新的真实服务评测。

当前为两个仓库的本地修改，尚未提交或推送。文件摘要见 [checksums.json](checksums.json)。
