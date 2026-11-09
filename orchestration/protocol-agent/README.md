# A2A / MCP 远程 Agent 案例

在一个 Go 进程中启动 **同一个 management.Service、Worker 和任务存储**，同时挂载 A2A、MCP 与管理 HTTP。客户端通过官方 SDK 演示跨协议输入、审批、查询、远端 Agent 工具调用及取消。

业务使用真实 `workflow.DurableGraph` 和 `management/adapters.GraphFactory`，流程为“补充发布说明 → 人工审批 → 返回合成发布结果”。默认离线模式无需模型密钥或数据库，不调用付费服务，不执行真实发布；显式 `-live` 可接入真实模型生成摘要。

## 一条命令运行全部案例

两个仓库需要处于同级目录，`goagent` 中包含本轮新增的协议适配代码：

```text
kart-io/
├── goagent/
└── goagent-examples/orchestration/protocol-agent/
```

```bash
cd goagent-examples/orchestration/protocol-agent
env -u GOROOT GOWORK=off go run . demo -auto
# 或 make demo
```

独立模块使用 Go 1.27.3 工具链，`go.mod` 的 replace 指向 `../../../goagent`；新协议代码尚未发布，所以目前不能仅依赖已发布的 v0.51.0。这里的 `GOWORK=off` 用于避免上级工作区遗漏嵌套模块，不会修改根模块依赖或 go.work。

`demo` 自动监听随机回环端口，创建临时令牌，执行后关闭服务。`-auto` 明确使用合成输入和预设批准；不加此参数时，从终端读取发布说明及 approve/reject 决定。

结束时输出 JSON 数组，四项结果预期为：

```json
[
  {"case":"cross","state":"succeeded"},
  {"case":"mcp","state":"succeeded"},
  {"case":"remote","state":"succeeded"},
  {"case":"cancel","state":"canceled"}
]
```

实际输出还包含 task_id 和业务 result。拒绝审批也会正常结束任务，但 result 明确为“用户拒绝，未执行模拟发布”，不能将管理状态 succeeded 理解为用户批准。

## 分开启动服务端和客户端

终端一：

```bash
cd goagent-examples/orchestration/protocol-agent
export DEMO_TOKEN='local-demo-token'
env -u GOROOT GOWORK=off go run . serve -listen 127.0.0.1:8080
```

终端二：

```bash
cd goagent-examples/orchestration/protocol-agent
export DEMO_TOKEN='local-demo-token'
env -u GOROOT GOWORK=off go run . client \
  -url http://127.0.0.1:8080 -case all -auto
```

所有入口均要求同一个 Bearer token，服务端将它映射为固定演示 Actor。监听仅允许回环 IP；令牌不会写入启动输出。客户端只向配置的同源地址发送令牌，拒绝跨主机转发和自动跟随重定向。

| 入口 | 地址 |
| --- | --- |
| A2A 能力发现 | `GET /.well-known/agent-card.json` |
| A2A JSON-RPC / SSE | `/a2a` |
| MCP Streamable HTTP | `/mcp` |
| 管理 HTTP | `/v1/` |

可直接查看 Card：

```bash
curl -fsS -H "Authorization: Bearer $DEMO_TOKEN" \
  http://127.0.0.1:8080/.well-known/agent-card.json
```

服务端 Ctrl+C 后取消在途请求和执行器，再等待 HTTP/Worker 退出。客户端 `-timeout` 默认2分钟，包含人工输入等待；取消客户端不会自动取消远端任务，操作长任务应使用显式取消接口。

## 四个案例

| `-case` | 路径与关键行为 | 主要代码 |
| --- | --- | --- |
| `cross` | A2A 提交 → MCP 返回输入检查点 → 序列化后关闭连接 → 新 MCP 连接回复 → HTTP 审批 → A2A/MCP/HTTP 核对同一任务终态 | `demoClient.cross` |
| `mcp` | `review_agent` 工具提交 → `task_wait` 经 WithInputHandler 连续收集输入和审批，最多4轮 | `demoClient.mcpWorkflow` |
| `remote` | 通过 `a2a/remote.Tool` 把远端 Agent 当作 `interfaces.Tool` 调用 | `demoClient.remoteAgent` |
| `cancel` | A2A 流式提交 → 读取首事件并断开 → 确认任务仍在运行 → MCP 显式取消 → A2A 确认 canceled | `demoClient.cancelTask` |

所有实现位于 [client.go](client.go)。单独运行一个案例：

```bash
env -u GOROOT GOWORK=off go run . demo -case cross
# 依次输入发布说明、approve 或 reject。

env -u GOROOT GOWORK=off go run . demo -case cancel -auto
```

## 真实模型验证

只有 `demo -live`、`serve -live` 或设置专用测试开关才会发起模型请求。默认离线测试保持不联网。密钥从环境读取，不要写入命令参数或报告。

```bash
# 环境中已有 DEEPSEEK_API_KEY 时：
env -u GOROOT GOWORK=off go run . demo -auto -live \
  -provider deepseek -model deepseek-flash -max-model-requests 3 -timeout 4m

# 环境中已有 GEMINI_API_KEY 时：
env -u GOROOT GOWORK=off go run . demo -auto -live \
  -provider gemini -model gemini-2.5-flash -max-model-requests 3 -timeout 4m
```

也可先 `serve -live -provider ...`，再用原 `client` 命令连接。`-live` 只设置在服务端；客户端不创建模型。省略 `-model` 时读取 `DEEPSEEK_MODEL` 或 `GEMINI_MODEL`，再使用上述默认模型。Gemini 默认思考预算512、总输出上限2048，已验证模型为2.5 Flash。

三个成功业务案例各发起一次模型请求：cross/mcp 在用户批准后生成摘要，remote 通过外部 A2A Agent 生成摘要。模型经真实 Builder 在 DurableGraph 节点中执行，并使用稳定 durable scope；成功回放不再次调用模型。审批仍由用户或显式 `-auto` 合成输入决定，模型没有审批权限。reject 分支和 cancel 案例不发起生成请求；cancel 验证的是任务执行器取消，不是取消正在生成的上游模型请求。

每个模型请求上限60秒/2048输出tokens；进程默认最多3次Provider调用，硬上限可设1–20，不做自动重试。模型正文必须包含 `PROTOCOL-LIVE-OK` 且供应商返回正数token总量，缺失即失败。任务结果增加 `model_evidence`（提供方、请求模型名、原始token用量），CLI结束时输出本进程请求数。上游返回的总量原样保存，不将 reasoning 等子项重复相加。

运行真实验收测试（每次最多3次生成，并额外验证两种审批拒绝均为零请求）：

```bash
env -u GOROOT GOWORK=off GOAGENT_PROTOCOL_LIVE=1 \
  GOAGENT_PROTOCOL_PROVIDER=deepseek \
  GOAGENT_PROTOCOL_REPORT=/tmp/protocol-deepseek.json \
  go test -race -count=1 -timeout=5m -run '^TestLiveModelProtocols$' -v ./...
```

将 provider 改为 gemini 可验证 Gemini。默认 `go test` 会跳过真实调用测试；这不算在线验收通过。已实际执行的两家模型结果见 [真实模型报告](LIVE_VERIFICATION.md)。

## 代码导览

| 文件 | 用途 |
| --- | --- |
| [main.go](main.go) | demo/serve/client 命令、超时与信号退出 |
| [host.go](host.go) | 管理服务、Agent 发布、三个协议入口、认证与资源关闭 |
| [graph.go](graph.go) | AwaitInput、真实审批节点、明确的拒绝结果、可取消执行 |
| [client.go](client.go) | 官方 A2A/MCP 客户端、人工输入处理器及四条端到端路径 |
| [model.go](model.go) | 显式真实模型接入、Builder、请求硬上限及用量证据 |
| [store.go](store.go) | 演示内存管理 Store，串行事务、回滚、CAS、租户与分页过滤 |
| [main_test.go](main_test.go) | 全案例、两种拒绝路径、认证、独立客户端、输入期限及配置回归 |
| [store_test.go](store_test.go) | 内存存储事务失败回滚、CAS、别名隔离、分页与可见范围 |

## 验证

```bash
env -u GOROOT GOWORK=off go test -race -count=1 -timeout=60s ./...
env -u GOROOT GOWORK=off go vet ./...
env -u GOROOT GOWORK=off go build -o /tmp/goagent-protocol-agent .
# 或 make test / make build
```

根模块的 `go test ./...` 不递归测试此嵌套模块。请使用以上命令或本目录 Makefile。实际验证结果见 [VERIFICATION.md](VERIFICATION.md)。

## 适用边界

管理数据、会话与图日志全部存于内存，**进程退出即丢失**。案例展示同进程跨连接续接，不证明进程重启或数据库故障恢复。生产接入应注入持久 Store/Session Store、真实身份授权及运行监控。

MCP 使用2026多轮输入和任务工具契约，没有宣告原生 tasks 扩展。A2A 状态流基于持久快照轮询，当前不支持 ListTasks、消息历史、文件或多 part 输入。官方 A2A 客户端的 any 数字解码有精度边界，精确大整数和小数应使用字符串。完整协议契约见同级核心仓库的 `docs/guides/PROTOCOL_ADAPTERS.md`。
