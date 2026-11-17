package reorder

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReplacedInstanceCannotOverwriteState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old, c := newTestEngine(t, func(cfg *Config) { cfg.StatePath = path })
	old.Claim(c.Now())
	pick(t, old, "S-old", "A")
	if err := old.SaveState(); err != nil {
		t.Fatal(err)
	}

	// CPA loads the replacement: it reads the file and claims it.
	c.Advance(time.Second)
	repl := NewEngine(old.Config())
	repl.SetClock(c.Now)
	if err := repl.LoadState(); err != nil {
		t.Fatal(err)
	}
	repl.Claim(c.Now())
	pick(t, repl, "S-new", "A")
	if err := repl.SaveState(); err != nil {
		t.Fatal(err)
	}
	written, _ := os.ReadFile(path)

	// The replaced instance keeps running inside CPA; its save must be refused.
	c.Advance(time.Second)
	pick(t, old, "S-stale", "A")
	if err := old.SaveState(); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("stale save: %v", err)
	}
	if now, _ := os.ReadFile(path); string(now) != string(written) || strings.Contains(string(now), shortKey(SessionKey("", "S-stale"))) {
		t.Fatal("replaced instance overwrote the state file")
	}

	// A rollback registers the old instance again: it owns the file again.
	c.Advance(time.Second)
	old.Claim(c.Now())
	if err := old.SaveState(); err != nil {
		t.Fatalf("after re-claim: %v", err)
	}
}
