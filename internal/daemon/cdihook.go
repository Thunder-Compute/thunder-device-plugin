package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// CDIHookCommand is the argv[1] the CDI spec invokes the daemon with.
	CDIHookCommand = "cdi-hook"

	// ThunderGuestDir is where the client lives inside a container. It is not
	// configurable: LD_PRELOAD names this path.
	ThunderGuestDir = "/etc/thunder"

	// Files the daemon stages, and the hook caches, per claim.
	thunderTokenFile  = "enrollment-token"
	thunderConfigFile = "config.json"
	thunderLockFile   = "stage.lock"

	// cdiHookTimeout bounds the whole hook. Container creation blocks on it,
	// so it has to fail rather than hang when Thunder is unreachable.
	cdiHookTimeout = 90 * time.Second

	// A burst of pods starting together produces a burst of enrollment
	// exchanges. Without a retry, one rate-limited response turns a busy
	// moment into a container that fails to start.
	exchangeAttempts     = 5
	exchangeInitialDelay = 500 * time.Millisecond
	exchangeMaxDelay     = 8 * time.Second
)

// ociState is the part of the OCI runtime state the hook is handed on stdin.
// The hook runs before pivot_root, so the container filesystem is reachable
// only through the bundle named here, never through "/".
type ociState struct {
	ID     string `json:"id"`
	Bundle string `json:"bundle"`
	Rootfs string `json:"rootfs"`
}

// CDIHookOptions is what the CDI spec encodes into the hook's argv.
type CDIHookOptions struct {
	// StateDir is the per-claim directory the daemon staged the enrollment
	// token into, and where the exchanged config is cached.
	StateDir string
	// CentralURL and TelemetryURL configure the client being enrolled.
	CentralURL   string
	TelemetryURL string
	// InstallURL, ArtifactBaseURL, LibthunderURL and LibthunderSHA256 locate
	// the client library.
	InstallURL       string
	ArtifactBaseURL  string
	LibthunderURL    string
	LibthunderSHA256 string
	// CacheDir holds this node's shared copy of the library.
	CacheDir string
	// CABundlePath is the node's trust store, staged into containers that
	// have none of their own.
	CABundlePath string
	// ClientName is what this client is called in Thunder. It is the pod name,
	// so an enrollment can be traced back to the workload holding it.
	ClientName string
}

// ParseCDIHookArgs reads the flags the CDI spec encodes into the hook argv.
func ParseCDIHookArgs(args []string) (CDIHookOptions, error) {
	var opts CDIHookOptions
	for i := 0; i < len(args); i++ {
		name, value := args[i], ""
		if equals := strings.IndexByte(name, '='); equals >= 0 {
			name, value = name[:equals], name[equals+1:]
		} else if i+1 < len(args) {
			i++
			value = args[i]
		} else {
			return CDIHookOptions{}, fmt.Errorf("flag %s requires a value", name)
		}

		switch name {
		case "--state-dir":
			opts.StateDir = value
		case "--central-url":
			opts.CentralURL = value
		case "--telemetry-url":
			opts.TelemetryURL = value
		case "--install-url":
			opts.InstallURL = value
		case "--artifact-base-url":
			opts.ArtifactBaseURL = value
		case "--libthunder-url":
			opts.LibthunderURL = value
		case "--libthunder-sha256":
			opts.LibthunderSHA256 = value
		case "--cache-dir":
			opts.CacheDir = value
		case "--ca-bundle":
			opts.CABundlePath = value
		case "--client-name":
			opts.ClientName = value
		default:
			return CDIHookOptions{}, fmt.Errorf("unknown flag %s", name)
		}
	}
	if strings.TrimSpace(opts.StateDir) == "" {
		return CDIHookOptions{}, fmt.Errorf("--state-dir is required")
	}
	return opts, nil
}

// RunCDIHook stages the Thunder client into a container that is being created.
//
// It runs on the host, once per container, with the container filesystem at
// <bundle>/rootfs. Everything the workload needs is written there: the library
// LD_PRELOAD names, and the config.json identifying the client. Nothing is
// assumed about the image — it needs no shell, no curl, and no root.
func RunCDIHook(ctx context.Context, opts CDIHookOptions, stdin io.Reader) error {
	ctx, cancel := context.WithTimeout(ctx, cdiHookTimeout)
	defer cancel()

	rootfs, err := readOCIRootfs(stdin)
	if err != nil {
		return err
	}

	// Containers sharing a claim are staged one at a time. Without this, two
	// containers starting together would both try to spend the same single-use
	// enrollment token, and the loser would fail to start.
	unlock, err := lockStateDir(opts.StateDir)
	if err != nil {
		return err
	}
	defer unlock()

	cache := &LibthunderCache{
		Dir:             opts.CacheDir,
		InstallURL:      opts.InstallURL,
		URL:             opts.LibthunderURL,
		SHA256:          opts.LibthunderSHA256,
		ArtifactBaseURL: opts.ArtifactBaseURL,
		HTTP:            &http.Client{Timeout: cdiHookTimeout},
	}
	libraryPath, err := cache.Ensure(ctx)
	if err != nil {
		return fmt.Errorf("stage libthunder.so: %w", err)
	}

	config, err := clientConfigForClaim(ctx, opts)
	if err != nil {
		return err
	}
	return stageThunderClient(rootfs, libraryPath, config, opts.CABundlePath)
}

// clientConfigForClaim returns the claim's client config, exchanging the
// enrollment token the first time and reusing the result afterwards.
//
// Reuse is what makes the hook safe to re-run. A container restart, or a second
// container on the same claim, invokes the hook again, and the enrollment token
// has already been spent by then — re-exchanging would fail and the container
// would never start.
func clientConfigForClaim(ctx context.Context, opts CDIHookOptions) (ThunderClientConfig, error) {
	configPath := filepath.Join(opts.StateDir, thunderConfigFile)
	if cached, err := os.ReadFile(configPath); err == nil {
		var config ThunderClientConfig
		if err := json.Unmarshal(cached, &config); err == nil && config.ClientID != "" {
			return config, nil
		}
		// A corrupt cache is not worth failing over; the token may still be
		// unspent, so fall through and try the exchange.
	}

	tokenPath := filepath.Join(opts.StateDir, thunderTokenFile)
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return ThunderClientConfig{}, fmt.Errorf("read staged enrollment token: %w", err)
	}

	// Falls back to the CDI device name only when the claim was not reserved
	// by anything at prepare time, which should not happen for a running pod.
	clientName := strings.TrimSpace(opts.ClientName)
	if clientName == "" {
		clientName = filepath.Base(opts.StateDir)
	}
	config, err := exchangeWithRetry(ctx, opts, strings.TrimSpace(string(token)), clientName)
	if err != nil {
		return ThunderClientConfig{}, err
	}

	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return ThunderClientConfig{}, fmt.Errorf("encode thunder client config: %w", err)
	}
	if err := writeFileAtomic(configPath, append(encoded, '\n'), 0o600); err != nil {
		return ThunderClientConfig{}, fmt.Errorf("cache thunder client config: %w", err)
	}
	// The token is spent. Removing it keeps a consumed secret from sitting on
	// the node for the life of the claim.
	_ = os.Remove(tokenPath)
	return config, nil
}

// exchangeWithRetry spends the enrollment token, retrying the failures another
// attempt could get past. A rejection is returned immediately: retrying an
// already-spent token only delays the real error and holds up the container.
//
// The whole loop lives inside the hook's deadline, so a node that cannot reach
// Thunder fails the container rather than retrying until the runtime kills it.
func exchangeWithRetry(ctx context.Context, opts CDIHookOptions, token, clientName string) (ThunderClientConfig, error) {
	httpClient := &http.Client{Timeout: cdiHookTimeout}
	delay := exchangeInitialDelay

	var err error
	for attempt := 1; attempt <= exchangeAttempts; attempt++ {
		var config ThunderClientConfig
		config, err = ExchangeClientEnrollment(ctx, httpClient,
			opts.CentralURL, opts.TelemetryURL, token, clientName)
		if err == nil {
			return config, nil
		}
		if !isRetryable(err) || attempt == exchangeAttempts {
			return ThunderClientConfig{}, err
		}

		wait := jitter(delay)
		// Fail now rather than sleeping into a deadline that will cut the next
		// attempt off mid-request.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait+exchangeInitialDelay {
			return ThunderClientConfig{}, err
		}
		select {
		case <-ctx.Done():
			return ThunderClientConfig{}, err
		case <-time.After(wait):
		}
		if delay < exchangeMaxDelay {
			delay *= 2
		}
	}
	return ThunderClientConfig{}, err
}

// jitter spreads retries so a burst of hooks does not resynchronise on the
// same backoff schedule and hit Thunder in lockstep.
func jitter(delay time.Duration) time.Duration {
	return delay/2 + time.Duration(rand.Int64N(int64(delay)))
}

// lockStateDir serialises hooks staging the same claim.
func lockStateDir(stateDir string) (func(), error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create claim state dir: %w", err)
	}
	return lockFile(filepath.Join(stateDir, thunderLockFile), "claim state")
}

// lockFile takes an exclusive advisory lock, blocking until it is granted, and
// returns the release. The lock is held on a file descriptor, so it is released
// even if the process is killed.
func lockFile(path, what string) (func(), error) {
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s lock: %w", what, err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		lock.Close()
		return nil, fmt.Errorf("lock %s: %w", what, err)
	}
	return func() {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
	}, nil
}

// readOCIRootfs resolves the container filesystem from the runtime state, which
// the runtime writes to the hook's stdin as one JSON document.
func readOCIRootfs(stdin io.Reader) (string, error) {
	if stdin == nil {
		return "", fmt.Errorf("no OCI state on stdin")
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, installerReadLimit))
	if err != nil {
		return "", fmt.Errorf("read OCI state: %w", err)
	}

	var state ociState
	if err := json.Unmarshal(raw, &state); err != nil {
		return "", fmt.Errorf("decode OCI state: %w", err)
	}
	// Most runtimes report the bundle and put the filesystem in the
	// conventional subdirectory; some report the rootfs outright.
	rootfs := strings.TrimSpace(state.Rootfs)
	if rootfs == "" {
		if strings.TrimSpace(state.Bundle) == "" {
			return "", fmt.Errorf("OCI state names neither a bundle nor a rootfs")
		}
		rootfs = filepath.Join(state.Bundle, "rootfs")
	}
	info, err := os.Stat(rootfs)
	if err != nil {
		return "", fmt.Errorf("stat container rootfs %s: %w", rootfs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("container rootfs %s is not a directory", rootfs)
	}
	return rootfs, nil
}

// stageThunderClient writes the library and its config into the container.
func stageThunderClient(rootfs, libraryPath string, config ThunderClientConfig, caBundlePath string) error {
	targetDir := filepath.Join(rootfs, ThunderGuestDir)
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("create %s in container: %w", ThunderGuestDir, err)
	}

	// Copy rather than bind-mount: the container filesystem goes away with the
	// container, so nothing is left to unmount when it exits.
	if err := copyFile(libraryPath, filepath.Join(targetDir, "libthunder.so"), 0o755); err != nil {
		return fmt.Errorf("stage libthunder.so into container: %w", err)
	}

	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encode thunder client config: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(targetDir, thunderConfigFile), append(encoded, '\n'), 0o644); err != nil {
		return fmt.Errorf("stage config.json into container: %w", err)
	}
	stageCABundle(rootfs, caBundlePath)
	return nil
}

// stageCABundle gives libthunder.so something to verify Thunder's certificate
// against. Minimal images — ubuntu:22.04 among them — ship no trust store at
// all, and without one every call to the control plane fails inside the library
// rather than anywhere the user can see.
//
// This is best effort, and deliberately so. An image that already has a trust
// store keeps it, and an image whose /etc/ssl layout this cannot write into is
// left alone rather than being refused a container. Failing here would block
// the container from starting at all, which is far worse than a client that
// cannot reach the control plane: it would take down workloads that never
// wanted a GPU library in the first place, such as a KubeVirt virt-launcher,
// where the guest installs its own client and this copy goes unused.
func stageCABundle(rootfs, caBundlePath string) {
	if strings.TrimSpace(caBundlePath) == "" {
		caBundlePath = DefaultCABundlePath
	}
	target := filepath.Join(rootfs, DefaultCABundlePath)

	// Lstat, not Stat: a symlink counts as the image having its own trust
	// configuration even when it dangles inside this rootfs. Stat follows the
	// link, reports "missing", and sends us on to clobber it.
	if _, err := os.Lstat(target); err == nil {
		return
	}
	if _, err := os.Stat(caBundlePath); err != nil {
		return
	}
	// The parent may exist as a symlink or a file rather than a directory, in
	// which case MkdirAll fails and the image keeps whatever it has.
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		log.Printf("not staging a CA bundle into the container: %v", err)
		return
	}
	if err := copyFile(caBundlePath, target, 0o644); err != nil {
		log.Printf("not staging a CA bundle into the container: %v", err)
	}
}

func copyFile(source, target string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.CreateTemp(filepath.Dir(target), ".stage-*")
	if err != nil {
		return err
	}
	tempName := out.Name()
	defer os.Remove(tempName)

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempName, mode); err != nil {
		return err
	}
	return os.Rename(tempName, target)
}

func writeFileAtomic(target string, data []byte, mode os.FileMode) error {
	out, err := os.CreateTemp(filepath.Dir(target), ".stage-*")
	if err != nil {
		return err
	}
	tempName := out.Name()
	defer os.Remove(tempName)

	if _, err := out.Write(data); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempName, mode); err != nil {
		return err
	}
	return os.Rename(tempName, target)
}

// StageHookBinary copies the running daemon to a host path the container
// runtime can execute, and returns that path.
//
// The hook runs as a bare process on the node, so it cannot be the binary
// inside this container. The kubelet plugin directory is a hostPath mounted at
// the same path on both sides, which makes a copy written here valid in the CDI
// spec the runtime reads. This is the model nvidia-ctk uses.
func StageHookBinary(pluginDir string) (string, error) {
	if strings.TrimSpace(pluginDir) == "" {
		return "", fmt.Errorf("kubelet plugin dir is required to stage the CDI hook")
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate running daemon binary: %w", err)
	}

	target := filepath.Join(pluginDir, "bin", "thunder-cdi-hook")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("create CDI hook dir: %w", err)
	}
	// Restaged on every daemon start so the hook tracks the image version.
	// Renaming over the old copy leaves any in-flight hook running the inode
	// it already opened.
	if err := copyFile(self, target, 0o755); err != nil {
		return "", fmt.Errorf("stage CDI hook binary: %w", err)
	}
	return target, nil
}
