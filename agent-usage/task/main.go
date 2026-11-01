// task counts a whole task: coordinator -> parallel children -> tools -> summary.
package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent-examples/agent-usage/internal/demo"
	"github.com/kart-io/goagent/tools"
)

func run() budget.AttributionReport {
	// One controller per business task; every descendant receives the same context.
	ctx, controller := demo.Context("order-analysis-001")
	var toolRuns, notifications atomic.Int64
	lookup := tools.NewFunctionTool("lookup_order", "Read order data", `{"type":"object"}`, func(context.Context, map[string]any) (any, error) {
		toolRuns.Add(1)
		return "order found", nil // A local tool uses no model tokens itself.
	})
	researcher := demo.Must(builder.NewSimpleBuilder(demo.NewModel(
		demo.ToolTurn(1, 0, "lookup_order"), demo.Answer(2, 1), // 4 tokens in total
	)).WithAgentID("researcher").WithTools(demo.Dispatcher(lookup).Tools()...).WithMaxIterations(2).Build())
	reviewer := demo.Must(builder.NewSimpleBuilder(demo.NewModel(demo.Answer(3, 2))).WithAgentID("reviewer").Build())
	parent := demo.Must(builder.NewSimpleBuilder(demo.NewModel(
		demo.ToolTurn(2, 1, "research", "review"), demo.Answer(4, 2), // parent Self = 9
	)).WithAgentID("coordinator").WithTools(demo.Dispatcher(
		demo.ChildTool("research", researcher), demo.ChildTool("review", reviewer),
	).Tools()...).WithParallelToolCalls(2).WithMaxIterations(2).
		WithUsageHook(func(_ context.Context, e budget.UsageEvent) {
			notifications.Add(1)
			// Live notification only. Do not add cumulative Root snapshots together.
			fmt.Printf("hook agent=%s attempt_tokens=%d replay=%t committed=%t\n", e.Model.AgentID, e.Model.Tokens.TotalTokens, e.Model.Replayed, e.Model.AccountingCommitted)
			if e.StatisticsError != nil {
				fmt.Printf("statistics unavailable: %v\n", e.StatisticsError)
			}
		}).Build())
	_, err := parent.AsRunnable().Invoke(ctx, "Analyze the order, review it, and summarize")
	demo.Check(err)
	// Query after task completion: this ledger snapshot is the final task total.
	report := demo.Must(controller.Attribution(ctx))
	fmt.Printf("task tokens: input=%d output=%d total=%d model_requests=%d tool_calls=%d complete=%t\n",
		report.Root.KnownTokens.PromptTokens, report.Root.KnownTokens.CompletionTokens, report.Root.KnownTokens.TotalTokens,
		report.Root.ReservedRequests, report.Root.ReservedToolCalls, report.Complete)
	for _, agent := range report.Agents {
		fmt.Printf("agent=%s self=%d\n", agent.AgentID, agent.Self.KnownTokens.TotalTokens)
	}
	fmt.Printf("local_tool_runs=%d hook_notifications=%d unknown=%d partial=%d\n", toolRuns.Load(), notifications.Load(), report.UnknownUsageEntries, report.PartialUsageEntries)
	if toolRuns.Load() != 1 || notifications.Load() != 5 {
		panic("unexpected execution coverage")
	}
	return report
}
func main() { run() }
