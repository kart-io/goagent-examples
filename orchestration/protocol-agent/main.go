package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/kart-io/goagent/codec/json"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, input io.Reader, output io.Writer) (err error) {
	command := "demo"
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}
	if command != "demo" && command != "serve" && command != "client" {
		return fmt.Errorf("用法：protocol-agent demo|serve|client；各命令可用 -h 查看参数")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(output)
	address := flags.String("listen", "127.0.0.1:8080", "服务监听地址（仅回环IP）")
	endpoint := flags.String("url", "http://127.0.0.1:8080", "客户端连接的服务根地址")
	selected := flags.String("case", "all", "cross、mcp、remote、cancel 或 all")
	auto := flags.Bool("auto", false, "用预设输入和批准运行合成案例")
	live := flags.Bool("live", false, "显式启用真实模型请求")
	provider := flags.String("provider", "deepseek", "真实模型提供方 deepseek 或 gemini")
	modelName := flags.String("model", "", "模型名，默认读取对应提供方环境变量")
	maxRequests := flags.Int64("max-model-requests", 3, "本进程模型请求硬上限（1到20）")
	timeout := flags.Duration("timeout", 2*time.Minute, "客户端案例总时限，包含输入等待")
	if err = flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("不接受额外位置参数")
	}
	if *timeout <= 0 {
		return fmt.Errorf("timeout 必须大于0")
	}
	var backend *modelBackend
	if *live {
		if command == "client" {
			return fmt.Errorf("-live 应设置在 demo 或 serve 端")
		}
		backend, err = newLiveModel(*provider, *modelName, *maxRequests)
		if err != nil {
			return err
		}
		defer func() {
			writeErr := json.Encode(output, map[string]any{"live_provider": backend.provider, "model": backend.model, "provider_invocations": backend.client.calls.Load(), "max_model_requests": *maxRequests})
			err = errors.Join(err, writeErr)
		}()
	}
	token := os.Getenv("DEMO_TOKEN")
	if command == "demo" {
		token = uuid.NewString()
	}
	if token == "" {
		return fmt.Errorf("serve/client 需要设置相同的 DEMO_TOKEN")
	}
	if command != "client" {
		listen := *address
		if command == "demo" {
			listen = "127.0.0.1:0"
		}
		app, startErr := startApplication(ctx, listen, token, backend)
		if startErr != nil {
			return startErr
		}
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = errors.Join(err, app.Close(closeCtx))
		}()
		*endpoint = app.URL
		if _, err = fmt.Fprintf(output, "服务已启动：%s；agent_id=%s；内存数据仅存于本次进程\n", app.URL, app.AgentID); err != nil {
			return err
		}
		if command == "serve" {
			select {
			case <-ctx.Done():
				return nil
			case <-app.httpDone:
				return app.httpErr
			case <-app.workerDone:
				return app.workerErr
			}
		}
	}
	clientCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	// run 拥有命令输入；取消时关闭 stdin/管道，解除阻塞读取。
	if closer, ok := input.(io.Closer); ok {
		stopInput := context.AfterFunc(clientCtx, func() { _ = closer.Close() })
		defer stopInput()
	}
	a := &answers{auto: *auto, scanner: bufio.NewScanner(input), out: output}
	results, err := runCases(clientCtx, *endpoint, token, *selected, a)
	if err != nil {
		return err
	}
	return json.Encode(output, results)
}
