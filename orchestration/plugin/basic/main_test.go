package main

import (
	"context"
	"testing"

	"github.com/kart-io/goagent-examples/internal/livemodel/livemodeltest"
	"github.com/stretchr/testify/require"
)

func TestBasicComposesPlugins(t *testing.T) {
	out, err := run(context.Background(), demoModel())
	require.NoError(t, err)
	require.Equal(t, []string{"orders@1.0.0", "shipping@1.0.0"}, out.Plugins)
	require.EqualValues(t, 1, out.OrderLookups)
	require.Contains(t, out.Answer, "A100")
	require.Contains(t, out.Answer, "上海")
}

func TestBasicConflict(t *testing.T) {
	err := conflict(context.Background())
	require.ErrorContains(t, err, `插件 "orders" 与 "orders-copy" 提供了同名工具 "lookup_order"`)
}

func TestBasicLive(t *testing.T) {
	client := livemodeltest.Require(t)
	out, err := run(context.Background(), client)
	require.NoError(t, err)
	require.GreaterOrEqual(t, out.OrderLookups, int32(1), "真实模型应调用订单工具")
}
