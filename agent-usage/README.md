# Token 消耗统计案例

这组案例使用真实的 Builder、Agent-as-tool、普通工具、UsageHook 和预算账本执行；模型响应由离线脚本提供，Token 数字是预设的供应商 usage，用来验证统计口径。无需 API Key，不访问模型服务。替换 `demo.NewModel(...)` 为实际模型客户端后，统计代码保持不变，数值取自供应商报告。

从 goagent-examples 仓库根目录运行。go.mod 已固定包含 Token 明细、推理设置和统计钩子的 GoAgent 提交，无需同级源码：

```sh
GOWORK=off go test -race ./agent-usage/...
```

需要联调尚未提交的本地 GoAgent 时，可选执行 make work 创建被 git 忽略的工作区；不提供旧接口分支或复制核心实现。

## 1. 按完成任务统计：多个 Agent + 工具

```sh
go run ./agent-usage/task
```

源码：[task/main.go](task/main.go)。业务流程：

```text
用户任务 → coordinator 分析（3）
             ├─ researcher → lookup_order 普通工具 → 整理（共 4）
             └─ reviewer 审核（5）
         → coordinator 汇总（6）→ 任务完成
```

两个子 Agent 并行执行。父 Builder 注册一次 `WithUsageHook`，后代继承；钩子输出每次模型尝试的 Agent、Token、回放和记账状态。任务结束后通过同一 `controller.Attribution(ctx)` 读取最终账本：

```text
task tokens: input=12 output=6 total=18 model_requests=5 tool_calls=3 complete=true
agent=coordinator self=9
agent=researcher self=4
agent=reviewer self=5
local_tool_runs=1 hook_notifications=5 unknown=0 partial=0
```

并行钩子的先后顺序不固定。每个业务任务创建独立 Controller，并将其 context 传入所有 Agent。`Root.KnownTokens` 是任务累计，覆盖父分析、工具选择、子 Agent 和父汇总。三个工具调用包括两个子 Agent 委派和一次普通工具执行。普通工具本身没有 LLM Token，模型读取工具结果所消耗的 Token 属于之后的模型请求。

## 2. 按单个 Agent／子 Agent 统计

```sh
# 独立 writer Agent
go run ./agent-usage/agent -mode single

# coordinator 调用 writer 子 Agent
go run ./agent-usage/agent -mode child
```

源码：[agent/main.go](agent/main.go)。使用 `budget.WithUsageHook(ctx, hook)` 绑定一次通知，按 `e.Model.AgentID == "writer"` 筛选，并从 `e.Statistics.Agents` 读取当前角色累计。查询失败或没有预算时 `HasStatistics=false`，不能把零值当作累计结果。

| 模式 | writer 输入 / 输出 | writer Self / Inclusive | coordinator Self / Inclusive | 任务 Root |
| --- | --- | --- | --- | --- |
| single | 7 / 3 | 10 / 10 | 无 | 10 |
| child | 7 / 3 | 10 / 10 | 7 / 17 | 17 |

`report.Agents` 按角色 AgentID 汇总 Self；同名 Agent 调用多次时会累计。`report.Calls` 按 AgentCallKey 区分每次逻辑调用，含父关系、Self 和 Inclusive。需要某个具体子任务的消耗时使用调用键筛选；需要该子任务及其全部后代时读取该调用的 Inclusive。

## 统计边界

- `e.Model.Tokens` 是单次模型尝试用量；`e.Statistics` 是时点累计快照。连续快照不能相加，父子 Inclusive 也不能相加。
- 不要使用整个任务前后快照差值计算某个并行子 Agent 的消耗；使用归属报告。
- 不要按通知次数计费或在回放时再次累加。`Replayed`、`AttemptKey`、`Revision` 可用于宿主投影去重；最终累计以账本为准。
- 钩子可能并发调用，回调要支持并发并及时返回；缺失通知可通过账本查询核对。`AccountingCommitted=false` 表示本次证据没有可靠登记。
- 案例完整报告输入、输出、总量；实际供应商可能缺失 usage。检查 `KnownFields`、`Complete`、`UnknownUsageEntries`、`PartialUsageEntries`，已知用量不代表供应商最终账单。
- 第三方工具内部绕过 GoAgent 模型入口直接调用 SDK 时，需自行接入统一计量，框架不会自动探测。

## DeepSeek / MiniMax / Gemini 真实模型验证

真实模型入口见 [live/README.md](live/README.md)，不会回退到离线模型：

```sh
go run ./agent-usage/live -provider gemini -model gemini-2.5-flash -out /tmp/usage-gemini-new
go run ./agent-usage/live -provider deepseek -out /tmp/usage-deepseek-new
go run ./agent-usage/live -provider minimax -out /tmp/usage-minimax-new
```

需配置对应 `API_KEY`、`MODEL`，端点可通过 `BASE_URL` 设置。真实验证覆盖三种场景，保存逐尝试和最终账本证据；费用和结果均来自供应商实际返回的 usage。

## 失败重试与持久回放

保留原有扩展案例：

```sh
go run ./agent-usage
```

它演示两个子 Agent、一次缺少 usage 的失败重试和持久回放。两轮 Root 均为 18，unknown=1；收到 live=5、replay=2 通知。临时目录由示例自行清理。

验证两个新案例：

```sh
go test -race ./agent-usage/...
```

统计契约见[多 Agent 用量指南](https://github.com/kart-io/goagent/blob/master/docs/guides/MULTI_AGENT_USAGE.md)。

## 跨平台与边界案例

[cases/](cases/README.md) 补充 Gemini 父 Agent + DeepSeek/MiniMax 子 Agent 的同任务计量、持久回放，以及受控工具报错、执行中取消。CLI 使用真实模型；离线测试校验预期错误、归属和去重。回放成功后要求客户端调用数及普通工具执行数不增加。
