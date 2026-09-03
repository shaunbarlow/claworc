package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/gluk-w/claworc/control-plane/internal/database"
	"github.com/gluk-w/claworc/control-plane/internal/middleware"
	"github.com/gluk-w/claworc/control-plane/internal/openbaoprov"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
)

// openbao_secrets.go implements the per-agent secret browser: list every
// secret in one instance's own OpenBao namespace
// (secret/agents/<instance-uuid>/**), set a single field on any path
// (creating the secret if it does not exist yet), reveal one value on
// demand, and delete a field or a whole entry.
//
// Deliberately scoped to one instance's own namespace and nothing else:
// every path this file addresses is built by prefixing
// instanceSecretBasePath(inst), so no request shape -- however malformed --
// can reach another agent's secrets or an admin-managed shared set. Shared
// sets stay managed by the grant editor (SecretGrants, see openbao.go),
// which is about *access*, not content.
//
// The calls go out over Claworc's own admin token (full CRUD on
// secret/data/* and secret/metadata/*, see openbaoAdminPolicyDocument), not
// the agent's token: an admin must be able to seed a secret for an agent
// that is stopped, tokenless, or has never booted.

// maxSecretEntries and maxSecretListDepth bound the recursive walk of an
// agent's namespace. An agent is free to write an arbitrarily deep tree of
// secrets and each leaf costs one read, so the walk needs a ceiling to keep
// one page load from turning into thousands of round trips. Both are
// generous relative to any plausible hand-managed secret set; hitting
// either sets truncated=true in the response so the UI can say so rather
// than silently showing a partial list.
const (
	maxSecretEntries   = 200
	maxSecretListDepth = 6
)

// secretPathSegmentRegex constrains one segment of a secret path, and
// secretFieldKeyRegex one field name within a secret. Both are enforced on
// write/reveal/delete input only -- names *read back* from OpenBao are
// rendered as-is, since a secret an agent already wrote through the bao CLI
// is real whether or not it matches Claworc's own preferred shape.
//
// The charset is OpenBao's own practical path charset minus anything that
// would need URL escaping on the way back out, which keeps path handling
// free of escaping questions (same reasoning as secretSetNameRegex in
// openbao_shared_sets.go). "." and ".." are rejected outright below so no
// input can walk up out of the agent's own prefix.
var (
	secretPathSegmentRegex = regexp.MustCompile(`^[A-Za-z0-9._~@+-]{1,64}$`)
	secretFieldKeyRegex    = regexp.MustCompile(`^[A-Za-z0-9._~@+-]{1,128}$`)
)

// instanceSecretBasePath is the logical KV v2 prefix owned by inst, without
// the data/ or metadata/ infix (AdminClient adds that) and with a trailing
// slash so it can be concatenated with a relative path directly. Must stay
// in lockstep with instanceOpenbaoPolicyDocument's secret/data/agents/<uuid>/*
// grant, which is what makes this prefix the agent's own.
func instanceSecretBasePath(inst *database.Instance) string {
	return "agents/" + inst.UUID + "/"
}

// validateSecretRelPath reports whether p is a well-formed path *relative to*
// an agent's own namespace: one or more segments, no leading/trailing or
// doubled slashes, and no "." or ".." segment.
func validateSecretRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return false
	}
	segments := strings.Split(p, "/")
	if len(segments) > maxSecretListDepth {
		return false
	}
	for _, seg := range segments {
		if seg == "." || seg == ".." || !secretPathSegmentRegex.MatchString(seg) {
			return false
		}
	}
	return true
}

type secretFieldResponse struct {
	Key string `json:"key"`
	// Masked is the value rendered as "****" + its last 4 characters, per
	// the project-wide convention that the API never returns a secret in
	// full (see the API-key masking rule in CLAUDE.md). The plaintext is
	// available only from RevealInstanceSecret, one field at a time.
	Masked string `json:"masked"`
}

type secretEntryResponse struct {
	// Path is relative to the agent's own namespace ("github/token"), which
	// is also the form every write/reveal/delete request takes -- the UI
	// never has to know the secret/agents/<uuid>/ prefix.
	Path      string                `json:"path"`
	Version   int                   `json:"version"`
	UpdatedAt string                `json:"updated_at"`
	Fields    []secretFieldResponse `json:"fields"`
}

type instanceSecretsResponse struct {
	// Enabled mirrors openbao_enabled; Ready additionally means the
	// workload is reachable and bootstrapped. Both are reported instead of
	// erroring so the panel can render "not enabled" / "still starting"
	// states rather than an error toast during a boot race.
	Enabled bool `json:"enabled"`
	Ready   bool `json:"ready"`
	// BasePath is the full KV v2 path of this agent's namespace, shown in
	// the UI so an admin can copy it into a `bao kv` command.
	BasePath  string                `json:"base_path"`
	Entries   []secretEntryResponse `json:"entries"`
	Truncated bool                  `json:"truncated"`
}

// resolveInstanceSecretRequest performs the checks every handler in this
// file shares: parse the instance ID, require an admin (secret *values* are
// a strictly higher bar than the rest of the instance API, which team
// managers can reach -- grants are admin-only too, see AgentDetailPage),
// load the instance, and resolve an OpenBao admin client.
//
// ok=false means a response has already been written. clientReady=false with
// ok=true means the feature is off or not bootstrapped yet, which only the
// list handler treats as a normal state.
func resolveInstanceSecretRequest(w http.ResponseWriter, r *http.Request) (inst *database.Instance, client *openbaoprov.AdminClient, ok bool) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid instance ID")
		return nil, nil, false
	}
	user := middleware.GetUser(r)
	if user == nil || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "Admin access required")
		return nil, nil, false
	}
	var found database.Instance
	if err := database.DB.First(&found, uint(id)).Error; err != nil {
		writeError(w, http.StatusNotFound, "Instance not found")
		return nil, nil, false
	}
	c, ready := resolvedOpenbaoAdminClient(r.Context())
	if !ready {
		return &found, nil, true
	}
	return &found, c, true
}

// ListInstanceSecrets handles GET /api/v1/instances/{id}/secrets (admin
// only): every secret in this agent's own OpenBao namespace, with field
// names and masked values.
func ListInstanceSecrets(w http.ResponseWriter, r *http.Request) {
	inst, client, ok := resolveInstanceSecretRequest(w, r)
	if !ok {
		return
	}
	resp := instanceSecretsResponse{
		Enabled:  openbaoEnabled(),
		Ready:    client != nil,
		BasePath: "secret/" + strings.TrimSuffix(instanceSecretBasePath(inst), "/"),
		Entries:  []secretEntryResponse{},
	}
	if client == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	entries, truncated, err := walkInstanceSecrets(r.Context(), client, instanceSecretBasePath(inst))
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("Failed to list secrets: %v", err))
		return
	}
	resp.Entries = entries
	resp.Truncated = truncated
	writeJSON(w, http.StatusOK, resp)
}

// walkInstanceSecrets recursively enumerates every secret under base,
// reading each leaf so its field names can be reported. Returns entries
// sorted by path, and truncated=true if either bound was hit.
func walkInstanceSecrets(ctx context.Context, client *openbaoprov.AdminClient, base string) (entries []secretEntryResponse, truncated bool, err error) {
	entries = []secretEntryResponse{}
	var walk func(rel string, depth int) error
	walk = func(rel string, depth int) error {
		keys, err := client.ListSecretPaths(ctx, base+rel)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if len(entries) >= maxSecretEntries {
				truncated = true
				return nil
			}
			if strings.HasSuffix(key, "/") {
				if depth+1 >= maxSecretListDepth {
					truncated = true
					continue
				}
				if err := walk(rel+key, depth+1); err != nil {
					return err
				}
				continue
			}
			entry, err := client.ReadSecret(ctx, base+rel+key)
			if err != nil {
				// A key can be listed but unreadable when its latest
				// version was soft-deleted (metadata survives a `bao kv
				// delete`, data does not). Skip it rather than failing the
				// whole page: from an admin's point of view that secret has
				// no current value.
				if errors.Is(err, openbaoprov.ErrNotFound) {
					continue
				}
				return err
			}
			entries = append(entries, secretEntryResponse{
				Path:      rel + key,
				Version:   entry.Version,
				UpdatedAt: entry.CreatedTime,
				Fields:    maskSecretFields(entry.Fields),
			})
		}
		return nil
	}
	if err := walk("", 0); err != nil {
		return nil, false, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, truncated, nil
}

// maskSecretFields renders a secret's field map for display: field names in
// sorted order, values masked. Non-string values (possible via OpenBao's raw
// API, never via `bao kv put`) are stringified first so their shape is
// masked the same way rather than leaking through a JSON encode.
func maskSecretFields(fields map[string]interface{}) []secretFieldResponse {
	out := make([]secretFieldResponse, 0, len(fields))
	for k, v := range fields {
		out = append(out, secretFieldResponse{Key: k, Masked: utils.Mask(secretValueToString(v))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func secretValueToString(v interface{}) string {
	switch typed := v.(type) {
	case string:
		return typed
	case nil:
		return ""
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprintf("%v", typed)
		}
		return string(encoded)
	}
}

type instanceSecretWriteRequest struct {
	// Path is relative to the agent's namespace, e.g. "github/token".
	Path  string `json:"path"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

// PutInstanceSecret handles PUT /api/v1/instances/{id}/secrets (admin
// only): set one field on one secret in this agent's namespace, creating
// the secret (and any intermediate path prefixes) if it does not exist yet.
//
// Read-merge-write rather than a bare write, because KV v2 replaces a
// secret's entire field set on every write: setting "password" on a secret
// that also holds "username" with a plain write would silently drop the
// username. The merge is not atomic against a concurrent writer (OpenBao
// offers a check-and-set option for that), which is an accepted limit here
// -- the competing writer would be the agent itself editing the very same
// field of the very same secret in the same instant.
func PutInstanceSecret(w http.ResponseWriter, r *http.Request) {
	inst, client, ok := resolveInstanceSecretRequest(w, r)
	if !ok {
		return
	}
	if client == nil {
		writeError(w, http.StatusConflict, "OpenBao is not enabled or not ready yet")
		return
	}
	var body instanceSecretWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	body.Path = strings.Trim(strings.TrimSpace(body.Path), "/")
	body.Key = strings.TrimSpace(body.Key)
	if !validateSecretRelPath(body.Path) {
		writeError(w, http.StatusBadRequest, "path must be one or more slash-separated segments of letters, digits, . _ ~ @ + or -")
		return
	}
	if !secretFieldKeyRegex.MatchString(body.Key) {
		writeError(w, http.StatusBadRequest, "key must be letters, digits, . _ ~ @ + or - (max 128 chars)")
		return
	}
	if body.Value == "" {
		writeError(w, http.StatusBadRequest, "value must not be empty")
		return
	}

	full := instanceSecretBasePath(inst) + body.Path
	fields := map[string]interface{}{}
	existing, err := client.ReadSecret(r.Context(), full)
	if err != nil && !errors.Is(err, openbaoprov.ErrNotFound) {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("Failed to read existing secret: %v", err))
		return
	}
	if err == nil && existing.Fields != nil {
		fields = existing.Fields
	}
	fields[body.Key] = body.Value

	if err := client.WriteSecret(r.Context(), full, fields); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("Failed to write secret: %v", err))
		return
	}
	logSecretAction(r, "wrote", inst, body.Path, body.Key)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path":   body.Path,
		"key":    body.Key,
		"masked": utils.Mask(body.Value),
	})
}

// RevealInstanceSecret handles GET /api/v1/instances/{id}/secrets/reveal
// (admin only): the plaintext of exactly one field, one request at a time.
//
// Split out from the list endpoint on purpose. The list is polled by an open
// browser tab and returns masked values only; revealing is a deliberate,
// individually logged act, so a screenshot or an idle tab never has every
// one of an agent's secrets in the clear.
func RevealInstanceSecret(w http.ResponseWriter, r *http.Request) {
	inst, client, ok := resolveInstanceSecretRequest(w, r)
	if !ok {
		return
	}
	if client == nil {
		writeError(w, http.StatusConflict, "OpenBao is not enabled or not ready yet")
		return
	}
	path := strings.Trim(strings.TrimSpace(r.URL.Query().Get("path")), "/")
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if !validateSecretRelPath(path) || !secretFieldKeyRegex.MatchString(key) {
		writeError(w, http.StatusBadRequest, "Invalid secret path or key")
		return
	}
	entry, err := client.ReadSecret(r.Context(), instanceSecretBasePath(inst)+path)
	if err != nil {
		if errors.Is(err, openbaoprov.ErrNotFound) {
			writeError(w, http.StatusNotFound, "Secret not found")
			return
		}
		writeError(w, http.StatusBadGateway, fmt.Sprintf("Failed to read secret: %v", err))
		return
	}
	value, exists := entry.Fields[key]
	if !exists {
		writeError(w, http.StatusNotFound, "Secret has no such key")
		return
	}
	logSecretAction(r, "revealed", inst, path, key)
	writeJSON(w, http.StatusOK, map[string]string{"value": secretValueToString(value)})
}

// DeleteInstanceSecret handles DELETE /api/v1/instances/{id}/secrets
// (admin only). With ?key= it removes that one field, deleting the whole
// secret if it was the last one; without, it removes the entry and every
// version of it.
func DeleteInstanceSecret(w http.ResponseWriter, r *http.Request) {
	inst, client, ok := resolveInstanceSecretRequest(w, r)
	if !ok {
		return
	}
	if client == nil {
		writeError(w, http.StatusConflict, "OpenBao is not enabled or not ready yet")
		return
	}
	path := strings.Trim(strings.TrimSpace(r.URL.Query().Get("path")), "/")
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if !validateSecretRelPath(path) {
		writeError(w, http.StatusBadRequest, "Invalid secret path")
		return
	}
	full := instanceSecretBasePath(inst) + path

	if key == "" {
		if err := client.DeleteSecret(r.Context(), full); err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("Failed to delete secret: %v", err))
			return
		}
		logSecretAction(r, "deleted", inst, path, "")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if !secretFieldKeyRegex.MatchString(key) {
		writeError(w, http.StatusBadRequest, "Invalid secret key")
		return
	}
	entry, err := client.ReadSecret(r.Context(), full)
	if err != nil {
		if errors.Is(err, openbaoprov.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent) // already gone
			return
		}
		writeError(w, http.StatusBadGateway, fmt.Sprintf("Failed to read secret: %v", err))
		return
	}
	delete(entry.Fields, key)
	if len(entry.Fields) == 0 {
		// Writing an empty field map would leave a versioned secret with no
		// data in it, which shows up in listings as an entry with nothing in
		// it. Removing the last field means removing the secret.
		if err := client.DeleteSecret(r.Context(), full); err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("Failed to delete secret: %v", err))
			return
		}
	} else if err := client.WriteSecret(r.Context(), full, entry.Fields); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("Failed to update secret: %v", err))
		return
	}
	logSecretAction(r, "deleted key from", inst, path, key)
	w.WriteHeader(http.StatusNoContent)
}

// logSecretAction records who touched which secret. Names only, never
// values -- the point is an after-the-fact trail of which admin read or
// changed what, not a copy of the secret in the log.
func logSecretAction(r *http.Request, action string, inst *database.Instance, path, key string) {
	actor := "unknown"
	if user := middleware.GetUser(r); user != nil {
		actor = user.Username
	}
	target := path
	if key != "" {
		target += "#" + key
	}
	log.Printf("openbao: admin %s %s secret %s for instance %d (%s)",
		utils.SanitizeForLog(actor), action, utils.SanitizeForLog(target), inst.ID,
		utils.SanitizeForLog(inst.Name))
}
