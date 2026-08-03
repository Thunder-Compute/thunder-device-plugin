package thunder

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

type EnrollmentToken struct {
	EnrollmentTokenID string     `json:"enrollmentTokenId"`
	EnrollmentToken   string     `json:"enrollmentToken"`
	OrgID             string     `json:"orgId"`
	ZoneID            string     `json:"zoneId,omitempty"`
	Role              string     `json:"role"`
	GPUType           string     `json:"gpuType,omitempty"`
	GPUCount          uint32     `json:"gpuCount,omitempty"`
	ExpiresAt         *time.Time `json:"expiresAt,omitempty"`
}

type DeleteEnrollmentNodeResponse struct {
	EnrollmentTokenID string    `json:"enrollmentTokenId"`
	Role              string    `json:"role"`
	ClientID          string    `json:"clientId,omitempty"`
	HostID            string    `json:"hostId,omitempty"`
	NodeDeleted       bool      `json:"nodeDeleted"`
	DeletedAt         time.Time `json:"deletedAt"`
}

func (c *Client) createEnrollment(ctx context.Context, body any) (EnrollmentToken, error) {
	var response EnrollmentToken
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/enrollment-tokens", body, &response); err != nil {
		return EnrollmentToken{}, err
	}
	return response, nil
}

func (c *Client) DeleteEnrollmentNode(ctx context.Context, enrollmentTokenID string) (DeleteEnrollmentNodeResponse, error) {
	var response DeleteEnrollmentNodeResponse
	path := "/api/v1/enrollment-tokens/" + url.PathEscape(enrollmentTokenID) + "/node"
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, &response); err != nil {
		return DeleteEnrollmentNodeResponse{}, err
	}
	return response, nil
}
