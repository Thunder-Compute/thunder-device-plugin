package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	thunder "github.com/Thunder-Compute/thunder-sdk"
)

// recordedRequest is one call the daemon made to the Thunder API.
type recordedRequest struct {
	Method string
	Path   string
	Body   map[string]any
}

// recordingRegistry is a Thunder API that records what was written to it, so a
// test can assert on the requests the daemon makes rather than only on what it
// does with the responses.
type recordingRegistry struct {
	server   *httptest.Server
	requests []recordedRequest
	zones    []map[string]any
}

func newRecordingRegistry(t *testing.T) *recordingRegistry {
	t.Helper()
	registry := &recordingRegistry{}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/zones", func(w http.ResponseWriter, r *http.Request) {
		registry.record(t, r)
		writeJSON(t, w, map[string]any{"zones": registry.zones})
	})
	mux.HandleFunc("POST /api/v1/zones/ensure", func(w http.ResponseWriter, r *http.Request) {
		body := registry.record(t, r)
		name, _ := body["displayName"].(string)
		registry.zones = append(registry.zones, map[string]any{"zoneId": "zone-1", "displayName": name})
		writeJSON(t, w, map[string]any{"zoneId": "zone-1", "displayName": name})
	})
	mux.HandleFunc("POST /api/v1/enrollment-tokens", func(w http.ResponseWriter, r *http.Request) {
		body := registry.record(t, r)
		role, _ := body["role"].(string)
		writeJSON(t, w, map[string]any{
			"enrollmentTokenId": "token-id-" + role,
			"enrollmentToken":   "token-secret-" + role,
			"role":              role,
		})
	})
	mux.HandleFunc("DELETE /api/v1/enrollment-tokens/{id}/node", func(w http.ResponseWriter, r *http.Request) {
		registry.record(t, r)
		writeJSON(t, w, map[string]any{"enrollmentTokenId": r.PathValue("id"), "nodeDeleted": true})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		registry.record(t, r)
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusNotImplemented)
	})

	registry.server = httptest.NewServer(mux)
	t.Cleanup(registry.server.Close)
	return registry
}

func (r *recordingRegistry) record(t *testing.T, req *http.Request) map[string]any {
	t.Helper()
	recorded := recordedRequest{Method: req.Method, Path: req.URL.Path}
	if raw, err := io.ReadAll(req.Body); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &recorded.Body)
	}
	r.requests = append(r.requests, recorded)
	return recorded.Body
}

// writes returns only the requests that changed state.
func (r *recordingRegistry) writes() []recordedRequest {
	var out []recordedRequest
	for _, request := range r.requests {
		if request.Method != http.MethodGet {
			out = append(out, request)
		}
	}
	return out
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func TestEnsureThunderZoneCreatesTheZoneItWasAskedFor(t *testing.T) {
	registry := newRecordingRegistry(t)
	client := thunder.NewClient(registry.server.URL, "token")

	zoneID, err := ensureThunderZone(context.Background(), client, "us-west-2a")
	if err != nil {
		t.Fatalf("ensureThunderZone: %v", err)
	}
	if zoneID != "zone-1" {
		t.Fatalf("zoneID = %q, want zone-1", zoneID)
	}

	writes := registry.writes()
	if len(writes) != 1 {
		t.Fatalf("writes = %#v, want exactly one zone create", writes)
	}
	if writes[0].Method != http.MethodPost || writes[0].Path != "/api/v1/zones/ensure" {
		t.Fatalf("write = %s %s, want POST /api/v1/zones/ensure", writes[0].Method, writes[0].Path)
	}
	if got := writes[0].Body["displayName"]; got != "us-west-2a" {
		t.Fatalf("displayName = %v, want us-west-2a", got)
	}
}

func TestEnsureThunderZoneDoesNotCreateAnExistingZone(t *testing.T) {
	registry := newRecordingRegistry(t)
	registry.zones = []map[string]any{{"zoneId": "zone-existing", "displayName": "us-west-2a"}}

	zoneID, err := ensureThunderZone(context.Background(), thunder.NewClient(registry.server.URL, "token"), "us-west-2a")
	if err != nil {
		t.Fatalf("ensureThunderZone: %v", err)
	}
	if zoneID != "zone-existing" {
		t.Fatalf("zoneID = %q, want zone-existing", zoneID)
	}
	if writes := registry.writes(); len(writes) != 0 {
		t.Fatalf("writes = %#v, want none for an existing zone", writes)
	}
}

func TestThunderTokenIssuerMintRequestsAClientEnrollment(t *testing.T) {
	registry := newRecordingRegistry(t)
	issuer := ThunderTokenIssuer{
		Client: thunder.NewClient(registry.server.URL, "token"),
		ZoneID: "zone-1",
	}

	tokenID, token, _, err := issuer.Mint(context.Background(), Allocation{
		Zone:     "us-west-2a",
		GPUType:  "A6000",
		GPUCount: 2,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if tokenID != "token-id-client" || token != "token-secret-client" {
		t.Fatalf("Mint returned %q / %q", tokenID, token)
	}

	writes := registry.writes()
	if len(writes) != 1 || writes[0].Path != "/api/v1/enrollment-tokens" {
		t.Fatalf("writes = %#v", writes)
	}
	body := writes[0].Body
	// The claim's GPU model and count have to reach Thunder, or the client is
	// enrolled against the wrong hardware.
	if body["role"] != "client" {
		t.Fatalf("role = %v, want client", body["role"])
	}
	if body["zoneId"] != "zone-1" {
		t.Fatalf("zoneId = %v, want zone-1", body["zoneId"])
	}
	if body["gpuType"] != "A6000" {
		t.Fatalf("gpuType = %v, want A6000", body["gpuType"])
	}
	if body["gpuCount"] != float64(2) {
		t.Fatalf("gpuCount = %v, want 2", body["gpuCount"])
	}
}

func TestThunderTokenIssuerMintFallsBackToTheAllocationZone(t *testing.T) {
	registry := newRecordingRegistry(t)
	issuer := ThunderTokenIssuer{Client: thunder.NewClient(registry.server.URL, "token")}

	if _, _, _, err := issuer.Mint(context.Background(), Allocation{
		Zone:     "us-west-2b",
		GPUType:  "H100",
		GPUCount: 1,
	}); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got := registry.writes()[0].Body["zoneId"]; got != "us-west-2b" {
		t.Fatalf("zoneId = %v, want the allocation's zone", got)
	}
}

func TestThunderTokenIssuerRevokeDeletesTheEnrollment(t *testing.T) {
	registry := newRecordingRegistry(t)
	issuer := ThunderTokenIssuer{Client: thunder.NewClient(registry.server.URL, "token")}

	if err := issuer.Revoke(context.Background(), "token-id-1"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	writes := registry.writes()
	if len(writes) != 1 {
		t.Fatalf("writes = %#v", writes)
	}
	if writes[0].Method != http.MethodDelete || writes[0].Path != "/api/v1/enrollment-tokens/token-id-1/node" {
		t.Fatalf("write = %s %s", writes[0].Method, writes[0].Path)
	}
}

func TestThunderTokenIssuerRevokeIsQuietForAnEmptyOrMissingToken(t *testing.T) {
	registry := newRecordingRegistry(t)
	issuer := ThunderTokenIssuer{Client: thunder.NewClient(registry.server.URL, "token")}

	// Unprepare runs on paths where the token may never have been minted.
	if err := issuer.Revoke(context.Background(), "   "); err != nil {
		t.Fatalf("Revoke with an empty token: %v", err)
	}
	if writes := registry.writes(); len(writes) != 0 {
		t.Fatalf("writes = %#v, want none", writes)
	}

	// A token Thunder no longer knows about is already in the desired state.
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"NotFound"}`, http.StatusNotFound)
	}))
	defer gone.Close()
	missing := ThunderTokenIssuer{Client: thunder.NewClient(gone.URL, "token")}
	if err := missing.Revoke(context.Background(), "token-id-1"); err != nil {
		t.Fatalf("Revoke of an unknown token: %v, want nil", err)
	}
}

func TestRunEnrollsTheServerWithItsAdvertisedIPAndZone(t *testing.T) {
	registry := newRecordingRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hostRoot := t.TempDir()
	for _, name := range []string{"libcuda.so.1", "libnvidia-ml.so.1", "nvidia-smi"} {
		touch(t, hostRoot+"/"+name)
	}

	runner := &fakeRunner{
		outputs: map[string][]byte{
			"thunder status --json": []byte(`{"healthy":false}`),
			"/nvidia-smi --query-gpu=driver_version --format=csv,noheader,nounits": []byte("610.43.02\n"),
			"/nvidia-smi --query-gpu=index --format=csv,noheader,nounits":          []byte("0\n1\n"),
		},
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
		// DRA is served only after enrollment, and starting it needs an
		// in-cluster config this test does not have.
		DRAEnabled: false,
	}
	nodes := &fakeNodeInfoReader{node: NodeInfo{
		Labels:     map[string]string{DefaultZoneLabel: "us-west-2a"},
		InternalIP: "10.0.0.5",
	}}

	// run blocks monitoring Thunder once enrolled, so stop it after the
	// enrollment has happened.
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, runner, nodes) }()

	deadline := time.After(5 * time.Second)
	for {
		if len(registry.writes()) >= 2 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("run returned early: %v", err)
		case <-deadline:
			t.Fatalf("no enrollment after 5s; writes = %#v", registry.writes())
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()

	writes := registry.writes()
	// The zone is created because the registry starts empty, then the server
	// is enrolled into it.
	if writes[0].Path != "/api/v1/zones/ensure" || writes[0].Body["displayName"] != "us-west-2a" {
		t.Fatalf("first write = %#v, want the zone create", writes[0])
	}
	if writes[1].Path != "/api/v1/enrollment-tokens" {
		t.Fatalf("second write = %#v, want the server enrollment", writes[1])
	}
	if writes[1].Body["role"] != "server" {
		t.Fatalf("role = %v, want server", writes[1].Body["role"])
	}
	if writes[1].Body["zoneId"] != "zone-1" {
		t.Fatalf("zoneId = %v, want the zone just created", writes[1].Body["zoneId"])
	}

	// The installer has to be handed the address clients will reach this node
	// on, which defaulted to the node's own IP.
	var install string
	for _, command := range runner.commands {
		if strings.Contains(command, "THUNDER_INSTALL_MODE=thunderd") {
			install = command
		}
	}
	if install == "" {
		t.Fatalf("the installer was never run; commands = %#v", runner.commands)
	}
	for _, want := range []string{"token-secret-server", "10.0.0.5", "us-west-2a", "node-a"} {
		if !strings.Contains(install, want) {
			t.Fatalf("install command missing %q:\n%s", want, install)
		}
	}
}
