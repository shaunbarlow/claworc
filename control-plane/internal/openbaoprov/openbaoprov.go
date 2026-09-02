// Package openbaoprov manages Claworc's own OpenBao deployment: a single
// shared, always-on service backing every agent instance's secret storage,
// structurally identical in shape to internal/connectorprov (which manages
// the shared OpenConnector deployment) — one singleton workload, applied via
// the generic orchestrator.WorkloadSpec/Apply() primitive, reached over
// plain HTTP (no SSH tunnel needed; OpenBao's API is designed to be dialed
// directly, same as OpenConnector's).
//
// This package owns only the workload lifecycle (build spec, apply, health
// check, resolve address). Init/unseal/policy/token bookkeeping is layered
// on top in internal/handlers/openbao.go, mirroring how connectorprov stays
// ignorant of runtime-token minting (that lives in handlers/connector.go
// too).
package openbaoprov

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/gluk-w/claworc/control-plane/internal/orchestrator"
)

// WorkloadName is the fixed name Apply/DeleteWorkload use for the shared
// OpenBao container/Deployment (+ Service/NetworkPolicy on Kubernetes).
// Singleton: one name, cluster-wide, not derived from any instance. Also
// used as the in-network hostname agents dial directly (see
// handlers.buildOpenbaoEnvVars), same convention as connectorprov.WorkloadName.
const WorkloadName = "claworc-openbao"

// dataVolumeName is the persistent volume backing OpenBao's file storage
// backend (encrypted-at-rest by OpenBao itself once initialized) at
// /openbao/data.
const dataVolumeName = "claworc-openbao-data"

// containerPort is the fixed port OpenBao's image listens on by default.
// Configurable in principle, but pinned here the same way connectorprov
// pins OpenConnector's port 3000 — one fewer moving part.
const containerPort = 8200

// storagePath is where the file storage backend lives, and must stay
// /openbao/file: the image runs as USER openbao (uid 100), so the server
// process never holds root and cannot chown its own storage directory. The
// image pre-creates (and declares as a VOLUME) exactly /openbao/file and
// /openbao/logs, owned by openbao, which is what makes a volume mounted
// there writable — Docker seeds a fresh named volume with the image
// directory's ownership. Any other path (this was /openbao/data) does not
// exist in the image, so the volume is created root-owned and init dies with
// "failed to persist keyring: mkdir /openbao/file/core: permission denied".
// On Kubernetes the equivalent guarantee comes from FSGroup below, since a
// PVC ignores the image directory's ownership.
const storagePath = "/openbao/file"

// openbaoGID is the group the image's openbao user belongs to. Used as the
// pod FSGroup on Kubernetes so the mounted PVC is group-writable by the
// non-root server process.
const openbaoGID int64 = 1000

// healthTimeout bounds a single /v1/sys/health probe request.
const healthTimeout = 5 * time.Second

// Config is the resolved settings needed to (re)apply the OpenBao workload.
// Callers (the settings handler) assemble this from the openbao_* database
// settings; this package has no direct DB dependency.
type Config struct {
	// Image is the OpenBao image reference to run, e.g. "openbao/openbao:latest".
	Image string
	// StorageSize is the requested capacity for the data volume, e.g. "10Gi".
	// Honoured only on first creation.
	StorageSize string
}

// GenerateRandomHex returns n random bytes hex-encoded. Used by callers that
// need a fresh random token-shaped value outside OpenBao's own minting (none
// currently — OpenBao mints its own root token and unseal key at init — but
// kept here for parity with connectorprov.GenerateAdminToken in case a
// future caller needs one, e.g. a webhook shared secret for OpenBao's audit
// log if that is ever wired up).
func GenerateRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// buildSpec describes the OpenBao workload to the orchestrator. Config is
// supplied inline via the BAO_LOCAL_CONFIG env var — the official
// openbao/openbao image's documented mechanism for containerized config,
// avoiding a config-file volume mount entirely.
func buildSpec(cfg Config) orchestrator.WorkloadSpec {
	storage := cfg.StorageSize
	if storage == "" {
		storage = "1Gi"
	}
	fsGroup := openbaoGID

	localConfig := fmt.Sprintf(`{
  "storage": {"file": {"path": "%s"}},
  "listener": {"tcp": {"address": "0.0.0.0:%d", "tls_disable": true}},
  "disable_mlock": true,
  "api_addr": "http://%s:%d"
}`, storagePath, containerPort, WorkloadName, containerPort)

	return orchestrator.WorkloadSpec{
		Name:  WorkloadName,
		Image: cfg.Image,
		Args:  []string{"server"},
		Env: map[string]string{
			"BAO_LOCAL_CONFIG": localConfig,
		},
		// Args must pin the subcommand to a bare `server`. The image's
		// default CMD is `server -dev -dev-no-store-token`, and inheriting it
		// is fatal: the entrypoint always appends -dev-listen-address, so
		// dev mode binds 0.0.0.0:8200 itself and then the listener from
		// BAO_LOCAL_CONFIG fails with "address already in use", crash-looping
		// the container. Dev mode would also force inmem storage, silently
		// discarding the persistent volume below. Args (not Command) so the
		// image's docker-entrypoint.sh still runs -- it is what materialises
		// BAO_LOCAL_CONFIG into /openbao/config/local.json and drops root.
		Labels: map[string]string{
			"claworc-role": "openbao",
		},
		// PullIfNotPresent (the zero value): unlike the connector's mutable
		// "tip" tag, "latest" here is expected to be pinned/updated
		// deliberately by an admin editing openbao_image, not force-pulled on
		// every apply. Matches ordinary instance image semantics.
		Volumes: []orchestrator.VolumeMount{
			{Name: dataVolumeName, Size: storage, MountPath: storagePath},
		},
		Ports: []orchestrator.PortSpec{
			{Name: "http", ContainerPort: containerPort, Publish: true},
		},
		// FSGroup makes the PVC group-writable by the image's non-root
		// openbao user on Kubernetes. No-op on Docker, where ownership is
		// inherited from the image directory instead — see storagePath.
		Security: orchestrator.SecurityOptions{FSGroup: &fsGroup},
		// No Probes.Liveness: OpenBao's own image ships a working
		// HEALTHCHECK; a generic TCP probe would only add a redundant
		// failure mode (a sealed-but-listening OpenBao is a normal, healthy
		// startup state that a naive TCP check would already pass anyway).
	}
}

// Manager owns the lifecycle of the shared OpenBao workload: applying it
// against the active orchestrator, tearing it down, checking status, and
// probing readiness. Stateless beyond the orchestrator reference, same
// shape as connectorprov.Manager.
type Manager struct {
	orch orchestrator.ContainerOrchestrator
}

// New returns a Manager driving orch. orch must not be nil.
func New(orch orchestrator.ContainerOrchestrator) *Manager {
	return &Manager{orch: orch}
}

// Apply creates or updates the OpenBao workload to match cfg. Idempotent:
// safe to call on every control-plane boot (when openbao_enabled) and
// whenever the admin edits openbao settings.
func (m *Manager) Apply(ctx context.Context, cfg Config) error {
	if cfg.Image == "" {
		return fmt.Errorf("openbaoprov: Config.Image is required")
	}
	if err := m.orch.Apply(ctx, buildSpec(cfg)); err != nil {
		return fmt.Errorf("apply openbao workload: %w", err)
	}
	return nil
}

// Delete tears down the OpenBao workload and its data volume. Used when an
// admin disables the feature entirely and asks to remove the deployment,
// not just stop routing to it. Note: this destroys the file-storage-backed
// secret data irrecoverably — callers should confirm with the admin before
// calling this rather than on a simple "disable" toggle (see
// handlers.applyOpenbaoAsync, which stops rather than deletes on disable).
func (m *Manager) Delete(ctx context.Context) error {
	spec := orchestrator.WorkloadSpec{
		Name: WorkloadName,
		Volumes: []orchestrator.VolumeMount{
			{Name: dataVolumeName, MountPath: "/openbao/data"},
		},
	}
	return m.orch.DeleteWorkload(ctx, spec)
}

// Status reports the OpenBao workload's coarse lifecycle state.
func (m *Manager) Status(ctx context.Context) (string, error) {
	return m.orch.GetInstanceStatus(ctx, WorkloadName)
}

// Address resolves the (host, port) to dial OpenBao's HTTP port from the
// control plane itself, using the orchestrator's generic WorkloadAddress
// primitive (container IP, cluster Service DNS, or published loopback port,
// depending on backend and where the control plane runs). Distinct from the
// stable in-network hostname:port (WorkloadName:containerPort) rendered into
// agent env vars — see the doc comment on WorkloadName.
func (m *Manager) Address(ctx context.Context) (string, int, error) {
	return m.orch.WorkloadAddress(ctx, WorkloadName, containerPort)
}

// WaitHealthy polls OpenBao's /v1/sys/health endpoint until it responds
// (any status code — see probeReachable) or timeout elapses. OpenBao's
// health endpoint intentionally returns non-200 codes for perfectly normal
// states (429 standby, 472/473/501/503 sealed/uninitialized variants per
// https://openbao.org/api-docs/system/health/), so "healthy" here means
// "reachable and responding with a well-formed status", not HTTP 200 —
// the actual seal/init state is inspected separately by the caller via
// AdminClient.SealStatus.
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
		if err := probeReachable(ctx, host, port); err != nil {
			lastErr = err
			time.Sleep(2 * time.Second)
			continue
		}
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("openbao not reachable within %s: %w", timeout, lastErr)
	}
	return fmt.Errorf("openbao not reachable within %s", timeout)
}

func probeReachable(ctx context.Context, host string, port int) error {
	reqCtx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		fmt.Sprintf("http://%s/v1/sys/health", net.JoinHostPort(host, fmt.Sprintf("%d", port))), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Any response at all (even 4xx/5xx sealed/uninitialized codes) means the
	// listener is up and speaking HTTP; that is all "reachable" needs to mean
	// here. A connection-level error is the only thing that should retry.
	return nil
}
