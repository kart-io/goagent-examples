# 新增可执行案例验收

2026-10-31。案例建立在 examples d21bf52 和 contrib/httpagent d35f054 上，仅修改 examples，未发布扩展。

## 新案例

- `cmd/client -case invoke|sse|ws|webhook|ws-cancel|all`：分别运行同步工具、流式工具、WebSocket、inline Webhook 和显式取消；失败非零退出，无终态不当作成功，不自动重提。
- `cmd/webhook-receiver`：实际HTTP接收出站通知并逐行输出，必要字段/体积/方法校验。仅本地演示，不提供持久存储、去重或认证。
- 现有 Plugin PreStep Hook 增加实际调用计数及日志，与运行生命周期 Observer 分开观察。
- [分步操作与预期结果](../../SCENARIOS.md)，并补齐仓库目录索引。

## 验证

Go均使用 `env -u GOROOT GOTOOLCHAIN=go1.25.0`，通过外部 `GOWORK=/tmp/goagent-httpagent-implementation.work` 绑定尚未发布的新扩展。

| 检查 | 结果 |
| --- | --- |
| `go test -race -count=1 ./...` | 根包1.933秒、接收器1.440秒，全部通过；[race.log](race.log) |
| 五路径集成再跑10轮race | 通过，5.458秒，无竞态；包含相同运行结果、工具/Hook计数及唯一出站通知核对 |
| `go vet ./...` | exit0；[vet.log](vet.log) |
| golangci-lint v2.6.2 | 0 issues；[lint.log](lint.log) |
| `go build ./...` | 成功，三个可执行程序均构建 |
| JS parser/protocol回归 | 6/6通过；[javascript.log](javascript.log) |

测试先因本地实现尚不存在而失败：[案例客户端RED](cases-red.log)、[接收器RED](receiver-red.log)，实现后完整测试通过。客户端实现缺失所致的本地包解析错误不是第三方依赖获取验证。新增测试实际使用原有 Builder、Plugin、Dispatcher 和离线脚本模型；未调用真实付费模型。

独立进程实测运行新构建的接收器（19000）和Agent服务（19097），再执行 `client -case all`：

- [client.jsonl](client.jsonl)：5条结果，4 completed + 1 cancelled。
- [callbacks.jsonl](callbacks.jsonl)：5条真实HTTP终态回调，逐条与客户端run_id匹配，event_id互异，4 run.completed + 1 run.cancelled。
- [server.log](server.log)：实际原生Hook调用9次，生命周期日志独立输出；Agent进程SIGTERM正常exit0。所有自建进程均已停止。

回归另验证：无终态SSE、HTTP失败、WS仅ready没有终态必须返回错误；接收器无效信封/JSON400、GET405、超限413且不打印成已接收事件。

新扩展尚未推送，仍依赖本地workspace；queued持久接纳/生产worker/outbox不是此组inline案例的交付内容。其他多级委派、workflow和业务Hook案例在指南中链接现有实现。
