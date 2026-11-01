// cases verifies mixed-provider tasks, replay and controlled failures with real
// clients. Offline tests inject scripted clients; the CLI never does.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/core/usage"
	"github.com/kart-io/goagent-examples/agent-usage/internal/demo"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/tools"
)

var errControlled = errors.New("controlled lookup failure")

const orderSchema = `{"type":"object","properties":{"order_id":{"type":"string","enum":["synthetic-order"]}},"required":["order_id"],"additionalProperties":false}`
const taskSchema = `{"type":"object","properties":{"task":{"type":"string","enum":["synthetic-order"]}},"required":["task"],"additionalProperties":false}`

type countedClient struct {
	llm.ToolStreamClient
	calls atomic.Int64
}

func (c *countedClient) CompleteStream(ctx context.Context, r *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	c.calls.Add(1)
	return c.ToolStreamClient.CompleteStream(ctx, r)
}
func (c *countedClient) CompleteToolsStream(ctx context.Context, r *llm.CompletionRequest, t []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	c.calls.Add(1)
	return c.ToolStreamClient.CompleteToolsStream(ctx, r, t)
}
func (c *countedClient) ModelName() string {
	if n, ok := c.ToolStreamClient.(interface{ ModelName() string }); ok {
		return n.ModelName()
	}
	return "unknown"
}
func (c *countedClient) ResolveReasoning(r *llm.CompletionRequest) *usage.ReasoningConfig {
	if n, ok := c.ToolStreamClient.(interface {
		ResolveReasoning(*llm.CompletionRequest) *usage.ReasoningConfig
	}); ok {
		return n.ResolveReasoning(r)
	}
	return r.Reasoning.Clone()
}
func (c *countedClient) ValidateReasoning(r *llm.CompletionRequest) error {
	return llm.ValidateReasoningRequest(c.ToolStreamClient, r)
}

type outcome struct {
	Case                                  string
	Passed                                bool
	Failure                               string `json:",omitempty"`
	ExpectedErrorObserved                 bool
	Before, After                         budget.AttributionReport
	Attempts                              []usage.ModelEvidence
	Platforms                             map[string]interfaces.TokenUsage
	PlatformRequests                      map[string]int
	AgentPlatforms                        map[string]string
	NetworkCallsBefore, NetworkCallsAfter int64
	ToolRuns                              int64
	ReplayNotifications                   int
}

func runCase(host context.Context, name string, clients map[string]llm.ToolStreamClient, dir string) (r outcome) {
	r.Case = name
	ctx, cancel := context.WithCancel(host)
	defer cancel()
	store := demo.Must(durable.NewFileStore(dir))
	controller := demo.Must(budget.NewDurable(budget.Config{RootKey: name, Version: "usage-cases", MaxModelRequests: 10, MaxAgentCalls: 8, MaxToolCalls: 8}, store))
	ctx = budget.WithController(durable.WithJournal(durable.WithExecutionKey(ctx, name), store), controller)
	models := map[string]*countedClient{}
	for p, c := range clients {
		models[p] = &countedClient{ToolStreamClient: c}
	}
	r.AgentPlatforms = map[string]string{"coordinator": "gemini", "researcher": "deepseek", "reviewer": "minimax"}
	var mu sync.Mutex
	var hookErr bool
	ctx = budget.WithUsageHook(ctx, func(_ context.Context, e budget.UsageEvent) {
		mu.Lock()
		defer mu.Unlock()
		r.Attempts = append(r.Attempts, e.Model)
		hookErr = hookErr || !e.HasStatistics || e.StatisticsError != nil
		if e.Model.Replayed {
			r.ReplayNotifications++
		}
	})
	var toolRuns atomic.Int64
	lookup := tools.NewFunctionTool("lookup_order", "Lookup synthetic-order by order_id", orderSchema, func(context.Context, map[string]any) (any, error) {
		toolRuns.Add(1)
		switch name {
		case "tool-error":
			return nil, errControlled
		case "cancel":
			cancel()
			return nil, ctx.Err()
		default:
			return map[string]any{"order_id": "synthetic-order", "status": "paid"}, nil
		}
	})
	build := func(id, platform, system string, ts ...interfaces.Tool) *builder.SimpleAgent {
		return demo.Must(builder.NewSimpleBuilder(models[platform]).WithAgentID(id).WithDurableScope(id).WithSystemPrompt(system).
			WithTools(demo.Dispatcher(ts...).Tools()...).WithParallelToolCalls(2).WithMaxIterations(4).WithMaxTokens(2048).Build())
	}
	var entry *builder.SimpleAgent
	switch name {
	case "mixed-replay":
		researcher := build("researcher", "deepseek", `Call lookup_order exactly once with {"order_id":"synthetic-order"}, then reply only with the status.`, lookup)
		reviewer := build("reviewer", "minimax", "Review the synthetic order and reply approved in one word.")
		child := func(name string, a *builder.SimpleAgent) interfaces.Tool {
			return demo.Must(tools.NewRunnableTool(a.AsRunnable(), tools.RunnableToolConfig[any, *builder.AgentOutput]{Name: name, Description: "Delegate synthetic-order", Version: "usage-cases", ModelInvoking: true, ArgsSchema: taskSchema,
				Decode: func(args map[string]any) (any, error) { return "Complete task for " + args["task"].(string), nil }, Encode: func(o *builder.AgentOutput) (any, error) { return o.Result, nil }}))
		}
		entry = build("coordinator", "gemini", `Call research and review each exactly once with {"task":"synthetic-order"}. After both results, summarize in at most ten words. Do not repeat tools.`, child("research", researcher), child("review", reviewer))
	case "tool-error", "cancel":
		entry = build("coordinator", "gemini", `Call lookup_order exactly once with {"order_id":"synthetic-order"} before answering.`, lookup)
	default:
		r.Failure = "unknown case"
		return
	}
	query := func() budget.AttributionReport {
		q, stop := context.WithTimeout(context.WithoutCancel(host), 5*time.Second)
		defer stop()
		v, err := controller.Attribution(q)
		if err != nil {
			r.Failure = "ledger query failed"
		}
		return v
	}
	count := func() int64 {
		var n int64
		for _, c := range models {
			n += c.calls.Load()
		}
		return n
	}
	_, err := entry.AsRunnable().Invoke(ctx, "Complete synthetic-order")
	r.Before = query()
	r.NetworkCallsBefore = count()
	if name == "mixed-replay" && err == nil && r.Failure == "" {
		// Same scope/input/store: replay must not contact models or repeat the tool.
		_, err = entry.AsRunnable().Invoke(ctx, "Complete synthetic-order")
	}
	r.After = query()
	r.NetworkCallsAfter = count()
	r.ToolRuns = toolRuns.Load()
	r.ExpectedErrorObserved = (name == "tool-error" && errors.Is(err, errControlled)) || (name == "cancel" && errors.Is(err, context.Canceled))
	r.Platforms = map[string]interfaces.TokenUsage{}
	r.PlatformRequests = map[string]int{}
	var attempts interfaces.TokenUsage
	seen := map[string]bool{}
	for _, e := range r.Attempts {
		if e.Replayed {
			continue
		}
		if seen[e.AttemptKey] || !e.AccountingCommitted || e.KnownFields&usage.BasicFields != usage.BasicFields {
			r.Failure = "invalid or duplicate attempt evidence"
		}
		seen[e.AttemptKey] = true
		attempts.Add(&e.Tokens)
		// The trusted host routing map identifies vendors; an OpenAI-compatible
		// client's Provider label alone cannot identify a MiniMax endpoint.
		platform := r.AgentPlatforms[e.AgentID]
		if platform == "" {
			r.Failure = "unknown Agent platform routing"
		}
		tokens := r.Platforms[platform]
		tokens.Add(&e.Tokens)
		r.Platforms[platform] = tokens
		r.PlatformRequests[platform]++
	}
	if attempts != r.After.Root.KnownTokens || int64(len(seen)) != r.NetworkCallsAfter || len(seen) != r.After.Root.ReservedRequests || hookErr {
		r.Failure = "hook/ledger/client accounting mismatch"
	}
	if !r.After.Complete || r.After.Root.KnownTokens.TotalTokens <= 0 {
		r.Failure = "missing complete provider usage"
	}
	if r.ToolRuns != 1 {
		r.Failure = "ordinary tool must execute exactly once"
	}
	if name == "mixed-replay" {
		if r.After.Root.ReservedToolCalls != 3 || len(r.After.Calls) != 3 {
			r.Failure = "missing child/tool attribution"
		}
		if err != nil {
			r.Failure = fmt.Sprintf("unexpected task/replay error (%T)", err)
		}
		if !reflect.DeepEqual(r.Before, r.After) || r.NetworkCallsBefore != r.NetworkCallsAfter || r.ReplayNotifications == 0 {
			r.Failure = "replay changed accounting, called a model, or emitted no replay notification"
		}
		for _, platform := range []string{"gemini", "deepseek", "minimax"} {
			if r.PlatformRequests[platform] == 0 {
				r.Failure = "missing model platform: " + platform
			}
		}
		var self interfaces.TokenUsage
		for _, a := range r.After.Agents {
			self.Add(&a.Self.KnownTokens)
		}
		if self != attempts {
			r.Failure = "agent Self totals mismatch"
		}
		for _, c := range r.After.Calls {
			if c.AgentID == "coordinator" && c.Inclusive.KnownTokens != attempts {
				r.Failure = "parent Inclusive mismatch"
			}
		}
	} else if !r.ExpectedErrorObserved {
		r.Failure = fmt.Sprintf("expected controlled error not observed (%T)", err)
	}
	r.Passed = r.Failure == ""
	return
}

func main() {
	which := flag.String("case", "all", "all, mixed-replay, tool-error, cancel")
	out := flag.String("out", "", "new report directory (required)")
	flag.Parse()
	if err := execute(*which, *out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func execute(which, out string) error {
	names := []string{which}
	if which == "all" {
		names = []string{"mixed-replay", "tool-error", "cancel"}
	}
	needMixed := false
	for _, n := range names {
		switch n {
		case "mixed-replay":
			needMixed = true
		case "tool-error", "cancel":
		default:
			return errors.New("unknown case")
		}
	}
	if out == "" {
		return errors.New("-out NEW_DIRECTORY is required")
	}
	specs := []struct {
		platform, model string
		config          usage.ReasoningConfig
	}{
		{"gemini", "gemini-2.5-flash", usage.ReasoningConfig{BudgetSet: true, BudgetTokens: 128}},
		{"deepseek", "deepseek-chat", usage.ReasoningConfig{Effort: "low"}},
		{"minimax", "MiniMax-M3.1-Flash", usage.ReasoningConfig{Effort: "low"}},
	}
	clients := map[string]llm.ToolStreamClient{}
	for _, s := range specs {
		if s.platform != "gemini" && !needMixed {
			continue
		}
		model := os.Getenv(upper(s.platform) + "_MODEL")
		if model == "" {
			model = s.model
		}
		c, err := demo.NewLiveClient(s.platform, model, s.config)
		if err != nil {
			return err
		}
		clients[s.platform] = c.(llm.ToolStreamClient)
	}
	if err := os.Mkdir(out, 0700); err != nil {
		return err
	}
	var results []outcome
	for _, name := range names {
		dir, err := os.MkdirTemp("", "goagent-usage-case-")
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		r := runCase(ctx, name, clients, dir)
		cancel()
		_ = os.RemoveAll(dir)
		results = append(results, r)
		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(out, "results.json"), data, 0600); err != nil {
			return err
		}
		fmt.Printf("case=%s passed=%t tokens=%d calls_before=%d calls_after=%d tools=%d replay_notifications=%d expected_error=%t\n", name, r.Passed, r.After.Root.KnownTokens.TotalTokens, r.NetworkCallsBefore, r.NetworkCallsAfter, r.ToolRuns, r.ReplayNotifications, r.ExpectedErrorObserved)
		for _, p := range []string{"gemini", "deepseek", "minimax"} {
			if r.PlatformRequests[p] > 0 {
				fmt.Printf("  platform=%s requests=%d tokens=%d reasoning_known_sum=%d\n", p, r.PlatformRequests[p], r.Platforms[p].TotalTokens, r.Platforms[p].ReasoningTokens)
			}
		}
		if !r.Passed {
			return fmt.Errorf("case %s failed: %s; evidence saved, no retry", name, r.Failure)
		}
	}
	return nil
}
func upper(s string) string {
	switch s {
	case "gemini":
		return "GEMINI"
	case "deepseek":
		return "DEEPSEEK"
	default:
		return "MINIMAX"
	}
}
