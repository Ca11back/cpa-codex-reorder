package reorder

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// usageBody mimics a wham/usage response: a 5h window (primary) and a weekly window (secondary).
func usageBody(shortUsed float64, shortResetAfter int64, weeklyUsed float64, weeklyResetAfter int64) string {
	return fmt.Sprintf(`{"plan_type":"team","rate_limit":{"allowed":true,"limit_reached":false,`+
		`"primary_window":{"used_percent":%g,"limit_window_seconds":18000,"reset_after_seconds":%d},`+
		`"secondary_window":{"used_percent":%g,"limit_window_seconds":604800,"reset_after_seconds":%d}}}`,
		shortUsed, shortResetAfter, weeklyUsed, weeklyResetAfter)
}

func mustParse(t *testing.T, body string, at time.Time) ProbeResult {
	t.Helper()
	res, err := ParseUsageResponse([]byte(body), at)
	if err != nil {
		t.Fatalf("parse %s: %v", body, err)
	}
	return res
}

func TestParseUsageResponse(t *testing.T) {
	res := mustParse(t, usageBody(12, 3600, 40, 2*86400), t0)
	if res.Weekly == nil || res.WeeklyIdle || res.Weekly.UsedPercent != 40 || res.Weekly.WindowMinutes != 10080 || !res.Weekly.ResetAt.Equal(t0.Add(48*time.Hour)) {
		t.Fatalf("weekly: %+v idle=%v", res.Weekly, res.WeeklyIdle)
	}
	if res.Short == nil || res.ShortIdle || res.Short.UsedPercent != 12 || !res.Short.ResetAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("short: %+v", res.Short)
	}

	// Not running: 0% used and a placeholder reset one full window ahead.
	idle := mustParse(t, usageBody(0, 18000, 0, 604800), t0)
	if !idle.WeeklyIdle || !idle.ShortIdle {
		t.Fatalf("idle windows not recognised: %+v", idle)
	}
	// 0% but the window has been running for a while: open.
	if r := mustParse(t, usageBody(0, 18000, 0, 604000), t0); r.WeeklyIdle {
		t.Fatalf("running window at 0%% taken as idle")
	}

	// reset_at only (number or RFC 3339), weekly reported as the primary window.
	atOnly := fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%d},"secondary_window":null}}`, t0.Add(weekLength).Unix())
	if r := mustParse(t, atOnly, t0); r.Weekly == nil || !r.WeeklyIdle || r.Short != nil {
		t.Fatalf("reset_at placeholder: %+v", r)
	}
	rfc := fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":604800,"reset_at":%q}}}`, t0.Add(30*time.Hour).Format(time.RFC3339))
	if r := mustParse(t, rfc, t0); r.Weekly == nil || r.WeeklyIdle || !r.Weekly.ResetAt.Equal(t0.Add(30*time.Hour)) {
		t.Fatalf("RFC 3339 reset_at: %+v", r)
	}

	for _, bad := range []string{
		`not json`,
		`{}`,
		`{"rate_limit":{}}`,
		`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"reset_after_seconds":5}}}`,
		`{"rate_limit":{"primary_window":{"used_percent":101,"limit_window_seconds":18000,"reset_after_seconds":5}}}`,
		`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000}}}`,
	} {
		if _, err := ParseUsageResponse([]byte(bad), t0); err == nil {
			t.Fatalf("expected error for %s", bad)
		}
	}
}

func weeklyState(t *testing.T, e *Engine, id string) AccountView {
	t.Helper()
	for _, a := range e.Snapshot(nil, nil).Accounts {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("account %s not found", id)
	return AccountView{}
}

func TestProbeFillsUnknownAccounts(t *testing.T) {
	e, c := newTestEngine(t, nil)
	events := captureEvents(e)
	// A has an open window per the query, B is confirmed idle, C stays unknown.
	e.ApplyProbe("A", mustParse(t, usageBody(10, 3600, 30, 86400), c.Now()), c.Now())
	e.ApplyProbe("B", mustParse(t, usageBody(0, 18000, 0, 604800), c.Now()), c.Now())
	if a := weeklyState(t, e, "A"); a.WeeklyState != WeeklyOpen || a.Weekly.UsedPercent != 30 || a.LastProbeAt.IsZero() {
		t.Fatalf("A: %+v", a)
	}
	if b := weeklyState(t, e, "B"); b.WeeklyState != WeeklyIdle || b.Weekly != nil {
		t.Fatalf("B: %+v", b)
	}
	if len(*events) != 2 || (*events)[0].kind != EventProbe {
		t.Fatalf("probe events: %+v", *events)
	}
	// unopened_first: the idle account wins over the open one, like an unknown one.
	ranking, _ := e.rankLocked([]string{"A", "B"}, map[string]int{}, c.Now())
	if ranking[0].AuthID != "B" || ranking[0].Opened {
		t.Fatalf("ranking: %+v", ranking)
	}

	// Assigning B opens it provisionally; the first response makes it open.
	pick(t, e, "S1", "B")
	if b := weeklyState(t, e, "B"); b.WeeklyState != WeeklyProvisional {
		t.Fatalf("after assignment: %+v", b)
	}
	seedWeekly(e, "B", c.Now().Add(weekLength), 1)
	if b := weeklyState(t, e, "B"); b.WeeklyState != WeeklyOpen {
		t.Fatalf("after response: %+v", b)
	}
}

func TestProbeNeverOverridesNewerData(t *testing.T) {
	e, c := newTestEngine(t, nil)
	events := captureEvents(e)
	sent := c.Now()
	c.Advance(time.Second)
	// A request reported data after the query was sent.
	seedWeekly(e, "A", c.Now().Add(20*time.Hour), 55)
	e.ApplyProbe("A", mustParse(t, usageBody(1, 3600, 50, 72000), sent), sent)
	if a := weeklyState(t, e, "A"); a.Weekly.UsedPercent != 55 {
		t.Fatalf("older query overwrote newer data: %+v", a.Weekly)
	}
	ev := (*events)[len(*events)-1].payload.(ProbeEvent)
	if len(ev.Ignored) == 0 || ev.Ignored[0] != "weekly" {
		t.Fatalf("ignored not reported: %+v", ev)
	}

	// An assignment after the query was sent keeps the account provisional.
	sent = c.Now()
	c.Advance(time.Second)
	pick(t, e, "S1", "B")
	e.ApplyProbe("B", mustParse(t, usageBody(0, 18000, 0, 604800), sent), sent)
	if b := weeklyState(t, e, "B"); b.WeeklyState != WeeklyProvisional {
		t.Fatalf("idle query undid a later assignment: %+v", b)
	}
}

func TestIdleProbeEndsStoredWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e, c := newTestEngine(t, func(cfg *Config) { cfg.StatePath = path })
	events := captureEvents(e)
	seedWeekly(e, "A", t0.Add(3*24*time.Hour), 35)
	c.Advance(time.Hour)
	// The query says no window is running: the stored one was reset early.
	e.ApplyProbe("A", mustParse(t, usageBody(0, 18000, 0, 604800), c.Now()), c.Now())
	ends := windowEnds(*events)
	if len(ends) != 1 || !ends[0].Early || ends[0].UsedPercent != 35 {
		t.Fatalf("window end: %+v", *events)
	}
	if a := weeklyState(t, e, "A"); a.WeeklyState != WeeklyIdle {
		t.Fatalf("A: %+v", a)
	}
	// The idle state survives a restart.
	if err := e.SaveState(); err != nil {
		t.Fatal(err)
	}
	e2 := NewEngine(e.Config())
	e2.SetClock(c.Now)
	if err := e2.LoadState(); err != nil {
		t.Fatal(err)
	}
	if a := weeklyState(t, e2, "A"); a.WeeklyState != WeeklyIdle || a.LastProbeAt.IsZero() {
		t.Fatalf("after restart: %+v", a)
	}
}

func TestProbeShortWindowAndErrors(t *testing.T) {
	e, c := newTestEngine(t, nil)
	// An exhausted 5h window, then a query showing the 5h window idle again.
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: "A", At: c.Now(), Headers: map[string][]string{
		"X-Codex-Primary-Used-Percent":        {"100"},
		"X-Codex-Primary-Window-Minutes":      {"300"},
		"X-Codex-Primary-Reset-After-Seconds": {"3600"},
	}})
	if !weeklyState(t, e, "A").Exhausted {
		t.Fatal("5h exhaustion not recorded")
	}
	c.Advance(time.Minute)
	view := e.RecordProbeError("A", "HTTP 401: unauthorized")
	if view.LastProbeError == "" || !view.Exhausted {
		t.Fatalf("error must keep data: %+v", view)
	}
	view = e.ApplyProbe("A", mustParse(t, usageBody(0, 18000, 20, 86400), c.Now()), c.Now())
	if view.Exhausted || view.Short != nil || view.LastProbeError != "" || view.WeeklyState != WeeklyOpen {
		t.Fatalf("after query: %+v", view)
	}
	info := e.ProbeInfo("A")
	if !info.DataAt.Equal(c.Now()) || info.LastProbeError != "" {
		t.Fatalf("probe info: %+v", info)
	}
}

func TestShortWindowStates(t *testing.T) {
	// Weekly-only plan: the 5h slot is listed but null.
	weeklyOnly := `{"rate_limit":{"primary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_after_seconds":90000},"secondary_window":null}}`
	if r := mustParse(t, weeklyOnly, t0); !r.NoShort || r.Short != nil {
		t.Fatalf("weekly-only plan: %+v", r)
	}
	// An omitted slot proves nothing.
	partial := `{"rate_limit":{"primary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_after_seconds":90000}}}`
	if r := mustParse(t, partial, t0); r.NoShort {
		t.Fatalf("omitted slot taken as no 5h limit: %+v", r)
	}
	if r := mustParse(t, usageBody(0, 18000, 30, 90000), t0); r.NoShort || !r.ShortIdle {
		t.Fatalf("idle 5h: %+v", r)
	}

	e, c := newTestEngine(t, nil)
	open := e.ApplyProbe("A", mustParse(t, usageBody(12, 3600, 30, 90000), c.Now()), c.Now())
	idle := e.ApplyProbe("B", mustParse(t, usageBody(0, 18000, 30, 90000), c.Now()), c.Now())
	none := e.ApplyProbe("C", mustParse(t, weeklyOnly, c.Now()), c.Now())
	unknown := e.ApplyProbe("D", mustParse(t, partial, c.Now()), c.Now())
	if open.ShortState != ShortStateOpen || idle.ShortState != ShortStateIdle || none.ShortState != ShortStateNone || unknown.ShortState != ShortStateUnknown {
		t.Fatalf("states: open=%s idle=%s none=%s unknown=%s", open.ShortState, idle.ShortState, none.ShortState, unknown.ShortState)
	}
	// A running 5h window from a request replaces "not started".
	c.Advance(time.Minute)
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: "B", At: c.Now(), Headers: weeklyHeaders(c.Now().Add(20*time.Hour), 31)})
	if b := weeklyState(t, e, "B"); b.ShortState != ShortStateOpen {
		t.Fatalf("B after request: %+v", b.ShortState)
	}
	// A 5h window whose reset has passed counts as not running.
	c.Advance(2 * time.Hour)
	if a := weeklyState(t, e, "A"); a.ShortState != ShortStateIdle {
		t.Fatalf("expired 5h window: %s", a.ShortState)
	}
}
