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
// needs it to do: running, answering its local API, and holding the auth token
// Thunder issued it.
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
func (s thunderStatus) nodeHealthy() bool {
	if s.Healthy {
		return true
	}
	return s.serviceActive() && s.LocalAPI.Healthy && s.enrolled()
}

// serviceActive reports whether systemd has thunderd up.
func (s thunderStatus) serviceActive() bool {
	return strings.EqualFold(strings.TrimSpace(s.Service.Active), "active")
}

// enrolled reports whether the node already holds a Thunder auth token. An
// enrolled node that is merely down needs starting, not enrolling again.
func (s thunderStatus) enrolled() bool {
	return s.Config.AuthTokenConfigured
}

// summary describes a status in the terms the daemon decides on. The reconciler
// compares it between passes and logs only when it changes, so a node that is
// simply fine does not reprint the same line every ten seconds.
//
// `enabled` is not in it: a transient unit is never enabled, so the field says
// nothing about this node beyond how it was installed.
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
	if load := strings.TrimSpace(s.Service.Load); load != "" && !strings.EqualFold(load, "loaded") {
		summary += " load=" + load
	}
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
