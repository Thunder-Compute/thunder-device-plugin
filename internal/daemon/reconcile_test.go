package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	thunder "github.com/Thunder-Compute/thunder-sdk"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/version"
)

// scriptedRunner answers `thunder status --json` from a script, one entry per
// call with the last entry repeating, so a test can describe a node whose
// thunderd changes state underneath the daemon.
type scriptedRunner struct {
	statuses  []scriptedStatus
	statusHit int
	nvidia    map[string][]byte
	// shell holds the commands that succeeded; attempted holds every command
	// the daemon ran, including the ones shellErr or shellErrs failed.
	shell     []string
	attempted []string
	shellErr  error
	// shellErrs fails only RunShell calls whose command contains the key, so
	// a test can target one specific repair command. (by claude)
	shellErrs map[string]error
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

func (r *scriptedRunner) RunShell(_ context.Context, _ string, command string) error {
	r.attempted = append(r.attempted, command)
	for substr, err := range r.shellErrs {
		if strings.Contains(command, substr) {
			return err
		}
	}
	if r.shellErr != nil {
		return r.shellErr
	}
	r.shell = append(r.shell, command)
	return nil
}

func (r *scriptedRunner) Stream(ctx context.Context, _ func(string), _ string, _ ...string) error {
	<-ctx.Done()
	return ctx.Err()
}

func (r *scriptedRunner) enrollments() int {
	return countCommands(r.shell, "THUNDER_INSTALL_MODE=thunderd")
}

// restartAttempts counts the repairs that reused what the node already had
// rather than downloading the CLI and spending an enrollment token on it,
// including the ones that failed.
func (r *scriptedRunner) restartAttempts() int {
	return countCommands(r.attempted, "thunder up")
}

func countCommands(commands []string, substring string) int {
	count := 0
	for _, command := range commands {
		if strings.Contains(command, substring) {
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

// writeHostEnvFile writes thunderd's env file the way thunderd itself
// would, for hostAuthTokenConfigured to read. (by claude)
func writeHostEnvFile(t *testing.T, hostRoot, token string) {
	t.Helper()
	path := filepath.Join(hostRoot, thunderdEnvPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`THUNDERD_AUTH_TOKEN="`+token+`"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A node whose thunderd was uninstalled reports exit 127 from `thunder status`.
// The daemon has to climb out of that without a pod restart.
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
		statuses:  []scriptedStatus{{output: `{"healthy":false}`}},
		shellErrs: map[string]error{"THUNDER_INSTALL_MODE=thunderd": errors.New("installer exited 1")},
	}
	reconciler, registry := newTestReconciler(t, runner)
	ctx := context.Background()

	if err := reconciler.reconcile(ctx); err == nil {
		t.Fatal("reconcile succeeded, want the installer failure surfaced")
	}

	runner.shellErrs = nil
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

// thunderd passes through stop-sigterm on its way back up, including right
// after the daemon enrolls it. Reinstalling then restarts a service that was
// already returning, which can loop: enroll, restart, enroll again.
func TestReconcileWaitsOutASystemdTransition(t *testing.T) {
	stopping := `{"healthy":false,"service":{"active":"deactivating","subState":"stop-sigterm"}}`
	runner := &scriptedRunner{statuses: []scriptedStatus{
		{output: healthyStatus},
		{output: stopping}, {output: stopping}, {output: stopping},
		{output: stopping}, {output: stopping}, {output: stopping},
		{output: healthyStatus},
	}}
	reconciler, _ := newTestReconciler(t, runner)
	ctx := context.Background()

	for pass := 0; pass < 8; pass++ {
		if err := reconciler.reconcile(ctx); err != nil {
			t.Fatalf("reconcile pass %d: %v", pass, err)
		}
	}
	if got := runner.enrollments(); got != 0 {
		t.Fatalf("enrollments while thunderd was restarting = %d, want 0", got)
	}
	if reconciler.transitional != 0 {
		t.Fatalf("transitional = %d after recovery, want 0", reconciler.transitional)
	}
}

// A transition that never ends is as broken as a failed service, so the wait
// is bounded rather than indefinite.
func TestReconcileEnrollsAfterAStuckTransition(t *testing.T) {
	runner := &scriptedRunner{statuses: []scriptedStatus{
		{output: healthyStatus},
		{output: `{"healthy":false,"service":{"active":"activating","subState":"start-pre"}}`},
	}}
	reconciler, _ := newTestReconciler(t, runner)
	ctx := context.Background()

	// One healthy pass, then the transition holds forever.
	for pass := 0; pass < transitionalReconcileThreshold+unhealthyReconcileThreshold+1; pass++ {
		if err := reconciler.reconcile(ctx); err != nil {
			t.Fatalf("reconcile pass %d: %v", pass, err)
		}
	}
	if got := runner.enrollments(); got != 1 {
		t.Fatalf("enrollments after a stuck transition = %d, want 1", got)
	}
}

// A failed service is not a transition and gets the normal short window.
func TestReconcileReenrollsPromptlyOnAFailedService(t *testing.T) {
	runner := &scriptedRunner{statuses: []scriptedStatus{
		{output: healthyStatus},
		{output: `{"healthy":false,"service":{"active":"failed","subState":"failed"}}`},
	}}
	reconciler, _ := newTestReconciler(t, runner)
	ctx := context.Background()

	for pass := 0; pass < 1+unhealthyReconcileThreshold; pass++ {
		if err := reconciler.reconcile(ctx); err != nil {
			t.Fatalf("reconcile pass %d: %v", pass, err)
		}
	}
	if got := runner.enrollments(); got != 1 {
		t.Fatalf("enrollments after a failed service = %d, want 1", got)
	}
}

// A declared host data-port range must reach the installer, and no declared
// range must leave the command exactly as it was before the setting existed.
// Both enrollments run against one reconciler so the rest of the command (API
// URL, enrollment token) is identical and only the range can differ. (by claude)
func TestEnrollPassesPortRangeToInstaller(t *testing.T) {
	tests := []struct {
		name      string
		portRange string
		wantEnv   string
	}{
		{name: "unset", portRange: ""},
		{name: "declared", portRange: "32000-32199", wantEnv: " THUNDERD_PORT_RANGE='32000-32199'"},
	}

	runner := &scriptedRunner{}
	reconciler, _ := newTestReconciler(t, runner)
	commands := map[string]string{}

	for i, test := range tests {
		cfg := reconciler.cfg
		cfg.Zone = "us-west-2a"
		cfg.AdvertisedIP = "10.0.0.5"
		cfg.PortRange = test.portRange

		if err := reconciler.enroll(context.Background(), cfg); err != nil {
			t.Fatalf("%s: enroll: %v", test.name, err)
		}
		if len(runner.shell) != i+1 {
			t.Fatalf("%s: ran %d installer commands, want %d", test.name, len(runner.shell), i+1)
		}
		command := runner.shell[i]
		commands[test.name] = command

		if test.wantEnv == "" {
			if strings.Contains(command, "THUNDERD_PORT_RANGE") {
				t.Fatalf("%s: command sets THUNDERD_PORT_RANGE:\n%s", test.name, command)
			}
			continue
		}
		if !strings.Contains(command, test.wantEnv) {
			t.Fatalf("%s: command does not carry %q:\n%s", test.name, test.wantEnv, command)
		}
	}

	if stripped := strings.Replace(commands["declared"], " THUNDERD_PORT_RANGE='32000-32199'", "", 1); stripped != commands["unset"] {
		t.Fatalf("an unset range changed the command beyond the range itself:\n%s\nwant\n%s", stripped, commands["unset"])
	}
}

// The installer must be told to run thunderd as a transient unit, and a command
// this cannot add that to must fail rather than quietly install a unit file.
func TestWithTransientThunderd(t *testing.T) {
	command, err := withTransientThunderd("curl -fsSL 'https://get.thundercompute.com/install.sh' | sudo THUNDER_INSTALL_MODE=thunderd sh")
	if err != nil {
		t.Fatalf("withTransientThunderd: %v", err)
	}
	want := "curl -fsSL 'https://get.thundercompute.com/install.sh' | sudo THUNDERD_TRANSIENT=1 THUNDER_INSTALL_MODE=thunderd sh"
	if command != want {
		t.Fatalf("command =\n%s\nwant\n%s", command, want)
	}

	if _, err := withTransientThunderd("curl -fsSL 'https://get.thundercompute.com/install.sh' | sh"); err == nil {
		t.Fatal("withTransientThunderd accepted a command it could not add the setting to")
	}
}

// The status a working node reports when thunderd was installed the way this
// daemon installs it: running and answering, but transient, so systemd has no
// unit file to call enabled and `thunder status` calls the node unhealthy.
const transientHealthyStatus = `{"service":{"service":"thunderd.service","active":"active","enabled":"unknown","load":"loaded","subState":"running"},` +
	`"localApi":{"healthy":true},"config":{"envPath":"/etc/thunder/thunderd.env","authTokenConfigured":true},"healthy":false,` +
	`"warnings":[],"diagnostics":[],"recentLogs":["line"]}`

// A transiently installed thunderd is never `enabled`, so `thunder status`
// reports healthy=false on a node that is working. Believing it reinstalled
// thunderd every ten seconds, downloading the CLI and minting an enrollment
// token each pass.
func TestReconcileLeavesATransientlyInstalledThunderdAlone(t *testing.T) {
	runner := &scriptedRunner{statuses: []scriptedStatus{{output: transientHealthyStatus}}}
	reconciler, registry := newTestReconciler(t, runner)

	for pass := 0; pass < 5; pass++ {
		if err := reconciler.reconcile(context.Background()); err != nil {
			t.Fatalf("reconcile pass %d: %v", pass, err)
		}
	}

	if got := runner.enrollments(); got != 0 {
		t.Fatalf("enrollments on a healthy transient node = %d, want 0", got)
	}
	if got := runner.restartAttempts(); got != 0 {
		t.Fatalf("restarts on a healthy transient node = %d, want 0", got)
	}
	if got := countCommands(paths(registry.writes()), "/api/v1/enrollment-tokens"); got != 0 {
		t.Fatalf("enrollment tokens minted = %d, want 0", got)
	}
	if !reconciler.everHealthy {
		t.Fatal("everHealthy = false, want the node counted as healthy")
	}
}

// A node that is down but still holds its auth token needs thunderd started,
// not installed: reinstalling re-downloads the CLI and spends a fresh
// single-use enrollment token on a node Thunder has already enrolled.
func TestReconcileRestartsAnEnrolledNodeInsteadOfReinstallingIt(t *testing.T) {
	runner := &scriptedRunner{statuses: []scriptedStatus{{
		output: `{"healthy":false,"service":{"service":"thunderd.service","active":"inactive","subState":"dead"},` +
			`"localApi":{"healthy":false,"error":"socket missing"},"config":{"authTokenConfigured":true}}`,
	}}}
	reconciler, registry := newTestReconciler(t, runner)

	if err := reconciler.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := runner.enrollments(); got != 0 {
		t.Fatalf("enrollments for an enrolled node = %d, want 0", got)
	}
	if got := runner.restartAttempts(); got != 1 {
		t.Fatalf("restarts = %d, want 1", got)
	}
	if got := countCommands(paths(registry.writes()), "/api/v1/enrollment-tokens"); got != 0 {
		t.Fatalf("enrollment tokens minted = %d, want 0", got)
	}

	// The dangling-symlink sweep (B3) runs before any repair command.
	sweepAt := commandIndex(runner.attempted, "systemctl daemon-reload")
	restartAt := commandIndex(runner.attempted, "thunder up")
	if sweepAt == -1 || restartAt == -1 || sweepAt > restartAt {
		t.Fatalf("sweep did not run before the restart: attempted = %#v", runner.attempted)
	}

	restart := runner.attempted[restartAt]
	for _, want := range []string{"THUNDERD_TRANSIENT=1", "thunder up", "--ip '10.0.0.5'", "--zone 'us-west-2a'", "--node-name 'node-a'"} {
		if !strings.Contains(restart, want) {
			t.Fatalf("restart command missing %q:\n%s", want, restart)
		}
	}
	// A restart reuses the auth token on the node. Passing an enrollment token
	// would mean one had been minted.
	if strings.Contains(restart, "--token") || strings.Contains(restart, "curl") {
		t.Fatalf("restart command enrolls the node again:\n%s", restart)
	}
}

// commandIndex returns the index of the first command containing substring,
// or -1. (by claude)
func commandIndex(commands []string, substring string) int {
	for i, command := range commands {
		if strings.Contains(command, substring) {
			return i
		}
	}
	return -1
}

// A node whose thunderd is not enrolled at all cannot be restarted into
// health, so it is installed and enrolled.
func TestReconcileEnrollsANodeThatHasNoAuthToken(t *testing.T) {
	runner := &scriptedRunner{statuses: []scriptedStatus{{
		output: `{"healthy":false,"service":{"active":"inactive"},"config":{"authTokenConfigured":false}}`,
	}}}
	reconciler, _ := newTestReconciler(t, runner)

	if err := reconciler.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := runner.restartAttempts(); got != 0 {
		t.Fatalf("restarts for a node with no auth token = %d, want 0", got)
	}
	if got := runner.enrollments(); got != 1 {
		t.Fatalf("enrollments = %d, want 1", got)
	}
}

func paths(writes []recordedRequest) []string {
	values := make([]string, 0, len(writes))
	for _, write := range writes {
		values = append(values, write.Path)
	}
	return values
}

// Restarting is the only repair for an enrolled node, so a restart that keeps
// failing is retried -- up to the repair budget -- rather than ever
// escalating to enroll(): enrolling would mint a fresh token and create a
// duplicate host row for a node Central already knows about, which is
// exactly what spammed Central in the 2026-08-24 incident. (by claude)
func TestReconcileNeverEnrollsAnEnrolledNodeEvenWhenRestartKeepsFailing(t *testing.T) {
	runner := &scriptedRunner{
		statuses:  []scriptedStatus{{output: `{"healthy":false,"service":{"active":"inactive"},"config":{"authTokenConfigured":true}}`}},
		shellErrs: map[string]error{"thunder up": errors.New("thunder: unknown flag --node-name")},
	}
	reconciler, registry := newTestReconciler(t, runner)
	reconciler.repairBudget = &memoryRepairBudgetStore{}
	ctx := context.Background()

	// A failing restart is retried, not surfaced as a reconcile failure.
	// (by claude)
	for pass := 0; pass < repairAttemptLimit+2; pass++ {
		if err := reconciler.reconcile(ctx); err != nil {
			t.Fatalf("reconcile pass %d: %v", pass, err)
		}
	}
	if got := runner.enrollments(); got != 0 {
		t.Fatalf("enrollments for a node whose restart keeps failing = %d, want 0", got)
	}
	if got := countCommands(paths(registry.writes()), "/api/v1/enrollment-tokens"); got != 0 {
		t.Fatalf("enrollment tokens minted = %d, want 0", got)
	}
	if got := runner.restartAttempts(); got != repairAttemptLimit {
		t.Fatalf("restart attempts = %d, want %d (the repair budget caps them)", got, repairAttemptLimit)
	}
}

// A broken CLI on an already-enrolled node is a CLI problem, not an
// enrollment problem: reinstall the binaries, spend no token. (by claude)
func TestReconcileReinstallsTheCLIWithoutATokenWhenAnEnrolledNodesStatusIsUnreadable(t *testing.T) {
	runner := &scriptedRunner{statuses: []scriptedStatus{
		{output: healthyStatus},
		{output: "", err: errors.New("exit status 127: nsenter: failed to execute thunder: No such file or directory")},
	}}
	reconciler, registry := newTestReconciler(t, runner)
	reconciler.cfg.ThunderInstallURL = "https://get.thundercompute.com/install.sh"
	writeHostEnvFile(t, reconciler.cfg.HostRoot, "tok_existing")
	ctx := context.Background()

	if err := reconciler.reconcile(ctx); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
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

	if got := runner.enrollments(); got != 0 {
		t.Fatalf("enrollments for a node whose auth token is already on the host = %d, want 0", got)
	}
	if got := countCommands(paths(registry.writes()), "/api/v1/enrollment-tokens"); got != 0 {
		t.Fatalf("enrollment tokens minted = %d, want 0", got)
	}
	reinstallAt := commandIndex(runner.attempted, "install.sh")
	if reinstallAt == -1 {
		t.Fatalf("the CLI was never reinstalled; commands = %#v", runner.attempted)
	}
	reinstall := runner.attempted[reinstallAt]
	if strings.Contains(reinstall, "THUNDER_ENROLLMENT_TOKEN") || strings.Contains(reinstall, "THUNDER_INSTALL_MODE") {
		t.Fatalf("CLI reinstall command spends an enrollment token:\n%s", reinstall)
	}
}

// The symlink sweep (B3) runs before every repair, not only when a
// dangling symlink happens to be present. (by claude)
func TestReconcileSweepsDanglingSymlinksBeforeEveryRepair(t *testing.T) {
	runner := &scriptedRunner{statuses: []scriptedStatus{{
		output: `{"healthy":false,"service":{"active":"inactive"},"config":{"authTokenConfigured":false}}`,
	}}}
	reconciler, _ := newTestReconciler(t, runner)

	if err := reconciler.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	sweepAt := commandIndex(runner.attempted, "systemctl daemon-reload")
	enrollAt := commandIndex(runner.attempted, "THUNDER_INSTALL_MODE=thunderd")
	if sweepAt == -1 || enrollAt == -1 || sweepAt > enrollAt {
		t.Fatalf("sweep did not run before enroll: attempted = %#v", runner.attempted)
	}
}

// The repair budget survives the reconciler being recreated, and once spent
// it gives up entirely until a healthy pass resets it. (by claude)
func TestReconcileRepairBudgetCapsAttemptsAcrossRestartsAndResetsOnHealth(t *testing.T) {
	unhealthy := `{"healthy":false,"service":{"active":"inactive"},"config":{"authTokenConfigured":true}}`
	statuses := make([]scriptedStatus, 0, repairAttemptLimit+2)
	for i := 0; i < repairAttemptLimit+1; i++ {
		statuses = append(statuses, scriptedStatus{output: unhealthy})
	}
	statuses = append(statuses, scriptedStatus{output: healthyStatus})

	runner := &scriptedRunner{statuses: statuses}
	budget := &memoryRepairBudgetStore{}

	reconciler, _ := newTestReconciler(t, runner)
	reconciler.repairBudget = budget
	ctx := context.Background()

	// Spend the whole budget.
	for i := 0; i < repairAttemptLimit; i++ {
		if err := reconciler.reconcile(ctx); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if got := runner.restartAttempts(); got != repairAttemptLimit {
		t.Fatalf("restart attempts = %d, want %d", got, repairAttemptLimit)
	}
	if budget.marker.Attempts != repairAttemptLimit {
		t.Fatalf("budget marker attempts = %d, want %d", budget.marker.Attempts, repairAttemptLimit)
	}

	// A pod restart recreates the reconciler against the same store: no
	// fresh budget, no further repair of any kind.
	restarted, _ := newTestReconciler(t, runner)
	restarted.repairBudget = budget
	if err := restarted.reconcile(ctx); err != nil {
		t.Fatalf("pass after recreation: %v", err)
	}
	if got := runner.restartAttempts(); got != repairAttemptLimit {
		t.Fatalf("restart attempts after recreation = %d, want %d (budget spent, no retry)", got, repairAttemptLimit)
	}
	if got := runner.enrollments(); got != 0 {
		t.Fatalf("enrollments = %d, want 0: a spent budget never enrolls", got)
	}

	// A healthy pass resets the budget for the next outage.
	if err := restarted.reconcile(ctx); err != nil {
		t.Fatalf("healthy pass: %v", err)
	}
	if !budget.marker.isZero() {
		t.Fatalf("repair budget marker = %+v, want cleared after a healthy pass", budget.marker)
	}
}

// A spent budget from an old daemon build must not block a new image that
// may well contain a fix. (by claude)
func TestReconcileRepairBudgetResetsOnANewDaemonVersion(t *testing.T) {
	oldVersion := version.Version
	version.Version = "v1.0.0"
	t.Cleanup(func() { version.Version = oldVersion })

	unhealthy := `{"healthy":false,"service":{"active":"inactive"},"config":{"authTokenConfigured":true}}`
	statuses := make([]scriptedStatus, repairAttemptLimit+1)
	for i := range statuses {
		statuses[i] = scriptedStatus{output: unhealthy}
	}
	runner := &scriptedRunner{statuses: statuses}
	budget := &memoryRepairBudgetStore{}
	reconciler, _ := newTestReconciler(t, runner)
	reconciler.repairBudget = budget
	ctx := context.Background()

	for i := 0; i < repairAttemptLimit; i++ {
		if err := reconciler.reconcile(ctx); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if got := runner.restartAttempts(); got != repairAttemptLimit {
		t.Fatalf("restart attempts = %d, want %d", got, repairAttemptLimit)
	}

	// A new daemon image ships; the budget must not carry over.
	version.Version = "v1.0.1"
	if err := reconciler.reconcile(ctx); err != nil {
		t.Fatalf("pass after version change: %v", err)
	}
	if got := runner.restartAttempts(); got != repairAttemptLimit+1 {
		t.Fatalf("restart attempts after version change = %d, want %d", got, repairAttemptLimit+1)
	}
	if budget.marker.Attempts != 1 {
		t.Fatalf("budget marker attempts = %d, want 1 (reset by the version change)", budget.marker.Attempts)
	}
}

// fetchThunderdChecksum reads the thunderd line out of a real
// sha256sums.txt response. (by claude)
func TestFetchThunderdChecksumParsesTheThunderdLine(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "aaaa  install.sh\nbbbb  thunderd\ncccc  thunder\n")
	}))
	defer server.Close()

	checksum, err := fetchThunderdChecksum(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("fetchThunderdChecksum: %v", err)
	}
	if checksum != "bbbb" {
		t.Fatalf("checksum = %q, want bbbb", checksum)
	}
}

// The checksum is only fetched once the budget is spent, and a change
// resets the budget and reattempts the repair immediately. (by claude)
func TestReconcileRepairBudgetResetsOnANewThunderdChecksum(t *testing.T) {
	restore := repairThunderdChecksum
	t.Cleanup(func() { repairThunderdChecksum = restore })
	checksum := "checksum-1"
	repairThunderdChecksum = func(context.Context, string) (string, error) { return checksum, nil }

	unhealthy := `{"healthy":false,"service":{"active":"inactive"},"config":{"authTokenConfigured":true}}`
	statuses := make([]scriptedStatus, repairAttemptLimit+2)
	for i := range statuses {
		statuses[i] = scriptedStatus{output: unhealthy}
	}
	runner := &scriptedRunner{statuses: statuses}
	budget := &memoryRepairBudgetStore{}
	reconciler, _ := newTestReconciler(t, runner)
	reconciler.repairBudget = budget
	ctx := context.Background()

	for i := 0; i < repairAttemptLimit; i++ {
		if err := reconciler.reconcile(ctx); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	// Still spent, same checksum served: no repair, but it gets stamped.
	if err := reconciler.reconcile(ctx); err != nil {
		t.Fatalf("pass while spent: %v", err)
	}
	if got := runner.restartAttempts(); got != repairAttemptLimit {
		t.Fatalf("restart attempts while spent = %d, want %d", got, repairAttemptLimit)
	}
	if budget.marker.ThunderdChecksum != checksum {
		t.Fatalf("stamped checksum = %q, want %q", budget.marker.ThunderdChecksum, checksum)
	}

	// Distribution now serves a different thunderd: reset and retry.
	checksum = "checksum-2"
	if err := reconciler.reconcile(ctx); err != nil {
		t.Fatalf("pass after checksum change: %v", err)
	}
	if got := runner.restartAttempts(); got != repairAttemptLimit+1 {
		t.Fatalf("restart attempts after checksum change = %d, want %d", got, repairAttemptLimit+1)
	}
	if budget.marker.ThunderdChecksum != checksum {
		t.Fatalf("restamped checksum = %q, want %q", budget.marker.ThunderdChecksum, checksum)
	}
}

// A checksum fetch failure must not reset the budget. (by claude)
func TestReconcileRepairBudgetStaysSpentWhenTheChecksumFetchFails(t *testing.T) {
	restore := repairThunderdChecksum
	t.Cleanup(func() { repairThunderdChecksum = restore })
	repairThunderdChecksum = func(context.Context, string) (string, error) {
		return "", errors.New("distribution unreachable")
	}

	unhealthy := `{"healthy":false,"service":{"active":"inactive"},"config":{"authTokenConfigured":true}}`
	statuses := make([]scriptedStatus, repairAttemptLimit+1)
	for i := range statuses {
		statuses[i] = scriptedStatus{output: unhealthy}
	}
	runner := &scriptedRunner{statuses: statuses}
	budget := &memoryRepairBudgetStore{}
	reconciler, _ := newTestReconciler(t, runner)
	reconciler.repairBudget = budget
	ctx := context.Background()

	for i := 0; i < repairAttemptLimit; i++ {
		if err := reconciler.reconcile(ctx); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if err := reconciler.reconcile(ctx); err != nil {
		t.Fatalf("pass after budget spent: %v", err)
	}
	if got := runner.restartAttempts(); got != repairAttemptLimit {
		t.Fatalf("restart attempts = %d, want %d (a fetch error must not reset the budget)", got, repairAttemptLimit)
	}
	if budget.marker.ThunderdChecksum != "" {
		t.Fatalf("checksum stamped despite the fetch failing: %q", budget.marker.ThunderdChecksum)
	}
}

// memoryRepairBudgetStore is a repairBudgetStore fake a test can share
// across two reconciler instances, the way a host file would. (by claude)
type memoryRepairBudgetStore struct {
	marker repairBudgetMarker
}

func (s *memoryRepairBudgetStore) Load(context.Context) (repairBudgetMarker, bool, error) {
	return s.marker, false, nil
}

func (s *memoryRepairBudgetStore) Save(_ context.Context, marker repairBudgetMarker) error {
	s.marker = marker
	return nil
}
