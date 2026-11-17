package reorder

import (
	"path/filepath"
	"testing"
	"time"
)

type capturedEvent struct {
	kind    string
	payload any
}

func captureEvents(e *Engine) *[]capturedEvent {
	var events []capturedEvent
	e.SetEventSink(func(kind string, payload any) {
		events = append(events, capturedEvent{kind, payload})
	})
	return &events
}

func windowEnds(events []capturedEvent) []WindowEndEvent {
	var out []WindowEndEvent
	for _, ev := range events {
		if w, ok := ev.payload.(WindowEndEvent); ok {
			out = append(out, w)
		}
	}
	return out
}

func accountStats(t *testing.T, st *Stats, id string) AccountStats {
	t.Helper()
	for _, a := range st.Accounts {
		if a.AuthID == id {
			return a
		}
	}
	t.Fatalf("no stats for %s in %+v", id, st.Accounts)
	return AccountStats{}
}

func TestPickEventsOnlyForBindingChanges(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	events := captureEvents(e)
	e.Pick(PickInput{SessionID: "s1", Model: "gpt-x", Candidates: cands("A", "B", "C", "D")})
	pick(t, e, "s1", "A", "B", "C", "D") // sticky
	e.Pick(PickInput{Candidates: cands("A", "B")})
	if len(*events) != 1 || (*events)[0].kind != EventPick {
		t.Fatalf("want exactly one pick event, got %+v", *events)
	}
	ev := (*events)[0].payload.(PickEvent)
	if ev.Kind != KindNew || ev.Model != "gpt-x" || ev.Session == "" || ev.Reason == "" || len(ev.Top) != topEntries || ev.Top[0].AuthID != ev.AuthID {
		t.Fatalf("pick event: %+v", ev)
	}

	st := e.TakeStats()
	if st == nil || st.Picks != 3 || st.Kinds[KindNew] != 1 || st.Kinds[KindSticky] != 1 || st.Kinds[KindNoSession] != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestStatsCountFailuresAndPeak(t *testing.T) {
	e, c := newTestEngine(t, nil)
	pick(t, e, "s1", "A")
	pick(t, e, "s2", "A")
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: "A", Failed: true, StatusCode: 429, At: c.Now()})
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: "A", Failed: true, At: c.Now()})
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: "A", At: c.Now()})

	st := e.TakeStats()
	a := accountStats(t, st, "A")
	if a.Active != 2 || a.Peak != 2 || a.Picks != 2 || a.New != 2 || a.OK != 1 || a.Failures["429"] != 1 || a.Failures["error"] != 1 {
		t.Fatalf("account A: %+v", a)
	}
	if st.Usage != 3 || st.Failures["429"] != 1 || st.ActiveSessions != 2 {
		t.Fatalf("stats: %+v", st)
	}

	// The next interval's peak starts at the sessions still active.
	c.Advance(time.Minute)
	pick(t, e, "s1", "A")
	if a := accountStats(t, e.TakeStats(), "A"); a.Peak != 2 || a.New != 0 || a.Picks != 1 {
		t.Fatalf("second interval: %+v", a)
	}

	// Idle intervals produce no stats.
	c.Advance(time.Hour)
	if st := e.TakeStats(); st != nil {
		t.Fatalf("idle interval: %+v", st)
	}
}

func TestWindowEndReportedOnceAfterReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e, c := newTestEngine(t, func(cfg *Config) {
		cfg.PlanMultipliers = map[string]float64{"A": 10}
		cfg.StatePath = path
	})
	events := captureEvents(e)
	seedWeekly(e, "A", t0.Add(2*time.Hour), 70)
	e.TakeStats()
	if len(windowEnds(*events)) != 0 {
		t.Fatalf("window still running, got %+v", *events)
	}

	c.Advance(3 * time.Hour)
	e.TakeStats()
	ends := windowEnds(*events)
	if len(ends) != 1 {
		t.Fatalf("want one window end, got %+v", *events)
	}
	if w := ends[0]; w.AuthID != "A" || w.Early || w.UsedPercent != 70 || w.Unused != 300 || !w.ResetAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("window end: %+v", w)
	}

	// Not repeated by later ticks, by a late record of the same window whose
	// reset drifted a few seconds, or after a restart.
	e.TakeStats()
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: "A", Headers: weeklyHeaders(t0.Add(2*time.Hour+5*time.Second), 71), At: c.Now()})
	e.TakeStats()
	if err := e.SaveState(); err != nil {
		t.Fatal(err)
	}
	e2 := NewEngine(e.Config())
	e2.SetClock(c.Now)
	if err := e2.LoadState(); err != nil {
		t.Fatal(err)
	}
	restarted := captureEvents(e2)
	e2.TakeStats()
	if n := len(windowEnds(*events)) + len(windowEnds(*restarted)); n != 1 {
		t.Fatalf("window end repeated: %+v %+v", *events, *restarted)
	}

	// The next window is reported when it ends in turn.
	seedWeekly(e2, "A", c.Now().Add(weekLength), 0)
	if len(windowEnds(*restarted)) != 0 {
		t.Fatalf("new window must not report the old one again: %+v", *restarted)
	}
}

func TestEarlyResetReportedWhenNewWindowReplacesOld(t *testing.T) {
	e, c := newTestEngine(t, nil)
	events := captureEvents(e)
	seedWeekly(e, "A", t0.Add(3*24*time.Hour), 40)
	c.Advance(time.Hour)
	seedWeekly(e, "A", c.Now().Add(weekLength), 0)
	ends := windowEnds(*events)
	if len(ends) != 1 || !ends[0].Early || ends[0].UsedPercent != 40 || ends[0].Unused != 60 {
		t.Fatalf("early reset: %+v", *events)
	}
}

// Compact stats lines list only the accounts active in the interval; full
// lines list every account.
func TestCompactStatsListOnlyActiveAccounts(t *testing.T) {
	e, c := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(24*time.Hour), 10)
	seedWeekly(e, "B", t0.Add(48*time.Hour), 10)
	e.TakeStats()
	c.Advance(time.Minute)
	pick(t, e, "S1", "A")
	st := e.TakeStatsMode(false)
	if st == nil || st.Full || len(st.Accounts) != 1 || st.Accounts[0].AuthID != "A" {
		t.Fatalf("compact = %+v", st)
	}
	c.Advance(time.Minute)
	pick(t, e, "S1", "A")
	st = e.TakeStatsMode(true)
	if st == nil || !st.Full || len(st.Accounts) != 2 {
		t.Fatalf("full = %+v", st)
	}
	// DiscardStats drops the interval without a line.
	c.Advance(time.Minute)
	pick(t, e, "S1", "A")
	e.DiscardStats()
	if st := e.TakeStats(); st != nil {
		t.Fatalf("after discard = %+v", st)
	}
}
