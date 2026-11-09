# 协议案例验证

本文件保留引入真实模型前的离线验收快照；最新真实模型验证及当前源码校验见 [LIVE_VERIFICATION.md](LIVE_VERIFICATION.md)。

日期：2026-11-09。范围仅为新增 `orchestration/protocol-agent` 嵌套模块，以及案例根 README 的入口链接。核心协议实现仍位于 goagent，未迁入案例项目。

## 结果

- `GOWORK=off go test -race -count=1 -timeout=60s -coverprofile=... ./...` 通过，语句覆盖率76.5%。[测试输出](verification/tests.txt)、[函数覆盖率](verification/coverage.txt)。
- 生命周期修复后，全模块 `go test -race -count=5` 连续5轮通过；随后新增在途请求/Worker关闭测试，最终单轮全模块race仍通过。
- `GOWORK=off go build -o /tmp/goagent-protocol-example .`、`go vet ./...`、golangci-lint 均通过；[lint输出](verification/lint.txt)为0 issues。
- 真正启动服务端与客户端两个进程：cross/mcp/remote/cancel 全部完成；人工输入 reject 得到“未执行模拟发布”；错误令牌客户端退出码1；服务端 SIGINT 退出码0。[进程摘要](verification/process.json)、[全部案例输出](verification/client-all.txt)、[拒绝输出](verification/client-reject.txt)。
- 已核对案例仓库预先存在的84个已修改文件内容哈希，全部保持原样；核心源码与上一轮验收的校验值一致，见 [核心未变校验](verification/core-unchanged.txt)。

## 真实路径

服务端使用同一个 management.Service、内存管理Store、MemorySessionStore和Worker；GraphFactory运行真实DurableGraph，包含AwaitInput、审批节点与明确的拒绝分支。客户端调用官方A2A/MCP HTTP transport，HTTP审批经管理API，不直接修改存储或跳过协议。

测试覆盖全部案例、MCP/HTTP两种拒绝路径、发现卡/工具/管理接口认证、独立客户端命令、输入超时、在途请求与执行器关闭、令牌同源限制，以及存储事务回滚/CAS/分页/可见范围。

## 修复记录

首轮race出现一次HTTP关闭超时。随后为客户端创建独立HTTP连接池并显式清理；宿主关闭先取消Worker和HTTP请求的BaseContext，再等待关闭，避免长task_wait阻挡生产者退出。新增对应关闭回归，连续5轮验证通过。lint首轮的5处未显式处理资源关闭返回值已修正，后续为0问题。

## 独立复核

独立审查代理 Dalton 对 main.go、host.go、client.go、graph.go、store.go 完成静态复核，未发现明确新增bug：确认三入口共用Service且发现卡也鉴权、跨协议审批核对任务身份、reject分支不执行模拟发布、内存事务回滚/深拷贝/CAS完整。

关闭顺序和连接池清理改动后又进行了限定增量复核：独立Transport与清理转发正确；defer先Destroy再CloseIdleConnections；HTTP请求继承runCtx，关闭先BeginShutdown/cancel，再等待HTTP/Worker退出。增量复核通过。独立审查未运行测试，运行结果由主代理提供。

## 范围限制

案例不调用真实模型或外部业务系统，发布结果为合成字符串；不验证真实发布效果。全部状态在内存中，换连接续接不等于进程重启恢复。根模块及其原有修改未作全量回归，本次测试针对新增嵌套模块。依赖同级含新协议适配的goagent工作树；不是仅下载已发布版本即可运行的案例。

源码校验见 [SOURCE_SHA256SUMS](verification/SOURCE_SHA256SUMS)，该校验文件对应引入真实模型前的历史快照，当前代码应使用真实模型报告中的校验文件。当前未提交或推送。
