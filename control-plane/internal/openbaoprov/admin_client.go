package openbaoprov

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

// AdminClient talks to OpenBao's own HTTP API (Vault-API-compatible:
// /v1/sys/*, /v1/auth/token/*, /v1/secret/*) using either the root token
// (bootstrap only) or the control plane's own long-lived admin token
// (everyday use). Used only from the control plane; neither token is ever
// handed to an agent.
type AdminClient struct {
	host       string
	port       int
	token      string
	httpClient *http.Client
}

// NewAdminClient builds a client that dials OpenBao directly at host:port
// (as resolved by Manager.Address) using token for every request's
// X-Vault-Token header (OpenBao kept the Vault API's header name for
// compatibility).
func NewAdminClient(host string, port int, token string) *AdminClient {
	return &AdminClient{
		host:       host,
		port:       port,
		token:      token,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// SealStatusResponse is the subset of GET /v1/sys/seal-status this client
// needs.
type SealStatusResponse struct {
	Initialized bool `json:"initialized"`
	Sealed      bool `json:"sealed"`
}

// SealStatus reports whether OpenBao has been initialized and whether it is
// currently sealed. Does not require a token — this endpoint is
// unauthenticated by design (an operator needs to be able to check seal
// state before they have any token at all).
func (c *AdminClient) SealStatus(ctx context.Context) (SealStatusResponse, error) {
	var resp SealStatusResponse
	err := c.do(ctx, http.MethodGet, "/v1/sys/seal-status", nil, &resp, false)
	return resp, err
}

// InitResponse is the one-time response from PUT /v1/sys/init: the root
// token and the (here, single, since secret_threshold=1) unseal key. Both
// values are returned exactly once and cannot be retrieved again from
// OpenBao afterwards — callers must persist them immediately.
type InitResponse struct {
	RootToken string   `json:"root_token"`
	Keys      []string `json:"keys"`
	KeysB64   []string `json:"keys_base64"`
}

// Init initializes a fresh OpenBao instance with Shamir secret_shares=1,
// secret_threshold=1 (single key share, per Claworc's auto-unseal design —
// see docs/planning/openbao-integration-plan.md). Must only be called when
// SealStatus reports Initialized=false; calling it twice returns an error
// from OpenBao itself.
func (c *AdminClient) Init(ctx context.Context) (InitResponse, error) {
	body, _ := json.Marshal(map[string]int{
		"secret_shares":    1,
		"secret_threshold": 1,
	})
	var resp InitResponse
	err := c.do(ctx, http.MethodPut, "/v1/sys/init", body, &resp, false)
	return resp, err
}

// Unseal submits the single unseal key. Idempotent from the caller's
// perspective: safe to call whenever SealStatus reports Sealed=true,
// including redundantly on every control-plane boot.
func (c *AdminClient) Unseal(ctx context.Context, key string) error {
	body, _ := json.Marshal(map[string]string{"key": key})
	var resp SealStatusResponse
	return c.do(ctx, http.MethodPut, "/v1/sys/unseal", body, &resp, false)
}

// TuneTokenMaxTTL raises the max_lease_ttl of the token auth mount to ttl.
// Required for any token to outlive OpenBao's 768h (32 day) default: a
// create request asking for more is silently capped to the mount maximum
// rather than rejected, and neither explicit_max_ttl nor a periodic token
// escapes that ceiling -- tuning the mount is the only thing that does.
// Idempotent, and must run before minting any token that needs the longer
// life, since the cap is applied at creation time and never revisited.
//
// Requires "sudo" on sys/auth/token/tune (see openbaoAdminPolicyDocument);
// in practice this is called with the root token during bootstrap.
func (c *AdminClient) TuneTokenMaxTTL(ctx context.Context, ttl string) error {
	body, _ := json.Marshal(map[string]string{"max_lease_ttl": ttl})
	return c.do(ctx, http.MethodPost, "/v1/sys/auth/token/tune", body, nil, true)
}

// RevokeToken revokes a token by its value, invalidating it immediately.
// Used when dropping and re-minting an instance's token so the old one
// cannot continue to be used for the remainder of its (very long) TTL.
// Needs only "update" on auth/token/revoke -- not a sudo path.
//
// Revoking an already-revoked or expired token is reported by OpenBao as a
// plain success, so callers can treat this as idempotent.
func (c *AdminClient) RevokeToken(ctx context.Context, token string) error {
	body, _ := json.Marshal(map[string]string{"token": token})
	return c.do(ctx, http.MethodPost, "/v1/auth/token/revoke", body, nil, true)
}

// EnsureKVv2Mount enables the KV v2 secret engine at path "secret/" if it is
// not already mounted. Idempotent.
func (c *AdminClient) EnsureKVv2Mount(ctx context.Context) error {
	var mounts struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	// OpenBao's mount list is unauthenticated-shape-wise the same as Vault's:
	// GET /v1/sys/mounts requires a token, response has top-level mount paths
	// as keys (with a trailing slash) either directly or under "data"
	// depending on KV version of the response wrapper; probe both shapes
	// defensively via a raw map first.
	var raw map[string]json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/v1/sys/mounts", nil, &raw, true); err != nil {
		return fmt.Errorf("list mounts: %w", err)
	}
	if _, ok := raw["secret/"]; ok {
		return nil
	}
	if data, ok := raw["data"]; ok {
		if err := json.Unmarshal(data, &mounts.Data); err == nil {
			if _, ok := mounts.Data["secret/"]; ok {
				return nil
			}
		}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"type": "kv-v2",
	})
	return c.do(ctx, http.MethodPost, "/v1/sys/mounts/secret", body, nil, true)
}

// PutPolicy upserts an ACL policy by name. policyHCL is the policy body in
// OpenBao's HCL policy-document syntax (path capability blocks). Always safe
// to overwrite — this is how per-instance grant changes are applied (see
// docs/planning/openbao-integration-plan.md §2.3): the token stays the same,
// only the policy attached to it is rewritten.
func (c *AdminClient) PutPolicy(ctx context.Context, name string, policyHCL string) error {
	body, _ := json.Marshal(map[string]string{"policy": policyHCL})
	return c.do(ctx, http.MethodPut, "/v1/sys/policies/acl/"+name, body, nil, true)
}

// createOrphanTokenResponse is the subset of POST
// /v1/auth/token/create-orphan this client needs.
type createOrphanTokenResponse struct {
	Auth struct {
		ClientToken string `json:"client_token"`
	} `json:"auth"`
}

// CreateOrphanToken mints a new orphan (no parent, survives independently)
// token attached to the named policies, with the given TTL (OpenBao
// duration string, e.g. "87600h" for ~10 years). renewable controls whether
// the token can extend its own TTL later; Claworc's long-lived-token design
// mints with renewable=false and a large fixed TTL instead of relying on
// periodic renewal (see the integration plan's "Token lifetime" decision).
func (c *AdminClient) CreateOrphanToken(ctx context.Context, policies []string, ttl string, renewable bool) (string, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"policies":  policies,
		"ttl":       ttl,
		"renewable": renewable,
		"orphan":    true,
	})
	var resp createOrphanTokenResponse
	if err := c.do(ctx, http.MethodPost, "/v1/auth/token/create-orphan", body, &resp, true); err != nil {
		return "", err
	}
	if resp.Auth.ClientToken == "" {
		return "", fmt.Errorf("openbao did not return a client token")
	}
	return resp.Auth.ClientToken, nil
}

// do issues one request. requireToken=false is used only for the two
// bootstrap endpoints (seal-status, init, unseal) that OpenBao itself does
// not require a token for; every other call sets requireToken=true and
// fails locally (rather than sending an unauthenticated request that would
// just 403) if c.token is empty, since that always indicates a
// programming/config error in this codebase rather than a legitimate
// unauthenticated call.
func (c *AdminClient) do(ctx context.Context, method, path string, body []byte, out interface{}, requireToken bool) error {
	if requireToken && c.token == "" {
		return fmt.Errorf("openbao admin client: no token configured for %s %s", method, path)
	}
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
	if c.token != "" {
		req.Header.Set("X-Vault-Token", c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("openbao %s %s: %d %s", method, path, resp.StatusCode, string(respBody))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response from %s %s: %w", method, path, err)
		}
	}
	return nil
}
