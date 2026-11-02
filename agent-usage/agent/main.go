// agent demonstrates a standalone Agent and a selected child Agent's usage.
package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/kart-io/goagent-examples/agent-usage/internal/demo"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core/budget"
)

func run(mode string) budget.AttributionReport {
	ctx, controller := demo.Context("agent-demo-" + mode)
	hook := func(_ context.Context, e budget.UsageEvent) {
		// Filter by AgentID for a role, AgentCallKey for a particular invocation.
		// Model.Tokens belongs to this attempt; it excludes descendant requests.
		if e.Model.AgentID == "writer" {
			fmt.Printf("writer hook: tokens=%d replay=%t committed=%t\n", e.Model.Tokens.TotalTokens, e.Model.Replayed, e.Model.AccountingCommitted)
			if e.HasStatistics {
				for _, agent := range e.Statistics.Agents {
					if agent.AgentID == "writer" {
						fmt.Printf("writer hook cumulative self=%d\n", agent.Self.KnownTokens.TotalTokens)
					}
				}
			}
		}
	}
	writer := demo.Must(builder.NewSimpleBuilder(demo.NewModel(demo.Answer(7, 3))).WithAgentID("writer").Build())
	entry := writer
	switch mode {
	case "single":
		// Bind the same hook to the invocation context (also works via WithUsageHook).
	case "child":
		entry = demo.Must(builder.NewSimpleBuilder(demo.NewModel(
			demo.ToolTurn(2, 1, "write"), demo.Answer(3, 1),
		)).WithAgentID("coordinator").WithTools(demo.Dispatcher(demo.ChildTool("write", writer)).Tools()...).WithMaxIterations(2).Build())
	default:
		panic("mode must be single or child")
	}
	ctx = budget.WithUsageHook(ctx, hook) // Inherited when the writer runs as a child.
	_, err := entry.AsRunnable().Invoke(ctx, "Write the answer")
	demo.Check(err)
	report := demo.Must(controller.Attribution(ctx))
	for _, agent := range report.Agents {
		if agent.AgentID == "writer" {
			fmt.Printf("writer role: input=%d output=%d self=%d requests=%d\n", agent.Self.KnownTokens.PromptTokens, agent.Self.KnownTokens.CompletionTokens, agent.Self.KnownTokens.TotalTokens, agent.Self.ReservedRequests)
		}
	}
	for _, call := range report.Calls {
		// Calls distinguish repeated invocations of the same AgentID.
		fmt.Printf("call agent=%s self=%d inclusive=%d has_parent=%t\n", call.AgentID, call.Self.KnownTokens.TotalTokens, call.Inclusive.KnownTokens.TotalTokens, call.ParentAgentCallKey != "")
	}
	fmt.Printf("mode=%s task_total=%d complete=%t\n", mode, report.Root.KnownTokens.TotalTokens, report.Complete)
	return report
}
func main() {
	mode := flag.String("mode", "single", "single or child")
	flag.Parse()
	run(*mode)
}
