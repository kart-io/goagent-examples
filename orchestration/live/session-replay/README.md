# 会话日志与重放：中断续写、历史推导与回归重放

客服 Agent 用 `lookup_order` 查订单物流，回答“结论：X”。`WithSessionLog` 把对话、轮次和持久操作的快照都记在同一份会话日志里（`session.FileStore`，持久日志用 `session.DurableStore` 指向同一会话）。订单数据是合成的。

| 轮次 | 用户输入 | 应得结论 |
| --- | --- | --- |
| 0 | 我的订单 A100 到哪了？ | 已发货 |
| 1 | 那 B200 呢？（依赖上一轮） | 待发货 |

```sh
dir=$(mktemp -d)
go run ./orchestration/live/session-replay -dir "$dir" -ask 0
go run ./orchestration/live/session-replay -dir "$dir" -ask 1
go run ./orchestration/live/session-replay -dir "$dir" -replay 0                         # 全部回放，0 次请求
go run ./orchestration/live/session-replay -dir "$dir" -replay 0 -live-tools -delivered  # 工具改动后实跑
go run ./orchestration/live/session-replay -dir "$dir" -replay 1 -cut-model 1            # 用当前模型重答
go run ./orchestration/live/session-replay -dir "$dir" -export trajectory.jsonl          # OpenAI 微调格式
```

测试的每一步：

1. 第一轮：工具查到结果后、检查点保存前进程退出，日志中留下未结束的轮次。
2. 新进程以同一执行键续写这一轮：第一次模型回合从日志回放，工具重跑一次，只请求模型给出答复。两个进程合计的请求数等于记录的模型回合数。
3. 第二轮在另一个进程：历史从会话日志推导，第一次请求就带着第一轮的全部对话。
4. 导出：两轮对话、一份工具定义；中断的那次运行没有留下重复消息，工具调用统计与实际完成的次数一致。
5. 重放（`testing/replay`），都不写原日志：
   - 全部回放：模型和工具都不调用，答复与记录相同。
   - `Live: replay.Kind("tool")`：工具实跑，结果不变时没有分歧，模型继续回放。
   - 同上，但工具数据改为 A100 已签收：在工具边界报告分歧（记录值“已发货”、实跑值“已签收”），之后的模型回合用当前模型实跑，结论变为“已签收”。
   - `Cut: replay.ModelTurn(1)`：第二轮的查询回放，只用当前模型重新作答。

同一套断言既用脚本化模型离线跑（`TestScriptedSessionReplay`），也用真实模型跑（`TestLiveSessionReplay`，约 6 次请求）。

重放要求 system 提示词、工具定义和会话 ID 与记录时一致；改了被回放边界的输入（如提示词）而没有切开时，重放直接报错，应从出错的边界切开。返回值带时间戳等每次都会变的工具，实跑时总会判为分歧，应保持回放。
