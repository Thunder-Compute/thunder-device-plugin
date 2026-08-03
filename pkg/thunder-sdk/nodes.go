package thunder

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type CreateNodeEnrollmentRequest struct {
	ZoneID           string `json:"zoneId"`
	ExpiresInSeconds int64  `json:"expiresInSeconds,omitempty"`
}

type NodeEnrollmentCommandRequest struct {
	EnrollmentToken string
	IP              string
	Zone            string
	PortRange       string
	NodeName        string
}

type Node struct {
	HostID          string     `json:"hostId"`
	ZoneID          string     `json:"zoneId"`
	DisplayName     string     `json:"displayName"`
	Hostname        string     `json:"hostname"`
	ThunderdVersion *string    `json:"thunderdVersion,omitempty"`
	GPUType         string     `json:"gpuType"`
	GPUCount        uint32     `json:"gpuCount"`
	Status          string     `json:"status"`
	LastSeenAt      *time.Time `json:"lastSeenAt,omitempty"`
}

type HostNode = Node

type RevokeNodeResponse struct {
	HostID    string    `json:"hostId"`
	RevokedAt time.Time `json:"revokedAt"`
}

type RevokeHostResponse = RevokeNodeResponse

func (c *Client) CreateNodeEnrollment(ctx context.Context, req CreateNodeEnrollmentRequest) (EnrollmentToken, error) {
	body := struct {
		ZoneID           string `json:"zoneId"`
		Role             string `json:"role"`
		ExpiresInSeconds int64  `json:"expiresInSeconds,omitempty"`
	}{ZoneID: req.ZoneID, Role: RoleServer, ExpiresInSeconds: req.ExpiresInSeconds}
	return c.createEnrollment(ctx, body)
}

func (c *Client) EnrollNode(ctx context.Context, req CreateNodeEnrollmentRequest) (EnrollmentToken, error) {
	return c.CreateNodeEnrollment(ctx, req)
}

func (c *Client) UnenrollNode(ctx context.Context, enrollmentTokenID string) (DeleteEnrollmentNodeResponse, error) {
	return c.DeleteEnrollmentNode(ctx, enrollmentTokenID)
}

func (c *Client) NodeEnrollmentCommand(req NodeEnrollmentCommandRequest) string {
	return nodeEnrollmentCommand(c.installURL, c.baseURL, req)
}

func nodeEnrollmentCommand(installURL, centralURL string, req NodeEnrollmentCommandRequest) string {
	env := []string{
		"THUNDER_INSTALL_MODE=thunderd",
		"THUNDER_CENTRAL_URL=" + shellQuote(centralURL),
		"THUNDER_ENROLLMENT_TOKEN=" + shellQuote(req.EnrollmentToken),
	}
	if strings.TrimSpace(req.IP) != "" {
		env = append(env, "THUNDERD_IP="+shellQuote(req.IP))
	}
	if strings.TrimSpace(req.Zone) != "" {
		env = append(env, "THUNDER_ZONE="+shellQuote(req.Zone))
	}
	if strings.TrimSpace(req.PortRange) != "" {
		env = append(env, "THUNDERD_PORT_RANGE="+shellQuote(req.PortRange))
	}
	if strings.TrimSpace(req.NodeName) != "" {
		env = append(env, "THUNDERD_NODE_NAME="+shellQuote(req.NodeName))
	}
	return fmt.Sprintf("curl -fsSL %s | sudo %s sh", shellQuote(installURL), strings.Join(env, " "))
}

func (c *Client) ListNodes(ctx context.Context, zoneID string) ([]Node, error) {
	return c.ListHosts(ctx, zoneID)
}

func (c *Client) ListHosts(ctx context.Context, zoneID string) ([]Node, error) {
	var response struct {
		Hosts []Node `json:"hosts"`
	}
	path := "/api/v1/hosts?zoneId=" + url.QueryEscape(zoneID)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response.Hosts, nil
}

func (c *Client) RevokeNode(ctx context.Context, hostID string) (RevokeNodeResponse, error) {
	return c.RevokeHost(ctx, hostID)
}

func (c *Client) RevokeHost(ctx context.Context, hostID string) (RevokeNodeResponse, error) {
	var response RevokeNodeResponse
	path := "/api/v1/hosts/" + url.PathEscape(hostID) + "/revoke"
	if err := c.doJSON(ctx, http.MethodPost, path, nil, &response); err != nil {
		return RevokeNodeResponse{}, err
	}
	return response, nil
}
