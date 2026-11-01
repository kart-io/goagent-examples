// live verifies real provider usage. It never falls back to mock models.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/usage"
	"github.com/kart-io/goagent-examples/agent-usage/internal/demo"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/tools"
)

type result struct {
	Scenario string
	Started  time.Time
	Finished time.Time
	Passed   bool
	Failure  string `json:",omitempty"`
	Report   budget.AttributionReport
	Attempts []usage.ModelEvidence
	ToolRuns map[string]int64
}

func client(provider, model string, configs ...usage.ReasoningConfig) (llm.Client, error) {
	return demo.NewLiveClient(provider, model, configs...)
}

func runScenario(host context.Context, model llm.Client, scenario string) (r result) {
	r = result{Scenario: scenario, Started: time.Now().UTC(), ToolRuns: map[string]int64{}}
	defer func() { r.Finished = time.Now().UTC() }()
	controller := demo.Must(budget.New(budget.Config{RootKey: "live-" + scenario, Version: "usage-live", MaxModelRequests: 12, MaxAgentCalls: 8, MaxToolCalls: 8}))
	ctx := budget.WithController(host, controller)
	var mu sync.Mutex
	var hookError bool
	ctx = budget.WithUsageHook(ctx, func(_ context.Context, e budget.UsageEvent) {
		mu.Lock()
		defer mu.Unlock()
		r.Attempts = append(r.Attempts, e.Model)
		hookError = hookError || e.StatisticsError != nil || !e.HasStatistics
	})
	build := func(id, system string, dispatcher *tools.Dispatcher) *builder.SimpleAgent {
		b := builder.NewSimpleBuilder(model).WithAgentID(id).WithSystemPrompt(system).WithMaxTokens(2048).WithMaxIterations(4)
		if dispatcher != nil {
			b.WithTools(dispatcher.Tools()...)
		}
		return demo.Must(b.Build())
	}
	var lookup, research, review atomic.Int64
	writer := build("writer", "Reply with exactly OK. Do not explain.", nil)
	entry := writer
	prompt := "Reply with OK."
	switch scenario {
	case "single":
	case "child":
		child := demo.ChildTool("write", writer)
		entry = build("coordinator", "You must call write exactly once using {} before answering. After receiving its result, reply OK without calling any tool again.", demo.Dispatcher(child))
		prompt = "Delegate to write once and return its result."
	case "task":
		tool := tools.NewFunctionTool("lookup_order", "Read the synthetic test order by order_id", `{"type":"object","properties":{"order_id":{"type":"string","enum":["synthetic-order"]}},"required":["order_id"],"additionalProperties":false}`, func(context.Context, map[string]any) (any, error) {
			lookup.Add(1)
			return map[string]any{"order_id": "synthetic-order", "status": "paid"}, nil
		})
		researcher := build("researcher", `Call lookup_order exactly once with {"order_id":"synthetic-order"}. After the tool result, reply only with the order status. Do not call any other tool.`, demo.Dispatcher(tool))
		reviewer := build("reviewer", "Review the synthetic task. Reply with exactly approved.", nil)
		researchTool := childTool("research", researcher, &research)
		reviewTool := childTool("review", reviewer, &review)
		entry = build("coordinator", "Call research and review each exactly once with {}. You must receive both results before answering. Then reply with a brief summary in at most 10 words. Do not repeat tool calls.", demo.Dispatcher(researchTool, reviewTool))
		prompt = "Analyze synthetic-order with research and review, then summarize."
	default:
		r.Failure = "unknown scenario"
		return
	}
	_, invokeErr := entry.AsRunnable().Invoke(ctx, prompt)
	// Completed accounting must remain queryable even if execution was canceled.
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(host), 5*time.Second)
	defer cancel()
	var reportErr error
	r.Report, reportErr = controller.Attribution(reportCtx)
	r.ToolRuns = map[string]int64{"lookup_order": lookup.Load(), "research": research.Load(), "review": review.Load()}
	switch {
	case invokeErr != nil:
		classified := llm.ClassifyError(invokeErr)
		var pe *llm.ProviderError
		status := 0
		if errors.As(invokeErr, &pe) {
			status = pe.StatusCode
		}
		r.Failure = fmt.Sprintf("agent execution failed (%T, reason=%s, http=%d); usage preserved", invokeErr, classified.Reason, status)
		var argument *tools.ArgumentError
		if errors.As(invokeErr, &argument) {
			detail := argument.Message
			for _, prefix := range []string{"GEMINI", "DEEPSEEK", "MINIMAX"} {
				if key := os.Getenv(prefix + "_API_KEY"); key != "" {
					detail = strings.ReplaceAll(detail, key, "[REDACTED]")
				}
			}
			r.Failure += "; argument validation: " + detail
		}
	case reportErr != nil:
		r.Failure = "final ledger query failed"
	case hookError:
		r.Failure = "hook statistics unavailable"
	default:
		if err := validate(r); err != nil {
			r.Failure = err.Error()
		}
	}
	r.Passed = r.Failure == ""
	return
}

func childTool(name string, a *builder.SimpleAgent, count *atomic.Int64) interfaces.Tool {
	return demo.Must(tools.NewRunnableTool(a.AsRunnable(), tools.RunnableToolConfig[any, *builder.AgentOutput]{
		Name: name, Description: "Delegate the synthetic order task; call once with {}", Version: "usage-live", ModelInvoking: true,
		ArgsSchema: `{"type":"object","properties":{},"additionalProperties":false}`,
		Decode: func(map[string]any) (any, error) {
			count.Add(1)
			return "Complete the synthetic order task according to your instructions.", nil
		},
		Encode: func(o *builder.AgentOutput) (any, error) { return o.Result, nil },
	}))
}

func validate(r result) error {
	if !r.Report.Complete || r.Report.Root.KnownTokens.TotalTokens <= 0 {
		return errors.New("missing complete, nonzero provider usage")
	}
	var roleTotal, attemptTotal int
	var roleDetails, attemptDetails interfaces.TokenUsage
	for _, agent := range r.Report.Agents {
		roleTotal += agent.Self.KnownTokens.TotalTokens
		roleDetails.Add(&agent.Self.KnownTokens)
	}
	seen := map[string]bool{}
	for _, e := range r.Attempts {
		if !e.AccountingCommitted || e.Replayed || e.KnownFields&usage.BasicFields != usage.BasicFields || e.AttemptKey == "" || seen[e.AttemptKey] {
			return errors.New("unexpected missing, repeated, or uncommitted attempt evidence")
		}
		seen[e.AttemptKey] = true
		attemptTotal += e.Tokens.TotalTokens
		attemptDetails.Add(&e.Tokens)
	}
	if roleDetails != r.Report.Root.KnownTokens || attemptDetails != roleDetails {
		return errors.New("hook, role, and root detail fields do not agree")
	}
	if len(r.Attempts) != r.Report.Root.ReservedRequests || roleTotal != r.Report.Root.KnownTokens.TotalTokens || attemptTotal != roleTotal {
		return errors.New("hook, role, request count, and root totals do not agree")
	}
	calls := map[string][]budget.CallAttribution{}
	for _, c := range r.Report.Calls {
		calls[c.AgentID] = append(calls[c.AgentID], c)
	}
	if r.Scenario == "single" {
		if len(calls["writer"]) != 1 || calls["writer"][0].Self.KnownTokens.TotalTokens != roleTotal {
			return errors.New("single Agent attribution mismatch")
		}
	} else {
		if len(calls["coordinator"]) != 1 || calls["coordinator"][0].Inclusive.KnownTokens.TotalTokens != roleTotal {
			return errors.New("parent subtree mismatch")
		}
		children := []string{"writer"}
		if r.Scenario == "task" {
			children = []string{"researcher", "reviewer"}
		}
		for _, id := range children {
			if len(calls[id]) != 1 || calls[id][0].ParentAgentCallKey != calls["coordinator"][0].AgentCallKey || calls[id][0].Self.KnownTokens.TotalTokens <= 0 {
				return fmt.Errorf("child attribution mismatch: %s", id)
			}
		}
	}
	if r.Scenario == "task" && (r.ToolRuns["lookup_order"] != 1 || r.ToolRuns["research"] != 1 || r.ToolRuns["review"] != 1 || r.Report.Root.ReservedToolCalls != 3) {
		return errors.New("task must execute both children and the ordinary tool exactly once")
	}
	return nil
}

type report struct {
	Provider           string
	Model              string
	RequestedReasoning *usage.ReasoningConfig `json:",omitempty"`
	Results            []result
}

func main() {
	provider := flag.String("provider", "", "deepseek, minimax, or gemini (required)")
	modelName := flag.String("model", "", "model name; defaults to PROVIDER_MODEL environment variable")
	scenario := flag.String("scenario", "all", "all, single, child, or task")
	out := flag.String("out", "", "new report directory (required; never overwrites)")
	effort := flag.String("reasoning-effort", "", "explicit reasoning effort (supported values depend on provider/model)")
	thinkingBudget := flag.Int("thinking-budget", 0, "explicit Gemini thinking budget; 0 disables where supported, -1 is dynamic")
	flag.Parse()
	budgetSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "thinking-budget" {
			budgetSet = true
		}
	})
	var configs []usage.ReasoningConfig
	if *effort != "" || budgetSet {
		configs = append(configs, usage.ReasoningConfig{Effort: *effort, BudgetTokens: *thinkingBudget, BudgetSet: budgetSet})
	}
	if err := execute(*provider, *modelName, *scenario, *out, configs...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func execute(provider, modelName, scenario, out string, configs ...usage.ReasoningConfig) error {
	if len(configs) > 0 {
		if err := configs[0].Validate(); err != nil {
			return err
		}
	}
	if provider != "deepseek" && provider != "minimax" && provider != "gemini" {
		return errors.New("-provider must be deepseek, minimax, or gemini")
	}
	if modelName == "" {
		modelName = os.Getenv(strings.ToUpper(provider) + "_MODEL")
	}
	if modelName == "" {
		return errors.New("-model or corresponding PROVIDER_MODEL is required")
	}
	if out == "" {
		return errors.New("-out NEW_DIRECTORY is required")
	}
	scenarios := []string{scenario}
	if scenario == "all" {
		scenarios = []string{"single", "child", "task"}
	}
	for _, s := range scenarios {
		if s != "single" && s != "child" && s != "task" {
			return errors.New("unknown -scenario")
		}
	}
	c, err := client(provider, modelName, configs...)
	if err != nil {
		return err
	}
	if err = os.Mkdir(out, 0700); err != nil {
		return err
	}
	r := report{Provider: provider, Model: modelName}
	if len(configs) > 0 {
		copy := configs[0]
		r.RequestedReasoning = &copy
	}
	for _, s := range scenarios {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		one := runScenario(ctx, c, s)
		cancel()
		r.Results = append(r.Results, one)
		data, marshalErr := json.MarshalIndent(r, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		if err := os.WriteFile(filepath.Join(out, "results.json"), data, 0600); err != nil {
			return err
		}
		fmt.Printf("provider=%s scenario=%s passed=%t tokens=%d requests=%d tools=%d unknown=%d partial=%d\n", provider, s, one.Passed, one.Report.Root.KnownTokens.TotalTokens, one.Report.Root.ReservedRequests, one.Report.Root.ReservedToolCalls, one.Report.UnknownUsageEntries, one.Report.PartialUsageEntries)
		for _, a := range one.Report.Agents {
			fmt.Printf("  agent=%s self=%d\n", a.AgentID, a.Self.KnownTokens.TotalTokens)
		}
		for _, e := range one.Attempts {
			fmt.Printf("  attempt agent=%s requested_model=%s response_model=%q reasoning=%d reasoning_known=%t tool_input=%d tool_input_known=%t reasoning_configured=%t\n", e.AgentID, e.Model, e.ResponseModel, e.Tokens.ReasoningTokens, e.KnownFields&usage.Reasoning != 0, e.Tokens.ToolUsePromptTokens, e.KnownFields&usage.ToolUsePrompt != 0, e.ReasoningConfigured)
		}
		if !one.Passed {
			return fmt.Errorf("%s failed: %s; evidence saved, no retry", s, one.Failure)
		}
	}
	return nil
}
