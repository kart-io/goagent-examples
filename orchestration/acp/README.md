# ACP Agent

以 [Agent Client Protocol](https://agentclientprotocol.com) 经 stdio 运行的 goagent Agent，基于 [goagent-contrib/acp](https://github.com/kart-io/goagent-contrib/tree/master/acp)。提供 `read_file` 与 `run_command` 两个工具：读文件直接执行，执行命令前经编辑器审批，可选"本会话允许"。编辑器支持时文件经编辑器读取、命令在编辑器终端中运行。

```bash
go build -o /tmp/goagent-acp ./orchestration/acp
go test ./orchestration/acp/   # 离线：模拟编辑器驱动一轮对话
```

Zed 的 `settings.json`：

```json
{
  "agent_servers": {
    "goagent": {
      "type": "custom",
      "command": "/tmp/goagent-acp",
      "args": [],
      "env": { "GOAGENT_LIVE": "1", "DEEPSEEK_API_KEY": "..." }
    }
  }
}
```

不设 `GOAGENT_LIVE` 时使用离线脚本模型，只会按固定步骤读 README 并执行 `ls`。

## 注意

- 示例中的"本会话允许"会放行本会话内的全部命令；实际使用应按命令前缀等收窄 Grant。


## ACP 契约修复依赖

案例通过 `WithTerminalCapabilities` 明确声明本地编辑器终端可访问网络；远程、容器或无网络部署须按实际环境调整。`go.mod` 固定到已推送的 ACP 修复提交 `b9d39c785211`（伪版本 `v0.1.2-0.20261031134247-b9d39c785211`），不依赖本地 `replace` 或 `go.work`，也不表示已发布 v0.1.2。

```sh
GOWORK=off go test -race -count=1 ./orchestration/acp/...
GOWORK=off go build ./orchestration/acp/...
```

离线测试覆盖本地执行与编辑器终端转发两条路径，不能替代真实编辑器联调。后续发布正式版本时再更新依赖。
