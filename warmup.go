package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ca11back/cpa-codex-reorder/internal/reorder"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Automatic weekly window opening. The engine decides which account to open
// when; this worker sends the request through CPA (host.model.execute pinned to
// the account, so CPA uses the account's own token, proxy and client
// cloaking) and reports the outcome back.

const (
	warmupPath = "/codex-reorder/warmup"
	// warmupFirstCheck is the delay of the first check after the workers start.
	warmupFirstCheck = 15 * time.Second
	// warmupMaxManualBatch bounds how many queued manual openings one check sends.
	warmupMaxManualBatch = 20
	// apiKeyCandidateWindow: with manage_api_keys, config API keys CPA offered
	// within this window can be opened too (they are not in host.auth.list).
	apiKeyCandidateWindow = 24 * time.Hour
)

var warmupWake = make(chan struct{}, 1)

// wakeWarmup makes the worker check now (manual requests).
func wakeWarmup() {
	select {
	case warmupWake <- struct{}{}:
	default:
	}
}

func warmupLoop(stop chan struct{}) {
	timer := time.NewTimer(warmupFirstCheck)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-warmupWake:
		case <-timer.C:
		}
		runWarmup(stop)
		timer.Reset(engine.Config().Warmup.CheckInterval)
	}
}

// runWarmup sends at most one automatic opening and any queued manual ones.
func runWarmup(stop chan struct{}) {
	for i := 0; i < warmupMaxManualBatch; i++ {
		if !engine.WarmupWanted() {
			return
		}
		allowed := warmupAllowed()
		if len(allowed) == 0 {
			return
		}
		job := engine.WarmupTick(allowed)
		if job == nil {
			return
		}
		status, errMsg := sendWarmup(job)
		engine.FinishWarmup(job, status, errMsg)
		if !job.Manual {
			return
		}
		select {
		case <-stop:
			return
		default:
		}
	}
}

// warmupAllowed lists the accounts CPA would currently use: Codex account
// credentials that are enabled, available and not cooling down; with
// manage_api_keys also the config API keys CPA offered recently.
func warmupAllowed() map[string]bool {
	entries, _, _, err := hostAccounts()
	if err != nil {
		return nil
	}
	now := time.Now()
	allowed := make(map[string]bool, len(entries))
	for id, entry := range entries {
		if strings.Contains(strings.ToLower(id), ":apikey:") {
			continue
		}
		if entry.Disabled || entry.Unavailable || strings.EqualFold(entry.Status, "disabled") || entry.NextRetryAfter.After(now) {
			continue
		}
		allowed[id] = true
	}
	if engine.Config().ManageAPIKeys {
		for _, id := range engine.RecentCandidates(apiKeyCandidateWindow) {
			if strings.Contains(strings.ToLower(id), ":apikey:") {
				allowed[id] = true
			}
		}
	}
	return allowed
}

// warmupBody is a minimal OpenAI Responses request.
func warmupBody(job *reorder.WarmupJob) ([]byte, error) {
	body := map[string]any{"model": job.Model, "input": job.Prompt, "stream": false, "store": false}
	if job.ReasoningEffort != "" {
		body["reasoning"] = map[string]any{"effort": job.ReasoningEffort}
	}
	return json.Marshal(body)
}

// sendWarmup runs the request through CPA pinned to the account. It returns the
// HTTP status and an error text (empty on success).
func sendWarmup(job *reorder.WarmupJob) (int, string) {
	body, err := warmupBody(job)
	if err != nil {
		return 0, err.Error()
	}
	raw, err := hostCall(pluginabi.MethodHostModelExecute, pluginapi.HostModelExecutionRequest{
		EntryProtocol: "openai-response",
		Model:         job.Model,
		Body:          body,
		AuthID:        job.AuthID,
		Path:          "/v1/responses",
	})
	if err != nil {
		return 0, err.Error()
	}
	var resp pluginapi.HostModelExecutionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, "decode model execution response: " + err.Error()
	}
	if resp.StatusCode == 0 {
		resp.StatusCode = http.StatusOK
	}
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(strings.Join(strings.Fields(string(resp.Body)), " "), 160))
	}
	return resp.StatusCode, ""
}

type warmupRequest struct {
	AuthID string `json:"auth_id"`
}

// queueWarmup handles the dashboard's "open now" button: it queues a manual
// opening for one account and wakes the worker.
func queueWarmup(raw []byte) (int, map[string]any) {
	var req warmupRequest
	if err := json.Unmarshal(raw, &req); err != nil || strings.TrimSpace(req.AuthID) == "" {
		return http.StatusBadRequest, map[string]any{"error": "auth_id is required"}
	}
	id := strings.TrimSpace(req.AuthID)
	if !warmupAllowed()[id] {
		return http.StatusBadRequest, map[string]any{"error": "not an enabled Codex account credential known to CPA"}
	}
	if err := engine.ForceWarmup(id); err != nil {
		if errors.Is(err, reorder.ErrWindowRunning) {
			return http.StatusConflict, map[string]any{"error": "周额度窗口已在计时，无需开窗"}
		}
		return http.StatusInternalServerError, map[string]any{"error": err.Error()}
	}
	wakeWarmup()
	return http.StatusAccepted, map[string]any{"queued": true}
}
