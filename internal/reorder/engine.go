// Package reorder implements the Codex account selection policy:
//
//  1. An existing session keeps its bound account while that account is usable.
//  2. A new session goes to the first account of a ranking whose fields and their
//     order are configured (Config.Order): active sessions, CPA priority, weekly
//     window opened or not, urgency, weekly reset, remaining quota.
//  3. An account whose quota is known to be exhausted always ranks last.
//
// Availability (disabled credentials, 429 cooldowns for both 5h and weekly limits,
// unsupported models) is decided by CPA before candidates reach the plugin.
package reorder

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// ProviderCodex is the CPA provider key handled by this plugin.
const ProviderCodex = "codex"

const (
	weekLength       = 7 * 24 * time.Hour
	maxDecisions     = 300
	shortKeyLength   = 10
	sessionKeyLength = 32

	// selectionTTL bounds how long a pick waits for its usage record (long streams
	// report at the end). maxSelections caps memory; on overflow, references older
	// than selectionShedAge are dropped first.
	selectionTTL     = 3 * time.Hour
	maxSelections    = 50000
	selectionShedAge = 15 * time.Minute

	// housekeepingInterval throttles the full sweep of sessions, selections and
	// accounts, so a pick does not walk every map on every request.
	housekeepingInterval = 30 * time.Second
	// staleAccountAfter drops accounts that CPA has not offered, used or reported
	// on for longer than a weekly window, and that carry no session.
	staleAccountAfter = 8 * 24 * time.Hour
)

// Candidate is one account offered by CPA for the current pick.
type Candidate struct {
	ID       string
	Priority int
}

// PickInput is the scheduler request reduced to what the policy needs.
type PickInput struct {
	// SessionID is CPA's canonical session identity (metadata canonical_session_id).
	SessionID string
	// Scope is CPA's opaque caller scope; it keeps equal session IDs from different clients apart.
	Scope string
	// SelectionID correlates this pick with the usage record CPA publishes afterwards.
	SelectionID string
	// TraceID matches CPA's inbound log ID; RequestID identifies the host request lifecycle.
	TraceID   string
	RequestID string
	// Pinned reports an explicit credential constraint applied by CPA.
	Pinned bool
	Model  string
	// Image marks an image generation request (CPA metadata request_path).
	Image      bool
	Candidates []Candidate
	// Lineage carries what CPA and the client reported about where the session
	// comes from (parent session, fork, Codex subagent signals). Only logged.
	Lineage Lineage
}

// Lineage describes a session's origin as reported by CPA metadata and Codex
// client headers. Values are short, sanitized labels; ParentSessionID is hashed
// before it is stored or logged.
type Lineage struct {
	ParentSessionID string
	Fork            bool
	NodeKind        string
	// Subagent is the X-Openai-Subagent header value.
	Subagent string
	// SubagentKind and AgentName come from the X-Codex-Turn-Metadata header.
	SubagentKind string
	AgentName    string
}

func (l Lineage) subagentSignal() bool {
	return l.Subagent != "" || l.SubagentKind != "" || l.AgentName != "" || l.Fork
}

// PickResult is the policy decision.
type PickResult struct {
	AuthID  string
	Handled bool
	Kind    string
}

// UsageInput is a completed (or failed) upstream attempt reported by CPA.
type UsageInput struct {
	Provider    string
	AuthID      string
	SelectionID string
	// Model is the usage record's model (an image model on CPA's direct image path).
	Model      string
	Failed     bool
	StatusCode int
	Headers    map[string][]string
	At         time.Time
}

// Decision kinds recorded in the decision log.
const (
	KindNew       = "new"
	KindNoSession = "no_session"
	KindSticky    = "sticky"
	KindRebind    = "rebind"
	KindPinned    = "pinned"
	// KindHeld: the bound account cannot serve, but CPA offered no other
	// account, so the attempt stays on it and the binding is kept.
	KindHeld = "held"
)

type session struct {
	Key        string    `json:"key"`
	AuthID     string    `json:"auth_id"`
	BoundAt    time.Time `json:"bound_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Requests   int64     `json:"requests"`
	Successes  int64     `json:"successes"`
	// LegacyDetourAuthID is a temporary account stored by versions before
	// 0.6.3 (which detoured and later returned); LoadState rebinds to it.
	LegacyDetourAuthID string `json:"detour_auth_id,omitempty"`
	LastModel          string `json:"last_model,omitempty"`
	// ParentKey is the hashed parent session reported when the session started.
	ParentKey string `json:"parent_key,omitempty"`
}

type account struct {
	ID                string    `json:"id"`
	Priority          int       `json:"priority"`
	Weekly            *Window   `json:"weekly,omitempty"`
	Short             *Window   `json:"short,omitempty"`
	ProvisionalOpenAt time.Time `json:"provisional_open_at,omitempty"`
	LastAssignedAt    time.Time `json:"last_assigned_at,omitempty"`
	LastCandidateAt   time.Time `json:"last_candidate_at,omitempty"`
	Assignments       int64     `json:"assignments"`
	LastUsageAt       time.Time `json:"last_usage_at,omitempty"`
	LastFailureStatus int       `json:"last_failure_status,omitempty"`
	LastFailureAt     time.Time `json:"last_failure_at,omitempty"`
	// WeeklyEndLogged is the reset time of the last weekly window reported by EventWindowEnd.
	WeeklyEndLogged time.Time `json:"weekly_end_logged,omitempty"`
	// WeeklyIdleAt is when a quota query last reported no running weekly window.
	WeeklyIdleAt time.Time `json:"weekly_idle_at,omitempty"`
	// LastProbeAt and LastProbeError describe the last quota query.
	LastProbeAt    time.Time `json:"last_probe_at,omitempty"`
	LastProbeError string    `json:"last_probe_error,omitempty"`
	// ShortStatus is what the last quota query said about a 5h window that is not
	// running: ShortStateIdle or ShortStateNone. Cleared when a running 5h window is seen.
	ShortStatus string `json:"short_status,omitempty"`
	// Automatic weekly window opening (see warmup.go). WarmupFor marks the start
	// of the current unopened period; WarmupDoneFor the period already handled.
	WarmupFor        time.Time `json:"warmup_for,omitempty"`
	WarmupPlanAt     time.Time `json:"warmup_plan_at,omitempty"`
	WarmupDoneFor    time.Time `json:"warmup_done_for,omitempty"`
	WarmupForced     bool      `json:"warmup_forced,omitempty"`
	WarmupLastAt     time.Time `json:"warmup_last_at,omitempty"`
	WarmupLastResult string    `json:"warmup_last_result,omitempty"`
	WarmupLastError  string    `json:"warmup_last_error,omitempty"`
	// WarmupDay (in the warmup time zone) and WarmupDayCount count the
	// automatic attempts of that day.
	WarmupDay      string `json:"warmup_day,omitempty"`
	WarmupDayCount int    `json:"warmup_day_count,omitempty"`
	// LastImageAt is when the account last received an image request (rotation).
	LastImageAt time.Time `json:"last_image_at,omitempty"`
	// Image is the image generation window, as last reported by an image response.
	Image *Window `json:"image,omitempty"`
	// HasCredits is the x-codex-credits-has-credits flag of the last image
	// response (nil when never seen).
	HasCredits *bool `json:"has_credits,omitempty"`
}

// exhausted reports a known, still-running window that is fully used. Such an
// account would only answer with 429, so it never receives new sessions while
// another account is available.
// lastActivity is the latest time the account was offered, assigned, used or observed.
func (a *account) lastActivity() time.Time {
	t := a.LastCandidateAt
	for _, c := range []time.Time{a.LastAssignedAt, a.LastUsageAt, a.ProvisionalOpenAt, a.LastProbeAt, a.WarmupFor, a.WarmupLastAt} {
		if c.After(t) {
			t = c
		}
	}
	if a.Weekly != nil && a.Weekly.ObservedAt.After(t) {
		t = a.Weekly.ObservedAt
	}
	return t
}

// clearWarmupPlan ends the current unopened period (a weekly window is running).
func (a *account) clearWarmupPlan() {
	a.WarmupFor, a.WarmupPlanAt, a.WarmupDoneFor, a.WarmupForced = time.Time{}, time.Time{}, time.Time{}, false
}

func (a *account) exhausted(now time.Time) bool {
	return a.Weekly.Exhausted(now) || a.Short.Exhausted(now)
}

type selectionRef struct {
	sessionKey string
	authID     string
	at         time.Time
	// warmup marks the pick of a window-opening request (kept out of statistics).
	warmup bool
	// image marks the pick of an image request (its quota headers are not chat windows).
	image bool
}

// Engine holds all scheduling state. All methods are safe for concurrent use; a
// single mutex makes concurrent new sessions observe each other's bindings.
type Engine struct {
	mu         sync.Mutex
	saveMu     sync.Mutex
	cfg        Config
	now        func() time.Time
	sessions   map[string]*session
	accounts   map[string]*account
	selections map[string]selectionRef
	decisions  []Decision
	// sink receives analysis events; stats holds the counters of the current interval.
	sink   EventSink
	stats  *statCounters
	dirty  bool
	loaded bool
	// lastHousekeeping is when the last full sweep ran (see housekeepingLocked).
	lastHousekeeping time.Time
	// claim is when this instance last took ownership of the state file (Claim).
	claim time.Time
	// Window opening: the day's counter and spacing gate (persisted), requests in
	// flight, and the random source (tests replace it).
	warmup        warmupPersist
	pendingWarmup map[string]string
	rnd           func(n int64) int64
}

// NewEngine creates an engine with the given configuration.
func NewEngine(cfg Config) *Engine {
	return &Engine{
		cfg:        cfg,
		now:        time.Now,
		sessions:   make(map[string]*session),
		accounts:   make(map[string]*account),
		selections: make(map[string]selectionRef),
	}
}

// SetClock replaces the time source (tests only).
func (e *Engine) SetClock(now func() time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = now
}

// Config returns the active configuration.
func (e *Engine) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// SetConfig applies a new configuration without touching existing bindings.
func (e *Engine) SetConfig(cfg Config) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg = cfg
}

// SessionKey derives the stored session key. Raw session identifiers are never kept.
func SessionKey(scope, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(scope + "\n" + sessionID))
	return hex.EncodeToString(sum[:])[:sessionKeyLength]
}

func shortKey(key string) string {
	if len(key) > shortKeyLength {
		return key[:shortKeyLength]
	}
	return key
}

// Pick chooses the account for one upstream attempt.
func (e *Engine) Pick(in PickInput) PickResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.housekeepingLocked(now, false)
	res := e.pickLocked(in, now)
	e.countPickLocked(res, now)
	return res
}

func (e *Engine) pickLocked(in PickInput, now time.Time) PickResult {
	if len(in.Candidates) == 0 {
		return PickResult{}
	}

	ids := make([]string, 0, len(in.Candidates))
	offered := make(map[string]bool, len(in.Candidates))
	for _, c := range in.Candidates {
		if c.ID == "" || offered[c.ID] {
			continue
		}
		acc := e.ensureAccountLocked(c.ID)
		acc.Priority = c.Priority
		acc.LastCandidateAt = now
		ids = append(ids, c.ID)
		offered[c.ID] = true
	}
	if len(ids) == 0 {
		return PickResult{}
	}
	if len(ids) == 1 && e.isWarmupPickLocked(ids[0], in.Scope) {
		id := ids[0]
		// The plugin's own window-opening request: route it, but create no
		// binding and count no assignment.
		e.rememberSelectionLocked(in.SelectionID, "", id, now)
		if ref, exists := e.selections[in.SelectionID]; exists {
			ref.warmup = true
			e.selections[in.SelectionID] = ref
		}
		return PickResult{AuthID: id, Handled: true, Kind: KindWarmup}
	}
	if in.Image {
		return e.imagePickLocked(in, ids, now)
	}
	usable := func(id string) bool {
		acc := e.accounts[id]
		return offered[id] && acc != nil && !acc.exhausted(now)
	}
	loads := e.loadsLocked(now)
	key := SessionKey(in.Scope, in.SessionID)

	if key == "" {
		ranking, by := e.rankLocked(ids, loads, now)
		choice := ranking[0].AuthID
		e.assignLocked(choice, now)
		e.rememberSelectionLocked(in.SelectionID, "", choice, now)
		e.logLocked(Decision{At: now, Model: in.Model, Kind: KindNoSession, AuthID: choice, Reason: "请求没有会话 ID，只按排序选号，不建立绑定；" + rankingReason(ranking, by), DecidedBy: by, Ranking: ranking})
		return PickResult{AuthID: choice, Handled: true, Kind: KindNoSession}
	}

	s := e.sessions[key]
	if s != nil && !e.sessionActive(s, now) {
		// Expired but not swept yet: it is a new session now.
		delete(e.sessions, key)
		s = nil
	}
	if s == nil {
		ranking, by := e.rankLocked(ids, loads, now)
		choice := ranking[0].AuthID
		lineage := e.lineageLocked(in, key, now)
		e.sessions[key] = &session{Key: key, AuthID: choice, BoundAt: now, LastSeenAt: now, Requests: 1, LastModel: in.Model, ParentKey: lineage.parentKey}
		e.assignLocked(choice, now)
		e.rememberSelectionLocked(in.SelectionID, key, choice, now)
		e.countLineageLocked(lineage, now)
		e.logLocked(Decision{At: now, Model: in.Model, Kind: KindNew, Session: shortKey(key), AuthID: choice, Reason: rankingReason(ranking, by), DecidedBy: by, Ranking: ranking, Lineage: lineage.view})
		return PickResult{AuthID: choice, Handled: true, Kind: KindNew}
	}

	s.LastSeenAt = now
	s.Requests++
	if in.Model != "" {
		s.LastModel = in.Model
	}
	e.dirty = true

	if usable(s.AuthID) {
		e.rememberSelectionLocked(in.SelectionID, key, s.AuthID, now)
		return PickResult{AuthID: s.AuthID, Handled: true, Kind: KindSticky}
	}

	// Choose a target without counting this session's own load.
	if loads[s.AuthID] > 0 {
		loads[s.AuthID]--
	}
	ranking, by := e.rankLocked(ids, loads, now)
	choice := ranking[0].AuthID
	diagnostic := &FailoverDetail{
		Candidates: len(ids), BoundOffered: offered[s.AuthID], Pinned: in.Pinned,
		TraceID: in.TraceID, RequestID: in.RequestID,
	}
	if bound := e.accounts[s.AuthID]; bound != nil {
		if bound.Short.Exhausted(now) {
			diagnostic.ExhaustedWindows = append(diagnostic.ExhaustedWindows, "5h")
		}
		if bound.Weekly.Exhausted(now) {
			diagnostic.ExhaustedWindows = append(diagnostic.ExhaustedWindows, "weekly")
		}
	}

	if prefixedModel(in.Model) && !offered[s.AuthID] {
		// A prefixed model ("prefix/model") is registered only for the
		// credentials with that prefix, so CPA offers only those. Like CPA's
		// own affinity (bound per model), this does not move the session.
		e.assignLocked(choice, now)
		e.rememberSelectionLocked(in.SelectionID, key, choice, now)
		e.logLocked(Decision{At: now, Model: in.Model, Kind: KindPinned, Session: shortKey(key), AuthID: choice, FromAuthID: s.AuthID,
			Reason: "带前缀的模型只能用对应前缀的账号，本次请求走它，会话绑定不变", DecidedBy: by, Ranking: ranking, failover: diagnostic})
		return PickResult{AuthID: choice, Handled: true, Kind: KindPinned}
	}

	if choice == s.AuthID {
		// CPA offered no better account (inside a Codex websocket connection it
		// offers only the connection's account): serve on the bound account and
		// keep the binding. The real move happens on the next attempt that has
		// other candidates, usually after the upstream 429 closes the connection.
		e.rememberSelectionLocked(in.SelectionID, key, choice, now)
		e.logLocked(Decision{At: now, Model: in.Model, Kind: KindHeld, Session: shortKey(key), AuthID: choice, FromAuthID: s.AuthID,
			Reason: "原账号额度已用尽，当前候选没有可用额度，本次仍走原账号并保留绑定", DecidedBy: by, Ranking: ranking, failover: diagnostic})
		return PickResult{AuthID: choice, Handled: true, Kind: KindHeld}
	}

	// The bound account cannot serve this attempt (it failed and CPA is
	// retrying elsewhere, is cooling down, disabled, lacks the model, or its
	// quota is used up): move the session for good, like CPA's default
	// session-affinity-rebind-on: any. It does not come back.
	e.assignLocked(choice, now)
	e.rememberSelectionLocked(in.SelectionID, key, choice, now)
	reason := "原账号这次服务不了（出错重试、冷却、禁用或不支持该模型），改绑"
	if len(diagnostic.ExhaustedWindows) > 0 {
		reason = "原账号额度已用尽，改绑"
	}
	from := s.AuthID
	s.AuthID = choice
	s.BoundAt = now
	e.logLocked(Decision{At: now, Model: in.Model, Kind: KindRebind, Session: shortKey(key), AuthID: choice, FromAuthID: from, Reason: reason + "；新账号：" + rankingReason(ranking, by), DecidedBy: by, Ranking: ranking, failover: diagnostic})
	return PickResult{AuthID: choice, Handled: true, Kind: KindRebind}
}

// prefixedModel reports a CPA prefixed model name ("prefix/model").
func prefixedModel(model string) bool {
	i := strings.Index(model, "/")
	return i > 0 && i < len(model)-1
}

// HandleUsage records quota windows and session activity from a usage record.
func (e *Engine) HandleUsage(u UsageInput) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	at := u.At
	if at.IsZero() || at.After(now) {
		at = now
	}
	ref, hasRef := e.selections[u.SelectionID]
	if u.AuthID == "" {
		return
	}
	_, known := e.accounts[u.AuthID]
	if !hasRef && !known && u.Provider != ProviderCodex {
		return
	}
	acc := e.ensureAccountLocked(u.AuthID)
	if !hasRef || !ref.warmup {
		e.countUsageLocked(u, now)
	}
	if at.After(acc.LastUsageAt) {
		acc.LastUsageAt = at
	}
	if u.Failed {
		acc.LastFailureStatus = u.StatusCode
		acc.LastFailureAt = at
	}
	if (hasRef && ref.image) || isImageModel(u.Model) {
		e.imageUsageLocked(acc, u, at)
		e.dirty = true
		return
	}
	weekly, short := ParseQuotaHeaders(u.Headers, at)
	if weekly != nil && (acc.Weekly == nil || !weekly.ObservedAt.Before(acc.Weekly.ObservedAt)) {
		if acc.Weekly != nil {
			// Report the stored window before a newer one replaces it.
			e.closeWeeklyLocked(acc, now, weekly.ResetAt.Sub(acc.Weekly.ResetAt) > earlyResetGap)
		}
		acc.Weekly = weekly
		acc.ProvisionalOpenAt = time.Time{}
		acc.WeeklyIdleAt = time.Time{}
		if weekly.Active(now) {
			acc.clearWarmupPlan()
		}
	}
	if short != nil && (acc.Short == nil || !short.ObservedAt.Before(acc.Short.ObservedAt)) {
		acc.Short = short
		acc.ShortStatus = ""
	}
	if hasRef && ref.sessionKey != "" {
		if s := e.sessions[ref.sessionKey]; s != nil {
			if at.After(s.LastSeenAt) {
				s.LastSeenAt = at
			}
			if !u.Failed && ref.authID == u.AuthID {
				s.Successes++
			}
		}
	}
	e.dirty = true
}

func (e *Engine) ensureAccountLocked(id string) *account {
	acc := e.accounts[id]
	if acc == nil {
		acc = &account{ID: id}
		e.accounts[id] = acc
		e.dirty = true
	}
	return acc
}

func (e *Engine) assignLocked(id string, now time.Time) {
	acc := e.ensureAccountLocked(id)
	acc.LastAssignedAt = now
	acc.Assignments++
	if !acc.Weekly.Active(now) {
		if acc.ProvisionalOpenAt.IsZero() || !acc.ProvisionalOpenAt.Add(weekLength).After(now) {
			acc.ProvisionalOpenAt = now
		}
	}
	e.dirty = true
}

func (e *Engine) rememberSelectionLocked(selectionID, sessionKey, authID string, now time.Time) {
	if selectionID == "" {
		return
	}
	if len(e.selections) >= maxSelections {
		for id, ref := range e.selections {
			if now.Sub(ref.at) > selectionShedAge {
				delete(e.selections, id)
			}
		}
		if len(e.selections) >= maxSelections {
			e.selections = make(map[string]selectionRef)
		}
	}
	e.selections[selectionID] = selectionRef{sessionKey: sessionKey, authID: authID, at: now}
}

func (e *Engine) sessionActive(s *session, now time.Time) bool {
	return s.LastSeenAt.Add(e.cfg.SessionTTL).After(now)
}

// loadsLocked counts active sessions per serving account.
func (e *Engine) loadsLocked(now time.Time) map[string]int {
	loads := make(map[string]int, len(e.accounts))
	for _, s := range e.sessions {
		if e.sessionActive(s, now) {
			loads[s.AuthID]++
		}
	}
	return loads
}

// housekeepingLocked drops expired sessions and selection references and stale
// accounts. Unless force is set it runs at most once per housekeepingInterval;
// everything that reads sessions also checks expiry itself, so a late sweep
// never changes a decision.
func (e *Engine) housekeepingLocked(now time.Time, force bool) {
	if !force && now.Sub(e.lastHousekeeping) < housekeepingInterval && !now.Before(e.lastHousekeeping) {
		return
	}
	e.lastHousekeeping = now
	referenced := make(map[string]bool, len(e.sessions)*2)
	for key, s := range e.sessions {
		if !e.sessionActive(s, now) {
			delete(e.sessions, key)
			e.dirty = true
			continue
		}
		referenced[s.AuthID] = true
	}
	for id, ref := range e.selections {
		if now.Sub(ref.at) > selectionTTL {
			delete(e.selections, id)
		}
	}
	for id, acc := range e.accounts {
		if !referenced[id] && now.Sub(acc.lastActivity()) > staleAccountAfter {
			delete(e.accounts, id)
			e.dirty = true
		}
	}
}

func (e *Engine) logLocked(d Decision) {
	if len(d.Ranking) > maxRankingEntries {
		d.Ranking = d.Ranking[:maxRankingEntries]
	}
	e.decisions = append(e.decisions, d)
	if d.Kind != KindNoSession {
		e.emitLocked(EventPick, pickEvent(d))
	}
	if len(e.decisions) > maxDecisions {
		e.decisions = append([]Decision(nil), e.decisions[len(e.decisions)-maxDecisions:]...)
	}
	e.dirty = true
}
