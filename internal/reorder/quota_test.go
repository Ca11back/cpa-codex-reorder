package reorder

import (
	"strings"
	"testing"
	"time"
)

func TestParseQuotaHeadersFiveHourAndWeekly(t *testing.T) {
	at := time.Unix(1_800_000_000, 0).UTC()
	weekly, short := ParseQuotaHeaders(map[string][]string{
		"X-Codex-Primary-Used-Percent":     {"37.5"},
		"X-Codex-Primary-Window-Minutes":   {"300"},
		"X-Codex-Primary-Reset-At":         {"1800003600"},
		"X-Codex-Secondary-Used-Percent":   {"62"},
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {"1800300000"},
	}, at)
	if weekly == nil || weekly.UsedPercent != 62 || weekly.WindowMinutes != 10080 || weekly.ResetAt.Unix() != 1800300000 {
		t.Fatalf("weekly = %+v", weekly)
	}
	if short == nil || short.UsedPercent != 37.5 || short.WindowMinutes != 300 || short.ResetAt.Unix() != 1800003600 {
		t.Fatalf("short = %+v", short)
	}
}

func TestParseQuotaHeadersWeeklyOnlyPlan(t *testing.T) {
	at := time.Unix(1_800_000_000, 0).UTC()
	weekly, short := ParseQuotaHeaders(map[string][]string{
		"x-codex-primary-used-percent":        {"5"},
		"x-codex-primary-window-minutes":      {"10080"},
		"x-codex-primary-reset-after-seconds": {"3600"},
	}, at)
	if short != nil {
		t.Fatalf("short = %+v, want nil", short)
	}
	if weekly == nil || !weekly.ResetAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("weekly = %+v", weekly)
	}
}

func TestParseQuotaHeadersIgnoresIncompleteAndAdditionalLimits(t *testing.T) {
	weekly, short := ParseQuotaHeaders(map[string][]string{
		"X-Codex-Primary-Used-Percent":              {"5"}, // no window length
		"X-Codex-Bengalfox-Primary-Used-Percent":    {"90"},
		"X-Codex-Bengalfox-Primary-Window-Minutes":  {"10080"},
		"X-Codex-Bengalfox-Primary-Reset-At":        {"1800300000"},
		"X-Codex-Secondary-Used-Percent":            {"150"}, // out of range
		"X-Codex-Secondary-Window-Minutes":          {"10080"},
		"X-Codex-Secondary-Reset-After-Seconds":     {"60"},
		"X-Codex-Additional-Spark-Primary-Reset-At": {"1"},
	}, time.Now())
	if weekly != nil || short != nil {
		t.Fatalf("weekly=%+v short=%+v, want nil", weekly, short)
	}
}

func TestWindowStateAfterReset(t *testing.T) {
	now := time.Now()
	w := &Window{UsedPercent: 100, WindowMinutes: 10080, ResetAt: now.Add(-time.Minute)}
	if w.Active(now) || w.Exhausted(now) {
		t.Fatal("a window past its reset time must count as reset")
	}
}

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte("enabled: true\npriority: 10\norder: [priority, sessions, reset]\nurgency_minutes_per_percent: 2.5\nplan_multipliers:\n  acc-pro: 20\nsession_ttl: 45m\nstate_path: ''\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Order, ",") != "priority,sessions,reset" || cfg.UrgencyMinutesPerPercent != 2.5 ||
		cfg.Multiplier("acc-pro") != 20 || cfg.Multiplier("other") != 1 ||
		cfg.SessionTTL != 45*time.Minute || cfg.StatePath != "" || len(cfg.Warnings) != 0 {
		t.Fatalf("unexpected config %+v", cfg)
	}
	def, err := ParseConfig(nil)
	if err != nil || strings.Join(def.Order, ",") != "priority,sessions,unopened_first,urgency" || def.UrgencyMinutesPerPercent != 1 || def.SessionTTL != 30*time.Minute || def.ManageAPIKeys {
		t.Fatalf("defaults: %+v %v", def, err)
	}
	if old, err := ParseConfig([]byte("failover_rebind_after: 2m")); err != nil || len(old.Warnings) != 1 || !strings.Contains(old.Warnings[0], "failover_rebind_after") {
		t.Fatalf("removed failover_rebind_after: %+v %v", old.Warnings, err)
	}
	if keys, err := ParseConfig([]byte("manage_api_keys: true")); err != nil || !keys.ManageAPIKeys {
		t.Fatalf("manage_api_keys: %+v %v", keys, err)
	}
	for _, bad := range []string{
		"order: [sessions, bogus]",
		"order: [sessions, sessions]",
		"order: [unopened_first, unopened_last]",
		"urgency_minutes_per_percent: -1",
		"plan_multipliers: {a: 0}",
		"plan_multipliers: 'a'",
		"session_ttl: 10s",
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestParseConfigStringForms(t *testing.T) {
	cfg, err := ParseConfig([]byte("order: 'Sessions, unopened_last ,urgency'\nplan_multipliers: 'a=10, b:20'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Order, ",") != "sessions,unopened_last,urgency" || cfg.Multiplier("a") != 10 || cfg.Multiplier("b") != 20 {
		t.Fatalf("unexpected config %+v", cfg)
	}
}

func TestParseConfigWarnings(t *testing.T) {
	cases := map[string]int{
		"order: [urgency, sessions]":     0, // any order is allowed; later rules simply rarely apply
		"order: [unopened_first, reset]": 1, // no sessions field
		"order: [reset, priority]":       1,
		"max_sessions_per_account: 2":    1, // removed key
		"order: []":                      1,
	}
	for raw, want := range cases {
		cfg, err := ParseConfig([]byte(raw))
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if len(cfg.Warnings) != want {
			t.Fatalf("%q: warnings %v, want %d", raw, cfg.Warnings, want)
		}
	}
}

func TestParseStatsInterval(t *testing.T) {
	if cfg, err := ParseConfig(nil); err != nil || cfg.StatsInterval != 0 {
		t.Fatalf("default = %v, %v", cfg.StatsInterval, err)
	}
	for raw, want := range map[string]time.Duration{
		`stats_interval: off`:   0,
		`stats_interval: 0`:     0,
		`stats_interval: ""`:    0,
		`stats_interval: false`: 0,
		`stats_interval: 15m`:   15 * time.Minute,
		`stats_interval: 1h`:    time.Hour,
	} {
		cfg, err := ParseConfig([]byte(raw))
		if err != nil || cfg.StatsInterval != want {
			t.Errorf("%s: got %v, %v", raw, cfg.StatsInterval, err)
		}
	}
	for _, raw := range []string{`stats_interval: 30s`, `stats_interval: soon`} {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
}
