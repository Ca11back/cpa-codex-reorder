package reorder

import (
	"strings"
	"time"
)

// Image generation (/v1/images/generations, /v1/images/edits).
//
// Codex answers image requests with x-codex-* quota headers that describe a
// separate, shorter window (seen as a 1-day window with almost nothing used)
// instead of the account's 5h and weekly windows. Reading them like chat
// responses would replace an account's weekly window with that day window and
// make the account look urgent. So image responses never update the chat
// windows; their headers are logged as EventImageUsage for later analysis.
//
// Observed on 2026-11-16: image responses carry x-codex-active-limit
// "imagegen_premium" and a primary window of 1440 minutes (the daily image
// quota) with no secondary window. That window is kept per account as Image,
// passively (there is no query for it), and shown on the dashboard.
//
// Image requests carry no conversation and gain nothing from a cache, so they
// are not bound to a session: each one goes to the offered account that has
// waited longest since its last image (simple rotation). Accounts whose image
// window is known to be used up are skipped first, then accounts whose chat
// quota is used up, as long as another account is offered.

const (
	// KindImage: an image request routed by rotation, without a binding.
	KindImage = "image"
	// EventImageUsage: one image response with its raw x-codex-* headers.
	EventImageUsage = "image_usage"

	decidedRotation = "rotation"
)

// IsImagePath reports CPA's inbound image endpoints (metadata request_path).
func IsImagePath(path string) bool {
	path = strings.TrimSpace(path)
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimRight(path, "/")
	return strings.HasSuffix(path, "/images/generations") || strings.HasSuffix(path, "/images/edits")
}

// isImageModel reports an image model name ("gpt-image-…", optionally prefixed).
// CPA's direct image path reports it as the usage model.
func isImageModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	return strings.HasPrefix(model, "gpt-image")
}

// ImageUsageEvent is the log form of one image response.
type ImageUsageEvent struct {
	At     time.Time `json:"at"`
	AuthID string    `json:"auth"`
	Model  string    `json:"model,omitempty"`
	Failed bool      `json:"failed,omitempty"`
	Status int       `json:"status,omitempty"`
	// Quota holds the response's x-codex-* headers as received (lower-cased names).
	Quota map[string]string `json:"quota,omitempty"`
}

func (e *Engine) imagePickLocked(in PickInput, ids []string, now time.Time) PickResult {
	var both, imageOK []string
	for _, id := range ids {
		acc := e.accounts[id]
		if acc.Image.Exhausted(now) {
			continue
		}
		imageOK = append(imageOK, id)
		if !acc.exhausted(now) {
			both = append(both, id)
		}
	}
	reason := "生图请求：轮流分配，选最久没分到生图的账号，不建立会话绑定"
	pool := both
	switch {
	case len(both) > 0:
	case len(imageOK) > 0:
		pool = imageOK
		reason += "（生图额度没用满的候选账号，周额度都已用尽）"
	default:
		pool = ids
		reason += "（候选账号的生图额度都已用尽）"
	}
	choice := pool[0]
	for _, id := range pool[1:] {
		if e.accounts[id].LastImageAt.Before(e.accounts[choice].LastImageAt) {
			choice = id
		}
	}
	e.accounts[choice].LastImageAt = now
	e.dirty = true
	e.rememberSelectionLocked(in.SelectionID, "", choice, now)
	if ref, ok := e.selections[in.SelectionID]; ok {
		ref.image = true
		e.selections[in.SelectionID] = ref
	}
	e.logLocked(Decision{At: now, Model: in.Model, Kind: KindImage, AuthID: choice, Reason: reason, DecidedBy: decidedRotation})
	return PickResult{AuthID: choice, Handled: true, Kind: KindImage}
}

// imageUsageLocked records the image window of an image response and logs the
// response; it never touches the chat windows.
func (e *Engine) imageUsageLocked(acc *account, u UsageInput, at time.Time) {
	lookup := make(map[string]string)
	ev := ImageUsageEvent{At: logTime(at), AuthID: u.AuthID, Model: u.Model, Failed: u.Failed, Status: u.StatusCode}
	for key, values := range u.Headers {
		name := strings.ToLower(strings.TrimSpace(key))
		if !strings.HasPrefix(name, "x-codex-") || len(values) == 0 {
			continue
		}
		if ev.Quota == nil {
			ev.Quota = make(map[string]string)
		}
		ev.Quota[name] = strings.TrimSpace(values[len(values)-1])
		lookup[name] = ev.Quota[name]
	}
	if w := parseWindow(lookup, "x-codex-primary-", at); w != nil && (acc.Image == nil || !w.ObservedAt.Before(acc.Image.ObservedAt)) {
		acc.Image = w
	}
	if raw, ok := lookup["x-codex-credits-has-credits"]; ok && raw != "" {
		has := strings.EqualFold(raw, "true")
		acc.HasCredits = &has
	}
	e.emitLocked(EventImageUsage, ev)
}
