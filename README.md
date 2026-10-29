# GoAgent 业务案例

基于 [GoAgent](https://github.com/kart-io/goagent) 的业务验证案例：原生工具循环、持久图与审批、崩溃恢复、父子 Agent 共享预算、上下文治理，以及运维 AI 场景。离线测试使用本地模拟模型；真实模型调用需显式开启。

本项目自 goagent 提交 [`d429c47`](https://github.com/kart-io/goagent/tree/d429c47/examples) 迁出，此前的提交历史保留在 goagent 仓库中。

## 目录

| 目录 | 内容 |
| --- | --- |
| [workflow/business-cases](workflow/business-cases/README.md) | DevOps、文章审核、自动回复；含真实模型评测入口 |
| [workflow/refund-recovery](workflow/refund-recovery/README.md) | 退款审批与跨进程崩溃恢复 |
| [orchestration/live](orchestration/live/README.md) | 可组合编排的真实模型案例：审批、多级委派、本地化、长日志排障、审校循环、流式预算、工单分诊 |
| [orchestration/ops](orchestration/ops/README.md) | 运维 AI 案例：告警根因、容量复评、费用归因、受限子调查、SQL 变更审核 |
| `orchestration/` 其余目录 | 子 Agent 工具化、组合订单、上下文恢复、字段适配、并行诊断、Runnable 工具、流式管道 |
| `internal/livemodel` | 真实模型客户端的构造与用量记录，供各案例共用 |
| `scripts/` | 真实模型评测脚本 |

## 依赖 goagent

`go.mod` 固定一个 goagent 版本，单独克隆即可编译和测试。与本地 goagent 联调时，把两个仓库放在同级目录并建立工作区：

```bash
make work        # 生成 go.work，指向 ../goagent（已被 .gitignore 忽略）
make test        # 有 go.work 时使用本地 goagent
make test-remote # 忽略 go.work，只用 go.mod 固定的版本
```

goagent 发布新版本或案例需要新能力时，用 `GOWORK=off go get github.com/kart-io/goagent@<版本>` 更新 `go.mod`。

## 真实模型评测

默认测试不访问网络、不调用付费模型。需要真实调用时按脚本要求设置密钥与模型：

```bash
GEMINI_API_KEY=... GEMINI_MODEL=... scripts/business_cases_live.sh gemini /path/to/report.json
```

`scripts/business_tool_loop_live.sh` 与各案例 README 说明了其余入口与环境变量。历史评测报告保存在 goagent 的 [docs/evaluation](https://github.com/kart-io/goagent/tree/d429c47/docs/evaluation)。
