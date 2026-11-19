package db

import (
	"fmt"
	"path/filepath"
	"testing"
)

// All fixtures use isolated temporary databases, never the running task database.
func causalLineageFixture(t *testing.T) *ExplorationStore {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "causal-lineage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("lineage", "causal scope", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return d.Exploration(task.ExplorationID)
}

func causalLineageNode(t *testing.T, s *ExplorationStore, kind string) int64 {
	t.Helper()
	state := "confirmed"
	switch kind {
	case KindIntent:
		state = "done"
	case KindDigest:
		state = "active"
	}
	id, err := s.AddNode(kind, map[string]any{"summary": kind}, 5, state, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func causalLineageLink(t *testing.T, s *ExplorationStore, src int64, rel string, dst int64) {
	t.Helper()
	if err := s.Link(src, rel, dst); err != nil {
		t.Fatal(err)
	}
}

func causalLineageActivity(t *testing.T, s *ExplorationStore, a Activity) int64 {
	t.Helper()
	id, err := s.AppendActivity(a)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func assertCausalLineage(t *testing.T, s *ExplorationStore, target int64, want []int64, edgeCount int) {
	t.Helper()
	nodes, edges, err := s.FindingLineage(target)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	for _, n := range nodes {
		ids[n.ID] = true
	}
	if len(ids) != len(want) {
		t.Fatalf("lineage nodes=%v want=%v", ids, want)
	}
	for _, id := range want {
		if !ids[id] {
			t.Fatalf("missing causal node %d in %v", id, ids)
		}
	}
	if len(edges) != edgeCount {
		t.Fatalf("lineage edges=%+v want count=%d", edges, edgeCount)
	}
	for _, edge := range edges {
		if edge.Rel == RelCovers || !ids[edge.From] || !ids[edge.To] {
			t.Fatalf("noncausal or out-of-scope edge %+v", edge)
		}
	}
}

func TestFindingLineageCoversCannotImportUnrelatedAncestorsOrSessions(t *testing.T) {
	s := causalLineageFixture(t)
	producer := causalLineageNode(t, s, KindIntent)
	finding := causalLineageNode(t, s, KindFinding)
	unrelated := causalLineageNode(t, s, KindIntent)
	unrelatedFact := causalLineageNode(t, s, KindFact)
	digest := causalLineageNode(t, s, KindDigest)
	causalLineageLink(t, s, producer, RelYields, finding)
	causalLineageLink(t, s, unrelated, RelYields, unrelatedFact)
	causalLineageLink(t, s, unrelatedFact, RelDerivedFrom, digest)
	causalLineageLink(t, s, digest, RelCovers, producer)
	causalLineageLink(t, s, digest, RelCovers, unrelatedFact)
	produced := causalLineageActivity(t, s, Activity{NodeID: &producer, Worker: "work#1", Kind: "tool_result", NodeSources: []NodeSource{{NodeID: finding, Relation: "produced"}}})
	causalLineageActivity(t, s, Activity{NodeID: &unrelated, Worker: "work#1", Kind: "tool_result", NodeSources: []NodeSource{{NodeID: unrelatedFact, Relation: "produced"}}})
	seg := 2
	causalLineageActivity(t, s, Activity{Worker: "mainagent", MainSeg: &seg, Kind: "tool_result", NodeSources: []NodeSource{{NodeID: digest, Relation: "created"}}})

	assertCausalLineage(t, s, finding, []int64{producer, finding}, 1)
	sessions, err := s.FindingLineageSessions(finding)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Session != fmt.Sprint("intent:", producer) {
		t.Fatalf("compression imported unrelated sessions: %+v", sessions)
	}
	seenProduced, seenExecuted := false, false
	for _, c := range sessions[0].Contributions {
		switch {
		case c.NodeID == fmt.Sprint(finding) && c.Relation == "produced" && c.Precision == "event" && c.Seq == produced:
			seenProduced = true
		case c.NodeID == fmt.Sprint(producer) && c.Relation == "executed" && c.Precision == "session":
			seenExecuted = true
		default:
			t.Fatalf("out-of-scope contribution: %+v", c)
		}
	}
	if !seenProduced || !seenExecuted {
		t.Fatalf("lost genuine contributions: %+v", sessions)
	}
}

func TestFindingLineageRetainsExplicitCausalDigestButNotCoversEdges(t *testing.T) {
	s := causalLineageFixture(t)
	precursor := causalLineageNode(t, s, KindIntent)
	fact := causalLineageNode(t, s, KindFact)
	digest := causalLineageNode(t, s, KindDigest)
	executor := causalLineageNode(t, s, KindIntent)
	finding := causalLineageNode(t, s, KindFinding)
	causalLineageLink(t, s, precursor, RelYields, fact)
	causalLineageLink(t, s, fact, RelDerivedFrom, digest)
	causalLineageLink(t, s, digest, RelDerivedFrom, executor)
	causalLineageLink(t, s, executor, RelYields, finding)
	// Both endpoints are causal ancestors. The structural edge must still not
	// reappear in the returned lineage or introduce a false backwards cycle.
	causalLineageLink(t, s, digest, RelCovers, fact)
	causalLineageActivity(t, s, Activity{NodeID: &precursor, Worker: "work#1", Kind: "tool_result", NodeSources: []NodeSource{{NodeID: fact, Relation: "produced"}}})
	seg := 0
	created := causalLineageActivity(t, s, Activity{Worker: "mainagent", MainSeg: &seg, Kind: "tool_result", NodeSources: []NodeSource{{NodeID: digest, Relation: "created"}}})
	causalLineageActivity(t, s, Activity{NodeID: &executor, Worker: "work#2", Kind: "tool_result", NodeSources: []NodeSource{{NodeID: finding, Relation: "produced"}}})

	assertCausalLineage(t, s, finding, []int64{precursor, fact, digest, executor, finding}, 4)
	sessions, err := s.FindingLineageSessions(finding)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 3 {
		t.Fatalf("lost causal digest session: %+v", sessions)
	}
	for _, session := range sessions {
		if session.Session != "main:0" {
			continue
		}
		if len(session.Contributions) != 1 {
			t.Fatalf("digest contributions=%+v", session.Contributions)
		}
		c := session.Contributions[0]
		if c.NodeID == fmt.Sprint(digest) && c.Relation == "created" && c.Precision == "event" && c.Seq == created {
			return
		}
		t.Fatalf("wrong causal digest source: %+v", c)
	}
	t.Fatal("missing explicit causal digest source session")
}

func TestFindingLineageEarlierFindingDoesNotIncludeDownstreamIntents(t *testing.T) {
	s := causalLineageFixture(t)
	producer := causalLineageNode(t, s, KindIntent) // analogue of intent 124
	earlier := causalLineageNode(t, s, KindFinding) // analogue of finding 126
	middle := causalLineageNode(t, s, KindIntent)   // analogue of intent 161
	fact := causalLineageNode(t, s, KindFact)
	last := causalLineageNode(t, s, KindIntent) // analogue of intent 168
	final := causalLineageNode(t, s, KindFinding)
	digest := causalLineageNode(t, s, KindDigest)
	causalLineageLink(t, s, producer, RelYields, earlier)
	causalLineageLink(t, s, earlier, RelDerivedFrom, middle)
	causalLineageLink(t, s, middle, RelYields, fact)
	causalLineageLink(t, s, fact, RelDerivedFrom, last)
	causalLineageLink(t, s, last, RelYields, final)
	for _, id := range []int64{producer, middle, fact, last} {
		causalLineageLink(t, s, digest, RelCovers, id)
	}
	for _, id := range []int64{producer, middle, last} {
		intentID := id
		causalLineageActivity(t, s, Activity{NodeID: &intentID, Worker: "work#1", Kind: "result"})
	}

	assertCausalLineage(t, s, earlier, []int64{producer, earlier}, 1)
	assertCausalLineage(t, s, final, []int64{producer, earlier, middle, fact, last, final}, 5)
	sessions, err := s.FindingLineageSessions(earlier)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Session != fmt.Sprint("intent:", producer) {
		t.Fatalf("earlier finding imported downstream sessions: %+v", sessions)
	}
	for _, c := range sessions[0].Contributions {
		if c.NodeID != fmt.Sprint(producer) && c.NodeID != fmt.Sprint(earlier) {
			t.Fatalf("earlier finding imported downstream contribution: %+v", c)
		}
	}
	finalSessions, err := s.FindingLineageSessions(final)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalSessions) != 3 {
		t.Fatalf("final finding lost genuine upstream sessions: %+v", finalSessions)
	}
}
