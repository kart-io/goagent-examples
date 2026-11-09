package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	a2aremote "github.com/kart-io/goagent/a2a/remote"
	"github.com/kart-io/goagent/codec/json"
	mg "github.com/kart-io/goagent/management"
	"github.com/stretchr/testify/require"
)

func testApplication(t *testing.T) (context.Context, *application) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	a, err := startApplication(ctx, "127.0.0.1:0", "test-token")
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, a.Close(ctx))
	})
	return ctx, a
}

func TestDemoRunsAllCases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	require.NoError(t, run(ctx, []string{"demo", "-auto"}, strings.NewReader(""), &out))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var results []scenarioResult
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &results))
	require.Len(t, results, 4)
	ids := map[string]bool{}
	for _, r := range results {
		require.NotEmpty(t, r.TaskID)
		require.False(t, ids[r.TaskID])
		ids[r.TaskID] = true
		if r.Case == "cancel" {
			require.Equal(t, mg.Canceled, r.State)
		} else {
			require.Equal(t, mg.Succeeded, r.State)
		}
	}
	require.Contains(t, out.String(), "收到流式首事件")
}

func TestBothApprovalPathsHonorRejection(t *testing.T) {
	for _, name := range []string{"cross", "mcp"} {
		t.Run(name, func(t *testing.T) {
			ctx, a := testApplication(t)
			answers := &answers{scanner: bufio.NewScanner(strings.NewReader("先不要发布\nreject\n")), out: io.Discard}
			results, err := runCases(ctx, a.URL, "test-token", name, answers)
			require.NoError(t, err)
			require.Len(t, results, 1)
			var state reviewState
			require.NoError(t, json.Unmarshal(results[0].Result, &state))
			require.Equal(t, "reject", state.Decision)
			require.Equal(t, "用户拒绝，未执行模拟发布", state.Result)
		})
	}
}

func TestAuthenticationAndSeparateClientMode(t *testing.T) {
	ctx, a := testApplication(t)
	for _, path := range []string{"/.well-known/agent-card.json", "/mcp", "/v1/tasks"} {
		req, err := http.NewRequestWithContext(ctx, "GET", a.URL+path, nil)
		require.NoError(t, err)
		r, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		require.Equal(t, http.StatusUnauthorized, r.StatusCode)
	}
	http.DefaultClient.CloseIdleConnections()
	t.Setenv("DEMO_TOKEN", "wrong-token")
	require.Error(t, run(ctx, []string{"client", "-url", a.URL, "-case", "remote", "-auto"}, strings.NewReader(""), io.Discard))
	t.Setenv("DEMO_TOKEN", "test-token")
	var out bytes.Buffer
	require.NoError(t, run(ctx, []string{"client", "-url", a.URL, "-case", "remote", "-auto"}, strings.NewReader(""), &out))
	require.Contains(t, out.String(), "远端 Agent 已处理")
}

func TestInputWaitRespectsDeadline(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := run(ctx, []string{"demo", "-case", "cross", "-timeout", "300ms"}, reader, io.Discard)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 2*time.Second)
}

func TestBearerTokenDoesNotFollowForeignHost(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("不应访问其他主机") }))
	defer other.Close()
	client, err := authenticatedClient("http://127.0.0.1:1", "secret")
	require.NoError(t, err)
	req, err := http.NewRequest("GET", other.URL, nil)
	require.NoError(t, err)
	_, err = client.Do(req)
	require.ErrorContains(t, err, "拒绝向发现地址之外")
	require.Empty(t, req.Header.Get("Authorization"))
}

func TestInvalidCLIConfiguration(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "")
	for _, args := range [][]string{{"serve"}, {"client"}, {"unknown"}, {"demo", "-timeout", "0s"}, {"demo", "extra"}} {
		require.Error(t, run(context.Background(), args, strings.NewReader(""), io.Discard))
	}
	_, err := startApplication(context.Background(), "0.0.0.0:0", "token")
	require.Error(t, err)
}

func TestShutdownCancelsActiveRequestAndWorker(t *testing.T) {
	ctx, app := testApplication(t)
	httpClient, err := authenticatedClient(app.URL, "test-token")
	require.NoError(t, err)
	defer httpClient.CloseIdleConnections()
	client, err := a2aremote.Connect(ctx, app.URL, httpClient)
	require.NoError(t, err)
	defer client.Destroy()
	d := &demoClient{a2a: client}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = d.send(ctx, "cancel") }()
	require.Eventually(t, func() bool {
		page, err := app.service.ListTasks(ctx, demoActor, mg.Query{AgentID: app.AgentID})
		return err == nil && len(page.Items) == 1 && page.Items[0].State == mg.Running
	}, time.Second, 5*time.Millisecond)
	closing, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	require.NoError(t, app.Close(closing))
	select {
	case <-done:
	case <-closing.Done():
		t.Fatal("在途 HTTP 请求未随宿主关闭退出")
	}
	select {
	case <-app.workerDone:
	default:
		t.Fatal("执行器未退出")
	}
}
