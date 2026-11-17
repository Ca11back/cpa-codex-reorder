package reorder

import "time"

// EventProbe: the result of one quota query, written to CPA's log.
const EventProbe = "probe"

// ProbeEvent is the log form of a quota query result.
type ProbeEvent struct {
	At          time.Time  `json:"at"`
	AuthID      string     `json:"auth"`
	Error       string     `json:"error,omitempty"`
	WeeklyState string     `json:"weekly_state,omitempty"`
	WeeklyUsed  *float64   `json:"weekly_used_pct,omitempty"`
	WeeklyReset *time.Time `json:"weekly_reset,omitempty"`
	ShortUsed   *float64   `json:"short_used_pct,omitempty"`
	ShortReset  *time.Time `json:"short_reset,omitempty"`
	// ShortState is one of the Short* states (open, idle, none, unknown).
	ShortState string `json:"short_state,omitempty"`
	// Ignored lists windows dropped because a request reported newer data meanwhile.
	Ignored []string `json:"ignored,omitempty"`
}

// ApplyProbe records a quota query result for an account. observedAt is when the
// query was sent. Data observed later (from requests) is never overwritten, and
// a query that finds no running weekly window does not undo an assignment made
// after it was sent.
func (e *Engine) ApplyProbe(authID string, res ProbeResult, observedAt time.Time) AccountView {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	if observedAt.IsZero() || observedAt.After(now) {
		observedAt = now
	}
	acc := e.ensureAccountLocked(authID)
	acc.LastProbeAt = observedAt
	acc.LastProbeError = ""
	ev := ProbeEvent{At: logTime(now), AuthID: authID}

	switch {
	case res.Weekly != nil && !res.WeeklyIdle:
		if acc.Weekly == nil || !res.Weekly.ObservedAt.Before(acc.Weekly.ObservedAt) {
			if acc.Weekly != nil {
				e.closeWeeklyLocked(acc, now, res.Weekly.ResetAt.Sub(acc.Weekly.ResetAt) > earlyResetGap)
			}
			acc.Weekly = res.Weekly
			acc.ProvisionalOpenAt = time.Time{}
			acc.WeeklyIdleAt = time.Time{}
			if res.Weekly.Active(now) {
				acc.clearWarmupPlan()
			}
		} else {
			ev.Ignored = append(ev.Ignored, "weekly")
		}
	case res.WeeklyIdle:
		newerWindow := acc.Weekly != nil && acc.Weekly.ObservedAt.After(observedAt)
		assignedSince := acc.ProvisionalOpenAt.After(observedAt)
		if newerWindow || assignedSince {
			ev.Ignored = append(ev.Ignored, "weekly")
			break
		}
		if acc.Weekly != nil {
			// The stored window has ended, possibly early: report it once.
			e.closeWeeklyLocked(acc, now, true)
			acc.Weekly = nil
		}
		acc.ProvisionalOpenAt = time.Time{}
		acc.WeeklyIdleAt = observedAt
	}
	newerShort := acc.Short != nil && acc.Short.ObservedAt.After(observedAt)
	switch {
	case res.Short != nil && !res.ShortIdle:
		if !newerShort {
			acc.Short = res.Short
			acc.ShortStatus = ""
		} else {
			ev.Ignored = append(ev.Ignored, "short")
		}
	case res.ShortIdle || res.NoShort:
		if newerShort {
			ev.Ignored = append(ev.Ignored, "short")
			break
		}
		acc.Short = nil
		acc.ShortStatus = ShortStateIdle
		if res.NoShort {
			acc.ShortStatus = ShortStateNone
		}
	}
	e.dirty = true

	loads := e.loadsLocked(now)
	entry := rankEntry(e.cfg, acc, loads, now)
	ev.WeeklyState = entry.WeeklyState
	if acc.Weekly.Active(now) {
		used, reset := acc.Weekly.UsedPercent, logTime(acc.Weekly.ResetAt)
		ev.WeeklyUsed, ev.WeeklyReset = &used, &reset
	}
	if acc.Short.Active(now) {
		used, reset := acc.Short.UsedPercent, logTime(acc.Short.ResetAt)
		ev.ShortUsed, ev.ShortReset = &used, &reset
	}
	ev.ShortState = shortState(acc, now)
	e.emitLocked(EventProbe, ev)
	return e.accountViewLocked(acc, entry, loads)
}

// 5h window states shown on the dashboard. The 5h window only matters for
// ranking when it is exhausted; these states are informational.
const (
	ShortStateOpen    = "open"
	ShortStateIdle    = "idle"    // the window exists but is not running (no use in the last 5h)
	ShortStateNone    = "none"    // the account has no 5h limit (weekly-only plan)
	ShortStateUnknown = "unknown" // nothing known yet
)

func shortState(acc *account, now time.Time) string {
	switch {
	case acc.Short.Active(now):
		return ShortStateOpen
	case acc.ShortStatus == ShortStateNone:
		return ShortStateNone
	case acc.ShortStatus == ShortStateIdle, acc.Short != nil:
		// A stored window whose reset has passed is not running either.
		return ShortStateIdle
	default:
		return ShortStateUnknown
	}
}

// RecordProbeError records a failed quota query; the stored quota is kept.
func (e *Engine) RecordProbeError(authID, message string) AccountView {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	acc := e.ensureAccountLocked(authID)
	acc.LastProbeAt = now
	acc.LastProbeError = message
	e.dirty = true
	e.emitLocked(EventProbe, ProbeEvent{At: logTime(now), AuthID: authID, Error: message})
	loads := e.loadsLocked(now)
	return e.accountViewLocked(acc, rankEntry(e.cfg, acc, loads, now), loads)
}

// ProbeInfo describes an account for the quota query planner.
type ProbeInfo struct {
	// DataAt is when the account's quota data was last updated (from a request
	// or a query), zero when there is none.
	DataAt         time.Time
	LastProbeAt    time.Time
	LastProbeError string
}

// ProbeInfo returns what the plugin knows about an account's quota freshness.
func (e *Engine) ProbeInfo(authID string) ProbeInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	acc := e.accounts[authID]
	if acc == nil {
		return ProbeInfo{}
	}
	info := ProbeInfo{DataAt: acc.WeeklyIdleAt, LastProbeAt: acc.LastProbeAt, LastProbeError: acc.LastProbeError}
	if acc.Weekly != nil && acc.Weekly.ObservedAt.After(info.DataAt) {
		info.DataAt = acc.Weekly.ObservedAt
	}
	return info
}
