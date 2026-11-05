package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent-examples/internal/livemodel/livemodeltest"
)

// TestLiveRefundTriage 用真实模型跑一次退款分流，只在 GOAGENT_LIVE=1 时运行，会产生费用；业务仍为模拟。
func TestLiveRefundTriage(t *testing.T) {
	client := livemodeltest.Require(t)
	e := newHostEnv(t, func(c *HostConfig) { c.Client, c.LockTimeout = client, 10*time.Minute })
	before := client.Calls()
	r, err := e.host.Message(context.Background(), "c-1001", "agent-1", "live-1", "订单 A1001 申请退款 30 元")
	evidence, _ := json.Marshal(map[string]any{"provider": client.Live.Provider, "model": client.Live.Model, "status": r.Status,
		"network_calls": client.Calls() - before, "refunds": e.refunds("A1001"), "error": errString(err)})
	t.Log("LIVE_EVIDENCE " + string(evidence))
	require.NoError(t, err)
	require.Contains(t, []string{statusDelivered, statusWaitingReview, statusWaitingCustomer}, r.Status)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
