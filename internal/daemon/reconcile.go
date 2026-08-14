package daemon

import (
	"context"
	"fmt"
	"log"
	"strings"
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
	// takes to re-enroll a node that was previously healthy. A node that has
	// never been healthy skips the wait, so a fresh node still enrolls on the
	// first pass.
	unhealthyReconcileThreshold = 3

	// restartRepairLimit is how many restarts the daemon will try on an
	// enrolled node before reinstalling it instead. Restarting is the cheap
	// repair, not the only one: a restart that fails, or that runs but leaves
	// thunderd unhealthy anyway, must not become a loop that never reaches the
	// repair that would have worked.
	restartRepairLimit = 2

	// transitionalReconcileThreshold bounds how long thunderd may sit in a
	// systemd transition before it counts as broken. Restarting and stopping
	// are states thunderd passes through on its own — including immediately
	// after the daemon enrolls it — and reinstalling on top of one restarts a
	// service that was already coming back, which can loop.
	transitionalReconcileThreshold = 30
)

// transitionalServiceStates are systemd states thunderd is moving through
// rather than stuck in.
var transitionalServiceStates = map[string]struct{}{
	"activating":   {},
	"deactivating": {},
	"reloading":    {},
}

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
	resolved       Config
	zoneID         string
	pluginStarted  bool
	everHealthy    bool
	unhealthy      int
	transitional   int
	passes         int
	libthunderPath string

	// loggedStatus is the last thunderd status that was logged. Statuses are
	// logged when they change rather than every pass.
	loggedStatus statusKey

	// restarts counts the restarts attempted since thunderd was last healthy.
	// See restartRepairLimit.
	restarts int
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
	// Warm the library cache from here, where the daemon has ordinary node
	// networking and a failure only costs a retry. The hook can download it
	// too, but doing so blocks a container start on a ~32MB fetch.
	r.ensureLibthunder(ctx, cfg)
	return r.ensurePlugin(ctx, cfg)
}

// ensureEnrolled brings thunderd back to healthy on the host when it is not.
// An unreadable status counts as unhealthy: `thunder status` exiting 127 is
// what an uninstalled thunderd looks like, and that is precisely the case the
// daemon has to recover from.
func (r *reconciler) ensureEnrolled(ctx context.Context, cfg Config) error {
	status, statusErr := getThunderStatus(ctx, r.runner)
	r.logStatus(cfg, status, statusErr)

	if statusErr == nil && status.nodeHealthy() {
		if r.unhealthy > 0 || r.transitional > 0 {
			log.Printf("thunderd recovered on node %s after %d unhealthy and %d transitional check(s)",
				cfg.Node, r.unhealthy, r.transitional)
		}
		r.unhealthy = 0
		r.transitional = 0
		r.restarts = 0
		r.everHealthy = true
		return nil
	}

	// A service that is starting or stopping is not a service that needs
	// reinstalling. Wait for it to settle, but not forever: a transition that
	// never ends is as broken as a service that failed.
	if statusErr == nil && isTransitional(status) && r.transitional < transitionalReconcileThreshold {
		r.transitional++
		log.Printf("thunderd is %s on node %s (%s); waiting for it to settle (%d/%d)",
			status.Service.Active, cfg.Node, status.Service.SubState, r.transitional, transitionalReconcileThreshold)
		return nil
	}

	r.unhealthy++
	// A node that has been healthy before is given a grace window, so a
	// service restart is not mistaken for a node that needs repairing.
	threshold := 1
	if r.everHealthy {
		threshold = unhealthyReconcileThreshold
	}
	if r.unhealthy < threshold {
		log.Printf("thunderd unhealthy on node %s (%d/%d checks); waiting before repairing it",
			cfg.Node, r.unhealthy, threshold)
		return nil
	}

	// A node whose CLI answered and whose auth token is already on disk needs
	// thunderd started, not installed: reinstalling downloads the CLI again
	// and spends a fresh single-use enrollment token on a node Thunder has
	// already enrolled.
	if statusErr == nil && status.enrolled() && r.restarts < restartRepairLimit {
		r.restarts++
		log.Printf("thunderd is installed and enrolled on node %s but not healthy after %d check(s); restarting it (attempt %d/%d)",
			cfg.Node, r.unhealthy, r.restarts, restartRepairLimit)
		if err := r.restart(ctx, cfg); err != nil {
			log.Printf("could not restart thunderd on node %s, reinstalling it instead: %v", cfg.Node, err)
		} else {
			// Health is confirmed by the next pass rather than assumed here.
			// A restart that ran but did not fix the node is why the attempts
			// are counted rather than only the failures.
			r.unhealthy = 0
			r.transitional = 0
			return nil
		}
	}

	log.Printf("thunderd unhealthy on node %s after %d check(s); enrolling", cfg.Node, r.unhealthy)
	if err := r.enroll(ctx, cfg); err != nil {
		return err
	}
	// A reinstall re-establishes what the cheap repair could not, so restarts
	// are worth trying again on the next outage.
	r.restarts = 0
	r.unhealthy = 0
	r.transitional = 0
	return nil
}

// logStatus logs a thunderd status when it differs from the one logged last,
// so a steady node reports its state once instead of every pass. What changed
// is decided on the status fields, not on the line they produce.
func (r *reconciler) logStatus(cfg Config, status thunderStatus, statusErr error) {
	key := status.key()
	if statusErr != nil {
		key = unreadableStatusKey(statusErr)
	}
	if key == r.loggedStatus {
		return
	}
	r.loggedStatus = key

	if statusErr != nil {
		log.Printf("thunderd status unavailable on node %s: %v", cfg.Node, statusErr)
		return
	}
	logThunderStatus(status)
}

// restart brings thunderd back up with the credentials the node already has.
// It is the repair for a node that is enrolled but not running: no download,
// and no enrollment token, because the auth token thunderd saved when it
// enrolled is still the one Thunder issued this node.
func (r *reconciler) restart(ctx context.Context, cfg Config) error {
	command := strings.Join([]string{
		thunderdTransientEnv, "thunder", "up",
		"--ip", shellQuote(cfg.AdvertisedIP),
		"--zone", shellQuote(cfg.Zone),
		"--node-name", shellQuote(cfg.Node),
	}, " ")
	if err := r.runner.RunShell(ctx, "thunder up", command); err != nil {
		return fmt.Errorf("run thunder up: %w", err)
	}
	log.Printf("thunderd restarted on node %s", cfg.Node)
	return nil
}

// isTransitional reports whether thunderd is mid-transition. This does read a
// systemd state string, but it never calls a node healthy: it only buys a
// service that is visibly on its way up or down some time before the daemon
// repairs it. A state string this does not recognise costs that wait and
// nothing else — the node is repaired sooner, not left broken.
func isTransitional(status thunderStatus) bool {
	_, ok := transitionalServiceStates[strings.TrimSpace(status.Service.Active)]
	return ok
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

	command, err := withTransientThunderd(r.client.ServerEnrollmentCommand(thunder.ServerEnrollmentCommandRequest{
		EnrollmentToken: token.EnrollmentToken,
		IP:              cfg.AdvertisedIP,
		Zone:            cfg.Zone,
		ServerName:      cfg.Node,
	}))
	if err != nil {
		return err
	}
	if err := r.runner.RunShell(ctx, "thunder installer", command); err != nil {
		return fmt.Errorf("run thunder node setup: %w", err)
	}

	log.Printf("thunder node setup completed: node=%s enrollmentTokenId=%s", cfg.Node, token.EnrollmentTokenID)
	return nil
}

// thunderdTransientEnv makes the installer bring thunderd up as a systemd-run
// transient unit rather than an installed unit file, which is what a node whose
// /etc belongs to its image needs: nothing thunderd writes there survives a
// reboot to be reconciled against.
//
// It is an environment variable rather than a flag because the installer ends
// in `thunder up` and does not forward --transient to it. `thunder up` reads
// the setting from its environment, and then persists it, so a node only has to
// be told once.
const thunderdTransientEnv = "THUNDERD_TRANSIENT=1"

// withTransientThunderd adds that setting to the environment the SDK builds for
// the installer.
//
// A command it does not recognise is an error rather than a command passed
// through untouched: silently installing a unit file on a node that must not
// have one is the failure this exists to prevent, and nothing downstream would
// report it. The command carries a single-use enrollment token, so it is not
// quoted back in the error.
func withTransientThunderd(command string) (string, error) {
	const sudo = "| sudo "
	before, after, found := strings.Cut(command, sudo)
	if !found {
		return "", fmt.Errorf("thunder node setup command is not in the expected form, so %s could not be added to it", thunderdTransientEnv)
	}
	return before + sudo + thunderdTransientEnv + " " + after, nil
}

// ensureLibthunder pre-downloads the client library the CDI hook stages into
// containers. Failures are logged, not returned: a node that cannot reach the
// artifact host can still serve claims whose library is already cached, and the
// hook reports the problem against the specific container if it is not.
func (r *reconciler) ensureLibthunder(ctx context.Context, cfg Config) {
	if !cfg.DRAEnabled {
		return
	}
	cache := &LibthunderCache{
		Dir:             cfg.KubeletPluginDir,
		InstallURL:      cfg.ThunderInstallURL,
		URL:             cfg.LibthunderURL,
		SHA256:          cfg.LibthunderSHA256,
		ArtifactBaseURL: cfg.ArtifactBaseURL,
	}
	path, err := cache.Ensure(ctx)
	if err != nil {
		log.Printf("could not pre-cache libthunder.so on node %s (the CDI hook will retry per container): %v", cfg.Node, err)
		return
	}
	if path != r.libthunderPath {
		log.Printf("libthunder.so cached: path=%s", path)
		r.libthunderPath = path
	}
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
