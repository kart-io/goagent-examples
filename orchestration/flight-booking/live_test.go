package main

import (
	"context"
	"testing"

	"github.com/kart-io/goagent-examples/internal/livemodel/livemodeltest"
	"github.com/stretchr/testify/require"
)

func TestLiveExtractionAndComparison(t *testing.T) {
	client := livemodeltest.Require(t)
	c := testConfig(t)
	c.Client = client
	cmd := command("message")
	cmd.Message = "2030年1月10日从新加坡SIN到上海PVG，单程经济舱，一位成人，预算5000元"
	r, err := Run(context.Background(), c, cmd)
	require.NoError(t, err)
	require.Equal(t, "awaiting_selection", r.Status)
	require.True(t, r.LiveModel)
	require.Zero(t, r.Effects.Payments)
	cmd = command("compare")
	r, err = Run(context.Background(), c, cmd)
	require.NoError(t, err)
	require.Equal(t, 1, r.ReadOnlyTools)
	require.Equal(t, "awaiting_selection", r.Status)
	require.Positive(t, r.ModelCalls)
	require.Zero(t, r.Effects.Payments)
	t.Logf("provider=%s model=%s status=%s usage=%+v", client.Live.Provider, client.Live.Model, r.Status, r.Usage)
}
