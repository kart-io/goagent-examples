# 插件：循环钩子

| 插件 | 扩展点 | 作用 |
|---|---|---|
| `redact` | PreStep（身份 `plugin/redact@1.0.0/mask-phone`） | 模型请求前把手机号中间四位替换为星号 |
| `retry` | RequestError（`builder.RetryTransient`） | 上游 503 时重试，最多 2 次 |
| `verify` | TurnStopping | 答案缺少订单号时要求补充 |
| `audit` | 中间件 | 记录每次运行 |

离线模型的第一个请求返回 503、第一次作答缺少订单号，用来触发重试与续跑；实时模式（`GOAGENT_LIVE=1`）不注入 503，重试次数通常为 0，也不输出“模型见过完整手机号”。

```bash
go run ./orchestration/plugin/hooks
go test ./orchestration/plugin/hooks
```
