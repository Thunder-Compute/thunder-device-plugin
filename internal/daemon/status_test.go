package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestThunderStatusNodeHealthy(t *testing.T) {
	tests := []struct {
		name string
		json string
		want bool
	}{
		{
			// The status a transiently installed thunderd reports while it is
			// working: `enabled` has no answer for a unit that has no unit
			// file, so `thunder status` calls the node unhealthy.
			name: "transient unit that is running",
			json: transientHealthyStatus,
			want: true,
		},
		{
			name: "thunder says healthy",
			json: `{"healthy":true}`,
			want: true,
		},
		{
			name: "service is down",
			json: `{"healthy":false,"service":{"active":"inactive"},"localApi":{"healthy":true},"config":{"authTokenConfigured":true}}`,
			want: false,
		},
		{
			name: "local api is down",
			json: `{"healthy":false,"service":{"active":"active"},"localApi":{"healthy":false},"config":{"authTokenConfigured":true}}`,
			want: false,
		},
		{
			name: "node is not enrolled",
			json: `{"healthy":false,"service":{"active":"active"},"localApi":{"healthy":true},"config":{"authTokenConfigured":false}}`,
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var status thunderStatus
			if err := json.Unmarshal([]byte(test.json), &status); err != nil {
				t.Fatalf("decode status: %v", err)
			}
			if got := status.nodeHealthy(); got != test.want {
				t.Fatalf("nodeHealthy() = %t, want %t", got, test.want)
			}
		})
	}
}

// The summary is what the reconciler compares between passes to decide whether
// a status is worth logging again, so two different states must not summarise
// the same way, and one state must summarise the same way twice.
func TestThunderStatusSummaryDescribesTheState(t *testing.T) {
	var healthy, broken thunderStatus
	if err := json.Unmarshal([]byte(transientHealthyStatus), &healthy); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"healthy":false,"service":{"service":"thunderd.service","active":"failed","subState":"failed"},`+
		`"localApi":{"healthy":false,"error":"socket missing"},"config":{"authTokenConfigured":true},"warnings":["stale"]}`), &broken); err != nil {
		t.Fatalf("decode status: %v", err)
	}

	if healthy.summary() != healthy.summary() {
		t.Fatal("the same status summarised two different ways")
	}
	if healthy.summary() == broken.summary() {
		t.Fatalf("a healthy and a failed node summarised the same way: %s", healthy.summary())
	}
	for _, want := range []string{"thunderd healthy", "thunderd.service", "active/running", "localApi=ok", "authToken=configured"} {
		if !strings.Contains(healthy.summary(), want) {
			t.Fatalf("healthy summary missing %q: %s", want, healthy.summary())
		}
	}
	for _, want := range []string{"thunderd unhealthy", "failed/failed", "localApi=down(socket missing)", "warnings=1"} {
		if !strings.Contains(broken.summary(), want) {
			t.Fatalf("failed summary missing %q: %s", want, broken.summary())
		}
	}
	// `enabled` says how thunderd was installed, not whether it works, and a
	// transient unit is never enabled.
	if strings.Contains(healthy.summary(), "enabled") {
		t.Fatalf("summary reports enabled, which is meaningless for a transient unit: %s", healthy.summary())
	}
}
