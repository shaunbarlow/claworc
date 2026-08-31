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

func TestParseMemorySettings(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"empty", ``, false},
		{"valid", `{"provider":"openai","model":"text-embedding-3-small","fallback":"none","max_results":8,"min_score":0.35,"citations":"on","remember_across_conversations":true,"sessions_enabled":false}`, false},
		{"custom provider id", `{"provider":"ollama-5080"}`, false},
		{"advanced object", `{"advanced":{"remote":{"baseUrl":"https://example.com"}}}`, false},
		{"bad citations", `{"citations":"maybe"}`, true},
		{"max results too high", `{"max_results":500}`, true},
		{"max results too low", `{"max_results":0}`, true},
		{"min score out of range", `{"min_score":1.5}`, true},
		{"advanced not object", `{"advanced":[1,2]}`, true},
		{"unknown key", `{"providr":"openai"}`, true},
		{"blank provider", `{"provider":"   "}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseMemorySettings([]byte(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Errorf("parseMemorySettings(%q) err=%v, wantErr=%v", tc.raw, err, tc.wantErr)
			}
		})
	}
}

func TestMergeMemorySettings(t *testing.T) {
	global := MemorySettings{
		Provider:   "openai",
		MaxResults: intPtr(6),
		Citations:  "auto",
	}
	override := MemorySettings{
		Provider:        "gemini",
		SessionsEnabled: boolPtr(true),
	}
	got := mergeMemorySettings(global, override)
	if got.Provider != "gemini" {
		t.Errorf("Provider = %q, want override to win", got.Provider)
	}
	if got.MaxResults == nil || *got.MaxResults != 6 {
		t.Errorf("MaxResults = %v, want inherited 6", got.MaxResults)
	}
	if got.Citations != "auto" {
		t.Errorf("Citations = %q, want inherited auto", got.Citations)
	}
	if got.SessionsEnabled == nil || !*got.SessionsEnabled {
		t.Errorf("SessionsEnabled = %v, want override true", got.SessionsEnabled)
	}
}

func TestBuildMemoryConfig_Empty(t *testing.T) {
	setupTestDB(t)
	inst := nonLegacyInstance(t, "bot-mem1", "Mem1")

	cfg := buildMemoryConfig(&inst)
	if len(cfg) != 0 {
		t.Errorf("cfg = %#v, want empty (leave OpenClaw's own memory defaults alone)", cfg)
	}
}

func TestBuildMemoryConfig_GlobalAndOverride(t *testing.T) {
	setupTestDB(t)
	database.SetSetting("default_memory_settings", `{"provider":"openai","max_results":6,"citations":"auto"}`)

	inst := nonLegacyInstance(t, "bot-mem2", "Mem2")
	if err := database.DB.Model(&inst).Update("memory_settings",
		`{"provider":"gemini","min_score":0.4,"advanced":{"remote":{"baseUrl":"https://example.com"}}}`).Error; err != nil {
		t.Fatalf("seed override: %v", err)
	}
	database.DB.First(&inst, inst.ID)

	cfg := buildMemoryConfig(&inst)
	if cfg["citations"] != "auto" {
		t.Errorf("citations = %v, want inherited auto", cfg["citations"])
	}
	search, ok := cfg["search"].(map[string]interface{})
	if !ok {
		t.Fatalf("search subtree missing: %#v", cfg)
	}
	if search["provider"] != "gemini" {
		t.Errorf("provider = %v, want instance override gemini", search["provider"])
	}
	query, ok := search["query"].(map[string]interface{})
	if !ok {
		t.Fatalf("query subtree missing: %#v", search)
	}
	if got := query["maxResults"]; got != 6 && got != float64(6) {
		t.Errorf("query.maxResults = %v, want inherited 6", got)
	}
	if got := query["minScore"]; got != 0.4 {
		t.Errorf("query.minScore = %v, want override 0.4", got)
	}
	remote, ok := search["remote"].(map[string]interface{})
	if !ok || remote["baseUrl"] != "https://example.com" {
		t.Errorf("remote = %#v, want advanced overlay baseUrl", search["remote"])
	}
}

func TestBuildMemoryConfig_SessionsEnabledSources(t *testing.T) {
	setupTestDB(t)
	inst := nonLegacyInstance(t, "bot-mem3", "Mem3")
	if err := database.DB.Model(&inst).Update("memory_settings", `{"sessions_enabled":true}`).Error; err != nil {
		t.Fatalf("seed override: %v", err)
	}
	database.DB.First(&inst, inst.ID)

	cfg := buildMemoryConfig(&inst)
	search := cfg["search"].(map[string]interface{})
	sources, ok := search["sources"].([]string)
	if !ok || len(sources) != 2 || sources[0] != "memory" || sources[1] != "sessions" {
		t.Errorf("sources = %#v, want [memory sessions]", search["sources"])
	}
}

func TestBuildMemoryConfig_ExtraPathsFromIndexedFolders(t *testing.T) {
	setupTestDB(t)
	if err := database.DB.AutoMigrate(&database.SharedFolder{}); err != nil {
		t.Fatalf("migrate shared_folders: %v", err)
	}
	inst := nonLegacyInstance(t, "bot-mem4", "Mem4")

	mk := func(name, mount string, indexed bool, pattern string, instanceIDs []uint) {
		t.Helper()
		sf := database.SharedFolder{
			Name:               name,
			MountPath:          mount,
			OwnerID:            1,
			InstanceIDs:        database.EncodeSharedFolderInstanceIDs(instanceIDs),
			MemoryIndex:        indexed,
			MemoryIndexPattern: pattern,
		}
		if err := database.DB.Create(&sf).Error; err != nil {
			t.Fatalf("create folder %s: %v", name, err)
		}
	}
	mk("Team Docs", "/mnt/docs", true, "**/*.{md,txt}", []uint{inst.ID})
	mk("Scratch", "/mnt/scratch", false, "", []uint{inst.ID})
	mk("Elsewhere", "/mnt/other", true, "", []uint{inst.ID + 999})
	mk("Whole Folder", "/mnt/whole", true, "", []uint{inst.ID})

	cfg := buildMemoryConfig(&inst)
	search, ok := cfg["search"].(map[string]interface{})
	if !ok {
		t.Fatalf("search subtree missing: %#v", cfg)
	}
	extraPaths, ok := search["extraPaths"].([]interface{})
	if !ok || len(extraPaths) != 2 {
		t.Fatalf("extraPaths = %#v, want the two attached+indexed folders", search["extraPaths"])
	}
	// The patterned folder renders as a {path, pattern} object.
	obj, ok := extraPaths[0].(map[string]interface{})
	if !ok || obj["path"] != "/mnt/docs" || obj["pattern"] != "**/*.{md,txt}" {
		t.Errorf("extraPaths[0] = %#v", extraPaths[0])
	}
	// The unpatterned folder renders as a bare path string.
	if extraPaths[1] != "/mnt/whole" {
		t.Errorf("extraPaths[1] = %#v, want bare path string", extraPaths[1])
	}
}

func TestApplyMemoryConfig_ArgvAndRestart(t *testing.T) {
	mock := &mockInstance{}
	cfg := map[string]interface{}{"search": map[string]interface{}{"provider": "openai"}}
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
	search, ok := pushed["search"].(map[string]interface{})
	if !ok || search["provider"] != "openai" {
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
	applyMemoryConfig(context.Background(), mock, "bot-x", map[string]interface{}{"citations": "on"})
	if len(mock.calls) != 1 {
		t.Fatalf("calls = %v, want only the failed set and no gateway restart", mock.calls)
	}
	if mock.calls[0][1] != "set" {
		t.Errorf("call[0] = %v, want the config set", mock.calls[0])
	}
}

func TestSetInstanceMemory_UpdateAndClear(t *testing.T) {
	setupTestDB(t)
	inst := nonLegacyInstance(t, "bot-mem5", "Mem5")
	user := createTestUser(t, "admin")

	w := httptest.NewRecorder()
	SetInstanceMemory(w, buildJSONRequest(t, "PATCH", "/api/v1/instances/{id}/memory", user,
		map[string]string{"id": fmt.Sprintf("%d", inst.ID)},
		`{"settings":{"provider":"gemini","max_results":10}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	resp := parseResponse(t, w)
	settings, ok := resp["settings"].(map[string]interface{})
	if !ok || settings["provider"] != "gemini" {
		t.Errorf("resp.settings = %v", resp["settings"])
	}

	var row database.Instance
	database.DB.First(&row, inst.ID)
	saved := loadMemorySettings(row.MemorySettings)
	if saved.Provider != "gemini" || saved.MaxResults == nil || *saved.MaxResults != 10 {
		t.Errorf("MemorySettings = %q", row.MemorySettings)
	}

	// Clearing the override returns the instance to the global default (empty object).
	w = httptest.NewRecorder()
	SetInstanceMemory(w, buildJSONRequest(t, "PATCH", "/api/v1/instances/{id}/memory", user,
		map[string]string{"id": fmt.Sprintf("%d", inst.ID)}, `{"settings":{}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("clear: status %d body=%s", w.Code, w.Body.String())
	}
	database.DB.First(&row, inst.ID)
	if loadMemorySettings(row.MemorySettings).Provider != "" {
		t.Errorf("Provider = %q after clear, want \"\"", loadMemorySettings(row.MemorySettings).Provider)
	}
}

func TestSetInstanceMemory_Rejections(t *testing.T) {
	setupTestDB(t)
	user := createTestUser(t, "admin")

	// Invalid settings value.
	inst := nonLegacyInstance(t, "bot-mem6", "Mem6")
	w := httptest.NewRecorder()
	SetInstanceMemory(w, buildJSONRequest(t, "PATCH", "/api/v1/instances/{id}/memory", user,
		map[string]string{"id": fmt.Sprintf("%d", inst.ID)}, `{"settings":{"citations":"nope"}}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid citations: status %d, want 400", w.Code)
	}

	// Empty body.
	w = httptest.NewRecorder()
	SetInstanceMemory(w, buildJSONRequest(t, "PATCH", "/api/v1/instances/{id}/memory", user,
		map[string]string{"id": fmt.Sprintf("%d", inst.ID)}, `{}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty body: status %d, want 400", w.Code)
	}

	// Legacy embedded instances are rejected.
	legacy := createTestInstance(t, "bot-mem7", "Mem7")
	w = httptest.NewRecorder()
	SetInstanceMemory(w, buildJSONRequest(t, "PATCH", "/api/v1/instances/{id}/memory", user,
		map[string]string{"id": fmt.Sprintf("%d", legacy.ID)}, `{"settings":{"provider":"openai"}}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("legacy: status %d, want 400", w.Code)
	}
}

func TestGetInstanceMemory_IndexedFolders(t *testing.T) {
	setupTestDB(t)
	if err := database.DB.AutoMigrate(&database.SharedFolder{}); err != nil {
		t.Fatalf("migrate shared_folders: %v", err)
	}
	inst := nonLegacyInstance(t, "bot-mem8", "Mem8")
	user := createTestUser(t, "admin")
	sf := database.SharedFolder{
		Name:        "Docs",
		MountPath:   "/mnt/docs",
		OwnerID:     user.ID,
		InstanceIDs: database.EncodeSharedFolderInstanceIDs([]uint{inst.ID}),
		MemoryIndex: true,
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
	if f["mount_path"] != "/mnt/docs" {
		t.Errorf("folder = %v", f)
	}
}

// TestCloneInstance_CopiesMemoryOverride mirrors the browser_enabled clone
// test: the memory settings override must carry over.
func TestCloneInstance_CopiesMemoryOverride(t *testing.T) {
	cloneSetup(t)

	src := nonLegacyInstance(t, "bot-mem9", "Mem9")
	if err := database.DB.Model(&src).Updates(map[string]interface{}{
		"memory_settings": `{"provider":"gemini"}`,
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
	if loadMemorySettings(dst.MemorySettings).Provider != "gemini" {
		t.Errorf("MemorySettings = %q on clone, want provider gemini", dst.MemorySettings)
	}
}
