package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ca11back/cpa-codex-reorder/internal/reorder"
)

const (
	metaParentSessionID = "parent_session_id"
	metaIsFork          = "is_fork"
	metaNodeKind        = "node_kind"

	headerSubagent     = "X-Openai-Subagent"
	headerTurnMetadata = "X-Codex-Turn-Metadata"

	maxLabelLength = 32
)

// lineageFrom reads the session origin from CPA's scheduler metadata and the
// Codex client headers. Only short, sanitized labels are kept; the parent ID is
// hashed by the engine before it is stored or logged.
func lineageFrom(meta map[string]any, headers http.Header) reorder.Lineage {
	l := reorder.Lineage{
		ParentSessionID: metaString(meta, metaParentSessionID),
		NodeKind:        label(metaString(meta, metaNodeKind)),
	}
	if v, ok := meta[metaIsFork].(bool); ok {
		l.Fork = v
	}
	if v := headerValue(headers, headerSubagent); v != "" && !strings.EqualFold(v, "false") && v != "0" {
		l.Subagent = label(v)
	}
	if raw := headerValue(headers, headerTurnMetadata); raw != "" {
		var turn struct {
			SubagentKind string `json:"subagent_kind"`
			AgentName    string `json:"agent_name"`
		}
		if json.Unmarshal([]byte(raw), &turn) == nil {
			l.SubagentKind = label(turn.SubagentKind)
			name := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(turn.AgentName), "/root/"), "/")
			if name != "root" && name != "main" {
				l.AgentName = label(name)
			}
		}
	}
	return l
}

func headerValue(headers http.Header, name string) string {
	for k, v := range headers {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
	}
	return ""
}

// label keeps a short, log-safe identifier: letters, digits and ._:-/ only.
func label(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if b.Len() >= maxLabelLength {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("._:-/", r):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
