package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kart-io/goagent/constants"
	"github.com/kart-io/goagent/core/session"
	agentErrors "github.com/kart-io/goagent/errors"
	mg "github.com/kart-io/goagent/management"
	a2aserver "github.com/kart-io/goagent/management/a2a"
	"github.com/kart-io/goagent/management/httpapi"
	"github.com/kart-io/goagent/management/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var demoActor = mg.Actor{TenantID: "demo", Subject: "local-user"}

type actorKey struct{}

type application struct {
	URL                  string
	AgentID              string
	service              *mg.Service
	worker               *mg.Worker
	http                 *http.Server
	cancel               context.CancelFunc
	workerDone, httpDone chan struct{}
	workerErr, httpErr   error
}

// 应用持有唯一 Service/Store/Worker，三个入口不自行创建执行器或任务库。
func startApplication(ctx context.Context, address, token string, models ...*modelBackend) (_ *application, err error) {
	var model *modelBackend
	if len(models) > 1 {
		return nil, fmt.Errorf("只允许一个模型后端")
	}
	if len(models) == 1 {
		model = models[0]
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return nil, fmt.Errorf("演示监听地址必须是回环 IP:端口")
	}
	if token == "" {
		return nil, fmt.Errorf("DEMO_TOKEN 不能为空")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = listener.Close()
		}
	}()
	sessions := session.NewMemoryStore()
	svc, err := mg.New(mg.Config{Store: newDemoStore(), Sessions: sessions, HistoryReader: historyReader{sessions}, Authorizer: mg.TenantAuthorizer{}, Factories: map[string]mg.Factory{"review": reviewFactory(model)}})
	if err != nil {
		return nil, err
	}
	agent, err := svc.RegisterAgent(ctx, demoActor, mg.RegisterAgentRequest{IdempotencyKey: "demo-agent", Name: "协议发布审核示例"})
	if err != nil {
		return nil, err
	}
	_, err = svc.SaveVersion(ctx, demoActor, mg.SaveVersionRequest{IdempotencyKey: "demo-version", AgentID: agent.ID, Version: "v1", RunnerKind: "review", ImplementationVersion: "protocol-demo-v1", Config: []byte(`{}`)})
	if err != nil {
		return nil, err
	}
	_, err = svc.PublishVersion(ctx, demoActor, mg.PublishRequest{IdempotencyKey: "demo-publish", AgentID: agent.ID, Version: "v1", ExpectedRevision: agent.Revision})
	if err != nil {
		return nil, err
	}
	url := "http://" + listener.Addr().String()
	actor := func(r *http.Request) (mg.Actor, error) {
		a, _ := r.Context().Value(actorKey{}).(mg.Actor)
		return a, mg.ValidateActor(a)
	}
	ah, err := a2aserver.New(svc, a2aserver.Config{AgentID: agent.ID, URL: url + "/a2a", Description: "演示发布说明、审批和可取消任务", ActorFromRequest: actor, PollInterval: 10 * time.Millisecond})
	if err != nil {
		return nil, err
	}
	mh, err := mcpserver.NewHTTPHandler(svc, mcpserver.Config{Agents: []mcpserver.Agent{{ID: agent.ID, Name: "review_agent", Description: "合成发布审核；input含scenario和message"}}, PollInterval: 10 * time.Millisecond, ActorFromRequest: func(ctx context.Context, _ *mcp.CallToolRequest) (mg.Actor, error) {
		a, _ := ctx.Value(actorKey{}).(mg.Actor)
		return a, mg.ValidateActor(a)
	}})
	if err != nil {
		return nil, err
	}
	hh, err := httpapi.New(svc, httpapi.Config{ActorFromRequest: actor})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/.well-known/agent-card.json", ah)
	mux.Handle("/a2a", ah)
	mux.Handle("/mcp", mh)
	mux.Handle("/v1/", hh)
	authenticated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "需要正确的演示令牌", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, demoActor)))
	})
	runCtx, cancel := context.WithCancel(context.Background())
	a := &application{URL: url, AgentID: agent.ID, service: svc, worker: mg.NewWorker(svc), http: &http.Server{Handler: authenticated, ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return runCtx }}, cancel: cancel, workerDone: make(chan struct{}), httpDone: make(chan struct{})}
	go func() { defer close(a.workerDone); a.workerErr = a.worker.Run(runCtx) }()
	go func() {
		defer close(a.httpDone)
		a.httpErr = a.http.Serve(listener)
		if errors.Is(a.httpErr, http.ErrServerClosed) {
			a.httpErr = nil
		}
	}()
	return a, nil
}

func (a *application) Close(ctx context.Context) error {
	// 先取消生产者与在途请求，避免长 task_wait 阻挡 HTTP 关闭。
	a.worker.BeginShutdown()
	a.cancel()
	httpErr := a.http.Shutdown(ctx)
	if httpErr != nil {
		_ = a.http.Close()
	}
	workerErr := a.worker.Shutdown(ctx)
	a.cancel()
	select {
	case <-a.workerDone:
	case <-ctx.Done():
		return errors.Join(httpErr, workerErr, ctx.Err())
	}
	<-a.httpDone
	return errors.Join(httpErr, workerErr, a.workerErr, a.httpErr)
}

type historyReader struct{ sessions session.Store }

func (h historyReader) History(ctx context.Context, id string, after int64, limit int, types []string) (mg.HistoryPage, error) {
	events, err := h.sessions.Read(ctx, id, after)
	if err != nil {
		return mg.HistoryPage{}, err
	}
	if limit < 1 {
		return mg.HistoryPage{}, agentErrors.ErrManagementInvalid
	}
	p := mg.HistoryPage{Items: []session.Event{}, NextSeq: after}
	for _, ev := range events {
		if strings.HasPrefix(ev.Type, "management/") || ev.Type == constants.SessionEventBoundarySnapshot {
			continue
		}
		if len(types) > 0 && !slices.Contains(types, ev.Type) {
			continue
		}
		p.Items = append(p.Items, ev)
		p.NextSeq = ev.Seq
		if len(p.Items) == limit {
			break
		}
	}
	return p, nil
}
