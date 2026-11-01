# 机票预订案例验证

日期：2026-11-01。仓库：goagent-examples，分支 `feat/flight-booking`。业务及离线模型均为合成数据，未调用真实模型或真实航司/支付服务。

## 交付内容

- `orchestration/flight-booking`：实际 GoAgent Builder、只读原生工具循环、DurableGraph、两次独立用户确认和本地供应商账本。
- 结构化请求绕过 Agent；自然语言路径先澄清及提取；只读比价不能授权交易。
- 单程、往返、合成成人/儿童；报价/锁座有效期、精确金额和规则绑定；修改增加修订，旧授权失效。
- 支付、出票、退款结果未知先核对原收据；副作用提交后进程退出可恢复。
- 主动退票/改签首轮明确转人工，不执行新效果；出票失败补偿退款已实现。
- Token 统计采用持久根预算和 usage hook；离线数字为合成传输用量，不是供应商账单。

## 验证结果

| 检查 | 结果 | 证据 |
| --- | --- | --- |
| 新案例离线 race | 通过，11.750s | [flight-race.log](flight-race.log) |
| 根模块全量 race，关闭 go.work 和付费测试开关 | 35 个包通过，7 个包无测试；新包在并行负载下 41.328s | [root-race.log](root-race.log) |
| 根模块 `go build ./...` | 退出码 0 | 本次执行记录；成功时无输出 |
| 编译后 CLI 场景冒烟 | 20 项全部通过 | [cli-smoke.json](cli-smoke.json) |
| 独立审阅 | 三项业务正确性问题均补充回归并修复 | 见下节 |

执行命令：

```sh
env -u GOROOT GOTOOLCHAIN=go1.25.0 GOWORK=off go test -race -count=1 ./orchestration/flight-booking
env -u GOROOT -u GOAGENT_LIVE -u GOAGENT_NATIVE_LIVE -u GOAGENT_BUSINESS_LIVE \
  GOTOOLCHAIN=go1.25.0 GOWORK=off go test -race -count=1 ./...
env -u GOROOT GOTOOLCHAIN=go1.25.0 GOWORK=off go build ./...
env -u GOROOT GOTOOLCHAIN=go1.25.0 GOWORK=off go build -o /tmp/goagent-flight-booking ./orchestration/flight-booking
python3 scripts/flight_booking_smoke.py /tmp/goagent-flight-booking
```

20 项 CLI 验证包括：结构化流程与只读 Agent 工具比价、两轮自然语言澄清、家庭往返、缺参、无票、价格/币种/规则变化、库存耗尽、确认过期、支付拒绝、支付/出票/退款结果未知、出票失败补偿、错误授权拒绝、修订与取消，以及支付/出票/退款提交后退出码 86 的新进程恢复。

Go 测试额外覆盖并发确认与取消、旧修订/错误身份/错误令牌、重复确认、同请求不同参数、低预算和非法日期/旅客、同日往返航段顺序、模型非法输出及未知字段、已经接受授权但图未推进前过期、已付款且新出票条件失效时的补偿。独立进程恢复测试将时钟推进一天：只有付款收据时核对后退款；已有票号或退款时返回原记录，不创建重复效果。

## 审阅发现及 RED/GREEN

1. **新出票没有检查锁座及报价条件**：支付未知但实际已成功后，改变行情仍能新出票。已在供应商模拟边界补校验；未出票且条件失效进入退款，已提交票号优先核对返回。回归覆盖过期、涨价、币种、规则和库存变化。[RED](ticket-conditions-red.log)
2. **同日返程时间早于去程抵达**：固定返程示例改为 18:00 出发、23:00 抵达，并用实际 RFC3339 时间验证航段先后。[RED](recovery-chronology-red.log)
3. **授权保存后、付款前过期会停在不可恢复错误**：引入明确的条件失效分支，转 requote_required、失效授权并释放锁座；不会反复报错或付款。[RED](recovery-chronology-red.log)
4. 自查发现 confirm 携带另一份 input 会被静默忽略，可能按旧行程付款；已在命令边界拒绝，必须通过 revise。[RED](replacement-input-red.log)

CLI 冒烟脚本首次运行暴露的是验证脚本使用了错误的 JSON 字段名 `TotalTokens`；对照公开 TokenUsage JSON 标签修正为 `total_tokens` 后，20 项完整重跑通过。该错误未涉及订票代码。

## 验证边界

默认真实模型测试 `TestLiveExtractionAndComparison` 跳过；本报告不声称已经验证 Gemini/DeepSeek/MiniMax 的自由语言质量。现有适配器和显式 `-live`/GOAGENT_LIVE 开关保留了真实验证入口。

根模块命令不递归测试独立的 `orchestration/httpagent` 嵌套模块；本次没有改动该模块或任何核心库。状态存储假设可信本地文件系统，未验证跨主机接管、真实身份认证、真实支付和真实航司协议。幂等效果依赖模拟供应商账本，不意味着任意外部 API 都恰好执行一次。

[源文件与证据校验和](checksums.json) 固定本次验证的实现和日志。


## 后续真实验证

同日已扩展真实模型测试，并完成 Gemini、DeepSeek、MiniMax 三平台验证；同时重跑离线根模块全量 race。首轮离线记录保留，新增结果见 [真实模型报告](../2026-11-01-flight-booking-live/REPORT.md)。当前代码的测试入口及 README 校验和随此次文档更新刷新，首轮生产代码没有变化。
