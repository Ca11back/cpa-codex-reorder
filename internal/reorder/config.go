package reorder

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // warmup_timezone must work on hosts without a zoneinfo database

	"gopkg.in/yaml.v3"
)

// Ranking fields for new sessions, used in Config.Order.
const (
	// FieldSessions: fewer active sessions first (spreads load across accounts).
	FieldSessions = "sessions"
	// FieldPriority: higher CPA credential priority first.
	FieldPriority = "priority"
	// FieldUnopenedFirst: accounts without a running weekly window first.
	FieldUnopenedFirst = "unopened_first"
	// FieldUnopenedLast: accounts without a running weekly window last.
	FieldUnopenedLast = "unopened_last"
	// FieldUrgency: earliest "urgency time" first (weekly reset minus remaining quota converted to time).
	FieldUrgency = "urgency"
	// FieldReset: earliest weekly reset first.
	FieldReset = "reset"
	// FieldRemaining: most remaining weekly quota (times plan multiplier) first.
	FieldRemaining = "remaining"
)

// knownFields lists every accepted ranking field.
var knownFields = []string{FieldSessions, FieldPriority, FieldUnopenedFirst, FieldUnopenedLast, FieldUrgency, FieldReset, FieldRemaining}

// Config holds the plugin settings read from plugins.configs.<pluginID>.
type Config struct {
	// Order lists the ranking fields for new sessions, most important first.
	Order []string
	// UrgencyMinutesPerPercent converts remaining quota into time for FieldUrgency:
	// every 1% of remaining weekly quota (times the plan multiplier) moves the
	// account's urgency time this many minutes earlier.
	UrgencyMinutesPerPercent float64
	// PlanMultipliers maps a CPA auth ID to its plan size (Plus/Team standard = 1).
	// Accounts not listed count as 1.
	PlanMultipliers map[string]float64
	// SessionTTL is the sliding stickiness window. A session stays bound (and counts
	// as active load) until it has been idle for this long.
	SessionTTL time.Duration
	// StatePath is where bindings and quota snapshots are persisted. Empty disables persistence.
	StatePath string
	// ManageAPIKeys also lets the plugin schedule Codex API keys from CPA's config
	// (codex-api-key). By default only account credentials (OAuth / PAT auth files)
	// are scheduled: API keys have no Codex quota windows, so the ranking rules do
	// not apply to them, and CPA keeps choosing among them itself.
	ManageAPIKeys bool
	// QuotaProbeURL is the quota endpoint the dashboard queries through CPA's
	// api-call. Not shown in the plugin settings; only tests change it.
	QuotaProbeURL string
	// StatsInterval is how often a stats analysis line is written to CPA's log
	// (0 = never, the default). Pick, window_end, image_usage and other event
	// lines are written regardless.
	StatsInterval time.Duration
	// Warmup configures automatic weekly window opening.
	Warmup WarmupConfig
	// Warnings are non-fatal configuration problems shown in the log and dashboard.
	Warnings []string
}

const (
	defaultUrgencyMinutesPerPercent = 1.0
	defaultSessionTTL               = 30 * time.Minute
	defaultStatePath                = "codex-reorder-state.json"
	// DefaultQuotaProbeURL is the Codex quota endpoint (the same one CPA Manager Plus queries).
	DefaultQuotaProbeURL = "https://chatgpt.com/backend-api/wham/usage"
)

// DefaultOrder is the default ranking: respect CPA credential priorities (a no-op
// when they are all equal), spread sessions (risk control), open idle weekly
// windows (an unopened window earns nothing), then use the most urgent quota
// (avoid quota expiring unused).
func DefaultOrder() []string {
	return []string{FieldPriority, FieldSessions, FieldUnopenedFirst, FieldUrgency}
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		Order:                    DefaultOrder(),
		UrgencyMinutesPerPercent: defaultUrgencyMinutesPerPercent,
		PlanMultipliers:          map[string]float64{},
		SessionTTL:               defaultSessionTTL,
		StatePath:                defaultStatePath,
		QuotaProbeURL:            DefaultQuotaProbeURL,
		Warmup:                   DefaultWarmupConfig(),
	}
}

// Multiplier returns the plan multiplier for an account (1 when not configured).
func (c Config) Multiplier(authID string) float64 {
	if m, ok := c.PlanMultipliers[authID]; ok && m > 0 {
		return m
	}
	return 1
}

// Summary is a stable one-line description used for logging and change detection.
func (c Config) Summary() string {
	ids := make([]string, 0, len(c.PlanMultipliers))
	for id := range c.PlanMultipliers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	plans := make([]string, 0, len(ids))
	for _, id := range ids {
		plans = append(plans, id+"="+strconv.FormatFloat(c.PlanMultipliers[id], 'g', -1, 64))
	}
	stats := "off"
	if c.StatsInterval > 0 {
		stats = c.StatsInterval.String()
	}
	summary := fmt.Sprintf("order=%s urgency_minutes_per_percent=%g plan_multipliers={%s} session_ttl=%s state_path=%q manage_api_keys=%t stats_interval=%s",
		strings.Join(c.Order, ","), c.UrgencyMinutesPerPercent, strings.Join(plans, ","), c.SessionTTL, c.StatePath, c.ManageAPIKeys, stats)
	if c.QuotaProbeURL != DefaultQuotaProbeURL {
		summary += fmt.Sprintf(" quota_probe_url=%q", c.QuotaProbeURL)
	}
	return summary + " " + c.Warmup.Summary()
}

type rawConfig struct {
	Order                     yaml.Node `yaml:"order"`
	UrgencyMinutesPerPercent  *float64  `yaml:"urgency_minutes_per_percent"`
	PlanMultipliers           yaml.Node `yaml:"plan_multipliers"`
	SessionTTL                *string   `yaml:"session_ttl"`
	StatePath                 *string   `yaml:"state_path"`
	ManageAPIKeys             *bool     `yaml:"manage_api_keys"`
	QuotaProbeURL             *string   `yaml:"quota_probe_url"`
	StatsInterval             *string   `yaml:"stats_interval"`
	WarmupEnabled             *bool     `yaml:"warmup_enabled"`
	WarmupModel               *string   `yaml:"warmup_model"`
	WarmupReasoningEffort     *string   `yaml:"warmup_reasoning_effort"`
	WarmupHours               *string   `yaml:"warmup_hours"`
	WarmupTimezone            *string   `yaml:"warmup_timezone"`
	WarmupDelay               *string   `yaml:"warmup_delay"`
	WarmupSpacing             *string   `yaml:"warmup_spacing"`
	WarmupMaxPerAccountPerDay *int      `yaml:"warmup_max_per_account_per_day"`
	WarmupCheckInterval       *string   `yaml:"warmup_check_interval"`
	// Removed in 0.2.0 / 0.6.2; detected only to warn.
	MaxSessionsPerAccount *int      `yaml:"max_sessions_per_account"`
	WarmupMaxPerDay       yaml.Node `yaml:"warmup_max_per_day"`
	WarmupDryRun          yaml.Node `yaml:"warmup_dry_run"`
	FailoverRebindAfter   yaml.Node `yaml:"failover_rebind_after"`
}

// ParseConfig decodes the plugin YAML section. Omitted keys keep their defaults.
func ParseConfig(raw []byte) (Config, error) {
	return parseOnto(DefaultConfig(), raw)
}

// WithOverrides applies a partial YAML or JSON document (JSON is valid YAML) on
// top of base, with the same validation as ParseConfig. Keys that are omitted
// keep base's values. Used to preview a proposed ranking before saving it.
func WithOverrides(base Config, raw []byte) (Config, error) {
	base.Order = append([]string(nil), base.Order...)
	plans := make(map[string]float64, len(base.PlanMultipliers))
	for id, m := range base.PlanMultipliers {
		plans[id] = m
	}
	base.PlanMultipliers = plans
	return parseOnto(base, raw)
}

func parseOnto(cfg Config, raw []byte) (Config, error) {
	cfg.Warnings = nil
	if len(strings.TrimSpace(string(raw))) == 0 {
		cfg.Warnings = orderWarnings(cfg.Order)
		return cfg, nil
	}
	var rc rawConfig
	if err := yaml.Unmarshal(raw, &rc); err != nil {
		return Config{}, fmt.Errorf("decode plugin config: %w", err)
	}
	if rc.Order.Kind != 0 {
		order, err := parseOrder(&rc.Order)
		if err != nil {
			return Config{}, err
		}
		cfg.Order = order
	}
	if rc.UrgencyMinutesPerPercent != nil {
		if *rc.UrgencyMinutesPerPercent < 0 {
			return Config{}, fmt.Errorf("urgency_minutes_per_percent must not be negative, got %g", *rc.UrgencyMinutesPerPercent)
		}
		cfg.UrgencyMinutesPerPercent = *rc.UrgencyMinutesPerPercent
	}
	if rc.PlanMultipliers.Kind != 0 {
		plans, err := parsePlanMultipliers(&rc.PlanMultipliers)
		if err != nil {
			return Config{}, err
		}
		cfg.PlanMultipliers = plans
	}
	if rc.SessionTTL != nil {
		d, err := parseDuration("session_ttl", *rc.SessionTTL)
		if err != nil {
			return Config{}, err
		}
		if d < time.Minute {
			return Config{}, fmt.Errorf("session_ttl must be at least 1m, got %s", d)
		}
		cfg.SessionTTL = d
	}
	if rc.StatePath != nil {
		cfg.StatePath = strings.TrimSpace(*rc.StatePath)
	}
	if rc.ManageAPIKeys != nil {
		cfg.ManageAPIKeys = *rc.ManageAPIKeys
	}
	if rc.StatsInterval != nil {
		d, err := parseStatsInterval(*rc.StatsInterval)
		if err != nil {
			return Config{}, err
		}
		cfg.StatsInterval = d
	}
	if rc.QuotaProbeURL != nil {
		u := strings.TrimSpace(*rc.QuotaProbeURL)
		if u == "" {
			u = DefaultQuotaProbeURL
		}
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return Config{}, fmt.Errorf("quota_probe_url must be an http(s) URL, got %q", u)
		}
		cfg.QuotaProbeURL = u
	}
	if err := parseWarmup(&cfg.Warmup, &rc); err != nil {
		return Config{}, err
	}
	if rc.MaxSessionsPerAccount != nil {
		cfg.Warnings = append(cfg.Warnings, "max_sessions_per_account 已移除，不再生效；会话均摊由 order 里的 sessions 决定")
	}
	if rc.FailoverRebindAfter.Kind != 0 {
		cfg.Warnings = append(cfg.Warnings, "failover_rebind_after 已移除，可以删掉：原账号这次服务不了就立即改绑、不再转回（同 CPA 默认的 session-affinity-rebind-on: any）")
	}
	if rc.WarmupDryRun.Kind != 0 {
		cfg.Warnings = append(cfg.Warnings, "warmup_dry_run 已移除（不再有演练模式），可以删掉；自动开窗只看 warmup_enabled")
	}
	if rc.WarmupMaxPerDay.Kind != 0 {
		cfg.Warnings = append(cfg.Warnings, "warmup_max_per_day 已移除，不再生效；改用 warmup_max_per_account_per_day（每个号每天最多自动开窗几次，默认 2）")
	}
	cfg.Warnings = append(cfg.Warnings, orderWarnings(cfg.Order)...)
	if cfg.Warmup.Enabled && cfg.warmupBlockedByOrder() {
		cfg.Warnings = append(cfg.Warnings, "order 里用了 unopened_last（未开窗的靠后），自动周开窗不会执行；手动“立即开窗”仍可用")
	}
	return cfg, nil
}

// warmupBlockedByOrder reports an order that keeps unopened accounts closed on
// purpose, which automatic window opening must respect.
func (c Config) warmupBlockedByOrder() bool {
	for _, f := range c.Order {
		if f == FieldUnopenedLast {
			return true
		}
	}
	return false
}

// parseOrder accepts a YAML list or a comma-separated string (management UIs
// edit plugin fields as strings).
func parseOrder(node *yaml.Node) ([]string, error) {
	var items []string
	switch node.Kind {
	case yaml.SequenceNode:
		if err := node.Decode(&items); err != nil {
			return nil, fmt.Errorf("order: %w", err)
		}
	case yaml.ScalarNode:
		if node.Tag != "!!null" {
			for _, part := range strings.Split(node.Value, ",") {
				items = append(items, part)
			}
		}
	default:
		return nil, fmt.Errorf("order must be a list of field names")
	}
	order := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		f := strings.ToLower(strings.TrimSpace(item))
		if f == "" {
			continue
		}
		valid := false
		for _, k := range knownFields {
			if f == k {
				valid = true
				break
			}
		}
		if !valid {
			return nil, fmt.Errorf("order: unknown field %q (allowed: %s)", f, strings.Join(knownFields, ", "))
		}
		if seen[f] {
			return nil, fmt.Errorf("order: field %q listed twice", f)
		}
		seen[f] = true
		order = append(order, f)
	}
	if seen[FieldUnopenedFirst] && seen[FieldUnopenedLast] {
		return nil, fmt.Errorf("order: %s and %s cannot both be used", FieldUnopenedFirst, FieldUnopenedLast)
	}
	return order, nil
}

// parsePlanMultipliers accepts a YAML mapping or a string "id=10, id2=20".
func parsePlanMultipliers(node *yaml.Node) (map[string]float64, error) {
	plans := map[string]float64{}
	switch node.Kind {
	case yaml.MappingNode:
		var m map[string]float64
		if err := node.Decode(&m); err != nil {
			return nil, fmt.Errorf("plan_multipliers: %w", err)
		}
		for k, v := range m {
			plans[strings.TrimSpace(k)] = v
		}
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			break
		}
		for _, part := range strings.Split(node.Value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			sep := strings.LastIndexAny(part, "=:")
			if sep <= 0 {
				return nil, fmt.Errorf("plan_multipliers: %q must look like <auth-id>=<multiplier>", part)
			}
			v, err := strconv.ParseFloat(strings.TrimSpace(part[sep+1:]), 64)
			if err != nil {
				return nil, fmt.Errorf("plan_multipliers: %q: %w", part, err)
			}
			plans[strings.TrimSpace(part[:sep])] = v
		}
	default:
		return nil, fmt.Errorf("plan_multipliers must map auth IDs to numbers")
	}
	for id, v := range plans {
		if id == "" {
			return nil, fmt.Errorf("plan_multipliers: empty auth ID")
		}
		if !(v > 0) {
			return nil, fmt.Errorf("plan_multipliers: %s must be > 0, got %g", id, v)
		}
	}
	return plans, nil
}

// orderWarnings flags orders that are valid but probably not what was intended.
func orderWarnings(order []string) []string {
	var warnings []string
	hasSessions := false
	for _, f := range order {
		if f == FieldSessions {
			hasSessions = true
		}
	}
	if !hasSessions {
		warnings = append(warnings, "order 里没有 sessions：新会话可能集中到少数账号，单个账号的并发会变高")
	}
	return warnings
}

// parseStatsInterval accepts "off", "0" or "" (disabled) or a duration of at least 1m.
func parseStatsInterval(value string) (time.Duration, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" || v == "off" || v == "0" || v == "false" {
		return 0, nil
	}
	d, err := parseDuration("stats_interval", v)
	if err != nil {
		return 0, err
	}
	if d < time.Minute {
		return 0, fmt.Errorf("stats_interval must be off or at least 1m, got %s", d)
	}
	return d, nil
}

func parseDuration(name, value string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}
