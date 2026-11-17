package reorder

import (
	"sort"
	"strconv"
	"time"
)

// Analysis events. The plugin writes each one as a single line to CPA's log:
// "codex-reorder <kind> <json>". Requests that keep their account (sticky hits)
// and requests without a session ID are only counted in EventStats.
const (
	// EventPick: a decision that created or moved a binding (new, rebind),
	// routed a prefixed-model request away from it (pinned), kept an exhausted
	// binding because CPA offered nothing else (held), or routed an image
	// request (image).
	EventPick = "pick"
	// EventWindowEnd: a weekly window ended; reports how much of it was used.
	EventWindowEnd = "window_end"
	// EventStats: counters for one interval plus every account's current state.
	EventStats = "stats"
)

// topEntries is how many ranking entries a pick event carries.
const topEntries = 3

// earlyResetGap: a newly observed weekly window whose reset is this much later
// than the stored one, while the stored one has not reached its reset time yet,
// means the stored window was reset early.
const earlyResetGap = time.Hour

// EventSink receives analysis events. It is called with the engine lock held, so
// it must not block or call back into the engine. Payloads are not modified
// after the call.
type EventSink func(kind string, payload any)

// PickEvent is the log form of a Decision.
type PickEvent struct {
	At        time.Time  `json:"at"`
	Kind      string     `json:"kind"`
	Session   string     `json:"session,omitempty"`
	AuthID    string     `json:"auth"`
	From      string     `json:"from,omitempty"`
	Model     string     `json:"model,omitempty"`
	DecidedBy string     `json:"by,omitempty"`
	Reason    string     `json:"reason"`
	Top       []TopEntry `json:"top,omitempty"`
	// Lineage is set for new sessions that reported a parent or subagent signal.
	Lineage *LineageView `json:"lineage,omitempty"`
}

// TopEntry is one of the first ranking entries of a pick event.
type TopEntry struct {
	AuthID           string    `json:"auth"`
	Load             int       `json:"load"`
	WeeklyState      string    `json:"state"`
	RemainingPercent float64   `json:"remaining_pct"`
	Multiplier       float64   `json:"mult"`
	ResetAt          time.Time `json:"reset"`
	UrgencyAt        time.Time `json:"urgency"`
	Exhausted        bool      `json:"exhausted,omitempty"`
}

// WindowEndEvent reports a weekly window that has ended.
type WindowEndEvent struct {
	AuthID  string    `json:"auth"`
	ResetAt time.Time `json:"reset_at"`
	// Early is set when the window was replaced before its announced reset time.
	Early bool `json:"early,omitempty"`
	// UsedPercent is the last value seen before the window ended. Use outside this
	// CPA is only included as far as the last response reported it.
	UsedPercent    float64   `json:"used_pct"`
	LastObservedAt time.Time `json:"last_observed_at"`
	Multiplier     float64   `json:"mult"`
	// Unused is (100 − UsedPercent) × Multiplier: quota that expired unused, in
	// percent of a 1x account.
	Unused float64 `json:"unused"`
}

// Stats is one interval of counters plus the current state of every account.
type Stats struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Picks counts every request the plugin routed; Kinds splits them by decision
	// kind (sticky = kept its account).
	Picks int64            `json:"picks"`
	Kinds map[string]int64 `json:"kinds"`
	// Usage counts usage records (finished upstream attempts); Failures splits the
	// failed ones by HTTP status ("error" when CPA reported none).
	Usage int64 `json:"usage"`
	// NewWithParent counts new sessions that reported a parent session;
	// NewParentKnown those whose parent is an active session in the plugin;
	// NewSubagent those with a fork or Codex subagent signal.
	NewWithParent  int64            `json:"new_with_parent,omitempty"`
	NewParentKnown int64            `json:"new_parent_known,omitempty"`
	NewSubagent    int64            `json:"new_subagent,omitempty"`
	Failures       map[string]int64 `json:"failures,omitempty"`
	ActiveSessions int              `json:"active_sessions"`
	// Accounts lists every account on full lines (hourly), otherwise only the
	// accounts that were active during the interval.
	Accounts []AccountStats `json:"accounts"`
	Full     bool           `json:"full,omitempty"`
	// DroppedLines is filled in by the plugin: log lines lost because its queue was full.
	DroppedLines int64 `json:"dropped_lines,omitempty"`
}

// AccountStats is one account's line in Stats.
type AccountStats struct {
	AuthID string `json:"auth"`
	// Active is the number of sessions served now; Peak the highest number during the interval.
	Active   int              `json:"active,omitempty"`
	Peak     int              `json:"peak,omitempty"`
	Picks    int64            `json:"picks,omitempty"`
	New      int64            `json:"new,omitempty"`
	OK       int64            `json:"ok,omitempty"`
	Failures map[string]int64 `json:"failures,omitempty"`
	// WeeklyState is one of WeeklyOpen, WeeklyProvisional, WeeklyRefreshed, WeeklyIdle, WeeklyUnknown.
	WeeklyState string     `json:"weekly_state"`
	WeeklyUsed  *float64   `json:"weekly_used_pct,omitempty"`
	WeeklyReset *time.Time `json:"weekly_reset,omitempty"`
	ShortUsed   *float64   `json:"short_used_pct,omitempty"`
	ShortReset  *time.Time `json:"short_reset,omitempty"`
	// ImageUsed is the running image generation window's use (see image.go).
	ImageUsed  *float64 `json:"image_used_pct,omitempty"`
	Multiplier float64  `json:"mult"`
}

type statCounters struct {
	since          time.Time
	picks          int64
	usage          int64
	newWithParent  int64
	newParentKnown int64
	newSubagent    int64
	kinds          map[string]int64
	failures       map[string]int64
	accounts       map[string]*accountCounters
}

type accountCounters struct {
	picks    int64
	new      int64
	ok       int64
	failures map[string]int64
	peak     int
}

// SetEventSink installs the receiver of analysis events (nil disables them).
func (e *Engine) SetEventSink(sink EventSink) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sink = sink
}

func (e *Engine) emitLocked(kind string, payload any) {
	if e.sink != nil {
		e.sink(kind, payload)
	}
}

func (e *Engine) countersLocked(now time.Time) *statCounters {
	if e.stats == nil {
		e.stats = &statCounters{since: now}
	}
	return e.stats
}

func (c *statCounters) account(id string) *accountCounters {
	if c.accounts == nil {
		c.accounts = make(map[string]*accountCounters)
	}
	a := c.accounts[id]
	if a == nil {
		a = &accountCounters{}
		c.accounts[id] = a
	}
	return a
}

func increment(m *map[string]int64, key string) {
	if *m == nil {
		*m = make(map[string]int64)
	}
	(*m)[key]++
}

// countPickLocked records one routed request. Kinds that add a session to the
// chosen account update its peak.
func (e *Engine) countPickLocked(res PickResult, now time.Time) {
	if !res.Handled || res.AuthID == "" {
		return
	}
	c := e.countersLocked(now)
	if res.Kind == KindWarmup {
		// Window-opening requests are counted apart and not as traffic.
		increment(&c.kinds, res.Kind)
		return
	}
	c.picks++
	increment(&c.kinds, res.Kind)
	a := c.account(res.AuthID)
	a.picks++
	switch res.Kind {
	case KindNew:
		a.new++
	case KindSticky, KindNoSession, KindPinned, KindHeld, KindImage:
		// The chosen account's session count did not change.
		return
	}
	load := 0
	for _, s := range e.sessions {
		if s.AuthID == res.AuthID && e.sessionActive(s, now) {
			load++
		}
	}
	if load > a.peak {
		a.peak = load
	}
}

func (e *Engine) countUsageLocked(u UsageInput, now time.Time) {
	c := e.countersLocked(now)
	c.usage++
	a := c.account(u.AuthID)
	if !u.Failed {
		a.ok++
		return
	}
	status := "error"
	if u.StatusCode > 0 {
		status = strconv.Itoa(u.StatusCode)
	}
	increment(&c.failures, status)
	increment(&a.failures, status)
}

// logTime keeps log lines short: UTC, whole seconds.
func logTime(t time.Time) time.Time {
	return t.UTC().Truncate(time.Second)
}

// pickEvent converts a decision into its log form.
func pickEvent(d Decision) PickEvent {
	ev := PickEvent{
		At:        logTime(d.At),
		Kind:      d.Kind,
		Session:   d.Session,
		AuthID:    d.AuthID,
		From:      d.FromAuthID,
		Model:     d.Model,
		DecidedBy: d.DecidedBy,
		Reason:    d.Reason,
		Lineage:   d.Lineage,
	}
	for i, r := range d.Ranking {
		if i == topEntries {
			break
		}
		ev.Top = append(ev.Top, TopEntry{
			AuthID:           r.AuthID,
			Load:             r.Load,
			WeeklyState:      r.WeeklyState,
			RemainingPercent: r.RemainingPercent,
			Multiplier:       r.Multiplier,
			ResetAt:          logTime(r.ResetAt),
			UrgencyAt:        logTime(r.UrgencyAt),
			Exhausted:        r.Exhausted,
		})
	}
	return ev
}

// closeWeeklyLocked emits EventWindowEnd once for the account's stored weekly
// window when it has ended by now, or when replaced reports that a newer window
// replaces it.
func (e *Engine) closeWeeklyLocked(acc *account, now time.Time, replaced bool) {
	w := acc.Weekly
	if w == nil || w.ResetAt.IsZero() || sameWindow(acc.WeeklyEndLogged, w.ResetAt) {
		return
	}
	early := w.ResetAt.After(now)
	if early && !replaced {
		return
	}
	acc.WeeklyEndLogged = w.ResetAt
	e.dirty = true
	mult := e.cfg.Multiplier(acc.ID)
	e.emitLocked(EventWindowEnd, WindowEndEvent{
		AuthID:         acc.ID,
		ResetAt:        logTime(w.ResetAt),
		Early:          early,
		UsedPercent:    w.UsedPercent,
		LastObservedAt: logTime(w.ObservedAt),
		Multiplier:     mult,
		Unused:         clampPercent(100-w.UsedPercent) * mult,
	})
}

// sameWindow treats reset times less than earlyResetGap apart as the same
// window: resets derived from "reset after N seconds" drift by a few seconds.
func sameWindow(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	d := a.Sub(b)
	return d < earlyResetGap && d > -earlyResetGap
}

// CloseEndedWindows emits EventWindowEnd for weekly windows that have ended.
// The plugin calls it periodically whether or not stats lines are enabled.
func (e *Engine) CloseEndedWindows() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for _, acc := range e.accounts {
		e.closeWeeklyLocked(acc, now, false)
	}
}

// DiscardStats starts a new interval without reporting the current one (stats
// lines disabled), so counters never cover more than one check period.
func (e *Engine) DiscardStats() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.resetCountersLocked(now, e.loadsLocked(now))
}

// TakeStats closes the current interval with every account's current state;
// see TakeStatsMode.
func (e *Engine) TakeStats() *Stats { return e.TakeStatsMode(true) }

// TakeStatsMode closes the current interval: it emits EventWindowEnd for weekly
// windows that have ended, then returns the interval's counters and starts a new
// interval. With full, every account's current state is included; otherwise only
// accounts that served sessions or had picks, usage or failures in the interval.
// It returns nil when no request was routed and no usage was reported.
func (e *Engine) TakeStatsMode(full bool) *Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for _, acc := range e.accounts {
		e.closeWeeklyLocked(acc, now, false)
	}
	c := e.countersLocked(now)
	loads := e.loadsLocked(now)
	defer e.resetCountersLocked(now, loads)
	if c.picks == 0 && c.usage == 0 {
		return nil
	}
	st := &Stats{
		From:           logTime(c.since),
		To:             logTime(now),
		Picks:          c.picks,
		Kinds:          c.kinds,
		Usage:          c.usage,
		Failures:       c.failures,
		NewWithParent:  c.newWithParent,
		NewParentKnown: c.newParentKnown,
		NewSubagent:    c.newSubagent,
	}
	if st.Kinds == nil {
		st.Kinds = map[string]int64{}
	}
	st.Full = full
	for _, n := range loads {
		st.ActiveSessions += n
	}
	for id, acc := range e.accounts {
		entry := rankEntry(e.cfg, acc, loads, now)
		as := AccountStats{
			AuthID:      id,
			Active:      loads[id],
			Peak:        loads[id],
			WeeklyState: entry.WeeklyState,
			Multiplier:  entry.Multiplier,
		}
		a := c.accounts[id]
		if !full && loads[id] == 0 && (a == nil || (a.picks == 0 && a.ok == 0 && len(a.failures) == 0 && a.peak == 0)) {
			continue
		}
		if a != nil {
			as.Picks, as.New, as.OK, as.Failures = a.picks, a.new, a.ok, a.failures
			if a.peak > as.Peak {
				as.Peak = a.peak
			}
		}
		if acc.Image.Active(now) {
			used := acc.Image.UsedPercent
			as.ImageUsed = &used
		}
		if acc.Weekly.Active(now) {
			used, reset := acc.Weekly.UsedPercent, logTime(acc.Weekly.ResetAt)
			as.WeeklyUsed, as.WeeklyReset = &used, &reset
		}
		if acc.Short.Active(now) {
			used, reset := acc.Short.UsedPercent, logTime(acc.Short.ResetAt)
			as.ShortUsed, as.ShortReset = &used, &reset
		}
		st.Accounts = append(st.Accounts, as)
	}
	sort.Slice(st.Accounts, func(i, j int) bool { return st.Accounts[i].AuthID < st.Accounts[j].AuthID })
	return st
}

// resetCountersLocked starts a new interval; each account's peak starts at its
// current session count.
func (e *Engine) resetCountersLocked(now time.Time, loads map[string]int) {
	c := &statCounters{since: now}
	for id, n := range loads {
		if n > 0 {
			c.account(id).peak = n
		}
	}
	e.stats = c
}
