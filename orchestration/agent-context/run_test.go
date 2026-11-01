package main

import (
	"context"
	"fmt"
	"github.com/kart-io/goagent-examples/orchestration/internal/demomodel"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent/core/session"
	"github.com/kart-io/goagent/tools"
)

func TestRunAllModesActualAgentInputsAndIdentity(t *testing.T) {
	for _, mode := range []string{"delegation", "pipeline", "parallel"} {
		t.Run(mode, func(t *testing.T) {
			ctx := tools.WithSessionID(context.Background(), "contaminating-parent-session")
			result, err := Run(ctx, Config{Mode: mode, Dir: t.TempDir()}, DefaultTask())
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Accepted) == 0 || result.Requests > 12 || result.Requests < 2 || result.Peak > 2 || result.InFlight != 0 {
				t.Fatalf("bad result: %+v", result)
			}
			if result.ChildrenCreated != result.ChildrenClosed {
				t.Fatalf("child cleanup: %d/%d", result.ChildrenClosed, result.ChildrenCreated)
			}
			if result.Usage.Root.ReservedRequests != result.Requests {
				t.Fatalf("double or missing budget: %+v", result.Usage.Root)
			}
			for _, capture := range result.Inputs {
				if capture.Role == "parent" {
					continue
				}
				s := string(encode(capture.Messages))
				if contains(s, DefaultTask().PrivateNote) || contains(s, "contaminating-parent-session") {
					t.Fatal("child inherited parent private input")
				}
				for _, v := range []string{"no transit", "direct-only", "family-count", "SGD"} {
					if !contains(s, v) {
						t.Errorf("missing brief fact %s", v)
					}
				}
			}
			for _, identity := range result.Identities {
				if identity.SessionID != identity.BuilderSessionID || identity.SessionID != identity.ToolSessionID || identity.SessionID == "contaminating-parent-session" {
					t.Fatalf("identity mismatch %+v", identity)
				}
				events, err := result.Logs.Read(ctx, identity.SessionID, 0)
				if err != nil {
					t.Fatal(err)
				}
				messages, err := session.DeriveMessages(events)
				if err != nil || len(messages) == 0 {
					t.Fatalf("missing independent history: %v", err)
				}
			}
			if mode == "pipeline" {
				if len(result.Accepted) != 2 || len(result.Accepted[1].Envelope.Dependencies) != 1 {
					t.Fatal("pipeline lost accepted upstream binding")
				}
			}
		})
	}
}
func TestRunCancellationFailureAndBudgetCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, Config{Mode: "parallel", Dir: t.TempDir()}, DefaultTask()); err == nil {
		t.Fatal("cancellation ignored")
	}
	result, err := Run(context.Background(), Config{Mode: "parallel", Dir: t.TempDir(), MaxRequests: 1}, DefaultTask())
	if err == nil || result.Requests > 1 || result.InFlight != 0 || result.ChildrenCreated != result.ChildrenClosed {
		t.Fatalf("budget exhaustion leaked or merged: %+v %v", result, err)
	}
}
func TestLiveAgentContext(t *testing.T) {
	if os.Getenv("GOAGENT_LIVE") != "1" {
		t.Skip("set GOAGENT_LIVE=1 for paid model validation")
	}
	for _, mode := range []string{"delegation", "pipeline", "parallel"} {
		t.Run(mode, func(t *testing.T) {
			c, err := livemodel.FromEnv()
			if err != nil {
				t.Fatal(livemodel.Redact(err.Error()))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			result, runErr := Run(ctx, Config{Mode: mode, Dir: t.TempDir(), Client: c}, DefaultTask())
			// Print only structured safe accounting/outcomes, even if validation fails.
			t.Logf("provider=%s model=%s mode=%s requests=%d reserved=%d peak=%d inflight=%d children=%d/%d root_tokens=%+v unknown_usage=%d partial_usage=%d complete=%t", c.Live.Provider, c.Live.Model, mode, result.Requests, result.Usage.Root.ReservedRequests, result.Peak, result.InFlight, result.ChildrenClosed, result.ChildrenCreated, result.Usage.Root.KnownTokens, result.Usage.UnknownUsageEntries, result.Usage.PartialUsageEntries, result.Usage.Complete)
			for _, agent := range result.Usage.Agents {
				t.Logf("provider=%s mode=%s agent=%s self_tokens=%+v known_entries=%d unknown_entries=%d", c.Live.Provider, mode, agent.AgentID, agent.Self.KnownTokens, agent.Self.KnownUsageEntries, agent.Self.UnknownUsageEntries)
			}
			for _, a := range result.Accepted {
				t.Logf("provider=%s mode=%s role=%s execution=%s business=%s adoption=%s", c.Live.Provider, mode, a.Envelope.StepID, a.ExecutionStatus, a.Report.Conclusion, a.AdoptionStatus)
			}
			if runErr != nil {
				t.Fatalf("live execution failed: %s", livemodel.Redact(runErr.Error()))
			}
			want := 2
			if mode == "delegation" {
				want = 1
			}
			if len(result.Accepted) != want || result.Requests < 1 || result.Requests > 12 || result.Usage.Root.ReservedRequests != result.Requests || result.Peak > 2 || result.InFlight != 0 || result.ChildrenCreated != want || result.ChildrenCreated != result.ChildrenClosed {
				t.Fatalf("invalid live accounting/outcome: reports=%d want=%d requests=%d reserved=%d peak=%d inflight=%d children=%d/%d", len(result.Accepted), want, result.Requests, result.Usage.Root.ReservedRequests, result.Peak, result.InFlight, result.ChildrenClosed, result.ChildrenCreated)
			}
			roles := map[string]bool{}
			for _, a := range result.Accepted {
				if a.Report.Conclusion != "feasible" || a.AdoptionStatus != "accepted" || a.ExecutionStatus != "succeeded" {
					t.Fatalf("invalid live role outcome: role=%s execution=%s business=%s adoption=%s", a.Envelope.StepID, a.ExecutionStatus, a.Report.Conclusion, a.AdoptionStatus)
				}
				roles[a.Envelope.StepID] = true
			}
			if !roles["itinerary"] || (mode == "pipeline" && !roles["budget"]) || (mode == "parallel" && !roles["rules"]) {
				t.Fatalf("missing expected live report roles for %s", mode)
			}
			for _, input := range result.Inputs {
				if input.Role == "parent" {
					continue
				}
				payload := string(encode(input.Messages))
				if contains(payload, DefaultTask().PrivateNote) {
					t.Fatal("live child input leaked parent-private data")
				}
				for _, required := range []string{"no transit", "direct-only", "family-count", "SGD"} {
					if !contains(payload, required) {
						t.Fatalf("live %s brief lost required constraint/decision %q", input.Role, required)
					}
				}
			}
		})
	}
}

func TestPipelineAcceptedUpstreamContentAndScopedArtifactRead(t *testing.T) {
	readDependency := false
	client := &transportFunc{scriptedModel: &scriptedModel{}, fn: func(ctx context.Context, r *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
		var b Brief
		for _, m := range r.Messages {
			if m.Role == "user" {
				_ = jsonUnmarshal([]byte(m.Content), &b)
			}
		}
		if b.Role == "budget" && len(r.Messages) > 0 {
			if !contains(string(encode(b)), "UpstreamReports") {
				return nil, fmt.Errorf("downstream received a binding but no accepted upstream content")
			}
			if !readDependency {
				readDependency = true
				c := demomodel.Call("read-upstream", "read_evidence")
				c.Function.Arguments = string(encode(map[string]any{"id": b.Dependencies[0].Artifact.ID}))
				return streamTurn(llm.StreamChunk{ToolCalls: []llm.ToolCall{c}}), nil
			}
		}
		return (&scriptedModel{role: b.Role}).CompleteToolsStream(ctx, r, nil)
	}}
	result, err := Run(context.Background(), Config{Mode: "pipeline", Dir: t.TempDir(), Client: client}, DefaultTask())
	if err != nil {
		t.Fatal(err)
	}
	if !readDependency || len(result.Accepted) != 2 {
		t.Fatal("downstream did not consume upstream artifact")
	}
}

func TestPipelineNeedsInputDoesNotInvokeBudgetOrBecomeExecutionFailure(t *testing.T) {
	budgetInvoked := false
	client := &transportFunc{scriptedModel: &scriptedModel{}, fn: func(ctx context.Context, r *llm.CompletionRequest, ts []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
		var b Brief
		for _, m := range r.Messages {
			if m.Role == "user" {
				_ = jsonUnmarshal([]byte(m.Content), &b)
			}
		}
		if b.Role == "budget" {
			budgetInvoked = true
		}
		out, err := (&scriptedModel{role: b.Role}).CompleteToolsStream(ctx, r, ts)
		if err != nil {
			return nil, err
		}
		chunk := <-out
		if len(chunk.ToolCalls) == 1 && chunk.ToolCalls[0].Function.Name == "submit_report" {
			var report ReportDraft
			_ = jsonUnmarshal([]byte(chunk.ToolCalls[0].Function.Arguments), &report)
			report.Conclusion = "needs_input"
			report.MissingInputs = []string{"user must reconfirm dates"}
			chunk.ToolCalls[0].Function.Arguments = string(encode(report))
		}
		return streamTurn(*chunk), nil
	}}
	result, err := Run(context.Background(), Config{Mode: "pipeline", Dir: t.TempDir(), Client: client}, DefaultTask())
	if err != nil || budgetInvoked || len(result.Accepted) != 1 || result.Accepted[0].Report.Conclusion != "needs_input" {
		t.Fatalf("business needs_input incorrectly advanced or failed: budget=%v accepted=%d err=%v", budgetInvoked, len(result.Accepted), err)
	}
}

func TestRejectedReportKeepsUsageAndDistinctExecutionAdoptionState(t *testing.T) {
	client := &transportFunc{scriptedModel: &scriptedModel{}, fn: func(ctx context.Context, r *llm.CompletionRequest, ts []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
		var b Brief
		for _, m := range r.Messages {
			if m.Role == "user" {
				_ = jsonUnmarshal([]byte(m.Content), &b)
			}
		}
		out, err := (&scriptedModel{role: b.Role}).CompleteToolsStream(ctx, r, ts)
		if err != nil {
			return nil, err
		}
		chunk := <-out
		if len(chunk.ToolCalls) == 1 && chunk.ToolCalls[0].Function.Name == "submit_report" {
			var report ReportDraft
			_ = jsonUnmarshal([]byte(chunk.ToolCalls[0].Function.Arguments), &report)
			report.Claims.TotalMinor = 35000
			chunk.ToolCalls[0].Function.Arguments = string(encode(report))
		}
		return streamTurn(*chunk), nil
	}}
	result, err := Run(context.Background(), Config{Mode: "pipeline", Dir: t.TempDir(), Client: client}, DefaultTask())
	if err == nil || len(result.Accepted) != 0 || result.Requests != 2 || result.Usage.Root.ReservedRequests != 2 || result.Usage.Root.KnownTokens.TotalTokens != 30 {
		t.Fatalf("rejection refunded/lost usage: requests=%d reserved=%d err=%v", result.Requests, result.Usage.Root.ReservedRequests, err)
	}
	if len(result.Executions) != 1 || result.Executions[0].ExecutionStatus != "failed" || len(result.Receipts) != 1 || result.Receipts[0].Status != "rejected" {
		t.Fatalf("failure and rejection not represented separately: %+v %+v", result.Executions, result.Receipts)
	}
}

func TestFreeTextOnlySpecialistRecordsFailure(t *testing.T) {
	client := &transportFunc{scriptedModel: &scriptedModel{}, fn: func(context.Context, *llm.CompletionRequest, []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
		return streamTurn(llm.StreamChunk{Content: "I assessed the trip but did not submit a report."}), nil
	}}
	result, err := Run(context.Background(), Config{Mode: "pipeline", Dir: t.TempDir(), Client: client}, DefaultTask())
	if err == nil || !contains(err.Error(), "specialist ended without accepted report") {
		t.Fatalf("missing native free-text failure: %v", err)
	}
	if len(result.Accepted) != 0 || len(result.Receipts) != 0 || len(result.Executions) != 1 {
		t.Fatal("free text fabricated a report or lost execution")
	}
	execution := result.Executions[0]
	if execution.ExecutionStatus != "failed" || !contains(execution.Reason, "specialist ended without accepted report") {
		t.Fatalf("failed specialist recorded status=%s reason=%s", execution.ExecutionStatus, execution.Reason)
	}
	data, readErr := os.ReadFile(filepath.Join(result.Dir, "executions.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var persisted []ExecutionRecord
	if decodeErr := jsonUnmarshal(data, &persisted); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if len(persisted) != 1 || persisted[0] != execution {
		t.Fatal("persisted execution differs from failed outcome")
	}
	if result.Requests != 1 || result.Usage.Root.ReservedRequests != 1 || result.Usage.Root.KnownTokens.TotalTokens != 15 || result.InFlight != 0 || result.ChildrenCreated != 1 || result.ChildrenClosed != 1 {
		t.Fatal("free-text failure lost usage or lifecycle cleanup")
	}
}
