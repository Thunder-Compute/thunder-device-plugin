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

// NodeInfo is the subset of a Kubernetes Node the daemon needs before it can
// build in-cluster clients.
type NodeInfo struct {
	Labels map[string]string
	// InternalIP and ExternalIP come from status.addresses and are the
	// defaults for the advertised IP, in that order.
	InternalIP string
	ExternalIP string
}

type nodeInfoReader interface {
	Node(ctx context.Context, nodeName string) (NodeInfo, error)
}

type kubernetesNodeInfoReader struct{}

func (kubernetesNodeInfoReader) Node(ctx context.Context, nodeName string) (NodeInfo, error) {
	host, ok := os.LookupEnv("KUBERNETES_SERVICE_HOST")
	if !ok || strings.TrimSpace(host) == "" {
		return NodeInfo{}, errors.New("KUBERNETES_SERVICE_HOST is not set")
	}
	port := optionalEnv(os.LookupEnv, "KUBERNETES_SERVICE_PORT", "443")

	token, err := os.ReadFile(ServiceAccountToken)
	if err != nil {
		return NodeInfo{}, fmt.Errorf("read service account token: %w", err)
	}
	ca, err := os.ReadFile(ServiceAccountCA)
	if err != nil {
		return NodeInfo{}, fmt.Errorf("read service account CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return NodeInfo{}, errors.New("service account CA bundle did not contain a PEM certificate")
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	endpoint := "https://" + host + ":" + port + "/api/v1/nodes/" + url.PathEscape(nodeName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return NodeInfo{}, fmt.Errorf("create node request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return NodeInfo{}, fmt.Errorf("get node from kubernetes API: %w", err)
	}
	defer resp.Body.Close()

	const maxResponseSize = 10 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return NodeInfo{}, fmt.Errorf("read node response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return NodeInfo{}, fmt.Errorf("kubernetes API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	return decodeNodeInfo(body)
}

func decodeNodeInfo(body []byte) (NodeInfo, error) {
	var node struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Status struct {
			Addresses []struct {
				Type    string `json:"type"`
				Address string `json:"address"`
			} `json:"addresses"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &node); err != nil {
		return NodeInfo{}, fmt.Errorf("decode node response: %w", err)
	}

	info := NodeInfo{Labels: node.Metadata.Labels}
	for _, address := range node.Status.Addresses {
		value := strings.TrimSpace(address.Address)
		if value == "" {
			continue
		}
		switch address.Type {
		case "InternalIP":
			if info.InternalIP == "" {
				info.InternalIP = value
			}
		case "ExternalIP":
			if info.ExternalIP == "" {
				info.ExternalIP = value
			}
		}
	}
	return info, nil
}

// NodeIP is the address the daemon advertises when neither the environment nor
// a node label pins one.
func (n NodeInfo) NodeIP() string {
	return firstNonEmpty(n.InternalIP, n.ExternalIP)
}
