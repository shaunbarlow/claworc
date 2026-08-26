package connectorprov

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// AdminClient talks to the connector's own admin API (/api/*) using the
// control plane's OOMOL_CONNECT_ADMIN_TOKEN. It is used only from the
// control plane, to mint/revoke per-instance scoped runtime tokens; the
// admin token itself is never handed to an agent.
type AdminClient struct {
	host       string
	port       int
	adminToken string
	httpClient *http.Client
}

// NewAdminClient builds a client that dials the connector directly at
// host:port (as resolved by Manager.Address) using adminToken for every
// request.
func NewAdminClient(host string, port int, adminToken string) *AdminClient {
	return &AdminClient{
		host:       host,
		port:       port,
		adminToken: adminToken,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// RuntimeTokenSpec describes the policy a minted runtime token should carry.
// Mirrors the fields documented in docs/runtime-api.md's POST
// /api/runtime-tokens example. Nil slices are sent as empty (unrestricted
// within that dimension, per that doc); AllowedActions/AllowedConnections
// left empty is deliberate for a Level-1 shared deployment, where per-agent
// scoping is not yet configurable from the Claworc UI (see the integration
// plan's Level 2+ note) -- narrowing this later, once the UI exposes it, is
// additive.
type RuntimeTokenSpec struct {
	Name               string   `json:"name"`
	AllowedActions     []string `json:"allowedActions"`
	BlockedActions     []string `json:"blockedActions"`
	AllowedProxies     []string `json:"allowedProxies"`
	AllowedConnections []string `json:"allowedConnections,omitempty"`
}

// runtimeTokenCreateResponse is the subset of the create-token response body
// this client needs. The plaintext token is returned only once, at creation.
type runtimeTokenCreateResponse struct {
	Token  string `json:"token"`
	Record struct {
		ID string `json:"id"`
	} `json:"record"`
}

// CreateRuntimeToken mints a new scoped runtime token via POST
// /api/runtime-tokens and returns the plaintext token plus its stable
// record ID (needed later for revocation).
func (c *AdminClient) CreateRuntimeToken(ctx context.Context, spec RuntimeTokenSpec) (token string, recordID string, err error) {
	if spec.AllowedActions == nil {
		spec.AllowedActions = []string{}
	}
	if spec.BlockedActions == nil {
		spec.BlockedActions = []string{}
	}
	if spec.AllowedProxies == nil {
		spec.AllowedProxies = []string{}
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return "", "", fmt.Errorf("marshal runtime token spec: %w", err)
	}
	var resp runtimeTokenCreateResponse
	if err := c.do(ctx, http.MethodPost, "/api/runtime-tokens", body, &resp); err != nil {
		return "", "", err
	}
	if resp.Token == "" {
		return "", "", fmt.Errorf("connector did not return a token")
	}
	return resp.Token, resp.Record.ID, nil
}

// RevokeRuntimeToken deletes a previously minted token via DELETE
// /api/runtime-tokens/:id. Used when an instance is deleted or the connector
// feature is turned off for it.
func (c *AdminClient) RevokeRuntimeToken(ctx context.Context, recordID string) error {
	if recordID == "" {
		return nil
	}
	return c.do(ctx, http.MethodDelete, "/api/runtime-tokens/"+recordID, nil, nil)
}

func (c *AdminClient) do(ctx context.Context, method, path string, body []byte, out interface{}) error {
	url := fmt.Sprintf("http://%s%s", net.JoinHostPort(c.host, fmt.Sprintf("%d", c.port)), path)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("build request %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.adminToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.adminToken)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("connector admin API %s %s: %d %s", method, path, resp.StatusCode, string(respBody))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response from %s %s: %w", method, path, err)
		}
	}
	return nil
}
