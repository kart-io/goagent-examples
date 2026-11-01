# 全部案例实际运行报告

日期：2026-11-01。执行范围是 goagent-examples 当前本地工作区的全部 28 个 main 入口，而非只编译或只运行模拟测试。普通真实模型案例统一使用 Gemini gemini-2.5-flash，多平台 Token 案例使用 Gemini、DeepSeek、MiniMax。没有把离线模拟执行归为真实模型调用，也没有宣称完成所有平台与所有参数的笛卡尔积测试。

## 最终结果

- 28/28 个 CLI 入口实际执行，46 次场景／模式调用；首轮两个身份错误修复后，另有 2 次 CLI 重跑通过。全部入口最终通过，无遗漏。
- 9 个编排案例、5 个运维案例的 15 个真实模型测试全部通过（stream-budget 有摘要和争用两项）。包括审批、三层委派、子进程崩溃恢复、会话回放、预算拒绝、证据核验和 SQL 审批复核。
- DevOps、文章审核、自动回复的 6 个真实业务输入样例通过。
- 原生工具循环真实测试通过；模型调用分阶段为 1、1、0，两个合成业务效果、不同幂等键，已知 Token 823。
- 修复后全仓库离线 `go test -race -count=1 ./...` 通过，无数据竞争。

机器汇总见 [summary.json](summary.json)。本报告的“通过”指各案例的验收结果，预期取消、报错、审批等待和故障注入退出 86 不等于业务成功；按对应预期独立校验。

## CLI 逐项覆盖

| 入口 | 执行方式 | 首轮调用数 | 最终结果 |
| --- | --- | ---: | --- |
| [agent-usage](../../../agent-usage/main.go) | 离线确定性／脚本模型 | 1 | 通过 |
| [agent-usage/agent](../../../agent-usage/agent/main.go) | 离线确定性／脚本模型 | 2 | 通过 |
| [agent-usage/cases](../../../agent-usage/cases/main.go) | 三平台混用＋受控错误／取消 | 3 | 通过 |
| [agent-usage/live](../../../agent-usage/live/main.go) | 真实模型 | 3 | 通过 |
| [agent-usage/task](../../../agent-usage/task/main.go) | 离线确定性／脚本模型 | 1 | 通过 |
| [orchestration/agent-tool](../../../orchestration/agent-tool/main.go) | 离线确定性／脚本模型 | 1 | 通过 |
| [orchestration/composed-order](../../../orchestration/composed-order/main.go) | 离线确定性／脚本模型 | 1 | 通过 |
| [orchestration/context-recovery](../../../orchestration/context-recovery/main.go) | 离线确定性／脚本模型 | 1 | 通过 |
| [orchestration/field-adapter](../../../orchestration/field-adapter/main.go) | 离线确定性／脚本模型 | 1 | 通过 |
| [orchestration/live/approval](../../../orchestration/live/approval/main.go) | 真实模型 | 1 | 通过 |
| [orchestration/live/delegation](../../../orchestration/live/delegation/main.go) | 真实模型 | 1 | 通过 |
| [orchestration/live/localize](../../../orchestration/live/localize/main.go) | 真实模型 | 1 | 通过 |
| [orchestration/live/log-triage](../../../orchestration/live/log-triage/main.go) | 真实模型 | 1 | 通过 |
| [orchestration/live/parallel-diagnostics](../../../orchestration/live/parallel-diagnostics/main.go) | 真实模型 | 1 | 修复后复测通过 |
| [orchestration/live/review-loop](../../../orchestration/live/review-loop/main.go) | 真实模型 | 1 | 修复后复测通过 |
| [orchestration/live/session-replay](../../../orchestration/live/session-replay/main.go) | 真实模型 | 4 | 通过 |
| [orchestration/live/stream-budget](../../../orchestration/live/stream-budget/main.go) | 真实模型 | 1 | 通过 |
| [orchestration/live/ticket-triage](../../../orchestration/live/ticket-triage/main.go) | 真实模型 | 1 | 通过 |
| [orchestration/ops/alert-rca](../../../orchestration/ops/alert-rca/main.go) | 真实模型 | 1 | 通过 |
| [orchestration/ops/capacity-review](../../../orchestration/ops/capacity-review/main.go) | 真实模型 | 1 | 通过 |
| [orchestration/ops/cost-attribution](../../../orchestration/ops/cost-attribution/main.go) | 真实模型 | 1 | 通过 |
| [orchestration/ops/scoped-lookup](../../../orchestration/ops/scoped-lookup/main.go) | 真实模型 | 1 | 通过 |
| [orchestration/ops/sql-review](../../../orchestration/ops/sql-review/main.go) | 真实模型 | 1 | 通过 |
| [orchestration/parallel-diagnostics](../../../orchestration/parallel-diagnostics/main.go) | 离线确定性／脚本模型 | 1 | 通过 |
| [orchestration/runnable-tool](../../../orchestration/runnable-tool/main.go) | 离线确定性／脚本模型 | 1 | 通过 |
| [orchestration/stream-pipeline](../../../orchestration/stream-pipeline/main.go) | 离线确定性／脚本模型 | 1 | 通过 |
| [workflow/business-cases](../../../workflow/business-cases/main.go) | 真实模型 | 4 | 通过 |
| [workflow/refund-recovery](../../../workflow/refund-recovery/main.go) | 离线确定性／脚本模型 | 8 | 通过 |

[首轮46次执行记录](cli-results.json)、[两次修复后执行](cli-retests.json)、[初始覆盖统计](coverage.json)、[编译入口清单](builds.json)。初始覆盖统计中的 failed=2 是保留的首轮结果，最终状态以 summary.json 及修复重跑证据为准，未覆盖失败记录。

46 次执行包括单 Agent／子 Agent 两种模式、业务审批、退款两种执行器、故障退出与恢复、会话两轮问答／回放／导出、全部三类 Token 场景，以及混合平台／工具错误／取消。不是把同一个入口的一次默认调用当作全部参数都经过验证。

## 运行中发现并修复

### 并行诊断身份

原案例创建 RootKey=incident/net-1 的预算，但开启 Observer 后没有绑定根 RunID，Observer 自动产生的身份与预算冲突，3 个分支在请求模型前失败。现在在图 Invoke 前显式绑定预算 RootKey，保持宿主 tracer 的父关系。

### 审校循环身份

原预算 RootKey=doc/refund-notice，而持久图 RunID=doc-1，模型执行前拒绝。现在两者统一为 doc-1，没有关闭校验或放宽权限。

新增脚本化端到端回归：并行分支真实经过 Builder 调用；审校经历两轮，完成后同目录回放为零新增模型调用、预算不变。先复现 RED，再修复 GREEN，见 [RED](root-identity-red.log)、[GREEN](root-identity-green.log)。两处真实测试和二进制也重新运行通过。

首轮相关失败的模型进入次数为0，没有因这两个错误额外调用模型。复测使用新隔离目录，未覆盖旧失败产物。独立只读审核未发现阻断问题，未执行额外收费请求或修改文件。

## 真实测试与消耗证据

- [首轮真实测试 JSONL](live-tests.jsonl)保存各测试和子进程恢复结果。
- [修复后真实测试 JSONL](live-root-retest.jsonl)保存并行诊断和审校循环通过结果。
- [业务六样例](business-suite.json)及[日志](business-suite.log)使用真实模型作判断，外部业务动作仍为本地模拟。
- [原生工具循环](native-loop.json)及[日志](native-loop.log)保留崩溃、恢复、重放和已知用量。
- [CLI 预算快照汇总](cli-budget-snapshots.json)从结构化标准输出提取，原始逐例日志位于 logs/。
- Token 用量场景的完整结构化结果位于 outputs/usage-* 和 outputs/boundary-*。
- [离线 race 全量日志](offline-race-final.log)不冒充真实调用结果。

每次 CLI/测试使用独立的合成数据目录。API Key 未写入报告，归档按环境中的实际密钥及 URL 编码值脱敏；脱敏文件清单见 [redactions.json](redactions.json)。不归档二进制和完整持久状态目录，原始本机状态仍在本次 /tmp 运行目录中。

预算快照可能包含历史／回放用量：例如业务审批后仍显示之前判断的 Token，不能把每个快照再相加当作新增费用。会话重放、崩溃恢复和测试中的重复运行也需要区分。本次没有生成不可靠的“全项目最终账单”，供应商未提供的字段不猜测，金额未核对。可用的实际 usage 和调用计数均保留在对应证据中。

## 复现与环境

当前依赖同级最新本地 GoAgent，go.work 连接两个模块；go.mod 固定版本仍待框架发布后升级。工具链 Go 1.25.0，CGO/SQLite 可用，source ~/.zshrc 加载凭据；默认真实测试使用 Gemini。

```sh
make work
# 显式关闭付费 opt-in 的全量离线检查
env -u GOROOT -u GOAGENT_LIVE -u GOAGENT_NATIVE_LIVE -u GOAGENT_BUSINESS_LIVE \
  GOTOOLCHAIN=go1.25.0 go test -race -count=1 ./...
# 真实模型编排与运维检查
GOAGENT_LIVE=1 GOAGENT_LIVE_PROVIDER=gemini GEMINI_MODEL=gemini-2.5-flash \
  go test -count=1 -parallel=1 -p=2 -timeout 20m -run '^TestLive' \
  ./orchestration/live/... ./orchestration/ops/...
```

本次实际 CLI 执行脚本为 [run_cli.py](run_cli.py)，失败两项的构建／重跑为 [retest_cli.py](retest_cli.py)，证据汇总为 [summarize.py](summarize.py)。脚本含本次固定临时目录，复现需换成新目录，避免复用原持久状态；它们是本轮执行记录，不是无需配置就能重复付费执行的通用发布脚本。

所有业务副作用为案例本地模拟，没有调用真实付款、SQL 变更、发布、重启或消息发送接口。模型服务为真实调用；一次成功覆盖不表示模型输出在任意后续输入下都必然成功。

本轮修改只涉及两个案例的身份与新增回归，尚未提交推送。文件和当前源码摘要见 [checksums.json](checksums.json)。
