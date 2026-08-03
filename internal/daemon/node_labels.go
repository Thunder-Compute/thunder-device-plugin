package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const (
	ServiceAccountToken = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	ServiceAccountCA    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

type nodeLabelReader interface {
	Labels(ctx context.Context, nodeName string) (map[string]string, error)
}

type kubernetesNodeLabelReader struct{}

func (kubernetesNodeLabelReader) Labels(ctx context.Context, nodeName string) (map[string]string, error) {
	host, ok := os.LookupEnv("KUBERNETES_SERVICE_HOST")
	if !ok || strings.TrimSpace(host) == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST is not set")
	}
	port := optionalEnv(os.LookupEnv, "KUBERNETES_SERVICE_PORT", "443")

	token, err := os.ReadFile(ServiceAccountToken)
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}
	ca, err := os.ReadFile(ServiceAccountCA)
	if err != nil {
		return nil, fmt.Errorf("read service account CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("service account CA bundle did not contain a PEM certificate")
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	endpoint := "https://" + host + ":" + port + "/api/v1/nodes/" + url.PathEscape(nodeName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create node request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get node from kubernetes API: %w", err)
	}
	defer resp.Body.Close()

	const maxResponseSize = 10 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read node response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("kubernetes API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var response struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode node response: %w", err)
	}
	return response.Metadata.Labels, nil
}
