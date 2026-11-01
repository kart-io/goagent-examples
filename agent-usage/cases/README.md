# 多平台、回放、报错与取消的 Token 统计案例

运行真实 Gemini、DeepSeek、MiniMax 请求，验证同一任务跨平台累计，以及失败和回放的计量边界。CLI 不会回退到模拟模型；常规测试注入离线响应，不访问外网。

```sh
source ~/.zshrc
MINIMAX_BASE_URL=https://api.minimax.cn/v1 go run ./agent-usage/cases \
  -case all -out /tmp/new-token-cases
```

也可分别运行：

```sh
go run ./agent-usage/cases -case tool-error -out /tmp/new-token-error
go run ./agent-usage/cases -case cancel -out /tmp/new-token-cancel
MINIMAX_BASE_URL=https://api.minimax.cn/v1 go run ./agent-usage/cases \
  -case mixed-replay -out /tmp/new-token-mixed
```

输出目录必须不存在。工具报错和取消只需 GEMINI_API_KEY；mixed-replay 需要三个平台的 API_KEY。模型可通过 GEMINI_MODEL、DEEPSEEK_MODEL、MINIMAX_MODEL 覆盖，默认 gemini-2.5-flash、deepseek-chat、MiniMax-M3.1-Flash；端点通过对应 BASE_URL 设置。默认 MiniMax 工厂使用国际站，国内凭据应显式选择匹配端点。

## 1. 混合平台任务与持久回放

```text
Gemini coordinator
  ├─ DeepSeek researcher → lookup_order 普通工具 → 返回状态
  └─ MiniMax reviewer → 返回审核结果
→ Gemini 汇总
→ 相同输入／作用域／持久日志再次 Invoke
```

每个 Agent 使用独立客户端，共享同一持久预算。父钩子继承到两个子 Agent；按可信的 Agent → 平台路由表汇总，不能仅凭 `Provider=openai` 判断一个兼容端点的厂商。

验证首轮确实使用三平台、两个子 Agent 和普通工具；逐尝试、平台、Agent、Root 的全部六个 Token 字段一致。第二轮要求成功、账本完全不变、客户端调用数不增加、普通工具仍只执行一次，且收到回放通知。回放通知不再次累加。

## 2. 普通工具主动报错

Gemini 先真实选择 lookup_order，工具再返回已知哨兵错误。只有 errors.Is 命中预期错误、工具执行一次且此前 usage 保留时，验证才通过。模型没有调用工具或返回了其它错误不能算通过。

## 3. 执行中取消

Gemini 先真实选择工具，工具内取消本次 context。验证业务返回 context.Canceled，随后使用脱离取消信号的查询 context 读取账本，核对先前模型消费仍存在。

**Passed 表示验证案例通过。tool-error 和 cancel 的业务执行仍然失败，是预期结果。**

## 输出与边界

每个案例结束写入 results.json，包含前后账本、逐尝试证据、平台汇总、客户端调用次数、工具次数及预期错误是否出现。日志不保存模型对话或推理正文。

每场景最多 10 次模型请求、8 次工具调用，单次输出上限 2048，运行时限 180 秒。Gemini 显式每请求思考预算 128，另外两平台使用 effort=low；这不代表三平台推理策略等价。不配置自动重试，失败证据保存后退出。临时持久日志由案例创建并清理；该例验证同进程持久回放，不宣称完成进程崩溃恢复验证。

调用计数统计进入结构化客户端的次数，所用 Provider 没有隐式重试；该指标不等同于供应商账单条目。可选细项仍需 KnownFields 判断已知性，不按零值猜测“没有推理”，不将子类别再加到 TotalTokens。

离线验证：

```sh
go test -race ./agent-usage/...
```

[真实执行报告](https://github.com/kart-io/goagent/blob/master/docs/evaluation/2026-11-01-usage-boundary-cases/REPORT.md)。
