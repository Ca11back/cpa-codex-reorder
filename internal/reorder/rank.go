package reorder

import (
	"fmt"
	"sort"
	"time"
)

const maxRankingEntries = 12

// Weekly window states as shown on the dashboard.
const (
	// WeeklyOpen: a weekly window is running and its reset time is known.
	WeeklyOpen = "open"
	// WeeklyProvisional: the plugin just routed to the account and assumes its
	// window opened then; real data arrives with the first response.
	WeeklyProvisional = "provisional"
	// WeeklyRefreshed: the last known window has reset and nothing has used the
	// account since, so no window is running.
	WeeklyRefreshed = "refreshed"
	// WeeklyIdle: a quota query reported that no weekly window is running.
	WeeklyIdle = "idle"
	// WeeklyUnknown: no quota data has been seen for this account yet.
	WeeklyUnknown = "unknown"
)

// RankEntry records one candidate's values for every ranking field.
type RankEntry struct {
	AuthID   string `json:"auth_id"`
	Load     int    `json:"load"`
	Priority int    `json:"priority"`
	// WeeklyState is one of WeeklyOpen, WeeklyProvisional, WeeklyRefreshed, WeeklyIdle, WeeklyUnknown.
	WeeklyState string `json:"weekly_state"`
	// Opened reports a running weekly window (open or provisional).
	Opened bool `json:"opened"`
	// ResetAt is the weekly reset; accounts without a running window use now + 7 days.
	ResetAt time.Time `json:"reset_at"`
	// RemainingPercent is the weekly quota left (100 when no window is running).
	RemainingPercent float64 `json:"remaining_percent"`
	Multiplier       float64 `json:"multiplier"`
	// Remaining is RemainingPercent × Multiplier, in "1x account percent" units.
	Remaining float64 `json:"remaining"`
	// UrgencyAt is ResetAt minus Remaining converted to time.
	UrgencyAt time.Time `json:"urgency_at"`
	Exhausted bool      `json:"exhausted,omitempty"`

	lastAssigned time.Time
}

// Decision is one entry of the decision log shown on the dashboard.
type Decision struct {
	At         time.Time   `json:"at"`
	Kind       string      `json:"kind"`
	Session    string      `json:"session,omitempty"`
	AuthID     string      `json:"auth_id"`
	FromAuthID string      `json:"from_auth_id,omitempty"`
	Model      string      `json:"model,omitempty"`
	Reason     string      `json:"reason"`
	DecidedBy  string      `json:"decided_by,omitempty"`
	Ranking    []RankEntry `json:"ranking,omitempty"`
	// Lineage is set for new sessions that reported a parent or a subagent signal.
	Lineage  *LineageView `json:"lineage,omitempty"`
	failover *FailoverDetail
}

// Pseudo-fields reported by DecidedBy besides the configured order fields.
const (
	decidedOnly      = "only"
	decidedExhausted = "exhausted"
	decidedFallback  = "fallback"
)

// rankEntry computes the ranking values of one account under cfg.
func rankEntry(cfg Config, acc *account, loads map[string]int, now time.Time) RankEntry {
	id := acc.ID
	entry := RankEntry{
		AuthID:           id,
		Load:             loads[id],
		Priority:         acc.Priority,
		ResetAt:          now.Add(weekLength),
		RemainingPercent: 100,
		Multiplier:       cfg.Multiplier(id),
		Exhausted:        acc.exhausted(now),
		lastAssigned:     acc.LastAssignedAt,
	}
	switch {
	case acc.Weekly.Active(now):
		entry.WeeklyState = WeeklyOpen
		entry.Opened = true
		entry.ResetAt = acc.Weekly.ResetAt
		entry.RemainingPercent = clampPercent(100 - acc.Weekly.UsedPercent)
	case !acc.ProvisionalOpenAt.IsZero() && acc.ProvisionalOpenAt.Add(weekLength).After(now):
		entry.WeeklyState = WeeklyProvisional
		entry.Opened = true
		entry.ResetAt = acc.ProvisionalOpenAt.Add(weekLength)
	case acc.Weekly != nil:
		entry.WeeklyState = WeeklyRefreshed
	case !acc.WeeklyIdleAt.IsZero():
		entry.WeeklyState = WeeklyIdle
	default:
		entry.WeeklyState = WeeklyUnknown
	}
	entry.Remaining = entry.RemainingPercent * entry.Multiplier
	shift := time.Duration(entry.Remaining * cfg.UrgencyMinutesPerPercent * float64(time.Minute))
	entry.UrgencyAt = entry.ResetAt.Add(-shift)
	return entry
}

func clampPercent(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// compareField returns -1 when a ranks before b on field f, 1 when after, 0 on a tie.
func compareField(f string, a, b *RankEntry) int {
	switch f {
	case FieldSessions:
		return cmpInt(a.Load, b.Load)
	case FieldPriority:
		return cmpInt(b.Priority, a.Priority)
	case FieldUnopenedFirst:
		return cmpBool(!a.Opened, !b.Opened)
	case FieldUnopenedLast:
		return cmpBool(a.Opened, b.Opened)
	case FieldUrgency:
		return cmpTime(a.UrgencyAt, b.UrgencyAt)
	case FieldReset:
		return cmpTime(a.ResetAt, b.ResetAt)
	case FieldRemaining:
		switch {
		case a.Remaining > b.Remaining:
			return -1
		case a.Remaining < b.Remaining:
			return 1
		}
	}
	return 0
}

// compareEntries orders two candidates and names the rule that separated them.
// Fixed rules: an account known to be exhausted is always last; after the
// configured fields, the account assigned least recently wins, then the auth ID.
func compareEntries(order []string, a, b *RankEntry) (int, string) {
	if c := cmpBool(!a.Exhausted, !b.Exhausted); c != 0 {
		return c, decidedExhausted
	}
	for _, f := range order {
		if c := compareField(f, a, b); c != 0 {
			return c, f
		}
	}
	if c := cmpTime(a.lastAssigned, b.lastAssigned); c != 0 {
		return c, decidedFallback
	}
	if a.AuthID < b.AuthID {
		return -1, decidedFallback
	}
	if a.AuthID > b.AuthID {
		return 1, decidedFallback
	}
	return 0, decidedFallback
}

// rankLocked orders candidate account IDs; the first entry is the choice for a
// new session. ids must be non-empty and every ID must exist in e.accounts. The
// returned string names the rule that put the first entry ahead of the second.
func (e *Engine) rankLocked(ids []string, loads map[string]int, now time.Time) ([]RankEntry, string) {
	return rankAccounts(e.cfg, e.accounts, ids, loads, now)
}

// rankAccounts orders ids under cfg; every ID must exist in accounts. It does not
// modify anything, so it also serves previews of a proposed configuration.
func rankAccounts(cfg Config, accounts map[string]*account, ids []string, loads map[string]int, now time.Time) ([]RankEntry, string) {
	entries := make([]RankEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, rankEntry(cfg, accounts[id], loads, now))
	}
	order := cfg.Order
	sort.SliceStable(entries, func(i, j int) bool {
		c, _ := compareEntries(order, &entries[i], &entries[j])
		return c < 0
	})
	if len(entries) < 2 {
		return entries, decidedOnly
	}
	_, by := compareEntries(order, &entries[0], &entries[1])
	return entries, by
}

// rankingReason explains in Chinese why the top entry won.
func rankingReason(entries []RankEntry, by string) string {
	top := entries[0]
	var next RankEntry
	if len(entries) > 1 {
		next = entries[1]
	}
	switch by {
	case decidedOnly:
		return "唯一候选账号"
	case decidedExhausted:
		if top.Exhausted {
			return "所有候选账号的额度窗口都已用尽，交给 CPA 处理 429"
		}
		return "其余候选账号额度已用尽"
	case FieldSessions:
		return fmt.Sprintf("活跃会话最少（%d，次优 %d）", top.Load, next.Load)
	case FieldPriority:
		return fmt.Sprintf("CPA 优先级最高（%d，次优 %d）", top.Priority, next.Priority)
	case FieldUnopenedFirst:
		return "周额度未开窗，先用它开窗（" + weeklyStateText(top.WeeklyState) + "）"
	case FieldUnopenedLast:
		return "周额度已开窗，未开窗的账号靠后"
	case FieldUrgency:
		return fmt.Sprintf("紧迫度最高：折算到期 %s（周额度 %s 重置，剩余 %s）；次优 %s",
			fmtClock(top.UrgencyAt), fmtClock(top.ResetAt), fmtRemaining(top), fmtClock(next.UrgencyAt))
	case FieldReset:
		return fmt.Sprintf("周额度最早重置：%s；次优 %s", fmtClock(top.ResetAt), fmtClock(next.ResetAt))
	case FieldRemaining:
		return fmt.Sprintf("剩余额度最多：%s；次优 %s", fmtRemaining(top), fmtRemaining(next))
	default:
		return "排序各项打平，选最久没分配过的账号"
	}
}

func weeklyStateText(state string) string {
	switch state {
	case WeeklyOpen:
		return "周额度已开窗"
	case WeeklyProvisional:
		return "刚分配，按此刻开窗估算周额度"
	case WeeklyRefreshed:
		return "上个周窗口已重置，之后没用过"
	case WeeklyIdle:
		return "额度查询确认"
	default:
		return "还没有额度数据"
	}
}

func fmtClock(t time.Time) string {
	return t.Local().Format("01-02 15:04")
}

func fmtRemaining(r RankEntry) string {
	if r.Multiplier != 1 {
		return fmt.Sprintf("%.0f%% × %gx", r.RemainingPercent, r.Multiplier)
	}
	return fmt.Sprintf("%.0f%%", r.RemainingPercent)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// cmpBool ranks true before false.
func cmpBool(a, b bool) int {
	switch {
	case a && !b:
		return -1
	case !a && b:
		return 1
	}
	return 0
}

func cmpTime(a, b time.Time) int {
	switch {
	case a.Before(b):
		return -1
	case a.After(b):
		return 1
	}
	return 0
}
