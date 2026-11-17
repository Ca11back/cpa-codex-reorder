package reorder

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Window is one observed Codex rate-limit window (the 5h window or the weekly window).
type Window struct {
	UsedPercent   float64   `json:"used_percent"`
	WindowMinutes int       `json:"window_minutes"`
	ResetAt       time.Time `json:"reset_at"`
	ObservedAt    time.Time `json:"observed_at"`
}

// Active reports whether the window is still running at now. A window whose reset
// time has passed has reset; Codex reopens it only on the next request.
func (w *Window) Active(now time.Time) bool {
	return w != nil && !w.ResetAt.IsZero() && w.ResetAt.After(now)
}

// Exhausted reports whether the window is running and fully used.
func (w *Window) Exhausted(now time.Time) bool {
	return w.Active(now) && w.UsedPercent >= 100
}

// weeklyMinWindowMinutes separates the weekly window from the short (5h) window.
// Codex reports the weekly window as 10080 minutes and the short one as 300.
const weeklyMinWindowMinutes = 24 * 60

// ParseQuotaHeaders extracts the base Codex windows from upstream response headers.
// CPA forwards HTTP x-codex-* headers and folds websocket codex.rate_limits events
// into the same header names. Windows are classified by their length, not by the
// primary/secondary label, because weekly-only plans report the weekly window as primary.
func ParseQuotaHeaders(headers map[string][]string, observedAt time.Time) (weekly, short *Window) {
	if len(headers) == 0 {
		return nil, nil
	}
	lookup := make(map[string]string, len(headers))
	for key, values := range headers {
		if len(values) == 0 {
			continue
		}
		lookup[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(values[len(values)-1])
	}
	for _, label := range []string{"primary", "secondary"} {
		w := parseWindow(lookup, "x-codex-"+label+"-", observedAt)
		if w == nil {
			continue
		}
		if w.WindowMinutes >= weeklyMinWindowMinutes {
			if weekly == nil || w.WindowMinutes > weekly.WindowMinutes {
				weekly = w
			}
		} else if short == nil || w.WindowMinutes < short.WindowMinutes {
			short = w
		}
	}
	return weekly, short
}

func parseWindow(lookup map[string]string, prefix string, observedAt time.Time) *Window {
	used, okUsed := parseFloat(lookup[prefix+"used-percent"])
	minutes, okMinutes := parseFloat(lookup[prefix+"window-minutes"])
	if !okUsed || !okMinutes || used < 0 || used > 100 || minutes <= 0 {
		return nil
	}
	var resetAt time.Time
	if at, ok := parseFloat(lookup[prefix+"reset-at"]); ok && at > 0 {
		sec, frac := math.Modf(at)
		resetAt = time.Unix(int64(sec), int64(frac*1e9)).UTC()
	} else if after, ok := parseFloat(lookup[prefix+"reset-after-seconds"]); ok && after >= 0 {
		resetAt = observedAt.Add(time.Duration(after * float64(time.Second))).UTC()
	} else {
		return nil
	}
	return &Window{
		UsedPercent:   used,
		WindowMinutes: int(minutes),
		ResetAt:       resetAt,
		ObservedAt:    observedAt.UTC(),
	}
}

func parseFloat(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// ProbeResult is the quota of one account as reported by a quota query
// (GET chatgpt.com/backend-api/wham/usage).
type ProbeResult struct {
	Weekly *Window
	Short  *Window
	// WeeklyIdle and ShortIdle report a window that is not running: the query
	// returns 0% used and a placeholder reset one full window ahead. A query
	// never opens a window.
	WeeklyIdle bool
	ShortIdle  bool
	// NoShort: the response lists both window slots and neither is a short (5h)
	// window, so the account has no 5h limit (weekly-only plan).
	NoShort bool
}

// probeIdleSkew is the tolerance for recognising a placeholder reset.
const (
	probeIdleSkewAfter = 3 * time.Second  // with reset_after_seconds (server clock)
	probeIdleSkewAt    = 30 * time.Second // with reset_at only (includes clock skew)
)

type usageWindow struct {
	UsedPercent *float64        `json:"used_percent"`
	Seconds     float64         `json:"limit_window_seconds"`
	ResetAt     json.RawMessage `json:"reset_at"`
	ResetAfter  *float64        `json:"reset_after_seconds"`
}

// ParseUsageResponse decodes a wham/usage response body observed at observedAt.
// Windows are classified by length like ParseQuotaHeaders.
func ParseUsageResponse(body []byte, observedAt time.Time) (ProbeResult, error) {
	var resp struct {
		RateLimit map[string]json.RawMessage `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return ProbeResult{}, fmt.Errorf("quota response is not valid JSON: %w", err)
	}
	if resp.RateLimit == nil {
		return ProbeResult{}, errors.New("quota response has no rate_limit")
	}
	observedAt = observedAt.UTC()
	var out ProbeResult
	found := false
	slotsListed := 0
	for _, slot := range []string{"primary_window", "secondary_window"} {
		raw, listed := resp.RateLimit[slot]
		if !listed {
			continue
		}
		slotsListed++
		var uw *usageWindow
		if err := json.Unmarshal(raw, &uw); err != nil {
			return ProbeResult{}, fmt.Errorf("quota response %s is invalid: %w", slot, err)
		}
		if uw == nil {
			continue
		}
		if uw.UsedPercent == nil || math.IsNaN(*uw.UsedPercent) || *uw.UsedPercent < 0 || *uw.UsedPercent > 100 ||
			uw.Seconds <= 0 || uw.Seconds > 366*24*3600 {
			return ProbeResult{}, errors.New("quota response has an invalid window")
		}
		windowLen := time.Duration(uw.Seconds * float64(time.Second))
		reset, okReset := parseResetAt(uw.ResetAt)
		if !okReset && uw.ResetAfter != nil && *uw.ResetAfter >= 0 {
			reset, okReset = observedAt.Add(time.Duration(*uw.ResetAfter*float64(time.Second))), true
		}
		if !okReset {
			return ProbeResult{}, errors.New("quota response window has no reset time")
		}
		idle := false
		if *uw.UsedPercent <= 1e-6 {
			if uw.ResetAfter != nil {
				idle = time.Duration(*uw.ResetAfter*float64(time.Second)) >= windowLen-probeIdleSkewAfter
			} else {
				d := reset.Sub(observedAt.Add(windowLen))
				idle = d <= probeIdleSkewAt && d >= -probeIdleSkewAt
			}
		}
		w := &Window{UsedPercent: *uw.UsedPercent, WindowMinutes: int(uw.Seconds / 60), ResetAt: reset.UTC(), ObservedAt: observedAt}
		found = true
		if w.WindowMinutes >= weeklyMinWindowMinutes {
			if out.Weekly == nil || w.WindowMinutes > out.Weekly.WindowMinutes {
				out.Weekly, out.WeeklyIdle = w, idle
			}
		} else if out.Short == nil || w.WindowMinutes < out.Short.WindowMinutes {
			out.Short, out.ShortIdle = w, idle
		}
	}
	if !found {
		return ProbeResult{}, errors.New("quota response has no windows")
	}
	// An omitted slot proves nothing; a listed-but-null slot does.
	out.NoShort = out.Short == nil && slotsListed == 2
	return out, nil
}

// parseResetAt accepts Unix seconds (number or numeric string) or an RFC 3339 time.
func parseResetAt(raw json.RawMessage) (time.Time, bool) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return time.Time{}, false
	}
	if v, ok := parseFloat(s); ok && v > 0 {
		sec, frac := math.Modf(v)
		return time.Unix(int64(sec), int64(frac*1e9)).UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}
