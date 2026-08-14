package daemon

import (
	"context"
	"log"
	"strings"
	"time"
)

const (
	// thunderdLogRetryInterval is how long the daemon waits before following
	// thunderd's journal again after the stream ended.
	thunderdLogRetryInterval = 10 * time.Second

	// thunderdLogMaxRetryInterval caps that wait. A node without journalctl
	// keeps retrying cheaply rather than filling its pod log with attempts.
	thunderdLogMaxRetryInterval = 5 * time.Minute

	// thunderdLogStableStream is how long a stream has to last to count as
	// having worked. One that did is not a failure to back off from, however
	// it ended.
	thunderdLogStableStream = time.Minute
)

// thunderdLogsDisabled are the values of ThunderdLogUnit that turn the stream
// off, for an operator who would rather read thunderd's logs on the node.
var thunderdLogsDisabled = map[string]struct{}{
	"off":   {},
	"none":  {},
	"false": {},
	"0":     {},
}

// followThunderdLogs republishes thunderd's journal as this pod's log, so
// `kubectl logs` on a daemon pod shows what thunderd is doing on the node
// rather than only what the daemon concluded about it. The journal is what you
// would read over SSH; the pod is where whoever runs the cluster is already
// looking.
//
// It follows rather than polls, and it keeps following across restarts and
// reinstalls of thunderd: the journal outlives the unit, including the
// transient one the daemon installs. It returns only when ctx is cancelled.
func followThunderdLogs(ctx context.Context, runner commandRunner, unit string) {
	unit = strings.TrimSpace(unit)
	if _, off := thunderdLogsDisabled[strings.ToLower(unit)]; unit == "" || off {
		log.Printf("not following thunderd logs on this node")
		return
	}
	log.Printf("following thunderd logs: unit=%s", unit)

	var delay time.Duration
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		// --lines 0 starts at the end of the journal, so the pod log carries
		// what thunderd does from here rather than replaying its history on
		// every reconnect. --output cat drops the syslog framing, which is
		// already in the daemon's own log line.
		started := time.Now()
		err := runner.Stream(ctx, logThunderdLine,
			"journalctl", "--unit", unit, "--follow", "--lines", "0", "--output", "cat", "--no-pager")
		if ctx.Err() != nil {
			return
		}

		if time.Since(started) >= thunderdLogStableStream {
			failures = 0
		}
		failures++
		delay = reconcileBackoff(thunderdLogRetryInterval, thunderdLogMaxRetryInterval, failures)
		if err != nil {
			log.Printf("thunderd log stream ended (retrying in %s): %v", delay, err)
			continue
		}
		log.Printf("thunderd log stream ended (retrying in %s)", delay)
	}
}

// logThunderdLine republishes one journal line under a prefix that says where
// it came from, so a pod log distinguishes what thunderd reported from what the
// daemon decided.
func logThunderdLine(line string) {
	log.Printf("thunderd: %s", line)
}
