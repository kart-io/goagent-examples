package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/tools"
)

// These probes run against the actual tools offered by the production Builder,
// including its own Dispatcher; no stand-in schema or acceptance function.
func TestActualSubmitReportSchemaAndDispatcherBusinessEnum(t *testing.T) {
	probes := 0
	client := &transportFunc{scriptedModel: &scriptedModel{}, fn: func(ctx context.Context, r *llm.CompletionRequest, offered []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
		var b Brief
		for _, m := range r.Messages {
			if m.Role == "user" {
				_ = jsonUnmarshal([]byte(m.Content), &b)
			}
		}
		for _, tool := range offered {
			if tool.Name() != "submit_report" {
				continue
			}
			var schema jsonschema.Schema
			if err := jsonUnmarshal([]byte(tool.ArgsSchema()), &schema); err != nil {
				return nil, err
			}
			conclusion := schema.Properties["conclusion"]
			if conclusion == nil || !slices.Equal(conclusion.Enum, []any{"feasible", "infeasible", "needs_input", "needs_attention"}) {
				return nil, errors.New("actual report schema does not enumerate the four business conclusions")
			}
			if conclusion.Description == "" || schema.Properties["claims"].Description == "" || schema.Properties["evidence"].Description == "" {
				return nil, errors.New("actual report schema lacks meaning/provenance descriptions")
			}
			valid := ReportDraft{Conclusion: "feasible", Summary: "Validate actual schema and dispatcher boundary.", Claims: b.RequiredFacts, DecisionIDs: []string{"direct-only", "family-count"}, Evidence: b.Evidence[0].ID}
			args := map[string]any{}
			_ = jsonUnmarshal(encode(valid), &args)
			args["conclusion"] = "approved"
			_, err := tool.Invoke(ctx, &interfaces.ToolInput{Args: args})
			var argErr *tools.ArgumentError
			if !errors.As(err, &argErr) {
				return nil, fmt.Errorf("invalid conclusion bypassed actual Dispatcher schema validation: %v", err)
			}
			probes++
			break
		}
		return (&scriptedModel{role: b.Role}).CompleteToolsStream(ctx, r, offered)
	}}
	result, err := Run(context.Background(), Config{Mode: "pipeline", Dir: t.TempDir(), Client: client}, DefaultTask())
	if err != nil {
		t.Fatal(err)
	}
	if probes != 4 || len(result.Accepted) != 2 {
		t.Fatalf("actual schema probes or acceptance missing: probes=%d accepted=%d", probes, len(result.Accepted))
	}
}
func TestActualDispatcherAcceptsReportWithoutOptionalLists(t *testing.T) {
	client := &transportFunc{scriptedModel: &scriptedModel{}, fn: func(ctx context.Context, r *llm.CompletionRequest, offered []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
		var b Brief
		for _, m := range r.Messages {
			if m.Role == "user" {
				_ = jsonUnmarshal([]byte(m.Content), &b)
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
				for _, name := range []string{"Assumptions", "MissingInputs", "Conflicts", "assumptions", "missing_inputs", "conflicts"} {
					delete(args, name)
				}
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
		t.Fatalf("optional-list omission lost acceptance: %d", len(result.Accepted))
	}
}
func TestActualParentAndChildRequestsHaveExplicitCaseOutputCap(t *testing.T) {
	for _, mode := range []string{"delegation", "pipeline", "parallel"} {
		t.Run(mode, func(t *testing.T) {
			client := &transportFunc{scriptedModel: &scriptedModel{}, fn: func(ctx context.Context, r *llm.CompletionRequest, offered []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
				if r.MaxTokens != 8192 {
					return nil, fmt.Errorf("actual Builder request output cap=%d, want8192", r.MaxTokens)
				}
				role := "parent"
				for _, m := range r.Messages {
					if m.Role == "user" {
						var b Brief
						if jsonUnmarshal([]byte(m.Content), &b) == nil && b.Role != "" {
							role = b.Role
						}
					}
				}
				return (&scriptedModel{role: role}).CompleteToolsStream(ctx, r, offered)
			}}
			result, err := Run(context.Background(), Config{Mode: mode, Dir: t.TempDir(), Client: client}, DefaultTask())
			if err != nil {
				t.Fatal(err)
			}
			for _, capture := range result.Inputs {
				if capture.MaxTokens != 8192 {
					t.Fatalf("captured %s request cap=%d", capture.Role, capture.MaxTokens)
				}
			}
			if result.Requests != 4 {
				t.Fatalf("normal request count: %d", result.Requests)
			}
		})
	}
}

func TestInvalidConclusionDiagnosticIsBoundedAndDoesNotDumpReport(t *testing.T) {
	h := setupHost(t)
	e, r := prepared(t, h, "itinerary", nil)
	r.Conclusion = "approved"
	_, err := h.accept(e, "invalid-conclusion", r)
	if err == nil || !contains(err.Error(), `"approved"`) {
		t.Fatalf("missing bounded conclusion diagnostic: %v", err)
	}
	r.Conclusion = string(make([]byte, 300)) + h.task.PrivateNote
	r.Summary = h.task.PrivateNote
	_, err = h.accept(e, "long-invalid-conclusion", r)
	if err == nil || len(err.Error()) > 400 || contains(err.Error(), h.task.PrivateNote) {
		t.Fatal("conclusion diagnostic exposed long value or unrelated report content")
	}
}
