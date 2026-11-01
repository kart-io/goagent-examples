package main

import (
	"context"
	"os"
	"sync"
	"testing"
)

func setupHost(t *testing.T) *host {
	t.Helper()
	h, err := newHost(Config{Dir: t.TempDir()}, DefaultTask())
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func prepared(t *testing.T, h *host, role string, deps []Dependency) (Envelope, Report) {
	t.Helper()
	b, err := h.brief(role, deps)
	if err != nil {
		t.Fatal(err)
	}
	e, err := h.register(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	r := Report{Conclusion: "feasible", Summary: "The synthetic round trip meets the confirmed constraints.", Claims: h.task.Candidate, DecisionIDs: []string{"direct-only", "family-count"}, Evidence: []ArtifactRef{h.evidence}}
	return e, r
}
func TestBriefProjectionAndDeepCopy(t *testing.T) {
	h := setupHost(t)
	b, err := h.brief("itinerary", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(encode(b))
	if contains(s, h.task.PrivateNote) {
		t.Fatal("private parent data leaked")
	}
	for _, required := range []string{"Why", "no transit", "direct-only", "family-count", "Excluded", "Completion", "Evidence", "SGD"} {
		if !contains(s, required) {
			t.Errorf("insufficient brief: missing %q", required)
		}
	}
	b.HardConstraints[0] = "mutated"
	b.RequiredFacts.Legs[0].From = "XXX"
	next, _ := h.brief("itinerary", nil)
	if next.HardConstraints[0] == "mutated" || next.RequiredFacts.Legs[0].From == "XXX" {
		t.Fatal("projection aliases host state")
	}
}
func TestEvidenceMeaningMustSupportClaims(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Report)
	}{
		{"adult-count", func(r *Report) { r.Claims.Adults = 2 }},
		{"child-count", func(r *Report) { r.Claims.Children = 0 }},
		{"per-person-is-not-total", func(r *Report) { r.Claims.TotalMinor = 35000 }},
		{"currency", func(r *Report) { r.Claims.Currency = "USD" }},
		{"missing-return", func(r *Report) { r.Claims.Legs = r.Claims.Legs[:1] }},
		{"wrong-date", func(r *Report) { r.Claims.Legs[0].Date = "2027-01-01" }},
		{"wrong-airport", func(r *Report) { r.Claims.Legs[1].To = "KUL" }},
		{"baggage", func(r *Report) { r.Claims.BaggageKG = 30 }},
		{"baggage-basis", func(r *Report) { r.Claims.BaggageBasis = "whole party" }},
		{"refund-condition", func(r *Report) { r.Claims.Refundable = true }},
		{"change-condition", func(r *Report) { r.Claims.ChangeFeeMinor = 0 }},
		{"missing-decision", func(r *Report) { r.DecisionIDs = nil }},
		{"wrong-artifact-version", func(r *Report) { r.Evidence[0].Version++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setupHost(t)
			e, r := prepared(t, h, "itinerary", nil)
			tc.change(&r)
			if _, err := h.accept(e, "report", r); err == nil {
				t.Fatal("unsupported report accepted despite authentic evidence")
			}
		})
	}
	h := setupHost(t)
	e, r := prepared(t, h, "itinerary", nil)
	if err := os.Remove(h.path(h.evidence.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.accept(e, "report", r); err == nil {
		t.Fatal("unreadable artifact accepted")
	}
}
func TestAtomicAdoptionReplacementDedupAndHistory(t *testing.T) {
	h := setupHost(t)
	old, r := prepared(t, h, "itinerary", nil)
	newer, _ := prepared(t, h, "itinerary", nil)
	if _, err := h.accept(old, "old", r); err == nil {
		t.Fatal("replaced invocation accepted")
	}
	a, err := h.accept(newer, "same", r)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.accept(newer, "same", r)
	if err != nil || again.Digest != a.Digest {
		t.Fatal("identical retry did not return receipt")
	}
	r.Summary = "different payload"
	if _, err := h.accept(newer, "same", r); err == nil {
		t.Fatal("same ID with different payload accepted")
	}
	h.updateRevision(2)
	receipt, err := h.receipt("same")
	if err != nil || receipt.Digest != a.Digest {
		t.Fatal("historical receipt lost")
	}
	if h.current(a) {
		t.Fatal("old accepted receipt can advance new revision")
	}
}
func TestDependencyCandidateAndCancellationBinding(t *testing.T) {
	h := setupHost(t)
	e, r := prepared(t, h, "itinerary", nil)
	a, err := h.accept(e, "upstream", r)
	if err != nil {
		t.Fatal(err)
	}
	d := dependency(a)
	review, report := prepared(t, h, "budget", []Dependency{d})
	h.replaceCandidate("B")
	if _, err := h.accept(review, "review-A", report); err == nil {
		t.Fatal("review of A accepted for B")
	}
	h = setupHost(t)
	e, r = prepared(t, h, "itinerary", nil)
	h.cancelInvocation(e.InvocationID)
	if _, err := h.accept(e, "late", r); err == nil {
		t.Fatal("canceled invocation accepted")
	}
	h = setupHost(t)
	e, r = prepared(t, h, "itinerary", nil)
	a, err = h.accept(e, "upstream", r)
	if err != nil {
		t.Fatal(err)
	}
	review, report = prepared(t, h, "budget", []Dependency{dependency(a)})
	_, _ = prepared(t, h, "itinerary", nil)
	if _, err := h.accept(review, "stale-dependency", report); err == nil {
		t.Fatal("superseded upstream accepted")
	}
}
func TestAcceptanceAndVersionUpdateShareLock(t *testing.T) {
	for i := 0; i < 50; i++ {
		h := setupHost(t)
		e, r := prepared(t, h, "itinerary", nil)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = h.accept(e, "race", r) }()
		go func() { defer wg.Done(); h.updateRevision(2) }()
		wg.Wait()
		if a, err := h.receipt("race"); err == nil && h.current(a) {
			t.Fatal("old revision remains consumable")
		}
	}
}

func TestReportMustMatchBoundCandidateAndEveryConfirmedDecision(t *testing.T) {
	h := setupHost(t)
	h.task.Candidate.Adults = 2
	e, r := prepared(t, h, "itinerary", nil)
	r.Claims = h.truth
	if _, err := h.accept(e, "wrong-candidate-facts", r); err == nil {
		t.Fatal("authentic evidence for one adult was accepted for a two-adult candidate")
	}
	h = setupHost(t)
	h.task.ConfirmedDecisions = append(h.task.ConfirmedDecisions, "keep-dates: dates were explicitly confirmed")
	e, r = prepared(t, h, "itinerary", nil)
	if _, err := h.accept(e, "lost-decision", r); err == nil {
		t.Fatal("newly confirmed decision was silently dropped")
	}
}

func TestAdoptionReceiptsPreserveRejectedAndAcceptedHistory(t *testing.T) {
	h := setupHost(t)
	e, r := prepared(t, h, "itinerary", nil)
	r.Claims.TotalMinor = 35000
	if _, err := h.accept(e, "rejected", r); err == nil {
		t.Fatal("unsupported report accepted")
	}
	receipt, err := h.adoptionReceipt("rejected")
	if err != nil || receipt.Status != "rejected" || receipt.Reason == "" {
		t.Fatalf("rejection history missing: %+v %v", receipt, err)
	}
	r.Claims = h.task.Candidate
	if _, err := h.accept(e, "rejected", r); err == nil {
		t.Fatal("rejected report ID reused for a new payload")
	}
	a, err := h.accept(e, "accepted", r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(h.path(h.evidence.ID)); err != nil {
		t.Fatal(err)
	}
	replay, err := h.accept(e, "accepted", r)
	if err != nil || replay.Digest != a.Digest {
		t.Fatalf("same report retry lost historical receipt after artifact access changed: %v", err)
	}
}

func TestModelCannotInventConfirmedDecision(t *testing.T) {
	h := setupHost(t)
	e, r := prepared(t, h, "itinerary", nil)
	r.DecisionIDs = append(r.DecisionIDs, "payment-approved")
	if _, err := h.accept(e, "invented-authority", r); err == nil {
		t.Fatal("model invented decision was accepted as inherited host authority")
	}
}

func TestAuthorityRulesAndArtifactTaskBindingsCannotDrift(t *testing.T) {
	for _, name := range []string{"grants", "rules", "artifact-task", "artifact-revision", "artifact-candidate"} {
		t.Run(name, func(t *testing.T) {
			h := setupHost(t)
			e, r := prepared(t, h, "itinerary", nil)
			switch name {
			case "grants":
				h.mu.Lock()
				h.grantsVersion = "revoked"
				h.mu.Unlock()
			case "rules":
				h.mu.Lock()
				h.task.Candidate.RulesVersion = "changed"
				h.mu.Unlock()
			case "artifact-task":
				h.evidence.TaskID = "other-task"
				r.Evidence[0] = h.evidence
			case "artifact-revision":
				h.evidence.TaskRevision++
				r.Evidence[0] = h.evidence
			case "artifact-candidate":
				h.evidence.CandidateID = "B"
				r.Evidence[0] = h.evidence
			}
			if _, err := h.accept(e, "drift", r); err == nil {
				t.Fatal("changed authority/rules/evidence target was accepted")
			}
		})
	}
}
func TestUnjustifiedInfeasibilityIsNotEvidenceSupported(t *testing.T) {
	h := setupHost(t)
	e, r := prepared(t, h, "itinerary", nil)
	r.Conclusion = "infeasible"
	if _, err := h.accept(e, "unsupported-negative", r); err == nil {
		t.Fatal("unsupported negative conclusion accepted despite every hard constraint being met")
	}
}
