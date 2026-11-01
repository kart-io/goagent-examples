// agent-context demonstrates host-controlled context projection with real Agent loops.
package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

//go:embed fixtures/evidence.json
var fixture []byte

type Leg struct {
	From, To, Date string
	Direct         bool
}
type Candidate struct {
	CandidateID                                string
	Version                                    int
	Adults, Children                           int
	Currency                                   string
	AdultFareMinor, ChildFareMinor, TotalMinor int
	Legs                                       []Leg
	BaggagePieces, BaggageKG                   int
	BaggageBasis                               string
	Refundable                                 bool
	ChangeFeeMinor                             int
	ChangeCondition, RulesVersion              string
}
type Task struct {
	ID                                                      string
	Revision                                                int
	Goal, Why                                               string
	BudgetMinor                                             int
	Candidate                                               Candidate
	HardConstraints, ConfirmedDecisions, ExcludedDirections []string
	PrivateNote                                             string // parent-only: never encoded into a child brief or artifact
}

func DefaultTask() Task {
	var c Candidate
	if err := json.Unmarshal(fixture, &c); err != nil {
		panic(err)
	}
	return Task{ID: "synthetic-family-trip", Revision: 1, Goal: "Assess a family round trip SIN/NRT without booking", Why: "A parent and child need a predictable direct journey within the confirmed family budget.", BudgetMinor: 80000, Candidate: c,
		HardConstraints:    []string{"no transit: both legs must be direct", "one adult and one child; round trip", "SGD 80000 minor units total budget", "one checked bag of at least 20kg per traveler", "do not book, pay, issue tickets or modify business resources"},
		ConfirmedDecisions: []string{"direct-only: user confirmed no transit", "family-count: user confirmed one adult and one child"},
		ExcludedDirections: []string{"connecting routes excluded because the child cannot transit", "unverified promotional prices excluded"}, PrivateNote: "PARENT_PRIVATE_PAYMENT_TOKEN_DO_NOT_FORWARD"}
}

// ArtifactRef is a host-issued, scoped capability for the read_evidence tool.
// Locator is an opaque ID, never a model-controlled filesystem path.
type ArtifactRef struct {
	ID, Locator, MediaType, Schema, Digest, TaskID, InvocationID, Source, CandidateID, Scope string
	Version, TaskRevision, Size                                                              int
}
type Dependency struct {
	ReportID, ReportDigest, InvocationID, CandidateID, CandidateDigest string
	CandidateVersion                                                   int
	DecisionIDs                                                        []string
	Artifact                                                           ArtifactRef
}

// Brief contains model data only. Execution identities and grants stay in Envelope.
type Brief struct {
	UpstreamReports                                          []Report
	Role, Goal, Why                                          string
	HardConstraints                                          []string
	RequiredFacts                                            Candidate
	BudgetMinor                                              int
	ConfirmedDecisions, ExcludedDirections, WorkingJudgments []string
	Dependencies                                             []Dependency
	Evidence                                                 []ArtifactRef
	OutputContract, CompletionCriteria, CommonRules          string
	MissingInputs                                            []string
}
type Envelope struct {
	TaskID, RootRunID, StepID, InvocationID, AgentID, SessionID            string
	TaskRevision, InvocationEpoch                                          int
	CandidateID, CandidateDigest                                           string
	CandidateVersion                                                       int
	BriefDigest, ContextPolicy, ContextDigest, RulesVersion, GrantsVersion string
	Dependencies                                                           []Dependency
}
type Report struct {
	Conclusion    string        `json:"conclusion"`
	Summary       string        `json:"summary"`
	Claims        Candidate     `json:"claims"`
	DecisionIDs   []string      `json:"decision_ids"`
	Evidence      []ArtifactRef `json:"evidence"`
	Assumptions   []string      `json:"Assumptions,omitempty"`
	MissingInputs []string      `json:"MissingInputs,omitempty"`
	Conflicts     []string      `json:"Conflicts,omitempty"`
}

// ReportDraft is the model-facing submission. The host resolves the citation
// to a full immutable ArtifactRef; model text cannot supply authority metadata.
type ReportDraft struct {
	Conclusion    string    `json:"conclusion"`
	Summary       string    `json:"summary"`
	Claims        Candidate `json:"claims"`
	DecisionIDs   []string  `json:"decision_ids"`
	Evidence      string    `json:"evidence"`
	Assumptions   []string  `json:"Assumptions,omitempty"`
	MissingInputs []string  `json:"MissingInputs,omitempty"`
	Conflicts     []string  `json:"Conflicts,omitempty"`
}
type Accepted struct {
	ReportID, Digest                string
	Envelope                        Envelope
	Report                          Report
	Artifact                        ArtifactRef
	AdoptionStatus, ExecutionStatus string
}

func dependency(a Accepted) Dependency {
	return Dependency{ReportID: a.ReportID, ReportDigest: a.Digest, InvocationID: a.Envelope.InvocationID, CandidateID: a.Envelope.CandidateID, CandidateVersion: a.Envelope.CandidateVersion, CandidateDigest: a.Envelope.CandidateDigest, DecisionIDs: slices.Clone(a.Report.DecisionIDs), Artifact: a.Artifact}
}
func encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func digest(v any) string         { sum := sha256.Sum256(encode(v)); return hex.EncodeToString(sum[:]) }
func bytesDigest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func clone[T any](v T) T {
	var out T
	if err := json.Unmarshal(encode(v), &out); err != nil {
		panic(err)
	}
	return out
}
func contains(s, sub string) bool { return strings.Contains(s, sub) }
func decodeReportDraft(args map[string]any) (ReportDraft, error) {
	var r ReportDraft
	dec := json.NewDecoder(bytes.NewReader(encode(args)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("report schema: %w", err)
	}
	return r, nil
}

// AdoptionReceipt is immutable history, including rejected submissions. Looking it
// up never grants eligibility to advance a newer task or invocation.
type AdoptionReceipt struct {
	ReportID, Digest, Status, Reason string
	Envelope                         Envelope
}
type ExecutionRecord struct{ InvocationID, Role, SessionID, ExecutionStatus, Reason string }
