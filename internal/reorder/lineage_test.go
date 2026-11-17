package reorder

import "testing"

func TestLineageLoggedForNewSessions(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	events := captureEvents(e)
	e.Pick(PickInput{SessionID: "root", Candidates: cands("A", "B", "C")})
	e.Pick(PickInput{SessionID: "child", Candidates: cands("A", "B", "C"),
		Lineage: Lineage{ParentSessionID: "root", Subagent: "memory"}})
	e.Pick(PickInput{SessionID: "grandchild", Candidates: cands("A", "B", "C"),
		Lineage: Lineage{ParentSessionID: "child", SubagentKind: "thread_spawn", AgentName: "worker"}})
	e.Pick(PickInput{SessionID: "orphan", Candidates: cands("A", "B", "C"),
		Lineage: Lineage{ParentSessionID: "gone"}})

	var picks []PickEvent
	for _, ev := range *events {
		if p, ok := ev.payload.(PickEvent); ok {
			picks = append(picks, p)
		}
	}
	if len(picks) != 4 || picks[0].Lineage != nil {
		t.Fatalf("root session must carry no lineage: %+v", picks)
	}
	root := picks[0]
	if l := picks[1].Lineage; l == nil || l.Parent != root.Session || !l.ParentKnown || l.ParentAuth != root.AuthID || l.Ancestors != 1 || l.Subagent != "memory" {
		t.Fatalf("child lineage: %+v", picks[1].Lineage)
	}
	if l := picks[2].Lineage; l == nil || l.Parent != picks[1].Session || l.Ancestors != 2 || l.SubagentKind != "thread_spawn" || l.AgentName != "worker" {
		t.Fatalf("grandchild lineage: %+v", picks[2].Lineage)
	}
	if l := picks[3].Lineage; l == nil || l.ParentKnown || l.Ancestors != 0 || l.Parent == "" {
		t.Fatalf("unknown parent: %+v", picks[3].Lineage)
	}
	st := e.TakeStats()
	if st.NewWithParent != 3 || st.NewParentKnown != 2 || st.NewSubagent != 2 {
		t.Fatalf("lineage counters: %+v", st)
	}
	for _, s := range e.Snapshot(nil, nil).Sessions {
		if s.Session == picks[1].Session && s.Parent != root.Session {
			t.Fatalf("session view parent: %+v", s)
		}
	}
}
