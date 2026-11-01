package main

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
)

func TestNativeReportSchemaUsesConcreteArrays(t *testing.T) {
	s, err := reportSchema()
	if err != nil {
		t.Fatal(err)
	}
	var schema any
	_ = jsonUnmarshal(encode(s), &schema)
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if _, union := x["type"].([]any); union {
				t.Error("native schema emits a nullable union instead of a concrete array")
			}
			for _, value := range x {
				walk(value)
			}
		case []any:
			for _, value := range x {
				walk(value)
			}
		}
	}
	walk(schema)
}

func TestSupplierEvidenceDefinesBaggageBasis(t *testing.T) {
	var facts map[string]any
	if err := jsonUnmarshal(fixture, &facts); err != nil {
		t.Fatal(err)
	}
	if facts["BaggageBasis"] != "per traveler on each leg" {
		t.Fatal("supplier evidence leaves one-bag allowance ambiguous for two travelers")
	}
}

// Replace only the transport: the Builder, evidence tool and acceptance all run.
func TestNativeReportAcceptsReadCitationIDAndRetainsFullReference(t *testing.T) {
	client := &transportFunc{scriptedModel: &scriptedModel{}, fn: func(ctx context.Context, r *llm.CompletionRequest, offered []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
		var b Brief
		for _, msg := range r.Messages {
			if msg.Role == "user" {
				_ = jsonUnmarshal([]byte(msg.Content), &b)
			}
		}
		out, err := (&scriptedModel{role: b.Role}).CompleteToolsStream(ctx, r, offered)
		if err != nil {
			return nil, err
		}
		chunk := <-out
		for i, call := range chunk.ToolCalls {
			if call.Function.Name == "submit_report" {
				var args map[string]any
				_ = jsonUnmarshal([]byte(call.Function.Arguments), &args)
				args["evidence"] = b.Evidence[0].ID
				chunk.ToolCalls[i].Function.Arguments = string(encode(args))
			}
		}
		return streamTurn(*chunk), nil
	}}
	result, err := Run(context.Background(), Config{Mode: "pipeline", Dir: t.TempDir(), Client: client}, DefaultTask())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Accepted) != 2 {
		t.Fatal("citation did not yield both accepted reports")
	}
	for _, accepted := range result.Accepted {
		if len(accepted.Report.Evidence) != 1 {
			t.Fatal("full host reference missing")
		}
		ref := accepted.Report.Evidence[0]
		if ref.ID == "" || ref.Digest == "" || ref.TaskID != "synthetic-family-trip" || ref.Version != 1 || ref.CandidateID != "A" || ref.Size < 1 {
			t.Fatal(fmt.Sprintf("incomplete host evidence reference for %s", accepted.Envelope.StepID))
		}
	}
}

func TestNativeCitationRejectsUnknownUnreadAndOtherInvocationReads(t *testing.T) {
	for _, scenario := range []string{"unknown-id", "allowlisted-but-unread", "read-by-upstream-only"} {
		t.Run(scenario, func(t *testing.T) {
			upstreamRead := false
			client := &transportFunc{scriptedModel: &scriptedModel{}, fn: func(ctx context.Context, request *llm.CompletionRequest, offered []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
				var b Brief
				for _, msg := range request.Messages {
					if msg.Role == "user" {
						_ = jsonUnmarshal([]byte(msg.Content), &b)
					}
				}
				out, err := (&scriptedModel{role: b.Role}).CompleteToolsStream(ctx, request, offered)
				if err != nil {
					return nil, err
				}
				chunk := <-out
				if len(chunk.ToolCalls) != 1 {
					return nil, fmt.Errorf("probe expected one native call")
				}
				call := &chunk.ToolCalls[0]
				if b.Role == "itinerary" && call.Function.Name == "submit_report" {
					upstreamRead = true
				}
				attack := scenario == "allowlisted-but-unread" && call.Function.Name == "read_evidence"
				attack = attack || scenario == "read-by-upstream-only" && b.Role == "budget" && call.Function.Name == "read_evidence"
				if attack {
					// Having an allowed Brief reference (or an upstream receipt) is not a read.
					draft := ReportDraft{Conclusion: "feasible", Summary: "A structurally valid report without this invocation reading evidence.", Claims: b.RequiredFacts, DecisionIDs: []string{"direct-only", "family-count"}, Evidence: b.Evidence[0].ID}
					call.Function.Name = "submit_report"
					call.Function.Arguments = string(encode(draft))
				}
				if scenario == "unknown-id" && call.Function.Name == "submit_report" {
					var args map[string]any
					_ = jsonUnmarshal([]byte(call.Function.Arguments), &args)
					args["evidence"] = "evidence-from-another-task"
					call.Function.Arguments = string(encode(args))
				}
				return streamTurn(*chunk), nil
			}}
			result, err := Run(context.Background(), Config{Mode: "pipeline", Dir: t.TempDir(), Client: client}, DefaultTask())
			if err == nil || !contains(err.Error(), "evidence citation was not read in this invocation") {
				t.Fatalf("missing read-scope rejection: %v", err)
			}
			wantAccepted := 0
			if scenario == "read-by-upstream-only" {
				wantAccepted = 1
				if !upstreamRead {
					t.Fatal("cross-invocation test never read the upstream evidence")
				}
			}
			acceptedReceipts := 0
			for _, receipt := range result.Receipts {
				if receipt.Status == "accepted" {
					acceptedReceipts++
				}
			}
			if acceptedReceipts != wantAccepted {
				t.Fatalf("accepted %d receipts, want %d", acceptedReceipts, wantAccepted)
			}
			if result.InFlight != 0 || result.ChildrenCreated != result.ChildrenClosed {
				t.Fatal("rejected citation leaked lifecycle")
			}
		})
	}
}

// A successful read receipt does not make a later unreadable/corrupt artifact valid.
func TestNativeReadCitationRevalidatesStoredEvidence(t *testing.T) {
	h := setupHost(t)
	changed := false
	h.client.Client = &transportFunc{scriptedModel: &scriptedModel{}, fn: func(ctx context.Context, r *llm.CompletionRequest, offered []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
		out, err := (&scriptedModel{role: "itinerary"}).CompleteToolsStream(ctx, r, offered)
		if err != nil {
			return nil, err
		}
		chunk := <-out
		if len(chunk.ToolCalls) == 1 && chunk.ToolCalls[0].Function.Name == "submit_report" {
			changed = true
			if err := os.Chmod(h.path(h.evidence.ID), 0600); err != nil {
				return nil, err
			}
			if err := os.WriteFile(h.path(h.evidence.ID), []byte("{}"), 0600); err != nil {
				return nil, err
			}
		}
		return streamTurn(*chunk), nil
	}}
	b, err := h.brief("itinerary", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.execute(budget.WithController(context.Background(), h.controller), b)
	if !changed || err == nil || !contains(err.Error(), "artifact integrity mismatch") {
		t.Fatalf("read receipt bypassed immutable evidence verification: %v", err)
	}
	if len(h.receipts) != 0 || len(h.adoptionReceipts) != 1 {
		t.Fatal("corrupt evidence was adopted or rejection lost")
	}
}
