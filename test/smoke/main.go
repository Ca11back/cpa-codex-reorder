// Command smoke runs the codex-reorder plugin inside a real CPA binary against a
// mock Codex upstream and checks the scheduling behaviour end to end.
//
// Usage (from the repository root):
//
//	go run ./test/smoke -cpa test/smoke/run/cpa -plugin test/smoke/run/plugins/codex-reorder.so
//
// The mock upstream exposes three API keys whose weekly windows expire at
// different times:
//
//	k-a  weekly resets in 20h (opened earlier)
//	k-b  weekly resets in 44h (opened earlier)
//	k-c  weekly window opens on its first request (resets 7 days after that)
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	clientKey = "smoke-client"
	mgmtKey   = "smoke-mgmt"
	model     = "gpt-5.5"
	// smokeComment must survive saving the plugin config through CPA's management API.
	smokeComment = "smoke: comment kept across management saves"
)

func main() {
	cpaBin := flag.String("cpa", "test/smoke/run/cpa", "path to the CPA binary")
	pluginSO := flag.String("plugin", "test/smoke/run/plugins/codex-reorder.so", "path to the built plugin")
	runDir := flag.String("run", "test/smoke/run", "scratch directory")
	keep := flag.Bool("keep", false, "keep CPA and the mock running after the checks (Ctrl-C to stop)")
	quick := flag.Bool("quick", false, "check plugin loading, scheduling, stickiness and quota")
	flag.Parse()

	runAbs, err := filepath.Abs(*runDir)
	must(err)
	cpaAbs, err := filepath.Abs(*cpaBin)
	must(err)
	pluginAbs, err := filepath.Abs(*pluginSO)
	must(err)

	mock := newMockUpstream()
	mockLn, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	go func() { _ = http.Serve(mockLn, mock) }()
	mockURL := "http://" + mockLn.Addr().String()

	cpaPort := freePort()
	cpaURL := fmt.Sprintf("http://127.0.0.1:%d", cpaPort)
	must(prepareRunDir(runAbs, pluginAbs, mockURL, cpaPort))

	// Never leave a CPA child behind, whatever way this program exits.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		stopCurrent()
		os.Exit(130)
	}()

	cpa := startCPA(cpaAbs, runAbs)
	defer stopCurrent()
	waitReady(cpaURL)
	log.Printf("CPA ready at %s, mock upstream at %s", cpaURL, mockURL)

	failures := 0
	check := func(ok bool, format string, args ...any) {
		status := "PASS"
		if !ok {
			status = "FAIL"
			failures++
		}
		log.Printf("[%s] %s", status, fmt.Sprintf(format, args...))
	}

	// 1. Three new sessions spread over the three idle accounts (order: sessions first).
	keys := map[string]string{}
	for _, conv := range []string{"conv-1", "conv-2", "conv-3"} {
		keys[conv] = send(cpaURL, mock, conv)
	}
	check(distinct(keys["conv-1"], keys["conv-2"], keys["conv-3"]), "new sessions spread over idle accounts: %v", keys)
	time.Sleep(500 * time.Millisecond) // usage records (quota headers) are delivered asynchronously

	// 2. All accounts carry one session and the same remaining quota; the fourth goes to
	// the most urgent one, which is the earliest weekly expiry (k-a).
	keys["conv-4"] = send(cpaURL, mock, "conv-4")
	check(keys["conv-4"] == "k-a", "4th session goes to the most urgent account k-a (got %s)", keys["conv-4"])

	// 3. Existing sessions stay on their account.
	for _, conv := range []string{"conv-1", "conv-2", "conv-3"} {
		again := send(cpaURL, mock, conv)
		check(again == keys[conv], "%s stays sticky on %s (got %s)", conv, keys[conv], again)
	}

	// 4. Dashboard data: every account has a weekly window and the ranking preview is sane.
	st := state(cpaURL)
	check(len(st.Sessions) == 4, "state shows 4 active sessions (got %d)", len(st.Sessions))
	known := 0
	for _, a := range st.Accounts {
		if a.WeeklyState == "open" {
			known++
		}
	}
	check(known == 3, "all 3 accounts have weekly quota data (got %d)", known)
	if *quick {
		if failures > 0 {
			must(fmt.Errorf("%d check(s) failed; CPA log: %s", failures, filepath.Join(runAbs, "cpa.log")))
		}
		log.Print("native plugin checks passed")
		return
	}
	next := ""
	for _, a := range st.Accounts {
		if a.Order == 1 {
			next = mock.keyForReset(a.Weekly)
		}
	}
	check(next == "k-b", "next new session preview is k-b (least loaded, more urgent than k-c) (got %q)", next)
	check(st.NextBy == "urgency", "preview is decided by urgency between k-b and k-c (got %q)", st.NextBy)

	// 5. k-a runs out of quota: the request still succeeds on another account and the session moves.
	mock.exhaust("k-a")
	moved := send(cpaURL, mock, "conv-4")
	check(moved != "k-a" && moved != "", "conv-4 is served by another account after k-a returns 429 (got %s)", moved)
	time.Sleep(500 * time.Millisecond)
	after := send(cpaURL, mock, "conv-4")
	check(after == moved, "conv-4 keeps using %s afterwards (got %s)", moved, after)
	st = state(cpaURL)
	moveLogged := false
	for _, d := range st.Decisions {
		if d.Kind == "rebind" {
			moveLogged = true
		}
	}
	check(moveLogged, "decision log records the move away from k-a")
	newcomer := send(cpaURL, mock, "conv-5")
	check(newcomer != "k-a", "new sessions avoid exhausted k-a (got %s)", newcomer)

	// 6. Restart CPA: bindings survive through the state file. Use a session that is
	// not on k-a, because sessions on the exhausted k-a are correctly moved away.
	survivor := ""
	for _, conv := range []string{"conv-1", "conv-2", "conv-3"} {
		if keys[conv] != "k-a" {
			survivor = conv
			break
		}
	}
	cpa.stop()
	cpa = startCPA(cpaAbs, runAbs)
	waitReady(cpaURL)
	again := send(cpaURL, mock, survivor)
	check(again == keys[survivor], "after CPA restart %s is still on %s (got %s)", survivor, keys[survivor], again)

	// 7. The ranking editor: preview a proposed order without changing anything,
	// then save it through CPA's own management API and watch the plugin pick it up.
	status, body := do(http.MethodPost, cpaURL+"/v0/management/codex-reorder/preview", mgmtKey, `{"order":["sessions","reset"]}`)
	var preview struct {
		Current, Proposed struct {
			Entries []json.RawMessage `json:"entries"`
		}
	}
	_ = json.Unmarshal([]byte(body), &preview)
	check(status == http.StatusOK && len(preview.Proposed.Entries) > 0 && len(preview.Current.Entries) == len(preview.Proposed.Entries),
		"preview route ranks the accounts under a proposed order (HTTP %d)", status)
	check(strings.Join(state(cpaURL).Config.Order, ",") == "priority,sessions,unopened_first,urgency", "preview leaves the active order unchanged")
	status, _ = do(http.MethodPost, cpaURL+"/v0/management/codex-reorder/preview", mgmtKey, `{"order":["bogus"]}`)
	check(status == http.StatusBadRequest, "preview rejects an unknown field (HTTP %d)", status)

	status, body = do(http.MethodPatch, cpaURL+"/v0/management/plugins/codex-reorder/config", mgmtKey,
		`{"order":["sessions","unopened_last","reset"],"urgency_minutes_per_percent":2,"plan_multipliers":{}}`)
	check(status == http.StatusOK, "CPA accepts the plugin config patch (HTTP %d %s)", status, body)
	applied := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		c := state(cpaURL).Config
		if strings.Join(c.Order, ",") == "sessions,unopened_last,reset" && c.UrgencyMinutesPerPercent == 2 {
			applied = true
			break
		}
	}
	check(applied, "plugin picks up the saved order after CPA reloads")
	saved, _ := os.ReadFile(filepath.Join(runAbs, "config.yaml"))
	check(strings.Contains(string(saved), "unopened_last") && strings.Contains(string(saved), smokeComment) && strings.Contains(string(saved), "session_ttl"),
		"config.yaml holds the new order and keeps comments and other plugin keys")

	// 8. The public dashboard page carries no data.
	page := get(cpaURL+"/v0/resource/plugins/codex-reorder/dashboard", "")
	check(strings.Contains(page, "Codex Reorder") && !strings.Contains(page, "k-a"), "dashboard page is served and data-free")

	// 9. Binding decisions are written to CPA's own log, one line each.
	cpaLog, _ := os.ReadFile(filepath.Join(runAbs, "cpa.log"))
	newLines, moveLines := 0, 0
	for _, line := range strings.Split(string(cpaLog), "\n") {
		if !strings.Contains(line, "codex-reorder pick {") {
			continue
		}
		switch {
		case strings.Contains(line, `"kind":"new"`):
			newLines++
		case strings.Contains(line, `"kind":"rebind"`):
			moveLines++
		}
	}
	check(newLines == 5 && moveLines >= 1, "CPA log has one pick line per new session and the move away from k-a (new=%d move=%d)", newLines, moveLines)

	// 10. A prefixed model ("pc/<model>") pins the request to the key with prefix pc
	// (k-c). CPA offers the plugin only that key, so the plugin serves it there; a
	// session bound elsewhere keeps its binding (like CPA's per-model affinity).
	other := ""
	for _, conv := range []string{"conv-1", "conv-2", "conv-3"} {
		if keys[conv] != "k-a" && keys[conv] != "k-c" {
			other = conv
			break
		}
	}
	pinnedOther := sendModel(cpaURL, mock, other, "pc/"+model)
	backOther := send(cpaURL, mock, other)
	pinnedNew := sendModel(cpaURL, mock, "conv-pinned", "pc/"+model)
	cpaLog, _ = os.ReadFile(filepath.Join(runAbs, "cpa.log"))
	pluginSaw := strings.Contains(string(cpaLog), `"model":"pc/`+model+`"`)
	check(other != "" && pinnedOther == "k-c" && backOther == keys[other] && pinnedNew == "k-c" && pluginSaw,
		"prefixed requests go to the prefixed key and leave other bindings in place (%s bound to %s: pinned=%s, then=%s; new=%s; plugin logged=%t)",
		other, keys[other], pinnedOther, backOther, pinnedNew, pluginSaw)

	// 10b. A transient upstream error (502) on the bound key moves the session for
	// good, like CPA's default session-affinity-rebind-on: any: it stays on the
	// new key after the old one has recovered.
	if other != "" {
		bound := keys[other]
		rebinds := func() int {
			n := 0
			for _, d := range state(cpaURL).Decisions {
				if d.Kind == "rebind" {
					n++
				}
			}
			return n
		}
		before := rebinds()
		mock.mu.Lock()
		mock.failNext[bound] = 1
		mock.mu.Unlock()
		failedOver := send(cpaURL, mock, other)
		time.Sleep(300 * time.Millisecond)
		stays := send(cpaURL, mock, other)
		rebound := rebinds() == before+1
		check(failedOver != "" && failedOver != bound && stays == failedOver && rebound,
			"a 502 on the bound key rebinds the session, which then stays on the new key (%s: %s -> %s, then %s, rebind logged=%t)", other, bound, failedOver, stays, rebound)
	}

	// 11. With manage_api_keys off (the default), config API keys are left to CPA:
	// requests are still served, by CPA's own session affinity, and the plugin
	// records nothing for them.
	status, body = do(http.MethodPatch, cpaURL+"/v0/management/plugins/codex-reorder/config", mgmtKey, `{"manage_api_keys":false}`)
	keysOff := false
	for deadline := time.Now().Add(15 * time.Second); status == http.StatusOK && time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		if !state(cpaURL).Config.ManageAPIKeys {
			keysOff = true
			break
		}
	}
	const cpaNewBinding = "session-affinity: cache miss, new binding"
	before := len(state(cpaURL).Decisions)
	cpaLog, _ = os.ReadFile(filepath.Join(runAbs, "cpa.log"))
	boundBefore := strings.Count(string(cpaLog), cpaNewBinding)
	served := send(cpaURL, mock, "conv-keys-off")
	time.Sleep(500 * time.Millisecond)
	cpaLog, _ = os.ReadFile(filepath.Join(runAbs, "cpa.log"))
	cpaBound := strings.Count(string(cpaLog), cpaNewBinding) - boundBefore
	// CPA's own round-robin may first try the exhausted k-a (it lost that cooldown
	// in the restart above) and then bind again, so count at least one binding.
	check(keysOff && served != "" && len(state(cpaURL).Decisions) == before && cpaBound >= 1,
		"with manage_api_keys off, API keys are scheduled by CPA itself (patch HTTP %d %s, served by %q, CPA bindings %d)", status, body, served, cpaBound)

	// 12. Quota query, the way the dashboard runs it: the plugin lists the account
	// (without its token), CPA's api-call fills in the token and sends the query,
	// and the plugin records the result.
	probeOK, probeDetail := runProbeCheck(cpaURL, runAbs, mock)
	check(probeOK, "quota query through CPA's api-call fills the account's quota (%s)", probeDetail)

	// 13. Automatic weekly window opening: k-c's weekly window ends, the plugin
	// opens it again with one minimal request through CPA, without a binding.
	warmOK, warmDetail := runWarmupCheck(cpaURL, runAbs, mock)
	check(warmOK, "automatic window opening reopens an ended weekly window through CPA (%s)", warmDetail)

	if failures > 0 {
		log.Printf("%d check(s) failed; CPA log: %s", failures, filepath.Join(runAbs, "cpa.log"))
	} else {
		log.Printf("all checks passed")
	}
	if *keep {
		log.Printf("dashboard: %s/v0/resource/plugins/codex-reorder/dashboard (management key %q)", cpaURL, mgmtKey)
		select {}
	}
	if failures > 0 {
		stopCurrent()
		os.Exit(1)
	}
}

// ---- mock Codex upstream ----

// The quota query (wham/usage) mock: it answers only for the probe account's
// token and records the headers CPA's api-call sent.
const (
	mockUsagePath  = "/backend-api/wham/usage"
	probeAuthFile  = "codex-probe.json"
	probeToken     = "probe-token-not-a-secret"
	probeAccountID = "acct-probe"
)

type usageQuery struct {
	authorization, accountID, userAgent string
}

// warmupPrompt matches the plugin's window-opening prompt ("Reply with <n>.").
// CPA adds its own prompt_cache_key upstream, so these requests are recognised
// by their text.
var warmupPrompt = regexp.MustCompile(`Reply with [0-9]+\.`)

// warmupSeen is the plugin's own window-opening request as the upstream saw it.
type warmupSeen struct {
	key, model, input, effort string
}

type mockUpstream struct {
	mu         sync.Mutex
	usage      []usageQuery
	warmups    []warmupSeen
	warmupGate <-chan struct{}
	start      time.Time
	resets     map[string]time.Time
	exhausted  map[string]bool
	failNext   map[string]int // answer this many requests with a 502 "server_is_overloaded"
	opened     map[string]time.Time
	last       chan servedRequest
}

type servedRequest struct {
	key    string
	conv   string
	status int
}

func newMockUpstream() *mockUpstream {
	now := time.Now()
	return &mockUpstream{
		start: now,
		resets: map[string]time.Time{
			"k-a": now.Add(20 * time.Hour).Truncate(time.Second),
			"k-b": now.Add(44 * time.Hour).Truncate(time.Second),
		},
		exhausted: map[string]bool{},
		failNext:  map[string]int{},
		opened:    map[string]time.Time{},
		last:      make(chan servedRequest, 64),
	}
}

func (m *mockUpstream) exhaust(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exhausted[key] = true
}

// setReset makes key's weekly window reset at t; the first request after that
// opens a new 7-day window.
func (m *mockUpstream) setReset(key string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resets[key] = t.Truncate(time.Second)
}

func (m *mockUpstream) weeklyReset(key string) time.Time {
	if r, ok := m.resets[key]; ok {
		if !r.After(time.Now()) {
			r = time.Now().Add(7 * 24 * time.Hour).Truncate(time.Second)
			m.resets[key] = r
		}
		return r
	}
	if _, ok := m.opened[key]; !ok {
		m.opened[key] = time.Now()
	}
	return m.opened[key].Add(7 * 24 * time.Hour).Truncate(time.Second)
}

func (m *mockUpstream) keyForReset(w *window) string {
	if w == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, key := range []string{"k-a", "k-b", "k-c"} {
		if m.weeklyReset(key).Unix() == w.ResetAt.Unix() {
			return key
		}
	}
	return ""
}

func (m *mockUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == mockUsagePath {
		m.mu.Lock()
		m.usage = append(m.usage, usageQuery{r.Header.Get("Authorization"), r.Header.Get("Chatgpt-Account-Id"), r.Header.Get("User-Agent")})
		m.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+probeToken {
			http.Error(w, `{"detail":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"plan_type":"team","rate_limit":{"allowed":true,"limit_reached":false,`+
			`"primary_window":{"used_percent":4,"limit_window_seconds":18000,"reset_after_seconds":9000},`+
			`"secondary_window":{"used_percent":23,"limit_window_seconds":604800,"reset_after_seconds":259200}}}`)
		return
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses") {
		http.NotFound(w, r)
		return
	}
	key := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	body, _ := io.ReadAll(r.Body)
	var payload struct {
		PromptCacheKey string          `json:"prompt_cache_key"`
		Model          string          `json:"model"`
		Input          json.RawMessage `json:"input"`
		Reasoning      struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	_ = json.Unmarshal(body, &payload)

	m.mu.Lock()
	var warmupGate <-chan struct{}
	if warmupPrompt.Match(payload.Input) {
		m.warmups = append(m.warmups, warmupSeen{key: key, model: payload.Model, input: string(payload.Input), effort: payload.Reasoning.Effort})
		warmupGate = m.warmupGate
	}
	reset := m.weeklyReset(key)
	exhausted := m.exhausted[key]
	overloaded := m.failNext[key] > 0
	if overloaded {
		m.failNext[key]--
	}
	m.mu.Unlock()

	if warmupGate != nil {
		select {
		case <-warmupGate:
		case <-r.Context().Done():
			return
		}
	}

	if overloaded {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":{"message":"Our servers are currently overloaded. Please try again later.","type":"service_unavailable_error","code":"server_is_overloaded"}}`)
		m.last <- servedRequest{key: key, conv: payload.PromptCacheKey, status: http.StatusBadGateway}
		return
	}

	used := "30"
	if exhausted {
		used = "100"
	}
	h := w.Header()
	h.Set("X-Codex-Primary-Used-Percent", "5")
	h.Set("X-Codex-Primary-Window-Minutes", "300")
	h.Set("X-Codex-Primary-Reset-After-Seconds", "7200")
	h.Set("X-Codex-Secondary-Used-Percent", used)
	h.Set("X-Codex-Secondary-Window-Minutes", "10080")
	h.Set("X-Codex-Secondary-Reset-At", fmt.Sprint(reset.Unix()))

	if exhausted {
		h.Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":%d,"resets_in_seconds":%d}}`,
			reset.Unix(), int(time.Until(reset).Seconds()))
		m.last <- servedRequest{key: key, conv: payload.PromptCacheKey, status: http.StatusTooManyRequests}
		return
	}
	h.Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_%d\",\"object\":\"response\",\"status\":\"completed\",\"model\":%q,\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok from %s\"}]}],\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12}}}\n\n",
		time.Now().UnixNano(), model, key)
	m.last <- servedRequest{key: key, conv: payload.PromptCacheKey, status: http.StatusOK}
}

// ---- CPA process ----

type cpaProcess struct {
	cmd  *exec.Cmd
	logF *os.File
}

var (
	currentMu sync.Mutex
	current   *cpaProcess
)

func startCPA(bin, runDir string) *cpaProcess {
	logF, err := os.OpenFile(filepath.Join(runDir, "cpa.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	must(err)
	cmd := exec.Command(bin, "-config", filepath.Join(runDir, "config.yaml"), "-local-model")
	cmd.Dir = runDir
	cmd.Stdout = logF
	cmd.Stderr = logF
	must(cmd.Start())
	p := &cpaProcess{cmd: cmd, logF: logF}
	currentMu.Lock()
	current = p
	currentMu.Unlock()
	return p
}

func stopCurrent() {
	currentMu.Lock()
	p := current
	current = nil
	currentMu.Unlock()
	p.stop()
}

func (p *cpaProcess) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		_ = p.cmd.Process.Kill()
	} else {
		_ = p.cmd.Process.Signal(os.Interrupt)
	}
	done := make(chan struct{})
	go func() { _ = p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
	_ = p.logF.Close()
	p.cmd = nil
}

func prepareRunDir(runDir, pluginSO, mockURL string, port int) error {
	for _, dir := range []string{"plugins", "auths"} {
		if err := os.MkdirAll(filepath.Join(runDir, dir), 0o755); err != nil {
			return err
		}
	}
	for _, name := range []string{"codex-reorder-state.json", "cpa.log", "config.yaml", filepath.Join("auths", probeAuthFile)} {
		_ = os.Remove(filepath.Join(runDir, name))
	}
	if filepath.Dir(pluginSO) != filepath.Join(runDir, "plugins") {
		data, err := os.ReadFile(pluginSO)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(runDir, "plugins", "codex-reorder"+filepath.Ext(pluginSO)), data, 0o644); err != nil {
			return err
		}
	}
	cfg := fmt.Sprintf(`config-version: 8
server:
  host: "127.0.0.1"
  port: %d
  kv-store:
    disabled: true
management:
  allow-remote: false
  secret-key: %q
  disable-control-panel: true
access:
  api-keys:
    - %q
oauth:
  auth-dir: %q
routing:
  strategy: "round-robin"
  session-affinity: true
plugins:
  enabled: true
  dir: %q
  configs:
    codex-reorder:
      # %s
      enabled: true
      priority: 10
      order: [priority, sessions, unopened_first, urgency]
      # The mock accounts are config API keys, which the plugin leaves to CPA by default.
      manage_api_keys: true
      urgency_minutes_per_percent: 1
      session_ttl: "30m"
      state_path: %q
      # Quota queries go to the mock instead of chatgpt.com.
      quota_probe_url: %q
api-keys:
  codex:
    - name: "mock"
      base-url: %q
      models:
        - name: %q
      keys:
        - api-key: "k-a"
        - api-key: "k-b"
        - api-key: "k-c"
          prefix: "pc"
`, port, mgmtKey, clientKey, filepath.Join(runDir, "auths"), filepath.Join(runDir, "plugins"),
		smokeComment, filepath.Join(runDir, "codex-reorder-state.json"), mockURL+mockUsagePath, mockURL, model)
	return os.WriteFile(filepath.Join(runDir, "config.yaml"), []byte(cfg), 0o600)
}

func waitReady(base string) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/v0/management/codex-reorder/state", nil)
		req.Header.Set("Authorization", "Bearer "+mgmtKey)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	must(errors.New("CPA did not become ready (plugin state route unavailable); see cpa.log"))
}

// ---- client helpers ----

// send performs one /v1/responses call for the conversation and returns the mock key that served it.
func send(base string, mock *mockUpstream, conv string) string {
	return sendModel(base, mock, conv, model)
}

// sendModel is send with an explicit model name, e.g. a prefixed "pc/<model>".
func sendModel(base string, mock *mockUpstream, conv, modelName string) string {
	for len(mock.last) > 0 {
		<-mock.last
	}
	body := fmt.Sprintf(`{"model":%q,"input":"hello from %s","stream":true,"prompt_cache_key":%q}`, modelName, conv, conv)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+clientKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	must(err)
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var text strings.Builder
	for sc.Scan() {
		text.WriteString(sc.Text())
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("request %s: HTTP %d: %s", conv, resp.StatusCode, text.String())
		return ""
	}
	served := ""
	for {
		select {
		case r := <-mock.last:
			if r.status == http.StatusOK {
				served = r.key
			}
		default:
			return served
		}
	}
}

type window struct {
	UsedPercent float64   `json:"used_percent"`
	ResetAt     time.Time `json:"reset_at"`
}

type stateDoc struct {
	Accounts []struct {
		ID          string  `json:"id"`
		Order       int     `json:"order"`
		WeeklyState string  `json:"weekly_state"`
		Weekly      *window `json:"weekly"`
	} `json:"accounts"`
	NextBy string `json:"next_decided_by"`
	Config struct {
		Order                    []string `json:"order"`
		UrgencyMinutesPerPercent float64  `json:"urgency_minutes_per_percent"`
		ManageAPIKeys            bool     `json:"manage_api_keys"`
	} `json:"config"`
	Sessions  []json.RawMessage `json:"sessions"`
	Decisions []struct {
		Kind    string `json:"kind"`
		Session string `json:"session"`
		AuthID  string `json:"auth_id"`
		From    string `json:"from_auth_id"`
	} `json:"decisions"`
}

func state(base string) stateDoc {
	var st stateDoc
	must(json.Unmarshal([]byte(get(base+"/v0/management/codex-reorder/state", mgmtKey)), &st))
	return st
}

// do sends a management request and returns the status and body.
func do(method, url, key, body string) (int, string) {
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	must(err)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func get(url, key string) string {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	must(err)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		must(fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, data))
	}
	return string(data)
}

func distinct(values ...string) bool {
	seen := map[string]bool{}
	for _, v := range values {
		if v == "" || seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}

func freePort() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// must aborts on error after stopping any running CPA child.
func must(err error) {
	if err != nil {
		stopCurrent()
		log.Fatal(err)
	}
}

// runProbeCheck adds a Codex account file and queries its quota like the dashboard.
func runProbeCheck(cpaURL, runAbs string, mock *mockUpstream) (bool, string) {
	authJSON := fmt.Sprintf(`{"type":"codex","email":"probe@example.com","access_token":%q,"account_id":%q,"priority":-5}`, probeToken, probeAccountID)
	must(os.WriteFile(filepath.Join(runAbs, "auths", probeAuthFile), []byte(authJSON), 0o600))

	type target struct {
		AuthID    string `json:"auth_id"`
		AuthIndex string `json:"auth_index"`
		AccountID string `json:"account_id"`
		Name      string `json:"name"`
		Skip      string `json:"skip"`
	}
	var plan struct {
		URL       string   `json:"url"`
		UserAgent string   `json:"user_agent"`
		Targets   []target `json:"targets"`
	}
	var t target
	raw := ""
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && t.AuthID == ""; time.Sleep(300 * time.Millisecond) {
		_, raw = do(http.MethodGet, cpaURL+"/v0/management/codex-reorder/probe-targets", mgmtKey, "")
		_ = json.Unmarshal([]byte(raw), &plan)
		for _, c := range plan.Targets {
			if c.Name == "probe@example.com" && c.Skip == "" {
				t = c
			}
		}
	}
	if t.AuthID == "" {
		return false, "probe account not listed: " + raw
	}
	if strings.Contains(raw, probeToken) {
		return false, "probe-targets leaked the token"
	}
	call, _ := json.Marshal(map[string]any{"auth_index": t.AuthIndex, "method": "GET", "url": plan.URL, "header": map[string]string{
		"Authorization": "Bearer $TOKEN$", "Chatgpt-Account-Id": t.AccountID, "User-Agent": plan.UserAgent, "Content-Type": "application/json",
	}})
	status, apiRaw := do(http.MethodPost, cpaURL+"/v0/management/api-call", mgmtKey, string(call))
	var api struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	_ = json.Unmarshal([]byte(apiRaw), &api)
	if status != http.StatusOK || api.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("api-call HTTP %d, upstream %d: %s", status, api.StatusCode, apiRaw)
	}
	rec, _ := json.Marshal(map[string]any{"auth_id": t.AuthID, "status_code": api.StatusCode, "body": api.Body, "elapsed_ms": 50})
	status, recRaw := do(http.MethodPost, cpaURL+"/v0/management/codex-reorder/probe", mgmtKey, string(rec))
	if status != http.StatusOK || !strings.Contains(recRaw, `"ok":true`) {
		return false, fmt.Sprintf("probe route HTTP %d: %s", status, recRaw)
	}
	mock.mu.Lock()
	queries := append([]usageQuery(nil), mock.usage...)
	mock.mu.Unlock()
	if len(queries) != 1 || queries[0].authorization != "Bearer "+probeToken || queries[0].accountID != probeAccountID || !strings.HasPrefix(queries[0].userAgent, "codex_cli_rs/") {
		return false, fmt.Sprintf("upstream saw %+v", queries)
	}
	for _, a := range state(cpaURL).Accounts {
		if a.ID == t.AuthID {
			if a.WeeklyState == "open" && a.Weekly != nil && a.Weekly.UsedPercent == 23 {
				return true, "weekly 23% recorded, token filled in by CPA"
			}
			return false, fmt.Sprintf("state after query: %+v", a)
		}
	}
	return false, "probe account missing from state"
}

// runWarmupCheck lets k-c's weekly window end and waits for the plugin to open it again.
func runWarmupCheck(cpaURL, runAbs string, mock *mockUpstream) (bool, string) {
	gate := make(chan struct{})
	releaseWarmup := sync.OnceFunc(func() { close(gate) })
	defer releaseWarmup()
	mock.mu.Lock()
	mock.warmupGate = gate
	mock.mu.Unlock()

	status, body := do(http.MethodPatch, cpaURL+"/v0/management/plugins/codex-reorder/config", mgmtKey,
		`{"order":["priority","sessions","unopened_first","urgency"],"manage_api_keys":true,"warmup_enabled":true,"warmup_model":"`+model+`","warmup_reasoning_effort":"low",`+
			`"warmup_hours":"00:00-24:00","warmup_delay":"1s","warmup_spacing":"0s","warmup_check_interval":"1s"}`)
	if status != http.StatusOK {
		return false, fmt.Sprintf("config patch HTTP %d %s", status, body)
	}
	applied := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && !applied; time.Sleep(300 * time.Millisecond) {
		applied = strings.Contains(get(cpaURL+"/v0/management/codex-reorder/state", mgmtKey), `"mode":"on"`)
	}
	if !applied {
		return false, "warmup settings not applied"
	}
	// k-c's weekly window ends in 3 s; the plugin learns it from this response.
	mock.setReset("k-c", time.Now().Add(3*time.Second))
	if served := sendModel(cpaURL, mock, "conv-warm", "pc/"+model); served != "k-c" {
		return false, "setup request not served by k-c: " + served
	}
	decisionsBefore := len(state(cpaURL).Decisions)
	var seen []warmupSeen
	for deadline := time.Now().Add(25 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		mock.mu.Lock()
		seen = append([]warmupSeen(nil), mock.warmups...)
		mock.mu.Unlock()
		if len(seen) > 0 {
			break
		}
	}
	if len(seen) != 1 || seen[0].key != "k-c" {
		return false, fmt.Sprintf("upstream saw warmup requests %+v", seen)
	}
	w := seen[0]
	promptKnown := warmupPrompt.MatchString(w.input)
	// CPA drops reasoning effort for models without thinking metadata (the mock
	// model is config-defined); that the plugin sets it is covered by a unit test.
	if w.model != model || (w.effort != "" && w.effort != "low") || !promptKnown {
		return false, fmt.Sprintf("warmup request %+v", w)
	}
	// The prefix leaves only k-c as a candidate while the opening is in flight.
	// This ordinary request must still create a binding.
	if served := sendModel(cpaURL, mock, "conv-during-warmup", "pc/"+model); served != "k-c" {
		return false, "overlapping request not served by k-c: " + served
	}
	releaseWarmup()

	reopened, logged := false, false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !(reopened && logged); time.Sleep(300 * time.Millisecond) {
		for _, a := range state(cpaURL).Accounts {
			if a.WeeklyState == "open" && a.Weekly != nil && time.Until(a.Weekly.ResetAt) > 6*24*time.Hour && mock.keyForReset(a.Weekly) == "k-c" {
				reopened = true
			}
		}
		cpaLog, _ := os.ReadFile(filepath.Join(runAbs, "cpa.log"))
		logged = strings.Contains(string(cpaLog), `codex-reorder warmup {`) && strings.Contains(string(cpaLog), `"result":"sent"`)
	}
	if !reopened || !logged || len(state(cpaURL).Decisions) != decisionsBefore+1 {
		return false, fmt.Sprintf("reopened=%t logged=%t decisions %d -> %d", reopened, logged, decisionsBefore, len(state(cpaURL).Decisions))
	}
	return true, "k-c reopened; only the overlapping ordinary request created a binding"
}
