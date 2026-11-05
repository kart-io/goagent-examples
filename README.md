# GoAgent 业务案例

基于 [GoAgent](https://github.com/kart-io/goagent) 的业务验证案例：原生工具循环、持久图与审批、崩溃恢复、父子 Agent 共享预算、上下文治理，以及运维 AI 场景。离线测试使用本地模拟模型；真实模型调用需显式开启。

本项目自 goagent 提交 [`d429c47`](https://github.com/kart-io/goagent/tree/d429c47/examples) 迁出，此前的提交历史保留在 goagent 仓库中。

## 目录

多 Agent 场景扩展需求见 [需求草案](docs/requirements/multi-agent-scenarios.md)，定义自然语言意图路由、多轮澄清、Tool/Workflow/子 Agent 执行选择、返回契约与分阶段验收；新增案例尚未按该文档实施。路由方案比较与执行边界见 [调研分析](docs/research/intent-execution-routing.md)。 多 Agent 上下文传递的场景、隔离边界及候选案例见 [上下文协作分析](docs/research/2026-11-01-multi-agent-context.md)，只读 P1 案例见 [agent-context](orchestration/agent-context/README.md)，支持 delegation、pipeline、parallel；Fork、共享文件提交和持久恢复另行验收。

| 目录 | 内容 |
| --- | --- |
| [workflow/business-cases](workflow/business-cases/README.md) | DevOps、文章审核、自动回复；含真实模型评测入口 |
| [orchestration/agent-context](orchestration/agent-context/README.md) | 父子 Agent 简报投影、串行依赖、并行专家、证据与报告采纳、会话隔离及根请求/token 统计；只读合成差旅场景 |
| [orchestration/flight-booking](orchestration/flight-booking/README.md) | 机票预订：Agent 澄清与只读比价、Workflow 两次确认、支付出票核对与崩溃恢复；业务全部模拟 |
| [orchestration/customer-service](orchestration/customer-service/README.md) | 客服编排：分流、争议退款人工审核、追问、超时与转人工；退款写入模拟账本，需本地 goagent 工作区 |
| [workflow/refund-recovery](workflow/refund-recovery/README.md) | 退款审批与跨进程崩溃恢复 |
| [orchestration/live](orchestration/live/README.md) | 可组合编排的真实模型案例：审批、多级委派、本地化、长日志排障、审校循环、流式预算、工单分诊、会话日志与重放 |
| [orchestration/ops](orchestration/ops/README.md) | 运维 AI 案例：告警根因、容量复评、费用归因、受限子调查、SQL 变更审核 |
| [orchestration/plugin](orchestration/plugin/basic/README.md) | Agent 能力包：组合与冲突、循环钩子、资源与依赖、持久运行的客服端到端 |
| [orchestration/acp](orchestration/acp/README.md) | 以 ACP Agent 身份在 Zed 等编辑器中运行：工具通知、执行命令前审批、文件与终端经编辑器 |
| [orchestration/httpagent](orchestration/httpagent/SCENARIOS.md) | 独立模块：Web页面、JSON/SSE/WS客户端、取消、双向Webhook与Hooks；固定已推送扩展提交，可独立运行 |
| `orchestration/` 其余目录 | 子 Agent 工具化、组合订单、上下文恢复、字段适配、并行诊断、Runnable 工具、流式管道 |
| [agent-usage](agent-usage/README.md) | Token 统计：单 Agent、父子协作、多平台同任务、推理明细、失败／取消与持久回放；含真实调用与完整验证报告 |
| `internal/livemodel` | 真实模型客户端的构造与用量记录，供各案例共用 |
| `scripts/` | 真实模型评测脚本 |

## 依赖 goagent

根模块的 go.mod 固定包含 Token 明细与推理接口的 GoAgent 提交，单独克隆即可编译和测试。HTTP Agent 案例使用独立嵌套模块，其固定 contrib 依赖及运行要求见该目录 README；以下根模块命令不会递归测试嵌套模块。

与本地 goagent 联调时，把两个仓库放在同级目录并建立可选工作区：

```bash
make work        # 可选：生成 go.work，指向 ../goagent（已被 .gitignore 忽略）
make test        # 有 go.work 时使用本地 goagent
make test-remote # 忽略本地工作区，验证 go.mod 固定的远端提交
```

goagent 发布新版本或案例需要新能力时，用 `GOWORK=off go get github.com/kart-io/goagent@<版本>` 更新 `go.mod`。

## 真实模型评测

默认测试不访问网络、不调用付费模型。需要真实调用时按脚本要求设置密钥与模型：

```bash
GEMINI_API_KEY=... GEMINI_MODEL=... scripts/business_cases_live.sh gemini /path/to/report.json
```

`scripts/business_tool_loop_live.sh` 与各案例 README 说明了其余入口与环境变量。历史评测报告保存在 goagent 的 [docs/evaluation](https://github.com/kart-io/goagent/tree/d429c47/docs/evaluation)。
