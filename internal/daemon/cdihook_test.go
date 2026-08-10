package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// writeTestCABundle stands in for the node's trust store.
func writeTestCABundle(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca-certificates.crt")
	if err := os.WriteFile(path, []byte("-----BEGIN CERTIFICATE-----\nnode trust store\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// installerFixture is the shape the daemon reads a libthunder.so pin out of.
func installerFixture(base, sha string) string {
	return fmt.Sprintf(`#!/bin/sh
default_artifact_base_url='%s'
libthunder_sha256="%s"
`, base, sha)
}

// clientJWT builds a token whose payload carries the claims the exchange reads.
func clientJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"HS256"}`)) + "." + encode(payload) + "." + encode([]byte("signature"))
}

// thunderFixture is a stand-in for the Thunder artifact host and API.
type thunderFixture struct {
	server    *httptest.Server
	library   []byte
	sha       string
	exchanges int
	lastAuth  string
	lastBody  map[string]any
	caBundle  string
}

func newThunderFixture(t *testing.T) *thunderFixture {
	t.Helper()
	fixture := &thunderFixture{library: []byte("ELF libthunder payload")}
	fixture.sha = sha256Hex(fixture.library)

	// Stand in for the node's trust store.
	fixture.caBundle = filepath.Join(t.TempDir(), "ca-certificates.crt")
	if err := os.WriteFile(fixture.caBundle, []byte("-----BEGIN CERTIFICATE-----\nnode trust store\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/install.sh", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, installerFixture(strings.TrimSuffix(fixture.server.URL, "/"), fixture.sha))
	})
	mux.HandleFunc("/libthunder.so", func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture.library)
	})
	mux.HandleFunc("/api/v1/enrollment-tokens/enroll", func(w http.ResponseWriter, r *http.Request) {
		fixture.exchanges++
		fixture.lastAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&fixture.lastBody)
		writeJSON(t, w, map[string]any{
			"authToken": "auth-token-value",
			"jwt": clientJWT(t, map[string]any{
				"orgId": "org-1", "clientId": "client-1", "gpuType": "A6000", "gpuCount": 2,
			}),
		})
	})
	fixture.server = httptest.NewServer(mux)
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *thunderFixture) hookOptions(stateDir, cacheDir string) CDIHookOptions {
	return CDIHookOptions{
		StateDir:     stateDir,
		CacheDir:     cacheDir,
		CABundlePath: f.caBundle,
		CentralURL:   f.server.URL,
		TelemetryURL: "https://telemetry.test:2096",
		InstallURL:   f.server.URL + "/install.sh",
	}
}

// ociStateStdin is what the container runtime writes to the hook.
func ociStateStdin(t *testing.T, bundle string) *strings.Reader {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(bundle, "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(map[string]any{
		"ociVersion": "1.0.2", "id": "container-1", "pid": 4242, "status": "created", "bundle": bundle,
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReader(string(state))
}

func stageToken(t *testing.T, stateDir, token string) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, thunderTokenFile), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseLibthunderArtifact(t *testing.T) {
	sha := strings.Repeat("a", 64)
	installer := installerFixture("https://get.thundercompute.com", sha)

	artifact, err := parseLibthunderArtifact(installer, "")
	if err != nil {
		t.Fatalf("parseLibthunderArtifact: %v", err)
	}
	if artifact.URL != "https://get.thundercompute.com/libthunder.so" {
		t.Fatalf("URL = %q", artifact.URL)
	}
	if artifact.SHA256 != sha {
		t.Fatalf("SHA256 = %q", artifact.SHA256)
	}

	// An override replaces the host but keeps the installer's digest, so a
	// mirror still serves a verified build.
	artifact, err = parseLibthunderArtifact(installer, "https://mirror.test/artifacts/")
	if err != nil {
		t.Fatalf("parseLibthunderArtifact with override: %v", err)
	}
	if artifact.URL != "https://mirror.test/artifacts/libthunder.so" {
		t.Fatalf("overridden URL = %q", artifact.URL)
	}
	if artifact.SHA256 != sha {
		t.Fatalf("override changed the digest to %q", artifact.SHA256)
	}

	if _, err := parseLibthunderArtifact("#!/bin/sh\necho hi\n", ""); err == nil {
		t.Fatal("parsing an installer with no pin succeeded, want an error")
	}
}

func TestLibthunderCacheDownloadsVerifiesAndReuses(t *testing.T) {
	fixture := newThunderFixture(t)
	dir := t.TempDir()
	cache := &LibthunderCache{Dir: dir, InstallURL: fixture.server.URL + "/install.sh"}

	path, err := cache.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	staged, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read staged library: %v", err)
	}
	if string(staged) != string(fixture.library) {
		t.Fatalf("staged library = %q", staged)
	}
	// The digest is in the filename, so a new release cannot silently replace
	// the build running containers were staged from.
	if !strings.Contains(filepath.Base(path), fixture.sha) {
		t.Fatalf("cache path %q does not name the digest", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
	}

	// A second node-level call reuses the cache rather than downloading again.
	fixture.server.Close()
	again, err := cache.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure with the artifact host down: %v", err)
	}
	if again != path {
		t.Fatalf("second Ensure returned %q, want %q", again, path)
	}
}

func TestLibthunderCacheRejectsADigestMismatch(t *testing.T) {
	dir := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("/install.sh", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, installerFixture("http://"+r.Host, strings.Repeat("b", 64)))
	})
	mux.HandleFunc("/libthunder.so", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not the pinned build"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cache := &LibthunderCache{Dir: dir, InstallURL: server.URL + "/install.sh"}
	_, err := cache.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure accepted a library that does not match the pin")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("error = %v, want a digest mismatch", err)
	}
	// Nothing unverified is left behind for a later run to trust.
	entries, _ := os.ReadDir(filepath.Join(dir, libthunderCacheSubdir))
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("unverified download was published as %q", entry.Name())
		}
	}
}

func TestExchangeClientEnrollmentBuildsTheInstallerConfig(t *testing.T) {
	fixture := newThunderFixture(t)

	config, err := ExchangeClientEnrollment(context.Background(), nil,
		fixture.server.URL, "https://telemetry.test:2096", "tr_secret", "claim-abc")
	if err != nil {
		t.Fatalf("ExchangeClientEnrollment: %v", err)
	}

	if fixture.lastAuth != "Bearer tr_secret" {
		t.Fatalf("Authorization = %q", fixture.lastAuth)
	}
	if fixture.lastBody["hostname"] != "claim-abc" {
		t.Fatalf("hostname = %v", fixture.lastBody["hostname"])
	}
	// The field set has to match what the installer writes, since libthunder.so
	// is the thing reading it.
	want := ThunderClientConfig{
		DeviceID: "client-1", ClientID: "client-1", OrgID: "org-1",
		GPUType: "A6000", GPUCount: 2,
		CentralAPIURL: fixture.server.URL, AuthToken: "auth-token-value",
		Claims: config.Claims, EnableGRPCTLS: false, ThunderdDiscoveryEnabled: true,
		TelemetryCollector: "https://telemetry.test:2096",
	}
	if config != want {
		t.Fatalf("config = %#v\nwant %#v", config, want)
	}
}

func TestExchangeClientEnrollmentSurfacesAnAPIFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":"enrollment token already used"}`)
	}))
	defer server.Close()

	_, err := ExchangeClientEnrollment(context.Background(), nil, server.URL, "", "tr_spent", "claim")
	if err == nil {
		t.Fatal("exchange succeeded against a rejecting API")
	}
	if !strings.Contains(err.Error(), "already used") {
		t.Fatalf("error = %v, want the API message preserved", err)
	}
}

func TestDecodeClientClaimsRejectsIncompleteClaims(t *testing.T) {
	for name, token := range map[string]string{
		"not a JWT":        "garbage",
		"missing clientId": clientJWT(t, map[string]any{"orgId": "o", "gpuType": "A6000", "gpuCount": 1}),
		"missing gpuCount": clientJWT(t, map[string]any{"orgId": "o", "clientId": "c", "gpuType": "A6000"}),
	} {
		if _, err := decodeClientClaims(token); err == nil {
			t.Errorf("%s: decodeClientClaims succeeded, want an error", name)
		}
	}
}

func TestRunCDIHookStagesTheClientIntoTheContainer(t *testing.T) {
	fixture := newThunderFixture(t)
	stateDir := filepath.Join(t.TempDir(), "claim-abc")
	bundle := t.TempDir()
	stageToken(t, stateDir, "tr_secret")

	opts := fixture.hookOptions(stateDir, t.TempDir())
	if err := RunCDIHook(context.Background(), opts, ociStateStdin(t, bundle)); err != nil {
		t.Fatalf("RunCDIHook: %v", err)
	}

	// The container gets exactly what LD_PRELOAD needs, in a rootfs that began
	// with no shell, no curl and no Thunder client.
	library := filepath.Join(bundle, "rootfs", ThunderGuestDir, "libthunder.so")
	staged, err := os.ReadFile(library)
	if err != nil {
		t.Fatalf("read staged libthunder.so: %v", err)
	}
	if string(staged) != string(fixture.library) {
		t.Fatalf("staged library = %q", staged)
	}
	info, err := os.Stat(library)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("libthunder.so mode = %v, want 0755", info.Mode().Perm())
	}

	var config ThunderClientConfig
	raw, err := os.ReadFile(filepath.Join(bundle, "rootfs", ThunderGuestDir, thunderConfigFile))
	if err != nil {
		t.Fatalf("read staged config.json: %v", err)
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("decode staged config.json: %v", err)
	}
	if config.ClientID != "client-1" || config.AuthToken != "auth-token-value" {
		t.Fatalf("staged config = %#v", config)
	}

	// The token is single use; once spent it should not linger on the node.
	if _, err := os.Stat(filepath.Join(stateDir, thunderTokenFile)); !os.IsNotExist(err) {
		t.Fatalf("spent enrollment token still on disk: %v", err)
	}
}

// A container restart re-runs the hook after the token has been spent. Without
// a cached config the exchange would fail and the pod would never come back.
func TestRunCDIHookSurvivesAContainerRestart(t *testing.T) {
	fixture := newThunderFixture(t)
	stateDir := filepath.Join(t.TempDir(), "claim-abc")
	cacheDir := t.TempDir()
	stageToken(t, stateDir, "tr_secret")
	opts := fixture.hookOptions(stateDir, cacheDir)

	first := t.TempDir()
	if err := RunCDIHook(context.Background(), opts, ociStateStdin(t, first)); err != nil {
		t.Fatalf("first RunCDIHook: %v", err)
	}

	// Same claim, new container filesystem, token already gone.
	second := t.TempDir()
	if err := RunCDIHook(context.Background(), opts, ociStateStdin(t, second)); err != nil {
		t.Fatalf("RunCDIHook after restart: %v", err)
	}
	if fixture.exchanges != 1 {
		t.Fatalf("enrollment exchanges = %d, want 1 reused across both containers", fixture.exchanges)
	}

	var config ThunderClientConfig
	raw, err := os.ReadFile(filepath.Join(second, "rootfs", ThunderGuestDir, thunderConfigFile))
	if err != nil {
		t.Fatalf("read config staged into the restarted container: %v", err)
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if config.ClientID != "client-1" {
		t.Fatalf("restarted container got config %#v", config)
	}
}

func TestRunCDIHookRejectsUnusableOCIState(t *testing.T) {
	fixture := newThunderFixture(t)
	stateDir := filepath.Join(t.TempDir(), "claim-abc")
	stageToken(t, stateDir, "tr_secret")
	opts := fixture.hookOptions(stateDir, t.TempDir())

	for name, state := range map[string]string{
		"not JSON":       "{{{",
		"no bundle":      `{"ociVersion":"1.0.2","id":"c"}`,
		"missing rootfs": `{"ociVersion":"1.0.2","id":"c","bundle":"/nonexistent-bundle"}`,
	} {
		if err := RunCDIHook(context.Background(), opts, strings.NewReader(state)); err == nil {
			t.Errorf("%s: RunCDIHook succeeded, want an error", name)
		}
	}
}

func TestParseCDIHookArgs(t *testing.T) {
	opts, err := ParseCDIHookArgs([]string{
		"--state-dir", "/state/claim-a",
		"--central-url=https://central.test:2096",
		"--cache-dir", "/plugins",
	})
	if err != nil {
		t.Fatalf("ParseCDIHookArgs: %v", err)
	}
	if opts.StateDir != "/state/claim-a" || opts.CacheDir != "/plugins" {
		t.Fatalf("opts = %#v", opts)
	}
	if opts.CentralURL != "https://central.test:2096" {
		t.Fatalf("--flag=value form not handled: %#v", opts)
	}

	if _, err := ParseCDIHookArgs([]string{"--cache-dir", "/plugins"}); err == nil {
		t.Fatal("ParseCDIHookArgs accepted args with no --state-dir")
	}
	if _, err := ParseCDIHookArgs([]string{"--nope", "x"}); err == nil {
		t.Fatal("ParseCDIHookArgs accepted an unknown flag")
	}
}

// The CDI spec is what actually reaches the container runtime, so the hook has
// to survive the round trip into it.
func TestCDISpecCarriesTheHookAndStagesTheToken(t *testing.T) {
	specDir := t.TempDir()
	stateRoot := t.TempDir()
	store := NewFileCDIDeviceStore(specDir)
	store.StateDir = stateRoot
	store.ClientInstallCommand = "curl -fsSL https://get.thundercompute.com/install.sh | sh"
	store.HookPath = "/var/lib/kubelet/plugins/thundercompute.com/bin/thunder-cdi-hook"
	store.CacheDir = "/var/lib/kubelet/plugins/thundercompute.com"
	store.CentralURL = "https://central.test:2096"
	store.TelemetryURL = "https://telemetry.test:2096"

	allocation := Allocation{
		ClaimUID: "11111111-1111-1111-1111-111111111111", ClaimNamespace: "default",
		ClaimName: "claim-a", GPUType: "A6000", GPUCount: 1,
	}
	qualifiedName, err := store.Create(context.Background(), allocation, "tr_secret")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	raw, err := os.ReadFile(store.specPath(qualifiedName))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec struct {
		Devices []struct {
			ContainerEdits struct {
				Hooks []struct {
					HookName string   `json:"hookName"`
					Path     string   `json:"path"`
					Args     []string `json:"args"`
					Timeout  int      `json:"timeout"`
				} `json:"hooks"`
			} `json:"containerEdits"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	hooks := spec.Devices[0].ContainerEdits.Hooks
	if len(hooks) != 1 {
		t.Fatalf("hooks = %#v, want exactly one", hooks)
	}
	hook := hooks[0]
	// createRuntime, not createContainer. createContainer runs in the
	// container's namespaces, where the hook has no working network: it
	// resolves the host's resolv.conf inside the container's empty network
	// namespace and every fetch dies with connection refused.
	if hook.HookName != "createRuntime" {
		t.Fatalf("hookName = %q, want createRuntime", hook.HookName)
	}
	if hook.Path != store.HookPath || hook.Timeout != cdiHookTimeoutSeconds {
		t.Fatalf("hook = %#v", hook)
	}
	joined := strings.Join(hook.Args, " ")
	for _, want := range []string{
		CDIHookCommand,
		"--state-dir " + store.stateDir(cdiDeviceName(allocation.ClaimUID)),
		"--central-url https://central.test:2096",
		"--telemetry-url https://telemetry.test:2096",
		"--client-name claim-a",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("hook args %q missing %q", joined, want)
		}
	}

	// The token reaches the hook through a 0600 file, not the argv, so it is
	// not exposed in the spec or in host process listings.
	if strings.Contains(string(raw), "tr_secret\"") && strings.Contains(joined, "tr_secret") {
		t.Fatal("enrollment token leaked into the hook args")
	}
	tokenPath := filepath.Join(store.stateDir(cdiDeviceName(allocation.ClaimUID)), thunderTokenFile)
	staged, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read staged token: %v", err)
	}
	if strings.TrimSpace(string(staged)) != "tr_secret" {
		t.Fatalf("staged token = %q", staged)
	}
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %v, want 0600", info.Mode().Perm())
	}

	// Removing the claim takes the token and the cached config with it.
	if err := store.Remove(context.Background(), qualifiedName); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("token survived Remove: %v", err)
	}
}

// With no hook configured the spec must stay hook-free, so a deployment that
// supplies its own client image is not forced through the staging path.
func TestCDISpecOmitsTheHookWhenDisabled(t *testing.T) {
	store := NewFileCDIDeviceStore(t.TempDir())
	store.StateDir = t.TempDir()
	store.ClientInstallCommand = "install"

	qualifiedName, err := store.Create(context.Background(), Allocation{
		ClaimUID: "22222222-2222-2222-2222-222222222222", ClaimName: "claim-b", GPUType: "A6000", GPUCount: 1,
	}, "tr_secret")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	raw, err := os.ReadFile(store.specPath(qualifiedName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hooks") {
		t.Fatalf("spec carries hooks with no HookPath set:\n%s", raw)
	}
}

// libthunder.so verifies TLS to the Thunder control plane, and minimal images
// ship no trust store. Without this the library fails inside the container with
// a curl error the user never sees the cause of.
func TestRunCDIHookStagesACABundleWhenTheImageHasNone(t *testing.T) {
	fixture := newThunderFixture(t)
	stateDir := filepath.Join(t.TempDir(), "claim-abc")
	bundle := t.TempDir()
	stageToken(t, stateDir, "tr_secret")

	if err := RunCDIHook(context.Background(), fixture.hookOptions(stateDir, t.TempDir()), ociStateStdin(t, bundle)); err != nil {
		t.Fatalf("RunCDIHook: %v", err)
	}
	staged, err := os.ReadFile(filepath.Join(bundle, "rootfs", DefaultCABundlePath))
	if err != nil {
		t.Fatalf("read staged CA bundle: %v", err)
	}
	if !strings.Contains(string(staged), "node trust store") {
		t.Fatalf("staged CA bundle = %q", staged)
	}
}

// An image with its own trust store keeps it: overwriting would override a
// deliberate trust configuration.
func TestRunCDIHookKeepsAnImagesOwnCABundle(t *testing.T) {
	fixture := newThunderFixture(t)
	stateDir := filepath.Join(t.TempDir(), "claim-abc")
	bundle := t.TempDir()
	stageToken(t, stateDir, "tr_secret")

	target := filepath.Join(bundle, "rootfs", DefaultCABundlePath)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("image's own bundle"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RunCDIHook(context.Background(), fixture.hookOptions(stateDir, t.TempDir()), ociStateStdin(t, bundle)); err != nil {
		t.Fatalf("RunCDIHook: %v", err)
	}
	kept, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != "image's own bundle" {
		t.Fatalf("hook replaced the image's CA bundle with %q", kept)
	}
}

// The enrollment is named after the pod, so an enrollment in the Thunder
// console can be traced back to the workload holding it.
func TestRunCDIHookNamesTheClientAfterThePod(t *testing.T) {
	fixture := newThunderFixture(t)
	stateDir := filepath.Join(t.TempDir(), "claim-d2c9d1f7")
	stageToken(t, stateDir, "tr_secret")

	opts := fixture.hookOptions(stateDir, t.TempDir())
	opts.ClientName = "thunder-a6000-test-thunder-gpu-test-pod"
	if err := RunCDIHook(context.Background(), opts, ociStateStdin(t, t.TempDir())); err != nil {
		t.Fatalf("RunCDIHook: %v", err)
	}
	if fixture.lastBody["hostname"] != "thunder-a6000-test-thunder-gpu-test-pod" {
		t.Fatalf("enrolled hostname = %v, want the pod name", fixture.lastBody["hostname"])
	}
}

// A claim nothing has reserved still enrolls, under the claim name.
func TestThunderClientNameFallsBackToTheClaim(t *testing.T) {
	withPod := Allocation{ClaimName: "claim-a-gpu-xhjwv", Consumer: ResourceConsumer{Name: "my-pod", Resource: "pods"}}
	if got := thunderClientName(withPod); got != "my-pod" {
		t.Fatalf("thunderClientName = %q, want my-pod", got)
	}
	unreserved := Allocation{ClaimName: "claim-a-gpu-xhjwv"}
	if got := thunderClientName(unreserved); got != "claim-a-gpu-xhjwv" {
		t.Fatalf("thunderClientName = %q, want the claim name", got)
	}
}

// A client and the enrollment token it was exchanged for are separate objects
// in Thunder, so teardown revokes both. A client left behind would keep
// counting against the zone's capacity after its pod is gone.
func TestUnprepareRevokesTheClientTheHookEnrolled(t *testing.T) {
	specDir := t.TempDir()
	store := NewFileCDIDeviceStore(specDir)
	store.StateDir = t.TempDir()
	store.ClientInstallCommand = "install"
	store.HookPath = "/plugins/bin/thunder-cdi-hook"

	allocation := Allocation{
		ClaimUID: "33333333-3333-3333-3333-333333333333", ClaimNamespace: "default",
		ClaimName: "claim-c", GPUType: "A6000", GPUCount: 1,
	}
	qualifiedName, err := store.Create(context.Background(), allocation, "tr_secret")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// No container has started yet, so no client exists to revoke.
	if got := store.StagedClientID(qualifiedName); got != "" {
		t.Fatalf("StagedClientID before any container = %q, want empty", got)
	}

	// The hook caches the exchanged config when a container starts.
	stateDir := store.stateDir(cdiDeviceName(allocation.ClaimUID))
	encoded, err := json.Marshal(ThunderClientConfig{ClientID: "client-1", DeviceID: "client-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, thunderConfigFile), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := store.StagedClientID(qualifiedName); got != "client-1" {
		t.Fatalf("StagedClientID = %q, want client-1", got)
	}

	// And it is gone once the claim is torn down.
	if err := store.Remove(context.Background(), qualifiedName); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := store.StagedClientID(qualifiedName); got != "" {
		t.Fatalf("StagedClientID after Remove = %q, want empty", got)
	}
}

// A burst of pods produces a burst of exchanges. A rate-limited response must
// cost a retry, not a container.
func TestExchangeRetriesRateLimitsAndServerFaults(t *testing.T) {
	for name, status := range map[string]int{
		"rate limited": http.StatusTooManyRequests,
		"server fault": http.StatusBadGateway,
	} {
		t.Run(name, func(t *testing.T) {
			attempts := 0
			mux := http.NewServeMux()
			mux.HandleFunc("/install.sh", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, installerFixture("http://"+r.Host, sha256Hex([]byte("lib"))))
			})
			mux.HandleFunc("/libthunder.so", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("lib")) })
			mux.HandleFunc("/api/v1/enrollment-tokens/enroll", func(w http.ResponseWriter, r *http.Request) {
				attempts++
				if attempts < 3 {
					w.WriteHeader(status)
					return
				}
				writeJSON(t, w, map[string]any{
					"authToken": "auth-token-value",
					"jwt": clientJWT(t, map[string]any{
						"orgId": "org-1", "clientId": "client-1", "gpuType": "A6000", "gpuCount": 1,
					}),
				})
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			stateDir := filepath.Join(t.TempDir(), "claim-abc")
			stageToken(t, stateDir, "tr_secret")
			opts := CDIHookOptions{
				StateDir: stateDir, CacheDir: t.TempDir(),
				CentralURL: server.URL, InstallURL: server.URL + "/install.sh",
				CABundlePath: writeTestCABundle(t),
			}
			if err := RunCDIHook(context.Background(), opts, ociStateStdin(t, t.TempDir())); err != nil {
				t.Fatalf("RunCDIHook: %v", err)
			}
			if attempts != 3 {
				t.Fatalf("exchange attempts = %d, want 3 (two retried, one succeeded)", attempts)
			}
		})
	}
}

// A spent token is a verdict, not a blip. Retrying it only delays the real
// error while the container sits in creation.
func TestExchangeDoesNotRetryARejection(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":"enrollment token already used"}`)
	}))
	defer server.Close()

	_, err := ExchangeClientEnrollment(context.Background(), nil, server.URL, "", "tr_spent", "claim")
	if err == nil {
		t.Fatal("exchange succeeded against a rejecting API")
	}
	if isRetryable(err) {
		t.Fatalf("a 403 was classed retryable: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

// Concurrent hooks on a cold node must not each pull the library.
func TestLibthunderCacheDownloadsOnceUnderConcurrency(t *testing.T) {
	library := []byte("ELF libthunder payload")
	downloads := int64(0)
	mux := http.NewServeMux()
	mux.HandleFunc("/install.sh", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, installerFixture("http://"+r.Host, sha256Hex(library)))
	})
	mux.HandleFunc("/libthunder.so", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&downloads, 1)
		time.Sleep(50 * time.Millisecond) // widen the window a herd would race through
		w.Write(library)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	// Separate cache instances, as separate hook processes would be.
	dir := t.TempDir()
	var wg sync.WaitGroup
	paths := make([]string, 8)
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cache := &LibthunderCache{Dir: dir, InstallURL: server.URL + "/install.sh"}
			paths[i], errs[i] = cache.Ensure(context.Background())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Ensure %d: %v", i, err)
		}
		if paths[i] != paths[0] {
			t.Fatalf("Ensure %d returned %q, want %q", i, paths[i], paths[0])
		}
	}
	if got := atomic.LoadInt64(&downloads); got != 1 {
		t.Fatalf("downloads = %d, want 1 for 8 concurrent callers", got)
	}
}

// Teardown must wait for a hook that is mid-stage rather than deleting the
// token out from under it.
func TestRemoveWaitsForAnInFlightHook(t *testing.T) {
	store := NewFileCDIDeviceStore(t.TempDir())
	store.StateDir = t.TempDir()
	store.ClientInstallCommand = "install"

	allocation := Allocation{
		ClaimUID: "44444444-4444-4444-4444-444444444444", ClaimName: "claim-d", GPUType: "A6000", GPUCount: 1,
	}
	qualifiedName, err := store.Create(context.Background(), allocation, "tr_secret")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stateDir := store.stateDir(cdiDeviceName(allocation.ClaimUID))

	// Hold the claim lock as a staging hook would.
	unlock, err := lockStateDir(stateDir)
	if err != nil {
		t.Fatalf("lockStateDir: %v", err)
	}

	removed := make(chan error, 1)
	go func() { removed <- store.Remove(context.Background(), qualifiedName) }()

	select {
	case <-removed:
		t.Fatal("Remove deleted the claim state while a hook held the lock")
	case <-time.After(150 * time.Millisecond):
	}

	unlock()
	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("Remove: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Remove did not complete after the hook released the lock")
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("claim state survived Remove: %v", err)
	}
}

// Some images, virt-launcher among them, have /etc/ssl/certs as a symlink that
// does not resolve inside their own rootfs. Staging a CA bundle is best effort,
// so such an image still starts: the library and config are what matter, and a
// KubeVirt guest installs its own client and never reads this copy.
func TestRunCDIHookStartsContainersWhoseCertDirIsASymlink(t *testing.T) {
	fixture := newThunderFixture(t)
	stateDir := filepath.Join(t.TempDir(), "claim-abc")
	bundle := t.TempDir()
	stageToken(t, stateDir, "tr_secret")

	// Lay out the rootfs the way the failure was seen: /etc/ssl/certs is a
	// symlink whose target does not exist inside the container.
	rootfs := filepath.Join(bundle, "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "etc/ssl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/lib/ssl/certs", filepath.Join(rootfs, "etc/ssl/certs")); err != nil {
		t.Fatal(err)
	}

	if err := RunCDIHook(context.Background(), fixture.hookOptions(stateDir, t.TempDir()), ociStateStdin(t, bundle)); err != nil {
		t.Fatalf("RunCDIHook refused a container with a symlinked cert dir: %v", err)
	}

	// The library and config still land — those are what the hook is for.
	if _, err := os.Stat(filepath.Join(rootfs, ThunderGuestDir, "libthunder.so")); err != nil {
		t.Fatalf("libthunder.so was not staged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootfs, ThunderGuestDir, thunderConfigFile)); err != nil {
		t.Fatalf("config.json was not staged: %v", err)
	}
	// And the image's own symlink is untouched.
	info, err := os.Lstat(filepath.Join(rootfs, "etc/ssl/certs"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the image's /etc/ssl/certs symlink was replaced")
	}
}

// A node with no trust store of its own must also not block containers.
func TestRunCDIHookStartsContainersWhenTheNodeHasNoCABundle(t *testing.T) {
	fixture := newThunderFixture(t)
	stateDir := filepath.Join(t.TempDir(), "claim-abc")
	stageToken(t, stateDir, "tr_secret")

	opts := fixture.hookOptions(stateDir, t.TempDir())
	opts.CABundlePath = filepath.Join(t.TempDir(), "no-such-bundle.crt")
	if err := RunCDIHook(context.Background(), opts, ociStateStdin(t, t.TempDir())); err != nil {
		t.Fatalf("RunCDIHook failed with no CA bundle on the node: %v", err)
	}
}
