# 插件：客服端到端

`orders`、`refund-policy`、`redact`、`crash-probe` 四个插件组成持久运行的客服 Agent，插件工具经 `plugin.Dispatch` 记入日志。测试先让子进程在工具结果记入日志后、第二次模型请求前退出（请求已发出而结果未落盘会停在 `pending/ambiguous`，恢复不再重发，故崩溃点不能放在请求之内），再在同一日志目录恢复：已完成的模型回合与订单查询被回放，只补发崩溃前未完成的请求。脱敏插件升级为 2.0.0 后，PreStep 身份 `plugin/redact@2.0.0/mask-phone` 与日志不符，恢复以 `operation intent or attempt policy changed` 失败。

`crash-probe` 只用来让崩溃确定地落在工具结果记入日志之后，每次运行（含恢复）都必须注册：持久意图包含 Agent 上全部 PreStep 身份及其顺序，恢复时漏掉它本身就会以 `operation intent or attempt policy changed` 失败。

```bash
go run ./orchestration/plugin/support-desk /tmp/desk   # 第二次运行同一目录只回放
go test ./orchestration/plugin/support-desk
```
