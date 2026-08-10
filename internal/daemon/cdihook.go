package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	config, err := ExchangeClientEnrollment(ctx, &http.Client{Timeout: cdiHookTimeout},
		opts.CentralURL, opts.TelemetryURL, strings.TrimSpace(string(token)), clientName)
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

// lockStateDir serialises hooks staging the same claim.
func lockStateDir(stateDir string) (func(), error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create claim state dir: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(stateDir, thunderLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open claim state lock: %w", err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		lock.Close()
		return nil, fmt.Errorf("lock claim state dir: %w", err)
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
	return stageCABundle(rootfs, caBundlePath)
}

// stageCABundle gives libthunder.so something to verify Thunder's certificate
// against. Minimal images — ubuntu:22.04 among them — ship no trust store at
// all, and without one every call to the control plane fails inside the library
// rather than anywhere the user can see.
//
// An image that has its own bundle keeps it: overwriting would silently
// override a deliberate trust configuration.
func stageCABundle(rootfs, caBundlePath string) error {
	if strings.TrimSpace(caBundlePath) == "" {
		caBundlePath = DefaultCABundlePath
	}
	target := filepath.Join(rootfs, DefaultCABundlePath)
	if _, err := os.Stat(target); err == nil {
		return nil
	}
	if _, err := os.Stat(caBundlePath); err != nil {
		// No trust store on the node either. Say so against this container
		// rather than letting the library fail with a curl error later.
		return fmt.Errorf("container has no CA bundle and the node has none at %s: %w", caBundlePath, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create CA bundle dir in container: %w", err)
	}
	if err := copyFile(caBundlePath, target, 0o644); err != nil {
		return fmt.Errorf("stage CA bundle into container: %w", err)
	}
	return nil
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
