package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gluk-w/claworc/control-plane/internal/database"
)

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

func TestParseMemoryQmdSettings(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"empty", ``, false},
		{"valid", `{"search_mode":"query","update_interval":"10m","max_results":8,"sessions_enabled":true,"include_default_memory":false}`, false},
		{"advanced object", `{"advanced":{"limits":{"timeoutMs":8000}}}`, false},
		{"bad search mode", `{"search_mode":"fuzzy"}`, true},
		{"bad interval", `{"update_interval":"five minutes"}`, true},
		{"max results too high", `{"max_results":500}`, true},
		{"advanced not object", `{"advanced":[1,2]}`, true},
		{"unknown key", `{"serach_mode":"query"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseMemoryQmdSettings([]byte(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Errorf("parseMemoryQmdSettings(%q) err=%v, wantErr=%v", tc.raw, err, tc.wantErr)
			}
		})
	}
}

func TestParseMemoryQmdSettings_Scope(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"valid direct+channel", `{"scope":{"default":"deny","rules":[{"action":"allow","chat_type":"direct"},{"action":"allow","chat_type":"channel"}]}}`, false},
		{"valid default only", `{"scope":{"default":"allow"}}`, false},
		{"bad default", `{"scope":{"default":"maybe"}}`, true},
		{"bad rule action", `{"scope":{"rules":[{"action":"sometimes","chat_type":"direct"}]}}`, true},
		{"bad chat type", `{"scope":{"rules":[{"action":"allow","chat_type":"everywhere"}]}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseMemoryQmdSettings([]byte(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Errorf("parseMemoryQmdSettings(%q) err=%v, wantErr=%v", tc.raw, err, tc.wantErr)
			}
		})
	}
}

func TestBuildMemoryConfig_Scope(t *testing.T) {
	setupTestDB(t)
	database.SetSetting("default_memory_backend", "qmd")

	inst := nonLegacyInstance(t, "bot-scope1", "Scope1")
	scopeJSON := `{"scope":{"default":"deny","rules":[{"action":"allow","chat_type":"direct"},{"action":"allow","chat_type":"channel"}]}}`
	if err := database.DB.Model(&inst).Update("memory_qmd", scopeJSON).Error; err != nil {
		t.Fatalf("seed override: %v", err)
	}
	database.DB.First(&inst, inst.ID)

	cfg := buildMemoryConfig(&inst)
	qmd, ok := cfg["qmd"].(map[string]interface{})
	if !ok {
		t.Fatalf("qmd subtree missing: %#v", cfg)
	}
	scope, ok := qmd["scope"].(map[string]interface{})
	if !ok {
		t.Fatalf("scope subtree missing: %#v", qmd)
	}
	if scope["default"] != "deny" {
		t.Errorf("scope.default = %v, want deny", scope["default"])
	}
	rules, ok := scope["rules"].([]map[string]interface{})
	if !ok || len(rules) != 2 {
		t.Fatalf("scope.rules = %#v, want 2 entries", scope["rules"])
	}
	if rules[0]["action"] != "allow" {
		t.Errorf("rules[0].action = %v", rules[0]["action"])
	}
	match0, ok := rules[0]["match"].(map[string]interface{})
	if !ok || match0["chatType"] != "direct" {
		t.Errorf("rules[0].match = %#v, want chatType direct", rules[0]["match"])
	}
	match1, ok := rules[1]["match"].(map[string]interface{})
	if !ok || match1["chatType"] != "channel" {
		t.Errorf("rules[1].match = %#v, want chatType channel", rules[1]["match"])
	}
}

func TestMergeMemoryQmdSettings(t *testing.T) {
	global := MemoryQmdSettings{
		SearchMode:     "search",
		UpdateInterval: "5m",
		MaxResults:     intPtr(6),
	}
	override := MemoryQmdSettings{
		SearchMode:      "query",
		SessionsEnabled: boolPtr(true),
	}
	got := mergeMemoryQmdSettings(global, override)
	if got.SearchMode != "query" {
		t.Errorf("SearchMode = %q, want override to win", got.SearchMode)
	}
	if got.UpdateInterval != "5m" {
		t.Errorf("UpdateInterval = %q, want inherited 5m", got.UpdateInterval)
	}
	if got.MaxResults == nil || *got.MaxResults != 6 {
		t.Errorf("MaxResults = %v, want inherited 6", got.MaxResults)
	}
	if got.SessionsEnabled == nil || !*got.SessionsEnabled {
		t.Errorf("SessionsEnabled = %v, want override true", got.SessionsEnabled)
	}
}

func TestBuildMemoryConfig_BuiltinDefault(t *testing.T) {
	setupTestDB(t)
	inst := nonLegacyInstance(t, "bot-mem1", "Mem1")

	cfg := buildMemoryConfig(&inst)
	want := map[string]interface{}{"backend": "builtin"}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("cfg = %#v, want %#v", cfg, want)
	}
}

func TestBuildMemoryConfig_QmdWithFoldersAndOverrides(t *testing.T) {
	setupTestDB(t)
	if err := database.DB.AutoMigrate(&database.SharedFolder{}); err != nil {
		t.Fatalf("migrate shared_folders: %v", err)
	}
	database.SetSetting("default_memory_backend", "qmd")
	database.SetSetting("default_memory_qmd", `{"search_mode":"search","max_results":6}`)

	inst := nonLegacyInstance(t, "bot-mem2", "Mem2")
	if err := database.DB.Model(&inst).Update("memory_qmd", `{"search_mode":"vsearch","advanced":{"limits":{"timeoutMs":9000}}}`).Error; err != nil {
		t.Fatalf("seed override: %v", err)
	}
	database.DB.First(&inst, inst.ID)

	// One indexed folder attached directly, one attached but not indexed,
	// one indexed but not attached.
	mk := func(name, mount string, indexed bool, instanceIDs []uint) {
		t.Helper()
		sf := database.SharedFolder{
			Name:        name,
			MountPath:   mount,
			OwnerID:     1,
			InstanceIDs: database.EncodeSharedFolderInstanceIDs(instanceIDs),
			QmdIndex:    indexed,
			QmdPattern:  "",
		}
		if name == "Team Docs" {
			sf.QmdPattern = "**/*.{md,txt}"
		}
		if err := database.DB.Create(&sf).Error; err != nil {
			t.Fatalf("create folder %s: %v", name, err)
		}
	}
	mk("Team Docs", "/mnt/docs", true, []uint{inst.ID})
	mk("Scratch", "/mnt/scratch", false, []uint{inst.ID})
	mk("Elsewhere", "/mnt/other", true, []uint{inst.ID + 999})

	cfg := buildMemoryConfig(&inst)
	if cfg["backend"] != "qmd" {
		t.Fatalf("backend = %v, want qmd", cfg["backend"])
	}
	qmd, ok := cfg["qmd"].(map[string]interface{})
	if !ok {
		t.Fatalf("qmd subtree missing: %#v", cfg)
	}
	if qmd["searchMode"] != "vsearch" {
		t.Errorf("searchMode = %v, want instance override vsearch", qmd["searchMode"])
	}
	limits, _ := qmd["limits"].(map[string]interface{})
	if limits == nil {
		t.Fatalf("limits missing: %#v", qmd)
	}
	// maxResults comes from the global default; timeoutMs from the Advanced
	// overlay, deep-merged into the same limits object.
	if got := limits["maxResults"]; got != 6 && got != float64(6) {
		t.Errorf("limits.maxResults = %v (%T), want 6", got, got)
	}
	if got := limits["timeoutMs"]; got != float64(9000) {
		t.Errorf("limits.timeoutMs = %v, want 9000 from advanced overlay", got)
	}
	paths, _ := qmd["paths"].([]map[string]interface{})
	if len(paths) != 1 {
		t.Fatalf("paths = %#v, want exactly the one attached+indexed folder", qmd["paths"])
	}
	if paths[0]["path"] != "/mnt/docs" || paths[0]["pattern"] != "**/*.{md,txt}" {
		t.Errorf("paths[0] = %#v", paths[0])
	}
	if name, _ := paths[0]["name"].(string); name == "" {
		t.Errorf("paths[0].name empty, want a collection slug")
	}
}

func TestApplyMemoryConfig_ArgvAndRestart(t *testing.T) {
	mock := &mockInstance{}
	cfg := map[string]interface{}{"backend": "qmd", "qmd": map[string]interface{}{"searchMode": "search"}}
	applyMemoryConfig(context.Background(), mock, "bot-x", cfg)

	// One atomic set, no `config unset` — see applyMemoryConfig: unsetting is a
	// separate write OpenClaw's size-drop guard rejects, and when it does land
	// ahead of a failing set the agent loses its memory config entirely.
	if len(mock.calls) != 2 {
		t.Fatalf("calls = %d (%v), want set + gateway stop", len(mock.calls), mock.calls)
	}
	for _, c := range mock.calls {
		if len(c) > 1 && c[1] == "unset" {
			t.Errorf("config unset must not be used; got %v", c)
		}
	}
	if len(mock.calls[0]) != 6 || mock.calls[0][0] != "config" || mock.calls[0][1] != "set" ||
		mock.calls[0][2] != "memory" || mock.calls[0][4] != "--replace" || mock.calls[0][5] != "--json" {
		t.Fatalf("call[0] = %v", mock.calls[0])
	}
	var pushed map[string]interface{}
	if err := json.Unmarshal([]byte(mock.calls[0][3]), &pushed); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if pushed["backend"] != "qmd" {
		t.Errorf("payload = %v", pushed)
	}
	if !reflect.DeepEqual(mock.calls[1], []string{"gateway", "stop"}) {
		t.Errorf("call[1] = %v, want gateway restart", mock.calls[1])
	}
}

func TestApplyMemoryConfig_SetFailureSkipsRestart(t *testing.T) {
	mock := &mockInstance{results: []callResult{
		{code: 1, stderr: "invalid value"}, // the set fails
	}}
	applyMemoryConfig(context.Background(), mock, "bot-x", map[string]interface{}{"backend": "builtin"})
	if len(mock.calls) != 1 {
		t.Fatalf("calls = %v, want only the failed set and no gateway restart", mock.calls)
	}
	if mock.calls[0][1] != "set" {
		t.Errorf("call[0] = %v, want the config set", mock.calls[0])
	}
}

func TestSetInstanceMemory_UpdateAndClear(t *testing.T) {
	setupTestDB(t)
	inst := nonLegacyInstance(t, "bot-mem3", "Mem3")
	user := createTestUser(t, "admin")

	w := httptest.NewRecorder()
	SetInstanceMemory(w, buildJSONRequest(t, "PATCH", "/api/v1/instances/{id}/memory", user,
		map[string]string{"id": fmt.Sprintf("%d", inst.ID)},
		`{"memory_backend":"qmd","qmd":{"search_mode":"query","max_results":10}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	resp := parseResponse(t, w)
	if resp["effective_backend"] != "qmd" || resp["memory_backend"] != "qmd" {
		t.Errorf("resp = %v", resp)
	}

	var row database.Instance
	database.DB.First(&row, inst.ID)
	if row.MemoryBackend != "qmd" {
		t.Errorf("MemoryBackend = %q", row.MemoryBackend)
	}
	saved := loadMemoryQmdSettings(row.MemoryQmd)
	if saved.SearchMode != "query" || saved.MaxResults == nil || *saved.MaxResults != 10 {
		t.Errorf("MemoryQmd = %q", row.MemoryQmd)
	}

	// Clearing the override returns the instance to the global default.
	w = httptest.NewRecorder()
	SetInstanceMemory(w, buildJSONRequest(t, "PATCH", "/api/v1/instances/{id}/memory", user,
		map[string]string{"id": fmt.Sprintf("%d", inst.ID)}, `{"memory_backend":"","qmd":{}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("clear: status %d body=%s", w.Code, w.Body.String())
	}
	database.DB.First(&row, inst.ID)
	if row.MemoryBackend != "" {
		t.Errorf("MemoryBackend = %q after clear, want \"\"", row.MemoryBackend)
	}
	if got := parseResponse(t, w)["effective_backend"]; got != "builtin" {
		t.Errorf("effective_backend = %v after clear, want builtin", got)
	}
}

func TestSetInstanceMemory_Rejections(t *testing.T) {
	setupTestDB(t)
	user := createTestUser(t, "admin")

	// Invalid backend value.
	inst := nonLegacyInstance(t, "bot-mem4", "Mem4")
	w := httptest.NewRecorder()
	SetInstanceMemory(w, buildJSONRequest(t, "PATCH", "/api/v1/instances/{id}/memory", user,
		map[string]string{"id": fmt.Sprintf("%d", inst.ID)}, `{"memory_backend":"redis"}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid backend: status %d, want 400", w.Code)
	}

	// Invalid qmd settings.
	w = httptest.NewRecorder()
	SetInstanceMemory(w, buildJSONRequest(t, "PATCH", "/api/v1/instances/{id}/memory", user,
		map[string]string{"id": fmt.Sprintf("%d", inst.ID)}, `{"qmd":{"search_mode":"nope"}}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid qmd: status %d, want 400", w.Code)
	}

	// Empty body.
	w = httptest.NewRecorder()
	SetInstanceMemory(w, buildJSONRequest(t, "PATCH", "/api/v1/instances/{id}/memory", user,
		map[string]string{"id": fmt.Sprintf("%d", inst.ID)}, `{}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty body: status %d, want 400", w.Code)
	}

	// Legacy embedded instances are rejected.
	legacy := createTestInstance(t, "bot-mem5", "Mem5")
	w = httptest.NewRecorder()
	SetInstanceMemory(w, buildJSONRequest(t, "PATCH", "/api/v1/instances/{id}/memory", user,
		map[string]string{"id": fmt.Sprintf("%d", legacy.ID)}, `{"memory_backend":"qmd"}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("legacy: status %d, want 400", w.Code)
	}
}

func TestGetInstanceMemory_IndexedFolders(t *testing.T) {
	setupTestDB(t)
	if err := database.DB.AutoMigrate(&database.SharedFolder{}); err != nil {
		t.Fatalf("migrate shared_folders: %v", err)
	}
	inst := nonLegacyInstance(t, "bot-mem6", "Mem6")
	user := createTestUser(t, "admin")
	sf := database.SharedFolder{
		Name:        "Docs",
		MountPath:   "/mnt/docs",
		OwnerID:     user.ID,
		InstanceIDs: database.EncodeSharedFolderInstanceIDs([]uint{inst.ID}),
		QmdIndex:    true,
	}
	if err := database.DB.Create(&sf).Error; err != nil {
		t.Fatalf("create folder: %v", err)
	}

	w := httptest.NewRecorder()
	GetInstanceMemory(w, buildRequest(t, "GET", "/api/v1/instances/{id}/memory", user,
		map[string]string{"id": fmt.Sprintf("%d", inst.ID)}))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	resp := parseResponse(t, w)
	folders, _ := resp["indexed_folders"].([]interface{})
	if len(folders) != 1 {
		t.Fatalf("indexed_folders = %v, want 1", resp["indexed_folders"])
	}
	f := folders[0].(map[string]interface{})
	if f["mount_path"] != "/mnt/docs" || f["pattern"] != "**/*.md" {
		t.Errorf("folder = %v", f)
	}
}

// TestCloneInstance_CopiesMemoryOverride mirrors the browser_enabled clone
// test: the memory backend override must carry over.
func TestCloneInstance_CopiesMemoryOverride(t *testing.T) {
	cloneSetup(t)

	src := nonLegacyInstance(t, "bot-mem7", "Mem7")
	if err := database.DB.Model(&src).Updates(map[string]interface{}{
		"memory_backend": "qmd",
		"memory_qmd":     `{"search_mode":"query"}`,
	}).Error; err != nil {
		t.Fatalf("seed src: %v", err)
	}
	user := createTestUser(t, "admin")

	w := httptest.NewRecorder()
	CloneInstance(w, reqClone(t, src.ID, user))
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	dstID := uint(parseResponse(t, w)["id"].(float64))

	var dst database.Instance
	if err := database.DB.First(&dst, dstID).Error; err != nil {
		t.Fatalf("load dst: %v", err)
	}
	if dst.MemoryBackend != "qmd" {
		t.Errorf("MemoryBackend = %q on clone, want qmd", dst.MemoryBackend)
	}
	if loadMemoryQmdSettings(dst.MemoryQmd).SearchMode != "query" {
		t.Errorf("MemoryQmd = %q on clone", dst.MemoryQmd)
	}
}
