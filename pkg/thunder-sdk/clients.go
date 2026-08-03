package thunder

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type CreateClientEnrollmentRequest struct {
	ZoneID           string `json:"zoneId"`
	GPUType          string `json:"gpuType"`
	GPUCount         uint64 `json:"gpuCount"`
	ExpiresInSeconds int64  `json:"expiresInSeconds,omitempty"`
}

type ClientEnrollmentCommandRequest struct {
	EnrollmentToken    string
	EnrollmentTokenEnv string
}

type ClientNode struct {
	ClientID         string     `json:"clientId"`
	ZoneID           string     `json:"zoneId"`
	DisplayName      string     `json:"displayName"`
	GPUType          string     `json:"gpuType"`
	GPUCount         uint32     `json:"gpuCount"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	LastSeenAt       *time.Time `json:"lastSeenAt,omitempty"`
	DecommissionedAt *time.Time `json:"decommissionedAt,omitempty"`
}

type RevokeClientResponse struct {
	ClientID         string    `json:"clientId"`
	DecommissionedAt time.Time `json:"decommissionedAt"`
}

func (c *Client) CreateClientEnrollment(ctx context.Context, req CreateClientEnrollmentRequest) (EnrollmentToken, error) {
	body := struct {
		ZoneID           string `json:"zoneId"`
		Role             string `json:"role"`
		GPUType          string `json:"gpuType"`
		GPUCount         uint64 `json:"gpuCount"`
		ExpiresInSeconds int64  `json:"expiresInSeconds,omitempty"`
	}{ZoneID: req.ZoneID, Role: RoleClient, GPUType: req.GPUType, GPUCount: req.GPUCount, ExpiresInSeconds: req.ExpiresInSeconds}
	return c.createEnrollment(ctx, body)
}

func (c *Client) EnrollClient(ctx context.Context, req CreateClientEnrollmentRequest) (EnrollmentToken, error) {
	return c.CreateClientEnrollment(ctx, req)
}

func (c *Client) UnenrollClient(ctx context.Context, enrollmentTokenID string) (DeleteEnrollmentNodeResponse, error) {
	return c.DeleteEnrollmentNode(ctx, enrollmentTokenID)
}

func (c *Client) ClientEnrollmentCommand(enrollmentToken string) string {
	return c.ClientEnrollmentCommandFor(ClientEnrollmentCommandRequest{EnrollmentToken: enrollmentToken})
}

func (c *Client) ClientEnrollmentCommandFor(req ClientEnrollmentCommandRequest) string {
	return clientEnrollmentCommand(c.installURL, c.baseURL, req)
}

func clientEnrollmentCommand(installURL, centralURL string, req ClientEnrollmentCommandRequest) string {
	enrollmentToken := "THUNDER_ENROLLMENT_TOKEN=" + shellQuote(req.EnrollmentToken)
	if req.EnrollmentTokenEnv != "" {
		enrollmentToken = "THUNDER_ENROLLMENT_TOKEN=\"${" + req.EnrollmentTokenEnv + "}\""
	}
	return fmt.Sprintf("curl -fsSL %s | sudo THUNDER_INSTALL_MODE=client THUNDER_NOWARN=1 THUNDER_CENTRAL_URL=%s %s sh",
		shellQuote(installURL), shellQuote(centralURL), enrollmentToken)
}

func (c *Client) ListClients(ctx context.Context, zoneID string) ([]ClientNode, error) {
	var response struct {
		Clients []ClientNode `json:"clients"`
	}
	path := "/api/v1/clients?zoneId=" + url.QueryEscape(zoneID)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response.Clients, nil
}

func (c *Client) RevokeClient(ctx context.Context, clientID string) (RevokeClientResponse, error) {
	var response RevokeClientResponse
	path := "/api/v1/clients/" + url.PathEscape(clientID) + "/revoke"
	if err := c.doJSON(ctx, http.MethodPost, path, nil, &response); err != nil {
		return RevokeClientResponse{}, err
	}
	return response, nil
}
