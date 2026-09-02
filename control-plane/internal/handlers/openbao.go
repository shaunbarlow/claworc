package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gluk-w/claworc/control-plane/internal/database"
	"github.com/gluk-w/claworc/control-plane/internal/openbaoprov"
	"github.com/gluk-w/claworc/control-plane/internal/orchestrator"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
)

// openbao_enabled/openbao_image/openbao_storage are registered as plain
// settings, and openbao_root_token/openbao_unseal_key/openbao_admin_token
// as fixed-encrypted settings, directly in settings.go's plainSettings /
// fixedEncryptedSettings so GetSettings/UpdateSettings pick them up through
// the existing generic paths -- same pattern as the connector_* settings in
// connector.go.

const defaultOpenbaoImage = "openbao/openbao:latest"
const defaultOpenbaoStorage = "1Gi"

// openbaoApplyTimeout bounds the Apply/init/unseal/WaitHealthy round trip
// triggered from a settings save or boot. Generous for the same reason as
// connectorApplyTimeout: an image pull must not hang the caller forever.
const openbaoApplyTimeout = 5 * time.Minute

// openbaoTokenTTL is the fixed lifetime given to every per-instance orphan
// token this control plane mints. OpenBao has no literal "never expires"
// token mode outside the root token itself, so "long-lived" per Shaun's
// 2026-09-01 decision is implemented as a large fixed TTL instead of
// periodic renewal. ~10 years.
const openbaoTokenTTL = "87600h"

// openbaoAdminPolicyName is the ACL policy Claworc's own admin token is
// minted against once, right after init. Grants everything the control
// plane itself ever needs: full KV v2 CRUD (to manage per-agent and shared
// secrets, though in practice agents write their own KV data — Claworc
// itself only ever reads/writes the mount and policy layers) and policy
// management (to create/update per-instance policies on grant changes).
const openbaoAdminPolicyName = "claworc-admin"

// openbaoEnabled reports the current value of the openbao_enabled setting.
// Defaults to false: the managed OpenBao deployment is strictly opt-in.
func openbaoEnabled() bool {
	v, _ := database.GetSetting("openbao_enabled")
	return v == "true"
}

// resolvedOpenbaoConfig reads the openbao_* settings out of the database,
// returning an openbaoprov.Config ready for Apply. ok=false only when the
// image field is somehow empty (should not happen once defaulted at
// enable-time) -- unlike the connector, OpenBao's workload config carries no
// secret material of its own (BAO_LOCAL_CONFIG has no credentials in it),
// so there is nothing else that can leave this "not configured".
func resolvedOpenbaoConfig() (cfg openbaoprov.Config, ok bool) {
	image, _ := database.GetSetting("openbao_image")
	if image == "" {
		image = defaultOpenbaoImage
	}
	storage, _ := database.GetSetting("openbao_storage")
	if storage == "" {
		storage = defaultOpenbaoStorage
	}
	return openbaoprov.Config{Image: image, StorageSize: storage}, image != ""
}

// applyOpenbaoAsync applies (or, when enabled=false, stops) the OpenBao
// workload in the background, then runs ensureOpenbaoInitialized once it is
// reachable. Mirrors applyConnectorAsync's shape and reasoning: the HTTP
// settings-save request must not block on an image pull or the
// init/unseal round trip.
func applyOpenbaoAsync(enabled bool) {
	orch := orchestrator.Get()
	if orch == nil {
		log.Printf("openbao: orchestrator not available, skipping apply")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), openbaoApplyTimeout)
		defer cancel()
		mgr := openbaoprov.New(orch)
		if !enabled {
			// Stop, don't delete: the file-storage volume (and the
			// root token/unseal key/admin token already in settings) are
			// kept so re-enabling doesn't need re-init. See
			// openbaoprov.Manager.Delete's own doc comment for why that
			// destructive path is separate and not wired to this toggle.
			if err := orch.StopInstance(ctx, openbaoprov.WorkloadName); err != nil {
				log.Printf("openbao: stop failed: %v", err)
			}
			return
		}
		cfg, ok := resolvedOpenbaoConfig()
		if !ok {
			log.Printf("openbao: cannot apply, config not resolvable")
			return
		}
		if err := mgr.Apply(ctx, cfg); err != nil {
			log.Printf("openbao: apply failed: %v", err)
			return
		}
		if err := mgr.WaitHealthy(ctx, 2*time.Minute); err != nil {
			log.Printf("openbao: did not become reachable after apply: %v", err)
			return
		}
		if err := ensureOpenbaoInitialized(ctx, mgr); err != nil {
			log.Printf("openbao: init/unseal failed: %v", err)
			return
		}
		// Now that OpenBao is unsealed and the admin token exists, mint
		// tokens/policies for every instance that doesn't have one yet and
		// restart any running instance whose container env has drifted, so
		// enabling the feature reaches existing agents without a
		// per-instance edit -- same reasoning as
		// pushConnectorTokensForRunningInstances.
		pushOpenbaoTokensForRunningInstances()
	}()
}

// ensureOpenbaoInitialized runs OpenBao's one-time init sequence (init →
// store root token + unseal key → unseal → mint Claworc's own admin token →
// enable the KV v2 mount) and unseals it if merely sealed. Safe to call on
// every apply/boot: each step checks current state before acting, so a
// partially-completed prior run (e.g. init succeeded but the control plane
// crashed before minting the admin token) resumes cleanly rather than
// erroring on "already initialized".
func ensureOpenbaoInitialized(ctx context.Context, mgr *openbaoprov.Manager) error {
	host, port, err := mgr.Address(ctx)
	if err != nil {
		return fmt.Errorf("resolve address: %w", err)
	}

	// Bootstrap client: no token yet. SealStatus/Init/Unseal are the three
	// endpoints OpenBao itself does not require a token for.
	bootstrap := openbaoprov.NewAdminClient(host, port, "")
	status, err := bootstrap.SealStatus(ctx)
	if err != nil {
		return fmt.Errorf("seal status: %w", err)
	}

	rootTokenRaw, _ := database.GetSetting("openbao_root_token")
	unsealKeyRaw, _ := database.GetSetting("openbao_unseal_key")

	if !status.Initialized {
		initResp, err := bootstrap.Init(ctx)
		if err != nil {
			return fmt.Errorf("init: %w", err)
		}
		if initResp.RootToken == "" || len(initResp.Keys) == 0 {
			return fmt.Errorf("init: openbao did not return a root token and unseal key")
		}
		encRoot, err := utils.Encrypt(initResp.RootToken)
		if err != nil {
			return fmt.Errorf("encrypt root token: %w", err)
		}
		encKey, err := utils.Encrypt(initResp.Keys[0])
		if err != nil {
			return fmt.Errorf("encrypt unseal key: %w", err)
		}
		if err := database.SetSetting("openbao_root_token", encRoot); err != nil {
			return fmt.Errorf("persist root token: %w", err)
		}
		if err := database.SetSetting("openbao_unseal_key", encKey); err != nil {
			return fmt.Errorf("persist unseal key: %w", err)
		}
		rootTokenRaw = encRoot
		unsealKeyRaw = encKey
		status.Sealed = true // freshly initialized instances start sealed
	}

	if rootTokenRaw == "" || unsealKeyRaw == "" {
		return fmt.Errorf("openbao is initialized but claworc has no stored root token/unseal key -- manual recovery required")
	}
	unsealKey, err := utils.Decrypt(unsealKeyRaw)
	if err != nil || unsealKey == "" {
		return fmt.Errorf("decrypt unseal key: %w", err)
	}

	if status.Sealed {
		if err := bootstrap.Unseal(ctx, unsealKey); err != nil {
			return fmt.Errorf("unseal: %w", err)
		}
	}

	adminTokenRaw, _ := database.GetSetting("openbao_admin_token")
	if adminTokenRaw != "" {
		// Already bootstrapped past this point on a previous run. Re-push the
		// admin policy before using the token: OpenBao resolves a token's
		// policies by name on every request, so rewriting the policy under
		// the same name upgrades the capabilities of the already-minted
		// token in place, with no re-mint and no change to what is stored in
		// settings. Without this, a deployment that bootstrapped against an
		// older openbaoAdminPolicyDocument would keep its stale capabilities
		// forever, since the mint path below never runs again -- which is
		// exactly how the missing bare "sys/mounts" grant survived a
		// redeploy.
		rootToken, err := utils.Decrypt(rootTokenRaw)
		if err != nil || rootToken == "" {
			return fmt.Errorf("decrypt root token: %w", err)
		}
		root := openbaoprov.NewAdminClient(host, port, rootToken)
		if err := root.PutPolicy(ctx, openbaoAdminPolicyName, openbaoAdminPolicyDocument); err != nil {
			return fmt.Errorf("refresh admin policy: %w", err)
		}
		adminToken, err := utils.Decrypt(adminTokenRaw)
		if err != nil || adminToken == "" {
			return fmt.Errorf("decrypt admin token: %w", err)
		}
		admin := openbaoprov.NewAdminClient(host, port, adminToken)
		return admin.EnsureKVv2Mount(ctx)
	}

	// First-time bootstrap of the admin policy + orphan admin token, using
	// the root token exactly once. Per Shaun's 2026-09-01 decision the root
	// token itself is kept in settings for recovery purposes even though it
	// is not used again after this point in normal operation.
	rootToken, err := utils.Decrypt(rootTokenRaw)
	if err != nil || rootToken == "" {
		return fmt.Errorf("decrypt root token: %w", err)
	}
	root := openbaoprov.NewAdminClient(host, port, rootToken)
	if err := root.PutPolicy(ctx, openbaoAdminPolicyName, openbaoAdminPolicyDocument); err != nil {
		return fmt.Errorf("create admin policy: %w", err)
	}
	adminToken, err := root.CreateOrphanToken(ctx, []string{openbaoAdminPolicyName}, openbaoTokenTTL, false)
	if err != nil {
		return fmt.Errorf("mint admin token: %w", err)
	}
	encAdmin, err := utils.Encrypt(adminToken)
	if err != nil {
		return fmt.Errorf("encrypt admin token: %w", err)
	}
	if err := database.SetSetting("openbao_admin_token", encAdmin); err != nil {
		return fmt.Errorf("persist admin token: %w", err)
	}

	admin := openbaoprov.NewAdminClient(host, port, adminToken)
	return admin.EnsureKVv2Mount(ctx)
}

// openbaoAdminPolicyDocument grants Claworc's own admin token everything it
// needs going forward: full KV v2 CRUD under secret/ (mount setup and any
// direct inspection an admin UI action might need) and full control over
// ACL policies (to create/update per-instance policies whenever
// SecretGrants changes).
const openbaoAdminPolicyDocument = `
path "secret/data/*" {
  capabilities = ["create", "read", "update", "delete", "list"]
}
path "secret/metadata/*" {
  capabilities = ["create", "read", "update", "delete", "list"]
}
path "sys/policies/acl/*" {
  capabilities = ["create", "read", "update", "delete", "list"]
}
path "sys/mounts/*" {
  capabilities = ["create", "read", "update", "delete", "list"]
}
# The bare path as well as the glob: EnsureKVv2Mount lists mounts with
# GET /v1/sys/mounts, and an OpenBao path with a trailing /* matches only
# what follows the literal "sys/mounts/" prefix -- never "sys/mounts"
# itself, which would otherwise be denied.
path "sys/mounts" {
  capabilities = ["read", "list"]
}
`

// resolvedOpenbaoAdminClient returns an AdminClient authenticated with
// Claworc's own admin token, or ok=false if the feature isn't enabled, the
// orchestrator/address isn't available, or the admin token hasn't been
// minted yet (bootstrap not finished). Callers treat ok=false as
// "best-effort skip", matching every other openbao.go operation's failure
// posture.
func resolvedOpenbaoAdminClient(ctx context.Context) (client *openbaoprov.AdminClient, ok bool) {
	if !openbaoEnabled() {
		return nil, false
	}
	orch := orchestrator.Get()
	if orch == nil {
		return nil, false
	}
	adminTokenRaw, _ := database.GetSetting("openbao_admin_token")
	if adminTokenRaw == "" {
		return nil, false
	}
	adminToken, err := utils.Decrypt(adminTokenRaw)
	if err != nil || adminToken == "" {
		return nil, false
	}
	mgr := openbaoprov.New(orch)
	host, port, err := mgr.Address(ctx)
	if err != nil {
		log.Printf("openbao: address lookup failed: %v", err)
		return nil, false
	}
	return openbaoprov.NewAdminClient(host, port, adminToken), true
}

// instanceOpenbaoPolicyName is the per-instance ACL policy name, derived
// from the instance's stable UUID (not the sequential ID) so a policy name
// never collides across a delete+recreate at the same numeric ID and so it
// matches the same "non-enumerable identifier" convention UUID already
// serves elsewhere (webhook URLs, etc.).
func instanceOpenbaoPolicyName(inst *database.Instance) string {
	return "agent-" + inst.UUID
}

// instanceOpenbaoPolicyDocument builds the ACL policy document for inst:
// always full read+write on its own agent namespace, plus read-only or
// read+write on each shared set named in its SecretGrants, per capability.
// List/metadata access is granted alongside data access in both directions
// -- read-only grants still need "list" to enumerate keys within a shared
// set, which is a distinct OpenBao capability from "read" on a specific key.
func instanceOpenbaoPolicyDocument(inst *database.Instance) string {
	doc := fmt.Sprintf(`
path "secret/data/agents/%s/*" {
  capabilities = ["create", "read", "update", "delete", "list"]
}
path "secret/metadata/agents/%s/*" {
  capabilities = ["create", "read", "update", "delete", "list"]
}
`, inst.UUID, inst.UUID)

	for _, grant := range database.ParseSecretGrants(inst.SecretGrants) {
		if grant.SetName == "" {
			continue
		}
		dataCaps := `["read", "list"]`
		if grant.Capability == "write" {
			dataCaps = `["create", "read", "update", "delete", "list"]`
		}
		doc += fmt.Sprintf(`
path "secret/data/shared/%s/*" {
  capabilities = %s
}
path "secret/metadata/shared/%s/*" {
  capabilities = ["read", "list"]
}
`, grant.SetName, dataCaps, grant.SetName)
	}
	return doc
}

// applyInstanceOpenbaoPolicy upserts inst's ACL policy from its current
// SecretGrants. Safe to call whenever SecretGrants changes, and idempotent
// otherwise -- OpenBao re-evaluates a token's effective grants from its
// attached policies on every request, so rewriting the policy document
// takes effect immediately without touching the token itself.
func applyInstanceOpenbaoPolicy(ctx context.Context, inst *database.Instance) error {
	client, ok := resolvedOpenbaoAdminClient(ctx)
	if !ok {
		return fmt.Errorf("openbao not enabled or not ready")
	}
	return client.PutPolicy(ctx, instanceOpenbaoPolicyName(inst), instanceOpenbaoPolicyDocument(inst))
}

// ensureInstanceOpenbaoToken mints a long-lived orphan OpenBao token for
// inst if the feature is enabled and it doesn't already have one,
// (re)writing its policy either way, and persisting a freshly minted token
// (encrypted) onto the row. Returns true when a token was freshly minted
// (the caller then knows the container needs the new env var, i.e. treat it
// like any other env-var drift) -- a pure policy rewrite on an
// already-tokened instance does NOT return true, matching the integration
// plan's "policy change never needs a container restart" design.
//
// Best-effort: any failure is logged and returns false rather than blocking
// whatever create/edit flow called it.
func ensureInstanceOpenbaoToken(ctx context.Context, inst *database.Instance) (minted bool) {
	if !openbaoEnabled() {
		return false
	}
	client, ok := resolvedOpenbaoAdminClient(ctx)
	if !ok {
		return false
	}

	if err := client.PutPolicy(ctx, instanceOpenbaoPolicyName(inst), instanceOpenbaoPolicyDocument(inst)); err != nil {
		log.Printf("openbao: put policy for instance %d failed: %v", inst.ID, err)
		return false
	}

	if inst.OpenbaoToken != "" {
		return false
	}

	token, err := client.CreateOrphanToken(ctx, []string{instanceOpenbaoPolicyName(inst)}, openbaoTokenTTL, false)
	if err != nil {
		log.Printf("openbao: mint token for instance %d failed: %v", inst.ID, err)
		return false
	}
	encToken, err := utils.Encrypt(token)
	if err != nil {
		log.Printf("openbao: encrypt token for instance %d failed: %v", inst.ID, err)
		return false
	}
	if err := database.DB.Model(&database.Instance{}).Where("id = ?", inst.ID).
		Update("openbao_token", encToken).Error; err != nil {
		log.Printf("openbao: persist token for instance %d failed: %v", inst.ID, err)
		return false
	}
	inst.OpenbaoToken = encToken
	return true
}

// buildOpenbaoEnvVars returns the env vars buildCreateParams should inject
// for an instance's OpenBao access: the in-network base address and the
// instance's own long-lived token, when one has already been minted (see
// ensureInstanceOpenbaoToken). Read-only and side-effect-free, like
// buildConnectorEnvVars, so it is safe to call from the env-drift check as
// well as from a real (re)create.
func buildOpenbaoEnvVars(inst *database.Instance) map[string]string {
	out := map[string]string{}
	if !openbaoEnabled() || inst.OpenbaoToken == "" {
		return out
	}
	plain, err := utils.Decrypt(inst.OpenbaoToken)
	if err != nil || plain == "" {
		return out
	}
	out["OPENBAO_TOKEN"] = plain
	out["OPENBAO_ADDR"] = fmt.Sprintf("http://%s:%d", openbaoprov.WorkloadName, openbaoContainerPortForEnv)
	return out
}

// openbaoContainerPortForEnv mirrors openbaoprov's fixed container port.
// Duplicated rather than exported from openbaoprov for the same reason
// connectorContainerPortForEnv duplicates connectorprov's port: this is the
// Docker-bridge-network / cluster-DNS hostname:port pair meant for the
// *agent* container to dial directly, a different concern from
// openbaoprov.Manager.Address (which resolves the address the *control
// plane itself* should dial).
const openbaoContainerPortForEnv = 8200

// pushOpenbaoTokensForRunningInstances mints tokens/policies for every
// instance that doesn't have a token yet and restarts any running instance
// whose live container env has drifted from the database (new token
// minted, or the feature was just turned on). Mirrors
// pushConnectorTokensForRunningInstances.
func pushOpenbaoTokensForRunningInstances() {
	var instances []database.Instance
	database.DB.Find(&instances)
	ctx, cancel := context.WithTimeout(context.Background(), openbaoApplyTimeout)
	defer cancel()
	for i := range instances {
		inst := instances[i]
		ensureInstanceOpenbaoToken(ctx, &inst)
		EnsureEnvPropagated(ctx, inst, 0, "OPENBAO_TOKEN", "OPENBAO_ADDR")
	}
}

// BootApplyOpenbao re-applies the managed OpenBao workload on control-plane
// startup if openbao_enabled is on. Mirrors BootApplyConnector: a
// control-plane restart must not require an admin to re-save Settings just
// to get the OpenBao container (and its auto-unseal) back.
func BootApplyOpenbao() {
	if !openbaoEnabled() {
		return
	}
	applyOpenbaoAsync(true)
}

// GetOpenbaoStatus handles GET /api/v1/openbao/status (admin-only). Mirrors
// GetConnectorStatus's shape: workload lifecycle status plus enough
// configuration-state signal for the Settings UI to show "not initialized
// yet" vs "sealed" vs "ready" without the caller needing to know OpenBao's
// own API.
func GetOpenbaoStatus(w http.ResponseWriter, r *http.Request) {
	enabled := openbaoEnabled()
	resp := map[string]interface{}{
		"enabled": enabled,
	}
	adminTokenRaw, _ := database.GetSetting("openbao_admin_token")
	resp["configured"] = adminTokenRaw != ""

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
	mgr := openbaoprov.New(orch)
	status, err := mgr.Status(r.Context())
	if err != nil {
		resp["status"] = "unknown"
		resp["error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["status"] = status

	// Best-effort seal-status probe: only meaningful once the workload is up.
	if status == "running" {
		if host, port, err := mgr.Address(r.Context()); err == nil {
			client := openbaoprov.NewAdminClient(host, port, "")
			if sealStatus, err := client.SealStatus(r.Context()); err == nil {
				resp["initialized"] = sealStatus.Initialized
				resp["sealed"] = sealStatus.Sealed
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
