package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gluk-w/claworc/control-plane/internal/connectorprov"
	"github.com/gluk-w/claworc/control-plane/internal/database"
	"github.com/gluk-w/claworc/control-plane/internal/orchestrator"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
)

// connectorApplyTimeout bounds the Apply/Delete/WaitHealthy round trip
// triggered from a settings save. Image pulls can be slow on first run, so
// this is generous, but still request-scoped: a stuck orchestrator must not
// hang the goroutine forever.
const connectorApplyTimeout = 5 * time.Minute

// connector_enabled/connector_image/connector_storage/connector_origin are
// registered as plain settings, and connector_encryption_key/
// connector_admin_token as fixed-encrypted settings, directly in
// settings.go's plainSettings / fixedEncryptedSettings so GetSettings/
// UpdateSettings pick them up through the existing generic paths.

const defaultConnectorImage = "ghcr.io/shaunbarlow/open-connector:tip"
const defaultConnectorStorage = "10Gi"

// connectorEnabled reports the current value of the connector_enabled
// setting. Defaults to false: the managed connector is strictly opt-in.
func connectorEnabled() bool {
	v, _ := database.GetSetting("connector_enabled")
	return v == "true"
}

// resolvedConnectorConfig reads the connector_* settings out of the database
// and decrypts the secrets, returning a connectorprov.Config ready for
// Apply. ok=false means the feature isn't configured well enough to apply
// (no image, or secrets missing -- the latter should self-heal via
// ensureConnectorSecrets, but a race or a manual DB edit could leave it
// briefly inconsistent).
func resolvedConnectorConfig() (cfg connectorprov.Config, ok bool) {
	image, _ := database.GetSetting("connector_image")
	if image == "" {
		image = defaultConnectorImage
	}
	storage, _ := database.GetSetting("connector_storage")
	if storage == "" {
		storage = defaultConnectorStorage
	}
	origin, _ := database.GetSetting("connector_origin")

	encKeyRaw, _ := database.GetSetting("connector_encryption_key")
	adminTokenRaw, _ := database.GetSetting("connector_admin_token")
	if encKeyRaw == "" || adminTokenRaw == "" {
		return connectorprov.Config{}, false
	}
	encKey, err := utils.Decrypt(encKeyRaw)
	if err != nil || encKey == "" {
		return connectorprov.Config{}, false
	}
	adminToken, err := utils.Decrypt(adminTokenRaw)
	if err != nil || adminToken == "" {
		return connectorprov.Config{}, false
	}

	return connectorprov.Config{
		Image:         image,
		EncryptionKey: encKey,
		AdminToken:    adminToken,
		Origin:        origin,
		StorageSize:   storage,
	}, true
}

// ensureConnectorSecrets generates connector_encryption_key and
// connector_admin_token the first time the feature is enabled, if they are
// not already set. Idempotent: a second call with both already present is a
// no-op. Returns true if either secret was freshly generated.
func ensureConnectorSecrets() (generated bool, err error) {
	encKeyRaw, _ := database.GetSetting("connector_encryption_key")
	if encKeyRaw == "" {
		key, genErr := connectorprov.GenerateEncryptionKey()
		if genErr != nil {
			return false, genErr
		}
		enc, encErr := utils.Encrypt(key)
		if encErr != nil {
			return false, encErr
		}
		if err := database.SetSetting("connector_encryption_key", enc); err != nil {
			return false, err
		}
		generated = true
	}
	adminTokenRaw, _ := database.GetSetting("connector_admin_token")
	if adminTokenRaw == "" {
		token, genErr := connectorprov.GenerateAdminToken()
		if genErr != nil {
			return false, genErr
		}
		enc, encErr := utils.Encrypt(token)
		if encErr != nil {
			return false, encErr
		}
		if err := database.SetSetting("connector_admin_token", enc); err != nil {
			return false, err
		}
		generated = true
	}
	return generated, nil
}

// applyConnectorAsync applies (or, when enabled=false, stops) the connector
// workload in the background. Best-effort and logged, matching the pattern
// used by pushMemoryConfig/pushSearchConfig for other settings-triggered
// reconciliation: the HTTP settings-save request must not block on an image
// pull or orchestrator round trip.
func applyConnectorAsync(enabled bool) {
	orch := orchestrator.Get()
	if orch == nil {
		log.Printf("connector: orchestrator not available, skipping apply")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), connectorApplyTimeout)
		defer cancel()
		mgr := connectorprov.New(orch)
		if !enabled {
			if err := orch.StopInstance(ctx, connectorprov.WorkloadName); err != nil {
				log.Printf("connector: stop failed: %v", err)
			}
			return
		}
		cfg, ok := resolvedConnectorConfig()
		if !ok {
			log.Printf("connector: cannot apply, secrets not configured")
			return
		}
		if err := mgr.Apply(ctx, cfg); err != nil {
			log.Printf("connector: apply failed: %v", err)
			return
		}
		if err := mgr.WaitHealthy(ctx, 2*time.Minute); err != nil {
			log.Printf("connector: did not become healthy after apply: %v", err)
			return
		}
		// Now that the connector is reachable, mint tokens for every instance
		// that doesn't have one yet and restart any running instance whose
		// container env has drifted, so enabling the feature reaches existing
		// agents without a per-instance edit.
		pushConnectorTokensForRunningInstances()
	}()
}

// buildConnectorEnvVars returns the env vars buildCreateParams should inject
// for an instance's OpenConnector access: the base URL to reach the shared
// connector service and the instance's own scoped runtime token, when one
// has already been minted (see ensureInstanceConnectorToken). Read-only and
// side-effect-free, like buildSearchEnvVars, so it is safe to call from the
// env-drift check as well as from a real (re)create.
func buildConnectorEnvVars(inst *database.Instance) map[string]string {
	out := map[string]string{}
	if !connectorEnabled() || inst.ConnectorRuntimeToken == "" {
		return out
	}
	plain, err := utils.Decrypt(inst.ConnectorRuntimeToken)
	if err != nil || plain == "" {
		return out
	}
	out["OOMOL_CONNECT_RUNTIME_TOKEN"] = plain
	out["OPEN_CONNECTOR_BASE_URL"] = fmt.Sprintf("http://%s:%d", connectorprov.WorkloadName, connectorContainerPortForEnv)
	return out
}

// connectorContainerPortForEnv mirrors connectorprov's fixed container port.
// Duplicated rather than exported from connectorprov because the env var
// Claworc renders here is a Docker-bridge-network / cluster-DNS hostname:port
// pair meant for the *agent* container to dial directly -- a different
// concern from connectorprov.Manager.Address, which resolves the address the
// *control plane itself* should dial (host-published loopback port on
// Docker, Service DNS on Kubernetes). Agents always reach the connector by
// its stable in-network name, on both backends, since they run inside the
// same claworc bridge network / cluster namespace as the connector.
const connectorContainerPortForEnv = 3000

// ensureInstanceConnectorToken mints a scoped OpenConnector runtime token for
// inst if the feature is enabled and it doesn't already have one, persisting
// it (encrypted) onto the row and updating inst in place. Returns true when a
// token was freshly minted (the caller then knows the container needs the
// new env var, i.e. treat it like any other env-var drift).
//
// Best-effort: any failure (connector not reachable yet, admin API error) is
// logged and returns false rather than blocking whatever create/restart flow
// called it -- the agent simply boots without connector access this time
// and picks up a token on the next call once the connector is reachable.
func ensureInstanceConnectorToken(ctx context.Context, inst *database.Instance) (minted bool) {
	if !connectorEnabled() || inst.ConnectorRuntimeToken != "" {
		return false
	}
	orch := orchestrator.Get()
	if orch == nil {
		return false
	}
	cfg, ok := resolvedConnectorConfig()
	if !ok {
		return false
	}
	mgr := connectorprov.New(orch)
	host, port, err := mgr.Address(ctx)
	if err != nil {
		log.Printf("connector: cannot resolve address to mint token for instance %d: %v", inst.ID, err)
		return false
	}
	client := connectorprov.NewAdminClient(host, port, cfg.AdminToken)
	token, recordID, err := client.CreateRuntimeToken(ctx, connectorprov.RuntimeTokenSpec{
		Name: fmt.Sprintf("claworc-instance-%d", inst.ID),
	})
	if err != nil {
		log.Printf("connector: mint token for instance %d failed: %v", inst.ID, err)
		return false
	}
	encToken, err := utils.Encrypt(token)
	if err != nil {
		log.Printf("connector: encrypt token for instance %d failed: %v", inst.ID, err)
		return false
	}
	if err := database.DB.Model(&database.Instance{}).Where("id = ?", inst.ID).Updates(map[string]interface{}{
		"connector_runtime_token": encToken,
		"connector_token_id":      recordID,
	}).Error; err != nil {
		log.Printf("connector: persist token for instance %d failed: %v", inst.ID, err)
		return false
	}
	inst.ConnectorRuntimeToken = encToken
	inst.ConnectorTokenID = recordID
	return true
}

// revokeInstanceConnectorToken deletes inst's minted runtime token from the
// connector (best-effort) and clears the row. Called when an instance is
// deleted so the connector's token list doesn't accumulate orphans.
func revokeInstanceConnectorToken(ctx context.Context, inst database.Instance) {
	if inst.ConnectorTokenID == "" {
		return
	}
	orch := orchestrator.Get()
	if orch == nil {
		return
	}
	cfg, ok := resolvedConnectorConfig()
	if !ok {
		return
	}
	mgr := connectorprov.New(orch)
	host, port, err := mgr.Address(ctx)
	if err != nil {
		log.Printf("connector: cannot resolve address to revoke token for instance %d: %v", inst.ID, err)
		return
	}
	client := connectorprov.NewAdminClient(host, port, cfg.AdminToken)
	if err := client.RevokeRuntimeToken(ctx, inst.ConnectorTokenID); err != nil {
		log.Printf("connector: revoke token for instance %d failed: %v", inst.ID, err)
	}
}

// pushConnectorTokensForRunningInstances mints tokens for every instance that
// doesn't have one yet and restarts any running instance whose live container
// env has drifted from the database (new token minted, or the feature was
// just turned on). Mirrors pushSearchConfigForRunningInstances /
// UpdateSettings's env-var cascade: called once when connector_enabled flips
// on so existing instances don't have to be individually edited to pick up
// connector access.
func pushConnectorTokensForRunningInstances() {
	var instances []database.Instance
	database.DB.Find(&instances)
	ctx, cancel := context.WithTimeout(context.Background(), connectorApplyTimeout)
	defer cancel()
	for i := range instances {
		inst := instances[i]
		ensureInstanceConnectorToken(ctx, &inst)
		EnsureEnvPropagated(ctx, inst, 0, "OOMOL_CONNECT_RUNTIME_TOKEN", "OPEN_CONNECTOR_BASE_URL")
	}
}

// BootApplyConnector re-applies the managed connector workload on
// control-plane startup if connector_enabled is on. Mirrors how the browser
// bridge and tunnel manager reconcile their own workloads on boot rather
// than assuming whatever was last applied is still running — a control-plane
// restart must not require an admin to re-save Settings just to get the
// connector container back.
func BootApplyConnector() {
	if !connectorEnabled() {
		return
	}
	applyConnectorAsync(true)
}

// GetConnectorStatus handles GET /api/v1/connector/status (admin-only).
// Surfaces enough for the Settings page to show a live badge: enabled flag,
// coarse workload status, and whether secrets have been generated yet.
func GetConnectorStatus(w http.ResponseWriter, r *http.Request) {
	enabled := connectorEnabled()
	resp := map[string]interface{}{
		"enabled": enabled,
	}
	encKeyRaw, _ := database.GetSetting("connector_encryption_key")
	adminTokenRaw, _ := database.GetSetting("connector_admin_token")
	resp["configured"] = encKeyRaw != "" && adminTokenRaw != ""

	if !enabled {
		resp["status"] = "disabled"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	orch := orchestrator.Get()
	if orch == nil {
		resp["status"] = "unknown"
		resp["error"] = "orchestrator not available"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	mgr := connectorprov.New(orch)
	status, err := mgr.Status(r.Context())
	if err != nil {
		resp["status"] = "unknown"
		resp["error"] = err.Error()
	} else {
		resp["status"] = status
	}
	writeJSON(w, http.StatusOK, resp)
}

// ConnectorProxy proxies the OpenConnector Web Console + admin API to
// /connector/*, reusing the same in-process reverse-proxy path the
// per-instance OpenClaw dashboard uses at /openclaw/{id}/* (see
// control.go's ControlProxy) so no separately exposed port or URL is
// required to reach it -- admins hit Claworc's own origin.
//
// Every proxied request carries the connector's admin bearer token, so the
// caller only needs to already be an authenticated Claworc admin (enforced
// by the route's own middleware, see main.go) -- they never see or need the
// connector's own admin token.
func ConnectorProxy(w http.ResponseWriter, r *http.Request) {
	if !connectorEnabled() {
		writeError(w, http.StatusConflict, "the managed connector is disabled")
		return
	}
	orch := orchestrator.Get()
	if orch == nil {
		WriteOrchestratorUnavailable(w)
		return
	}
	cfg, ok := resolvedConnectorConfig()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "connector secrets are not configured yet")
		return
	}

	mgr := connectorprov.New(orch)
	host, port, err := mgr.Address(r.Context())
	if err != nil {
		writeConnectorConnectingPage(w)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/connector/")
	path = strings.TrimPrefix(path, "/connector")

	r.Header.Set("Authorization", "Bearer "+cfg.AdminToken)

	resp, err := doProxyRequestToHost(r, host, port, path)
	if err != nil {
		writeConnectorConnectingPage(w)
		return
	}
	if err := writeProxyResponse(w, resp, "/connector/"); err != nil {
		_ = err // already logged/partially written inside the helper
	}
}

func writeConnectorConnectingPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Retry-After", "2")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8">` +
		`<title>Connecting to OpenConnector...</title></head><body>` +
		`<p>The managed connector service is starting up. Refresh in a moment.</p>` +
		`<script>setTimeout(function(){location.reload()},2000)</script>` +
		`</body></html>`))
}
