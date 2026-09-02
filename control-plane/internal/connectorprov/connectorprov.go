// Package connectorprov manages Claworc's own OpenConnector (OOMOL Connect)
// deployment: a single shared, always-on service backing every agent
// instance, analogous to the on-demand browser feature in internal/browserprov
// but singleton rather than per-instance.
//
// Unlike the browser workload (which exposes only sshd and is reached over an
// SSH tunnel because CDP/noVNC are loopback-only inside the pod), OpenConnector
// serves plain HTTP on port 3000 and is designed to be reached directly, so
// this package talks to it over the orchestrator's generic WorkloadAddress
// primitive instead of dialing an SSH client.
package connectorprov

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/gluk-w/claworc/control-plane/internal/orchestrator"
)

// WorkloadName is the fixed name Apply/DeleteWorkload use for the shared
// connector container/Deployment (+ Service/NetworkPolicy on Kubernetes).
// Unlike browser workloads, this is a singleton: one name, cluster-wide,
// not derived from any instance.
const WorkloadName = "claworc-connector"

// dataVolumeName is the persistent volume backing OpenConnector's runtime
// database (encrypted credentials, OAuth configs, idempotent response
// cache) at /app/data. A single named volume, not per-instance.
const dataVolumeName = "claworc-connector-data"

// containerPort is the fixed port OpenConnector's image listens on. Not
// configurable, it is baked into the upstream Dockerfile (ENV PORT=3000).
const containerPort = 3000

// healthTimeout bounds a single /health probe request.
const healthTimeout = 5 * time.Second

// Config is the resolved settings needed to (re)apply the connector
// workload. Callers (the settings handler) assemble this from the
// connector_* database settings; this package has no direct DB dependency.
type Config struct {
	// Image is the OpenConnector image reference to run, e.g.
	// "ghcr.io/shaunbarlow/open-connector:tip".
	Image string
	// EncryptionKey is the value for OOMOL_CONNECT_ENCRYPTION_KEY. It
	// encrypts stored credentials, OAuth client configuration, and
	// completed idempotent Action responses. Losing it makes the encrypted
	// rows already in /app/data unrecoverable.
	EncryptionKey string
	// AdminToken is the value for OOMOL_CONNECT_ADMIN_TOKEN. It
	// authenticates the connector's admin API (/api/*) and Web Console.
	// Only the control plane holds this value; it is never rendered into
	// any agent's config or environment.
	AdminToken string
	// Origin is the value for OOMOL_CONNECT_ORIGIN, used by OpenConnector to
	// build OAuth redirect URLs. Optional; left unset lets it default to
	// http://localhost:<port>, which is fine as long as OAuth flows aren't
	// used through this deployment.
	Origin string
	// AllowedCustomOAuth is the value for OOMOL_CONNECT_ALLOWED_CUSTOM_OAUTH:
	// a comma-separated list of service ids (or "*" for every provider) that
	// may authorize a connection with its own connection-scoped OAuth app
	// (clientId/clientSecret passed to POST /oauth/authorize) instead of the
	// shared per-provider default. Optional; left empty disables the
	// override entirely, matching OpenConnector's own default. Only takes
	// effect when EncryptionKey is also set, per OpenConnector's own gate.
	AllowedCustomOAuth string
	// StorageSize is the requested capacity for the data volume, e.g. "10Gi".
	// Honoured only on first creation (matches VolumeMount.Size semantics).
	StorageSize string
}

// GenerateEncryptionKey returns a fresh base64-encoded 32-byte key suitable
// for OOMOL_CONNECT_ENCRYPTION_KEY, matching the format
// docs/docker-ghcr.md's "openssl rand -base64 32" example produces.
func GenerateEncryptionKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate encryption key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// GenerateAdminToken returns a fresh random hex token for
// OOMOL_CONNECT_ADMIN_TOKEN. Same shape as the gateway/webhook tokens
// generated elsewhere in the control plane (32 random bytes, hex-encoded).
func GenerateAdminToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate admin token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// buildSpec describes the connector workload to the orchestrator. Publish is
// set on the HTTP port so the Docker backend binds it to a host loopback
// port too, useful when the control plane itself runs outside Docker (dev,
// bare-metal install) and still needs to reach the connector directly for
// admin-API calls (see the future admin-token-minting client).
func buildSpec(cfg Config) orchestrator.WorkloadSpec {
	env := map[string]string{
		"OOMOL_CONNECT_DATA_DIR": "/app/data",
	}
	if cfg.EncryptionKey != "" {
		env["OOMOL_CONNECT_ENCRYPTION_KEY"] = cfg.EncryptionKey
	}
	if cfg.AdminToken != "" {
		env["OOMOL_CONNECT_ADMIN_TOKEN"] = cfg.AdminToken
	}
	if cfg.Origin != "" {
		env["OOMOL_CONNECT_ORIGIN"] = cfg.Origin
	}
	if cfg.AllowedCustomOAuth != "" {
		env["OOMOL_CONNECT_ALLOWED_CUSTOM_OAUTH"] = cfg.AllowedCustomOAuth
	}

	storage := cfg.StorageSize
	if storage == "" {
		storage = "10Gi"
	}

	return orchestrator.WorkloadSpec{
		Name:  WorkloadName,
		Image: cfg.Image,
		Env:   env,
		Labels: map[string]string{
			"claworc-role": "connector",
		},
		// PullAlways: the connector is deployed from a mutable tag
		// (default "ghcr.io/shaunbarlow/open-connector:tip", but any tag an
		// admin points it at is equally mutable). PullIfNotPresent would
		// only ever pull the very first time the tag is seen -- once
		// cached locally (Docker) or on a node (Kubernetes), every later
		// Apply (including the explicit "update to latest" action, see
		// handlers.UpdateConnectorImage) would silently reuse the stale
		// image forever instead of picking up a newer build pushed under
		// the same tag. Docker's Apply force-pulls on PullAlways (see
		// docker_apply.go); Kubernetes sets imagePullPolicy: Always, which
		// makes the kubelet re-pull on every pod (re)start.
		Pull: orchestrator.PullAlways,
		Volumes: []orchestrator.VolumeMount{
			{Name: dataVolumeName, Size: storage, MountPath: "/app/data"},
		},
		Ports: []orchestrator.PortSpec{
			{Name: "http", ContainerPort: containerPort, Publish: true},
		},
		// No Probes.Liveness here deliberately: the generic Docker backend
		// (docker_apply.go) turns any Probes.Liveness into a bash-based
		// `>/dev/tcp/...` CMD-SHELL healthcheck, which requires bash. Every
		// other workload this orchestrator manages is Debian-based and has
		// bash, but open-connector's image is `node:24-alpine`, which does
		// not ship bash. That combination made the container-level
		// healthcheck fail unconditionally and clobbered the image's own
		// working baked-in HEALTHCHECK (`CMD ["node", "scripts/healthcheck.ts"]`
		// against /health), permanently marking the container "unhealthy".
		// Leaving Probes unset lets that image-native healthcheck stand.
	}
}

// Manager owns the lifecycle of the shared connector workload: applying it
// against the active orchestrator, tearing it down, checking status, and
// probing readiness. It holds no state of its own beyond the orchestrator
// reference; Config is supplied fresh on every call from whatever settings
// the caller currently has in the database.
type Manager struct {
	orch orchestrator.ContainerOrchestrator
}

// New returns a Manager driving orch. orch must not be nil.
func New(orch orchestrator.ContainerOrchestrator) *Manager {
	return &Manager{orch: orch}
}

// Apply creates or updates the connector workload to match cfg. Idempotent:
// safe to call on every control-plane boot (when connector_enabled) and
// whenever the admin edits connector settings.
func (m *Manager) Apply(ctx context.Context, cfg Config) error {
	if cfg.Image == "" {
		return fmt.Errorf("connectorprov: Config.Image is required")
	}
	if err := m.orch.Apply(ctx, buildSpec(cfg)); err != nil {
		return fmt.Errorf("apply connector workload: %w", err)
	}
	return nil
}

// Delete tears down the connector workload and its data volume. Used when an
// admin disables the feature entirely (connector_enabled -> false) and asks
// to remove the deployment, not just stop routing to it.
func (m *Manager) Delete(ctx context.Context) error {
	spec := orchestrator.WorkloadSpec{
		Name: WorkloadName,
		Volumes: []orchestrator.VolumeMount{
			{Name: dataVolumeName, MountPath: "/app/data"},
		},
	}
	return m.orch.DeleteWorkload(ctx, spec)
}

// Status reports the connector workload's coarse lifecycle state, mirroring
// browserprov.Status's vocabulary ("running"/"creating"/"stopped"/"error" as
// returned by the orchestrator) so the Settings UI can reuse the same badge
// component pattern.
func (m *Manager) Status(ctx context.Context) (string, error) {
	return m.orch.GetInstanceStatus(ctx, WorkloadName)
}

// Address resolves the (host, port) to dial the connector's HTTP port,
// using the orchestrator's generic WorkloadAddress primitive (container IP,
// cluster Service DNS, or published loopback port, depending on backend and
// where the control plane itself is running).
func (m *Manager) Address(ctx context.Context) (string, int, error) {
	return m.orch.WorkloadAddress(ctx, WorkloadName, containerPort)
}

// WaitHealthy polls the connector's /health endpoint until it responds with
// HTTP 200 or timeout elapses. Mirrors browserprov's waitForCDPReady in
// spirit: readiness is defined by what the rest of the system actually
// depends on, but over plain HTTP instead of an SSH-tunneled TCP dial, since
// OpenConnector's health endpoint is directly reachable.
func (m *Manager) WaitHealthy(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		host, port, err := m.Address(ctx)
		if err != nil {
			lastErr = err
			time.Sleep(2 * time.Second)
			continue
		}
		if err := probeHealth(ctx, host, port); err != nil {
			lastErr = err
			time.Sleep(2 * time.Second)
			continue
		}
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("connector not healthy within %s: %w", timeout, lastErr)
	}
	return fmt.Errorf("connector not healthy within %s", timeout)
}

func probeHealth(ctx context.Context, host string, port int) error {
	reqCtx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		fmt.Sprintf("http://%s/v1/health", net.JoinHostPort(host, fmt.Sprintf("%d", port))), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned %d", resp.StatusCode)
	}
	return nil
}
