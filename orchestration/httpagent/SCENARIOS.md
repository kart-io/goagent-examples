# HTTP、流式工具、WebSocket、双向 Webhook 与 Hooks 案例

所有命令在 `orchestration/httpagent` 目录执行。固定扩展提交已推送，可直接下载依赖，无需本地 workspace；如上级目录存在开发 workspace，可在每个终端设置 `GOWORK=off` 使用远端依赖。修改 contrib 并联调时才按 [README](README.md#本地开发运行) 设置 workspace。默认离线模型，实际使用 Builder、Plugin、Dispatcher、工具和网络传输。

## 1. 启动出站通知接收器

终端一：

```sh
go run ./cmd/webhook-receiver -listen 127.0.0.1:9000
```

它接收 `POST /events`，将实际通知信封逐行输出到 stdout，成功返回204。信封含 `event_id`、`run_id`、`event_type` 和结果数据；错误请求不会打印为成功通知。接收器限1MiB，并验证必要信封字段。它仅用于本地观察，不提供认证、持久存储或去重；重新投递同一事件会再次打印。

终端二启动 Agent 服务：

```sh
WEBHOOK_URL=http://127.0.0.1:9000/events go run .
```

服务日志会分别显示 `原生 Hook demo-input` 和 `事件 run.started/run.completed/run.cancelled`。Hook 在模型请求前运行，Observer 在整个运行的生命周期触发，二者次数和作用不同。

## 2. JSON 同步调用

终端三：

```sh
go run ./cmd/client -case invoke -input '查询订单 E100'
```

路径：用户 → POST /api/invoke → Agent → echo_input 工具 → JSON结果。客户端打印 `case/run_id/status/result`；接收器打印相同 run_id 的 run.completed。同步调用也保留原生工具循环。

## 3. SSE 流式工具与结果

```sh
go run ./cmd/client -case sse -input '查询订单 E101'
```

客户端逐帧解析服务端事件，记录 tool_start，收到唯一 done/error 后确认结果；没有终态的 EOF 返回错误，不自动重提业务。打印结果中的 `tools` 应含 `echo_input`。需要直接观察文本/step/usage/done，可运行：

```sh
curl -N -H 'Content-Type: application/json' \
  -d '{"input":"查询订单 E101"}' http://127.0.0.1:8097/api/stream
```

## 4. WebSocket 执行及显式取消

```sh
go run ./cmd/client -case ws -input '查询订单 E102'
go run ./cmd/client -case ws-cancel
```

使用 `goagent.v1` 子协议：run → accepted → event → done/error → ready。取消案例固定使用慢任务，在实际收到 wait_operation 的 tool_start 后发送匹配 run_id 的 cancel；确认 cancelled 终态并等 ready 才成功退出。没有业务终态就收到 ready，或断线未确认，均返回错误。每次命令独立连接，不演示跨连接恢复。

浏览器入口 http://127.0.0.1:8097 提供相同的 SSE/WS 模式、工具状态和停止按钮。SSE 停止中断客户端连接，页面显示未确认；WS 停止通过协议确认取消。两者不自动重新执行请求。

## 5. 入站 Webhook → Agent → 出站通知

```sh
go run ./cmd/client -case webhook -input '收到订单 E103 的状态通知'
```

路径：业务事件 → POST /api/webhooks/demo → Decode → Agent → 工具 → HTTP结果，同时向终端一发送 run.completed 通知。入站是明确的 inline 模式，等待完成再应答；出站由独立 Sender 执行有界 best-effort 投递，通知失败不会重跑 Agent。

这里不声称适配了第三方平台的验签、challenge 或快速ACK要求。需要先ACK再执行时，使用 queued Receiver + 宿主持久 Acceptor + 独立 worker 调用 Handler.Run；当前案例没有生产队列/outbox，不用后台 goroutine 代替持久接纳。

## 6. 一次执行全部调用案例

```sh
go run ./cmd/client -case all -input '查询订单 E200'
```

预期打印5行JSON，前4行 status=completed，最后一行 status=cancelled；接收器打印4条 run.completed 和1条 run.cancelled。每条结果和通知可通过 run_id 对照。未开启 WEBHOOK_URL 时调用仍可运行，但不会有出站通知。参数 `-url` 可改为本例服务的其他HTTP地址，默认每个案例30秒期限。

```sh
go test -race -count=1 ./...
go vet ./...
node --test web/*.test.mjs
```

`TestExecutableCasesUseToolsHooksAndOutboundWebhook` 通过真实HTTP/WS跑完5条路径，核对工具、PreStep Hook、结果与唯一终态回调。错误流、ready缺少终态、HTTP失败及回调接收器非法/超大输入均有回归。

## 宿主框架自行升级 WebSocket

如需由现有框架接管握手，可直接运行本仓的宿主适配案例：

```sh
GOAGENT_WS_HOST=1 go run .
# 另一个终端
go run ./cmd/client -case ws -input '宿主框架接入订单 E300'
go run ./cmd/client -case ws-cancel
```

此模式仅替换 `/api/ws`，页面、JSON/SSE和Webhook继续使用同一应用。完整适配代码在 [internal/hostws/handler.go](internal/hostws/handler.go)：宿主完成GET/Origin/goagent.v1握手检查，将连接包装为 `httpagent.WebSocketConn`，然后阻塞调用 `h.ServeWebSocket(ctx, conn)`。Handler管理升级后的容量、读写、心跳、取消和停服，宿主管理升级前的握手与认证；不要在框架中再启动第二个reader/writer。

该案例的宿主选择Gorilla作为帧库，独立实现接口包装，仅依赖httpagent公开API。其他框架可替换该包装；它不是已适配所有框架的声明。使用net/http且希望保留Handler握手管理时，可改用 `Config.WebSocketUpgrader`/`WebSocketUpgraderFunc` 注入升级器，传入的 `WebSocketUpgradeOptions` 包含子协议和握手期限。

接口适配必须真正实现消息上限、读写期限、Pong回调及解除阻塞的Close，不可用空方法占位。框架升级前须验证Origin、认证及子协议，并使用持续到连接结束的context。已经升级后遭遇容量或停服拒绝，会关闭连接，不再返回HTTP接纳状态。更多契约见contrib/httpagent README的“接入其他Web框架或WebSocket库”。

`TestHostOwnedUpgradeExecutesPluginAndCancels` 使用真实HTTP/WS及正式Plugin工具验证执行/取消；`TestHostOwnedUpgradeKeepsHandshakePolicy` 验证错误Origin和子协议在工具执行前拒绝。

## 其他业务编排案例

HTTP 是入口适配层，可在 Factory 内装配已有业务 Agent；本例不额外复制业务编排逻辑。

- [多级子 Agent 委派](../live/delegation/README.md)：经理 → 专家子 Agent → workflow/函数工具，共享根预算。
- [业务 workflow](../../workflow/business-cases/README.md)：运维、文章审核、自动回复，含审批与恢复。
- [原生 Hook](../plugin/hooks/README.md)：脱敏、限流重试、答案校验和审计；本例的 demo-input Hook 仅展示调用时机，不执行上述业务策略。
