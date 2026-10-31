package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

// editor 模拟编辑器：批准每次审批，记录通知。SDK 在独立 goroutine 回调，需加锁。
type editor struct {
	mu          sync.Mutex
	updates     []acpsdk.SessionNotification
	permissions int
}

func (e *editor) SessionUpdate(_ context.Context, n acpsdk.SessionNotification) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.updates = append(e.updates, n)
	return nil
}
func (e *editor) RequestPermission(context.Context, acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.permissions++
	return acpsdk.RequestPermissionResponse{Outcome: acpsdk.NewRequestPermissionOutcomeSelected("allow_once")}, nil
}
func (e *editor) ReadTextFile(context.Context, acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error) {
	return acpsdk.ReadTextFileResponse{}, acpsdk.NewMethodNotFound("fs/read_text_file")
}
func (e *editor) WriteTextFile(context.Context, acpsdk.WriteTextFileRequest) (acpsdk.WriteTextFileResponse, error) {
	return acpsdk.WriteTextFileResponse{}, acpsdk.NewMethodNotFound("fs/write_text_file")
}
func (e *editor) CreateTerminal(context.Context, acpsdk.CreateTerminalRequest) (acpsdk.CreateTerminalResponse, error) {
	return acpsdk.CreateTerminalResponse{}, acpsdk.NewMethodNotFound("terminal/create")
}
func (e *editor) KillTerminal(context.Context, acpsdk.KillTerminalRequest) (acpsdk.KillTerminalResponse, error) {
	return acpsdk.KillTerminalResponse{}, nil
}
func (e *editor) TerminalOutput(context.Context, acpsdk.TerminalOutputRequest) (acpsdk.TerminalOutputResponse, error) {
	return acpsdk.TerminalOutputResponse{}, nil
}
func (e *editor) ReleaseTerminal(context.Context, acpsdk.ReleaseTerminalRequest) (acpsdk.ReleaseTerminalResponse, error) {
	return acpsdk.ReleaseTerminalResponse{}, nil
}
func (e *editor) WaitForTerminalExit(context.Context, acpsdk.WaitForTerminalExitRequest) (acpsdk.WaitForTerminalExitResponse, error) {
	return acpsdk.WaitForTerminalExitResponse{}, nil
}

func TestACPAgentReadsFileAndRunsCommandWithApproval(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# 演示项目"), 0o644))

	agentIn, editorOut := io.Pipe()
	editorIn, agentOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- newServer(demoModel()).Serve(ctx, agentIn, agentOut) }()
	t.Cleanup(func() { _ = editorOut.Close(); _ = agentOut.Close(); cancel(); <-done })

	ed := &editor{}
	c := acpsdk.NewClientSideConnection(ed, editorOut, editorIn)
	_, err := c.Initialize(ctx, acpsdk.InitializeRequest{ProtocolVersion: acpsdk.ProtocolVersionNumber})
	require.NoError(t, err)
	s, err := c.NewSession(ctx, acpsdk.NewSessionRequest{Cwd: dir, McpServers: []acpsdk.McpServer{}})
	require.NoError(t, err)
	resp, err := c.Prompt(ctx, acpsdk.PromptRequest{SessionId: s.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("介绍一下这个项目")}})
	require.NoError(t, err)
	require.Equal(t, acpsdk.StopReasonEndTurn, resp.StopReason)

	ed.mu.Lock()
	defer ed.mu.Unlock()
	require.Equal(t, 1, ed.permissions, "执行命令前请求审批，读文件不需要")

	var answer string
	var finished int
	for _, n := range ed.updates {
		if m := n.Update.AgentMessageChunk; m != nil && m.Content.Text != nil {
			answer += m.Content.Text.Text
		}
		if u := n.Update.ToolCallUpdate; u != nil && u.Status != nil && *u.Status == acpsdk.ToolCallStatusCompleted {
			finished++
		}
	}
	require.Equal(t, 2, finished)
	require.Contains(t, answer, "演示项目")
}
