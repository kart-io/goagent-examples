package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResourcesSharedAcrossPlugins(t *testing.T) {
	out, err := run(context.Background(), demoModel(), false)
	require.NoError(t, err)
	require.Contains(t, out.Answer, "库存 12")
	require.Contains(t, out.Answer, "单价 99")
	require.Equal(t, []string{"打开库存", "定价就绪", "定价停止", "关闭库存"}, out.Events)
	require.True(t, out.InventoryClosed)
}

func TestStartFailureRollsBack(t *testing.T) {
	out, err := run(context.Background(), demoModel(), true)
	require.ErrorContains(t, err, `插件 "audit-log" 启动失败`)
	require.Equal(t, []string{"打开库存", "定价就绪", "定价停止", "关闭库存"}, out.Events)
	require.True(t, out.InventoryClosed, "已打开的库存被回滚关闭")
}
