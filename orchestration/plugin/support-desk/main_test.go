package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// 子进程在第二次模型请求前退出，模拟工具结果已记入日志后崩溃。
func TestSupportDeskCrashAndResume(t *testing.T) {
	if dir := os.Getenv("GOAGENT_PLUGIN_CRASH_DIR"); dir != "" {
		_, err := run(context.Background(), options{Dir: dir, RedactVersion: "1.0.0", CrashAfterFirstStep: true})
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSupportDeskCrashAndResume$")
	cmd.Env = append(os.Environ(), "GOAGENT_PLUGIN_CRASH_DIR="+dir)
	err := cmd.Run()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, crashCode, exit.ExitCode())

	out, err := run(context.Background(), options{Dir: dir, RedactVersion: "1.0.0"})
	require.NoError(t, err)
	require.Contains(t, out.Answer, "7 天")
	require.Zero(t, out.OrderLookups, "恢复时回放工具结果，不重复查询订单")
	require.EqualValues(t, 1, out.ModelCalls, "只补发崩溃前未完成的那次请求")

	_, err = run(context.Background(), options{Dir: dir, RedactVersion: "2.0.0"})
	require.ErrorContains(t, err, "operation intent or attempt policy changed", "脱敏插件升级后不能按旧日志恢复")
}
