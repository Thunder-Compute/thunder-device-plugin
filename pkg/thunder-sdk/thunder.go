package thunder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultBaseURL    = "https://api.thundercompute.com:2096"
	DefaultInstallURL = "https://get.thundercompute.com/install.sh"

	RoleClient = "client"
	RoleServer = "server"

	CapabilityCreateClientEnrollmentToken = "client_enrollment_tokens:create"
	CapabilityCreateServerEnrollmentToken = "server_enrollment_tokens:create"
	CapabilityRevokeClient                = "clients:revoke"
	CapabilityRevokeHost                  = "hosts:revoke"
	CapabilityReadHosts                   = "hosts:read"
	CapabilityReadClients                 = "clients:read"
	CapabilityReadZones                   = "zones:read"
	CapabilityDeleteZone                  = "zones:delete"
	CapabilityCreateZone                  = "zones:create"
)

// Client is a typed SDK over Central's organization API-token bearer surface.
type Client struct {
	baseURL    string
	apiToken   string
	userAgent  string
	httpClient *http.Client
	installURL string
}

type Option func(*Client)

func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) {
		if httpClient != nil {
			c.httpClient = httpClient
		}
	}
}

func WithUserAgent(userAgent string) Option {
	return func(c *Client) {
		if strings.TrimSpace(userAgent) != "" {
			c.userAgent = strings.TrimSpace(userAgent)
		}
	}
}

func WithInstallURL(installURL string) Option {
	return func(c *Client) {
		if strings.TrimSpace(installURL) != "" {
			c.installURL = strings.TrimSpace(installURL)
		}
	}
}

func NewClient(baseURL, apiToken string, opts ...Option) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiToken:   strings.TrimSpace(apiToken),
		userAgent:  "thunder-sdk/dev",
		httpClient: &http.Client{Timeout: 30 * time.Second},
		installURL: DefaultInstallURL,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if c.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	const maxResponseSize = 10 << 20
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		apiErr := &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Status: resp.Status}
		_ = json.Unmarshal(responseBody, apiErr)
		if apiErr.ErrorType == "" {
			apiErr.ErrorType = http.StatusText(resp.StatusCode)
		}
		if apiErr.Message == "" && len(responseBody) > 0 {
			apiErr.Message = string(bytes.TrimSpace(responseBody))
			if len(apiErr.Message) > 200 {
				apiErr.Message = apiErr.Message[:200]
			}
		}
		return apiErr
	}
	if out == nil || len(responseBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(responseBody, out); err != nil {
		return fmt.Errorf("decode response body: %w", err)
	}
	return nil
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
