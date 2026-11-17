package reorder

import "time"

// maxAncestorWalk bounds the walk up the parent chain (cycles are possible in
// client-supplied IDs).
const maxAncestorWalk = 8

// LineageView is the logged form of a new session's origin.
type LineageView struct {
	// Parent is the hashed parent session (same form as Decision.Session).
	Parent string `json:"parent,omitempty"`
	// ParentKnown reports that the parent is an active session in the plugin;
	// ParentAuth is the account it is served by.
	ParentKnown bool   `json:"parent_known,omitempty"`
	ParentAuth  string `json:"parent_auth,omitempty"`
	// Ancestors counts the known sessions up the parent chain (1 = the parent is
	// a top-level session, 2 = the parent itself has a parent, ...).
	Ancestors    int    `json:"ancestors,omitempty"`
	Fork         bool   `json:"fork,omitempty"`
	NodeKind     string `json:"node_kind,omitempty"`
	Subagent     string `json:"subagent,omitempty"`
	SubagentKind string `json:"subagent_kind,omitempty"`
	AgentName    string `json:"agent_name,omitempty"`
}

type lineageResult struct {
	parentKey string
	view      *LineageView
	signal    bool
}

// lineageLocked resolves a new session's lineage against the sessions the
// plugin knows. It returns a nil view when nothing was reported.
func (e *Engine) lineageLocked(in PickInput, key string, now time.Time) lineageResult {
	l := in.Lineage
	res := lineageResult{signal: l.subagentSignal()}
	if l.ParentSessionID != "" {
		if pk := SessionKey(in.Scope, l.ParentSessionID); pk != key {
			res.parentKey = pk
		}
	}
	if res.parentKey == "" && !res.signal && l.NodeKind == "" {
		return res
	}
	v := &LineageView{Fork: l.Fork, NodeKind: l.NodeKind, Subagent: l.Subagent, SubagentKind: l.SubagentKind, AgentName: l.AgentName}
	if res.parentKey != "" {
		v.Parent = shortKey(res.parentKey)
		seen := map[string]bool{key: true}
		for cur := res.parentKey; cur != "" && !seen[cur] && v.Ancestors < maxAncestorWalk; {
			seen[cur] = true
			s := e.sessions[cur]
			if s == nil || !e.sessionActive(s, now) {
				break
			}
			if v.Ancestors == 0 {
				v.ParentKnown = true
				v.ParentAuth = s.AuthID
			}
			v.Ancestors++
			cur = s.ParentKey
		}
	}
	res.view = v
	return res
}

func (e *Engine) countLineageLocked(l lineageResult, now time.Time) {
	c := e.countersLocked(now)
	if l.parentKey != "" {
		c.newWithParent++
		if l.view != nil && l.view.ParentKnown {
			c.newParentKnown++
		}
	}
	if l.signal {
		c.newSubagent++
	}
}
