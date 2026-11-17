package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ca11back/cpa-codex-reorder/internal/reorder"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Quota queries are driven by the dashboard: it asks the plugin which accounts
// to query (probe-targets), sends each query through CPA's own
// POST /v0/management/api-call (CPA fills in the account's token and uses the
// account's proxy_url), and hands the response to the plugin (probe). The plugin
// itself never sends a request and never holds a token.

type probeTarget struct {
	AuthID      string    `json:"auth_id"`
	AuthIndex   string    `json:"auth_index"`
	AccountID   string    `json:"account_id,omitempty"`
	Name        string    `json:"name"`
	Skip        string    `json:"skip,omitempty"`
	DataAt      time.Time `json:"data_at,omitempty"`
	LastProbeAt time.Time `json:"last_probe_at,omitempty"`
}

type probeTargetsResponse struct {
	URL          string        `json:"url"`
	UserAgent    string        `json:"user_agent"`
	IntervalMs   int64         `json:"interval_ms"`
	FreshSeconds int64         `json:"fresh_seconds"`
	HostError    string        `json:"host_error,omitempty"`
	Targets      []probeTarget `json:"targets"`
}

// Skip reasons reported to the dashboard.
const (
	skipDisabled = "disabled"
	skipFresh    = "fresh"
	skipNoIndex  = "no_index"
)

// buildProbeTargets lists CPA's Codex account credentials (OAuth / PAT auth
// files). With onlyID set, it returns just that account and never skips it as
// fresh (a single-account refresh is explicit).
func buildProbeTargets(onlyID string) probeTargetsResponse {
	resp := probeTargetsResponse{
		URL:          engine.Config().QuotaProbeURL,
		UserAgent:    probeUserAgent,
		IntervalMs:   probeInterval.Milliseconds(),
		FreshSeconds: int64(probeFreshFor / time.Second),
		Targets:      []probeTarget{},
	}
	entries, _, _, err := hostAccounts()
	if err != nil {
		resp.HostError = err.Error()
		return resp
	}
	now := time.Now()
	for id, entry := range entries {
		if onlyID != "" && id != onlyID {
			continue
		}
		if strings.Contains(strings.ToLower(id), ":apikey:") {
			continue
		}
		name := entry.Email
		if name == "" {
			name = entry.Label
		}
		if name == "" {
			name = id
		}
		info := engine.ProbeInfo(id)
		t := probeTarget{AuthID: id, AuthIndex: entry.AuthIndex, Name: name, DataAt: info.DataAt, LastProbeAt: info.LastProbeAt}
		switch {
		case entry.Disabled || strings.EqualFold(entry.Status, "disabled"):
			t.Skip = skipDisabled
		case entry.AuthIndex == "":
			t.Skip = skipNoIndex
		case onlyID == "" && !info.DataAt.IsZero() && now.Sub(info.DataAt) < probeFreshFor:
			t.Skip = skipFresh
		}
		if t.Skip == "" {
			t.AccountID = accountIDOf(entry.AuthIndex)
		}
		resp.Targets = append(resp.Targets, t)
	}
	sort.Slice(resp.Targets, func(i, j int) bool { return resp.Targets[i].Name < resp.Targets[j].Name })
	return resp
}

// accountIDOf reads the ChatGPT account (workspace) ID from the credential file.
// Only that field is decoded; the token in the same file is not kept.
func accountIDOf(authIndex string) string {
	raw, err := hostCall(pluginabi.MethodHostAuthGet, map[string]any{"auth_index": authIndex})
	if err != nil {
		return ""
	}
	var resp struct {
		JSON struct {
			AccountID string `json:"account_id"`
		} `json:"json"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return ""
	}
	return strings.TrimSpace(resp.JSON.AccountID)
}

type probeRequest struct {
	AuthID     string `json:"auth_id"`
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
	ElapsedMs  int64  `json:"elapsed_ms"`
	Error      string `json:"error"`
}

type probeResponse struct {
	OK      bool                 `json:"ok"`
	Error   string               `json:"error,omitempty"`
	Account *reorder.AccountView `json:"account,omitempty"`
}

// recordProbe stores one query result. Only Codex account credentials CPA
// currently lists are accepted.
func recordProbe(raw []byte) (int, probeResponse) {
	if len(raw) > probeMaxBodyBytes {
		return http.StatusRequestEntityTooLarge, probeResponse{Error: "request too large"}
	}
	var req probeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return http.StatusBadRequest, probeResponse{Error: "invalid JSON"}
	}
	req.AuthID = strings.TrimSpace(req.AuthID)
	entries, _, _, err := hostAccounts()
	if err != nil {
		return http.StatusBadGateway, probeResponse{Error: "cannot read CPA credentials: " + err.Error()}
	}
	if _, ok := entries[req.AuthID]; !ok || req.AuthID == "" {
		return http.StatusBadRequest, probeResponse{Error: "not a Codex account credential known to CPA"}
	}
	fail := func(msg string) (int, probeResponse) {
		view := engine.RecordProbeError(req.AuthID, msg)
		return http.StatusOK, probeResponse{Error: msg, Account: &view}
	}
	switch {
	case strings.TrimSpace(req.Error) != "":
		return fail(truncate(strings.TrimSpace(req.Error), 200))
	case req.StatusCode != http.StatusOK:
		return fail(fmt.Sprintf("HTTP %d: %s", req.StatusCode, truncate(strings.Join(strings.Fields(req.Body), " "), 160)))
	}
	elapsed := time.Duration(req.ElapsedMs) * time.Millisecond
	if elapsed < 0 || elapsed > probeMaxElapsed {
		elapsed = 0
	}
	observedAt := time.Now().Add(-elapsed)
	res, err := reorder.ParseUsageResponse([]byte(req.Body), observedAt)
	if err != nil {
		return fail(err.Error())
	}
	view := engine.ApplyProbe(req.AuthID, res, observedAt)
	return http.StatusOK, probeResponse{OK: true, Account: &view}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
