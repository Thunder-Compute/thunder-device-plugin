package thunder

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientSendsBearerAndJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/zones" {
			t.Fatalf("path = %s, want /api/v1/zones", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tcapi_test" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("Accept = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "test-sdk" {
			t.Fatalf("User-Agent = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if got := body["displayName"]; got != "prod" {
			t.Fatalf("displayName = %v, want prod", got)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"zoneId":"zone-1","orgId":"org-1","displayName":"prod"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL+"/", " tcapi_test ", WithHTTPClient(server.Client()), WithUserAgent("test-sdk"))
	zone, err := client.CreateZone(context.Background(), CreateZoneRequest{DisplayName: "prod"})
	if err != nil {
		t.Fatalf("CreateZone: %v", err)
	}
	if zone.ZoneID != "zone-1" || zone.OrgID != "org-1" || zone.DisplayName != "prod" {
		t.Fatalf("zone = %+v", zone)
	}
}

func TestEnrollmentRequests(t *testing.T) {
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/enrollment-tokens" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		call++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch call {
		case 1:
			if body["role"] != RoleClient || body["zoneId"] != "zone-1" || body["gpuType"] != "nvidia-l4" || body["gpuCount"] != float64(2) || body["expiresInSeconds"] != float64(3600) {
				t.Fatalf("client enrollment body = %#v", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"enrollmentTokenId":"et-client","enrollmentToken":"tr_client","orgId":"org-1","zoneId":"zone-1","role":"client","gpuType":"nvidia-l4","gpuCount":2}`))
		case 2:
			if body["role"] != RoleServer || body["zoneId"] != "auto" {
				t.Fatalf("node enrollment body = %#v", body)
			}
			if _, ok := body["gpuType"]; ok {
				t.Fatalf("server enrollment body included gpuType: %#v", body)
			}
			if _, ok := body["gpuCount"]; ok {
				t.Fatalf("server enrollment body included gpuCount: %#v", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"enrollmentTokenId":"et-node","enrollmentToken":"tr_node","orgId":"org-1","zoneId":"auto","role":"server"}`))
		default:
			t.Fatalf("unexpected call %d", call)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "tcapi_test", WithHTTPClient(server.Client()))
	clientEnrollment, err := client.EnrollClient(context.Background(), CreateClientEnrollmentRequest{ZoneID: "zone-1", GPUType: "nvidia-l4", GPUCount: 2, ExpiresInSeconds: 3600})
	if err != nil {
		t.Fatalf("EnrollClient: %v", err)
	}
	if clientEnrollment.EnrollmentTokenID != "et-client" || clientEnrollment.Role != RoleClient || clientEnrollment.GPUCount != 2 {
		t.Fatalf("client enrollment = %+v", clientEnrollment)
	}
	nodeEnrollment, err := client.EnrollNode(context.Background(), CreateNodeEnrollmentRequest{ZoneID: "auto"})
	if err != nil {
		t.Fatalf("EnrollNode: %v", err)
	}
	if nodeEnrollment.EnrollmentTokenID != "et-node" || nodeEnrollment.Role != RoleServer {
		t.Fatalf("node enrollment = %+v", nodeEnrollment)
	}
}

func TestListRoutes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/zones":
			if r.Method != http.MethodGet {
				t.Fatalf("zones method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"zones":[{"zoneId":"zone-1","displayName":"prod","nodeCount":1,"clientCount":2,"createdAt":"2026-08-01T00:00:00Z"}]}`))
		case "/api/v1/clients":
			if r.Method != http.MethodGet || r.URL.Query().Get("zoneId") != "zone-1" {
				t.Fatalf("clients request = %s %s", r.Method, r.URL.String())
			}
			_, _ = w.Write([]byte(`{"clients":[{"clientId":"client-1","zoneId":"zone-1","displayName":"client","gpuType":"nvidia-l4","gpuCount":1,"createdAt":"2026-08-01T00:00:00Z","updatedAt":"2026-08-01T00:00:00Z"}]}`))
		case "/api/v1/hosts":
			if r.Method != http.MethodGet || r.URL.Query().Get("zoneId") != "zone-1" {
				t.Fatalf("hosts request = %s %s", r.Method, r.URL.String())
			}
			_, _ = w.Write([]byte(`{"hosts":[{"hostId":"host-1","zoneId":"zone-1","displayName":"node","hostname":"node-1","gpuType":"nvidia-l4","gpuCount":1,"status":"online"}]}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "tcapi_test", WithHTTPClient(server.Client()))
	zones, err := client.ListZones(context.Background())
	if err != nil {
		t.Fatalf("ListZones: %v", err)
	}
	if len(zones) != 1 || zones[0].ZoneID != "zone-1" || zones[0].NodeCount != 1 || zones[0].ClientCount != 2 {
		t.Fatalf("zones = %+v", zones)
	}
	clients, err := client.ListClients(context.Background(), "zone-1")
	if err != nil {
		t.Fatalf("ListClients: %v", err)
	}
	if len(clients) != 1 || clients[0].ClientID != "client-1" || clients[0].GPUCount != 1 {
		t.Fatalf("clients = %+v", clients)
	}
	nodes, err := client.ListNodes(context.Background(), "zone-1")
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].HostID != "host-1" || nodes[0].Status != "online" {
		t.Fatalf("nodes = %+v", nodes)
	}
}

func TestDeleteAndRevokeRoutes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/enrollment-tokens/enroll-1/node":
			if r.Method != http.MethodDelete {
				t.Fatalf("delete enrollment method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"enrollmentTokenId":"enroll-1","role":"client","clientId":"client-1","nodeDeleted":true,"deletedAt":"2026-08-01T00:00:00Z"}`))
		case "/api/v1/clients/client-1/revoke":
			if r.Method != http.MethodPost {
				t.Fatalf("revoke client method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"clientId":"client-1","decommissionedAt":"2026-08-01T00:00:00Z"}`))
		case "/api/v1/hosts/host-1/revoke":
			if r.Method != http.MethodPost {
				t.Fatalf("revoke node method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"hostId":"host-1","revokedAt":"2026-08-01T00:00:00Z"}`))
		case "/api/v1/zones/zone-1":
			if r.Method != http.MethodDelete {
				t.Fatalf("delete zone method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"zoneId":"zone-1"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "tcapi_test", WithHTTPClient(server.Client()))
	deleted, err := client.UnenrollClient(context.Background(), "enroll-1")
	if err != nil {
		t.Fatalf("UnenrollClient: %v", err)
	}
	if !deleted.NodeDeleted || deleted.ClientID != "client-1" {
		t.Fatalf("deleted = %+v", deleted)
	}
	revokedClient, err := client.RevokeClient(context.Background(), "client-1")
	if err != nil {
		t.Fatalf("RevokeClient: %v", err)
	}
	if revokedClient.ClientID != "client-1" {
		t.Fatalf("revokedClient = %+v", revokedClient)
	}
	revokedNode, err := client.RevokeNode(context.Background(), "host-1")
	if err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}
	if revokedNode.HostID != "host-1" {
		t.Fatalf("revokedNode = %+v", revokedNode)
	}
	if err := client.DeleteZone(context.Background(), "zone-1"); err != nil {
		t.Fatalf("DeleteZone: %v", err)
	}
}

func TestAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden","message":"missing capability"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "tcapi_test", WithHTTPClient(server.Client()))
	_, err := client.ListZones(context.Background())
	if err == nil {
		t.Fatal("ListZones error = nil, want error")
	}
	if !IsForbidden(err) || !IsPermanent(err) || IsUnauthorized(err) || IsNotFound(err) || IsConflict(err) {
		t.Fatalf("error helpers returned unexpected values for %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Method != http.MethodGet || apiErr.Path != "/api/v1/zones" || apiErr.ErrorType != "forbidden" || apiErr.Message != "missing capability" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
}

func TestEnrollmentCommands(t *testing.T) {
	client := NewClient("https://central.test", "tcapi_test", WithInstallURL("https://install.test/install.sh"))

	clientCommand := client.ClientEnrollmentCommand("tr_client's token")
	for _, want := range []string{
		"curl -fsSL 'https://install.test/install.sh' | sudo",
		"THUNDER_INSTALL_MODE=client",
		"THUNDER_NOWARN=1",
		"THUNDER_CENTRAL_URL='https://central.test'",
		"THUNDER_ENROLLMENT_TOKEN='tr_client'\"'\"'s token'",
	} {
		if !strings.Contains(clientCommand, want) {
			t.Fatalf("client command %q missing %q", clientCommand, want)
		}
	}

	clientEnvCommand := client.ClientEnrollmentCommandFor(ClientEnrollmentCommandRequest{EnrollmentTokenEnv: "THUNDER_ENROLLMENT_TOKEN"})
	if !strings.Contains(clientEnvCommand, `THUNDER_ENROLLMENT_TOKEN="${THUNDER_ENROLLMENT_TOKEN}"`) {
		t.Fatalf("client env command %q does not read token from env", clientEnvCommand)
	}
	if strings.Contains(clientEnvCommand, "tr_client") {
		t.Fatalf("client env command %q should not contain a raw token", clientEnvCommand)
	}

	nodeCommand := client.NodeEnrollmentCommand(NodeEnrollmentCommandRequest{
		EnrollmentToken: "tr_node",
		IP:              "10.0.0.5",
		Zone:            "zone-1",
		PortRange:       "10000-10100",
		NodeName:        "friendly node",
	})
	for _, want := range []string{
		"THUNDER_INSTALL_MODE=thunderd",
		"THUNDER_CENTRAL_URL='https://central.test'",
		"THUNDER_ENROLLMENT_TOKEN='tr_node'",
		"THUNDERD_IP='10.0.0.5'",
		"THUNDER_ZONE='zone-1'",
		"THUNDERD_PORT_RANGE='10000-10100'",
		"THUNDERD_NODE_NAME='friendly node'",
	} {
		if !strings.Contains(nodeCommand, want) {
			t.Fatalf("node command %q missing %q", nodeCommand, want)
		}
	}
}
