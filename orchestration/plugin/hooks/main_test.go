package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHookPlugins(t *testing.T) {
	model, seen := demoModel()
	out, err := run(context.Background(), &flaky{ToolStreamClient: model, failures: 1})
	require.NoError(t, err)
	require.EqualValues(t, 1, out.Retries, "首个请求 503 后由限流重试插件重试")
	require.EqualValues(t, 1, out.Continued, "答案缺少订单号时续跑一次")
	require.GreaterOrEqual(t, out.Audited, int32(1))
	require.Contains(t, out.Answer, "A100")
	require.False(t, seen(), "模型从未看到完整手机号")
}
