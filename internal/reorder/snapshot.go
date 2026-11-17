package reorder

import (
	"sort"
	"time"
)

// Snapshot is the dashboard view of the engine state.
type Snapshot struct {
	Now    time.Time  `json:"now"`
	Config ConfigView `json:"config"`
	// NextReason explains why the first account of the preview would get the next new session.
	NextReason string        `json:"next_reason,omitempty"`
	NextBy     string        `json:"next_decided_by,omitempty"`
	Accounts   []AccountView `json:"accounts"`
	Sessions   []SessionView `json:"sessions"`
	Decisions  []Decision    `json:"decisions"`
	Warmup     WarmupStatus  `json:"warmup"`
}

// ConfigView mirrors Config with JSON-friendly durations.
type ConfigView struct {
	Order                    []string           `json:"order"`
	UrgencyMinutesPerPercent float64            `json:"urgency_minutes_per_percent"`
	PlanMultipliers          map[string]float64 `json:"plan_multipliers"`
	SessionTTLSeconds        int64              `json:"session_ttl_seconds"`
	StatePath                string             `json:"state_path"`
	ManageAPIKeys            bool               `json:"manage_api_keys"`
	// StatsIntervalSeconds is how often a stats line is written (0 = off).
	StatsIntervalSeconds int64      `json:"stats_interval_seconds"`
	Warmup               WarmupView `json:"warmup"`
	Warnings             []string   `json:"warnings,omitempty"`
}

// AccountView describes one Codex account.
type AccountView struct {
	ID string `json:"id"`
	// Order is the position in the next new-session ranking (1 = next pick); 0 when not offered.
	Order          int     `json:"order"`
	Priority       int     `json:"priority"`
	ActiveSessions int     `json:"active_sessions"`
	Weekly         *Window `json:"weekly,omitempty"`
	Short          *Window `json:"short,omitempty"`
	// WeeklyState is one of WeeklyOpen, WeeklyProvisional, WeeklyRefreshed, WeeklyIdle, WeeklyUnknown.
	WeeklyState       string    `json:"weekly_state"`
	ResetAt           time.Time `json:"reset_at"`
	RemainingPercent  float64   `json:"remaining_percent"`
	Multiplier        float64   `json:"multiplier"`
	UrgencyAt         time.Time `json:"urgency_at"`
	Exhausted         bool      `json:"exhausted"`
	LastAssignedAt    time.Time `json:"last_assigned_at,omitempty"`
	LastCandidateAt   time.Time `json:"last_candidate_at,omitempty"`
	LastUsageAt       time.Time `json:"last_usage_at,omitempty"`
	Assignments       int64     `json:"assignments"`
	LastFailureStatus int       `json:"last_failure_status,omitempty"`
	LastFailureAt     time.Time `json:"last_failure_at,omitempty"`
	LastProbeAt       time.Time `json:"last_probe_at,omitempty"`
	LastProbeError    string    `json:"last_probe_error,omitempty"`
	// ShortState is one of ShortStateOpen, ShortStateIdle, ShortStateNone, ShortStateUnknown.
	ShortState string `json:"short_state"`
	// Automatic weekly window opening for this account.
	WarmupPlanAt     time.Time `json:"warmup_plan_at,omitempty"`
	WarmupDone       bool      `json:"warmup_done,omitempty"`
	WarmupForced     bool      `json:"warmup_forced,omitempty"`
	WarmupLastAt     time.Time `json:"warmup_last_at,omitempty"`
	WarmupLastResult string    `json:"warmup_last_result,omitempty"`
	WarmupLastError  string    `json:"warmup_last_error,omitempty"`
	// WarmupToday counts today's automatic attempts for this account.
	WarmupToday int `json:"warmup_today,omitempty"`
	// Image is the image generation window from the last image response.
	Image      *Window `json:"image,omitempty"`
	HasCredits *bool   `json:"has_credits,omitempty"`
}

// WarmupView is the window-opening configuration shown on the dashboard. The
// *_seconds fields let the settings panel fill in its inputs.
type WarmupView struct {
	Mode                string `json:"mode"`
	Blocked             bool   `json:"blocked,omitempty"`
	Model               string `json:"model"`
	ReasoningEffort     string `json:"reasoning_effort"`
	Hours               string `json:"hours"`
	HoursAll            bool   `json:"hours_all,omitempty"`
	TimeZone            string `json:"timezone"`
	Delay               string `json:"delay"`
	DelayMinSeconds     int64  `json:"delay_min_seconds"`
	DelayMaxSeconds     int64  `json:"delay_max_seconds"`
	Spacing             string `json:"spacing"`
	SpacingMinSeconds   int64  `json:"spacing_min_seconds"`
	SpacingMaxSeconds   int64  `json:"spacing_max_seconds"`
	MaxPerAccountPerDay int    `json:"max_per_account_per_day"`
}

// SessionView describes one bound session.
type SessionView struct {
	Session    string    `json:"session"`
	AuthID     string    `json:"auth_id"`
	BoundAt    time.Time `json:"bound_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Requests   int64     `json:"requests"`
	Successes  int64     `json:"successes"`
	LastModel  string    `json:"last_model,omitempty"`
	// Parent is the hashed parent session, when one was reported.
	Parent string `json:"parent,omitempty"`
}

// recentCandidateWindow is how long an account counts as "offered" for the
// dashboard preview after CPA last offered it, when CPA's credential list does
// not cover it (config API-key credentials) or could not be read.
const recentCandidateWindow = 10 * time.Minute

// Snapshot returns the dashboard state. The "next new session" preview ranks
// offered (accounts CPA would currently offer) plus every account CPA offered as a
// candidate in the last 10 minutes that is not listed in hostKnown. Pass the IDs
// of all credentials CPA reported as hostKnown so disabled or cooling ones stay
// out of the preview. Snapshot only reads: viewing the dashboard never creates
// accounts or marks the state dirty.
func (e *Engine) Snapshot(offered []Candidate, hostKnown map[string]bool) Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	loads := e.loadsLocked(now)
	accounts := e.viewAccountsLocked(offered)
	previewIDs := e.previewIDsLocked(offered, hostKnown, now)
	position := make(map[string]int, len(previewIDs))
	nextReason, nextBy := "", ""
	if len(previewIDs) > 0 {
		ranking, by := rankAccounts(e.cfg, accounts, previewIDs, loads, now)
		for i, entry := range ranking {
			position[entry.AuthID] = i + 1
		}
		nextReason, nextBy = rankingReason(ranking, by), by
	}

	snap := Snapshot{
		Now:        now,
		NextReason: nextReason,
		NextBy:     nextBy,
		Config:     configView(e.cfg),
	}
	for id, acc := range accounts {
		view := e.accountViewLocked(acc, rankEntry(e.cfg, acc, loads, now), loads)
		view.ID = id
		view.Order = position[id]
		snap.Accounts = append(snap.Accounts, view)
	}
	sort.Slice(snap.Accounts, func(i, j int) bool {
		a, b := snap.Accounts[i], snap.Accounts[j]
		if (a.Order == 0) != (b.Order == 0) {
			return a.Order != 0
		}
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return a.ID < b.ID
	})
	for _, s := range e.sessions {
		if !e.sessionActive(s, now) {
			continue
		}
		view := SessionView{
			Session:    shortKey(s.Key),
			AuthID:     s.AuthID,
			BoundAt:    s.BoundAt,
			LastSeenAt: s.LastSeenAt,
			ExpiresAt:  s.LastSeenAt.Add(e.cfg.SessionTTL),
			Requests:   s.Requests,
			Successes:  s.Successes,
			LastModel:  s.LastModel,
			Parent:     shortKey(s.ParentKey),
		}
		snap.Sessions = append(snap.Sessions, view)
	}
	sort.Slice(snap.Sessions, func(i, j int) bool {
		return snap.Sessions[i].BoundAt.Before(snap.Sessions[j].BoundAt)
	})
	snap.Warmup = WarmupStatus{Today: e.warmup.Count, NextAt: e.warmup.NextAt}
	if snap.Warmup.Today > 0 && e.warmup.Day != now.In(e.cfg.Warmup.Location).Format("2006-01-02") {
		snap.Warmup.Today = 0
	}
	snap.Decisions = make([]Decision, 0, len(e.decisions))
	for i := len(e.decisions) - 1; i >= 0; i-- {
		snap.Decisions = append(snap.Decisions, e.decisions[i])
	}
	return snap
}

// accountViewLocked builds the dashboard view of one account (Order is left 0).
func (e *Engine) accountViewLocked(acc *account, entry RankEntry, loads map[string]int) AccountView {
	return AccountView{
		ShortState:        shortState(acc, e.now()),
		WarmupPlanAt:      acc.WarmupPlanAt,
		WarmupDone:        !acc.WarmupFor.IsZero() && acc.WarmupDoneFor.Equal(acc.WarmupFor),
		WarmupForced:      acc.WarmupForced,
		WarmupLastAt:      acc.WarmupLastAt,
		WarmupLastResult:  acc.WarmupLastResult,
		WarmupLastError:   acc.WarmupLastError,
		WarmupToday:       acc.warmupToday(warmupDay(e.cfg.Warmup, e.now())),
		ID:                acc.ID,
		Priority:          acc.Priority,
		ActiveSessions:    loads[acc.ID],
		Weekly:            copyWindow(acc.Weekly),
		Short:             copyWindow(acc.Short),
		Image:             copyWindow(acc.Image),
		HasCredits:        acc.HasCredits,
		WeeklyState:       entry.WeeklyState,
		ResetAt:           entry.ResetAt,
		RemainingPercent:  entry.RemainingPercent,
		Multiplier:        entry.Multiplier,
		UrgencyAt:         entry.UrgencyAt,
		Exhausted:         entry.Exhausted,
		LastAssignedAt:    acc.LastAssignedAt,
		LastCandidateAt:   acc.LastCandidateAt,
		LastUsageAt:       acc.LastUsageAt,
		Assignments:       acc.Assignments,
		LastFailureStatus: acc.LastFailureStatus,
		LastFailureAt:     acc.LastFailureAt,
		LastProbeAt:       acc.LastProbeAt,
		LastProbeError:    acc.LastProbeError,
	}
}

func copyWindow(w *Window) *Window {
	if w == nil {
		return nil
	}
	c := *w
	return &c
}

// previewIDsLocked lists the accounts of the "next new session" preview: those
// offered now, plus accounts CPA offered as candidates within the last 10 minutes
// that hostKnown does not cover.
func (e *Engine) previewIDsLocked(offered []Candidate, hostKnown map[string]bool, now time.Time) []string {
	var ids []string
	seen := map[string]bool{}
	for _, c := range offered {
		if c.ID == "" || seen[c.ID] {
			continue
		}
		ids = append(ids, c.ID)
		seen[c.ID] = true
	}
	recent := []string{}
	for id, acc := range e.accounts {
		if !seen[id] && !hostKnown[id] && now.Sub(acc.LastCandidateAt) <= recentCandidateWindow {
			recent = append(recent, id)
		}
	}
	sort.Strings(recent)
	return append(ids, recent...)
}

// RankingView is one complete ordering of the preview accounts.
type RankingView struct {
	Entries   []RankEntry `json:"entries"`
	Reason    string      `json:"reason,omitempty"`
	DecidedBy string      `json:"decided_by,omitempty"`
}

// Preview compares the active ranking with the one a proposed configuration
// would produce, on the same accounts and the same moment.
type Preview struct {
	Now      time.Time   `json:"now"`
	Config   ConfigView  `json:"config"`
	Current  RankingView `json:"current"`
	Proposed RankingView `json:"proposed"`
}

// Preview ranks the preview accounts under both the active and the proposed
// configuration. It changes no engine state.
func (e *Engine) Preview(proposed Config, offered []Candidate, hostKnown map[string]bool) Preview {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	accounts := e.viewAccountsLocked(offered)
	loads := e.loadsLocked(now)
	ids := e.previewIDsLocked(offered, hostKnown, now)
	view := func(cfg Config) RankingView {
		if len(ids) == 0 {
			return RankingView{Entries: []RankEntry{}}
		}
		entries, by := rankAccounts(cfg, accounts, ids, loads, now)
		return RankingView{Entries: entries, Reason: rankingReason(entries, by), DecidedBy: by}
	}
	return Preview{
		Now:      now,
		Config:   configView(proposed),
		Current:  view(e.cfg),
		Proposed: view(proposed),
	}
}

// viewAccountsLocked returns a read-only view of the accounts for the dashboard:
// accounts CPA offers that the engine has not seen yet get a temporary record,
// and offered priorities override stored ones. Engine records are not modified.
func (e *Engine) viewAccountsLocked(offered []Candidate) map[string]*account {
	accounts := make(map[string]*account, len(e.accounts)+len(offered))
	for id, acc := range e.accounts {
		accounts[id] = acc
	}
	for _, c := range offered {
		if c.ID == "" {
			continue
		}
		cp := account{ID: c.ID}
		if acc := e.accounts[c.ID]; acc != nil {
			cp = *acc
		}
		cp.Priority = c.Priority
		accounts[c.ID] = &cp
	}
	return accounts
}

func configView(cfg Config) ConfigView {
	plans := make(map[string]float64, len(cfg.PlanMultipliers))
	for id, m := range cfg.PlanMultipliers {
		plans[id] = m
	}
	return ConfigView{
		Order:                    append([]string{}, cfg.Order...),
		UrgencyMinutesPerPercent: cfg.UrgencyMinutesPerPercent,
		PlanMultipliers:          plans,
		SessionTTLSeconds:        int64(cfg.SessionTTL / time.Second),
		StatePath:                cfg.StatePath,
		ManageAPIKeys:            cfg.ManageAPIKeys,
		StatsIntervalSeconds:     int64(cfg.StatsInterval / time.Second),
		Warmup: WarmupView{
			Mode:                cfg.Warmup.Mode(),
			Blocked:             cfg.Warmup.Enabled && cfg.warmupBlockedByOrder(),
			Model:               cfg.Warmup.Model,
			ReasoningEffort:     cfg.Warmup.ReasoningEffort,
			Hours:               cfg.Warmup.HoursText(),
			HoursAll:            cfg.Warmup.HoursAll(),
			TimeZone:            cfg.Warmup.TimeZone,
			Delay:               rangeText(cfg.Warmup.DelayMin, cfg.Warmup.DelayMax),
			DelayMinSeconds:     int64(cfg.Warmup.DelayMin / time.Second),
			DelayMaxSeconds:     int64(cfg.Warmup.DelayMax / time.Second),
			Spacing:             rangeText(cfg.Warmup.SpacingMin, cfg.Warmup.SpacingMax),
			SpacingMinSeconds:   int64(cfg.Warmup.SpacingMin / time.Second),
			SpacingMaxSeconds:   int64(cfg.Warmup.SpacingMax / time.Second),
			MaxPerAccountPerDay: cfg.Warmup.MaxPerAccountPerDay,
		},
		Warnings: append([]string(nil), cfg.Warnings...),
	}
}

// RecentCandidates lists accounts CPA offered as candidates within the window.
func (e *Engine) RecentCandidates(within time.Duration) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	var ids []string
	for id, acc := range e.accounts {
		if !acc.LastCandidateAt.IsZero() && now.Sub(acc.LastCandidateAt) <= within {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
