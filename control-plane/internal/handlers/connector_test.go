package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gluk-w/claworc/control-plane/internal/config"
	"github.com/gluk-w/claworc/control-plane/internal/database"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
)

func TestResolveConnectorOrigin_ExplicitWins(t *testing.T) {
	prev := config.Cfg.RPOrigins
	config.Cfg.RPOrigins = []string{"https://claworc.example.com"}
	t.Cleanup(func() { config.Cfg.RPOrigins = prev })

	origin, source := resolveConnectorOrigin("https://manual-override.example.com/")
	if origin != "https://manual-override.example.com" {
		t.Errorf("origin = %q, want trailing slash trimmed explicit value", origin)
	}
	if source != "explicit" {
		t.Errorf("source = %q, want \"explicit\"", source)
	}
}

func TestResolveConnectorOrigin_AutoDerivesFromRPOrigins(t *testing.T) {
	prev := config.Cfg.RPOrigins
	config.Cfg.RPOrigins = []string{"https://claworc.example.com/"}
	t.Cleanup(func() { config.Cfg.RPOrigins = prev })

	origin, source := resolveConnectorOrigin("")
	want := "https://claworc.example.com/connector"
	if origin != want {
		t.Errorf("origin = %q, want %q", origin, want)
	}
	if source != "auto" {
		t.Errorf("source = %q, want \"auto\"", source)
	}
}

func TestResolveConnectorOrigin_EmptyWhenNothingConfigured(t *testing.T) {
	prev := config.Cfg.RPOrigins
	config.Cfg.RPOrigins = nil
	t.Cleanup(func() { config.Cfg.RPOrigins = prev })

	origin, source := resolveConnectorOrigin("")
	if origin != "" {
		t.Errorf("origin = %q, want empty when no explicit setting and no RPOrigins", origin)
	}
	if source != "auto" {
		t.Errorf("source = %q, want \"auto\"", source)
	}
}

func TestUpdateSettings_ConnectorEnabled_GeneratesSecretsOnce(t *testing.T) {
	setupSettingsTest(t)

	w := httptest.NewRecorder()
	UpdateSettings(w, postJSON("/api/v1/settings", map[string]interface{}{
		"connector_enabled": "true",
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}

	enabled, _ := database.GetSetting("connector_enabled")
	if enabled != "true" {
		t.Fatalf("connector_enabled = %q, want true", enabled)
	}
	encKeyRaw, _ := database.GetSetting("connector_encryption_key")
	adminTokenRaw, _ := database.GetSetting("connector_admin_token")
	if encKeyRaw == "" || adminTokenRaw == "" {
		t.Fatalf("expected connector secrets to be generated, got encKey=%q adminToken=%q", encKeyRaw, adminTokenRaw)
	}

	// Secrets must be stored encrypted, not plaintext.
	plainKey, err := utils.Decrypt(encKeyRaw)
	if err != nil || plainKey == "" {
		t.Fatalf("connector_encryption_key did not decrypt: %v", err)
	}

	// A second enable call must not rotate the already-generated secrets.
	w2 := httptest.NewRecorder()
	UpdateSettings(w2, postJSON("/api/v1/settings", map[string]interface{}{
		"connector_enabled": "true",
	}))
	if w2.Code != http.StatusOK {
		t.Fatalf("second enable status = %d, want 200", w2.Code)
	}
	encKeyRaw2, _ := database.GetSetting("connector_encryption_key")
	if encKeyRaw2 != encKeyRaw {
		t.Errorf("re-enabling regenerated the encryption key; secrets must be stable once minted")
	}
}

func TestUpdateSettings_ConnectorEnabled_RejectsNonBool(t *testing.T) {
	setupSettingsTest(t)

	w := httptest.NewRecorder()
	UpdateSettings(w, postJSON("/api/v1/settings", map[string]interface{}{
		"connector_enabled": "maybe",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if v, _ := database.GetSetting("connector_enabled"); v == "maybe" {
		t.Errorf("invalid value was persisted: %q", v)
	}
}

func TestUpdateSettings_ConnectorEnabled_DisableDoesNotWipeSecrets(t *testing.T) {
	setupSettingsTest(t)

	UpdateSettings(httptest.NewRecorder(), postJSON("/api/v1/settings", map[string]interface{}{
		"connector_enabled": "true",
	}))
	encKeyRaw, _ := database.GetSetting("connector_encryption_key")
	if encKeyRaw == "" {
		t.Fatalf("expected secrets after enabling")
	}

	w := httptest.NewRecorder()
	UpdateSettings(w, postJSON("/api/v1/settings", map[string]interface{}{
		"connector_enabled": "false",
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if v, _ := database.GetSetting("connector_enabled"); v != "false" {
		t.Errorf("connector_enabled = %q, want false", v)
	}
	// Disabling is a lifecycle stop, not a secret wipe -- re-enabling later
	// should not need to re-provision every instance's stored token.
	encKeyRaw2, _ := database.GetSetting("connector_encryption_key")
	if encKeyRaw2 != encKeyRaw {
		t.Errorf("disabling the connector should not touch its secrets")
	}
}

func TestResolvedConnectorConfig_NotOkWithoutSecrets(t *testing.T) {
	setupSettingsTest(t)

	if _, ok := resolvedConnectorConfig(); ok {
		t.Errorf("expected resolvedConnectorConfig to report not-ok before secrets are generated")
	}
}

func TestResolvedConnectorConfig_DefaultsImageAndStorage(t *testing.T) {
	setupSettingsTest(t)
	if _, err := ensureConnectorSecrets(); err != nil {
		t.Fatalf("ensureConnectorSecrets: %v", err)
	}

	cfg, ok := resolvedConnectorConfig()
	if !ok {
		t.Fatalf("expected resolvedConnectorConfig to report ok once secrets exist")
	}
	if cfg.Image != defaultConnectorImage {
		t.Errorf("Image = %q, want default %q", cfg.Image, defaultConnectorImage)
	}
	if cfg.StorageSize != defaultConnectorStorage {
		t.Errorf("StorageSize = %q, want default %q", cfg.StorageSize, defaultConnectorStorage)
	}
}

func TestBuildConnectorEnvVars_EmptyWhenDisabledOrNoToken(t *testing.T) {
	setupSettingsTest(t)
	database.SetSetting("connector_enabled", "false")

	inst := &database.Instance{ID: 1, ConnectorRuntimeToken: "irrelevant"}
	if env := buildConnectorEnvVars(inst); len(env) != 0 {
		t.Errorf("expected no env vars while connector_enabled=false, got %+v", env)
	}

	database.SetSetting("connector_enabled", "true")
	inst2 := &database.Instance{ID: 2}
	if env := buildConnectorEnvVars(inst2); len(env) != 0 {
		t.Errorf("expected no env vars for an instance with no minted token, got %+v", env)
	}
}

func TestBuildConnectorEnvVars_RendersTokenAndBaseURL(t *testing.T) {
	setupSettingsTest(t)
	database.SetSetting("connector_enabled", "true")

	enc, err := utils.Encrypt("plaintext-runtime-token")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	inst := &database.Instance{ID: 3, ConnectorRuntimeToken: enc}

	env := buildConnectorEnvVars(inst)
	if env["OOMOL_CONNECT_RUNTIME_TOKEN"] != "plaintext-runtime-token" {
		t.Errorf("OOMOL_CONNECT_RUNTIME_TOKEN = %q, want plaintext-runtime-token", env["OOMOL_CONNECT_RUNTIME_TOKEN"])
	}
	if env["OPEN_CONNECTOR_BASE_URL"] == "" {
		t.Errorf("expected OPEN_CONNECTOR_BASE_URL to be set")
	}
}

func TestGetConnectorStatus_DisabledReportsWithoutOrchestrator(t *testing.T) {
	setupSettingsTest(t)
	database.SetSetting("connector_enabled", "false")

	w := httptest.NewRecorder()
	GetConnectorStatus(w, httptest.NewRequest("GET", "/api/v1/connector/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["enabled"] != false {
		t.Errorf("enabled = %v, want false", body["enabled"])
	}
	if body["status"] != "disabled" {
		t.Errorf("status = %v, want disabled", body["status"])
	}
}

func TestConnectorTokenName_IncludesDisplayNameNameAndID(t *testing.T) {
	inst := &database.Instance{ID: 42, Name: "bot-research-bot", DisplayName: "Research Bot"}
	got := connectorTokenName(inst)
	for _, want := range []string{"Research Bot", "bot-research-bot", "42"} {
		if !strings.Contains(got, want) {
			t.Errorf("connectorTokenName(%+v) = %q, want it to contain %q", inst, got, want)
		}
	}
}

func TestDefaultConnectorTokenPolicy_RestrictiveByDefault(t *testing.T) {
	allowedActions, allowedProxies, allowedConnections := defaultConnectorTokenPolicy()

	if len(allowedActions) == 0 {
		t.Fatal("expected a non-empty allowedActions grant for the curated no_auth test services")
	}
	for _, rule := range allowedActions {
		if !strings.HasSuffix(rule, ".*") {
			t.Errorf("allowedActions rule %q does not match open-connector's required service.* syntax", rule)
		}
	}
	// No provider proxy access and no stored-credential connection grant by
	// default -- proxies are deny-by-default on an empty grant per
	// docs/runtime-api.md, and no_auth virtual connections never need a grant
	// (so an empty AllowedConnections does not itself widen access).
	if allowedProxies == nil || len(allowedProxies) != 0 {
		t.Errorf("allowedProxies = %v, want an explicit empty (deny-by-default) grant", allowedProxies)
	}
	if allowedConnections == nil || len(allowedConnections) != 0 {
		t.Errorf("allowedConnections = %v, want an explicit empty grant", allowedConnections)
	}
}

func TestDefaultConnectorTokenPolicy_MatchesCuratedServiceList(t *testing.T) {
	allowedActions, _, _ := defaultConnectorTokenPolicy()
	if len(allowedActions) != len(connectorTestOnlyServices) {
		t.Fatalf("allowedActions has %d rules, want one per connectorTestOnlyServices entry (%d)",
			len(allowedActions), len(connectorTestOnlyServices))
	}
	for i, svc := range connectorTestOnlyServices {
		if want := svc + ".*"; allowedActions[i] != want {
			t.Errorf("allowedActions[%d] = %q, want %q", i, allowedActions[i], want)
		}
	}
}
