# 插件：基础与组合

订单插件提供 `lookup_order` 工具与使用说明，物流插件提供 `track_parcel`，两者经 `plugin.Install` 装进同一个 Agent。另一个插件也提供 `lookup_order` 时，`Install` 在写入 Builder 前报告冲突双方。

```bash
go run ./orchestration/plugin/basic                 # 离线模型
GOAGENT_LIVE=1 DEEPSEEK_API_KEY=... go run ./orchestration/plugin/basic
go test ./orchestration/plugin/basic
```
