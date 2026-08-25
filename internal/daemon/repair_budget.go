package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/version"
)

const (
	repairBudgetMarkerPath = "/var/lib/thunder/repair-budget.json"

	// repairAttemptLimit is how many repair attempts the daemon makes before
	// giving up entirely for this outage. Persisted so a pod restart
	// mid-outage does not re-arm it. (by claude)
	repairAttemptLimit = 5

	repairBudgetMarkerAbsentSentinel = "THUNDER_REPAIR_BUDGET_MARKER_ABSENT"
)

// repairBudgetMarker is the durable state behind the per-outage repair
// budget, written to the host so recreating the reconciler doesn't reset it
// mid-outage. (by claude)
type repairBudgetMarker struct {
	Attempts int `json:"attempts"`
	// DaemonVersion is the daemon build that last wrote this marker. A
	// mismatch means a new image shipped, so the old budget is discarded.
	// (by claude)
	DaemonVersion string `json:"daemonVersion,omitempty"`
}

// isZero reports whether the marker has nothing worth persisting. (by claude)
func (m repairBudgetMarker) isZero() bool {
	return m.Attempts == 0
}

type repairBudgetStore interface {
	Load(context.Context) (repairBudgetMarker, bool, error)
	Save(context.Context, repairBudgetMarker) error
}

// hostRepairBudgetStore persists the marker on the host as a small JSON
// file, written atomically (temp file + rename). Its own file, separate from
// any other marker, so clearing it has no side effects on unrelated state.
// (by claude)
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
		// Unparseable is treated as absent: starting over is safe, refusing
		// to repair over a corrupt marker is not. (by claude)
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

// repairAttempt sweeps dangling symlinks (B3) and runs action if the budget
// allows it, discarding a marker from a different daemon version first.
// Returns whether action ran, separately from its error. (by claude)
func (r *reconciler) repairAttempt(ctx context.Context, cfg Config, action func(context.Context) error) (bool, error) {
	store := r.repairBudgetStore()
	marker, _, err := store.Load(ctx)
	if err != nil {
		return false, fmt.Errorf("load repair budget marker: %w", err)
	}

	current := version.Get()
	if marker.DaemonVersion != "" && marker.DaemonVersion != current {
		log.Printf("node %s: new daemon version, retrying repairs", cfg.Node)
		marker = repairBudgetMarker{}
	}
	marker.DaemonVersion = current

	if marker.Attempts >= repairAttemptLimit {
		return false, nil
	}

	if err := r.sweepDanglingSymlinks(ctx); err != nil {
		log.Printf("node %s: could not sweep dangling thunderd symlinks, repairing anyway: %v", cfg.Node, err)
	}

	marker.Attempts++
	if err := store.Save(ctx, marker); err != nil {
		return false, fmt.Errorf("save repair budget marker: %w", err)
	}

	actionErr := action(ctx)
	if marker.Attempts >= repairAttemptLimit {
		log.Printf("node %s: giving up on repairing thunderd on this node after %d failed attempts; needs a human",
			cfg.Node, repairAttemptLimit)
	}
	return true, actionErr
}

// resetRepairBudget clears the durable budget once the node is healthy
// again, so the next outage gets the full attempt count. (by claude)
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
