package reorder

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// warmupEngine: default hours 09:00-22:00 Asia/Shanghai; t0 is 20:00 there.
func warmupEngine(t *testing.T, mutate func(*WarmupConfig)) (*Engine, *clock, *[]capturedEvent) {
	t.Helper()
	e, c := newTestEngine(t, func(cfg *Config) {
		cfg.Warmup.Enabled = true
		cfg.Warmup.DelayMin, cfg.Warmup.DelayMax = 10*time.Minute, 10*time.Minute
		cfg.Warmup.SpacingMin, cfg.Warmup.SpacingMax = 5*time.Minute, 5*time.Minute
		if mutate != nil {
			mutate(&cfg.Warmup)
		}
	})
	e.SetRand(func(n int64) int64 { return 0 })
	return e, c, captureEvents(e)
}

func warmupEvents(events []capturedEvent) []WarmupEvent {
	var out []WarmupEvent
	for _, ev := range events {
		if w, ok := ev.payload.(WarmupEvent); ok {
			out = append(out, w)
		}
	}
	return out
}

func allow(ids ...string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func TestWarmupOpensRefreshedWindowAfterDelay(t *testing.T) {
	e, c, events := warmupEngine(t, nil)
	seedWeekly(e, "A", t0.Add(time.Minute), 40) // ends at 20:01 local
	seedWeekly(e, "B", t0.Add(48*time.Hour), 10)
	e.TakeStats() // start counting after the seeded usage
	c.Advance(2 * time.Minute)
	if job := e.WarmupTick(allow("A", "B")); job != nil {
		t.Fatalf("sent before the delay: %+v", job)
	}
	a := weeklyState(t, e, "A")
	if !a.WarmupPlanAt.Equal(t0.Add(time.Minute + 10*time.Minute)) {
		t.Fatalf("plan: %v", a.WarmupPlanAt)
	}
	if b := weeklyState(t, e, "B"); !b.WarmupPlanAt.IsZero() {
		t.Fatalf("open account planned: %+v", b.WarmupPlanAt)
	}

	c.Advance(10 * time.Minute)
	job := e.WarmupTick(allow("A", "B"))
	if job == nil || job.AuthID != "A" || job.Manual || job.Model != "gpt-6-luna" || job.ReasoningEffort != "low" || !warmupPromptRE.MatchString(job.Prompt) {
		t.Fatalf("job: %+v", job)
	}
	// The plugin's request reaches the scheduler pinned to A: no binding, no assignment, not traffic.
	res := e.Pick(PickInput{SessionID: "derived", Scope: job.CallerScope, SelectionID: "sel-w", Candidates: cands("A")})
	if res.Kind != KindWarmup || res.AuthID != "A" {
		t.Fatalf("warmup pick: %+v", res)
	}
	if len(e.Snapshot(nil, nil).Sessions) != 0 || weeklyState(t, e, "A").Assignments != 0 {
		t.Fatal("warmup request created a binding or an assignment")
	}
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: "A", SelectionID: "sel-w", Headers: weeklyHeaders(c.Now().Add(weekLength), 0), At: c.Now()})
	e.FinishWarmup(job, 200, "")
	st := e.TakeStats()
	if st != nil && (st.Picks != 0 || st.Usage != 0) {
		t.Fatalf("warmup counted as traffic: %+v", st)
	}
	if a := weeklyState(t, e, "A"); a.WeeklyState != WeeklyOpen || !a.WarmupPlanAt.IsZero() || a.WarmupLastResult != WarmupSent {
		t.Fatalf("after warmup: %+v", a)
	}
	ws := warmupEvents(*events)
	if len(ws) != 1 || ws[0].Result != WarmupSent || ws[0].AuthID != "A" || ws[0].TodayCount != 1 {
		t.Fatalf("events: %+v", ws)
	}
	c.Advance(10 * time.Minute)
	if job := e.WarmupTick(allow("A", "B")); job != nil {
		t.Fatalf("opened account picked again: %+v", job)
	}
}

func TestWarmupOnlyMatchesItsOwnRequest(t *testing.T) {
	e, c, _ := warmupEngine(t, nil)
	seedWeekly(e, "A", t0.Add(time.Minute), 40)
	c.Advance(12 * time.Minute)
	job := e.WarmupTick(allow("A"))
	if job == nil || job.AuthID != "A" || job.CallerScope == "" {
		t.Fatalf("want marked warmup for A, got %+v", job)
	}

	if got := e.Pick(PickInput{SessionID: "chat", Scope: "ordinary", SelectionID: "sel-chat", Candidates: cands("A")}); got.Kind != KindNew {
		t.Fatalf("ordinary chat during warmup: %+v", got)
	}
	if got := e.Pick(PickInput{SessionID: "image", Image: true, SelectionID: "sel-image", Candidates: cands("A")}); got.Kind != KindImage {
		t.Fatalf("ordinary image during warmup: %+v", got)
	}
	if got := e.Pick(PickInput{SessionID: "other", Scope: job.CallerScope, Candidates: cands("B")}); got.Kind != KindNew {
		t.Fatalf("warmup scope for a different account: %+v", got)
	}
	if got := e.Pick(PickInput{SessionID: "derived", Scope: job.CallerScope, Candidates: cands("A")}); got.Kind != KindWarmup {
		t.Fatalf("warmup during ordinary traffic: %+v", got)
	}
	e.FinishWarmup(job, 200, "")
	if got := e.Pick(PickInput{SessionID: "later", Scope: job.CallerScope, Candidates: cands("A")}); got.Kind != KindNew {
		t.Fatalf("completed warmup marker was still accepted: %+v", got)
	}
}

func TestWarmupRespectsHoursAndSpacing(t *testing.T) {
	e, c, _ := warmupEngine(t, nil)
	// Windows that end at 21:55, 21:56, 21:57 local; delay 10m pushes them past 22:00.
	for i, id := range []string{"A", "B", "C"} {
		seedWeekly(e, id, t0.Add(time.Hour+55*time.Minute+time.Duration(i)*time.Minute), 10)
	}
	c.Advance(2 * time.Hour)
	if job := e.WarmupTick(allow("A", "B", "C")); job != nil {
		t.Fatalf("sent outside the hours: %+v", job)
	}
	loc := e.Config().Warmup.Location
	nextStart := time.Date(2026, 11, 10, 9, 0, 0, 0, loc)
	if a := weeklyState(t, e, "A"); !a.WarmupPlanAt.Equal(nextStart) {
		t.Fatalf("plan not moved to 09:00 next day: %v", a.WarmupPlanAt.In(loc))
	}

	c.Advance(nextStart.Sub(c.Now()))
	first := e.WarmupTick(allow("A", "B", "C"))
	if first == nil {
		t.Fatal("nothing sent at 09:00")
	}
	e.FinishWarmup(first, 200, "")
	if job := e.WarmupTick(allow("A", "B", "C")); job != nil {
		t.Fatalf("spacing ignored: %+v", job)
	}
	c.Advance(5 * time.Minute)
	second := e.WarmupTick(allow("A", "B", "C"))
	if second == nil || second.AuthID == first.AuthID {
		t.Fatalf("second: %+v", second)
	}
	e.FinishWarmup(second, 500, "")
	if job := e.WarmupTick(allow("A", "B", "C")); job != nil {
		t.Fatalf("spacing ignored after a failure: %+v", job)
	}
	c.Advance(5 * time.Minute)
	third := e.WarmupTick(allow("A", "B", "C"))
	if third == nil || third.AuthID == first.AuthID || third.AuthID == second.AuthID {
		t.Fatalf("third account not opened (no global daily limit): %+v", third)
	}
	e.FinishWarmup(third, 200, "")
	// The failed one is planned for the next day, not retried now.
	if v := weeklyState(t, e, second.AuthID); v.WarmupLastResult != WarmupFailed || !v.WarmupPlanAt.After(c.Now().Add(20*time.Hour)) {
		t.Fatalf("failure handling: %+v", v)
	}
}

// A window that keeps "ending" (state flapping) is opened at most
// MaxPerAccountPerDay times a day; other accounts are not held back.
func TestWarmupPerAccountDailyLimit(t *testing.T) {
	e, c, events := warmupEngine(t, func(w *WarmupConfig) {
		w.MaxPerAccountPerDay = 1
		w.SpacingMin, w.SpacingMax = 0, 0
	})
	seedWeekly(e, "A", t0.Add(time.Minute), 10)
	seedWeekly(e, "B", t0.Add(48*time.Hour), 10)
	c.Advance(11 * time.Minute)
	job := e.WarmupTick(allow("A", "B"))
	if job == nil || job.AuthID != "A" {
		t.Fatalf("first opening: %+v", job)
	}
	e.FinishWarmup(job, 200, "")
	if v := weeklyState(t, e, "A"); v.WarmupToday != 1 {
		t.Fatalf("account count: %+v", v)
	}
	// The window shows up, then "ends" again a few minutes later.
	seedWeekly(e, "A", c.Now().Add(2*time.Minute), 0)
	c.Advance(15 * time.Minute)
	if job := e.WarmupTick(allow("A", "B")); job != nil {
		t.Fatalf("per-account limit ignored: %+v", job)
	}
	// Another account is not affected by A's limit.
	seedWeekly(e, "B", c.Now().Add(time.Minute), 10)
	c.Advance(11 * time.Minute)
	if job := e.WarmupTick(allow("A", "B")); job == nil || job.AuthID != "B" {
		t.Fatalf("B held back by A's limit: %+v", job)
	} else {
		e.FinishWarmup(job, 200, "")
	}
	// Next day, inside the hours, A's new period is opened.
	c.Advance(14 * time.Hour)
	job = e.WarmupTick(allow("A", "B"))
	if job == nil || job.AuthID != "A" {
		t.Fatalf("A not opened the next day: %+v", job)
	}
	e.FinishWarmup(job, 200, "")
	ws := warmupEvents(*events)
	if last := ws[len(ws)-1]; last.AuthID != "A" || last.AccountToday != 1 {
		t.Fatalf("event: %+v", last)
	}
}

// 0.6.0/0.6.1 stored dry-run results; after the upgrade the period is opened for real.
func TestLegacyDryRunPeriodIsOpenedAfterUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	e, c := newTestEngine(t, func(cfg *Config) {
		cfg.StatePath = path
		cfg.Warmup.Enabled = true
		cfg.Warmup.DelayMin, cfg.Warmup.DelayMax = 0, 0
	})
	seedWeekly(e, "A", t0.Add(time.Minute), 10)
	c.Advance(2 * time.Minute)
	e.WarmupTick(allow("A")) // plans the period
	e.mu.Lock()
	acc := e.accounts["A"]
	acc.WarmupDoneFor, acc.WarmupLastResult, acc.WarmupLastAt = acc.WarmupFor, legacyWarmupDryRun, c.Now()
	e.mu.Unlock()
	if err := e.SaveState(); err != nil {
		t.Fatal(err)
	}
	e2 := NewEngine(e.Config())
	e2.SetClock(c.Now)
	if err := e2.LoadState(); err != nil {
		t.Fatal(err)
	}
	if job := e2.WarmupTick(allow("A")); job == nil || job.AuthID != "A" {
		t.Fatalf("legacy dry-run period not opened: %+v", job)
	}
}

func TestManualWarmupAndNotOpened(t *testing.T) {
	e, c, events := warmupEngine(t, func(w *WarmupConfig) { w.Enabled = false })
	seedWeekly(e, "OPEN", t0.Add(48*time.Hour), 10)
	if err := e.ForceWarmup("OPEN"); err != ErrWindowRunning {
		t.Fatalf("manual on a running window: %v", err)
	}
	if e.WarmupWanted() {
		t.Fatal("WarmupWanted with nothing to do")
	}
	if err := e.ForceWarmup("NEW"); err != nil {
		t.Fatal(err)
	}
	job := e.WarmupTick(allow("NEW", "OPEN"))
	if job == nil || !job.Manual || job.AuthID != "NEW" {
		t.Fatalf("manual job: %+v", job)
	}
	e.FinishWarmup(job, 200, "")
	// The response carried no quota headers: no window shows up.
	c.Advance(4 * time.Minute)
	e.WarmupTick(allow("NEW", "OPEN"))
	ws := warmupEvents(*events)
	if len(ws) != 2 || ws[1].Result != WarmupNotOpened {
		t.Fatalf("not-opened follow-up: %+v", ws)
	}
	if v := weeklyState(t, e, "NEW"); v.WarmupLastResult != WarmupNotOpened {
		t.Fatalf("state: %+v", v)
	}
}

func TestWarmupBlockedByUnopenedLast(t *testing.T) {
	cfg, err := ParseConfig([]byte("order: [sessions, unopened_last, reset]\nwarmup_enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) == 0 || !strings.Contains(strings.Join(cfg.Warnings, " "), "unopened_last") {
		t.Fatalf("warning missing: %v", cfg.Warnings)
	}
	e := NewEngine(cfg)
	c := &clock{now: t0}
	e.SetClock(c.Now)
	e.SetRand(func(n int64) int64 { return 0 })
	seedWeekly(e, "A", t0.Add(time.Minute), 10)
	c.Advance(3 * time.Hour)
	if job := e.WarmupTick(allow("A")); job != nil {
		t.Fatalf("unopened_last must block automatic opening: %+v", job)
	}
}

func TestParseWarmupConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
warmup_enabled: true
warmup_model: gpt-test
warmup_reasoning_effort: Minimal
warmup_hours: "22:00-02:00"
warmup_timezone: UTC
warmup_delay: 5m-30m
warmup_spacing: 2m
warmup_max_per_account_per_day: 3
`))
	if err != nil {
		t.Fatal(err)
	}
	w := cfg.Warmup
	if !w.Enabled || w.Model != "gpt-test" || w.ReasoningEffort != "minimal" ||
		w.HoursStart != 22*time.Hour || w.HoursEnd != 2*time.Hour || w.TimeZone != "UTC" ||
		w.DelayMin != 5*time.Minute || w.DelayMax != 30*time.Minute || w.SpacingMin != 2*time.Minute || w.SpacingMax != 2*time.Minute || w.MaxPerAccountPerDay != 3 {
		t.Fatalf("parsed: %+v", w)
	}
	// Wrapping hours: 23:00 and 01:00 are inside, 03:00 is not.
	for h, want := range map[int]bool{23: true, 1: true, 3: false, 21: false} {
		if got := inWarmupHours(w, time.Date(2026, 11, 9, h, 0, 0, 0, time.UTC)); got != want {
			t.Fatalf("hour %d: got %v", h, got)
		}
	}
	if !strings.Contains(cfg.Summary(), "warmup=on") {
		t.Fatalf("summary: %s", cfg.Summary())
	}
	def, _ := ParseConfig(nil)
	if def.Warmup.Enabled || def.Warmup.SpacingMax != 0 || def.Warmup.MaxPerAccountPerDay != 2 || def.Warmup.HoursAll() || !strings.Contains(def.Summary(), "warmup=off") {
		t.Fatalf("defaults: %+v", def.Warmup)
	}
	for _, bad := range []string{
		"warmup_hours: 9-22",
		"warmup_hours: \"25:00-26:00\"",
		"warmup_timezone: Mars/Base",
		"warmup_delay: 30m-5m",
		"warmup_max_per_account_per_day: 0",
		"warmup_check_interval: 10ms",
		"warmup_enabled: true\nwarmup_model: \"\"",
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

var warmupPromptRE = regexp.MustCompile(`^Reply with [0-9]{2,4}\.$`)

func TestWarmupPromptIsRandomNumber(t *testing.T) {
	seen := map[string]bool{}
	for n := warmupPromptMin; n <= warmupPromptMax; n += 997 {
		p := WarmupPrompt(n)
		if !warmupPromptRE.MatchString(p) {
			t.Fatalf("prompt %q", p)
		}
		seen[p] = true
	}
	if len(seen) < 5 {
		t.Fatalf("prompts not varied: %v", seen)
	}
}

// The dashboard settings panel sends JSON like this through CPA's plugin config
// PATCH; the preview route parses it the same way.
func TestWarmupSettingsFromPanel(t *testing.T) {
	base, _ := ParseConfig(nil)
	cfg, err := WithOverrides(base, []byte(`{"warmup_enabled":true,"warmup_model":" gpt-x ","warmup_reasoning_effort":"",`+
		`"warmup_hours":"08:30-24:00","warmup_timezone":"UTC","warmup_delay":"10m-1h","warmup_spacing":"90s","warmup_max_per_account_per_day":4}`))
	if err != nil {
		t.Fatal(err)
	}
	v := configView(cfg).Warmup
	if v.Mode != "on" || v.HoursAll || v.Model != "gpt-x" || v.ReasoningEffort != "" || v.Hours != "08:30-24:00" || v.TimeZone != "UTC" ||
		v.DelayMinSeconds != 600 || v.DelayMaxSeconds != 3600 || v.SpacingMinSeconds != 90 || v.SpacingMaxSeconds != 90 || v.MaxPerAccountPerDay != 4 {
		t.Fatalf("view: %+v", v)
	}
	if _, err := WithOverrides(base, []byte(`{"warmup_delay":"90m-20m"}`)); err == nil {
		t.Fatal("reversed range accepted")
	}
	off, err := WithOverrides(cfg, []byte(`{"warmup_enabled":false}`))
	if err != nil || off.Warmup.Mode() != "off" || off.Warmup.Model != "gpt-x" {
		t.Fatalf("off keeps the other settings: %+v %v", off.Warmup, err)
	}
	// The panel's "no limit" choices.
	all, err := WithOverrides(base, []byte(`{"warmup_hours":"00:00-24:00","warmup_delay":"0s","warmup_spacing":"0s"}`))
	if err != nil {
		t.Fatal(err)
	}
	if v := configView(all).Warmup; !v.HoursAll || v.DelayMaxSeconds != 0 || v.SpacingMaxSeconds != 0 {
		t.Fatalf("no-limit view: %+v", v)
	}
	// Removed keys only warn.
	old, err := ParseConfig([]byte("warmup_dry_run: true\nwarmup_max_per_day: 10\n"))
	if err != nil || !strings.Contains(strings.Join(old.Warnings, " "), "warmup_dry_run") || !strings.Contains(strings.Join(old.Warnings, " "), "warmup_max_per_day") {
		t.Fatalf("removed keys: %v %v", old.Warnings, err)
	}
}
