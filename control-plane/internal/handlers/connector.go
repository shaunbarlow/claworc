package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gluk-w/claworc/control-plane/internal/analytics"
	"github.com/gluk-w/claworc/control-plane/internal/config"
	"github.com/gluk-w/claworc/control-plane/internal/connectorprov"
	"github.com/gluk-w/claworc/control-plane/internal/database"
	"github.com/gluk-w/claworc/control-plane/internal/orchestrator"
	"github.com/gluk-w/claworc/control-plane/internal/sshproxy"
	"github.com/gluk-w/claworc/control-plane/internal/taskmanager"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
)

// connectorApplyTimeout bounds the Apply/Delete/WaitHealthy round trip
// triggered from a settings save. Image pulls can be slow on first run, so
// this is generous, but still request-scoped: a stuck orchestrator must not
// hang the goroutine forever.
const connectorApplyTimeout = 5 * time.Minute

// connector_enabled/connector_image/connector_storage/connector_origin/
// connector_allowed_custom_oauth are registered as plain settings, and
// connector_encryption_key/connector_admin_token as fixed-encrypted
// settings, directly in settings.go's plainSettings / fixedEncryptedSettings
// so GetSettings/UpdateSettings pick them up through the existing generic
// paths.

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
	explicitOrigin, _ := database.GetSetting("connector_origin")
	origin, _ := resolveConnectorOrigin(explicitOrigin)
	allowedCustomOAuth, _ := database.GetSetting("connector_allowed_custom_oauth")

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
		Image:              image,
		EncryptionKey:      encKey,
		AdminToken:         adminToken,
		Origin:             origin,
		AllowedCustomOAuth: allowedCustomOAuth,
		StorageSize:        storage,
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

// buildConnectorAdminEnvVars returns the env vars buildCreateParams should
// inject for an instance's opt-in OpenConnector *admin* access
// (Instance.ConnectorAdminAccessEnabled): the shared connector's own admin
// bearer token, letting the agent call OpenConnector's admin API directly
// (e.g. to manage its own runtime tokens/policy) instead of only the scoped
// runtime token buildConnectorEnvVars carries.
//
// Deliberately independent of ConnectorMCPEnabled/buildConnectorEnvVars:
// admin access is a materially bigger grant (full admin control of the
// shared connector, not just this agent's own scoped token) so it is its
// own opt-in rather than implied by the MCP one. Read-only and
// side-effect-free, like buildConnectorEnvVars, so it is safe to call from
// the env-drift check as well as from a real (re)create.
func buildConnectorAdminEnvVars(inst *database.Instance) map[string]string {
	out := map[string]string{}
	if !connectorEnabled() || !inst.ConnectorAdminAccessEnabled {
		return out
	}
	cfg, ok := resolvedConnectorConfig()
	if !ok || cfg.AdminToken == "" {
		return out
	}
	out["OOMOL_CONNECT_ADMIN_TOKEN"] = cfg.AdminToken
	out["OPEN_CONNECTOR_BASE_URL"] = fmt.Sprintf("http://%s:%d", connectorprov.WorkloadName, connectorContainerPortForEnv)
	return out
}

// connectorMCPServerName is the config key Claworc registers the managed
// connector under in an opted-in instance's own OpenClaw config, i.e.
// mcp.servers.open-connector.
const connectorMCPServerName = "open-connector"

// buildConnectorMCPConfig resolves the mcp.servers.open-connector subtree
// Claworc pushes into an instance's own OpenClaw config once it has opted
// into ConnectorMCPEnabled and been minted a token, carrying the instance's
// own scoped runtime token as a static Authorization header (see
// docs/cli/mcp.md's SSE/HTTP transport `headers` field). OpenConnector's
// /mcp endpoint accepts a runtime-scoped bearer the same way its REST API
// does (see the fork's server/api/auth.go path classification for /mcp).
//
// ok=false means there is nothing to push yet: feature off, this instance
// hasn't opted in, or no token has been minted yet (self-heals the next
// time ensureInstanceConnectorToken succeeds and this is called again).
func buildConnectorMCPConfig(inst *database.Instance) (cfg map[string]interface{}, ok bool) {
	if !connectorEnabled() || !inst.ConnectorMCPEnabled || inst.ConnectorRuntimeToken == "" {
		return nil, false
	}
	plain, err := utils.Decrypt(inst.ConnectorRuntimeToken)
	if err != nil || plain == "" {
		return nil, false
	}
	return map[string]interface{}{
		"url":       fmt.Sprintf("http://%s:%d/mcp", connectorprov.WorkloadName, connectorContainerPortForEnv),
		"transport": "streamable-http",
		"headers": map[string]string{
			"Authorization": "Bearer " + plain,
		},
		"connectionTimeoutMs": 10000,
		"requestTimeoutMs":    30000,
	}, true
}

// applyConnectorMCPConfig reconciles an instance's mcp.servers.open-connector
// OpenClaw config over SSH, in both directions: writes it when the instance
// has opted in and has a live token, clears it otherwise (never opted in,
// opted back out, feature disabled, or the token was revoked). Mirrors
// applySearchConfig's bidirectional reconcile pattern. Best-effort: failures
// are logged, not returned, same as the rest of this integration.
func applyConnectorMCPConfig(ctx context.Context, agent sshproxy.Instance, name string, inst *database.Instance) {
	name = utils.SanitizeForLog(name)
	cfg, ok := buildConnectorMCPConfig(inst)
	if !ok {
		_, stderr, code, err := agent.ExecOpenclaw(ctx, "config", "unset", "mcp.servers."+connectorMCPServerName)
		if err != nil {
			log.Printf("connector-mcp: %s: unset mcp server: %v", name, err)
			return
		}
		if code != 0 && !strings.Contains(stderr, "Config path not found") {
			log.Printf("connector-mcp: %s: unset mcp server failed: %s", name, utils.SanitizeForLog(stderr))
		}
		return
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("connector-mcp: %s: marshal mcp server config: %v", name, err)
		return
	}
	_, stderr, code, err := agent.ExecOpenclaw(ctx, "config", "set", "mcp.servers."+connectorMCPServerName, string(payload), "--replace", "--json")
	if err != nil {
		log.Printf("connector-mcp: %s: set mcp server: %v", name, err)
		return
	}
	if code != 0 {
		log.Printf("connector-mcp: %s: set mcp server failed: %s", name, utils.SanitizeForLog(stderr))
	}
}

// pushConnectorMCPConfig is the async best-effort wrapper around
// applyConnectorMCPConfig for a running instance, mirroring pushSearchConfig.
func pushConnectorMCPConfig(instanceID uint, name string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), searchConfigSSHWait)
		defer cancel()
		sshClient, err := SSHMgr.WaitForSSH(ctx, instanceID, searchConfigSSHWait)
		if err != nil {
			log.Printf("connector-mcp: %s: could not get SSH connection to push mcp config: %v", utils.SanitizeForLog(name), err)
			return
		}
		var inst database.Instance
		if err := database.DB.First(&inst, instanceID).Error; err != nil {
			log.Printf("connector-mcp: %s: could not re-read instance %d: %v", utils.SanitizeForLog(name), instanceID, err)
			return
		}
		applyConnectorMCPConfig(ctx, sshproxy.NewSSHInstance(sshClient), name, &inst)
	}()
}

// connectorTestOnlyServices lists the no_auth (no credential/API key setup
// required) provider services granted to every freshly minted instance
// token by default. This is deliberately narrow: Level 1 has no Claworc UI
// for per-agent policy yet (see
// docs/planning/open-connector-integration-plan.md's Level 2+ note), so a
// wide-open default token would hand every agent instance unrestricted
// access to every configured connection (including ones an admin added
// credentials for) the moment the feature is enabled. Restricting the
// default grant to a curated set of no_auth/public-API services lets agents
// exercise the connector for real (and lets an admin verify the integration
// end-to-end) without silently granting access to anything that requires a
// stored credential. Widening a specific instance's grant is a manual
// PUT /api/runtime-tokens/:id via the connector's own admin API/Web Console
// (reachable at /connector/* -- see docs/openconnector.md) until per-agent
// policy is exposed in the Claworc UI.
//
// Sourced from the shipped catalog (`grep -rl '"no_auth"' src/providers/*/definition.ts`
// in the open-connector fork) as of this writing; re-verify against the
// fork's catalog if `connector_image` is pointed at a build with catalog
// changes.
var connectorTestOnlyServices = []string{
	"arxiv",
	"hackernews",
	"wttr_in",
	"quickchart",
}

// defaultConnectorTokenPolicy returns the restrictive default policy every
// instance token is minted with: action access scoped to
// connectorTestOnlyServices (via a `service.*` allow rule per service, the
// syntax open-connector's policy-input validator requires -- see its
// assertRuleSyntax), no provider proxy access at all (`AllowedProxies: []`,
// deny-by-default per docs/runtime-api.md), and no stored-credential
// connection grant (`AllowedConnections: []`, which does not need to include
// anything since no_auth virtual connections never require a grant -- see
// runtime-api.md's "virtual no_auth connections do not require grants").
func defaultConnectorTokenPolicy() (allowedActions, allowedProxies, allowedConnections []string) {
	actions := make([]string, len(connectorTestOnlyServices))
	for i, svc := range connectorTestOnlyServices {
		actions[i] = svc + ".*"
	}
	return actions, []string{}, []string{}
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

// connectorTokenName builds the descriptive name Claworc mints an instance's
// runtime token under, e.g. "Claworc agent: Research Bot (bot-research-bot,
// #42)". Shown as-is in the connector's own Web Console token list, so it
// needs to identify which agent instance a token belongs to without an
// admin having to cross-reference IDs against Claworc's own instance list --
// the previous "claworc-instance-42" name required exactly that lookup.
func connectorTokenName(inst *database.Instance) string {
	return fmt.Sprintf("Claworc agent: %s (%s, #%d)", inst.DisplayName, inst.Name, inst.ID)
}

// syncConnectorTokenNameAndPolicy reconciles an instance's token name and
// policy at the connector side (if it exists) to match the Claworc canonical
// state: descriptive name + restrictive default policy per
// connectorTokenName() and defaultConnectorTokenPolicy(). Returns true when
// the token was updated at the connector (not an error, just a state change
// detected and synced).
//
// This is idempotent and best-effort: used in ensureInstanceConnectorToken
// when a token already exists (to upgrade old-style unrestricted or
// poorly-named tokens to the new standard) and later during token sync
// operations (see syncAllInstanceTokens).
func syncConnectorTokenNameAndPolicy(ctx context.Context, inst *database.Instance) bool {
	if inst.ConnectorTokenID == "" {
		return false
	}
	cfg, ok := resolvedConnectorConfig()
	if !ok {
		return false
	}
	orch := orchestrator.Get()
	if orch == nil {
		return false
	}
	mgr := connectorprov.New(orch)
	host, port, err := mgr.Address(ctx)
	if err != nil {
		log.Printf("connector: sync token name/policy for instance %d: address lookup failed: %v", inst.ID, err)
		return false
	}
	client := connectorprov.NewAdminClient(host, port, cfg.AdminToken)
	allowedActions, allowedProxies, allowedConnections := defaultConnectorTokenPolicy()
	_, err = client.UpdateTokenNameAndPolicy(
		ctx,
		inst.ConnectorTokenID,
		connectorTokenName(inst),
		connectorprov.RuntimeTokenSpec{
			AllowedActions:     allowedActions,
			AllowedProxies:     allowedProxies,
			AllowedConnections: allowedConnections,
		},
	)
	if err != nil {
		log.Printf("connector: sync token name/policy for instance %d failed: %v", inst.ID, err)
		return false
	}
	return true
}

// revokeInstanceConnectorTokenIfPresent clears inst's minted OpenConnector
// runtime token when present, revoking it at the connector first
// (best-effort). Called from ensureInstanceConnectorToken whenever the
// instance isn't (or is no longer) opted in via ConnectorMCPEnabled, so
// turning that opt-in off actually revokes the grant rather than merely
// leaving an unused token sitting at the connector. Returns true if a token
// was cleared -- the caller then knows the container env / MCP config need
// to be reconciled away, same as a freshly minted token needs reconciling
// in.
func revokeInstanceConnectorTokenIfPresent(ctx context.Context, inst *database.Instance) bool {
	if inst.ConnectorRuntimeToken == "" && inst.ConnectorTokenID == "" {
		return false
	}
	revokeInstanceConnectorToken(ctx, *inst)
	if err := database.DB.Model(&database.Instance{}).Where("id = ?", inst.ID).Updates(map[string]interface{}{
		"connector_runtime_token": "",
		"connector_token_id":      "",
	}).Error; err != nil {
		log.Printf("connector: clear token for instance %d failed: %v", inst.ID, err)
	}
	inst.ConnectorRuntimeToken = ""
	inst.ConnectorTokenID = ""
	return true
}

// ensureInstanceConnectorToken mints a scoped OpenConnector runtime token for
// inst if the feature is enabled, this instance has opted in
// (ConnectorMCPEnabled), and it doesn't already have one, persisting it
// (encrypted) onto the row and updating inst in place. Returns true when a
// token was freshly minted, revoked, or cleared (the caller then knows the
// container/MCP config needs to be reconciled, i.e. treat it like any other
// env-var drift).
//
// The minted token carries connectorTokenName's descriptive label and
// defaultConnectorTokenPolicy's restrictive default grant (a curated set of
// no_auth test-purpose services, no proxy access, no stored-credential
// connection access) -- see those doc comments for why. An admin who wants
// this instance to reach more of the catalog widens its token's policy
// directly via the connector's own admin API/Web Console
// (PUT /api/runtime-tokens/:id, reachable at /connector/*); Claworc does not
// yet expose per-agent policy editing in its own UI.
//
// Best-effort: any failure (connector not reachable yet, admin API error) is
// logged and returns false rather than blocking whatever create/restart flow
// called it -- the agent simply boots without connector access this time
// and picks up a token on the next call once the connector is reachable.
func ensureInstanceConnectorToken(ctx context.Context, inst *database.Instance) (minted bool) {
	if !connectorEnabled() || !inst.ConnectorMCPEnabled {
		// Feature off globally, or this instance hasn't opted in (default
		// for every instance now -- see Instance.ConnectorMCPEnabled).
		// Revoke/clear anything left over from before the opt-in existed or
		// from this instance opting back out.
		return revokeInstanceConnectorTokenIfPresent(ctx, inst)
	}
	// If token already exists, sync its name and policy to canonical state
	if inst.ConnectorRuntimeToken != "" {
		if inst.ConnectorTokenID != "" {
			return syncConnectorTokenNameAndPolicy(ctx, inst)
		}
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
	allowedActions, allowedProxies, allowedConnections := defaultConnectorTokenPolicy()
	token, recordID, err := client.CreateRuntimeToken(ctx, connectorprov.RuntimeTokenSpec{
		Name:               connectorTokenName(inst),
		AllowedActions:     allowedActions,
		AllowedProxies:     allowedProxies,
		AllowedConnections: allowedConnections,
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

// pushConnectorTokensForRunningInstances mints/revokes tokens for every
// instance to match its own ConnectorMCPEnabled opt-in and restarts any
// running instance whose live container env has drifted from the database
// (new token minted, revoked, or the feature was just turned on). Mirrors
// pushSearchConfigForRunningInstances / UpdateSettings's env-var cascade:
// called once when connector_enabled flips on so already-opted-in instances
// don't have to be individually edited to pick up connector access.
//
// Since ConnectorMCPEnabled became a per-instance opt-in, this only mints a
// token for instances that already have it set (e.g. from creation) --
// flipping the global connector_enabled setting no longer implicitly opts
// every instance in.
func pushConnectorTokensForRunningInstances() {
	var instances []database.Instance
	database.DB.Find(&instances)
	ctx, cancel := context.WithTimeout(context.Background(), connectorApplyTimeout)
	defer cancel()
	for i := range instances {
		inst := instances[i]
		ensureInstanceConnectorToken(ctx, &inst)
		EnsureEnvPropagated(ctx, inst, 0, "OOMOL_CONNECT_RUNTIME_TOKEN", "OPEN_CONNECTOR_BASE_URL", "OOMOL_CONNECT_ADMIN_TOKEN")
		pushConnectorMCPConfig(inst.ID, inst.Name)
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

// resolveConnectorOrigin returns the value that should be sent to
// OpenConnector as OOMOL_CONNECT_ORIGIN, plus whether it was explicitly
// configured ("explicit") or derived automatically from Claworc's own
// public origin ("auto"). An admin-supplied connector_origin setting always
// wins; otherwise this derives the origin OpenConnector needs from
// config.Cfg.RPOrigins -- the same "what is Claworc's own public URL" value
// already relied on for WebAuthn RP validation -- plus the "/connector"
// prefix that ConnectorProxy mounts everything under (see ConnectorProxy's
// doc comment and connectorprov.Config.Origin's doc comment for why the
// bare host is wrong here: OpenConnector hardcodes "/oauth/callback" onto
// whatever origin it's given, and that request only ever reaches
// OpenConnector via Claworc's /connector/* proxy).
//
// Left unset (both no explicit setting and no usable RPOrigins entry), this
// returns "" and callers fall back to whatever connectorprov/OpenConnector
// itself defaults to (http://localhost:<port>, unreachable from a real
// provider redirect) -- matching the previous "unset means OAuth silently
// doesn't work" behavior for the one case (no configured public origin at
// all) where auto-derivation has nothing sensible to derive from.
func resolveConnectorOrigin(explicit string) (origin string, source string) {
	explicit = strings.TrimRight(strings.TrimSpace(explicit), "/")
	if explicit != "" {
		return explicit, "explicit"
	}
	for _, o := range config.Cfg.RPOrigins {
		o = strings.TrimRight(strings.TrimSpace(o), "/")
		if o == "" {
			continue
		}
		return o + "/connector", "auto"
	}
	return "", "auto"
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

	explicitOrigin, _ := database.GetSetting("connector_origin")
	resolvedOrigin, originSource := resolveConnectorOrigin(explicitOrigin)
	resp["resolved_origin"] = resolvedOrigin
	resp["origin_source"] = originSource

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

// UpdateConnectorImage handles POST /api/v1/connector/update-image
// (admin-only). Forces a fresh pull-and-recreate of the managed connector
// workload against whatever image reference is currently configured
// (connector_image, default "ghcr.io/shaunbarlow/open-connector:tip").
//
// This exists because the connector is normally deployed from a mutable tag
// ("tip") that gets pushed to repeatedly upstream. Before this handler there
// was no way to make Claworc actually fetch a newer image under that same
// tag once it had been pulled once: toggling connector_enabled off/on
// re-applies the *cached* image (mgr.Apply's Pull policy is now PullAlways,
// see connectorprov.buildSpec, but that only helps once Apply actually runs
// again), and there was no button/endpoint that called Apply on demand
// without also flipping the enabled state. This is the explicit "pull
// latest and restart" action, mirroring SelfUpdateControlPlane's role for
// the control-plane's own image and UpdateInstanceImage's role for agent
// instances.
func UpdateConnectorImage(w http.ResponseWriter, r *http.Request) {
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

	userID := callerID(r)
	log.Printf("Connector image update requested by user %d (image=%s)", userID, cfg.Image)
	analytics.Track(r.Context(), analytics.EventConnectorImageUpdated, nil)

	work := func(ctx context.Context) error {
		mgr := connectorprov.New(orch)
		// mgr.Apply always force-pulls (spec.Pull is PullAlways for this
		// workload) and unconditionally stops+recreates the container
		// (Docker) / rolls the Deployment (Kubernetes) -- unlike
		// SelfUpdate's digest short-circuit, there is no "already up to
		// date, skip" path here: this handler is the explicit, deliberate
		// "pull now" action, so it always restarts once secrets/config are
		// present. See buildSpec's Pull field comment for why a mutable tag
		// needs PullAlways rather than PullIfNotPresent.
		if err := mgr.Apply(ctx, cfg); err != nil {
			return fmt.Errorf("apply connector workload: %w", err)
		}
		if err := mgr.WaitHealthy(ctx, 2*time.Minute); err != nil {
			return fmt.Errorf("connector did not become healthy after update: %w", err)
		}
		return nil
	}

	taskID := ""
	if TaskMgr != nil {
		taskID = TaskMgr.Start(taskmanager.StartOpts{
			Type:         taskmanager.TaskConnectorImageUpdate,
			UserID:       userID,
			ResourceName: "OpenConnector",
			Title:        "Updating managed OpenConnector",
			Run: func(ctx context.Context, h *taskmanager.Handle) error {
				h.UpdateMessage("Pulling latest connector image...")
				if err := work(ctx); err != nil {
					log.Printf("Connector image update failed: %v", err)
					return err
				}
				h.UpdateMessage("Connector updated and healthy.")
				return nil
			},
		})
	} else {
		go func() {
			// Independent background context: the HTTP request context is
			// canceled the moment this handler returns, but the pull +
			// container recreate must run to completion regardless.
			if err := work(context.Background()); err != nil {
				log.Printf("Connector image update (no task manager) failed: %v", err)
			}
		}()
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "updating",
		"task_id": taskID,
		"detail":  fmt.Sprintf("Pulling %s and restarting the managed connector.", cfg.Image),
	})
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
