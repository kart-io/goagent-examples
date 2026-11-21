// Command ka 是团队知识助手的唯一入口：迁移、采集文档、提供服务与运维回收 Worker 槽位。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kart-io/goagent-contrib/retrieval"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/llm"
	mg "github.com/kart-io/goagent/management"

	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/assistant"
	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/chat"
	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/config"
	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/docs"
	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/embed"
	"github.com/kart-io/goagent-examples/products/knowledge-assistant/internal/web"
)

const usage = "用法：ka migrate | ingest | serve | recover-worker -reason <文本> -confirm-stopped"

var commands = map[string]func(context.Context, config.Config, []string) error{
	"migrate":        migrate,
	"ingest":         ingest,
	"serve":          serve,
	"recover-worker": recoverWorker,
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || commands[args[0]] == nil {
		return errors.New(usage)
	}
	c, err := config.FromEnv()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return commands[args[0]](ctx, c, args[1:])
}

func migrate(ctx context.Context, c config.Config, _ []string) error {
	pool, err := pgxpool.New(ctx, c.PGURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	return assistant.Migrate(ctx, pool)
}

func newEmbedder(ctx context.Context, c config.Config) (retrieval.Embedder, error) {
	if c.Embedder == "hash" {
		return embed.NewHash(3072), nil
	}
	return embed.NewGemini(ctx, c.EmbedParallel)
}

func newModel(c config.Config) (llm.ToolStreamClient, error) {
	if c.Model == "script" {
		return chat.NewScript(), nil
	}
	return chat.FromEnv()
}

// ingest 以 docs.ContentKey 为幂等键：语料未变时重复执行不重写，换嵌入模型则必须重建。
func ingest(ctx context.Context, c config.Config, _ []string) error {
	e, err := newEmbedder(ctx, c)
	if err != nil {
		return err
	}
	chunks, err := docs.Collect(c.CorpusDir)
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, c.PGURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	idx, err := docs.NewIndex(ctx, c.QdrantAddr, c.DocsAlias, e, pool)
	if err != nil {
		return err
	}
	defer func() { _ = idx.Close() }()
	name, err := idx.Rebuild(ctx, docs.ContentKey(embed.ModelID(e), chunks), chunks)
	if err != nil {
		return err
	}
	fmt.Printf("已索引 %d 个文本块，%s 指向 %s\n", len(chunks), c.DocsAlias, name)
	return nil
}

// app 是 serve 与 recover-worker 共用的进程装配。
type app struct {
	stores *assistant.Stores
	index  *docs.Index
	svc    *mg.Service
}

func open(ctx context.Context, c config.Config, confirm func(context.Context, string, int64) error) (_ *app, err error) {
	e, err := newEmbedder(ctx, c)
	if err != nil {
		return nil, err
	}
	model, err := newModel(c)
	if err != nil {
		return nil, err
	}
	a := &app{}
	defer func() {
		if err != nil {
			err = errors.Join(err, a.close(context.WithoutCancel(ctx)))
		}
	}()
	if a.stores, err = assistant.Open(ctx, c, model, e); err != nil {
		return nil, err
	}
	if a.index, err = docs.NewIndex(ctx, c.QdrantAddr, c.DocsAlias, e, a.stores.Pool); err != nil {
		return nil, err
	}
	a.svc, err = mg.New(mg.Config{
		Store:                a.stores.Manage,
		Sessions:             a.stores.Sessions,
		HistoryReader:        a.stores.Manage,
		Authorizer:           mg.TenantAuthorizer{},
		ConfirmWorkerStopped: confirm,
		// goagent 不再替宿主选限额，显式沿用原先的分页、接纳、大小与保留期，以及原先固定的任务预算。
		Limits:            mg.DefaultLimits(),
		DefaultTaskBudget: mg.TaskBudget{MaxModelRequests: 100, MaxToolCalls: 1000, MaxAgentCalls: 1000, Timeout: 24 * time.Hour},
		Factories: map[string]mg.Factory{assistant.RunnerKind: assistant.Factory(assistant.Deps{
			Stores: a.stores, Model: model, Index: a.index, CorpusDir: c.CorpusDir, Embedder: e,
		})},
		Report: func(stage string, err error) { fmt.Fprintln(os.Stderr, "管理诊断：", stage, err) },
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (a *app) close(ctx context.Context) error {
	var errs []error
	if a.index != nil {
		errs = append(errs, a.index.Close())
	}
	if a.stores != nil {
		errs = append(errs, a.stores.Close(ctx))
	}
	return errors.Join(errs...)
}

func serve(ctx context.Context, c config.Config, _ []string) (runErr error) {
	a, err := open(ctx, c, nil)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, a.close(context.Background())) }()
	h, err := web.NewHandler(a.svc, a.stores, a.index)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	// 请求 ctx 派生自 workCtx：关闭时一并取消仍在进行的长请求。
	workCtx, cancelWork := context.WithCancel(context.Background())
	server := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return workCtx },
	}
	httpDone := make(chan error, 1)
	go func() { httpDone <- server.Serve(listener) }()

	// 后台循环（管理 Worker、记忆抽取）任一提前退出都结束进程，交给编排系统重启。
	// 关闭时取消 workCtx，循环返回的 context.Canceled 属正常退出。
	background := make(chan error, 2)
	finish := func(err error) {
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		background <- err
	}
	running := 0
	var worker *mg.Worker
	if c.Worker {
		worker = mg.NewWorker(a.svc)
		running++
		go func() { finish(worker.Run(workCtx)) }()
	}
	// 只有真实模型才能抽取记忆；script 模式下 Stores 未配置 Model，改跑 RunMaintenance：
	// 框架要求不跑记忆 Worker 的部署必须运行它，否则 Purge 中断后的旧作用域、Forget 残留的向量与删除标记都无人清理。
	// 两个实例都跑：Maintain 不取锁、各步幂等，并发执行无害；live 下两个实例也都跑 Memory.Run，但由框架的
	// memory/worker 锁互斥。两者都不必跟着 management 的 Worker 槽位做人工故障转移。
	running++
	memoryLoop := "extract"
	if c.Model == "live" {
		go func() {
			finish(a.stores.Memory.Run(workCtx, func(err error) { fmt.Fprintln(os.Stderr, "记忆抽取：", err) }))
		}()
	} else {
		memoryLoop = "maintenance"
		go func() {
			finish(a.stores.Memory.RunMaintenance(workCtx, 0, func(err error) { fmt.Fprintln(os.Stderr, "记忆维护：", err) }))
		}()
	}
	fmt.Printf("团队知识助手已启动：http://%s（worker=%v，model=%s，memory=%s）\n", listener.Addr(), c.Worker, c.Model, memoryLoop)

	defer func() {
		if worker != nil {
			worker.BeginShutdown()
		}
		cancelWork()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			runErr = errors.Join(runErr, err, server.Close())
		}
		if worker != nil {
			runErr = errors.Join(runErr, worker.Shutdown(shutdown))
		}
		// 等后台循环全部返回，再由外层关闭它们使用的存储；卡住的循环不能让进程永远挂起。
		for ; running > 0; running-- {
			select {
			case err := <-background:
				runErr = errors.Join(runErr, err)
			case <-shutdown.Done():
				runErr = errors.Join(runErr, fmt.Errorf("等待 %d 个后台循环退出超时", running))
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil
	case err := <-background:
		running--
		if err == nil {
			err = errors.New("后台循环意外退出")
		}
		return err
	case err := <-httpDone:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// recoverWorker 释放已停止实例占着的 Worker 槽位。框架没有读取槽位持有者的接口（FINDINGS F2），
// 只能直接读管理存储里的 workers 文档。
func recoverWorker(ctx context.Context, c config.Config, args []string) error {
	fs := flag.NewFlagSet("recover-worker", flag.ContinueOnError)
	reason := fs.String("reason", "", "回收原因，写入审计")
	confirmed := fs.Bool("confirm-stopped", false, "运维确认旧 Worker 实例已停止")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*reason) == "" {
		return errors.New("须用 -reason 写明回收原因（写入审计）")
	}
	confirm := func(context.Context, string, int64) error {
		if !*confirmed {
			return errors.New("须由运维确认旧实例已停止")
		}
		return nil
	}
	a, err := open(ctx, c, confirm)
	if err != nil {
		return err
	}
	defer func() { _ = a.close(context.Background()) }()
	doc, err := a.stores.Manage.Get(ctx, "workers", "__system", "default")
	if errors.Is(err, agentErrors.ErrManagementNotFound) {
		fmt.Println("Worker 槽位从未被认领，无需回收")
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取 Worker 槽位：%w", err)
	}
	var owner mg.WorkerOwner
	if err := json.Unmarshal(doc.Data, &owner); err != nil {
		return fmt.Errorf("解析 Worker 槽位：%w", err)
	}
	if owner.OwnerID == "" {
		fmt.Println("Worker 槽位当前无人持有，无需回收")
		return nil
	}
	ops := mg.Actor{TenantID: "ops", Subject: "ops"}
	if err := a.svc.RecoverWorkerSlot(ctx, ops, owner.OwnerID, owner.Generation, *reason); err != nil {
		return err
	}
	fmt.Printf("已回收 Worker 槽位：持有者 %s，代次 %d\n", owner.OwnerID, owner.Generation)
	return nil
}
