package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ca11back/cpa-codex-reorder/internal/reorder"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func resetPlugin(t *testing.T, hostEntries []pluginapi.HostAuthFileEntry) {
	t.Helper()
	cfg := reorder.DefaultConfig()
	cfg.StatePath = ""
	engine = reorder.NewEngine(cfg)
	saveErrMu.Lock()
	lastSaveErr = ""
	saveErrMu.Unlock()
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return json.Marshal(map[string]any{"files": hostEntries})
		default:
			return json.RawMessage(`{}`), nil
		}
	}
	t.Cleanup(func() { hostCall = callHost })
}

func call(t *testing.T, method string, payload any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	out, err := handleMethod(method, raw)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("%s: decode envelope: %v", method, err)
	}
	if !env.OK {
		t.Fatalf("%s: error envelope %+v", method, env.Error)
	}
	return env.Result
}

func pickRequest(provider, session string, candidates ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{
		Provider:  provider,
		Providers: []string{provider},
		Model:     "gpt-5.5",
		Options: pluginapi.SchedulerOptions{
			Headers: map[string][]string{"Content-Type": {"application/json"}},
			Metadata: map[string]any{
				"canonical_session_id": session,
				"selection_id":         "sel-" + session,
				"caller_scope":         "scope-1",
			},
		},
		Candidates: candidates,
	}
}

func codexCandidate(id string) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{ID: id, Provider: "codex", Status: "active"}
}

func decodePick(t *testing.T, raw json.RawMessage) pluginapi.SchedulerPickResponse {
	t.Helper()
	var resp pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRegisterDeclaresCapabilities(t *testing.T) {
	resetPlugin(t, nil)
	defer shutdown()
	raw := call(t, pluginabi.MethodPluginRegister, map[string]any{
		"config_yaml":    []byte("enabled: true\npriority: 10\norder: [sessions, reset]\nstate_path: ''\n"),
		"schema_version": pluginabi.SchemaVersion,
	})
	var reg struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  map[string]any     `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.SchemaVersion != pluginabi.SchemaVersion || reg.Metadata.Name != pluginName {
		t.Fatalf("registration = %+v", reg)
	}
	// CPA's validPlugin rejects registrations missing any of these fields.
	m := reg.Metadata
	if m.Name == "" || m.Version == "" || m.Author == "" || m.GitHubRepository == "" {
		t.Fatalf("metadata incomplete, CPA would refuse to load the plugin: %+v", m)
	}
	for _, capability := range []string{"scheduler", "scheduler_across_priorities", "usage_plugin", "management_api"} {
		if reg.Capabilities[capability] != true {
			t.Fatalf("capability %s not declared: %v", capability, reg.Capabilities)
		}
	}
	if got := strings.Join(engine.Config().Order, ","); got != "sessions,reset" {
		t.Fatalf("config not applied: %s", got)
	}
}

func TestRegisterRejectsBadConfig(t *testing.T) {
	resetPlugin(t, nil)
	out, err := handleMethod(pluginabi.MethodPluginRegister, []byte(`{"config_yaml":"b3JkZXI6IFtzZXNzaW9ucywgYm9ndXNd"}`)) // order: [sessions, bogus]
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"ok":false`) || !strings.Contains(string(out), "invalid_config") {
		t.Fatalf("got %s", out)
	}
}

func TestPickHandlesCodexAndStaysSticky(t *testing.T) {
	resetPlugin(t, nil)
	first := decodePick(t, call(t, pluginabi.MethodSchedulerPick, pickRequest("codex", "conv-1", codexCandidate("a.json"), codexCandidate("b.json"))))
	if !first.Handled || first.AuthID == "" {
		t.Fatalf("first = %+v", first)
	}
	second := decodePick(t, call(t, pluginabi.MethodSchedulerPick, pickRequest("codex", "conv-2", codexCandidate("a.json"), codexCandidate("b.json"))))
	if second.AuthID == first.AuthID {
		t.Fatalf("second session should go to the other idle account, both got %s", first.AuthID)
	}
	again := decodePick(t, call(t, pluginabi.MethodSchedulerPick, pickRequest("codex", "conv-1", codexCandidate("a.json"), codexCandidate("b.json"))))
	if again.AuthID != first.AuthID {
		t.Fatalf("conv-1 moved from %s to %s", first.AuthID, again.AuthID)
	}
}

// CPA's request_path metadata marks image requests: they rotate without a
// binding, and their quota headers do not become the account's weekly window.
func TestImageRequestFromRequestPath(t *testing.T) {
	resetPlugin(t, nil)
	img := pickRequest("codex", "conv-img", codexCandidate("a.json"), codexCandidate("b.json"))
	img.Model = "gpt-5.4-mini"
	img.Options.Metadata["request_path"] = "/v1/images/generations"
	first := decodePick(t, call(t, pluginabi.MethodSchedulerPick, img))
	img.Options.Metadata["selection_id"] = "sel-img-2"
	second := decodePick(t, call(t, pluginabi.MethodSchedulerPick, img))
	if !first.Handled || !second.Handled || first.AuthID == second.AuthID {
		t.Fatalf("image picks should rotate: %+v then %+v", first, second)
	}
	call(t, pluginabi.MethodUsageHandle, pluginapi.UsageRecord{
		SelectionID: "sel-conv-img",
		Provider:    "codex",
		Model:       "gpt-5.4-mini",
		AuthID:      first.AuthID,
		RequestedAt: time.Now().Add(-2 * time.Second),
		Latency:     time.Second,
		ResponseHeaders: http.Header{
			"X-Codex-Primary-Used-Percent":        {"0"},
			"X-Codex-Primary-Window-Minutes":      {"1440"},
			"X-Codex-Primary-Reset-After-Seconds": {"86400"},
		},
	})
	snap := engine.Snapshot(nil, nil)
	if len(snap.Sessions) != 0 {
		t.Fatalf("image request created a session: %+v", snap.Sessions)
	}
	for _, acc := range snap.Accounts {
		if acc.Weekly != nil {
			t.Fatalf("image quota became a weekly window: %+v", acc)
		}
	}
}

func TestPickLeavesOtherProvidersToCPA(t *testing.T) {
	resetPlugin(t, nil)
	claude := pluginapi.SchedulerAuthCandidate{ID: "c.json", Provider: "claude"}
	if resp := decodePick(t, call(t, pluginabi.MethodSchedulerPick, pickRequest("claude", "x", claude))); resp.Handled {
		t.Fatalf("claude pick handled: %+v", resp)
	}
	if resp := decodePick(t, call(t, pluginabi.MethodSchedulerPick, pickRequest("", "x", codexCandidate("a.json"), claude))); resp.Handled {
		t.Fatalf("mixed pick handled: %+v", resp)
	}
}

func TestUsageFeedsWeeklyWindow(t *testing.T) {
	resetPlugin(t, nil)
	call(t, pluginabi.MethodSchedulerPick, pickRequest("codex", "conv-1", codexCandidate("a.json")))
	reset := time.Now().Add(30 * time.Hour).Truncate(time.Second)
	call(t, pluginabi.MethodUsageHandle, pluginapi.UsageRecord{
		SelectionID: "sel-conv-1",
		Provider:    "codex",
		AuthID:      "a.json",
		RequestedAt: time.Now().Add(-2 * time.Second),
		Latency:     time.Second,
		ResponseHeaders: http.Header{
			"X-Codex-Secondary-Used-Percent":   {"41"},
			"X-Codex-Secondary-Window-Minutes": {"10080"},
			"X-Codex-Secondary-Reset-At":       {itoa(reset.Unix())},
		},
	})
	snap := engine.Snapshot(nil, nil)
	if len(snap.Accounts) != 1 || snap.Accounts[0].Weekly == nil || !snap.Accounts[0].Weekly.ResetAt.Equal(reset.UTC()) {
		t.Fatalf("accounts = %+v", snap.Accounts)
	}
	if snap.Sessions[0].Successes != 1 {
		t.Fatalf("session success not recorded: %+v", snap.Sessions[0])
	}
}

func TestManagementRegistrationMatchesHostSchema(t *testing.T) {
	resetPlugin(t, nil)
	raw := call(t, pluginabi.MethodManagementRegister, pluginapi.ManagementRegistrationRequest{BasePath: "/v0/management"})
	// Same shape as CPA's internal rpcManagementRegistrationResponse.
	var resp struct {
		Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
		Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Routes) != 5 || resp.Routes[4].Path != warmupPath || resp.Routes[0].Path != statePath || resp.Routes[0].Method != http.MethodGet ||
		resp.Routes[1].Path != previewPath || resp.Routes[1].Method != http.MethodPost ||
		resp.Routes[2].Path != probeTargetsPath || resp.Routes[2].Method != http.MethodGet ||
		resp.Routes[3].Path != probePath || resp.Routes[3].Method != http.MethodPost {
		t.Fatalf("routes = %+v", resp.Routes)
	}
	if len(resp.Resources) != 1 || resp.Resources[0].Path != dashboardPath || resp.Resources[0].Menu == "" {
		t.Fatalf("resources = %+v", resp.Resources)
	}
}

func TestStateRouteJoinsHostAccounts(t *testing.T) {
	resetPlugin(t, []pluginapi.HostAuthFileEntry{
		{ID: "a.json", AuthIndex: "1", Provider: "codex", Email: "a@example.com", Status: "active"},
		{ID: "b.json", AuthIndex: "2", Provider: "codex", Email: "b@example.com", Disabled: true},
		{ID: "g.json", AuthIndex: "3", Provider: "gemini", Email: "g@example.com"},
	})
	call(t, pluginabi.MethodSchedulerPick, pickRequest("codex", "conv-1", codexCandidate("a.json")))
	raw := call(t, pluginabi.MethodManagementHandle, map[string]any{"Method": "GET", "Path": "/v0/management" + statePath})
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Headers.Get("Content-Type"), "application/json") {
		t.Fatalf("resp = %d %v", resp.StatusCode, resp.Headers)
	}
	var state stateResponse
	if err := json.Unmarshal(resp.Body, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Accounts) != 2 {
		t.Fatalf("want 2 codex accounts, got %+v", state.Accounts)
	}
	byID := map[string]stateAccount{}
	for _, a := range state.Accounts {
		byID[a.ID] = a
	}
	if byID["a.json"].Email != "a@example.com" || byID["a.json"].Order != 1 || byID["a.json"].ActiveSessions != 1 {
		t.Fatalf("a.json = %+v", byID["a.json"])
	}
	if !byID["b.json"].Disabled || byID["b.json"].Order != 0 {
		t.Fatalf("b.json = %+v", byID["b.json"])
	}
	if len(state.Sessions) != 1 || state.Sessions[0].AuthID != "a.json" {
		t.Fatalf("sessions = %+v", state.Sessions)
	}
	if strings.Contains(string(resp.Body), "conv-1") {
		t.Fatal("raw session IDs must not be exposed")
	}
}

func TestDashboardResourceIsStaticHTML(t *testing.T) {
	resetPlugin(t, nil)
	call(t, pluginabi.MethodSchedulerPick, pickRequest("codex", "conv-1", codexCandidate("secret-account.json")))
	raw := call(t, pluginabi.MethodManagementHandle, map[string]any{"Method": "GET", "Path": "/v0/resource/plugins/codex-reorder" + dashboardPath})
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Headers.Get("Content-Type"), "text/html") {
		t.Fatalf("resp = %d %v", resp.StatusCode, resp.Headers)
	}
	body := string(resp.Body)
	if !strings.Contains(body, "/v0/management/codex-reorder/state") || strings.Contains(body, "secret-account") {
		t.Fatal("dashboard must be a data-free shell that loads state through the management API")
	}
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func previewCall(t *testing.T, method, body string) pluginapi.ManagementResponse {
	t.Helper()
	raw := call(t, pluginabi.MethodManagementHandle, map[string]any{"Method": method, "Path": "/v0/management" + previewPath, "Body": []byte(body)})
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestPreviewRanksProposedOrderWithoutChangingState(t *testing.T) {
	resetPlugin(t, []pluginapi.HostAuthFileEntry{
		{ID: "a.json", Provider: "codex"},
		{ID: "b.json", Provider: "codex"},
	})
	// a.json resets soon with little left, b.json a bit later with a lot left.
	now := time.Now()
	engine.HandleUsage(reorder.UsageInput{Provider: "codex", AuthID: "a.json", At: now, Headers: map[string][]string{
		"X-Codex-Secondary-Used-Percent": {"95"}, "X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At": {strconv.FormatInt(now.Add(10*time.Hour).Unix(), 10)},
	}})
	engine.HandleUsage(reorder.UsageInput{Provider: "codex", AuthID: "b.json", At: now, Headers: map[string][]string{
		"X-Codex-Secondary-Used-Percent": {"10"}, "X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At": {strconv.FormatInt(now.Add(10*time.Hour+10*time.Minute).Unix(), 10)},
	}})
	before := engine.Config().Summary()
	decisionsBefore := len(engine.Snapshot(nil, nil).Decisions)

	resp := previewCall(t, "POST", `{"order":["sessions","reset"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, resp.Body)
	}
	var p reorder.Preview
	if err := json.Unmarshal(resp.Body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Current.Entries[0].AuthID != "b.json" || p.Current.DecidedBy != reorder.FieldUrgency {
		t.Fatalf("current = %+v", p.Current)
	}
	if p.Proposed.Entries[0].AuthID != "a.json" || p.Proposed.DecidedBy != reorder.FieldReset {
		t.Fatalf("proposed = %+v", p.Proposed)
	}
	if strings.Join(p.Config.Order, ",") != "sessions,reset" || len(p.Config.Warnings) != 0 {
		t.Fatalf("proposed config = %+v", p.Config)
	}
	if engine.Config().Summary() != before || len(engine.Snapshot(nil, nil).Decisions) != decisionsBefore {
		t.Fatal("preview must not change the active config or the decision log")
	}

	// Warnings come back with the preview; invalid input is rejected.
	resp = previewCall(t, "POST", `{"order":["reset"]}`) // no sessions rule
	if err := json.Unmarshal(resp.Body, &p); err != nil || resp.StatusCode != http.StatusOK || len(p.Config.Warnings) != 1 {
		t.Fatalf("warning preview: %d %s", resp.StatusCode, resp.Body)
	}
	if resp := previewCall(t, "POST", `{"order":["bogus"]}`); resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(resp.Body), "bogus") {
		t.Fatalf("bad order: %d %s", resp.StatusCode, resp.Body)
	}
	if resp := previewCall(t, "GET", ``); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
}

func TestSaveFailureIsLoggedOnce(t *testing.T) {
	resetPlugin(t, nil)
	var mu sync.Mutex
	var logs []string
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostLog {
			m := payload.(map[string]any)
			mu.Lock()
			logs = append(logs, m["level"].(string)+": "+m["message"].(string))
			mu.Unlock()
		}
		return json.RawMessage(`{}`), nil
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := engine.Config()
	cfg.StatePath = filepath.Join(blocker, "state.json") // parent is a file: every save fails
	engine.SetConfig(cfg)
	for i := 0; i < 3; i++ {
		call(t, pluginabi.MethodSchedulerPick, pickRequest("codex", fmt.Sprintf("conv-%d", i), codexCandidate("a.json")))
		saveState()
	}
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	engine.SetConfig(cfg)
	call(t, pluginabi.MethodSchedulerPick, pickRequest("codex", "conv-ok", codexCandidate("a.json")))
	saveState()
	time.Sleep(100 * time.Millisecond) // logAsync delivers in the background
	mu.Lock()
	defer mu.Unlock()
	warns, infos := 0, 0
	for _, l := range logs {
		switch {
		case strings.HasPrefix(l, "warn: codex-reorder: save state"):
			warns++
		case strings.HasPrefix(l, "info: codex-reorder: state saved again"):
			infos++
		}
	}
	if warns != 1 || infos != 1 {
		t.Fatalf("want one warning and one recovery line, got %q", logs)
	}
}

func TestAnalysisLinesReachCPALog(t *testing.T) {
	resetPlugin(t, nil)
	var mu sync.Mutex
	var lines []string
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostLog {
			m := payload.(map[string]any)
			if msg := m["message"].(string); strings.HasPrefix(msg, eventLogPrefix) {
				mu.Lock()
				lines = append(lines, m["level"].(string)+": "+msg)
				mu.Unlock()
			}
		}
		return json.RawMessage(`{}`), nil
	}
	engine.SetEventSink(emitEvent)
	startSaver()

	call(t, pluginabi.MethodSchedulerPick, pickRequest("codex", "conv-1", codexCandidate("a.json"), codexCandidate("b.json")))
	call(t, pluginabi.MethodSchedulerPick, pickRequest("codex", "conv-1", codexCandidate("a.json"), codexCandidate("b.json")))
	emitStats()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(lines)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("want a pick line and a stats line, got %q", lines)
	}
	if !strings.HasPrefix(lines[0], "info: codex-reorder pick {") || !strings.Contains(lines[0], `"kind":"new"`) {
		t.Fatalf("pick line: %s", lines[0])
	}
	const statsPrefix = "info: codex-reorder stats "
	if !strings.HasPrefix(lines[1], statsPrefix) {
		t.Fatalf("stats line: %s", lines[1])
	}
	var st reorder.Stats
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], statsPrefix)), &st); err != nil {
		t.Fatal(err)
	}
	if st.Picks != 2 || st.Kinds[reorder.KindNew] != 1 || st.Kinds[reorder.KindSticky] != 1 || st.ActiveSessions != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func apiKeyCandidate(id string, priority int) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{ID: id, Provider: "codex", Status: "active", Priority: priority,
		Attributes: map[string]string{"source": "config:codex[" + id + "]"}}
}

func accountCandidate(id string, priority int) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{ID: id, Provider: "codex", Status: "active", Priority: priority,
		Attributes: map[string]string{"source": "/auths/" + id, "source_backend": "file"}}
}

func TestAPIKeysAreLeftToCPA(t *testing.T) {
	resetPlugin(t, nil)
	pickWith := func(session string, cands ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickResponse {
		return decodePick(t, call(t, pluginabi.MethodSchedulerPick, pickRequest("codex", session, cands...)))
	}

	// Keys never compete with accounts, even though they look "unopened".
	for i := 0; i < 4; i++ {
		resp := pickWith(fmt.Sprintf("mixed-%d", i), apiKeyCandidate("codex:apikey:k1", 3), accountCandidate("acc-1.json", 9), apiKeyCandidate("codex:apikey:k2", 3), accountCandidate("acc-2.json", 9))
		if !resp.Handled || !strings.HasPrefix(resp.AuthID, "acc-") {
			t.Fatalf("mixed pool %d: %+v", i, resp)
		}
	}
	// Only keys available: CPA chooses.
	if resp := pickWith("keys-only", apiKeyCandidate("codex:apikey:k1", 3), apiKeyCandidate("codex:apikey:k2", 3)); resp.Handled {
		t.Fatalf("keys only: %+v", resp)
	}
	// A key ranked above every account by CPA priority: CPA chooses (it would use the key).
	if resp := pickWith("key-higher", apiKeyCandidate("codex:apikey:k1", 10), accountCandidate("acc-1.json", 9)); resp.Handled {
		t.Fatalf("higher-priority key: %+v", resp)
	}
	// Recognised by ID alone when attributes are missing.
	if resp := pickWith("id-only", pluginapi.SchedulerAuthCandidate{ID: "codex:apikey:k3", Provider: "codex"}); resp.Handled {
		t.Fatalf("key by ID: %+v", resp)
	}
	// Usage served by a key does not create an account.
	call(t, pluginabi.MethodUsageHandle, pluginapi.UsageRecord{Provider: "codex", AuthID: "codex:apikey:k1", AuthType: "apikey"})
	for _, a := range buildState().Accounts {
		if strings.Contains(a.ID, ":apikey:") {
			t.Fatalf("key tracked as account: %+v", a)
		}
	}

	// manage_api_keys: true schedules keys like accounts.
	cfg := engine.Config()
	cfg.ManageAPIKeys = true
	engine.SetConfig(cfg)
	if resp := pickWith("keys-managed", apiKeyCandidate("codex:apikey:k1", 3), apiKeyCandidate("codex:apikey:k2", 3)); !resp.Handled {
		t.Fatalf("managed keys: %+v", resp)
	}
}

func mgmtCall(t *testing.T, method, path, query, body string) pluginapi.ManagementResponse {
	t.Helper()
	req := map[string]any{"Method": method, "Path": "/v0/management" + path, "Body": []byte(body)}
	if query != "" {
		q, err := url.ParseQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		req["Query"] = q
	}
	raw := call(t, pluginabi.MethodManagementHandle, req)
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// probeHost fakes CPA: three Codex account files (one disabled) whose stored
// JSON carries a token next to the account ID.
func probeHost(t *testing.T) {
	t.Helper()
	entries := []pluginapi.HostAuthFileEntry{
		{ID: "a.json", AuthIndex: "idx-a", Provider: "codex", Email: "a@example.com"},
		{ID: "b.json", AuthIndex: "idx-b", Provider: "codex", Email: "b@example.com"},
		{ID: "off.json", AuthIndex: "idx-off", Provider: "codex", Email: "off@example.com", Disabled: true},
	}
	resetPlugin(t, entries)
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return json.Marshal(map[string]any{"files": entries})
		case pluginabi.MethodHostAuthGet:
			idx := payload.(map[string]any)["auth_index"].(string)
			return json.Marshal(map[string]any{"auth_index": idx, "json": map[string]any{
				"type": "codex", "access_token": "SECRET-TOKEN-" + idx, "account_id": "acct-" + idx,
			}})
		default:
			return json.RawMessage(`{}`), nil
		}
	}
}

func TestProbeTargetsListAccountsWithoutTokens(t *testing.T) {
	probeHost(t)
	resp := mgmtCall(t, http.MethodGet, probeTargetsPath, "", "")
	if resp.StatusCode != http.StatusOK || strings.Contains(string(resp.Body), "SECRET-TOKEN") {
		t.Fatalf("targets: %d %s", resp.StatusCode, resp.Body)
	}
	var targets probeTargetsResponse
	if err := json.Unmarshal(resp.Body, &targets); err != nil {
		t.Fatal(err)
	}
	if targets.URL != reorder.DefaultQuotaProbeURL || targets.UserAgent == "" || targets.IntervalMs != 2000 || len(targets.Targets) != 3 {
		t.Fatalf("targets: %+v", targets)
	}
	byID := map[string]probeTarget{}
	for _, tg := range targets.Targets {
		byID[tg.AuthID] = tg
	}
	if a := byID["a.json"]; a.Skip != "" || a.AuthIndex != "idx-a" || a.AccountID != "acct-idx-a" {
		t.Fatalf("a.json: %+v", a)
	}
	if off := byID["off.json"]; off.Skip != skipDisabled || off.AccountID != "" {
		t.Fatalf("disabled: %+v", off)
	}

	// Fresh data is skipped in a full refresh, but not when one account is asked for.
	engine.HandleUsage(reorder.UsageInput{Provider: "codex", AuthID: "a.json", At: time.Now(), Headers: map[string][]string{
		"X-Codex-Secondary-Used-Percent": {"5"}, "X-Codex-Secondary-Window-Minutes": {"10080"}, "X-Codex-Secondary-Reset-After-Seconds": {"90000"},
	}})
	var refreshed probeTargetsResponse
	_ = json.Unmarshal(mgmtCall(t, http.MethodGet, probeTargetsPath, "", "").Body, &refreshed)
	for _, tg := range refreshed.Targets {
		if tg.AuthID == "a.json" && tg.Skip != skipFresh {
			t.Fatalf("fresh account not skipped: %+v", tg)
		}
	}
	var single probeTargetsResponse
	_ = json.Unmarshal(mgmtCall(t, http.MethodGet, probeTargetsPath, "auth_id=a.json", "").Body, &single)
	if len(single.Targets) != 1 || single.Targets[0].Skip != "" || single.Targets[0].AccountID != "acct-idx-a" {
		t.Fatalf("single account: %+v", single.Targets)
	}
}

func TestProbeRecordsQueryResults(t *testing.T) {
	probeHost(t)
	post := func(body any) (int, probeResponse) {
		raw, _ := json.Marshal(body)
		resp := mgmtCall(t, http.MethodPost, probePath, "", string(raw))
		var out probeResponse
		_ = json.Unmarshal(resp.Body, &out)
		return resp.StatusCode, out
	}
	usage := `{"rate_limit":{"primary_window":{"used_percent":7,"limit_window_seconds":18000,"reset_after_seconds":3000},` +
		`"secondary_window":{"used_percent":42,"limit_window_seconds":604800,"reset_after_seconds":200000}}}`
	if status, out := post(map[string]any{"auth_id": "b.json", "status_code": 200, "body": usage, "elapsed_ms": 800}); status != 200 || !out.OK ||
		out.Account.WeeklyState != reorder.WeeklyOpen || out.Account.Weekly.UsedPercent != 42 {
		t.Fatalf("ok result: %d %+v", status, out)
	}
	idle := `{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000},` +
		`"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":604800}}}`
	if _, out := post(map[string]any{"auth_id": "a.json", "status_code": 200, "body": idle}); !out.OK || out.Account.WeeklyState != reorder.WeeklyIdle {
		t.Fatalf("idle result: %+v", out)
	}
	if _, out := post(map[string]any{"auth_id": "a.json", "status_code": 401, "body": `{"detail":"token expired"}`}); out.OK ||
		!strings.Contains(out.Error, "HTTP 401") || out.Account.LastProbeError == "" || out.Account.WeeklyState != reorder.WeeklyIdle {
		t.Fatalf("failed query must keep data: %+v", out)
	}
	if _, out := post(map[string]any{"auth_id": "a.json", "error": "network down"}); out.OK || out.Error != "network down" {
		t.Fatalf("transport error: %+v", out)
	}
	if status, _ := post(map[string]any{"auth_id": "codex:apikey:x", "status_code": 200, "body": usage}); status != http.StatusBadRequest {
		t.Fatalf("unknown credential accepted: %d", status)
	}
	if resp := mgmtCall(t, http.MethodGet, probePath, "", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET probe: %d", resp.StatusCode)
	}
	for _, a := range buildState().Accounts {
		if a.ID == "b.json" && (a.Weekly == nil || a.Weekly.UsedPercent != 42) {
			t.Fatalf("state not updated: %+v", a)
		}
	}
}

func workersRunning() bool {
	saverMu.Lock()
	defer saverMu.Unlock()
	return saverStop != nil
}

func TestQuiesceStopsBackgroundWorkUntilRegisteredAgain(t *testing.T) {
	resetPlugin(t, nil)
	t.Cleanup(stopWorkers)
	call(t, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("state_path: \"\"\n")})
	if !workersRunning() {
		t.Fatal("workers not started on register")
	}
	call(t, pluginabi.MethodPluginQuiesce, map[string]any{})
	if workersRunning() {
		t.Fatal("workers still running after quiesce")
	}
	// CPA rolls a failed replacement back by registering this instance again.
	call(t, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("state_path: \"\"\n")})
	if !workersRunning() {
		t.Fatal("workers not restarted after re-register")
	}
}

func TestLineageFromMetadataAndHeaders(t *testing.T) {
	meta := map[string]any{"parent_session_id": "codex:parent-1", "is_fork": true, "node_kind": "fork"}
	headers := http.Header{
		"X-Openai-Subagent":     {"memory"},
		"X-Codex-Turn-Metadata": {`{"subagent_kind":"thread_spawn","agent_name":"/root/summarizer"}`},
	}
	l := lineageFrom(meta, headers)
	if l.ParentSessionID != "codex:parent-1" || !l.Fork || l.NodeKind != "fork" || l.Subagent != "memory" || l.SubagentKind != "thread_spawn" || l.AgentName != "summarizer" {
		t.Fatalf("lineage: %+v", l)
	}
	if l := lineageFrom(nil, http.Header{"X-Openai-Subagent": {"false"}, "X-Codex-Turn-Metadata": {`{"agent_name":"main"}`}}); l.Subagent != "" || l.AgentName != "" {
		t.Fatalf("non-signals kept: %+v", l)
	}
	if got := label("evil value\nwith newline and a very long tail that goes on"); len(got) > maxLabelLength || strings.ContainsAny(got, " \n") {
		t.Fatalf("label not sanitized: %q", got)
	}
}

func TestManualWarmupSendsPinnedRequest(t *testing.T) {
	probeHost(t)
	var mu sync.Mutex
	var sent []pluginapi.HostModelExecutionRequest
	listHost := hostCall
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostModelExecute {
			mu.Lock()
			sent = append(sent, payload.(pluginapi.HostModelExecutionRequest))
			mu.Unlock()
			return json.Marshal(pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"status":"completed"}`)})
		}
		return listHost(method, payload)
	}
	// b.json has a running weekly window, a.json has no data (unopened).
	engine.HandleUsage(reorder.UsageInput{Provider: "codex", AuthID: "b.json", At: time.Now(), Headers: map[string][]string{
		"X-Codex-Secondary-Used-Percent": {"5"}, "X-Codex-Secondary-Window-Minutes": {"10080"}, "X-Codex-Secondary-Reset-After-Seconds": {"90000"},
	}})
	post := func(id string) pluginapi.ManagementResponse {
		return mgmtCall(t, http.MethodPost, warmupPath, "", `{"auth_id":"`+id+`"}`)
	}
	if resp := post("b.json"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("running window: %d %s", resp.StatusCode, resp.Body)
	}
	if resp := post("off.json"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("disabled account: %d", resp.StatusCode)
	}
	if resp := post("a.json"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("queue: %d %s", resp.StatusCode, resp.Body)
	}
	runWarmup(make(chan struct{}))
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 {
		t.Fatalf("want one request, got %d", len(sent))
	}
	req := sent[0]
	var body map[string]any
	_ = json.Unmarshal(req.Body, &body)
	reasoning, _ := body["reasoning"].(map[string]any)
	if req.AuthID != "a.json" || req.EntryProtocol != "openai-response" || req.Path != "/v1/responses" || req.Stream ||
		body["model"] != "gpt-6-luna" || body["input"] == "" || body["stream"] != false || reasoning["effort"] != "low" {
		t.Fatalf("request: %+v body %s", req, req.Body)
	}
	for _, a := range buildState().Accounts {
		if a.ID == "a.json" && a.WarmupLastResult != reorder.WarmupSent {
			t.Fatalf("result not recorded: %+v", a)
		}
	}
}

func TestQuiesceDoesNotWaitForAnOpeningRequest(t *testing.T) {
	probeHost(t)
	t.Cleanup(stopWorkers)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	listHost := hostCall
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostModelExecute {
			started <- struct{}{}
			<-release // a slow upstream
			return json.Marshal(pluginapi.HostModelExecutionResponse{StatusCode: 200})
		}
		return listHost(method, payload)
	}
	startSaver()
	if err := engine.ForceWarmup("a.json"); err != nil {
		t.Fatal(err)
	}
	wakeWarmup()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("opening request not sent")
	}
	done := make(chan struct{})
	go func() { quiesce(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("quiesce waited for the in-flight opening request")
	}
	close(release)
	warmupLoops.Wait()
}

// With stats_interval off (the default) no stats line is written and the
// counters are discarded; when set, a line is written once per interval and
// lists every account only on the hourly full line.
func TestStatsClock(t *testing.T) {
	resetPlugin(t, nil)
	var kinds []bool
	drain := func() {
		for {
			select {
			case ev := <-eventCh:
				if ev.kind == reorder.EventStats {
					kinds = append(kinds, ev.payload.(*reorder.Stats).Full)
				}
			default:
				return
			}
		}
	}
	drain()
	start := time.Now()
	var c statsClock
	c.last = start
	busy := func() {
		call(t, pluginabi.MethodSchedulerPick, pickRequest("codex", "conv-1", codexCandidate("a.json")))
	}
	busy()
	c.tick(start.Add(time.Minute), 0)
	drain()
	if len(kinds) != 0 || engine.TakeStats() != nil {
		t.Fatalf("stats off: lines %v, counters kept", kinds)
	}
	interval := 15 * time.Minute
	for m := 2; m <= 76; m++ {
		busy()
		c.tick(start.Add(time.Duration(m)*time.Minute), interval)
		drain()
	}
	// The off tick at minute 1 restarted the interval: lines at 16, 31, 46, 61
	// and 76 minutes; the first and the one an hour later are full.
	want := []bool{true, false, false, false, true}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("lines = %v, want %v", kinds, want)
	}
}
