package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/version"
)

const (
	repairBudgetMarkerPath = "/var/lib/thunder/repair-budget.json"

	// repairAttemptLimit is how many repair attempts the daemon makes before
	// giving up entirely for this outage. Persisted so a pod restart
	// mid-outage does not re-arm it. (by claude)
	repairAttemptLimit = 5

	// checksumsReadLimit bounds the sha256sums.txt fetch; the real file is a
	// few hundred bytes. (by claude)
	checksumsReadLimit = int64(1 << 16)

	repairBudgetMarkerAbsentSentinel = "THUNDER_REPAIR_BUDGET_MARKER_ABSENT"
)

// repairBudgetMarker is the durable state behind the per-outage repair
// budget, written to the host so recreating the reconciler doesn't reset it
// mid-outage. (by claude)
type repairBudgetMarker struct {
	Attempts int `json:"attempts"`
	// DaemonVersion is the daemon build that last wrote this marker (secondary
	// reset trigger: new repair code justifies a retry too). (by claude)
	DaemonVersion string `json:"daemonVersion,omitempty"`
	// ThunderdChecksum is the thunderd sha256 last seen served, stamped only
	// once the budget is spent (primary reset trigger). (by claude)
	ThunderdChecksum string `json:"thunderdChecksum,omitempty"`
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

// repairThunderdChecksum is the func used to fetch the sha256 the
// distribution service currently serves for thunderd. A field so tests can
// stub it without a real HTTP server. (by claude)
var repairThunderdChecksum = fetchThunderdChecksum

// fetchThunderdChecksum reads <artifactBaseURL>/sha256sums.txt -- anonymous,
// no token needed -- and returns the thunderd line's checksum. (by claude)
func fetchThunderdChecksum(ctx context.Context, artifactBaseURL string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(artifactBaseURL), "/")
	if base == "" {
		return "", errors.New("artifact base URL is not configured")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/sha256sums.txt", nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("fetch sha256sums.txt: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch sha256sums.txt: unexpected status %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, checksumsReadLimit))
	if err != nil {
		return "", fmt.Errorf("read sha256sums.txt: %w", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && filepath.Base(fields[1]) == "thunderd" {
			return fields[0], nil
		}
	}
	return "", errors.New("sha256sums.txt does not list thunderd")
}

// repairAttempt sweeps dangling symlinks (B3) and runs action if the budget
// allows it. Two things reset a spent budget first: a different daemon
// version, or (checked only once the budget is already spent, so a healthy
// node never polls for it) a different thunderd checksum served by
// distribution. Returns whether action ran, separately from its error.
// (by claude)
func (r *reconciler) repairAttempt(ctx context.Context, cfg Config, action func(context.Context) error) (bool, error) {
	store := r.repairBudgetStore()
	marker, _, err := store.Load(ctx)
	if err != nil {
		return false, fmt.Errorf("load repair budget marker: %w", err)
	}
	changed := false

	current := version.Get()
	if marker.DaemonVersion != current {
		if marker.DaemonVersion != "" {
			log.Printf("node %s: new daemon version, retrying repairs", cfg.Node)
			marker = repairBudgetMarker{}
		}
		marker.DaemonVersion = current
		changed = true
	}

	if marker.Attempts >= repairAttemptLimit {
		if checksum, err := repairThunderdChecksum(ctx, cfg.ArtifactBaseURL); err == nil {
			if marker.ThunderdChecksum != checksum {
				if marker.ThunderdChecksum != "" {
					log.Printf("node %s: distribution serves a new thunderd, retrying repairs", cfg.Node)
					marker = repairBudgetMarker{DaemonVersion: current}
				}
				marker.ThunderdChecksum = checksum
				changed = true
			}
		}
		if marker.Attempts >= repairAttemptLimit {
			if changed {
				if err := store.Save(ctx, marker); err != nil {
					log.Printf("node %s: could not save repair budget marker: %v", cfg.Node, err)
				}
			}
			return false, nil
		}
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
