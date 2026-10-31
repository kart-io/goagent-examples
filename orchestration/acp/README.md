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
    "goagent": { "command": "/tmp/goagent-acp", "env": { "GOAGENT_LIVE": "1", "DEEPSEEK_API_KEY": "..." } }
  }
}
```

不设 `GOAGENT_LIVE` 时使用离线脚本模型，只会按固定步骤读 README 并执行 `ls`。
