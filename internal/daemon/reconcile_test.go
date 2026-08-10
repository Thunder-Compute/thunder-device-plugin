package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	thunder "github.com/Thunder-Compute/thunder-sdk"
)

// scriptedRunner answers `thunder status --json` from a script, one entry per
// call with the last entry repeating, so a test can describe a node whose
// thunderd changes state underneath the daemon.
type scriptedRunner struct {
	statuses  []scriptedStatus
	statusHit int
	nvidia    map[string][]byte
	shell     []string
	shellErr  error
}

type scriptedStatus struct {
	output string
	err    error
}

func (r *scriptedRunner) CombinedOutput(_ context.Context, name string, args ...string) ([]byte, error) {
	key := commandKey(name, args...)
	if key == "thunder status --json" {
		status := r.statuses[min(r.statusHit, len(r.statuses)-1)]
		r.statusHit++
		return []byte(status.output), status.err
	}
	return r.nvidia[key], nil
}

func (r *scriptedRunner) RunShell(_ context.Context, command string) error {
	if r.shellErr != nil {
		return r.shellErr
	}
	r.shell = append(r.shell, command)
	return nil
}

func (r *scriptedRunner) enrollments() int {
	count := 0
	for _, command := range r.shell {
		if strings.Contains(command, "THUNDER_INSTALL_MODE=thunderd") {
			count++
		}
	}
	return count
}

// newTestReconciler wires a reconciler against a recording Thunder API and a
// host that passes the NVIDIA checks.
func newTestReconciler(t *testing.T, runner *scriptedRunner) (*reconciler, *recordingRegistry) {
	t.Helper()

	registry := newRecordingRegistry(t)
	hostRoot := t.TempDir()
	for _, name := range []string{"libcuda.so.1", "libnvidia-ml.so.1", "nvidia-smi"} {
		touch(t, hostRoot+"/"+name)
	}
	if runner.nvidia == nil {
		runner.nvidia = map[string][]byte{
			"/nvidia-smi --query-gpu=driver_version --format=csv,noheader,nounits": []byte("610.43.02\n"),
			"/nvidia-smi --query-gpu=index --format=csv,noheader,nounits":          []byte("0\n1\n"),
		}
	}

	cfg := Config{
		Node:             "node-a",
		ThunderAPIURL:    registry.server.URL,
		ThunderAPIToken:  "token",
		HostRoot:         hostRoot,
		LibCUDAPath:      "/libcuda.so.1",
		LibNVMLPath:      "/libnvidia-ml.so.1",
		NVSMIPath:        "/nvidia-smi",
		MinDriverVersion: "610",
		ZoneLabel:        DefaultZoneLabel,
	}
	nodes := &fakeNodeInfoReader{node: NodeInfo{
		Labels:     map[string]string{DefaultZoneLabel: "us-west-2a"},
		InternalIP: "10.0.0.5",
	}}

	return &reconciler{
		cfg:    cfg,
		runner: runner,
		nodes:  nodes,
		client: thunder.NewClient(cfg.ThunderAPIURL, cfg.ThunderAPIToken),
		startPlugin: func(context.Context, Config, *thunder.Client, string) error {
			return nil
		},
	}, registry
}

const healthyStatus = `{"healthy":true,"service":{"active":"active"}}`

// A node whose thunderd was uninstalled reports exit 127 from `thunder status`.
// That is the state the staging node was left in, and the daemon has to climb
// out of it without a pod restart.
func TestReconcileReenrollsAfterThunderIsUninstalled(t *testing.T) {
	runner := &scriptedRunner{statuses: []scriptedStatus{
		{output: healthyStatus},
		{output: "", err: errors.New("exit status 127: nsenter: failed to execute thunder: No such file or directory")},
	}}
	reconciler, _ := newTestReconciler(t, runner)
	ctx := context.Background()

	// First pass finds a healthy node and enrolls nothing.
	if err := reconciler.reconcile(ctx); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if got := runner.enrollments(); got != 0 {
		t.Fatalf("enrollments after a healthy pass = %d, want 0", got)
	}

	// thunderd then disappears. The node was healthy before, so the daemon
	// waits out the grace window before reinstalling.
	for pass := 1; pass < unhealthyReconcileThreshold; pass++ {
		if err := reconciler.reconcile(ctx); err != nil {
			t.Fatalf("reconcile during grace window: %v", err)
		}
		if got := runner.enrollments(); got != 0 {
			t.Fatalf("enrollments during grace window = %d, want 0", got)
		}
	}

	if err := reconciler.reconcile(ctx); err != nil {
		t.Fatalf("reconcile after grace window: %v", err)
	}
	if got := runner.enrollments(); got != 1 {
		t.Fatalf("enrollments after grace window = %d, want 1", got)
	}
}

// A node that has never been healthy is enrolled on the first pass, so a fresh
// node does not sit idle through the grace window.
func TestReconcileEnrollsANeverHealthyNodeImmediately(t *testing.T) {
	runner := &scriptedRunner{statuses: []scriptedStatus{{output: `{"healthy":false}`}}}
	reconciler, _ := newTestReconciler(t, runner)

	if err := reconciler.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := runner.enrollments(); got != 1 {
		t.Fatalf("enrollments = %d, want 1", got)
	}
}

// thunderd reports unhealthy while it restarts. Reinstalling underneath a
// restart would fight whoever is doing maintenance, so a blip is ridden out.
func TestReconcileRidesOutATransientRestart(t *testing.T) {
	runner := &scriptedRunner{statuses: []scriptedStatus{
		{output: healthyStatus},
		{output: `{"healthy":false,"service":{"active":"deactivating","subState":"stop-sigterm"}}`},
		{output: `{"healthy":false,"service":{"active":"activating"}}`},
		{output: healthyStatus},
	}}
	reconciler, _ := newTestReconciler(t, runner)
	ctx := context.Background()

	for pass := 0; pass < 4; pass++ {
		if err := reconciler.reconcile(ctx); err != nil {
			t.Fatalf("reconcile pass %d: %v", pass, err)
		}
	}
	if got := runner.enrollments(); got != 0 {
		t.Fatalf("enrollments across a restart = %d, want 0", got)
	}
	// The counter is cleared, so the next outage gets the full window again.
	if reconciler.unhealthy != 0 {
		t.Fatalf("unhealthy = %d after recovery, want 0", reconciler.unhealthy)
	}
}

// A failed enrollment must not leave the node stuck: the next pass tries again
// with a freshly minted token, because enrollment tokens are single use.
func TestReconcileRetriesAFailedEnrollmentWithAFreshToken(t *testing.T) {
	runner := &scriptedRunner{
		statuses: []scriptedStatus{{output: `{"healthy":false}`}},
		shellErr: errors.New("installer exited 1"),
	}
	reconciler, registry := newTestReconciler(t, runner)
	ctx := context.Background()

	if err := reconciler.reconcile(ctx); err == nil {
		t.Fatal("reconcile succeeded, want the installer failure surfaced")
	}

	runner.shellErr = nil
	if err := reconciler.reconcile(ctx); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if got := runner.enrollments(); got != 1 {
		t.Fatalf("enrollments = %d, want 1", got)
	}

	tokens := 0
	for _, write := range registry.writes() {
		if write.Path == "/api/v1/enrollment-tokens" {
			tokens++
		}
	}
	if tokens != 2 {
		t.Fatalf("enrollment tokens minted = %d, want 2 (one per attempt)", tokens)
	}
}

// Losing the Thunder API must not take the DRA plugin down or crash the pod;
// the pass fails and the loop retries.
func TestReconcileSurfacesZoneFailuresWithoutPanicking(t *testing.T) {
	runner := &scriptedRunner{statuses: []scriptedStatus{{output: healthyStatus}}}
	reconciler, registry := newTestReconciler(t, runner)
	registry.server.Close()

	if err := reconciler.reconcile(context.Background()); err == nil {
		t.Fatal("reconcile succeeded against a dead registry, want an error")
	}
}

// The plugin is started once and not restarted on every pass.
func TestReconcileStartsTheDRAPluginOnce(t *testing.T) {
	runner := &scriptedRunner{statuses: []scriptedStatus{{output: healthyStatus}}}
	reconciler, _ := newTestReconciler(t, runner)

	starts := 0
	reconciler.startPlugin = func(context.Context, Config, *thunder.Client, string) error {
		starts++
		return nil
	}

	for pass := 0; pass < 3; pass++ {
		if err := reconciler.reconcile(context.Background()); err != nil {
			t.Fatalf("reconcile pass %d: %v", pass, err)
		}
	}
	if starts != 1 {
		t.Fatalf("plugin starts = %d, want 1", starts)
	}
}

// A plugin that fails to start is retried, rather than leaving a node that is
// enrolled with Thunder but serves no claims.
func TestReconcileRetriesAFailedPluginStart(t *testing.T) {
	runner := &scriptedRunner{statuses: []scriptedStatus{{output: healthyStatus}}}
	reconciler, _ := newTestReconciler(t, runner)

	starts := 0
	reconciler.startPlugin = func(context.Context, Config, *thunder.Client, string) error {
		starts++
		if starts == 1 {
			return errors.New("kubelet socket not ready")
		}
		return nil
	}

	if err := reconciler.reconcile(context.Background()); err == nil {
		t.Fatal("first reconcile succeeded, want the plugin failure surfaced")
	}
	if err := reconciler.reconcile(context.Background()); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if starts != 2 {
		t.Fatalf("plugin starts = %d, want 2", starts)
	}
	if !reconciler.pluginStarted {
		t.Fatal("pluginStarted = false after a successful start")
	}
}

// A zone label added after the pod started must be picked up without a restart.
func TestReconcilePicksUpALaterZoneLabel(t *testing.T) {
	runner := &scriptedRunner{statuses: []scriptedStatus{{output: healthyStatus}}}
	reconciler, _ := newTestReconciler(t, runner)
	nodes := reconciler.nodes.(*fakeNodeInfoReader)
	nodes.node.Labels = map[string]string{}

	if err := reconciler.reconcile(context.Background()); err == nil {
		t.Fatal("reconcile succeeded without a zone label, want an error")
	}

	nodes.node.Labels = map[string]string{DefaultZoneLabel: "us-west-2a"}
	if err := reconciler.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after the label was added: %v", err)
	}
	if reconciler.resolved.Zone != "us-west-2a" {
		t.Fatalf("zone = %q, want us-west-2a", reconciler.resolved.Zone)
	}
}

func TestReconcileBackoffDoublesAndCaps(t *testing.T) {
	base := 10 * time.Second
	max := 5 * time.Minute

	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{6, 5 * time.Minute},
		{100, 5 * time.Minute},
	} {
		if got := reconcileBackoff(base, max, tc.failures); got != tc.want {
			t.Errorf("reconcileBackoff(failures=%d) = %s, want %s", tc.failures, got, tc.want)
		}
	}
}

// The loop keeps running past a failing pass and returns only when cancelled.
func TestReconcileLoopSurvivesFailuresAndStopsOnCancel(t *testing.T) {
	runner := &scriptedRunner{statuses: []scriptedStatus{{output: healthyStatus}}}
	reconciler, registry := newTestReconciler(t, runner)
	registry.server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconciler.loop(ctx, time.Millisecond, 2*time.Millisecond) }()

	// Give the loop enough time to fail several passes without returning.
	select {
	case err := <-done:
		t.Fatalf("loop returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("loop returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not return after cancel")
	}
}
