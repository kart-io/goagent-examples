# A2A/MCP 真实模型验收

2026-11-09，本轮**实际访问真实模型API**，使用现有环境凭据，不是模拟响应。真实请求通过 GoAgent Provider → Builder → DurableGraph → 同一管理任务服务执行，结果由官方 A2A/MCP 客户端取回；没有绕过协议直接把模型结果塞进测试。

## 实际结果

| 提供方 / 请求模型 | cross | mcp | remote | 总生成调用 | 供应商报告token总量 |
| --- | --- | --- | --- | --- | --- |
| DeepSeek / deepseek-flash | 通过，284 tokens | 通过，277 tokens | 通过，226 tokens | 3 | 787 |
| Gemini / gemini-2.5-flash | 通过，558 tokens | 通过，561 tokens | 通过，433 tokens | 3 | 1552 |

共6次成功生成，供应商报告token合计2339。每个成功任务均包含 `PROTOCOL-LIVE-OK` 和正数用量。模型字段记录实际请求名，不推断提供方背后的内部模型版本。Gemini 的 reasoning 子项已包含在供应商总量中，未重复相加。

- **cross**：A2A 提交 → MCP 普通输入 → 序列化检查点后换连接 → HTTP 人工审批 → 真实模型摘要 → 三入口核对相同任务终态。
- **mcp**：MCP工具提交，宿主回调提供两轮输入/审批，批准后调用真实模型，取回正文与用量。
- **remote**：经 `a2a/remote.Tool` 调用真实模型驱动的远端Agent。
- **cancel**：本地等待执行器显式取消，两个提供方下均为0生成调用。此项不证明正在生成的上游模型请求可取消。
- **cross-reject / mcp-reject**：两条审批路径均返回拒绝结果，生成调用增量为0；模型不能替代审批。审批测试使用明确的合成输入，不执行任何外部发布。

DeepSeek完成时间 `2026-11-09T06:28:52Z`，Gemini完成时间 `2026-11-09T06:28:54Z`，均为UTC。

Gemini的cross实际回复：

> 已收到合成的示例发布请求及离线合成发布说明。PROTOCOL-LIVE-OK

## 可核对证据

- [DeepSeek结构化证据](verification/live-2026-11-09/deepseek.json)、[原始测试日志](verification/live-2026-11-09/deepseek.txt)。
- [Gemini结构化证据](verification/live-2026-11-09/gemini.json)、[原始测试日志](verification/live-2026-11-09/gemini.txt)。
- JSON逐项保存Task ID、业务输出、Provider调用增量、模型名、prompt/completion/reasoning/total用量。日志归档前检查未包含环境密钥；不记录模型思考正文。
- [最终离线race](verification/live-2026-11-09/offline.txt)、[lint](verification/live-2026-11-09/lint.txt)、[vet](verification/live-2026-11-09/vet.txt)、[build](verification/live-2026-11-09/build.txt)均退出0。离线语句覆盖率74.4%；默认跳过真实调用测试不计为在线验收通过。

## 复现命令

在 `goagent-examples/orchestration/protocol-agent` 下运行，环境中需已有相应API密钥：

```bash
env -u GOROOT GOWORK=off GOAGENT_PROTOCOL_LIVE=1 \
  GOAGENT_PROTOCOL_PROVIDER=deepseek \
  GOAGENT_PROTOCOL_REPORT=/tmp/protocol-deepseek.json \
  go test -race -count=1 -timeout=5m -run '^TestLiveModelProtocols$' -v ./...

env -u GOROOT GOWORK=off GOAGENT_PROTOCOL_LIVE=1 \
  GOAGENT_PROTOCOL_PROVIDER=gemini \
  GOAGENT_PROTOCOL_REPORT=/tmp/protocol-gemini.json \
  go test -race -count=1 -timeout=5m -run '^TestLiveModelProtocols$' -v ./...
```

两条命令已实际执行且退出0，没有SKIP。每条最多3次生成、每次60秒/2048输出token，没有自动重试。两家模型的目录API也已成功访问，确认请求模型可用。官方模型目录资料：[DeepSeek](https://api-docs.deepseek.com/api/list-models/)、[Gemini](https://ai.google.dev/api/models)。

CLI也可使用 `demo -auto -live -provider deepseek -max-model-requests 3 -timeout 4m`；完整用法见 [README](README.md)。已有测试验证模型请求硬上限、已完成节点回放零新增调用；默认离线模式不受影响。

## 改动与边界

新增 `model.go`/`model_test.go`，为服务端增加可选模型后端及显式 `-live` 开关；输出附带真实用量。初始任务输入禁止伪造model_evidence，普通输入和审批继续经过原协议与授权检查。本轮只改新增案例模块，案例仓库原有84个已修改文件保持原样。

使用真实HTTP/API和真实token计量，但业务仍为合成发布，存储仍为内存，不证明生产发布或数据库故障恢复。默认只支持DeepSeek/Gemini；此次没有调用MiniMax。源码校验见 [当前快照](verification/live-2026-11-09/SOURCE_SHA256SUMS)，在本模块目录用 `sha256sum -c verification/live-2026-11-09/SOURCE_SHA256SUMS` 核对。

## 独立增量复核

独立审查代理 Planck 完成静态复核，未发现明确bug：确认live门禁、请求上限、宿主审批边界、Provider入口计数、响应用量、密钥脱敏及失败拒绝路径；核对两家各3次调用和拒绝/取消零调用证据一致。审查代理未发起模型请求、未修改文件。真实运行与最终门禁由主代理执行。本轮未提交或推送。
