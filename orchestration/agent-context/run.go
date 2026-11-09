package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kart-io/goagent/constants"
	"github.com/kart-io/goagent/core"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/session"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/workflow"
)

type Config struct {
	Mode, Dir   string
	Client      llm.ToolStreamClient
	MaxRequests int
}
type Result struct {
	Executions                                                             []ExecutionRecord
	Receipts                                                               []AdoptionReceipt
	Mode, Dir, Explanation                                                 string
	Accepted                                                               []Accepted
	Requests, Peak, InFlight, ChildrenCreated, ChildrenClosed, UsageEvents int
	Inputs                                                                 []InputCapture
	Identities                                                             []Identity
	Usage                                                                  budget.AttributionReport
	Logs                                                                   session.Store `json:"-"`
}
type tripState struct {
	Briefs  map[string]Brief
	Reports []Accepted
}

func (h *host) graph(ctx context.Context, parallel bool) ([]Accepted, error) {
	node := core.NewRunnableFunc(func(ctx context.Context, b Brief) (Accepted, error) { return h.execute(ctx, b) })
	adapt := func(role string) (*core.StreamRunnable[tripState, tripState], error) {
		return workflow.Adapt(node, func(s tripState) (Brief, error) {
			if role == "budget" {
				if len(s.Reports) != 1 || s.Reports[0].Report.Conclusion != "feasible" || !h.current(s.Reports[0]) {
					return Brief{}, fmt.Errorf("pipeline cannot advance without a current feasible itinerary")
				}
				return h.brief(role, []Dependency{dependency(s.Reports[0])})
			}
			return clone(s.Briefs[role]), nil
		}, func(s tripState, a Accepted) (tripState, error) { s.Reports = append(s.Reports, a); return s, nil })
	}
	g := workflow.NewGraph[tripState]()
	s := tripState{Briefs: map[string]Brief{}}
	roles := []string{"itinerary"}
	if parallel {
		roles = append(roles, "rules")
	}
	for _, role := range roles {
		b, err := h.brief(role, nil)
		if err != nil {
			return nil, err
		}
		s.Briefs[role] = b
	}
	if parallel {
		var branches []workflow.Branch[tripState]
		for _, role := range roles {
			r, err := adapt(role)
			if err != nil {
				return nil, err
			}
			branches = append(branches, workflow.Branch[tripState]{Name: role, Run: r.Invoke})
		}
		err := g.AddParallel("experts", workflow.ParallelConfig[tripState]{Branches: branches, Clone: func(s tripState) tripState { return clone(s) }, Merge: func(_ context.Context, s tripState, out []tripState) (tripState, error) {
			// AddParallel supplies registration order and never calls Merge after execution failure.
			s.Reports = nil
			for _, branch := range out {
				if len(branch.Reports) != 1 {
					return s, fmt.Errorf("missing branch result")
				}
				a := branch.Reports[0]
				if !h.current(a) {
					return s, fmt.Errorf("branch superseded")
				}
				s.Reports = append(s.Reports, a)
			}
			if digest(s.Reports[0].Report.Claims) != digest(s.Reports[1].Report.Claims) || s.Reports[0].Report.Conclusion != s.Reports[1].Report.Conclusion {
				return s, fmt.Errorf("needs_attention: expert conflict")
			}
			return s, nil
		}})
		if err != nil {
			return nil, err
		}
		if err = g.AddEdge(constants.WorkflowStart, "experts"); err != nil {
			return nil, err
		}
		if err = g.AddEdge("experts", constants.WorkflowEnd); err != nil {
			return nil, err
		}
	} else {
		first, err := adapt("itinerary")
		if err != nil {
			return nil, err
		}
		second, err := adapt("budget")
		if err != nil {
			return nil, err
		}
		if err = g.AddRunnable("itinerary", first); err != nil {
			return nil, err
		}
		if err = g.AddRunnable("budget", second); err != nil {
			return nil, err
		}
		if err = g.AddBranch("itinerary", func(_ context.Context, s tripState) (string, error) {
			if len(s.Reports) != 1 || !h.current(s.Reports[0]) {
				return "", fmt.Errorf("no eligible itinerary")
			}
			if s.Reports[0].Report.Conclusion != "feasible" {
				return constants.WorkflowEnd, nil
			}
			return "budget", nil
		}, "budget", constants.WorkflowEnd); err != nil {
			return nil, err
		}
		for _, edge := range [][2]string{{constants.WorkflowStart, "itinerary"}, {"budget", constants.WorkflowEnd}} {
			if err = g.AddEdge(edge[0], edge[1]); err != nil {
				return nil, err
			}
		}
	}
	compiled, err := g.Compile(ctx)
	if err != nil {
		return nil, err
	}
	out, err := compiled.Invoke(ctx, s)
	return out.Reports, err
}

// Run executes actual Builder Agents; without Client only their LLM transport is scripted.
func Run(ctx context.Context, cfg Config, task Task) (result Result, err error) {
	if cfg.Mode == "" {
		cfg.Mode = "delegation"
	}
	if cfg.Mode != "delegation" && cfg.Mode != "pipeline" && cfg.Mode != "parallel" {
		return result, fmt.Errorf("mode must be delegation, pipeline or parallel")
	}
	if cfg.Dir == "" {
		return result, fmt.Errorf("-dir is required")
	}
	h, err := newHost(cfg, task)
	if err != nil {
		return result, err
	}
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	h.rootCtx = runCtx
	runCtx = budget.WithController(runCtx, h.controller)
	runCtx = budget.WithUsageHook(runCtx, func(_ context.Context, _ budget.UsageEvent) { h.mu.Lock(); h.usageEvents++; h.mu.Unlock() })
	defer func() {
		// Owned child cleanup cancels its relay. Root cancellation invalidates eligibility first.
		if err != nil {
			h.cancelTask()
		}
		cancel()
		cleanCtx, cleanCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cleanCancel()
		err = errors.Join(err, h.limiter.wait(cleanCtx))
		result.Mode = cfg.Mode
		result.Dir = h.dir
		result.Requests, result.InFlight, result.Peak = h.limiter.stats()
		h.mu.Lock()
		result.ChildrenCreated = h.created
		result.ChildrenClosed = h.closed
		result.Inputs = clone(h.inputs)
		result.Identities = clone(h.identities)
		result.UsageEvents = h.usageEvents
		for _, v := range h.records {
			result.Executions = append(result.Executions, clone(v))
		}
		for _, v := range h.adoptionReceipts {
			result.Receipts = append(result.Receipts, clone(v))
		}
		sort.Slice(result.Executions, func(i, j int) bool { return result.Executions[i].Role < result.Executions[j].Role })
		sort.Slice(result.Receipts, func(i, j int) bool { return result.Receipts[i].ReportID < result.Receipts[j].ReportID })
		h.mu.Unlock()
		result.Usage, _ = h.controller.Attribution(cleanCtx)
		result.Logs = h.logs
		if writeErr := os.WriteFile(filepath.Join(h.dir, "executions.json"), encode(result.Executions), 0600); writeErr != nil {
			err = errors.Join(err, writeErr)
		}
		if writeErr := os.WriteFile(filepath.Join(h.dir, "adoption-receipts.json"), encode(result.Receipts), 0600); writeErr != nil {
			err = errors.Join(err, writeErr)
		}
		// Observe one final ledger snapshot; hook snapshots are never added together.
		if writeErr := os.WriteFile(filepath.Join(h.dir, "usage.json"), encode(result.Usage), 0600); writeErr != nil {
			err = errors.Join(err, writeErr)
		}
	}()
	if err = runCtx.Err(); err != nil {
		return result, err
	}
	if cfg.Mode == "delegation" {
		result.Accepted, result.Explanation, err = h.delegation(runCtx)
	} else {
		result.Accepted, err = h.graph(runCtx, cfg.Mode == "parallel")
		result.Explanation = "Host-validated synthetic family trip reports; no booking or business write."
	}
	return result, err
}
