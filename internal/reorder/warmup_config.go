package reorder

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// WarmupConfig configures automatic weekly window opening: an account whose
// weekly window is not running gets one minimal request, at a random time
// inside the allowed hours, so its weekly clock starts.
type WarmupConfig struct {
	Enabled         bool
	Model           string
	ReasoningEffort string
	// HoursStart and HoursEnd bound the allowed time of day (offsets from local
	// midnight in TimeZone). HoursEnd < HoursStart wraps past midnight; equal
	// values allow the whole day.
	HoursStart time.Duration
	HoursEnd   time.Duration
	TimeZone   string
	Location   *time.Location
	// DelayMin..DelayMax: random wait after a window stops running.
	DelayMin time.Duration
	DelayMax time.Duration
	// SpacingMin..SpacingMax: random gap between two openings (any accounts).
	SpacingMin time.Duration
	SpacingMax time.Duration
	// MaxPerAccountPerDay bounds the automatic attempts (sent, failed or dry
	// run) per account and day. One unopened period gets one attempt, so this
	// only matters when the state keeps flipping.
	MaxPerAccountPerDay int
	// CheckInterval is how often the plan is evaluated (hidden; tests shorten it).
	CheckInterval time.Duration
}

// WarmupPrompt is the text of a window-opening request: "Reply with <n>."
// with a random number, so the requests are short and not all identical.
func WarmupPrompt(n int) string {
	return "Reply with " + strconv.Itoa(n) + "."
}

const (
	// warmupPromptMin..warmupPromptMax bound the random number in the prompt.
	warmupPromptMin = 10
	warmupPromptMax = 9999
)

const defaultWarmupTimeZone = "Asia/Shanghai"

// DefaultWarmupConfig returns the documented defaults (disabled).
func DefaultWarmupConfig() WarmupConfig {
	loc, err := time.LoadLocation(defaultWarmupTimeZone)
	if err != nil {
		loc = time.UTC
	}
	return WarmupConfig{
		Model:               "gpt-6-luna",
		ReasoningEffort:     "low",
		HoursStart:          9 * time.Hour,
		HoursEnd:            22 * time.Hour,
		TimeZone:            defaultWarmupTimeZone,
		Location:            loc,
		DelayMin:            20 * time.Minute,
		DelayMax:            90 * time.Minute,
		MaxPerAccountPerDay: 2,
		CheckInterval:       time.Minute,
	}
}

// Mode is "off" or "on".
func (w WarmupConfig) Mode() string {
	if w.Enabled {
		return "on"
	}
	return "off"
}

// HoursAll reports that every time of day is allowed.
func (w WarmupConfig) HoursAll() bool {
	return warmupLength(w) == 24*time.Hour
}

// HoursText formats the allowed hours as "HH:MM-HH:MM".
func (w WarmupConfig) HoursText() string {
	return clockText(w.HoursStart) + "-" + clockText(w.HoursEnd)
}

func clockText(d time.Duration) string {
	m := int(d / time.Minute)
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

func rangeText(lo, hi time.Duration) string {
	if lo == hi {
		return lo.String()
	}
	return lo.String() + "-" + hi.String()
}

// Summary is part of Config.Summary (change detection and logging).
func (w WarmupConfig) Summary() string {
	if !w.Enabled {
		return "warmup=off"
	}
	return fmt.Sprintf("warmup=%s model=%s effort=%q hours=%s tz=%s delay=%s spacing=%s max_per_account_per_day=%d check=%s",
		w.Mode(), w.Model, w.ReasoningEffort, w.HoursText(), w.TimeZone,
		rangeText(w.DelayMin, w.DelayMax), rangeText(w.SpacingMin, w.SpacingMax), w.MaxPerAccountPerDay, w.CheckInterval)
}

func parseWarmup(w *WarmupConfig, rc *rawConfig) error {
	if rc.WarmupEnabled != nil {
		w.Enabled = *rc.WarmupEnabled
	}
	if rc.WarmupModel != nil {
		w.Model = strings.TrimSpace(*rc.WarmupModel)
	}
	if rc.WarmupReasoningEffort != nil {
		w.ReasoningEffort = strings.ToLower(strings.TrimSpace(*rc.WarmupReasoningEffort))
	}
	if rc.WarmupHours != nil {
		start, end, err := parseHours(*rc.WarmupHours)
		if err != nil {
			return err
		}
		w.HoursStart, w.HoursEnd = start, end
	}
	if rc.WarmupTimezone != nil {
		name := strings.TrimSpace(*rc.WarmupTimezone)
		if name == "" {
			name = defaultWarmupTimeZone
		}
		loc, err := time.LoadLocation(name)
		if err != nil {
			return fmt.Errorf("warmup_timezone: %w", err)
		}
		w.TimeZone, w.Location = name, loc
	}
	if rc.WarmupDelay != nil {
		lo, hi, err := parseDurationRange("warmup_delay", *rc.WarmupDelay)
		if err != nil {
			return err
		}
		w.DelayMin, w.DelayMax = lo, hi
	}
	if rc.WarmupSpacing != nil {
		lo, hi, err := parseDurationRange("warmup_spacing", *rc.WarmupSpacing)
		if err != nil {
			return err
		}
		w.SpacingMin, w.SpacingMax = lo, hi
	}
	if rc.WarmupMaxPerAccountPerDay != nil {
		if *rc.WarmupMaxPerAccountPerDay < 1 {
			return fmt.Errorf("warmup_max_per_account_per_day must be at least 1, got %d", *rc.WarmupMaxPerAccountPerDay)
		}
		w.MaxPerAccountPerDay = *rc.WarmupMaxPerAccountPerDay
	}
	if rc.WarmupCheckInterval != nil {
		d, err := parseDuration("warmup_check_interval", *rc.WarmupCheckInterval)
		if err != nil {
			return err
		}
		if d < time.Second {
			return fmt.Errorf("warmup_check_interval must be at least 1s, got %s", d)
		}
		w.CheckInterval = d
	}
	if w.Enabled && w.Model == "" {
		return fmt.Errorf("warmup_model is required when warmup_enabled is true")
	}
	return nil
}

// parseHours parses "HH:MM-HH:MM" (24:00 allowed as an end).
func parseHours(s string) (time.Duration, time.Duration, error) {
	parts := strings.Split(strings.TrimSpace(s), "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("warmup_hours must look like 09:00-22:00, got %q", s)
	}
	var out [2]time.Duration
	for i, p := range parts {
		hm := strings.Split(strings.TrimSpace(p), ":")
		if len(hm) != 2 {
			return 0, 0, fmt.Errorf("warmup_hours: %q is not HH:MM", p)
		}
		h, errH := strconv.Atoi(hm[0])
		m, errM := strconv.Atoi(hm[1])
		if errH != nil || errM != nil || h < 0 || m < 0 || m > 59 || h > 24 || (h == 24 && m != 0) {
			return 0, 0, fmt.Errorf("warmup_hours: %q is not a valid time", p)
		}
		out[i] = time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
	}
	if out[0] == 24*time.Hour {
		out[0] = 0
	}
	return out[0], out[1], nil
}

// parseDurationRange parses "20m-90m" or a single duration.
func parseDurationRange(name, s string) (time.Duration, time.Duration, error) {
	s = strings.TrimSpace(s)
	lo, hi := s, s
	if i := strings.Index(s, "-"); i > 0 {
		lo, hi = s[:i], s[i+1:]
	}
	a, err := parseDuration(name, lo)
	if err != nil {
		return 0, 0, err
	}
	b, err := parseDuration(name, hi)
	if err != nil {
		return 0, 0, err
	}
	if a < 0 || b < a {
		return 0, 0, fmt.Errorf("%s must be a non-negative range like 20m-90m, got %q", name, s)
	}
	return a, b, nil
}
