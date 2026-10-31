package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kart-io/goagent-contrib/httpagent"
	"github.com/kart-io/goagent-examples/orchestration/httpagent/internal/scenarios"
	"github.com/stretchr/testify/require"
)

func TestExecutableCasesUseToolsHooksAndOutboundWebhook(t *testing.T) {
	var mu sync.Mutex
	var notices []httpagent.RunEvent
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e httpagent.RunEvent
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		mu.Lock()
		notices = append(notices, e)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer callback.Close()
	t.Setenv("WEBHOOK_URL", callback.URL)
	app, err := newApp(&demoModel{})
	require.NoError(t, err)
	defer func() { require.NoError(t, app.Close(context.Background())) }()
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	var ids []string
	for _, kind := range []string{"invoke", "sse", "ws", "webhook", "ws-cancel"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := scenarios.Run(ctx, server.Client(), server.URL, kind, "案例订单 E100")
			require.NoError(t, err)
			require.NotEmpty(t, result.RunID)
			ids = append(ids, result.RunID)
			if kind == "ws-cancel" {
				require.Equal(t, "cancelled", result.Status)
			} else {
				require.Equal(t, "completed", result.Status)
				require.Contains(t, result.Result, "案例订单 E100")
			}
		})
	}
	require.NoError(t, app.Close(context.Background()))
	require.EqualValues(t, 5, app.Calls.Load())
	require.GreaterOrEqual(t, app.Hooks.Load(), int32(9))
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, notices, 5)
	for _, id := range ids {
		count := 0
		for _, e := range notices {
			if e.RunID == id {
				count++
				require.NotEmpty(t, e.EventID)
			}
		}
		require.Equal(t, 1, count, "one terminal callback per run")
	}
}

func TestCaseClientRejectsIncompleteSSEAndHTTPFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"missing terminal", "event: text\ndata: {\"type\":\"text\",\"run_id\":\"one\",\"delta\":\"partial\"}\n\n", 200},
		{"server rejection", `{"error":{"code":"capacity"}}`, 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			_, err := scenarios.Run(context.Background(), s.Client(), s.URL, "sse", "task")
			require.Error(t, err)
		})
	}
}

func TestCaseClientRejectsReadyWithoutBusinessTerminal(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{Subprotocols: []string{"goagent.v1"}}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _, err = c.ReadMessage()
		if err != nil {
			return
		}
		_ = c.WriteJSON(map[string]string{"type": "accepted", "run_id": "one"})
		_ = c.WriteJSON(map[string]string{"type": "ready"})
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := scenarios.Run(ctx, s.Client(), s.URL, "ws", "task")
	require.ErrorContains(t, err, "ready before terminal")
}
