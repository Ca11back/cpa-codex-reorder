package reorder

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestEngine(t *testing.T, mutate func(*Config)) (*Engine, *clock) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.StatePath = ""
	if mutate != nil {
		mutate(&cfg)
	}
	c := &clock{now: t0}
	e := NewEngine(cfg)
	e.SetClock(c.Now)
	return e, c
}

func cands(ids ...string) []Candidate {
	out := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, Candidate{ID: id})
	}
	return out
}

// weeklyHeaders mimics a Codex response for a plan with a 5h window (primary) and a weekly window (secondary).
func weeklyHeaders(weeklyReset time.Time, weeklyUsed float64) map[string][]string {
	return map[string][]string{
		"X-Codex-Primary-Used-Percent":     {"10"},
		"X-Codex-Primary-Window-Minutes":   {"300"},
		"X-Codex-Primary-Reset-At":         {strconv.FormatInt(weeklyReset.Add(-time.Hour).Unix(), 10)},
		"X-Codex-Secondary-Used-Percent":   {strconv.FormatFloat(weeklyUsed, 'f', -1, 64)},
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {strconv.FormatInt(weeklyReset.Unix(), 10)},
	}
}

func seedWeekly(e *Engine, id string, reset time.Time, used float64) {
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: id, Headers: weeklyHeaders(reset, used), At: e.now()})
}

func pick(t *testing.T, e *Engine, session string, ids ...string) PickResult {
	t.Helper()
	res := e.Pick(PickInput{SessionID: session, SelectionID: "sel-" + session + "-" + strconv.FormatInt(e.now().UnixNano(), 10), Candidates: cands(ids...)})
	if !res.Handled {
		t.Fatalf("pick for %q not handled", session)
	}
	return res
}

// succeed reports a successful upstream attempt for the session's latest pick.
func succeed(e *Engine, session, authID string) {
	sel := "ok-" + session + "-" + strconv.FormatInt(e.now().UnixNano(), 10)
	e.mu.Lock()
	e.selections[sel] = selectionRef{sessionKey: SessionKey("", session), authID: authID, at: e.now()}
	e.mu.Unlock()
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: authID, SelectionID: sel, At: e.now()})
}

func withOrder(order ...string) func(*Config) {
	return func(c *Config) { c.Order = order }
}

func TestDefaultOrderOpensIdleWindowThenUsesMostUrgent(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(24*time.Hour), 40)
	seedWeekly(e, "B", t0.Add(48*time.Hour), 40)
	all := []string{"A", "B", "D"} // D has never been used: no running window
	want := []string{"D", "A", "B"}
	for i, w := range want {
		if got := pick(t, e, fmt.Sprintf("S%d", i), all...).AuthID; got != w {
			t.Fatalf("S%d: got %s, want %s", i, got, w)
		}
	}
}

func TestRefreshedAccountCountsAsUnopened(t *testing.T) {
	e, c := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(time.Hour), 30)
	c.Advance(2 * time.Hour) // A's weekly window reset; nothing has used it since
	seedWeekly(e, "B", t0.Add(24*time.Hour), 0)
	res := pick(t, e, "S1", "A", "B")
	if res.AuthID != "A" {
		t.Fatalf("got %+v, want A", res)
	}
	d := e.Snapshot(nil, nil).Decisions[0]
	if d.DecidedBy != FieldUnopenedFirst {
		t.Fatalf("decided by %q", d.DecidedBy)
	}
}

func TestUnopenedLastKeepsIdleAccountClosed(t *testing.T) {
	e, c := newTestEngine(t, withOrder(FieldSessions, FieldUnopenedLast, FieldUrgency))
	seedWeekly(e, "A", t0.Add(24*time.Hour), 40)
	seedWeekly(e, "B", t0.Add(48*time.Hour), 40)
	seedWeekly(e, "C", t0.Add(120*time.Hour), 40)
	all := []string{"A", "B", "C", "D"}
	want := map[string]string{"S1": "A", "S2": "B", "S3": "C"}
	for _, s := range []string{"S1", "S2", "S3"} {
		if got := pick(t, e, s, all...).AuthID; got != want[s] {
			t.Fatalf("%s: got %s, want %s", s, got, want[s])
		}
	}
	c.Advance(10 * time.Minute)
	if got := pick(t, e, "S1", all...); got.AuthID != "A" || got.Kind != KindSticky {
		t.Fatalf("S1 sticky: got %+v", got)
	}
	// S1 goes idle past the TTL while S2 and S3 stay active.
	c.Advance(25 * time.Minute)
	pick(t, e, "S2", all...)
	pick(t, e, "S3", all...)
	c.Advance(6 * time.Minute)
	if got := pick(t, e, "S4", all...).AuthID; got != "A" {
		t.Fatalf("S4: got %s, want A (D must stay unopened)", got)
	}
}

func TestUrgencyPrefersMoreRemainingWhenResetsAreClose(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(10*time.Hour), 95)                // 5% left
	seedWeekly(e, "B", t0.Add(10*time.Hour+10*time.Minute), 10) // 90% left
	if got := pick(t, e, "S1", "A", "B").AuthID; got != "B" {
		t.Fatalf("got %s, want B", got)
	}
	d := e.Snapshot(nil, nil).Decisions[0]
	if d.DecidedBy != FieldUrgency {
		t.Fatalf("decided by %q", d.DecidedBy)
	}
	// Plain reset ordering picks the account that resets first.
	e2, _ := newTestEngine(t, withOrder(FieldSessions, FieldReset))
	seedWeekly(e2, "A", t0.Add(10*time.Hour), 95)
	seedWeekly(e2, "B", t0.Add(10*time.Hour+10*time.Minute), 10)
	if got := pick(t, e2, "S1", "A", "B").AuthID; got != "A" {
		t.Fatalf("reset order: got %s, want A", got)
	}
}

func TestUrgencyKeepsEarlierResetWhenFarApart(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(24*time.Hour), 80) // 20% left
	seedWeekly(e, "B", t0.Add(48*time.Hour), 20) // 80% left
	if got := pick(t, e, "S1", "A", "B").AuthID; got != "A" {
		t.Fatalf("got %s, want A", got)
	}
}

func TestZeroUrgencyFactorEqualsReset(t *testing.T) {
	e, _ := newTestEngine(t, func(c *Config) { c.UrgencyMinutesPerPercent = 0 })
	seedWeekly(e, "A", t0.Add(10*time.Hour), 95)
	seedWeekly(e, "B", t0.Add(10*time.Hour+10*time.Minute), 10)
	if got := pick(t, e, "S1", "A", "B").AuthID; got != "A" {
		t.Fatalf("got %s, want A", got)
	}
}

func TestPlanMultiplierScalesRemaining(t *testing.T) {
	seed := func(e *Engine) {
		seedWeekly(e, "A", t0.Add(10*time.Hour), 50)
		seedWeekly(e, "B", t0.Add(11*time.Hour), 50)
	}
	e, _ := newTestEngine(t, nil)
	seed(e)
	if got := pick(t, e, "S1", "A", "B").AuthID; got != "A" {
		t.Fatalf("same plan: got %s, want A", got)
	}
	// B is a 10x plan: its 50% is ten times the quota and needs far longer to use up.
	e2, _ := newTestEngine(t, func(c *Config) { c.PlanMultipliers = map[string]float64{"B": 10} })
	seed(e2)
	if got := pick(t, e2, "S1", "A", "B").AuthID; got != "B" {
		t.Fatalf("10x plan: got %s, want B", got)
	}
	e3, _ := newTestEngine(t, func(c *Config) {
		c.Order = []string{FieldRemaining}
		c.PlanMultipliers = map[string]float64{"A": 20}
	})
	seedWeekly(e3, "A", t0.Add(10*time.Hour), 90) // 10% × 20 = 200
	seedWeekly(e3, "B", t0.Add(10*time.Hour), 0)  // 100% × 1 = 100
	if got := pick(t, e3, "S1", "A", "B").AuthID; got != "A" {
		t.Fatalf("remaining with multiplier: got %s, want A", got)
	}
}

func TestSessionsFirstSpreadsEvenly(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(24*time.Hour), 0)
	seedWeekly(e, "B", t0.Add(48*time.Hour), 0)
	got := []string{}
	for i := 1; i <= 5; i++ {
		got = append(got, pick(t, e, fmt.Sprintf("S%d", i), "A", "B").AuthID)
	}
	want := []string{"A", "B", "A", "B", "A"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestWithoutSessionsFieldConcentrates(t *testing.T) {
	e, _ := newTestEngine(t, withOrder(FieldUrgency))
	seedWeekly(e, "A", t0.Add(24*time.Hour), 0)
	seedWeekly(e, "B", t0.Add(48*time.Hour), 0)
	for i := 1; i <= 3; i++ {
		if got := pick(t, e, fmt.Sprintf("S%d", i), "A", "B").AuthID; got != "A" {
			t.Fatalf("S%d: got %s, want A", i, got)
		}
	}
}

func TestJustAssignedUnknownAccountCountsAsOpened(t *testing.T) {
	e, c := newTestEngine(t, withOrder(FieldUnopenedFirst, FieldUrgency))
	if got := pick(t, e, "S1", "A", "B").AuthID; got != "A" {
		t.Fatalf("S1: got %s, want A", got)
	}
	c.Advance(time.Second)
	// No usage data yet, but A's window opened when S1 started; B is still closed.
	if got := pick(t, e, "S2", "A", "B").AuthID; got != "B" {
		t.Fatalf("S2: got %s, want B", got)
	}
	for _, a := range e.Snapshot(nil, nil).Accounts {
		if a.WeeklyState != WeeklyProvisional {
			t.Fatalf("%s state %q", a.ID, a.WeeklyState)
		}
	}
}

// Like CPA's default session-affinity-rebind-on: any, a bound account that
// cannot serve an attempt loses the session for good.
func TestUnavailableBoundAccountRebindsAndStays(t *testing.T) {
	e, c := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(24*time.Hour), 0)
	seedWeekly(e, "B", t0.Add(48*time.Hour), 0)
	pick(t, e, "S1", "A", "B")
	succeed(e, "S1", "A")

	c.Advance(time.Second)
	res := pick(t, e, "S1", "B") // A failed this attempt or is cooling down
	if res.AuthID != "B" || res.Kind != KindRebind {
		t.Fatalf("rebind: got %+v", res)
	}
	c.Advance(30 * time.Second)
	if res := pick(t, e, "S1", "A", "B"); res.AuthID != "B" || res.Kind != KindSticky {
		t.Fatalf("the session must stay on B once A is back: got %+v", res)
	}
	var moves int
	for _, d := range e.Snapshot(nil, nil).Decisions {
		if d.Kind == KindRebind && d.FromAuthID == "A" && d.AuthID == "B" {
			moves++
		}
	}
	if moves != 1 {
		t.Fatalf("expected one rebind decision, got %d", moves)
	}
}

func TestExhaustedBoundAccountRebinds(t *testing.T) {
	e, c := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(24*time.Hour), 50)
	seedWeekly(e, "B", t0.Add(48*time.Hour), 0)
	pick(t, e, "S1", "A", "B")
	succeed(e, "S1", "A")
	seedWeekly(e, "A", t0.Add(24*time.Hour), 100)
	c.Advance(time.Second)
	if res := pick(t, e, "S1", "A", "B"); res.Kind != KindRebind || res.AuthID != "B" {
		t.Fatalf("got %+v", res)
	}
}

// Inside a Codex websocket connection CPA offers only the connection's account.
// When that bound account is exhausted there is nothing to move to: the attempt
// stays on it, the binding is kept and no rebind is recorded. The next attempt
// with other candidates moves the session.
func TestExhaustedBoundAccountOnlyCandidateIsHeld(t *testing.T) {
	e, c := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(24*time.Hour), 50)
	seedWeekly(e, "B", t0.Add(48*time.Hour), 0)
	pick(t, e, "S1", "A", "B")
	succeed(e, "S1", "A")
	seedWeekly(e, "A", t0.Add(24*time.Hour), 100)
	c.Advance(time.Second)
	res := pick(t, e, "S1", "A")
	if res.Kind != KindHeld || res.AuthID != "A" {
		t.Fatalf("only candidate: %+v", res)
	}
	var held int
	for _, d := range e.Snapshot(nil, nil).Decisions {
		if d.Kind == KindRebind && d.FromAuthID == "A" && d.AuthID == "A" {
			t.Fatalf("rebind onto the same account recorded: %+v", d)
		}
		if d.Kind == KindHeld && d.AuthID == "A" {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("expected one held decision, got %d", held)
	}
	if st := e.TakeStats(); st == nil || st.Kinds[KindHeld] != 1 || st.Kinds[KindRebind] != 0 {
		t.Fatalf("stats: %+v", st)
	}
	c.Advance(time.Second)
	if res := pick(t, e, "S1", "A", "B"); res.Kind != KindRebind || res.AuthID != "B" {
		t.Fatalf("with another candidate: %+v", res)
	}
}

// A prefixed model is offered only on the prefixed credential; like CPA's
// per-model affinity it does not move the session's binding.
func TestPrefixedModelKeepsBinding(t *testing.T) {
	e, c := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(24*time.Hour), 0)
	seedWeekly(e, "C", t0.Add(48*time.Hour), 0)
	pick(t, e, "S1", "A", "C")
	succeed(e, "S1", "A")
	c.Advance(time.Second)
	res := e.Pick(PickInput{SessionID: "S1", Model: "pc/gpt-x", Candidates: cands("C")})
	if res.AuthID != "C" || res.Kind != KindPinned {
		t.Fatalf("pinned: %+v", res)
	}
	c.Advance(time.Second)
	if res := pick(t, e, "S1", "A", "C"); res.AuthID != "A" || res.Kind != KindSticky {
		t.Fatalf("binding moved by a prefixed request: %+v", res)
	}
	// Without the prefix, the same situation is an unavailable account: rebind.
	c.Advance(time.Second)
	if res := e.Pick(PickInput{SessionID: "S1", Model: "gpt-x", Candidates: cands("C")}); res.Kind != KindRebind || res.AuthID != "C" {
		t.Fatalf("unprefixed: %+v", res)
	}
}

// State saved mid-detour by 0.6.2 or older: the session moves to the account
// that was serving it.
func TestLegacyDetourIsRebound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e, c := newTestEngine(t, func(cfg *Config) { cfg.StatePath = path })
	seedWeekly(e, "A", t0.Add(24*time.Hour), 0)
	seedWeekly(e, "B", t0.Add(48*time.Hour), 0)
	pick(t, e, "S1", "A", "B")
	e.mu.Lock()
	for _, s := range e.sessions {
		s.LegacyDetourAuthID = "B"
	}
	e.dirty = true
	e.mu.Unlock()
	if err := e.SaveState(); err != nil {
		t.Fatal(err)
	}
	e2 := NewEngine(e.Config())
	e2.SetClock(c.Now)
	if err := e2.LoadState(); err != nil {
		t.Fatal(err)
	}
	if res := e2.Pick(PickInput{SessionID: "S1", Candidates: cands("A", "B")}); res.AuthID != "B" || res.Kind != KindSticky {
		t.Fatalf("legacy detour: %+v", res)
	}
}

func TestFirstAttemptFailureRebindsImmediately(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(24*time.Hour), 0)
	seedWeekly(e, "B", t0.Add(48*time.Hour), 0)
	pick(t, e, "S1", "A", "B")
	// CPA retries the same request without A.
	if res := pick(t, e, "S1", "B"); res.Kind != KindRebind || res.AuthID != "B" {
		t.Fatalf("got %+v", res)
	}
}

func TestExhaustedAccountSkippedAndBoundSessionsRebind(t *testing.T) {
	e, c := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(24*time.Hour), 50)
	seedWeekly(e, "B", t0.Add(48*time.Hour), 0)
	pick(t, e, "S1", "A", "B")
	succeed(e, "S1", "A")
	c.Advance(time.Minute)
	seedWeekly(e, "A", t0.Add(24*time.Hour), 100) // the weekly quota ran out
	if res := pick(t, e, "S1", "A", "B"); res.Kind != KindRebind || res.AuthID != "B" {
		t.Fatalf("bound session: got %+v", res)
	}
	if res := pick(t, e, "S2", "A", "B"); res.AuthID != "B" {
		t.Fatalf("new session must skip exhausted A: got %+v", res)
	}
	// After the weekly reset A is usable again and counts as unopened.
	c.Advance(25 * time.Hour)
	for _, a := range e.Snapshot([]Candidate{{ID: "A"}, {ID: "B"}}, nil).Accounts {
		if a.ID == "A" && (a.Exhausted || a.WeeklyState != WeeklyRefreshed || a.Order != 1) {
			t.Fatalf("A after reset: %+v", a)
		}
	}
}

func TestShortWindowExhaustionAlsoSkips(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	h := weeklyHeaders(t0.Add(24*time.Hour), 30)
	h["X-Codex-Primary-Used-Percent"] = []string{"100"}
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: "A", Headers: h, At: t0})
	seedWeekly(e, "B", t0.Add(48*time.Hour), 0)
	if res := pick(t, e, "S1", "A", "B"); res.AuthID != "B" {
		t.Fatalf("got %+v", res)
	}
}

func TestOnlyExhaustedCandidatesStillHandled(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(24*time.Hour), 100)
	if res := pick(t, e, "S1", "A"); res.AuthID != "A" {
		t.Fatalf("got %+v", res)
	}
}

func TestPriorityFieldPrefersHigherPriority(t *testing.T) {
	prio := []Candidate{{ID: "A", Priority: 0}, {ID: "B", Priority: 5}}
	e, _ := newTestEngine(t, withOrder(FieldPriority, FieldSessions, FieldUrgency))
	seedWeekly(e, "A", t0.Add(24*time.Hour), 0)
	seedWeekly(e, "B", t0.Add(120*time.Hour), 0)
	if res := e.Pick(PickInput{SessionID: "S1", Candidates: prio}); res.AuthID != "B" {
		t.Fatalf("got %+v", res)
	}
	// The default order puts CPA priority first too.
	e2, _ := newTestEngine(t, nil)
	seedWeekly(e2, "A", t0.Add(24*time.Hour), 0)
	seedWeekly(e2, "B", t0.Add(120*time.Hour), 0)
	if res := e2.Pick(PickInput{SessionID: "S1", Candidates: prio}); res.AuthID != "B" {
		t.Fatalf("default: got %+v", res)
	}
	// Without priority in the order, it is ignored.
	e3, _ := newTestEngine(t, withOrder(FieldSessions, FieldUrgency))
	seedWeekly(e3, "A", t0.Add(24*time.Hour), 0)
	seedWeekly(e3, "B", t0.Add(120*time.Hour), 0)
	if res := e3.Pick(PickInput{SessionID: "S1", Candidates: prio}); res.AuthID != "A" {
		t.Fatalf("without priority: got %+v", res)
	}
}

func TestBoundSessionKeepsLowerPriorityAccount(t *testing.T) {
	e, c := newTestEngine(t, withOrder(FieldPriority, FieldSessions))
	pick(t, e, "S1", "A")
	succeed(e, "S1", "A")
	c.Advance(time.Minute)
	res := e.Pick(PickInput{SessionID: "S1", Candidates: []Candidate{{ID: "A", Priority: 0}, {ID: "B", Priority: 5}}})
	if res.AuthID != "A" || res.Kind != KindSticky {
		t.Fatalf("got %+v", res)
	}
}

func TestNoSessionIDCreatesNoBinding(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	res := e.Pick(PickInput{Candidates: cands("A", "B")})
	if !res.Handled || res.Kind != KindNoSession {
		t.Fatalf("got %+v", res)
	}
	if n := len(e.Snapshot(nil, nil).Sessions); n != 0 {
		t.Fatalf("sessions = %d", n)
	}
}

func TestScopeSeparatesSessions(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	a := e.Pick(PickInput{SessionID: "same", Scope: "client-1", Candidates: cands("A", "B")})
	b := e.Pick(PickInput{SessionID: "same", Scope: "client-2", Candidates: cands("A", "B")})
	if a.AuthID == b.AuthID {
		t.Fatalf("both scopes landed on %s; they must be separate sessions", a.AuthID)
	}
}

func TestUsageExtendsSessionLifetime(t *testing.T) {
	e, c := newTestEngine(t, nil)
	e.Pick(PickInput{SessionID: "S1", SelectionID: "sel-1", Candidates: cands("A", "B")})
	c.Advance(25 * time.Minute)
	// A long streaming response finishes now.
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: "A", SelectionID: "sel-1", At: c.Now()})
	c.Advance(25 * time.Minute)
	if res := pick(t, e, "S1", "A", "B"); res.Kind != KindSticky || res.AuthID != "A" {
		t.Fatalf("got %+v", res)
	}
}

func TestExpiredSessionIsNew(t *testing.T) {
	e, c := newTestEngine(t, nil)
	pick(t, e, "S1", "A", "B")
	c.Advance(31 * time.Minute)
	if res := pick(t, e, "S1", "A", "B"); res.Kind != KindNew {
		t.Fatalf("got %+v", res)
	}
}

func TestConcurrentNewSessionsSpreadEvenly(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	ids := []string{"A", "B", "C", "D", "E"}
	for i, id := range ids {
		seedWeekly(e, id, t0.Add(time.Duration(i+1)*24*time.Hour), 0)
	}
	var wg sync.WaitGroup
	results := make([]string, 23)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = e.Pick(PickInput{SessionID: fmt.Sprintf("S%d", i), Candidates: cands(ids...)}).AuthID
		}(i)
	}
	wg.Wait()
	counts := map[string]int{}
	for _, id := range results {
		counts[id]++
	}
	min, max := len(results), 0
	for _, id := range ids {
		if counts[id] < min {
			min = counts[id]
		}
		if counts[id] > max {
			max = counts[id]
		}
	}
	if max-min > 1 {
		t.Fatalf("uneven spread: %v", counts)
	}
	// 23 sessions over 5 accounts: the three earliest-expiring accounts carry the extra ones.
	if counts["A"] != 5 || counts["B"] != 5 || counts["C"] != 5 || counts["D"] != 4 || counts["E"] != 4 {
		t.Fatalf("extra sessions should go to the earliest expiry: %v", counts)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e, c := newTestEngine(t, func(cfg *Config) { cfg.StatePath = path })
	seedWeekly(e, "A", t0.Add(24*time.Hour), 12)
	first := pick(t, e, "S1", "A", "B").AuthID
	if err := e.SaveState(); err != nil {
		t.Fatal(err)
	}

	e2 := NewEngine(e.Config())
	e2.SetClock(func() time.Time { return c.Now().Add(time.Minute) })
	if err := e2.LoadState(); err != nil {
		t.Fatal(err)
	}
	if res := e2.Pick(PickInput{SessionID: "S1", Candidates: cands("A", "B")}); res.AuthID != first || res.Kind != KindSticky {
		t.Fatalf("binding lost after restore: %+v", res)
	}
	snap := e2.Snapshot(nil, nil)
	var found bool
	for _, a := range snap.Accounts {
		if a.ID == "A" && a.Weekly != nil && a.Weekly.UsedPercent == 12 {
			found = true
		}
	}
	if !found {
		t.Fatalf("weekly snapshot lost: %+v", snap.Accounts)
	}
	if len(snap.Decisions) != 1 || snap.Decisions[0].Kind != KindNew {
		t.Fatalf("decision log lost: %+v", snap.Decisions)
	}
}

func TestLoadStateDropsExpiredSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e, c := newTestEngine(t, func(cfg *Config) { cfg.StatePath = path })
	pick(t, e, "S1", "A")
	if err := e.SaveState(); err != nil {
		t.Fatal(err)
	}
	e2 := NewEngine(e.Config())
	e2.SetClock(func() time.Time { return c.Now().Add(time.Hour) })
	if err := e2.LoadState(); err != nil {
		t.Fatal(err)
	}
	if n := len(e2.Snapshot(nil, nil).Sessions); n != 0 {
		t.Fatalf("sessions = %d", n)
	}
}

func TestDecisionLogExplainsNewSession(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(24*time.Hour), 0)
	pick(t, e, "S1", "A", "B")
	snap := e.Snapshot(nil, nil)
	if len(snap.Decisions) != 1 {
		t.Fatalf("decisions = %d", len(snap.Decisions))
	}
	d := snap.Decisions[0]
	// B has never been used, so the default order opens it first.
	if d.Kind != KindNew || d.AuthID != "B" || d.DecidedBy != FieldUnopenedFirst || len(d.Ranking) != 2 || d.Reason == "" {
		t.Fatalf("unexpected decision %+v", d)
	}
}

func TestSnapshotPreviewOrder(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(48*time.Hour), 0)
	seedWeekly(e, "B", t0.Add(24*time.Hour), 0)
	snap := e.Snapshot([]Candidate{{ID: "A"}, {ID: "B"}, {ID: "C"}}, nil)
	order := []string{}
	for _, a := range snap.Accounts {
		order = append(order, fmt.Sprintf("%s:%d", a.ID, a.Order))
	}
	if fmt.Sprint(order) != "[C:1 B:2 A:3]" {
		t.Fatalf("got %v", order)
	}
}

func TestPreviewLeavesEngineUntouched(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(10*time.Hour), 95)
	seedWeekly(e, "B", t0.Add(10*time.Hour+10*time.Minute), 10)
	proposed, err := WithOverrides(e.Config(), []byte(`{"order":["sessions","reset"]}`))
	if err != nil {
		t.Fatal(err)
	}
	// "N" is offered by CPA but unknown to the engine; it must not be stored.
	p := e.Preview(proposed, []Candidate{{ID: "A"}, {ID: "B"}, {ID: "N", Priority: 3}}, nil)
	order := func(v RankingView) string {
		ids := []string{}
		for _, r := range v.Entries {
			ids = append(ids, r.AuthID)
		}
		return fmt.Sprint(ids)
	}
	if order(p.Current) != "[N B A]" || order(p.Proposed) != "[A B N]" {
		t.Fatalf("current %s proposed %s", order(p.Current), order(p.Proposed))
	}
	if p.Current.Entries[0].Priority != 3 {
		t.Fatalf("offered priority not used: %+v", p.Current.Entries[0])
	}
	snap := e.Snapshot(nil, nil)
	if len(snap.Accounts) != 2 || len(snap.Decisions) != 0 || strings.Join(snap.Config.Order, ",") != "priority,sessions,unopened_first,urgency" {
		t.Fatalf("engine changed: %+v", snap)
	}
	// Omitted keys keep the active values.
	keep, err := WithOverrides(Config{Order: []string{FieldReset}, UrgencyMinutesPerPercent: 7, SessionTTL: time.Hour, PlanMultipliers: map[string]float64{"A": 10}}, []byte(`{"urgency_minutes_per_percent": 2}`))
	if err != nil || strings.Join(keep.Order, ",") != "reset" || keep.UrgencyMinutesPerPercent != 2 || keep.SessionTTL != time.Hour || keep.Multiplier("A") != 10 {
		t.Fatalf("overrides: %+v %v", keep, err)
	}
}

func TestStaleAccountsArePruned(t *testing.T) {
	e, c := newTestEngine(t, nil)
	seedWeekly(e, "old", t0.Add(24*time.Hour), 10)
	pick(t, e, "S1", "busy")
	c.Advance(9 * 24 * time.Hour)
	// "busy" keeps a live session, so it stays even though it was last offered long ago.
	e.mu.Lock()
	e.sessions[SessionKey("", "S1")].LastSeenAt = c.Now()
	e.mu.Unlock()
	pick(t, e, "S2", "fresh")
	e.mu.Lock()
	_, hasOld := e.accounts["old"]
	_, hasBusy := e.accounts["busy"]
	_, hasFresh := e.accounts["fresh"]
	e.mu.Unlock()
	if hasOld || !hasBusy || !hasFresh {
		t.Fatalf("old=%v busy=%v fresh=%v", hasOld, hasBusy, hasFresh)
	}
}

func TestExpiredSessionIsNewBeforeSweep(t *testing.T) {
	e, c := newTestEngine(t, nil)
	pick(t, e, "S1", "A", "B")
	c.Advance(31 * time.Minute)
	e.mu.Lock()
	e.lastHousekeeping = c.Now() // the periodic sweep has not removed S1 yet
	e.mu.Unlock()
	if res := pick(t, e, "S1", "A", "B"); res.Kind != KindNew {
		t.Fatalf("got %+v", res)
	}
	if n := len(e.Snapshot(nil, nil).Sessions); n != 1 {
		t.Fatalf("sessions = %d", n)
	}
}

func TestSnapshotDoesNotCreateAccountsOrDirtyState(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(24*time.Hour), 0)
	e.mu.Lock()
	e.dirty = false
	e.mu.Unlock()
	snap := e.Snapshot([]Candidate{{ID: "A"}, {ID: "N", Priority: 2}}, nil)
	var n *AccountView
	for i := range snap.Accounts {
		if snap.Accounts[i].ID == "N" {
			n = &snap.Accounts[i]
		}
	}
	if n == nil || n.Order != 1 || n.Priority != 2 || n.WeeklyState != WeeklyUnknown {
		t.Fatalf("offered account missing from the view: %+v", snap.Accounts)
	}
	e.mu.Lock()
	_, stored := e.accounts["N"]
	dirty := e.dirty
	e.mu.Unlock()
	if stored || dirty {
		t.Fatalf("snapshot changed the engine: stored=%v dirty=%v", stored, dirty)
	}
}

func TestSelectionOverflowShedsOldReferences(t *testing.T) {
	e, c := newTestEngine(t, nil)
	e.mu.Lock()
	for i := 0; i < maxSelections; i++ {
		e.selections[strconv.Itoa(i)] = selectionRef{at: c.Now()}
	}
	e.selections["recent"] = selectionRef{at: c.Now().Add(20 * time.Minute)}
	e.mu.Unlock()
	c.Advance(20 * time.Minute)
	e.mu.Lock()
	e.rememberSelectionLocked("new", "", "A", c.Now())
	n := len(e.selections)
	_, keptRecent := e.selections["recent"]
	e.mu.Unlock()
	if n != 2 || !keptRecent {
		t.Fatalf("selections = %d, recent kept = %v", n, keptRecent)
	}
}
