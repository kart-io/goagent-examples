# 推送前的远端依赖验证

日期：2026-11-01。本次同步保留了远端 main 的 21 个提交（Plugin、ACP、HTTP Agent 等），并合入本地 Token 案例、身份修复及运行证据。原先仅工作区可用的条件已解除：根 go.mod 固定到已推送、已合入 GoAgent master 的提交 da848ae4f5e23460761c95d407483b9e9f2d7ec1，对应伪版本 v0.32.3-0.20261101061326-da848ae4f5e2；没有创建发布标签或提交本地 replace。

以下验证均退出码 0，明确设置 GOWORK=off，不依赖同级 GoAgent 工作树：

- 根模块 go test -race ./...，包括新 Token 案例及合并后的 Plugin、ACP 案例，见 [root-race.log](root-race.log)。
- 根模块 go build ./...，全部根模块入口编译通过。
- 独立 HTTP Agent 嵌套模块 go test -race ./...，见 [httpagent-race.log](httpagent-race.log)。该模块保留其原有固定依赖。
- HTTP Agent 的 node --test web/*.test.mjs，6 项协议测试通过，见 [web-protocol.log](web-protocol.log)。

本轮没有重复执行付费模型；此前 [28 入口真实运行记录](../2026-11-01-all-examples-run/REPORT.md)是同步远端新增案例之前的明确范围。新同步的 Plugin、ACP、HTTP Agent 在此记录的是离线回归，不冒充新增真实模型或编辑器联调结果。

此前迁移报告中的“尚未发布、需本地工作区”是当时状态；当前安装以根 README、agent-usage/README.md 和固定依赖为准。GoAgent 本次以快进方式合并到 master，没有覆盖远端提交。无关 Sandbox 调研留在核心工作区，未纳入此次提交。

当前文件摘要见 [checksums.json](checksums.json)。
