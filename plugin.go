package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Ca11back/cpa-codex-reorder/internal/reorder"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pluginName    = "codex-reorder"
	pluginVersion = "0.6.7"
	pluginAuthor  = "cpa-codex-reorder"
	// pluginRepository must be non-empty: CPA rejects plugins without a repository URL.
	pluginRepository = "https://github.com/Ca11back/cpa-codex-reorder"

	// statePath is the authenticated Management API route serving dashboard data.
	statePath   = "/codex-reorder/state"
	previewPath = "/codex-reorder/preview"
	// probeTargetsPath lists the accounts the dashboard may query; probePath
	// receives each result. The dashboard runs the query itself through CPA's
	// api-call, so CPA supplies the token and the account's own proxy.
	probeTargetsPath = "/codex-reorder/probe-targets"
	probePath        = "/codex-reorder/probe"
	// dashboardPath is the public resource route serving the (data-free) dashboard page.
	dashboardPath = "/dashboard"

	saveInterval = 15 * time.Second
	// tickInterval drives the periodic checks: weekly window ends and, when
	// stats_interval is set, the stats line.
	tickInterval = time.Minute
	// statsFullEvery is how often a stats line lists every account; the lines in
	// between list only the accounts active during their interval.
	statsFullEvery = time.Hour
	// eventQueueSize bounds the analysis log lines waiting to be written; when
	// CPA's log cannot keep up, further lines are dropped and counted.
	eventQueueSize = 1024
	// eventLogPrefix starts every analysis line: "codex-reorder <kind> <json>".
	eventLogPrefix = "codex-reorder "
	// stateDecisionLimit is how many recent decisions the state route returns.
	stateDecisionLimit = 100

	// Quota query pacing and identity. The User-Agent matches CPA Manager Plus's
	// own quota inspection, so the queries look the same upstream.
	probeUserAgent    = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"
	probeInterval     = 2 * time.Second
	probeFreshFor     = 10 * time.Minute
	probeMaxBodyBytes = 256 << 10
	probeMaxElapsed   = 2 * time.Minute

	metaCanonicalSessionID = "canonical_session_id"
	metaExecutionSessionID = "execution_session_id"
	metaCallerScope        = "caller_scope"
	metaSelectionID        = "selection_id"
	metaPinnedAuthID       = "pinned_auth_id"
	metaRequestID          = "request_id"
	metaTraceID            = "trace_id"
	// metaRequestPath is CPA's inbound request path ("/v1/images/generations").
	metaRequestPath = "request_path"
)

//go:embed web/dashboard.html
var dashboardHTML []byte

var (
	engine = reorder.NewEngine(reorder.DefaultConfig())

	// hostCall is swapped in tests.
	hostCall = callHost

	saverMu   sync.Mutex
	saverStop chan struct{}
	saverDone chan struct{}

	// eventCh carries analysis events to the goroutine that writes them to CPA's
	// log in order; eventDropped counts events lost because it was full.
	eventCh      = make(chan queuedEvent, eventQueueSize)
	eventDropped atomic.Int64
)

type queuedEvent struct {
	kind    string
	payload any
	call    func(method string, payload any) (json.RawMessage, error)
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

type capabilities struct {
	Scheduler                 bool `json:"scheduler"`
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities"`
	UsagePlugin               bool `json:"usage_plugin"`
	ManagementAPI             bool `json:"management_api"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return errorEnvelope("invalid_config", err.Error()), nil
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce:
		quiesce()
		return okEnvelope(struct{}{})
	case pluginabi.MethodPluginShutdown:
		shutdown()
		return okEnvelope(struct{}{})
	case pluginabi.MethodSchedulerPick:
		return handlePick(request)
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	cfg, err := reorder.ParseConfig(req.ConfigYAML)
	if err != nil {
		return err
	}
	previous := engine.Config()
	engine.SetConfig(cfg)
	engine.SetEventSink(emitEvent)
	// Registration (also after CPA rolls a failed replacement back to this
	// instance) makes this instance the owner of the state file again.
	engine.Claim(time.Now())
	if errLoad := engine.LoadState(); errLoad != nil {
		logAsync("warn", "codex-reorder: state not restored: "+errLoad.Error())
	}
	startSaver()
	// CPA registers and reconfigures plugins several times while starting; log only real changes.
	configLogMu.Lock()
	changed := !configLogged || previous.Summary() != cfg.Summary()
	configLogged = true
	configLogMu.Unlock()
	if changed {
		wakeWarmup() // pick up new window-opening settings without waiting for the next check
		logAsync("info", fmt.Sprintf("codex-reorder %s configured: %s", pluginVersion, cfg.Summary()))
		for _, w := range cfg.Warnings {
			logAsync("warn", "codex-reorder config: "+w)
		}
	}
	return nil
}

var (
	configLogMu  sync.Mutex
	configLogged bool
)

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepository,
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "order",
					Type:        pluginapi.ConfigFieldTypeString,
					Default:     "priority, sessions, unopened_first, urgency",
					Description: "新会话的排序规则，靠前的先比，打平才比下一条。可选：sessions（活跃会话少的优先）、priority（CPA 优先级高的优先）、unopened_first / unopened_last（周额度未开窗的放前/放后）、urgency（紧迫度高的优先）、reset（周额度先重置的优先）、remaining（剩余额度多的优先）。顺序随意；urgency、reset、remaining 是连续数值，很少打平，排在它们后面的规则多数时候比不到。已知额度用尽的账号固定排最后。",
				},
				{
					Name:        "urgency_minutes_per_percent",
					Type:        pluginapi.ConfigFieldTypeNumber,
					Default:     1,
					Description: "紧迫度的折算系数：紧迫度时间 = 周重置时间 − 剩余% × 套餐倍数 × 本值（分钟）。剩得越多越要早用。设为 0 时 urgency 等同 reset。",
				},
				{
					Name:        "plan_multipliers",
					Type:        pluginapi.ConfigFieldTypeString,
					Default:     "",
					Description: "按 CPA 账号 ID 指定套餐倍数（Plus/Team 普通席位 1，Team Pro 席位与 Pro 100 刀 10，Pro 200 刀 20，500 刀 50），格式 <账号ID>=<倍数>，逗号分隔，或 YAML 映射。未列出的账号按 1。只影响 urgency 和 remaining。",
				},
				{
					Name:        "session_ttl",
					Type:        pluginapi.ConfigFieldTypeString,
					Default:     "30m",
					Description: "粘性有效期（滑动）：会话最后一次请求后多久失效。有效期内会话固定在原账号，并计入该账号的活跃会话数。原账号某次服务不了（出错重试、冷却、禁用、额度用尽）时立即改绑到新账号，不再转回（同 CPA 默认的 session-affinity-rebind-on: any）。",
				},
				{
					Name:        "state_path",
					Type:        pluginapi.ConfigFieldTypeString,
					Default:     "codex-reorder-state.json",
					Description: "绑定和额度快照的保存路径（相对 CPA 工作目录）。留空则不保存，CPA 重启后会话重新分配。",
				},
				{
					Name:        "warmup_enabled",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Default:     false,
					Description: "自动周开窗：周额度窗口没在计时的账号（上个窗口已重置、查询确认未开窗、或没有数据），在允许时段内随机挑时间发一个极简请求（Reply with 随机数字），让周额度开始计时。默认关闭；开启后真的发请求。排序用了 unopened_last 时不执行。",
				},
				{
					Name:        "warmup_model",
					Type:        pluginapi.ConfigFieldTypeString,
					Default:     "gpt-6-luna",
					Description: "开窗请求用的模型。要选账号支持、并且计入主额度的模型；发完后插件会检查周窗口是否真的开了，没开会记 not_opened。",
				},
				{
					Name:        "warmup_reasoning_effort",
					Type:        pluginapi.ConfigFieldTypeString,
					Default:     "low",
					Description: "开窗请求的思考强度（如 none / minimal / low / medium），留空则不指定。",
				},
				{
					Name:        "warmup_hours",
					Type:        pluginapi.ConfigFieldTypeString,
					Default:     "09:00-22:00",
					Description: "允许开窗的时段（按 warmup_timezone），如 09:00-22:00；可跨零点，如 22:00-02:00；00:00-24:00 表示不限时段。计划落在时段外会顺延到下一个时段开始后的随机时刻。",
				},
				{
					Name:        "warmup_timezone",
					Type:        pluginapi.ConfigFieldTypeString,
					Default:     "Asia/Shanghai",
					Description: "warmup_hours 使用的时区（IANA 名称）。",
				},
				{
					Name:        "warmup_delay",
					Type:        pluginapi.ConfigFieldTypeString,
					Default:     "20m-90m",
					Description: "周窗口停止计时后，随机等待多久再开窗（如 20m-90m）；0s 表示不等待。期间如果有真实请求把窗口打开了，就不再开。",
				},
				{
					Name:        "warmup_spacing",
					Type:        pluginapi.ConfigFieldTypeString,
					Default:     "0s",
					Description: "两次开窗之间的随机间隔（不论哪个账号），如 5m-20m；0s 表示不间隔（插件每分钟检查一次，一次最多开一个号）。",
				},
				{
					Name:        "warmup_max_per_account_per_day",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Default:     2,
					Description: "每个账号每天最多自动开窗几次（成功、失败都算，手动“立即开窗”不计入）。同一次未开窗只开一次、失败顺延到第二天，所以正常最多 1 次；这个上限防止状态来回跳时反复开。",
				},
				{
					Name:        "manage_api_keys",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Default:     false,
					Description: "是否也调度配置文件里的 Codex API Key（codex-api-key）。默认否：插件只调度账号凭证（OAuth / PAT 授权文件），API Key 没有 Codex 额度窗口，交给 CPA 自己选。只有 API Key 可用、或 API Key 的优先级高于所有可用账号时，整次选号交回 CPA。",
				},
			},
		},
		Capabilities: capabilities{
			Scheduler:                 true,
			SchedulerAcrossPriorities: true,
			UsagePlugin:               true,
			ManagementAPI:             true,
		},
	}
}

// handlePick handles scheduler.pick. Only pure Codex picks are handled; everything
// else is left to CPA's built-in scheduler.
func handlePick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode scheduler.pick: %w", err)
	}
	in, ok := pickInput(req, engine.Config().ManageAPIKeys)
	if !ok {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	res := engine.Pick(in)
	if !res.Handled || res.AuthID == "" {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: res.AuthID, Handled: true})
}

// pickInput reduces a scheduler request to the candidates the plugin schedules.
// It reports false when the whole pick is left to CPA: other or mixed providers,
// or (unless manageKeys) no account credential available, or an API key ranked
// above every available account by CPA priority.
func pickInput(req pluginapi.SchedulerPickRequest, manageKeys bool) (reorder.PickInput, bool) {
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider != "" && provider != reorder.ProviderCodex {
		return reorder.PickInput{}, false
	}
	if len(req.Candidates) == 0 {
		return reorder.PickInput{}, false
	}
	cands := make([]reorder.Candidate, 0, len(req.Candidates))
	keyPriority, hasKey := 0, false
	for _, c := range req.Candidates {
		if !strings.EqualFold(strings.TrimSpace(c.Provider), reorder.ProviderCodex) {
			// Mixed-provider pools stay with CPA so one request is never split
			// between two schedulers.
			return reorder.PickInput{}, false
		}
		if !manageKeys && isConfigAPIKey(c) {
			if !hasKey || c.Priority > keyPriority {
				keyPriority = c.Priority
			}
			hasKey = true
			continue
		}
		cands = append(cands, reorder.Candidate{ID: strings.TrimSpace(c.ID), Priority: c.Priority})
	}
	if len(cands) == 0 {
		return reorder.PickInput{}, false
	}
	if hasKey {
		top := cands[0].Priority
		for _, c := range cands[1:] {
			if c.Priority > top {
				top = c.Priority
			}
		}
		if keyPriority > top {
			// CPA itself would use the API keys first.
			return reorder.PickInput{}, false
		}
	}
	meta := req.Options.Metadata
	sessionID := metaString(meta, metaCanonicalSessionID)
	if sessionID == "" {
		sessionID = metaString(meta, metaExecutionSessionID)
	}
	return reorder.PickInput{
		SessionID:   sessionID,
		Scope:       metaString(meta, metaCallerScope),
		SelectionID: metaString(meta, metaSelectionID),
		RequestID:   diagnosticID(metaString(meta, metaRequestID)),
		TraceID:     diagnosticID(metaString(meta, metaTraceID)),
		Pinned:      metaString(meta, metaPinnedAuthID) != "",
		Model:       req.Model,
		Image:       reorder.IsImagePath(metaString(meta, metaRequestPath)),
		Candidates:  cands,
		Lineage:     lineageFrom(meta, req.Options.Headers),
	}, true
}

// diagnosticID preserves a bounded host-generated ID for exact log correlation.
func diagnosticID(value string) string {
	if len(value) > 64 || strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-')
	}) >= 0 {
		return ""
	}
	return value
}

// isConfigAPIKey reports a Codex API key from CPA's config (codex-api-key), as
// opposed to an account credential loaded from an auth file (OAuth / PAT). CPA
// marks config credentials with source "config:<provider>[...]" and gives them
// IDs of the form "<provider>:apikey:<hash>".
func isConfigAPIKey(c pluginapi.SchedulerAuthCandidate) bool {
	if source := strings.ToLower(strings.TrimSpace(c.Attributes["source"])); strings.HasPrefix(source, "config:") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(c.Attributes["auth_kind"]), "apikey") {
		return true
	}
	return strings.Contains(strings.ToLower(c.ID), ":apikey:")
}

// isAPIKeyUsage reports a usage record served by a config API key.
func isAPIKeyUsage(rec pluginapi.UsageRecord) bool {
	return strings.EqualFold(strings.TrimSpace(rec.AuthType), "apikey") || strings.Contains(strings.ToLower(rec.AuthID), ":apikey:")
}

func metaString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	switch v := meta[key].(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return ""
	}
}

func handleUsage(raw []byte) ([]byte, error) {
	var rec pluginapi.UsageRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("decode usage.handle: %w", err)
	}
	if !engine.Config().ManageAPIKeys && isAPIKeyUsage(rec) {
		// API keys are left to CPA; tracking them would only add accounts without quota data.
		return okEnvelope(struct{}{})
	}
	at := time.Time{}
	if !rec.RequestedAt.IsZero() {
		at = rec.RequestedAt.Add(rec.Latency)
	}
	engine.HandleUsage(reorder.UsageInput{
		Provider:    strings.ToLower(strings.TrimSpace(rec.Provider)),
		AuthID:      strings.TrimSpace(rec.AuthID),
		SelectionID: strings.TrimSpace(rec.SelectionID),
		Model:       strings.TrimSpace(rec.Model),
		Failed:      rec.Failed,
		StatusCode:  rec.Failure.StatusCode,
		Headers:     rec.ResponseHeaders,
		At:          at,
	})
	return okEnvelope(struct{}{})
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes"`
	Resources []resourceRoute   `json:"resources"`
}

func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []managementRoute{{
			Method:      http.MethodGet,
			Path:        statePath,
			Description: "Codex reorder scheduler state (accounts, bindings, decisions).",
		}, {
			Method:      http.MethodPost,
			Path:        previewPath,
			Description: "Rank the current accounts under a proposed order without changing anything.",
		}, {
			Method:      http.MethodGet,
			Path:        probeTargetsPath,
			Description: "Accounts whose quota the dashboard may query through CPA's api-call.",
		}, {
			Method:      http.MethodPost,
			Path:        probePath,
			Description: "Record one quota query result (wham/usage response) for an account.",
		}, {
			Method:      http.MethodPost,
			Path:        warmupPath,
			Description: "Open an account's weekly window now (one minimal request through CPA).",
		}},
		Resources: []resourceRoute{{
			Path:        dashboardPath,
			Menu:        "Codex Reorder",
			Description: "Codex 账号绑定与选号面板（数据接口需要管理密钥）。",
		}},
	}
}

type managementRequest struct {
	Method         string
	Path           string
	Headers        http.Header
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode management.handle: %w", err)
	}
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	switch {
	case strings.HasSuffix(path, statePath):
		return jsonResponse(http.StatusOK, buildState())
	case strings.HasSuffix(path, previewPath):
		if !strings.EqualFold(req.Method, http.MethodPost) {
			return jsonResponse(http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
		}
		proposed, err := reorder.WithOverrides(engine.Config(), req.Body)
		if err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		_, hostKnown, offered, _ := hostAccounts()
		return jsonResponse(http.StatusOK, engine.Preview(proposed, offered, hostKnown))
	case strings.HasSuffix(path, probeTargetsPath):
		return jsonResponse(http.StatusOK, buildProbeTargets(strings.TrimSpace(req.Query.Get("auth_id"))))
	case strings.HasSuffix(path, probePath):
		if !strings.EqualFold(req.Method, http.MethodPost) {
			return jsonResponse(http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
		}
		status, resp := recordProbe(req.Body)
		return jsonResponse(status, resp)
	case strings.HasSuffix(path, warmupPath):
		if !strings.EqualFold(req.Method, http.MethodPost) {
			return jsonResponse(http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
		}
		status, resp := queueWarmup(req.Body)
		return jsonResponse(status, resp)
	case strings.HasSuffix(path, dashboardPath):
		return okEnvelope(managementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":           {"text/html; charset=utf-8"},
				"Cache-Control":          {"no-store"},
				"X-Content-Type-Options": {"nosniff"},
				"Referrer-Policy":        {"no-referrer"},
			},
			Body: dashboardHTML,
		})
	default:
		return okEnvelope(managementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    http.Header{"Content-Type": {"text/plain; charset=utf-8"}},
			Body:       []byte("not found"),
		})
	}
}

func jsonResponse(status int, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		Body: body,
	})
}

// stateAccount joins the engine's view of an account with CPA's credential record.
type stateAccount struct {
	reorder.AccountView
	AuthIndex      string    `json:"auth_index,omitempty"`
	Label          string    `json:"label,omitempty"`
	Email          string    `json:"email,omitempty"`
	Name           string    `json:"name,omitempty"`
	Status         string    `json:"status,omitempty"`
	StatusMessage  string    `json:"status_message,omitempty"`
	Disabled       bool      `json:"disabled,omitempty"`
	Unavailable    bool      `json:"unavailable,omitempty"`
	NextRetryAfter time.Time `json:"next_retry_after,omitempty"`
	InHost         bool      `json:"in_host"`
}

type stateResponse struct {
	Plugin     string                `json:"plugin"`
	Version    string                `json:"version"`
	HostError  string                `json:"host_error,omitempty"`
	Now        time.Time             `json:"now"`
	Config     reorder.ConfigView    `json:"config"`
	NextReason string                `json:"next_reason,omitempty"`
	NextBy     string                `json:"next_decided_by,omitempty"`
	Accounts   []stateAccount        `json:"accounts"`
	Sessions   []reorder.SessionView `json:"sessions"`
	Decisions  []reorder.Decision    `json:"decisions"`
	Warmup     reorder.WarmupStatus  `json:"warmup"`
}

type hostAuthList struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

// hostAccounts reads CPA's credential list. It returns the Codex entries, the set
// of IDs CPA knows, and the usable ones as preview candidates. host.auth.list
// covers file-backed (OAuth) credentials; credentials it does not list, such as
// config API keys, fall back to "recently offered by CPA" in the preview.
func hostAccounts() (map[string]pluginapi.HostAuthFileEntry, map[string]bool, []reorder.Candidate, error) {
	entries := map[string]pluginapi.HostAuthFileEntry{}
	known := map[string]bool{}
	var offered []reorder.Candidate
	rawList, err := hostCall(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return entries, known, nil, err
	}
	var list hostAuthList
	if err := json.Unmarshal(rawList, &list); err != nil {
		return entries, known, nil, err
	}
	now := time.Now()
	for _, entry := range list.Files {
		if !isCodexEntry(entry) || entry.ID == "" {
			continue
		}
		entries[entry.ID] = entry
		known[entry.ID] = true
		if entry.Disabled || entry.Unavailable || strings.EqualFold(entry.Status, "disabled") || entry.NextRetryAfter.After(now) {
			continue
		}
		offered = append(offered, reorder.Candidate{ID: entry.ID, Priority: entry.Priority})
	}
	return entries, known, offered, nil
}

func buildState() stateResponse {
	resp := stateResponse{Plugin: pluginName, Version: pluginVersion}
	hostEntries, hostKnown, offered, err := hostAccounts()
	if err != nil {
		resp.HostError = err.Error()
	}
	snap := engine.Snapshot(offered, hostKnown)
	resp.Now = snap.Now
	resp.Config = snap.Config
	resp.NextReason = snap.NextReason
	resp.NextBy = snap.NextBy
	resp.Sessions = snap.Sessions
	resp.Warmup = snap.Warmup
	// The dashboard shows the latest decisions only; sending all of them every
	// refresh would multiply the response size.
	if len(snap.Decisions) > stateDecisionLimit {
		snap.Decisions = snap.Decisions[:stateDecisionLimit]
	}
	resp.Decisions = snap.Decisions
	seen := make(map[string]bool, len(snap.Accounts))
	for _, view := range snap.Accounts {
		seen[view.ID] = true
		resp.Accounts = append(resp.Accounts, joinAccount(view, hostEntries))
	}
	for id, entry := range hostEntries {
		if seen[id] {
			continue
		}
		view := reorder.AccountView{ID: id, Priority: entry.Priority, WeeklyState: reorder.WeeklyUnknown, Multiplier: engine.Config().Multiplier(id)}
		resp.Accounts = append(resp.Accounts, joinAccount(view, map[string]pluginapi.HostAuthFileEntry{id: entry}))
	}
	return resp
}

func isCodexEntry(entry pluginapi.HostAuthFileEntry) bool {
	return strings.EqualFold(entry.Provider, reorder.ProviderCodex) || strings.EqualFold(entry.Type, reorder.ProviderCodex)
}

func joinAccount(view reorder.AccountView, hostEntries map[string]pluginapi.HostAuthFileEntry) stateAccount {
	out := stateAccount{AccountView: view}
	if entry, ok := hostEntries[view.ID]; ok {
		out.InHost = true
		out.AuthIndex = entry.AuthIndex
		out.Label = entry.Label
		out.Email = entry.Email
		out.Name = entry.Name
		out.Status = entry.Status
		out.StatusMessage = entry.StatusMessage
		out.Disabled = entry.Disabled
		out.Unavailable = entry.Unavailable
		out.NextRetryAfter = entry.NextRetryAfter
	}
	return out
}

func startSaver() {
	saverMu.Lock()
	defer saverMu.Unlock()
	if saverStop != nil {
		return
	}
	saverStop = make(chan struct{})
	saverDone = make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	// The window-opening loop is not waited for: it may be in the middle of a
	// model request, and CPA's quiesce call has a deadline. It exits after that
	// request because the stop channel is closed.
	warmupLoops.Add(1)
	go func(stop chan struct{}) {
		defer warmupLoops.Done()
		warmupLoop(stop)
	}(saverStop)
	go func(stop chan struct{}) {
		defer workers.Done()
		saveTicker := time.NewTicker(saveInterval)
		defer saveTicker.Stop()
		tick := time.NewTicker(tickInterval)
		defer tick.Stop()
		var stats statsClock
		stats.last = time.Now()
		for {
			select {
			case <-stop:
				return
			case <-saveTicker.C:
				saveState()
			case now := <-tick.C:
				engine.CloseEndedWindows()
				stats.tick(now, engine.Config().StatsInterval)
			}
		}
	}(saverStop)
	go func(stop chan struct{}) {
		defer workers.Done()
		for {
			select {
			case <-stop:
				return
			case ev := <-eventCh:
				writeEvent(ev)
			}
		}
	}(saverStop)
	go func(done chan struct{}) {
		workers.Wait()
		close(done)
	}(saverDone)
}

// emitEvent is the engine's event sink. It runs under the engine lock, so it only
// queues the event; a full queue drops it.
func emitEvent(kind string, payload any) {
	select {
	case eventCh <- queuedEvent{kind: kind, payload: payload, call: hostCall}:
	default:
		eventDropped.Add(1)
	}
}

// writeEvent writes one analysis line to CPA's log.
func writeEvent(ev queuedEvent) {
	raw, err := json.Marshal(ev.payload)
	if err != nil {
		return
	}
	_, _ = ev.call(pluginabi.MethodHostLog, map[string]any{"level": "info", "message": eventLogPrefix + ev.kind + " " + string(raw)})
}

// statsClock decides when to write a stats line under the current
// stats_interval (0 = off), and when the line lists every account.
type statsClock struct {
	last, lastFull time.Time
}

func (c *statsClock) tick(now time.Time, interval time.Duration) {
	if interval <= 0 {
		// Disabled: keep the counters from growing into one huge interval.
		engine.DiscardStats()
		c.last = now
		return
	}
	if now.Sub(c.last) < interval-time.Second {
		return
	}
	c.last = now
	full := c.lastFull.IsZero() || now.Sub(c.lastFull) >= statsFullEvery-time.Second
	if emitStatsMode(full) && full {
		c.lastFull = now
	}
}

// emitStats closes the current statistics interval and queues a full line.
func emitStats() { emitStatsMode(true) }

// emitStatsMode closes the current statistics interval and queues its line,
// reporting whether one was written. Idle intervals produce no line (weekly
// window ends are still reported).
func emitStatsMode(full bool) bool {
	st := engine.TakeStatsMode(full)
	if st == nil {
		return false
	}
	st.DroppedLines = eventDropped.Swap(0)
	emitEvent(reorder.EventStats, st)
	return true
}

// warmupLoops counts running window-opening loops. Quiesce does not wait for
// them; tests do, so a finished loop never touches the next test's state.
var warmupLoops sync.WaitGroup

var (
	saveErrMu   sync.Mutex
	lastSaveErr string
	// notOwnerLogged limits the "newer instance owns the state file" warning to once.
	notOwnerLogged atomic.Bool
)

// saveState persists the engine state. A failure is logged once (and again only
// when the error changes), and recovery is logged once, so an unwritable path does
// not flood CPA's log every saveInterval.
func saveState() {
	err := engine.SaveState()
	if errors.Is(err, reorder.ErrNotOwner) {
		// A newer instance owns the state file: this one has been replaced.
		go stopWorkers()
		if notOwnerLogged.CompareAndSwap(false, true) {
			logAsync("warn", "codex-reorder: a newer plugin instance owns the state file; this instance stops its background work")
		}
		return
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	saveErrMu.Lock()
	previous := lastSaveErr
	lastSaveErr = msg
	saveErrMu.Unlock()
	switch {
	case msg != "" && msg != previous:
		logAsync("warn", "codex-reorder: "+msg+" (further identical errors are not logged)")
	case msg == "" && previous != "":
		logAsync("info", "codex-reorder: state saved again after earlier failures")
	}
}

// stopWorkers stops the background goroutines (saving, statistics, log writer,
// window opening) and waits for them.
func stopWorkers() {
	saverMu.Lock()
	stop, done := saverStop, saverDone
	saverStop, saverDone = nil, nil
	saverMu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}

// quiesce is called by CPA before it replaces this instance (hot reload). CPA
// keeps a replaced instance loaded and never shuts it down, so it must stop all
// background work here; otherwise it would keep logging and saving its stale
// state. If the replacement fails, CPA registers this instance again, which
// starts the workers again (configure).
func quiesce() {
	saveState()
	stopWorkers()
}

func shutdown() {
	stopWorkers()
	saveState()
}

// logAsync writes to CPA's log without blocking the calling host request.
func logAsync(level, message string) {
	call := hostCall
	go func() {
		_, _ = call(pluginabi.MethodHostLog, map[string]any{"level": level, "message": message})
	}()
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}
