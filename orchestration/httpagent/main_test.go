package main

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWebExampleInvokesRealPluginToolOnBothTransports(t *testing.T) {
	app, err := newApp(&demoModel{})
	require.NoError(t, err)
	defer app.Close(context.Background())
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	for _, path := range []string{"/api/invoke", "/api/stream"} {
		resp, err := server.Client().Post(server.URL+path, "application/json", strings.NewReader(`{"input":"订单A1"}`))
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode, string(body))
		require.Contains(t, string(body), "订单A1")
	}
	require.EqualValues(t, 2, app.Calls.Load())
	resp, err := server.Client().Get(server.URL)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Contains(t, string(body), "GoAgent Playground")
}
func TestInlineWebhookUsesPlugin(t *testing.T) {
	app, err := newApp(&demoModel{})
	require.NoError(t, err)
	defer app.Close(context.Background())
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	resp, err := server.Client().Post(server.URL+"/api/webhooks/demo", "application/json", strings.NewReader(`{"message":"来自Webhook"}`))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	require.Contains(t, string(body), "来自Webhook")
	require.EqualValues(t, 1, app.Calls.Load())
}
