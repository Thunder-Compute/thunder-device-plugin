package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ThunderClientConfig is /etc/thunder/config.json, the file libthunder.so reads
// to learn which client it is and which control plane to talk to. The field set
// and JSON shape mirror what the Thunder installer writes, so a container
// staged by the CDI hook is indistinguishable from one set up by `curl | sh`.
type ThunderClientConfig struct {
	DeviceID                 string `json:"deviceId"`
	ClientID                 string `json:"clientId"`
	OrgID                    string `json:"orgId"`
	GPUType                  string `json:"gpuType"`
	GPUCount                 int    `json:"gpuCount"`
	CentralAPIURL            string `json:"centralApiUrl"`
	AuthToken                string `json:"authToken"`
	Claims                   string `json:"claims"`
	EnableGRPCTLS            bool   `json:"enableGrpcTls"`
	ThunderdDiscoveryEnabled bool   `json:"thunderdDiscoveryEnabled"`
	TelemetryCollector       string `json:"telemetryCollector"`
}

// clientEnrollmentResponse is what POST /api/v1/enrollment-tokens/enroll
// returns: a long-lived auth token plus a JWT carrying the client identity.
type clientEnrollmentResponse struct {
	AuthToken string `json:"authToken"`
	JWT       string `json:"jwt"`
}

// clientClaims is the JWT payload of that response.
type clientClaims struct {
	OrgID    string `json:"orgId"`
	ClientID string `json:"clientId"`
	GPUType  string `json:"gpuType"`
	GPUCount int    `json:"gpuCount"`
}

// ExchangeClientEnrollment spends a single-use client enrollment token and
// returns the config the container needs. The token is consumed by this call,
// which is why it happens once per claim and is cached afterwards.
func ExchangeClientEnrollment(ctx context.Context, httpClient *http.Client, centralURL, telemetryURL, enrollmentToken, hostname string) (ThunderClientConfig, error) {
	if strings.TrimSpace(enrollmentToken) == "" {
		return ThunderClientConfig{}, fmt.Errorf("enrollment token is required")
	}
	if strings.TrimSpace(centralURL) == "" {
		return ThunderClientConfig{}, fmt.Errorf("central URL is required")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	body := []byte("{}")
	if strings.TrimSpace(hostname) != "" {
		encoded, err := json.Marshal(map[string]string{"hostname": hostname})
		if err != nil {
			return ThunderClientConfig{}, fmt.Errorf("encode enrollment body: %w", err)
		}
		body = encoded
	}

	endpoint := strings.TrimSuffix(centralURL, "/") + "/api/v1/enrollment-tokens/enroll"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ThunderClientConfig{}, fmt.Errorf("build enrollment request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+enrollmentToken)
	request.Header.Set("Content-Type", "application/json")

	response, err := httpClient.Do(request)
	if err != nil {
		return ThunderClientConfig{}, fmt.Errorf("exchange client enrollment: %w", err)
	}
	defer response.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(response.Body, installerReadLimit))
	if err != nil {
		return ThunderClientConfig{}, fmt.Errorf("read enrollment response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return ThunderClientConfig{}, fmt.Errorf("exchange client enrollment: status %s: %s",
			response.Status, strings.TrimSpace(string(payload)))
	}

	var enrolled clientEnrollmentResponse
	if err := json.Unmarshal(payload, &enrolled); err != nil {
		return ThunderClientConfig{}, fmt.Errorf("decode enrollment response: %w", err)
	}
	if enrolled.AuthToken == "" {
		return ThunderClientConfig{}, fmt.Errorf("enrollment response is missing authToken")
	}
	if enrolled.JWT == "" {
		return ThunderClientConfig{}, fmt.Errorf("enrollment response is missing jwt")
	}

	claims, err := decodeClientClaims(enrolled.JWT)
	if err != nil {
		return ThunderClientConfig{}, err
	}

	return ThunderClientConfig{
		DeviceID:                 claims.ClientID,
		ClientID:                 claims.ClientID,
		OrgID:                    claims.OrgID,
		GPUType:                  claims.GPUType,
		GPUCount:                 claims.GPUCount,
		CentralAPIURL:            strings.TrimSuffix(centralURL, "/"),
		AuthToken:                enrolled.AuthToken,
		Claims:                   enrolled.JWT,
		EnableGRPCTLS:            false,
		ThunderdDiscoveryEnabled: true,
		TelemetryCollector:       telemetryURL,
	}, nil
}

// decodeClientClaims reads the JWT payload. The signature is deliberately not
// verified here: the token arrived over TLS from an authenticated call to the
// Thunder API, and libthunder.so presents it back to that same API, which does
// verify it. Checking it here would need the signing key and prove nothing the
// transport has not already established.
func decodeClientClaims(token string) (clientClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return clientClaims{}, fmt.Errorf("client claims JWT is malformed")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return clientClaims{}, fmt.Errorf("client claims JWT payload could not be decoded: %w", err)
	}

	var claims clientClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return clientClaims{}, fmt.Errorf("client claims JWT payload is malformed: %w", err)
	}
	for field, value := range map[string]string{
		"orgId":    claims.OrgID,
		"clientId": claims.ClientID,
		"gpuType":  claims.GPUType,
	} {
		if strings.TrimSpace(value) == "" {
			return clientClaims{}, fmt.Errorf("client claims JWT is missing %s", field)
		}
	}
	if claims.GPUCount <= 0 {
		return clientClaims{}, fmt.Errorf("client claims JWT is missing gpuCount")
	}
	return claims, nil
}
