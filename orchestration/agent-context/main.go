package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/kart-io/goagent-examples/internal/livemodel"
)

// ReportView shows what the child returned and what the host accepted. It omits
// the trusted execution envelope, captured prompts and parent-private task data.
type ReportView struct {
	ReportID, Role, ExecutionStatus, AdoptionStatus string
	Report                                          Report
	Artifact                                        ArtifactRef
}

func reportView(a Accepted) ReportView {
	return ReportView{ReportID: a.ReportID, Role: a.Envelope.StepID, ExecutionStatus: a.ExecutionStatus, AdoptionStatus: a.AdoptionStatus, Report: clone(a.Report), Artifact: a.Artifact}
}
func main() {
	cfg := Config{}
	var live bool
	flag.StringVar(&cfg.Mode, "mode", "delegation", "delegation, pipeline or parallel")
	flag.StringVar(&cfg.Dir, "dir", "", "host artifact/session/usage directory (default: temporary directory)")
	flag.BoolVar(&live, "live", false, "opt in to real model access using GOAGENT_LIVE_PROVIDER and provider environment")
	flag.Parse()
	if cfg.Dir == "" {
		dir, err := os.MkdirTemp("", "goagent-agent-context-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		cfg.Dir = dir
	}
	if live {
		c, err := livemodel.FromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, livemodel.Redact(err.Error()))
			os.Exit(1)
		}
		cfg.Client = c
	}
	result, err := Run(context.Background(), cfg, DefaultTask())
	safe := struct {
		Mode, Dir, ExecutionStatus                                             string
		Reports                                                                []ReportView
		Requests, Peak, InFlight, ChildrenClosed, ChildrenCreated, KnownTokens int
	}{Mode: result.Mode, Dir: result.Dir, ExecutionStatus: "succeeded", Requests: result.Requests, Peak: result.Peak, InFlight: result.InFlight, ChildrenClosed: result.ChildrenClosed, ChildrenCreated: result.ChildrenCreated, KnownTokens: result.Usage.Root.KnownTokens.TotalTokens}
	for _, a := range result.Accepted {
		safe.Reports = append(safe.Reports, reportView(a))
	}
	if err != nil {
		safe.ExecutionStatus = "failed"
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(safe)
	if err != nil {
		fmt.Fprintln(os.Stderr, livemodel.Redact(err.Error()))
		os.Exit(1)
	}
}
