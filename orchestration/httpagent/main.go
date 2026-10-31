package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kart-io/goagent-contrib/httpagent"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/llm/constants"
	"github.com/kart-io/goagent/llm/providers"
	"github.com/kart-io/goagent/plugin"
	"github.com/kart-io/goagent/tools"
)

//go:embed web/index.html web/app.mjs web/sse.mjs web/protocol.mjs web/style.css
var assets embed.FS

type application struct {
	Handler http.Handler
	Core    *httpagent.Handler
	Host    *plugin.Host
	Calls   *atomic.Int32
}

func (a *application) Close(ctx context.Context) error {
	if err := a.Core.Shutdown(ctx); err != nil {
		return err
	}
	return a.Host.Stop(ctx)
}

type demoPlugin struct{ calls *atomic.Int32 }

func (*demoPlugin) Name() string    { return "http-demo" }
func (*demoPlugin) Version() string { return "1.0.0" }
func (p *demoPlugin) Contribute(context.Context, *plugin.Env) (plugin.Contribution, error) {
	echo := tools.NewBaseTool("echo_input", "处理演示输入", `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`, func(_ context.Context, in *interfaces.ToolInput) (*interfaces.ToolOutput, error) {
		p.calls.Add(1)
		return &interfaces.ToolOutput{Success: true, Result: "插件工具已处理：" + fmt.Sprint(in.Args["text"])}, nil
	})
	wait := tools.NewBaseTool("wait_operation", "演示可取消的慢任务", `{"type":"object","properties":{}}`, func(ctx context.Context, _ *interfaces.ToolInput) (*interfaces.ToolOutput, error) {
		p.calls.Add(1)
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return &interfaces.ToolOutput{Success: true, Result: "慢任务已完成"}, nil
		}
	})
	return plugin.Contribution{Tools: []interfaces.Tool{echo, wait}, PreSteps: []plugin.PreStep{{Identity: "demo-input", Hook: func(context.Context, *builder.Step) (builder.PreStepResult, error) {
		return builder.PreStepResult{}, nil
	}}}}, nil
}
func newApp(model llm.ToolStreamClient) (*application, error) {
	calls := &atomic.Int32{}
	host, err := plugin.NewHost(&demoPlugin{calls: calls})
	if err != nil {
		return nil, err
	}
	if err = host.Start(context.Background()); err != nil {
		return nil, err
	}
	cfg := httpagent.Config{
		Factory: func(ctx context.Context, req httpagent.RunRequest) (httpagent.Binding, httpagent.Cleanup, error) {
			if !llm.HasToolStreaming(model) {
				return httpagent.Binding{}, nil, &httpagent.Failure{Code: "unsupported_capability", Message: "本示例要求原生工具流模型"}
			}
			b := builder.NewSimpleBuilder(model).WithSessionID(req.ID).WithTemperature(0).WithMaxIterations(4).WithTimeout(10 * time.Minute)
			if err := plugin.Install(ctx, b, host, plugin.Dispatch(tools.DispatchConfig{})); err != nil {
				return httpagent.Binding{}, nil, err
			}
			agent, err := b.BuildContext(ctx)
			if err != nil {
				return httpagent.Binding{}, nil, err
			}
			return httpagent.Native(agent.ExecuteStream), nil, nil
		},
		Receivers: map[string]httpagent.Receiver{"demo": {Mode: "inline", Decode: func(_ context.Context, _ *http.Request, body []byte) (httpagent.Incoming, error) {
			var input struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(body, &input); err != nil {
				return httpagent.Incoming{}, err
			}
			if input.Message == "" {
				return httpagent.Incoming{}, errors.New("message 不能为空")
			}
			return httpagent.Incoming{Source: "demo", Input: input.Message}, nil
		}}},
		Observer: func(_ context.Context, e httpagent.RunEvent) error {
			log.Printf("事件 %s run_id=%s", e.EventType, e.RunID)
			return nil
		},
		Report: func(_ context.Context, r httpagent.Report) {
			log.Printf("运行报告 run_id=%s stage=%s error=%v", r.RunID, r.Stage, r.Err)
		},
	}
	if target := os.Getenv("WEBHOOK_URL"); target != "" {
		cfg.Delivery, err = httpagent.NewSender(httpagent.SenderConfig{Targets: []httpagent.Target{{URL: target}}})
		if err != nil {
			_ = host.Stop(context.Background())
			return nil, err
		}
	}
	h, err := httpagent.New(cfg)
	if err != nil {
		_ = host.Stop(context.Background())
		return nil, err
	}
	web, err := fs.Sub(assets, "web")
	if err != nil {
		_ = host.Stop(context.Background())
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", h))
	mux.Handle("/", http.FileServer(http.FS(web)))
	return &application{Handler: mux, Core: h, Host: host, Calls: calls}, nil
}
func main() {
	var model llm.ToolStreamClient = &demoModel{}
	if os.Getenv("GOAGENT_LIVE") == "1" {
		client, err := providers.NewOpenAIWithOptions(llm.WithAPIKey(os.Getenv("OPENAI_API_KEY")), llm.WithBaseURL(os.Getenv("OPENAI_BASE_URL")), llm.WithModel(os.Getenv("OPENAI_MODEL")), llm.WithTemperature(0))
		if err != nil {
			log.Fatal(err)
		}
		model = client
		log.Print("使用实际模型服务；请确认 API 地址、模型及费用配置")
	} else {
		log.Print("使用离线脚本模型；Plugin、工具、HTTP/SSE/WS 均真实执行")
	}
	app, err := newApp(model)
	if err != nil {
		log.Fatal(err)
	}
	addr := os.Getenv("GOAGENT_HTTP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8097"
	}
	server := &http.Server{Addr: addr, Handler: app.Handler, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { log.Printf("打开 http://%s", addr); serveErr <- server.ListenAndServe() }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err = <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Print(err)
		}
	}
	app.Core.BeginShutdown()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = errors.Join(server.Shutdown(shutdown), app.Close(shutdown))
	if err != nil {
		log.Fatal(err)
	}
}

// demoModel 仅脚本化模型响应；Agent、Plugin、工具和传输使用正式实现。
type demoModel struct{}

func (*demoModel) Provider() constants.Provider { return constants.Provider("offline-demo") }
func (*demoModel) IsAvailable() bool            { return true }
func (*demoModel) Complete(context.Context, *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	return &llm.CompletionResponse{Content: "离线演示"}, nil
}
func (m *demoModel) Chat(ctx context.Context, msg []llm.Message) (*llm.CompletionResponse, error) {
	return m.Complete(ctx, &llm.CompletionRequest{Messages: msg})
}
func (m *demoModel) ChatStream(ctx context.Context, msg []llm.Message) (<-chan *llm.StreamChunk, error) {
	return m.CompleteStream(ctx, &llm.CompletionRequest{Messages: msg})
}
func (m *demoModel) CompleteStream(ctx context.Context, r *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	return m.CompleteToolsStream(ctx, r, nil)
}
func (*demoModel) CompleteToolsStream(ctx context.Context, r *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	ch := make(chan *llm.StreamChunk)
	go func() {
		defer close(ch)
		send := func(c *llm.StreamChunk) bool {
			select {
			case ch <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}
		user, toolResult := "", ""
		for _, m := range r.Messages {
			if m.Role == "user" {
				user = m.Content
			}
			if m.Role == "tool" {
				toolResult = m.Content
			}
		}
		if toolResult == "" {
			name := "echo_input"
			args, _ := json.Marshal(map[string]string{"text": user})
			if strings.Contains(user, "慢任务") || strings.Contains(user, "slow") {
				name = "wait_operation"
				args = []byte(`{}`)
			}
			if !send(&llm.StreamChunk{Delta: "正在调用插件工具…"}) {
				return
			}
			call := llm.ToolCall{ID: "demo-call", Type: "function"}
			call.Function.Name = name
			call.Function.Arguments = string(args)
			send(&llm.StreamChunk{Done: true, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{call}})
			return
		}
		final := "执行结果：" + toolResult
		for _, r := range final {
			if !send(&llm.StreamChunk{Delta: string(r)}) {
				return
			}
			timer := time.NewTimer(4 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		send(&llm.StreamChunk{Done: true, FinishReason: "stop", Content: final})
	}()
	return ch, nil
}
