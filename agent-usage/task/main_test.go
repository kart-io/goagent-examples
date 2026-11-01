package main

import "testing"

func TestTaskUsage(t *testing.T) {
	r := run()
	if r.Root.KnownTokens.PromptTokens != 12 || r.Root.KnownTokens.CompletionTokens != 6 || r.Root.KnownTokens.TotalTokens != 18 || !r.Complete {
		t.Fatalf("unexpected task accounting: %+v", r)
	}
	if r.Root.ReservedRequests != 5 || r.Root.ReservedToolCalls != 3 {
		t.Fatalf("execution not fully counted: %+v", r.Root)
	}
	totals := map[string]int{}
	for _, a := range r.Agents {
		totals[a.AgentID] = a.Self.KnownTokens.TotalTokens
	}
	if totals["coordinator"] != 9 || totals["researcher"] != 4 || totals["reviewer"] != 5 {
		t.Fatalf("incorrect ownership: %v", totals)
	}
	for _, c := range r.Calls {
		if c.AgentID == "coordinator" && c.Inclusive.KnownTokens.TotalTokens != 18 {
			t.Fatalf("incorrect subtree: %+v", c)
		}
	}
}
