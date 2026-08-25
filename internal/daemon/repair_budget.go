package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
)

// repairAttemptLimit is how many repair attempts the daemon makes before
// giving up entirely for this outage. Kept in memory: a pod restart resets
// it, which is an acceptable, simpler tradeoff than persisting it to the
// host. (by claude)
const repairAttemptLimit = 5

// checksumsReadLimit bounds the sha256sums.txt fetch; the real file is a
// few hundred bytes. (by claude)
const checksumsReadLimit = int64(1 << 16)

// repairThunderdChecksum is the func used to fetch the sha256 the
// distribution service currently serves for thunderd. A var so tests can
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
// allows it.
//
// Two things reset a spent budget: a different thunderd checksum served by
// distribution (checked only once already spent, so a healthy node never
// polls for it) -- unless that checksum already failed a retry round.
// updateAction, when non-nil, is what a checksum-triggered reset runs
// instead of action, since `thunder up` never replaces an existing binary.
// When a round that started that way spends the budget again, rollbackAction
// (if non-nil) is tried once and the checksum is recorded as failed, so it
// never resets the budget again.
//
// Returns whether action or updateAction ran, separately from any error.
// (by claude)
func (r *reconciler) repairAttempt(ctx context.Context, cfg Config, action, updateAction, rollbackAction func(context.Context) error) (bool, error) {
	run := action

	if r.repairGivenUp {
		if updateAction != nil {
			if checksum, err := repairThunderdChecksum(ctx, cfg.ArtifactBaseURL); err == nil {
				haveBaseline := r.repairStampedChecksum != "" || r.repairFailedChecksum != ""
				isNew := checksum != r.repairStampedChecksum && checksum != r.repairFailedChecksum
				switch {
				case isNew && haveBaseline:
					log.Printf("node %s: distribution serves a new thunderd, retrying repairs", cfg.Node)
					r.repairAttempts = 0
					r.repairGivenUp = false
					r.repairStampedChecksum = checksum
					r.repairAttemptedChecksum = checksum
					r.repairUpdateRetried = true
					run = updateAction
				case isNew:
					// First observation this outage: nothing to compare
					// against yet, so only record a baseline. (by claude)
					r.repairStampedChecksum = checksum
				}
			}
		}
		if r.repairGivenUp {
			return false, nil
		}
	}

	if err := r.sweepDanglingSymlinks(ctx, cfg); err != nil {
		log.Printf("node %s: could not sweep dangling thunderd symlinks, repairing anyway: %v", cfg.Node, err)
	}

	r.repairAttempts++
	actionErr := run(ctx)
	if r.repairAttempts >= repairAttemptLimit {
		r.repairGivenUp = true
		if r.repairUpdateRetried {
			if rollbackAction != nil {
				if err := rollbackAction(ctx); err != nil {
					log.Printf("node %s: thunder update --rollback unavailable: %v", cfg.Node, err)
				}
			}
			r.repairFailedChecksum = r.repairAttemptedChecksum
		}
		log.Printf("node %s: giving up on repairing thunderd on this node after %d failed attempts; needs a human",
			cfg.Node, repairAttemptLimit)
	}
	return true, actionErr
}

// resetRepairBudget clears the budget once the node is healthy again, so
// the next outage gets the full attempt count. (by claude)
func (r *reconciler) resetRepairBudget() {
	r.repairAttempts = 0
	r.repairGivenUp = false
	r.repairStampedChecksum = ""
	r.repairAttemptedChecksum = ""
	r.repairFailedChecksum = ""
	r.repairUpdateRetried = false
}
