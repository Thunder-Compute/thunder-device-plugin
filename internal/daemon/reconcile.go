package daemon

import (
	"context"
	"fmt"
	"log"
	"time"

	thunder "github.com/Thunder-Compute/thunder-sdk"
)

const (
	// ThunderReconcileInterval is how often the daemon re-checks that the node
	// is in the state it should be in.
	ThunderReconcileInterval = 10 * time.Second

	// ThunderReconcileMaxBackoff caps the wait between passes after repeated
	// failures. A node that cannot enroll keeps retrying, but without hammering
	// the Thunder API or re-running the installer in a tight loop.
	ThunderReconcileMaxBackoff = 5 * time.Minute

	// unhealthyReconcileThreshold is how many consecutive unhealthy checks it
	// takes to re-enroll a node that was previously healthy. thunderd reports
	// unhealthy while it restarts, and reinstalling underneath a restart would
	// fight whoever is doing maintenance. A node that has never been healthy
	// skips the wait, so a fresh node still enrolls on the first pass.
	unhealthyReconcileThreshold = 3
)

// reconciler drives one node towards the state the chart declares: enrolled
// with Thunder in the zone its Kubernetes labels name, and serving the DRA
// kubelet plugin. Every step is idempotent and safe to repeat, so a pass that
// fails halfway is recovered by the next one rather than by a pod restart.
type reconciler struct {
	cfg    Config
	runner commandRunner
	nodes  nodeInfoReader
	client *thunder.Client

	// startPlugin is a field so tests can exercise the loop without an
	// in-cluster kubelet.
	startPlugin func(context.Context, Config, *thunder.Client, string) error

	// State carried between passes.
	resolved      Config
	zoneID        string
	pluginStarted bool
	everHealthy   bool
	unhealthy     int
	passes        int
}

// loop reconciles until the context is cancelled. It returns only on
// cancellation: every other failure is logged and retried with backoff, because
// the conditions the daemon depends on (a reachable Thunder API, a zone label,
// a working NVIDIA driver) can all arrive after the pod has started.
func (r *reconciler) loop(ctx context.Context, interval, maxBackoff time.Duration) error {
	log.Printf("starting thunder reconcile loop: interval=%s maxBackoff=%s", interval, maxBackoff)

	var delay time.Duration
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}

		if err := r.reconcile(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failures++
			delay = reconcileBackoff(interval, maxBackoff, failures)
			log.Printf("reconcile failed on node %s (consecutive failures=%d, retrying in %s): %v",
				r.cfg.Node, failures, delay, err)
			continue
		}
		if failures > 0 {
			log.Printf("reconcile recovered on node %s after %d failed attempts", r.cfg.Node, failures)
		}
		failures = 0
		delay = interval
	}
}

// reconcile runs one pass. Each step reads the world before acting, so calling
// it against an already-correct node does nothing but log.
func (r *reconciler) reconcile(ctx context.Context) error {
	cfg, err := resolveNodeAttributes(ctx, r.cfg, r.nodes)
	if err != nil {
		return err
	}
	// Labels are re-read every pass, so adding a zone or advertised-ip label to
	// a node fixes it without a pod restart. A new zone invalidates the Thunder
	// zone resolved for the old one.
	if r.passes == 0 || r.resolved.Zone != cfg.Zone || r.resolved.AdvertisedIP != cfg.AdvertisedIP {
		log.Printf("node attributes: node=%s zone=%s advertising=%s", cfg.Node, cfg.Zone, cfg.AdvertisedIP)
		if r.resolved.Zone != cfg.Zone {
			r.zoneID = ""
		}
	}
	r.resolved = cfg
	r.passes++

	if r.zoneID == "" {
		zoneID, err := ensureThunderZone(ctx, r.client, cfg.Zone)
		if err != nil {
			return err
		}
		r.zoneID = zoneID
		log.Printf("resolved thunder zone: kubernetesZone=%s thunderZoneId=%s", cfg.Zone, zoneID)
	}

	if err := r.ensureEnrolled(ctx, cfg); err != nil {
		return err
	}
	return r.ensurePlugin(ctx, cfg)
}

// ensureEnrolled re-runs enrollment when thunderd is not healthy on the host.
// An unreadable status counts as unhealthy: `thunder status` exiting 127 is
// what an uninstalled thunderd looks like, and that is precisely the case the
// daemon has to recover from.
func (r *reconciler) ensureEnrolled(ctx context.Context, cfg Config) error {
	status, statusErr := getThunderStatus(ctx, r.runner)
	healthy := statusErr == nil && status.Healthy
	if statusErr != nil {
		log.Printf("thunder status unavailable on node %s: %v", cfg.Node, statusErr)
	} else {
		prefix := "periodic"
		if !r.everHealthy && r.unhealthy == 0 {
			prefix = "initial"
		}
		logThunderStatus(prefix, status)
	}

	if healthy {
		if r.unhealthy > 0 {
			log.Printf("thunder recovered on node %s after %d unhealthy check(s)", cfg.Node, r.unhealthy)
		}
		r.unhealthy = 0
		r.everHealthy = true
		return nil
	}

	r.unhealthy++
	// A node that has been healthy before is given a grace window, so a
	// service restart is not mistaken for a node that needs reinstalling.
	threshold := 1
	if r.everHealthy {
		threshold = unhealthyReconcileThreshold
	}
	if r.unhealthy < threshold {
		log.Printf("thunder unhealthy on node %s (%d/%d checks); waiting before re-enrolling",
			cfg.Node, r.unhealthy, threshold)
		return nil
	}

	log.Printf("thunder unhealthy on node %s after %d check(s); enrolling", cfg.Node, r.unhealthy)
	if err := r.enroll(ctx, cfg); err != nil {
		return err
	}
	// Health is confirmed by the next pass rather than assumed here.
	r.unhealthy = 0
	return nil
}

// enroll runs the checks and the Thunder installer that put this node into its
// zone. The enrollment token is minted per attempt: it is single-use, so a
// retry needs a fresh one.
func (r *reconciler) enroll(ctx context.Context, cfg Config) error {
	gpuCount, driverVersion, err := nvidiaChecks(ctx, cfg, r.runner)
	if err != nil {
		return err
	}
	log.Printf("nvidia checks passed: driver=%s physical_gpus=%d", driverVersion, gpuCount)

	token, err := r.client.CreateServerEnrollment(ctx, thunder.CreateServerEnrollmentRequest{
		ZoneID: r.zoneID,
	})
	if err != nil {
		return fmt.Errorf("create thunder node enrollment: %w", err)
	}

	command := r.client.ServerEnrollmentCommand(thunder.ServerEnrollmentCommandRequest{
		EnrollmentToken: token.EnrollmentToken,
		IP:              cfg.AdvertisedIP,
		Zone:            cfg.Zone,
		ServerName:      cfg.Node,
	})
	if err := r.runner.RunShell(ctx, command); err != nil {
		return fmt.Errorf("run thunder node setup: %w", err)
	}

	log.Printf("thunder node setup completed: node=%s enrollmentTokenId=%s", cfg.Node, token.EnrollmentTokenID)
	return nil
}

// ensurePlugin starts the DRA kubelet plugin once. It is never stopped when
// thunderd goes unhealthy: the kubelet drives it for claims that are already
// prepared, and tearing it down would disturb workloads that are still running.
func (r *reconciler) ensurePlugin(ctx context.Context, cfg Config) error {
	if r.pluginStarted {
		return nil
	}
	if err := r.startPlugin(ctx, cfg, r.client, r.zoneID); err != nil {
		return err
	}
	r.pluginStarted = true
	return nil
}

// reconcileBackoff doubles the interval per consecutive failure up to max.
func reconcileBackoff(base, max time.Duration, failures int) time.Duration {
	delay := base
	for i := 1; i < failures; i++ {
		if delay >= max {
			break
		}
		delay *= 2
	}
	if delay > max {
		return max
	}
	return delay
}
