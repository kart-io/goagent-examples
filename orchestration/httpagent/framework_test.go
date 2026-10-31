package main

import (
	"context"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kart-io/goagent-examples/orchestration/httpagent/internal/hostws"
	"github.com/kart-io/goagent-examples/orchestration/httpagent/internal/scenarios"
	"github.com/stretchr/testify/require"
)

func TestHostOwnedUpgradeExecutesPluginAndCancels(t *testing.T) {
	app, err := newApp(&demoModel{})
	require.NoError(t, err)
	defer func() { require.NoError(t, app.Close(context.Background())) }()
	s := httptest.NewServer(hostws.NewHandler(app.Core))
	defer s.Close()
	for _, kind := range []string{"ws", "ws-cancel"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result, err := scenarios.Run(ctx, s.Client(), s.URL, kind, "宿主升级订单")
		cancel()
		require.NoError(t, err)
		if kind == "ws" {
			require.Equal(t, "completed", result.Status)
			require.Contains(t, result.Result, "宿主升级订单")
		} else {
			require.Equal(t, "cancelled", result.Status)
		}
	}
	require.EqualValues(t, 2, app.Calls.Load())
	require.GreaterOrEqual(t, app.Hooks.Load(), int32(3))
}

func TestHostOwnedUpgradeKeepsHandshakePolicy(t *testing.T) {
	app, err := newApp(&demoModel{})
	require.NoError(t, err)
	defer func() { require.NoError(t, app.Close(context.Background())) }()
	s := httptest.NewServer(hostws.NewHandler(app.Core))
	defer s.Close()
	for _, tc := range []struct {
		protocols []string
		origin    string
		status    int
	}{
		{[]string{"goagent.v1"}, "https://other.test", http.StatusForbidden},
		{[]string{"other"}, "", http.StatusUpgradeRequired},
	} {
		dialer := websocket.Dialer{Subprotocols: tc.protocols}
		headers := http.Header{}
		if tc.origin != "" {
			headers.Set("Origin", tc.origin)
		}
		conn, resp, err := dialer.Dial("ws"+strings.TrimPrefix(s.URL, "http")+"/api/ws", headers)
		if conn != nil {
			_ = conn.Close()
		}
		require.Error(t, err)
		require.NotNil(t, resp)
		require.Equal(t, tc.status, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	}
	require.Zero(t, app.Calls.Load())
}
