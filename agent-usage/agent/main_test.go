package main

import "testing"

func TestAgentUsage(t *testing.T) {
	for _, mode := range []string{"single", "child"} {
		t.Run(mode, func(t *testing.T) {
			r := run(mode)
			wantTotal := 10
			if mode == "child" {
				wantTotal = 17
			}
			if r.Root.KnownTokens.TotalTokens != wantTotal || !r.Complete {
				t.Fatalf("unexpected root: %+v", r)
			}
			found := false
			for _, c := range r.Calls {
				if c.AgentID != "writer" {
					continue
				}
				found = true
				if c.Self.KnownTokens.TotalTokens != 10 || c.Inclusive.KnownTokens.TotalTokens != 10 || (c.ParentAgentCallKey != "") != (mode == "child") {
					t.Fatalf("writer counted incorrectly: %+v", c)
				}
			}
			if !found {
				t.Fatal("missing writer attribution")
			}
		})
	}
}
