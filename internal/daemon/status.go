package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

type thunderStatus struct {
	Service struct {
		Service  string `json:"service"`
		Active   string `json:"active"`
		Enabled  string `json:"enabled"`
		Load     string `json:"load"`
		SubState string `json:"subState"`
	} `json:"service"`
	LocalAPI struct {
		Healthy bool   `json:"healthy"`
		Error   string `json:"error"`
	} `json:"localApi"`
	Config struct {
		EnvPath             string `json:"envPath"`
		AuthTokenConfigured bool   `json:"authTokenConfigured"`
		Error               string `json:"error"`
	} `json:"config"`
	Healthy     bool     `json:"healthy"`
	Warnings    []string `json:"warnings"`
	Diagnostics []struct {
		Severity string `json:"severity"`
		Message  string `json:"message"`
		Action   string `json:"action"`
	} `json:"diagnostics"`
	RecentLogs []string `json:"recentLogs"`
}

func getThunderStatus(ctx context.Context, runner commandRunner) (thunderStatus, error) {
	output, commandErr := runner.CombinedOutput(ctx, "thunder", "status", "--json")

	var status thunderStatus
	if err := json.Unmarshal(output, &status); err != nil {
		if commandErr != nil {
			return thunderStatus{}, fmt.Errorf("run thunder status --json: %w: %s", commandErr, strings.TrimSpace(string(output)))
		}
		return thunderStatus{}, fmt.Errorf("decode thunder status --json: %w", err)
	}
	return status, nil
}

// nodeHealthy reports whether thunderd is doing on this node what the daemon
// needs it to do: answering its local API, and holding the auth token Thunder
// issued it. Both are booleans `thunder status` reports from probing the node,
// not descriptions of it — nothing here is decided by matching a status string,
// because a wording change on the other side of that would silently reinstall
// every node in the fleet.
//
// It is deliberately not just status.Healthy. `thunder status` computes that
// field for a thunderd installed as a unit file, where part of being healthy is
// being enabled, so systemd brings the service back after a reboot. The daemon
// installs thunderd as a transient unit on purpose (see thunderdTransientEnv):
// the DaemonSet is what brings it back, and a transient unit is never
// `enabled`, so it reports enabled=unknown and healthy=false forever. Taking
// that at face value made every working node look broken, and the daemon
// reinstalled thunderd on every pass — re-downloading the CLI and burning a
// fresh enrollment token each time.
//
// The systemd state is not consulted either, and does not need to be: the local
// API is served by thunderd itself, so an answer from it is proof the service
// is up that no systemd state string can add to.
func (s thunderStatus) nodeHealthy() bool {
	if s.Healthy {
		return true
	}
	return s.LocalAPI.Healthy && s.enrolled()
}

// enrolled reports whether the node already holds a Thunder auth token. An
// enrolled node that is merely down needs starting, not enrolling again.
func (s thunderStatus) enrolled() bool {
	return s.Config.AuthTokenConfigured
}

// statusKey is a status reduced to a comparable value, so the reconciler can
// tell one pass's status from the last one's. Passes are deduplicated by
// comparing these rather than the lines logged from them: what a log says must
// never be what decides anything.
type statusKey struct {
	// failure is why the status could not be read, and is empty when it was.
	failure     string
	healthy     bool
	localAPI    bool
	enrolled    bool
	active      string
	subState    string
	warnings    int
	diagnostics int
}

func (s thunderStatus) key() statusKey {
	return statusKey{
		healthy:     s.nodeHealthy(),
		localAPI:    s.LocalAPI.Healthy,
		enrolled:    s.enrolled(),
		active:      s.Service.Active,
		subState:    s.Service.SubState,
		warnings:    len(s.Warnings),
		diagnostics: len(s.Diagnostics),
	}
}

// unreadableStatusKey is the key for a status the node would not report.
func unreadableStatusKey(err error) statusKey {
	return statusKey{failure: err.Error()}
}

// summary describes a status in the terms the daemon decides on. It is written
// for whoever reads the pod log; the decisions themselves are made on the
// fields, not on this.
//
// `enabled` is not in it: a transient unit is never enabled, so the field says
// nothing about this node beyond how it was installed. The systemd state is,
// because it is worth reading even though nothing is decided by it.
func (s thunderStatus) summary() string {
	state := "unhealthy"
	if s.nodeHealthy() {
		state = "healthy"
	}
	localAPI := "ok"
	if !s.LocalAPI.Healthy {
		localAPI = "down"
		if message := strings.TrimSpace(s.LocalAPI.Error); message != "" {
			localAPI = fmt.Sprintf("down(%s)", message)
		}
	}
	authToken := "missing"
	if s.enrolled() {
		authToken = "configured"
	}

	summary := fmt.Sprintf("thunderd %s: service=%s(%s/%s) localApi=%s authToken=%s",
		state,
		orUnknown(s.Service.Service),
		orUnknown(s.Service.Active),
		orUnknown(s.Service.SubState),
		localAPI,
		authToken,
	)
	if len(s.Warnings) > 0 {
		summary += fmt.Sprintf(" warnings=%d", len(s.Warnings))
	}
	if len(s.Diagnostics) > 0 {
		summary += fmt.Sprintf(" diagnostics=%d", len(s.Diagnostics))
	}
	return summary
}

func orUnknown(value string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return "unknown"
}

// logThunderStatus logs a status and the first problem reported behind it.
// Callers log a status only when it changed, so the detail is worth printing
// whenever this runs.
func logThunderStatus(status thunderStatus) {
	log.Print(status.summary())
	if status.Config.Error != "" {
		log.Printf("thunderd config error: %s", status.Config.Error)
	}
	if len(status.Warnings) > 0 {
		log.Printf("thunderd warning: %s", status.Warnings[0])
	}
	if len(status.Diagnostics) > 0 {
		diagnostic := status.Diagnostics[0]
		log.Printf("thunderd diagnostic (%s): %s; %s", diagnostic.Severity, diagnostic.Message, diagnostic.Action)
	}
}
