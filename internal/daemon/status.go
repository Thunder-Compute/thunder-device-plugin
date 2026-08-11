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

func logThunderStatus(prefix string, status thunderStatus) {
	log.Printf(
		"thunder status %s: healthy=%t service=%s active=%s subState=%s enabled=%s load=%s localApiHealthy=%t authTokenConfigured=%t warnings=%d diagnostics=%d recentLogs=%d",
		prefix,
		status.Healthy,
		status.Service.Service,
		status.Service.Active,
		status.Service.SubState,
		status.Service.Enabled,
		status.Service.Load,
		status.LocalAPI.Healthy,
		status.Config.AuthTokenConfigured,
		len(status.Warnings),
		len(status.Diagnostics),
		len(status.RecentLogs),
	)
	if status.LocalAPI.Error != "" {
		log.Printf("thunder status %s localApiError=%q", prefix, status.LocalAPI.Error)
	}
	if status.Config.Error != "" {
		log.Printf("thunder status %s configError=%q", prefix, status.Config.Error)
	}
	if len(status.Warnings) > 0 {
		log.Printf("thunder status %s warning=%q", prefix, status.Warnings[0])
	}
	if len(status.Diagnostics) > 0 {
		diagnostic := status.Diagnostics[0]
		log.Printf("thunder status %s diagnosticSeverity=%s diagnosticMessage=%q diagnosticAction=%q", prefix, diagnostic.Severity, diagnostic.Message, diagnostic.Action)
	}
}
