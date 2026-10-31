# HTTP / SSE / WebSocket / Webhook + Plugin 示例

运行一个带 Web 页面的真实 GoAgent 服务。默认只有模型回答是离线脚本；Builder、Plugin、Dispatcher、工具、HTTP/SSE、WebSocket 和取消均走正式实现。它不是完整聊天平台，不会将每个运行自动变成持久业务会话。

本例使用独立 Go 模块，固定已经推送的 contrib/httpagent 提交。正常运行不需要检出 contrib 或配置本地 workspace；go.mod 不包含本机 replace 路径。

新增 [分场景运行指南](SCENARIOS.md)：JSON调用、SSE工具流、WebSocket执行/取消、双向Webhook及Hook观察。`cmd/client` 可运行全部5条调用路径，`cmd/webhook-receiver` 可观察真实出站通知。

## 直接运行

在本目录执行：

```sh
GOWORK=off go run .
```

## 本地开发运行

仅在修改 contrib 并联调时，按实际路径建立本地 workspace：

```sh
# 仅首次创建；也可以用 GOWORK 指定项目外的临时 workspace。
go work init . /path/to/goagent-contrib/httpagent
# 对尚未推送的本地修改，精确替换固定版本；不要提交机器路径。
go work edit -replace=github.com/kart-io/goagent-contrib/httpagent@v0.0.0-20261031151618-cfb3e85df18a=/path/to/goagent-contrib/httpagent
go run .
```

打开 http://127.0.0.1:8097 。输入中文任务，选择 SSE 或 WebSocket，观察 echo_input 工具。输入“慢任务”可观察 wait_operation，并点击停止。

SSE 的停止按钮中断 HTTP 请求，页面如实显示服务端退出状态尚未确认；服务端实际取消后，日志会出现相同 run_id 的 run.cancelled。WS 显式取消可接收 cancelled 终态，并在 ready 后重新提交。断线不会自动重新提交业务。

可配置：

- `GOAGENT_HTTP_ADDR`：默认 127.0.0.1:8097。
- `WEBHOOK_URL`：配置后，将终态通过真实 HTTP POST 发送到该目标，best-effort 有界投递。
- `GOAGENT_WS_HOST=1`：由示例宿主自行升级 `/api/ws`，通过公开 `WebSocketConn`/`ServeWebSocket` 接入同一执行逻辑；实现与运行步骤见场景指南。
- `GOAGENT_LIVE=1`：使用实际 OpenAI 兼容模型服务；同时设置 `OPENAI_API_KEY`、`OPENAI_BASE_URL`、`OPENAI_MODEL`。可以使用已运行的本地 Ollama；示例不下载模型。模型需支持原生工具流。

GoAgent 核心使用已发布 v0.32.2（含显式零温修复），httpagent 固定远端可获取的 `v0.0.0-20261031151618-cfb3e85df18a`；已补齐 go.sum，并通过 GOWORK=off 的测试、vet 与构建。

## HTTP 与双向 Webhook

```sh
curl -H 'Content-Type: application/json' -d '{"input":"查询订单A1"}' http://127.0.0.1:8097/api/invoke
curl -N -H 'Content-Type: application/json' -d '{"input":"查询订单A1"}' http://127.0.0.1:8097/api/stream
curl -H 'Content-Type: application/json' -d '{"message":"来自Webhook的任务"}' http://127.0.0.1:8097/api/webhooks/demo
```

本例 demo 接收器明确使用 inline，等待完成才应答。第三方平台要求快速确认时，应使用 queued 接收器：注入持久 Acceptor，成功接纳后快速返回200/202，宿主 worker 调用同一 Handler.Run。队列去重、稳定 RunID、崩溃恢复和 outbox 后端由宿主实现，本例不以后台 goroutine 伪造可靠队列。接口与保证见 contrib/httpagent/README.md。

Plugin 的 PreStep、工具以及 HTTP 适配器 Observer 是不同扩展点；通知失败不改变已经冻结的业务结果。共享 Plugin Host 在请求执行和清理全部完成后才关闭。

## 验证

```sh
go test -race -count=1 ./...
go vet ./...
node --test web/*.test.mjs
```

Go 测试通过实际 HTTP 服务验证 Plugin 工具经 JSON/SSE/入站 Webhook 执行。JS 测试覆盖逐字节 UTF-8、CRLF、多 data 行、半帧及无终态 EOF。真实浏览器验证记录位于主库 docs/evaluation/2026-10-31-httpagent-implementation/。
