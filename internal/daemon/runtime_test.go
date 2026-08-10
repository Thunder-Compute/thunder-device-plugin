package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	thunder "github.com/Thunder-Compute/thunder-sdk"
)

func TestConfigFromLookup(t *testing.T) {
	env := map[string]string{
		EnvNode:               "node-a",
		EnvZone:               "us-east-1a",
		EnvAdvertisedIP:       "203.0.113.10",
		EnvMinNVDriverVersion: "535.104.05",
		EnvThunderAPIToken:    "token",
	}

	cfg, err := configFromLookup(mapLookup(env))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if cfg.Node != "node-a" {
		t.Fatalf("Node = %q, want node-a", cfg.Node)
	}
	if cfg.Zone != "us-east-1a" {
		t.Fatalf("Zone = %q, want us-east-1a", cfg.Zone)
	}
	if cfg.AdvertisedIP != "203.0.113.10" {
		t.Fatalf("AdvertisedIP = %q, want 203.0.113.10", cfg.AdvertisedIP)
	}
	if cfg.HostRoot != DefaultHostRoot {
		t.Fatalf("HostRoot = %q, want %q", cfg.HostRoot, DefaultHostRoot)
	}
}

func TestConfigFromLookupAllowsNodeLabelFallbacks(t *testing.T) {
	env := map[string]string{
		EnvNode:               "node-a",
		EnvMinNVDriverVersion: "535.104.05",
		EnvThunderAPIToken:    "token",
	}

	cfg, err := configFromLookup(mapLookup(env))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Zone != "" {
		t.Fatalf("Zone = %q, want empty", cfg.Zone)
	}
	if cfg.AdvertisedIP != "" {
		t.Fatalf("AdvertisedIP = %q, want empty", cfg.AdvertisedIP)
	}
	if cfg.ZoneLabel != DefaultZoneLabel {
		t.Fatalf("ZoneLabel = %q, want %q", cfg.ZoneLabel, DefaultZoneLabel)
	}
	if cfg.AdvertisedIPLabel != DefaultAdvertisedIPLabel {
		t.Fatalf("AdvertisedIPLabel = %q, want %q", cfg.AdvertisedIPLabel, DefaultAdvertisedIPLabel)
	}
}

func TestResolveNodeAttributesFromLabels(t *testing.T) {
	cfg := Config{
		Node:              "node-a",
		ZoneLabel:         DefaultZoneLabel,
		AdvertisedIPLabel: DefaultAdvertisedIPLabel,
	}
	reader := &fakeNodeInfoReader{node: NodeInfo{
		Labels: map[string]string{
			DefaultZoneLabel:         "us-east-1a",
			DefaultAdvertisedIPLabel: "203.0.113.10",
		},
		InternalIP: "10.0.0.5",
	}}

	cfg, err := resolveNodeAttributes(context.Background(), cfg, reader)
	if err != nil {
		t.Fatalf("resolveNodeAttributes: %v", err)
	}
	if cfg.Zone != "us-east-1a" {
		t.Fatalf("Zone = %q, want us-east-1a", cfg.Zone)
	}
	if cfg.AdvertisedIP != "203.0.113.10" {
		t.Fatalf("AdvertisedIP = %q, want 203.0.113.10", cfg.AdvertisedIP)
	}
	if reader.nodeName != "node-a" {
		t.Fatalf("nodeName = %q, want node-a", reader.nodeName)
	}
}

func TestResolveNodeAttributesDefaultsAdvertisedIPToNodeIP(t *testing.T) {
	tests := []struct {
		name string
		node NodeInfo
		want string
	}{
		{
			name: "internal ip",
			node: NodeInfo{InternalIP: "10.0.0.5", ExternalIP: "203.0.113.10"},
			want: "10.0.0.5",
		},
		{
			name: "external ip when no internal ip",
			node: NodeInfo{ExternalIP: "203.0.113.10"},
			want: "203.0.113.10",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			node := test.node
			node.Labels = map[string]string{DefaultZoneLabel: "us-east-1a"}
			cfg := Config{
				Node:              "node-a",
				ZoneLabel:         DefaultZoneLabel,
				AdvertisedIPLabel: DefaultAdvertisedIPLabel,
			}

			cfg, err := resolveNodeAttributes(context.Background(), cfg, &fakeNodeInfoReader{node: node})
			if err != nil {
				t.Fatalf("resolveNodeAttributes: %v", err)
			}
			if cfg.AdvertisedIP != test.want {
				t.Fatalf("AdvertisedIP = %q, want %q", cfg.AdvertisedIP, test.want)
			}
		})
	}
}

func TestResolveNodeAttributesPrefersLabelOverNodeIP(t *testing.T) {
	cfg := Config{
		Node:              "node-a",
		ZoneLabel:         DefaultZoneLabel,
		AdvertisedIPLabel: DefaultAdvertisedIPLabel,
	}
	reader := &fakeNodeInfoReader{node: NodeInfo{
		Labels: map[string]string{
			DefaultZoneLabel:         "us-east-1a",
			DefaultAdvertisedIPLabel: "198.51.100.7",
		},
		InternalIP: "10.0.0.5",
	}}

	cfg, err := resolveNodeAttributes(context.Background(), cfg, reader)
	if err != nil {
		t.Fatalf("resolveNodeAttributes: %v", err)
	}
	if cfg.AdvertisedIP != "198.51.100.7" {
		t.Fatalf("AdvertisedIP = %q, want 198.51.100.7", cfg.AdvertisedIP)
	}
}

func TestResolveNodeAttributesRequiresAnAdvertisableIP(t *testing.T) {
	cfg := Config{
		Node:              "node-a",
		ZoneLabel:         DefaultZoneLabel,
		AdvertisedIPLabel: DefaultAdvertisedIPLabel,
	}
	reader := &fakeNodeInfoReader{node: NodeInfo{
		Labels: map[string]string{DefaultZoneLabel: "us-east-1a"},
	}}

	if _, err := resolveNodeAttributes(context.Background(), cfg, reader); err == nil {
		t.Fatal("resolveNodeAttributes succeeded without any advertisable IP")
	}
}

func TestResolveNodeAttributesKeepsEnvOverrides(t *testing.T) {
	cfg := Config{
		Node:              "node-a",
		Zone:              "env-zone",
		AdvertisedIP:      "203.0.113.20",
		ZoneLabel:         DefaultZoneLabel,
		AdvertisedIPLabel: DefaultAdvertisedIPLabel,
	}
	reader := &fakeNodeInfoReader{node: NodeInfo{
		Labels: map[string]string{
			DefaultZoneLabel:         "label-zone",
			DefaultAdvertisedIPLabel: "203.0.113.10",
		},
	}}

	cfg, err := resolveNodeAttributes(context.Background(), cfg, reader)
	if err != nil {
		t.Fatalf("resolveNodeAttributes: %v", err)
	}
	if cfg.Zone != "env-zone" {
		t.Fatalf("Zone = %q, want env-zone", cfg.Zone)
	}
	if cfg.AdvertisedIP != "203.0.113.20" {
		t.Fatalf("AdvertisedIP = %q, want 203.0.113.20", cfg.AdvertisedIP)
	}
	if reader.nodeName != "" {
		t.Fatalf("node reader was called with %q", reader.nodeName)
	}
}

func TestDecodeNodeInfo(t *testing.T) {
	body := []byte(`{
		"metadata": {"labels": {"topology.kubernetes.io/zone": "us-east-1a"}},
		"status": {"addresses": [
			{"type": "Hostname", "address": "node-a"},
			{"type": "InternalIP", "address": "10.0.0.5"},
			{"type": "InternalIP", "address": "10.0.0.6"},
			{"type": "ExternalIP", "address": "203.0.113.10"}
		]}
	}`)

	info, err := decodeNodeInfo(body)
	if err != nil {
		t.Fatalf("decodeNodeInfo: %v", err)
	}
	if info.Labels["topology.kubernetes.io/zone"] != "us-east-1a" {
		t.Fatalf("Labels = %#v", info.Labels)
	}
	if info.InternalIP != "10.0.0.5" {
		t.Fatalf("InternalIP = %q, want 10.0.0.5", info.InternalIP)
	}
	if info.ExternalIP != "203.0.113.10" {
		t.Fatalf("ExternalIP = %q, want 203.0.113.10", info.ExternalIP)
	}
	if info.NodeIP() != "10.0.0.5" {
		t.Fatalf("NodeIP() = %q, want 10.0.0.5", info.NodeIP())
	}
}

func TestEnsureThunderZoneUsesSmallestExistingMatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/zones" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"zones":[{"zoneId":"zone-b","displayName":"us-east-1a"},{"zoneId":"zone-a","displayName":"us-east-1a"},{"zoneId":"zone-0","displayName":"us-east-1b"}]}`))
	}))
	defer server.Close()

	zoneID, err := ensureThunderZone(context.Background(), thunder.NewClient(server.URL, "token"), "us-east-1a")
	if err != nil {
		t.Fatalf("ensureThunderZone: %v", err)
	}
	if zoneID != "zone-a" {
		t.Fatalf("zoneID = %q, want zone-a", zoneID)
	}
}

func TestEnsureThunderZoneCreatesThenRelistsAndUsesSmallestMatch(t *testing.T) {
	getCount := 0
	postCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/zones":
			getCount++
			if getCount == 1 {
				_, _ = w.Write([]byte(`{"zones":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"zones":[{"zoneId":"zone-c","displayName":"us-east-1a"},{"zoneId":"zone-a","displayName":"us-east-1a"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/zones/ensure":
			postCount++
			_, _ = w.Write([]byte(`{"zoneId":"zone-c","displayName":"us-east-1a"}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	zoneID, err := ensureThunderZone(context.Background(), thunder.NewClient(server.URL, "token"), "us-east-1a")
	if err != nil {
		t.Fatalf("ensureThunderZone: %v", err)
	}
	if zoneID != "zone-a" {
		t.Fatalf("zoneID = %q, want zone-a", zoneID)
	}
	if getCount != 2 {
		t.Fatalf("getCount = %d, want 2", getCount)
	}
	if postCount != 1 {
		t.Fatalf("postCount = %d, want 1", postCount)
	}
}

func TestEnsureThunderZoneRelistsAfterConflict(t *testing.T) {
	getCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/zones":
			getCount++
			if getCount == 1 {
				_, _ = w.Write([]byte(`{"zones":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"zones":[{"zoneId":"zone-a","displayName":"us-east-1a"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/zones/ensure":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"Conflict","message":"zone already exists"}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	zoneID, err := ensureThunderZone(context.Background(), thunder.NewClient(server.URL, "token"), "us-east-1a")
	if err != nil {
		t.Fatalf("ensureThunderZone: %v", err)
	}
	if zoneID != "zone-a" {
		t.Fatalf("zoneID = %q, want zone-a", zoneID)
	}
}

func TestGetThunderStatusParsesUnhealthyJSONWithNonzeroExit(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"thunder status --json": []byte(`{"service":{"service":"thunderd.service","active":"inactive","enabled":"unknown","load":"loaded","subState":"dead"},"localApi":{"healthy":false,"error":"socket missing"},"config":{"envPath":"/etc/thunder/thunderd.env","authTokenConfigured":false},"healthy":false,"warnings":["daemon state unavailable"],"diagnostics":[{"severity":"error","message":"socket missing","action":"run thunder up"}],"recentLogs":["line"]}`),
		},
		errors: map[string]error{
			"thunder status --json": errors.New("exit status 1"),
		},
	}

	status, err := getThunderStatus(context.Background(), runner)
	if err != nil {
		t.Fatalf("getThunderStatus: %v", err)
	}
	if status.Healthy {
		t.Fatal("Healthy = true, want false")
	}
	if status.Service.Active != "inactive" {
		t.Fatalf("Service.Active = %q, want inactive", status.Service.Active)
	}
	if status.LocalAPI.Error != "socket missing" {
		t.Fatalf("LocalAPI.Error = %q, want socket missing", status.LocalAPI.Error)
	}
	if len(status.Diagnostics) != 1 || status.Diagnostics[0].Severity != "error" {
		t.Fatalf("Diagnostics = %#v", status.Diagnostics)
	}
}

func TestGetThunderStatusRejectsInvalidJSON(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"thunder status --json": []byte("not json"),
		},
	}

	if _, err := getThunderStatus(context.Background(), runner); err == nil {
		t.Fatal("getThunderStatus succeeded with invalid JSON")
	}
}

func TestEnsureVersionRecency(t *testing.T) {
	tests := []struct {
		name       string
		version    string
		minVersion string
		wantErr    bool
	}{
		{name: "equal", version: "535.104.05", minVersion: "535.104.05"},
		{name: "newer major", version: "536.1.0", minVersion: "535.999.999"},
		{name: "newer patch", version: "535.104.06", minVersion: "535.104.05"},
		{name: "short actual", version: "535.104", minVersion: "535.104.0"},
		{name: "too old", version: "535.103.99", minVersion: "535.104.05", wantErr: true},
		{name: "bad minimum", version: "535.104.05", minVersion: "535.x", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ensureVersionRecency(test.version, test.minVersion)
			if (err != nil) != test.wantErr {
				t.Fatalf("ensureVersionRecency() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestResolveNodePath(t *testing.T) {
	tests := []struct {
		hostRoot string
		nodePath string
		want     string
	}{
		{hostRoot: "/host", nodePath: "/usr/bin/nvidia-smi", want: "/host/usr/bin/nvidia-smi"},
		{hostRoot: "/", nodePath: "/usr/bin/nvidia-smi", want: "/usr/bin/nvidia-smi"},
		{hostRoot: "", nodePath: "/usr/bin/nvidia-smi", want: "/usr/bin/nvidia-smi"},
	}

	for _, test := range tests {
		if got := resolveNodePath(test.hostRoot, test.nodePath); got != test.want {
			t.Fatalf("resolveNodePath(%q, %q) = %q, want %q", test.hostRoot, test.nodePath, got, test.want)
		}
	}
}

func TestNvidiaChecksUsesNodePathsAndNvidiaSMI(t *testing.T) {
	tempDir := t.TempDir()
	cfg := Config{
		HostRoot:         tempDir,
		LibCUDAPath:      "/libcuda.so.1",
		LibNVMLPath:      "/libnvidia-ml.so.1",
		NVSMIPath:        "/nvidia-smi",
		MinDriverVersion: "535.104.05",
	}
	for _, path := range []string{"libcuda.so.1", "libnvidia-ml.so.1", "nvidia-smi"} {
		touch(t, tempDir+"/"+path)
	}

	runner := &fakeRunner{
		outputs: map[string][]byte{
			"/nvidia-smi --query-gpu=driver_version --format=csv,noheader,nounits": []byte("535.104.05\n535.104.05\n"),
			"/nvidia-smi --query-gpu=index --format=csv,noheader,nounits":          []byte("0\n1\n"),
		},
	}
	count, driver, err := nvidiaChecks(context.Background(), cfg, runner)
	if err != nil {
		t.Fatalf("nvidiaChecks: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
	if driver != "535.104.05" {
		t.Fatalf("driver = %q, want 535.104.05", driver)
	}
	if !reflect.DeepEqual(runner.commands, []string{
		"/nvidia-smi --query-gpu=driver_version --format=csv,noheader,nounits",
		"/nvidia-smi --query-gpu=index --format=csv,noheader,nounits",
	}) {
		t.Fatalf("commands = %#v", runner.commands)
	}
}

type fakeRunner struct {
	outputs  map[string][]byte
	errors   map[string]error
	commands []string
}

func (r *fakeRunner) CombinedOutput(_ context.Context, name string, args ...string) ([]byte, error) {
	key := commandKey(name, args...)
	r.commands = append(r.commands, key)
	return r.outputs[key], r.errors[key]
}

// RunShell records the command so a test can assert on what the daemon asked
// the host to run, such as the Thunder installer and its environment.
func (r *fakeRunner) RunShell(_ context.Context, command string) error {
	r.commands = append(r.commands, command)
	return nil
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func commandKey(name string, args ...string) string {
	if len(args) == 0 {
		return name
	}
	return name + " " + strings.Join(args, " ")
}

type fakeNodeInfoReader struct {
	node     NodeInfo
	nodeName string
}

func (r *fakeNodeInfoReader) Node(_ context.Context, nodeName string) (NodeInfo, error) {
	r.nodeName = nodeName
	return r.node, nil
}
