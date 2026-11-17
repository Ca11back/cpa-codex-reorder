package reorder

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const stateVersion = 1

// ErrNotOwner is returned by SaveState when the state file was claimed by a newer
// plugin instance. CPA keeps replaced instances loaded after a hot reload; such
// an instance must not overwrite the file of the one that replaced it.
var ErrNotOwner = errors.New("state file is owned by a newer plugin instance")

type persistedState struct {
	Version int       `json:"version"`
	SavedAt time.Time `json:"saved_at"`
	// Claim is when the instance that wrote the file was (re)registered.
	Claim     time.Time  `json:"claim,omitempty"`
	Sessions  []*session `json:"sessions"`
	Accounts  []*account `json:"accounts"`
	Decisions []Decision `json:"decisions,omitempty"`
	// Warmup is the window-opening day counter and spacing gate.
	Warmup *warmupPersist `json:"warmup,omitempty"`
}

// LoadState restores bindings and quota snapshots from cfg.StatePath once.
// A missing file is not an error. Expired sessions are dropped on load.
func (e *Engine) LoadState() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loaded {
		return nil
	}
	e.loaded = true
	path := e.cfg.StatePath
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state %s: %w", path, err)
	}
	var st persistedState
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("decode state %s: %w", path, err)
	}
	if st.Version != stateVersion {
		return fmt.Errorf("state %s has version %d, want %d", path, st.Version, stateVersion)
	}
	now := e.now()
	for _, acc := range st.Accounts {
		if acc == nil || acc.ID == "" {
			continue
		}
		if acc.WarmupLastResult == legacyWarmupDryRun {
			// A dry run did not open anything: the period is still to be handled.
			acc.WarmupDoneFor, acc.WarmupLastResult = time.Time{}, ""
		}
		e.accounts[acc.ID] = acc
	}
	for _, s := range st.Sessions {
		if s == nil || s.Key == "" || s.AuthID == "" {
			continue
		}
		if !e.sessionActive(s, now) {
			continue
		}
		if s.LegacyDetourAuthID != "" {
			// Saved mid-detour by a version before 0.6.3: the session now
			// stays on the account that was serving it.
			s.AuthID, s.BoundAt, s.LegacyDetourAuthID = s.LegacyDetourAuthID, now, ""
			e.dirty = true
		}
		e.sessions[s.Key] = s
	}
	if len(st.Decisions) > maxDecisions {
		st.Decisions = st.Decisions[len(st.Decisions)-maxDecisions:]
	}
	if st.Warmup != nil {
		e.warmup = *st.Warmup
	}
	e.decisions = append(st.Decisions, e.decisions...)
	if len(e.decisions) > maxDecisions {
		e.decisions = append([]Decision(nil), e.decisions[len(e.decisions)-maxDecisions:]...)
	}
	return nil
}

// SaveState writes the state file when something changed since the last save.
func (e *Engine) SaveState() error {
	e.saveMu.Lock()
	defer e.saveMu.Unlock()

	e.mu.Lock()
	path := e.cfg.StatePath
	if path == "" || !e.dirty {
		e.mu.Unlock()
		return nil
	}
	now := e.now()
	e.housekeepingLocked(now, false)
	claim := e.claim
	st := persistedState{Version: stateVersion, SavedAt: now.UTC(), Claim: claim}
	for _, s := range e.sessions {
		c := *s
		st.Sessions = append(st.Sessions, &c)
	}
	for _, acc := range e.accounts {
		c := *acc
		c.Weekly = copyWindow(acc.Weekly)
		c.Short = copyWindow(acc.Short)
		st.Accounts = append(st.Accounts, &c)
	}
	st.Decisions = append([]Decision(nil), e.decisions...)
	if wp := e.warmup; wp != (warmupPersist{}) {
		st.Warmup = &wp
	}
	e.dirty = false
	e.mu.Unlock()

	if owner, errOwner := fileClaim(path); errOwner == nil && owner.After(claim) {
		return ErrNotOwner
	}
	raw, err := json.Marshal(st)
	if err == nil {
		err = writeFileAtomic(path, raw)
	}
	if err != nil {
		e.mu.Lock()
		e.dirty = true
		e.mu.Unlock()
		return fmt.Errorf("save state %s: %w", path, err)
	}
	return nil
}

// Claim marks this engine as the current owner of the state file. The plugin
// calls it whenever CPA registers or reconfigures the instance (including when
// CPA rolls a failed replacement back to it).
func (e *Engine) Claim(at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.claim = at.UTC()
	e.dirty = true
}

// fileClaim reads only the owner claim of an existing state file.
func fileClaim(path string) (time.Time, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	var head struct {
		Claim time.Time `json:"claim"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return time.Time{}, err
	}
	return head.Claim, nil
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
