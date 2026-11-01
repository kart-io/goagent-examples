# DeepSeek / MiniMax / Gemini 真实模型 Token 验证

这是显式运行的真实网络验证入口，不会回退到模拟模型。覆盖独立 Agent、父子 Agent、两个子 Agent 与普通工具组成的完整任务。会产生供应商调用费用。

在本机安全配置相应环境变量，不要将密钥写入仓库或报告：

- Gemini：`GEMINI_API_KEY`、`GEMINI_MODEL`，可选 `GEMINI_BASE_URL`。
- DeepSeek：`DEEPSEEK_API_KEY`、`DEEPSEEK_MODEL`，可选 `DEEPSEEK_BASE_URL`。
- MiniMax：`MINIMAX_API_KEY`、`MINIMAX_MODEL`，可选 `MINIMAX_BASE_URL`，默认使用官方国际站 `https://api.minimax.io/v1`。国内站或代理需显式设置匹配凭据的端点；国内站官方端点为 `https://api.minimax.cn/v1`。

从仓库根目录执行，输出目录必须不存在：

```sh
go run ./agent-usage/live -provider gemini -model gemini-2.5-flash -out /tmp/usage-gemini-new
go run ./agent-usage/live -provider deepseek -out /tmp/usage-deepseek-new
go run ./agent-usage/live -provider minimax -out /tmp/usage-minimax-new
```

也可显式传入 `-model MODEL_NAME`，或通过 `-scenario single|child|task` 单独执行场景。未指定时执行全部三种场景。模型名需按实际账号可用模型设置，不猜测模型别名。

Gemini 使用现有原生 Provider，保留工具续接与 usage；其余入口使用 DeepSeek Provider 和现有 OpenAI 兼容 Provider（MiniMax），保留原生工具调用、推理续接及流式 usage。协议依据：[DeepSeek Chat Completions](https://api-docs.deepseek.com/api/create-chat-completion/)、[MiniMax OpenAI 兼容接口](https://platform.minimax.io/docs/api-reference/text-openai-api)。

每个场景独立创建根预算，最多 12 次模型请求、8 次工具调用、每次生成上限 2048 Token、120 秒运行时限，不配置自动重试。每个场景结束立即保存 `results.json`；失败保留已知用量并退出，不自动重跑，也不覆盖已有报告目录。模型可能未遵循工具指令或在生成上限内未完成，这些情况均算验证失败，不能当作框架统计成功的证据。

校验包括：

- 供应商报告基本 usage 完整、总量大于零；没有完整 usage 时失败。
- 钩子逐尝试用量之和 = 各 Agent Self 之和 = 任务 Root。
- 钩子尝试键唯一、已记账，通知数 = 模型请求预留数。
- 父 Agent Inclusive = 任务 Root，子 Agent 父调用键正确。
- 完整任务确实执行两个子 Agent 与一次普通工具，不能仅生成文字后判成功。

报告仅保存模型标识、时间、校验结果、账本归属、逐尝试 usage 元数据和工具执行计数；不保存凭据、原始对话或供应商错误正文；工具参数错误可保存经密钥过滤的校验提示，用于定位案例定义问题。失败时该报告是已有消费的证据，不代表完整任务已完成。供应商后台最终账单需另行核对。

常规测试不联网：

```sh
go test -race ./agent-usage/...
```

其中 live 包测试使用离线模型，仅验证测试入口本身的校验和缺少凭据时的拒绝行为；不能替代上述真实网络执行。

## 真实执行记录

执行 `source ~/.zshrc` 更新配置后的 [Gemini 复测](https://github.com/kart-io/goagent/blob/master/docs/evaluation/2026-11-01-gemini-token-usage-retest/REPORT.md)三个场景全部通过：单 Agent 44、父子协作 256、完整任务 1273 TotalTokens。数值来自当次真实响应，后续运行可能不同。原始额度不足记录保留，不能作为成功证据。

三平台 [真实消耗比较](https://github.com/kart-io/goagent/blob/master/docs/evaluation/2026-11-01-platform-token-comparison/REPORT.md)覆盖九个成功场景。各平台的钩子、角色和任务总量均一致；相同任务的实际 Token 数量不同。lookup_order 使用显式 order_id 参数以减少模型对空参数对象的歧义，仍保留严格 schema 校验。

## 显式推理设置和明细

```sh
# Gemini：每次模型请求的思考预算，不是整个任务的总预算
go run ./agent-usage/live -provider gemini -model gemini-2.5-flash -scenario task -thinking-budget 128 -out /tmp/new-gemini-details
# OpenAI 兼容 / DeepSeek：effort；模型必须支持
MINIMAX_BASE_URL=https://api.minimax.cn/v1 go run ./agent-usage/live -provider minimax -model MiniMax-M3.1-Flash -scenario task -reasoning-effort low -out /tmp/new-minimax-details
```

不传参数时不猜测供应商默认推理级别。报告和通知记录显式设置与实际响应模型；控制是请求意图，不保证不同模型采用相同思考策略。输出中的 reasoning_known、tool_input_known 区分可用明细与未知；只对已知字段解释用量，不把零值直接当作没有消费。

思考/工具输入字段独立累计，TotalTokens 始终取供应商值；不会把细项再次加进总量。流和失败路径保留已知证据，持久回放仍使用原始模型名及配置。
