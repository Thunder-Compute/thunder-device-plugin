package daemon

import (
	"encoding/json"
	"errors"
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
			name: "local api is down",
			json: `{"healthy":false,"service":{"active":"active"},"localApi":{"healthy":false},"config":{"authTokenConfigured":true}}`,
			want: false,
		},
		{
			name: "service is down",
			json: `{"healthy":false,"service":{"active":"inactive","subState":"dead"},"localApi":{"healthy":false,"error":"socket missing"},"config":{"authTokenConfigured":true}}`,
			want: false,
		},
		{
			// thunderd serves its own local API, so an answer from it is the
			// evidence that thunderd is up. The systemd state is not read at
			// all: a health check that turned on the wording of a status
			// string is what reinstalled every working node in the first
			// place.
			name: "systemd state is not what decides",
			json: `{"healthy":false,"service":{"active":"whatever-systemd-calls-it"},"localApi":{"healthy":true},"config":{"authTokenConfigured":true}}`,
			want: true,
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

// The summary is what whoever reads the pod log sees, so it has to say which
// node state it is describing. Nothing is decided on it — see
// TestStatusKeyDistinguishesStatesWithoutReadingTheLog.
func TestThunderStatusSummaryDescribesTheState(t *testing.T) {
	var healthy, broken thunderStatus
	if err := json.Unmarshal([]byte(transientHealthyStatus), &healthy); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"healthy":false,"service":{"service":"thunderd.service","active":"failed","subState":"failed"},`+
		`"localApi":{"healthy":false,"error":"socket missing"},"config":{"authTokenConfigured":true},"warnings":["stale"]}`), &broken); err != nil {
		t.Fatalf("decode status: %v", err)
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

// Statuses are deduplicated for logging on their fields rather than on the line
// they produce, so a node that is fine reports it once without a log message
// ever becoming the thing that decides something.
func TestStatusKeyDistinguishesStatesWithoutReadingTheLog(t *testing.T) {
	var healthy, down thunderStatus
	if err := json.Unmarshal([]byte(transientHealthyStatus), &healthy); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"healthy":false,"service":{"active":"inactive","subState":"dead"},`+
		`"localApi":{"healthy":false},"config":{"authTokenConfigured":true}}`), &down); err != nil {
		t.Fatalf("decode status: %v", err)
	}

	if healthy.key() != healthy.key() {
		t.Fatal("the same status produced two different keys")
	}
	if healthy.key() == down.key() {
		t.Fatal("a healthy and a stopped node produced the same key")
	}
	if healthy.key() == unreadableStatusKey(errors.New("exit status 127")) {
		t.Fatal("a node that answered and a node that did not produced the same key")
	}
	if unreadableStatusKey(errors.New("exit status 127")) == unreadableStatusKey(errors.New("connection refused")) {
		t.Fatal("two different failures produced the same key")
	}
}
