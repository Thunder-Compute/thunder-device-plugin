package thunder

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

type Zone struct {
	ZoneID      string    `json:"zoneId"`
	DisplayName string    `json:"displayName"`
	NodeCount   int       `json:"nodeCount"`
	ClientCount int       `json:"clientCount"`
	CreatedAt   time.Time `json:"createdAt"`
}

type CreateZoneRequest struct {
	DisplayName string `json:"displayName,omitempty"`
}

type CreateZoneResponse struct {
	ZoneID      string `json:"zoneId"`
	OrgID       string `json:"orgId,omitempty"`
	DisplayName string `json:"displayName"`
}

func (c *Client) ListZones(ctx context.Context) ([]Zone, error) {
	var response struct {
		Zones []Zone `json:"zones"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/zones", nil, &response); err != nil {
		return nil, err
	}
	return response.Zones, nil
}

func (c *Client) CreateZone(ctx context.Context, req CreateZoneRequest) (CreateZoneResponse, error) {
	var response CreateZoneResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/zones", req, &response); err != nil {
		return CreateZoneResponse{}, err
	}
	return response, nil
}

func (c *Client) DeleteZone(ctx context.Context, zoneID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/zones/"+url.PathEscape(zoneID), nil, nil)
}
