package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	repairBudgetMarkerPath = "/var/lib/thunder/repair-budget.json"

	// repairAttemptLimit is how many repair attempts (restart, CLI reinstall,
	// or enroll -- whichever this outage needed) the daemon makes before it
	// stops and waits for a human. Persisted so a pod restart mid-outage does
	// not re-arm the budget and repeat the mint-a-token-every-pass loop the
	// 2026-08-24 incident produced. (by claude)
	repairAttemptLimit = 5

	// repairCoolOff is how long the daemon waits between attempts once the
	// budget is spent. Only a restart is tried during cool-off; enrolling and
	// reinstalling the CLI stay off the table until a human looks at the
	// node. (by claude)
	repairCoolOff = time.Hour

	repairBudgetMarkerAbsentSentinel = "THUNDER_REPAIR_BUDGET_MARKER_ABSENT"
)

// repairBudgetMarker is the durable state behind the per-outage repair
// budget. It is written to the host, not kept only in the pod, so recreating
// the reconciler -- a pod restart or rollout -- does not reset it
// mid-outage. (by claude)
type repairBudgetMarker struct {
	Attempts      int       `json:"attempts"`
	LastAttemptAt time.Time `json:"lastAttemptAt,omitempty"`
	CoolingOff    bool      `json:"coolingOff"`
}

// isZero reports whether the marker has nothing worth persisting. (by claude)
func (m repairBudgetMarker) isZero() bool {
	return m.Attempts == 0 && !m.CoolingOff && m.LastAttemptAt.IsZero()
}

type repairBudgetStore interface {
	Load(context.Context) (repairBudgetMarker, bool, error)
	Save(context.Context, repairBudgetMarker) error
}

// hostRepairBudgetStore persists the marker on the host as a small JSON file,
// written atomically (temp file + rename) the same way the daemon writes any
// other host state. It is its own file rather than sharing one with some
// other marker, so clearing it can never have side effects on unrelated
// state. (by claude)
type hostRepairBudgetStore struct{ runner commandRunner }

func (s hostRepairBudgetStore) Load(ctx context.Context) (repairBudgetMarker, bool, error) {
	command := "if [ -e " + shellQuote(repairBudgetMarkerPath) + " ]; then cat " + shellQuote(repairBudgetMarkerPath) + "; else printf %s " + shellQuote(repairBudgetMarkerAbsentSentinel) + "; fi"
	output, err := s.runner.CombinedOutput(ctx, "/bin/sh", "-c", command)
	if err != nil {
		return repairBudgetMarker{}, false, fmt.Errorf("read repair budget marker: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if strings.TrimSpace(string(output)) == repairBudgetMarkerAbsentSentinel {
		return repairBudgetMarker{}, false, nil
	}
	var marker repairBudgetMarker
	if err := json.Unmarshal(output, &marker); err != nil {
		// A marker that cannot be parsed is treated as absent rather than as a
		// read failure: starting the budget over is safe, refusing to repair
		// because of a corrupt marker is not. (by claude)
		return repairBudgetMarker{}, true, nil
	}
	return marker, false, nil
}

func (s hostRepairBudgetStore) Save(ctx context.Context, marker repairBudgetMarker) error {
	if marker.isZero() {
		output, err := s.runner.CombinedOutput(ctx, "rm", "-f", repairBudgetMarkerPath)
		if err != nil {
			return fmt.Errorf("delete repair budget marker: %w: %s", err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("encode repair budget marker: %w", err)
	}
	command := "set -eu; umask 077; mkdir -p /var/lib/thunder; tmp=$(mktemp /var/lib/thunder/.repair-budget.XXXXXX); trap 'rm -f \"$tmp\"' EXIT; printf %s " + shellQuote(string(data)) + " > \"$tmp\"; mv \"$tmp\" " + shellQuote(repairBudgetMarkerPath) + "; trap - EXIT"
	if err := s.runner.RunShell(ctx, "repair budget marker", command); err != nil {
		return fmt.Errorf("write repair budget marker: %w", err)
	}
	return nil
}

func (r *reconciler) repairBudgetStore() repairBudgetStore {
	if r.repairBudget == nil {
		r.repairBudget = hostRepairBudgetStore{runner: r.runner}
	}
	return r.repairBudget
}

// clock lets tests control the passage of time for the cool-off window.
// (by claude)
func (r *reconciler) clock() time.Time {
	if r.now != nil {
		return r.now().UTC()
	}
	return time.Now().UTC()
}

// repairAttempt gates a repair action behind the durable per-outage budget:
// it sweeps dangling thunderd enable-symlinks (B3) and then runs action, but
// only if the budget allows it this pass. restartOnly marks the cheap,
// side-effect-free repair (restart) that is still tried once per hour once
// the budget is spent and the node is in cool-off; enrolling and reinstalling
// the CLI are not -- a node stuck enough to exhaust the budget needs a human,
// not another token or another install.
//
// It returns whether action actually ran, separately from any error action
// returned, so a caller can tell "the repair failed" from "the budget said
// not to try" -- the two must not be confused when deciding whether to reset
// the unhealthy counters. (by claude)
func (r *reconciler) repairAttempt(ctx context.Context, cfg Config, kind string, restartOnly bool, action func(context.Context) error) (bool, error) {
	store := r.repairBudgetStore()
	marker, _, err := store.Load(ctx)
	if err != nil {
		return false, fmt.Errorf("load repair budget marker: %w", err)
	}
	now := r.clock()

	if marker.CoolingOff {
		if !restartOnly {
			log.Printf("node %s: repair cool-off is active; %s waits for a human, not another attempt", cfg.Node, kind)
			return false, nil
		}
		if !marker.LastAttemptAt.IsZero() && now.Sub(marker.LastAttemptAt) < repairCoolOff {
			return false, nil
		}
		log.Printf("node %s: repair cool-off elapsed; trying one restart", cfg.Node)
	}

	if err := r.sweepDanglingSymlinks(ctx); err != nil {
		log.Printf("node %s: could not sweep dangling thunderd symlinks, repairing anyway: %v", cfg.Node, err)
	}

	reachedLimit := !marker.CoolingOff && marker.Attempts+1 >= repairAttemptLimit
	marker.Attempts++
	marker.LastAttemptAt = now
	if reachedLimit {
		marker.CoolingOff = true
	}
	if err := store.Save(ctx, marker); err != nil {
		return false, fmt.Errorf("save repair budget marker: %w", err)
	}

	actionErr := action(ctx)
	if reachedLimit {
		log.Printf("node %s: giving up on repairing thunderd on this node; will retry once per hour; needs a human", cfg.Node)
	}
	return true, actionErr
}

// resetRepairBudget clears the durable budget once the node is healthy again,
// so the next outage gets the full attempt count rather than inheriting
// whatever was left over from this one. (by claude)
func (r *reconciler) resetRepairBudget(ctx context.Context, cfg Config) {
	store := r.repairBudgetStore()
	marker, _, err := store.Load(ctx)
	if err != nil {
		log.Printf("node %s: could not read repair budget marker to reset it: %v", cfg.Node, err)
		return
	}
	if marker.isZero() {
		return
	}
	if err := store.Save(ctx, repairBudgetMarker{}); err != nil {
		log.Printf("node %s: could not reset repair budget marker: %v", cfg.Node, err)
	}
}
