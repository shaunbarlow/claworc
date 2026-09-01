package handlers

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/gluk-w/claworc/control-plane/internal/database"
)

// secretSetNameRegex restricts SharedSecretSet.Name to a conservative
// charset so it maps predictably onto the secret/shared/<name>/ KV v2 path
// segment. OpenBao itself would tolerate a wider character set, but keeping
// names lowercase-kebab avoids any need to think about path escaping.
var secretSetNameRegex = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

func validateSecretSetName(name string) bool {
	return secretSetNameRegex.MatchString(name)
}

// listSharedSecretSetNames returns every configured shared secret set's
// name, sorted by creation order. Used by settingsToResponse to surface the
// available sets for the agent-form grant picker, and safe to call even
// when the openbao feature is disabled (an empty/unused list either way).
func listSharedSecretSetNames() []string {
	var sets []database.SharedSecretSet
	database.DB.Order("created_at asc").Find(&sets)
	names := make([]string, len(sets))
	for i, s := range sets {
		names[i] = s.Name
	}
	return names
}

type sharedSecretSetResponse struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

// ListSharedSecretSets returns every configured shared secret set (admin only).
func ListSharedSecretSets(w http.ResponseWriter, r *http.Request) {
	var sets []database.SharedSecretSet
	database.DB.Order("created_at asc").Find(&sets)
	resp := make([]sharedSecretSetResponse, len(sets))
	for i, s := range sets {
		resp[i] = sharedSecretSetResponse{ID: s.ID, Name: s.Name}
	}
	writeJSON(w, http.StatusOK, resp)
}

type sharedSecretSetCreateRequest struct {
	Name string `json:"name"`
}

// CreateSharedSecretSet creates a new named shared secret set (admin only).
// Creating the row is all that's needed here -- the corresponding OpenBao
// KV path (secret/shared/<name>/*) comes into existence implicitly the
// first time any secret is written under it (KV v2 has no explicit
// "create this prefix" step), and no instance is granted access to it until
// an admin edits that instance's SecretGrants separately.
func CreateSharedSecretSet(w http.ResponseWriter, r *http.Request) {
	var body sharedSecretSetCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if !validateSecretSetName(body.Name) {
		writeError(w, http.StatusBadRequest, "name must match ^[a-z][a-z0-9-]{0,62}$")
		return
	}
	var count int64
	database.DB.Model(&database.SharedSecretSet{}).Where("name = ?", body.Name).Count(&count)
	if count > 0 {
		writeError(w, http.StatusConflict, "A shared secret set with that name already exists")
		return
	}
	set := database.SharedSecretSet{Name: body.Name}
	if err := database.DB.Create(&set).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create shared secret set")
		return
	}
	writeJSON(w, http.StatusCreated, sharedSecretSetResponse{ID: set.ID, Name: set.Name})
}

// DeleteSharedSecretSet deletes a named shared secret set (admin only).
// Deliberately does not touch OpenBao itself (the underlying secret data at
// secret/shared/<name>/* is left in place -- consistent with this feature's
// "leave-be" stance on cleanup elsewhere, see the integration plan) and does
// not scrub the name out of any instance's SecretGrants; a dangling grant
// referencing a deleted set simply grants access to a path an admin no
// longer manages through this UI, which is harmless (OpenBao itself still
// enforces the policy as written) but worth an admin cleaning up by hand if
// they care.
func DeleteSharedSecretSet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid id")
		return
	}
	if err := database.DB.Delete(&database.SharedSecretSet{}, id).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete shared secret set")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}
