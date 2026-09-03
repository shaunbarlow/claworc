package openbaoprov

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// ErrNotFound wraps every 404 OpenBao returns. Worth distinguishing from a
// generic failure because in KV v2 a 404 is routinely the *expected* answer
// rather than an error: reading a secret that was never written, or listing
// a path prefix under which nothing exists yet, both 404. Callers that mean
// "empty" by that use errors.Is(err, ErrNotFound); everyone else keeps
// treating it as the failure it is for them.
var ErrNotFound = errors.New("openbao: not found")

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

// KV v2 splits every logical secret path across two API prefixes: the data
// itself lives under secret/data/<path> and its version history under
// secret/metadata/<path>. Listing is only available on the metadata prefix,
// which is why ListSecretPaths and DeleteSecret address it while
// ReadSecret/WriteSecret address the data one. Callers pass the logical path
// (e.g. "agents/<uuid>/github") and this client adds the right prefix.
//
// Callers are responsible for validating path/field syntax before calling
// (see handlers.validateSecretPath): these methods interpolate the path into
// a URL as-is.

// SecretEntry is one KV v2 secret: its current field set plus the version
// metadata an admin UI needs to show when it last changed.
type SecretEntry struct {
	// Fields is the secret's current version's data map. Values are
	// whatever JSON the writer stored -- agents writing via `bao kv put`
	// always produce strings, but a raw API writer could store numbers or
	// nested objects, so this stays interface{} rather than lying about it.
	Fields map[string]interface{}
	// Version is the current version number of the secret.
	Version int
	// CreatedTime is when the current version was written (RFC3339).
	CreatedTime string
}

// ListSecretPaths lists the immediate children of a logical KV v2 path
// prefix (which must end in "/", or be empty for the mount root). Child
// names that are themselves prefixes come back with a trailing "/", exactly
// as OpenBao reports them -- recursion is the caller's business.
//
// A prefix that has never been written to returns an empty slice and no
// error: KV v2 answers 404 for it, which means "nothing here", not "broken".
func (c *AdminClient) ListSecretPaths(ctx context.Context, prefix string) ([]string, error) {
	var resp struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	// GET + ?list=true rather than the LIST HTTP verb OpenBao also accepts:
	// identical semantics, but a standard method survives any proxy or
	// middlebox between here and the workload.
	err := c.do(ctx, http.MethodGet, "/v1/secret/metadata/"+prefix+"?list=true", nil, &resp, true)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return resp.Data.Keys, nil
}

// ReadSecret returns the current version of the secret at a logical KV v2
// path. Returns ErrNotFound (wrapped) if it does not exist, so callers that
// treat "not written yet" as a normal state can test for it.
func (c *AdminClient) ReadSecret(ctx context.Context, path string) (SecretEntry, error) {
	var resp struct {
		Data struct {
			Data     map[string]interface{} `json:"data"`
			Metadata struct {
				Version     int    `json:"version"`
				CreatedTime string `json:"created_time"`
				Destroyed   bool   `json:"destroyed"`
			} `json:"metadata"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/secret/data/"+path, nil, &resp, true); err != nil {
		return SecretEntry{}, err
	}
	return SecretEntry{
		Fields:      resp.Data.Data,
		Version:     resp.Data.Metadata.Version,
		CreatedTime: resp.Data.Metadata.CreatedTime,
	}, nil
}

// WriteSecret writes fields as the new current version of the secret at a
// logical KV v2 path, creating the secret (and any intermediate path
// prefixes, which KV v2 materialises implicitly) if it does not exist.
//
// KV v2 replaces the whole field set on write rather than merging, so a
// caller changing one field of a multi-field secret must read, merge and
// write the complete map back -- see handlers.PutInstanceSecret.
func (c *AdminClient) WriteSecret(ctx context.Context, path string, fields map[string]interface{}) error {
	body, err := json.Marshal(map[string]interface{}{"data": fields})
	if err != nil {
		return fmt.Errorf("encode secret payload: %w", err)
	}
	return c.do(ctx, http.MethodPost, "/v1/secret/data/"+path, body, nil, true)
}

// DeleteSecret permanently removes the secret at a logical KV v2 path,
// including every version and its metadata. Deliberately the metadata
// delete (the destructive one) and not the data delete, which only marks
// the latest version deleted while leaving it recoverable and leaves the
// key visible in listings -- an admin who removes a secret from the UI
// means it to be gone.
func (c *AdminClient) DeleteSecret(ctx context.Context, path string) error {
	err := c.do(ctx, http.MethodDelete, "/v1/secret/metadata/"+path, nil, nil, true)
	if err != nil && errors.Is(err, ErrNotFound) {
		return nil // already gone; deleting is idempotent
	}
	return err
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
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("openbao %s %s: %w", method, path, ErrNotFound)
	}
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
