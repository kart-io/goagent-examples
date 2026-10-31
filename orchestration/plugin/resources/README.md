# 插件：资源与依赖

`inventory` 在 `Start` 中打开库存并以 `inventory.store` 发布；`pricing` 声明依赖 `inventory`，在 `Start` 与 `Contribute` 中用 `plugin.Lookup` 取出；`audit-log` 依赖 `pricing`。插件按相反顺序注册，Host 按依赖启动、逆序停止。`audit-log` 启动失败时，已启动的 `pricing`、`inventory` 被逆序停止，库存连接关闭。

```bash
go run ./orchestration/plugin/resources
go test ./orchestration/plugin/resources
```
