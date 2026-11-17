package reorder

import (
	"errors"
	"math/rand/v2"
	"time"
)

// KindWarmup marks the scheduler pick of a window-opening request.
const KindWarmup = "warmup"

// EventWarmup: one window-opening attempt.
const EventWarmup = "warmup"

// Warmup results stored per account and logged.
const (
	WarmupSent      = "sent"       // request answered 2xx
	WarmupFailed    = "failed"     // request failed; planned again for the next day
	WarmupNotOpened = "not_opened" // sent, but no running weekly window showed up afterwards
	// legacyWarmupDryRun was stored by 0.6.0/0.6.1 dry runs (removed in 0.6.2).
	legacyWarmupDryRun = "dry_run"
)

const (
	// warmupPendingFor bounds how long a scheduler pick for a single pinned
	// account counts as the window-opening request.
	warmupPendingFor = 2 * time.Minute
	// warmupVerifyAfter is when a sent request without a visible window counts as not opened.
	warmupVerifyAfter = 3 * time.Minute
	// warmupLateWindow caps the random offset when a plan is moved to the start
	// of the next allowed hours.
	warmupLateWindow = time.Hour
)

// ErrWindowRunning: a manual opening was asked for an account whose weekly window is running.
var ErrWindowRunning = errors.New("weekly window is already running")

// WarmupJob is one window-opening request the plugin has to send.
type WarmupJob struct {
	AuthID          string
	Model           string
	ReasoningEffort string
	Prompt          string
	Manual          bool
	PlanAt          time.Time
	// Period is the start of the unopened period the request is for.
	Period time.Time
}

// WarmupEvent is the log form of an attempt.
type WarmupEvent struct {
	At         time.Time `json:"at"`
	AuthID     string    `json:"auth"`
	Result     string    `json:"result"`
	Manual     bool      `json:"manual,omitempty"`
	Model      string    `json:"model,omitempty"`
	Effort     string    `json:"effort,omitempty"`
	Prompt     string    `json:"prompt,omitempty"`
	HTTPStatus int       `json:"http_status,omitempty"`
	Error      string    `json:"error,omitempty"`
	PlanAt     time.Time `json:"plan_at,omitempty"`
	Period     time.Time `json:"period,omitempty"`
	TodayCount int       `json:"today_count"`
	// AccountToday counts this account's automatic attempts today.
	AccountToday int `json:"account_today,omitempty"`
}

// WarmupStatus is the engine-wide part shown on the dashboard.
type WarmupStatus struct {
	Today  int       `json:"today"`
	NextAt time.Time `json:"next_at,omitempty"`
}

type warmupPersist struct {
	Day    string    `json:"day,omitempty"`
	Count  int       `json:"count,omitempty"`
	NextAt time.Time `json:"next_at,omitempty"`
}

func (e *Engine) randN(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if e.rnd != nil {
		return e.rnd(n)
	}
	return rand.Int64N(n)
}

func (e *Engine) randBetween(lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + time.Duration(e.randN(int64(hi-lo)+1))
}

// SetRand replaces the random source (tests only).
func (e *Engine) SetRand(r func(n int64) int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rnd = r
}

// unopenedSince reports whether the account's weekly window is not running and
// a marker for the start of that unopened period.
func (e *Engine) unopenedSince(acc *account, now time.Time) (time.Time, bool) {
	switch rankEntry(e.cfg, acc, nil, now).WeeklyState {
	case WeeklyRefreshed:
		return acc.Weekly.ResetAt, true
	case WeeklyIdle:
		return acc.WeeklyIdleAt, true
	case WeeklyUnknown:
		return now, true
	}
	return time.Time{}, false
}

// warmupDay is the calendar day of t in the warmup time zone.
func warmupDay(w WarmupConfig, t time.Time) string {
	return t.In(w.Location).Format("2006-01-02")
}

// warmupToday is how many automatic attempts the account had on day.
func (a *account) warmupToday(day string) int {
	if a.WarmupDay != day {
		return 0
	}
	return a.WarmupDayCount
}

// countWarmup records one automatic attempt on day.
func (a *account) countWarmup(day string) {
	if a.WarmupDay != day {
		a.WarmupDay, a.WarmupDayCount = day, 0
	}
	a.WarmupDayCount++
}

// inWarmupHours reports whether t falls inside the allowed hours.
func inWarmupHours(w WarmupConfig, t time.Time) bool {
	if w.HoursStart == w.HoursEnd {
		return true
	}
	local := t.In(w.Location)
	tod := time.Duration(local.Hour())*time.Hour + time.Duration(local.Minute())*time.Minute + time.Duration(local.Second())*time.Second
	if w.HoursStart < w.HoursEnd {
		return tod >= w.HoursStart && tod < w.HoursEnd
	}
	return tod >= w.HoursStart || tod < w.HoursEnd
}

// nextWarmupStart is the next start of the allowed hours after t.
func nextWarmupStart(w WarmupConfig, t time.Time) time.Time {
	local := t.In(w.Location)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, w.Location)
	start := midnight.Add(w.HoursStart)
	if !start.After(t) {
		start = time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, w.Location).Add(w.HoursStart)
	}
	return start
}

// warmupLength is the length of the allowed hours.
func warmupLength(w WarmupConfig) time.Duration {
	switch {
	case w.HoursStart == w.HoursEnd:
		return 24 * time.Hour
	case w.HoursStart < w.HoursEnd:
		return w.HoursEnd - w.HoursStart
	default:
		return 24*time.Hour - w.HoursStart + w.HoursEnd
	}
}

// planWarmupLocked moves t into the allowed hours: unchanged when inside, else
// the next start plus a random offset (at most an hour, within the hours).
func (e *Engine) planWarmupLocked(t time.Time) time.Time {
	w := e.cfg.Warmup
	if inWarmupHours(w, t) {
		return t
	}
	late := warmupLateWindow
	if l := warmupLength(w); l < late {
		late = l
	}
	return nextWarmupStart(w, t).Add(e.randBetween(0, late-time.Second))
}

// WarmupTick updates every account's plan and returns at most one request to
// send now. allowed lists the accounts CPA would currently use (enabled, not
// cooling). Manual requests come first and ignore hours, spacing and the daily
// limit.
func (e *Engine) WarmupTick(allowed map[string]bool) *WarmupJob {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	w := e.cfg.Warmup
	auto := w.Enabled && !e.cfg.warmupBlockedByOrder()
	day := warmupDay(w, now)
	if day != e.warmup.Day {
		e.warmup.Day, e.warmup.Count = day, 0
		e.dirty = true
	}
	for id := range allowed {
		e.ensureAccountLocked(id)
	}

	var forced, due *account
	for id, acc := range e.accounts {
		if !allowed[id] {
			continue
		}
		since, unopened := e.unopenedSince(acc, now)
		if !unopened || acc.exhausted(now) {
			if !acc.WarmupFor.IsZero() || acc.WarmupForced {
				acc.clearWarmupPlan()
				e.dirty = true
			}
			continue
		}
		if acc.WarmupFor.IsZero() {
			acc.WarmupFor, acc.WarmupPlanAt, acc.WarmupDoneFor = since, time.Time{}, time.Time{}
			e.dirty = true
		}
		done := acc.WarmupDoneFor.Equal(acc.WarmupFor)
		if auto && !done && acc.WarmupPlanAt.IsZero() {
			acc.WarmupPlanAt = e.planWarmupLocked(acc.WarmupFor.Add(e.randBetween(w.DelayMin, w.DelayMax)))
			e.dirty = true
		}
		if done && acc.WarmupLastResult == WarmupSent && now.Sub(acc.WarmupLastAt) >= warmupVerifyAfter {
			acc.WarmupLastResult = WarmupNotOpened
			e.dirty = true
			e.emitLocked(EventWarmup, WarmupEvent{At: logTime(now), AuthID: id, Result: WarmupNotOpened, Period: logTime(acc.WarmupFor), TodayCount: e.warmup.Count})
		}
		if _, busy := e.pendingWarmup[id]; busy {
			continue
		}
		switch {
		case acc.WarmupForced:
			if forced == nil {
				forced = acc
			}
		case auto && !done && !acc.WarmupPlanAt.After(now) && acc.warmupToday(day) < w.MaxPerAccountPerDay:
			if due == nil || acc.WarmupPlanAt.Before(due.WarmupPlanAt) || (acc.WarmupPlanAt.Equal(due.WarmupPlanAt) && id < due.ID) {
				due = acc
			}
		}
	}

	acc, manual := forced, true
	if acc == nil {
		manual = false
		if due == nil || now.Before(e.warmup.NextAt) {
			return nil
		}
		if !inWarmupHours(w, now) {
			// The plan passed while outside the hours (e.g. CPA was down): move it.
			due.WarmupPlanAt = e.planWarmupLocked(now)
			e.dirty = true
			return nil
		}
		acc = due
	}
	prompt := WarmupPrompt(warmupPromptMin + int(e.randN(warmupPromptMax-warmupPromptMin+1)))
	job := &WarmupJob{AuthID: acc.ID, Model: w.Model, ReasoningEffort: w.ReasoningEffort, Prompt: prompt, Manual: manual, PlanAt: acc.WarmupPlanAt, Period: acc.WarmupFor}
	acc.WarmupForced = false
	e.dirty = true
	if e.pendingWarmup == nil {
		e.pendingWarmup = make(map[string]time.Time)
	}
	e.pendingWarmup[acc.ID] = now
	return job
}

// FinishWarmup records the outcome of a sent request. errMsg is empty when CPA
// answered; status is the HTTP status. A failure is planned again for the next
// day; there is no other retry.
func (e *Engine) FinishWarmup(job *WarmupJob, status int, errMsg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	delete(e.pendingWarmup, job.AuthID)
	acc := e.ensureAccountLocked(job.AuthID)
	acc.WarmupLastAt = now
	ok := errMsg == "" && status >= 200 && status < 300
	if ok {
		acc.WarmupLastResult, acc.WarmupLastError = WarmupSent, ""
		if acc.WarmupFor.Equal(job.Period) {
			acc.WarmupDoneFor = job.Period
		}
	} else {
		if errMsg == "" {
			errMsg = "HTTP " + itoa(status)
		}
		if len(errMsg) > 200 {
			errMsg = errMsg[:200]
		}
		acc.WarmupLastResult, acc.WarmupLastError = WarmupFailed, errMsg
		if acc.WarmupFor.Equal(job.Period) {
			acc.WarmupPlanAt = e.planWarmupLocked(now.Add(24 * time.Hour))
		}
	}
	if !job.Manual {
		acc.countWarmup(warmupDay(e.cfg.Warmup, now))
		e.warmup.Count++
		w := e.cfg.Warmup
		e.warmup.NextAt = now.Add(e.randBetween(w.SpacingMin, w.SpacingMax))
	}
	e.dirty = true
	result := WarmupSent
	if !ok {
		result = WarmupFailed
	}
	e.emitLocked(EventWarmup, WarmupEvent{At: logTime(now), AuthID: job.AuthID, Result: result, Manual: job.Manual, Model: job.Model, Effort: job.ReasoningEffort,
		Prompt: job.Prompt, HTTPStatus: status, Error: acc.WarmupLastError, PlanAt: logTime(job.PlanAt), Period: logTime(job.Period), TodayCount: e.warmup.Count,
		AccountToday: acc.warmupToday(warmupDay(e.cfg.Warmup, now))})
}

// ForceWarmup queues a manual window opening for the account (sent by the next
// WarmupTick, regardless of hours, spacing and the daily limit).
func (e *Engine) ForceWarmup(authID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	acc := e.ensureAccountLocked(authID)
	if _, unopened := e.unopenedSince(acc, now); !unopened {
		return ErrWindowRunning
	}
	acc.WarmupForced = true
	e.dirty = true
	return nil
}

// WarmupWanted reports whether WarmupTick has anything to do (automatic opening
// enabled or a manual request queued), so the plugin can skip reading CPA's
// credential list otherwise.
func (e *Engine) WarmupWanted() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cfg.Warmup.Enabled {
		return true
	}
	for _, acc := range e.accounts {
		if acc.WarmupForced {
			return true
		}
	}
	return false
}

// warmupPickLocked recognises the scheduler pick of a window-opening request:
// one pinned candidate that has a request in flight.
func (e *Engine) warmupPickLocked(ids []string, now time.Time) (string, bool) {
	if len(ids) != 1 {
		return "", false
	}
	at, ok := e.pendingWarmup[ids[0]]
	return ids[0], ok && now.Sub(at) < warmupPendingFor
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
