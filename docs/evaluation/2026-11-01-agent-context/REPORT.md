# Agent Context P1 验收记录

日期：2026-11-01。范围：`orchestration/agent-context` 只读案例与设计修订。

## 交付与边界

保留 delegation、pipeline、parallel。父 Agent 只派发角色简报；宿主保存调用身份、修订、候选对象和依赖，子 Agent 通过原生工具读取证据、提交短报告。宿主校验证据含义、确认决定、产物摘要、依赖资格和采纳状态。取消、替换和采纳共享锁；真实调用即使失败或报告被拒绝也保留用量。

本次没有修改 GoAgent 核心 API 或依赖版本，examples 根模块复用 `v0.32.3-0.20261101061326-da848ae4f5e2`，HTTP 嵌套模块保留原有 `v0.32.2`；验证时使用 `GOWORK=off`，没有临时 replace。Builder 唯一预留请求预算，传输包装层仅限制根任务并发。P1 硬上限为根请求 12、根模型并发 2、子任务并发 2、深度 1、任务 5 分钟；token 为观测。Fork、共享文件提交、跨进程恢复和实际 token 停派分别留待 F/W/P2 验收。

## 专业审核与真实调用发现

专业代码审核发现并修复：单次子调用取消未传播到实际 Builder，以及模型可添加未经宿主确认的决定 ID。随后修复注册与取消回调绑定之间的竞态，增加确定性回归。

首轮真实模型测试三平台均未通过。结论字段没有枚举约束，模型返回了宿主不支持的值；可选空列表被自动 Schema 设为必填；Gemini 还发生输出预算截断。保留首轮日志与实际用量，不将失败调用计为零。修复必须作用于实际 Schema、提示词及案例输出预算，不降低宿主校验标准。

## 兼容性验证

| 检查 | 结果与证据 |
| --- | --- |
| GoAgent 根模块 `go test ./...` | 退出 0，54 个通过包，部分缓存；`core-baseline.log` |
| GoAgent 根模块 build | 退出 0 |
| frameworkbench 嵌套模块 test/build | 退出 0；`frameworkbench-baseline.log`；本次未做性能重基准 |
| HTTP 嵌套模块 race/build | 退出 0，部分缓存；`http-baseline.log` |
| HTTP 页面协议 Node tests | 6 项通过；`http-web.log` |
| 机票 CLI 场景回归 | 20 项通过；`flight-cli-regression.json` |

入口清单以 `2140026` 为基线：原有 37 个可执行入口保留，新 P1 增加 1 个，共 38 个；`entry-inventory.json` 可复核。测试和构建覆盖原有源码；没有逐个付费执行所有旧 CLI。不存在候选新核心升级，因此以上结果不表述为新核心版本兼容认证。

Examples 根模块最终全量 `go test -race -count=1 ./...` 与 build 退出 0，见 `examples-race.log` 和 `examples-build.log`。离线 CLI 三种模式各 4 次请求、60 个合成 token，子任务全部关闭、传输剩余为 0，见 `offline-cli.json`。并发峰值受调度影响，不能凭一次 CLI 峰值 1 推断无法并发；同步屏障回归单独证明峰值 2。

## P1 最终结果

DeepSeek 与 MiniMax 在最终稳定实现的 delegation/pipeline/parallel 全部通过。Gemini Flash 的三个模式分别在真实调用中成功，但最后整轮仍有连续生成非法函数调用的失败，不能宣称该模型稳定通过整体验收。有限恢复已在 Gemini parallel pilot3 实际生效：5 次请求、2 份报告采纳，失败调用的部分用量保留。Gemini Pro 对照三种模式均通过，三平台九项门槛在下列明确模型配置上通过；这不消除 Flash 的间歇性失败。

本轮不增加无限重试。精确 Gemini 原生 MALFORMED_FUNCTION_CALL 只有在 child 未发出输出、ctx 有效时，每 invocation 一次 Builder 恢复；失败 usage 先入账，下一 attempt 独立预留。6 组实际 Builder 回归覆盖恢复次数、额外用量、根预算、取消、已发出输出、其他错误/平台和宿主校验拒绝。


最终验收配置（每项均 4 请求=4 预留，inflight=0、子任务全部关闭、全部报告 succeeded/feasible/accepted，usage 完整）：

| 模型 | delegation 总 token | pipeline 总 token | parallel 总 token | 结果 |
| --- | ---: | ---: | ---: | --- |
| deepseek-flash | 8276 | 12589 | 10769 | 3/3 PASS |
| MiniMax-M3.1-Flash（CN） | 8162 | 12566 | 11240 | 3/3 PASS |
| gemini-2.5-pro | 10291 | 16284 | 14748 | 3/3 PASS |

对应 `deepseek-verification.log`、`minimax-verification.log`、`gemini-pro-verification.log`。Gemini Flash 对应的 `gemini-verification.log` 是失败记录，不能混入该成功表。保留三个历史 attempt 及三个 Gemini pilot；`live-usage.json` 共 42 条互不重复的根快照。

## 可复现命令

使用 Go 1.25：`env -u GOROOT GOTOOLCHAIN=go1.25.0 GOWORK=off`。各独立模块分别执行 `go test ./...`（examples/HTTP 加 `-race`）和 `go build ./...`。核心根模块的既有基线设置 `GO_TEST_MODE=true`；examples 根模块离线验证取消 `GOAGENT_LIVE`、`GOAGENT_NATIVE_LIVE`、`GOAGENT_BUSINESS_LIVE`；HTTP 和 frameworkbench 在各自模块目录使用 `GOWORK=off`。HTTP 页面使用 `node --test web/*.test.mjs`。机票 CLI 使用 `python3 scripts/flight_booking_smoke.py /tmp/goagent-flight-booking`。

新案例离线入口、实际模型运行命令见 [案例 README](../../../orchestration/agent-context/README.md)。真实测试在环境变量配置后执行 `go test -race -count=1 -timeout 15m -run '^TestLiveAgentContext$' -v ./orchestration/agent-context`，显式设置 `GOAGENT_LIVE=1`、`GOAGENT_LIVE_PROVIDER`；MiniMax 使用 `MINIMAX_BASE_URL=https://api.minimax.cn/v1`。日志移除凭据后归档。

## 用量解释

`live-usage.json` 每次运行只取一个根统计快照，不能再加同一日志的逐 Agent inclusive 统计。`TotalTokens` 使用平台报告值，缓存与推理 token 为明细，不再重复相加。未知/部分证据单列，失败与被拒绝结果的实际消耗仍保留；已知 token 不等同于可精确对账的供应商账单。三个平台的消息分词、推理开销和缓存口径不同，因此不要求相同 token 数。


## 最后收尾与判断

完整分支专业审核及最终限定复审均通过，无未关闭发现。真实模型测试后，最后修正“模型只返回自由文本、没有报告”的失败记账路径，新增实际 Builder 回归并重新执行全量离线 race/build；未为这一错误路径改动重复付费调用。

实施选择及代价：按既定只读 P1 方案执行、以实证缺口作为修改核心的前提，额外示例范围可在独立分支回退；模型仅提交已读取的证据 ID，完整引用的解析与校验由宿主承担，通用引用展示因此依赖宿主；精确 Gemini 生成错误最多恢复一次，每个子调用可能增加一次有账目的请求，连续失败仍会退出。

校验文件清单见 `SHA256SUMS`。本轮保留 feature 分支，不推送、不合并。
