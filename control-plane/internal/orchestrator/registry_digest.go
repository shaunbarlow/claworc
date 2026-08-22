package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// This file implements just enough of the Docker Registry HTTP API V2 (plus
// the anonymous-token dance most registries, including Docker Hub, require)
// to answer one question: "what digest does registry/repo:tag currently
// resolve to?" It exists so SelfUpdate can tell "the tag was re-pulled but
// nothing actually changed" apart from a real new build, without adding a
// full container-registry client library as a dependency for a single
// read-only lookup.

// manifestDigestTimeout bounds each network round trip (discovery, token
// fetch, manifest HEAD) so a slow/unreachable registry can't hang a
// self-update check indefinitely. SelfUpdate treats any failure here as
// "can't tell" and fails open into performing the restart anyway.
const manifestDigestTimeout = 10 * time.Second

// dockerHubHost is the API host Docker Hub is actually served from; images
// with no registry component (e.g. "ubuntu", "claworc/claworc") resolve
// here, matching what the Docker CLI/daemon do.
const dockerHubHost = "registry-1.docker.io"

// manifestAcceptHeaders lists every manifest media type worth asking for,
// covering both legacy Docker and modern OCI images, plus multi-arch
// manifest lists/indexes (registries pick the most specific type they have).
var manifestAcceptHeaders = strings.Join([]string{
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.oci.image.index.v1+json",
}, ",")

// imageRef is a parsed "[registry/]repository[:tag]" reference.
type imageRef struct {
	registry   string // API host, e.g. "registry-1.docker.io", "ghcr.io"
	repository string // e.g. "library/ubuntu", "myorg/myimage"
	tag        string // e.g. "latest"; empty only if the ref carried an explicit digest instead
}

// parseImageRef mirrors the Docker CLI's own reference-parsing conventions
// closely enough for this lookup: no registry component means Docker Hub,
// no explicit repository namespace on Docker Hub means "library/", and no
// tag means "latest".
func parseImageRef(ref string) imageRef {
	// A registry component is present only if the part before the first "/"
	// looks like a host (contains "." or ":") or is literally "localhost".
	// Otherwise "foo/bar" is a Docker Hub "foo/bar" repository, not a
	// registry named "foo" with repository "bar".
	registry := dockerHubHost
	rest := ref
	if idx := strings.Index(ref, "/"); idx >= 0 {
		candidate := ref[:idx]
		if candidate == "localhost" || strings.ContainsAny(candidate, ".:") {
			registry = candidate
			rest = ref[idx+1:]
		}
	}

	repository := rest
	tag := "latest"
	// Split tag from the last path segment only, so a registry port
	// (already consumed above) or a repository path can't be mistaken for
	// a tag separator.
	if idx := strings.LastIndex(rest, ":"); idx >= 0 && !strings.Contains(rest[idx:], "/") {
		repository = rest[:idx]
		tag = rest[idx+1:]
	}

	if registry == dockerHubHost && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}

	return imageRef{registry: registry, repository: repository, tag: tag}
}

// resolveManifestDigest returns the content digest a registry currently
// reports for ref (e.g. "sha256:abcd..."), following the standard
// discover-auth-challenge -> fetch-bearer-token -> retry flow used by
// registries that require it (Docker Hub, GHCR, etc.), and falling back to
// an unauthenticated request for registries that don't.
//
// It deliberately never downloads the manifest body: a HEAD request against
// the manifest endpoint is enough to read back the Docker-Content-Digest
// response header, which is what a real `docker pull` would resolve the tag
// to.
func resolveManifestDigest(ctx context.Context, ref string) (string, error) {
	parsed := parseImageRef(ref)
	ctx, cancel := context.WithTimeout(ctx, manifestDigestTimeout)
	defer cancel()

	client := &http.Client{Timeout: manifestDigestTimeout}
	manifestURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", parsed.registry, parsed.repository, parsed.tag)

	digest, status, err := headManifest(ctx, client, manifestURL, "")
	if err != nil {
		return "", err
	}
	if status == http.StatusOK {
		if digest == "" {
			return "", fmt.Errorf("registry response for %s had no Docker-Content-Digest header", ref)
		}
		return digest, nil
	}
	if status != http.StatusUnauthorized {
		return "", fmt.Errorf("unexpected status %d resolving manifest for %s", status, ref)
	}

	// Authorization required: re-issue the HEAD with a bearer token minted
	// per the WWW-Authenticate challenge from the first response.
	token, err := fetchBearerToken(ctx, client, lastAuthChallenge)
	if err != nil {
		return "", fmt.Errorf("auth challenge for %s: %w", ref, err)
	}
	digest, status, err = headManifest(ctx, client, manifestURL, token)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d resolving manifest for %s (after auth)", status, ref)
	}
	if digest == "" {
		return "", fmt.Errorf("registry response for %s had no Docker-Content-Digest header (after auth)", ref)
	}
	return digest, nil
}

// lastAuthChallenge is a package-level scratch var written by headManifest
// and read immediately after by resolveManifestDigest within the same call.
// Not meant for concurrent reuse; kept package-level only to avoid a bespoke
// two-return-value-plus-header-map signature for a single internal caller.
// Safe here because resolveManifestDigest never runs the two steps
// concurrently with itself for the same reference, and each call fully
// consumes the value before returning.
var lastAuthChallenge string

// headManifest issues a HEAD request for a manifest URL, optionally with a
// bearer token, and returns the digest header (if present) and status code.
// On a 401 it stashes the WWW-Authenticate header into lastAuthChallenge so
// the caller can mint a token and retry.
func headManifest(ctx context.Context, client *http.Client, url, bearerToken string) (digest string, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", 0, fmt.Errorf("build manifest request: %w", err)
	}
	req.Header.Set("Accept", manifestAcceptHeaders)
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("request manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		lastAuthChallenge = resp.Header.Get("WWW-Authenticate")
	}
	return resp.Header.Get("Docker-Content-Digest"), resp.StatusCode, nil
}

// fetchBearerToken parses a "Bearer realm=...,service=...,scope=..."
// WWW-Authenticate challenge and exchanges it for a token via the realm's
// anonymous-token endpoint (the same flow `docker pull` performs for public
// images; no credentials are sent, matching the read-only, unauthenticated
// nature of this lookup).
func fetchBearerToken(ctx context.Context, client *http.Client, challenge string) (string, error) {
	params, err := parseBearerChallenge(challenge)
	if err != nil {
		return "", err
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("WWW-Authenticate challenge missing realm: %q", challenge)
	}

	tokenURL := realm
	q := make([]string, 0, 2)
	if service := params["service"]; service != "" {
		q = append(q, "service="+service)
	}
	if scope := params["scope"]; scope != "" {
		q = append(q, "scope="+scope)
	}
	if len(q) > 0 {
		sep := "?"
		if strings.Contains(tokenURL, "?") {
			sep = "&"
		}
		tokenURL += sep + strings.Join(q, "&")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned status %d", resp.StatusCode)
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", fmt.Errorf("token endpoint response had neither token nor access_token")
}

// parseBearerChallenge extracts the key="value" pairs out of a
// `Bearer realm="...",service="...",scope="..."` WWW-Authenticate header.
func parseBearerChallenge(challenge string) (map[string]string, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(challenge, prefix) {
		return nil, fmt.Errorf("not a Bearer challenge: %q", challenge)
	}
	params := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(challenge, prefix), ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		params[kv[0]] = strings.Trim(kv[1], `"`)
	}
	return params, nil
}
