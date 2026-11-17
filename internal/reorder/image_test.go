package reorder

import (
	"strconv"
	"testing"
	"time"
)

func imagePick(t *testing.T, e *Engine, sel string, ids ...string) PickResult {
	t.Helper()
	res := e.Pick(PickInput{SessionID: "img-" + sel, SelectionID: sel, Model: "gpt-5.4-mini", Image: true, Candidates: cands(ids...)})
	if !res.Handled || res.Kind != KindImage {
		t.Fatalf("image pick %s: %+v", sel, res)
	}
	return res
}

func TestIsImagePath(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/images/generations":       true,
		"/v1/images/edits":             true,
		"/v1/images/edits/":            true,
		"/v1/images/generations?x=1":   true,
		"/backend/v1/images/edits":     true,
		"/v1/responses":                false,
		"/v1/images/variations":        false,
		"":                             false,
		"/v1/chat/completions":         false,
		"/v1/images/generations-extra": false,
	} {
		if got := IsImagePath(path); got != want {
			t.Errorf("IsImagePath(%q) = %v, want %v", path, got, want)
		}
	}
}

// Image requests rotate over the offered accounts and create no binding, even
// when they carry a session ID.
func TestImageRequestsRotateWithoutBinding(t *testing.T) {
	e, c := newTestEngine(t, nil)
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		res := imagePick(t, e, "img-"+strconv.Itoa(i), "A", "B", "C")
		seen[res.AuthID]++
		c.Advance(time.Second)
	}
	if seen["A"] != 2 || seen["B"] != 2 || seen["C"] != 2 {
		t.Fatalf("rotation = %v", seen)
	}
	snap := e.Snapshot(nil, nil)
	if len(snap.Sessions) != 0 {
		t.Fatalf("image requests created sessions: %+v", snap.Sessions)
	}
	// Chat sessions are unaffected: a new one still sees every account idle.
	if res := pick(t, e, "S1", "A", "B", "C"); res.Kind != KindNew {
		t.Fatalf("chat pick: %+v", res)
	}
	if st := e.TakeStats(); st == nil || st.Kinds[KindImage] != 6 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestImageRotationSkipsExhaustedAccounts(t *testing.T) {
	e, c := newTestEngine(t, nil)
	seedWeekly(e, "A", t0.Add(24*time.Hour), 100)
	for i := 0; i < 4; i++ {
		if res := imagePick(t, e, "img-"+strconv.Itoa(i), "A", "B"); res.AuthID != "B" {
			t.Fatalf("pick %d went to exhausted A", i)
		}
		c.Advance(time.Second)
	}
	// With nothing else offered, the exhausted account still serves.
	if res := imagePick(t, e, "img-only", "A"); res.AuthID != "A" {
		t.Fatalf("only candidate: %+v", res)
	}
}

// The quota headers of an image response (a separate 1-day window) must not
// replace the account's chat windows; they are logged as image_usage instead.
func TestImageUsageDoesNotTouchChatWindows(t *testing.T) {
	e, c := newTestEngine(t, nil)
	var events []ImageUsageEvent
	e.SetEventSink(func(kind string, payload any) {
		if kind == EventImageUsage {
			events = append(events, payload.(ImageUsageEvent))
		}
	})
	weeklyReset := t0.Add(5 * 24 * time.Hour)
	seedWeekly(e, "A", weeklyReset, 40)
	dayHeaders := map[string][]string{
		"X-Codex-Primary-Used-Percent":        {"0"},
		"X-Codex-Primary-Window-Minutes":      {"1440"},
		"X-Codex-Primary-Reset-After-Seconds": {"86400"},
		"Content-Type":                        {"application/json"},
	}

	// Routed by the plugin: recognised through the selection.
	imagePick(t, e, "img-1", "A")
	c.Advance(time.Second)
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: "A", SelectionID: "img-1", Model: "gpt-5.4-mini", Headers: dayHeaders, At: e.now()})
	// No selection (e.g. after a restart): recognised by the image model name.
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: "A", Model: "gpt-image-2.6", Failed: true, StatusCode: 429, Headers: dayHeaders, At: e.now()})

	acc := e.accounts["A"]
	if acc.Weekly == nil || !acc.Weekly.ResetAt.Equal(weeklyReset) || acc.Weekly.UsedPercent != 40 {
		t.Fatalf("weekly window changed by an image response: %+v", acc.Weekly)
	}
	if acc.Short == nil || acc.Short.WindowMinutes != 300 {
		t.Fatalf("short window changed by an image response: %+v", acc.Short)
	}
	if len(events) != 2 {
		t.Fatalf("image_usage events = %d", len(events))
	}
	if q := events[0].Quota; q["x-codex-primary-window-minutes"] != "1440" || q["content-type"] != "" || len(q) != 3 {
		t.Fatalf("quota headers = %v", q)
	}
	if events[1].Status != 429 || !events[1].Failed || events[1].Model != "gpt-image-2.6" {
		t.Fatalf("second event = %+v", events[1])
	}

	// A chat response on the same account still updates the windows.
	seedWeekly(e, "A", weeklyReset, 55)
	if e.accounts["A"].Weekly.UsedPercent != 55 {
		t.Fatalf("chat usage ignored: %+v", e.accounts["A"].Weekly)
	}
}

func imageHeaders(used float64, resetAfter time.Duration, credits string) map[string][]string {
	return map[string][]string{
		"X-Codex-Active-Limit":                {"imagegen_premium"},
		"X-Codex-Primary-Used-Percent":        {strconv.FormatFloat(used, 'f', -1, 64)},
		"X-Codex-Primary-Window-Minutes":      {"1440"},
		"X-Codex-Primary-Reset-After-Seconds": {strconv.Itoa(int(resetAfter / time.Second))},
		"X-Codex-Secondary-Window-Minutes":    {"0"},
		"X-Codex-Credits-Has-Credits":         {credits},
	}
}

// The image window and credits flag of image responses are kept per account
// and shown in the snapshot; an older response does not overwrite a newer one.
func TestImageWindowIsRecorded(t *testing.T) {
	e, c := newTestEngine(t, nil)
	imagePick(t, e, "img-1", "A")
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: "A", SelectionID: "img-1", Headers: imageHeaders(3, 20*time.Hour, "True"), At: e.now()})
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: "A", Model: "gpt-image-2", Headers: imageHeaders(1, 21*time.Hour, "False"), At: e.now().Add(-time.Minute)})
	var view *AccountView
	for _, a := range e.Snapshot(nil, nil).Accounts {
		if a.ID == "A" {
			a := a
			view = &a
		}
	}
	if view == nil || view.Image == nil || view.Image.UsedPercent != 3 || view.Image.WindowMinutes != 1440 || !view.Image.ResetAt.Equal(t0.Add(20*time.Hour)) {
		t.Fatalf("image window = %+v", view)
	}
	if view.HasCredits == nil || *view.HasCredits {
		// The flag follows the last response that reported it.
		t.Fatalf("has_credits = %v", view.HasCredits)
	}
	if view.Weekly != nil || view.Short != nil {
		t.Fatalf("chat windows set by image responses: %+v", view)
	}
	c.Advance(time.Minute)
	if st := e.TakeStats(); st == nil || len(st.Accounts) != 1 || st.Accounts[0].ImageUsed == nil || *st.Accounts[0].ImageUsed != 3 {
		t.Fatalf("stats = %+v", st)
	}
}

// Rotation skips an account whose image window is used up, before one whose
// chat quota is used up; with nothing else offered it still serves.
func TestImageRotationSkipsUsedUpImageWindow(t *testing.T) {
	e, c := newTestEngine(t, nil)
	e.HandleUsage(UsageInput{Provider: ProviderCodex, AuthID: "A", Model: "gpt-image-2", Headers: imageHeaders(100, 5*time.Hour, "False"), At: e.now()})
	seedWeekly(e, "B", t0.Add(24*time.Hour), 100)
	for i := 0; i < 3; i++ {
		if res := imagePick(t, e, "img-c"+strconv.Itoa(i), "A", "B", "C"); res.AuthID != "C" {
			t.Fatalf("pick %d = %s, want C", i, res.AuthID)
		}
		c.Advance(time.Second)
	}
	if res := imagePick(t, e, "img-ab", "A", "B"); res.AuthID != "B" {
		t.Fatalf("image-exhausted A chosen over chat-exhausted B: %s", res.AuthID)
	}
	c.Advance(time.Second)
	if res := imagePick(t, e, "img-a", "A"); res.AuthID != "A" {
		t.Fatalf("only candidate: %s", res.AuthID)
	}
	// After the image window resets, A is back in rotation.
	c.Advance(6 * time.Hour)
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		seen[imagePick(t, e, "img-r"+strconv.Itoa(i), "A", "C").AuthID] = true
		c.Advance(time.Second)
	}
	if !seen["A"] || !seen["C"] {
		t.Fatalf("after reset = %v", seen)
	}
}
