package handlers

import (
	"context"
	"fmt"
	"testing"

	"github.com/gluk-w/claworc/control-plane/internal/database"
)

// envFor builds an instance whose encrypted EnvVars column holds plain, plus
// the container env a fully up-to-date container for it would report.
func envFor(t *testing.T, plain map[string]string) (database.Instance, map[string]string) {
	t.Helper()
	encoded, err := encodeEncryptedEnvVars(plain)
	if err != nil {
		t.Fatalf("encode env vars: %v", err)
	}
	inst := database.Instance{Name: "bot-test", DisplayName: "Test", Status: "running", EnvVars: encoded}
	if err := database.DB.Create(&inst).Error; err != nil {
		t.Fatalf("create instance: %v", err)
	}
	live := map[string]string{"CLAWORC_INSTANCE_ID": fmt.Sprintf("%d", inst.ID)}
	for k, v := range plain {
		live[k] = v
	}
	return inst, live
}

func driftAgainst(t *testing.T, inst database.Instance, containerEnv map[string]string, touched ...string) bool {
	t.Helper()
	orch := &envDriftOrch{
		statusByName: map[string]string{inst.Name: "running"},
		envByName:    map[string]map[string]string{inst.Name: containerEnv},
	}
	drift, err := instanceEnvDrift(context.Background(), orch, inst, touched)
	if err != nil {
		t.Fatalf("instanceEnvDrift: %v", err)
	}
	return drift
}

// The bug this whole mechanism exists for: a token that was written to the
// database but never reached the container. Every path used to decide whether
// to restart from "did the row change", so once the write had landed, saving
// the same value again reported no change and skipped the restart forever --
// only a manual restart recovered it. Drift is measured against the container,
// so the second save heals it.
func TestInstanceEnvDrift_RecoversValueThatNeverReachedContainer(t *testing.T) {
	setupTestDB(t)
	inst, _ := envFor(t, map[string]string{"DISCORD_BOT_TOKEN": "tok-123"})

	container := map[string]string{"CLAWORC_INSTANCE_ID": fmt.Sprintf("%d", inst.ID)}
	if !driftAgainst(t, inst, container, "DISCORD_BOT_TOKEN") {
		t.Error("a token present in the database but absent from the container must count as drift")
	}
}

func TestInstanceEnvDrift_StaleValueInContainer(t *testing.T) {
	setupTestDB(t)
	inst, live := envFor(t, map[string]string{"DISCORD_BOT_TOKEN": "new-token"})
	live["DISCORD_BOT_TOKEN"] = "old-token"

	if !driftAgainst(t, inst, live, "DISCORD_BOT_TOKEN") {
		t.Error("a container holding an outdated value must count as drift")
	}
}

func TestInstanceEnvDrift_NoDriftWhenContainerMatches(t *testing.T) {
	setupTestDB(t)
	inst, live := envFor(t, map[string]string{"DISCORD_BOT_TOKEN": "tok-123"})

	// Image-supplied vars Claworc does not manage must not read as drift, or
	// every instance would restart on every save.
	live["PATH"] = "/usr/bin"
	live["LANG"] = "C.UTF-8"

	if driftAgainst(t, inst, live, "DISCORD_BOT_TOKEN") {
		t.Error("an up-to-date container must not be restarted")
	}
}

// A removed var leaves no trace in the desired map, so the subset check cannot
// see that the container still has it. Only the caller knows which keys it
// touched, which is why they are passed in explicitly.
func TestInstanceEnvDrift_RemovalStillLiveInContainer(t *testing.T) {
	setupTestDB(t)
	inst, live := envFor(t, map[string]string{})
	live["DISCORD_BOT_TOKEN"] = "tok-123"

	if !driftAgainst(t, inst, live, "DISCORD_BOT_TOKEN") {
		t.Error("a var removed from the database but still live in the container must count as drift")
	}
	if driftAgainst(t, inst, live) {
		t.Error("without being told the key was touched, a leftover var is indistinguishable from image env")
	}
}

// OPENCLAW_INITIAL_* is reconciled live over SSH and rewritten at every boot,
// so a stale copy in the container is not worth a restart. Without this,
// editing a Discord channel list -- which changes OPENCLAW_INITIAL_DISCORD --
// would bounce the container instead of taking effect live.
func TestInstanceEnvDrift_InitialConfigVarsAreExempt(t *testing.T) {
	setupTestDB(t)
	inst, live := envFor(t, map[string]string{})
	inst.DiscordConfig = `{"enabled":true,"channels":[]}`
	live["OPENCLAW_INITIAL_DISCORD"] = `{"enabled":false}`

	if driftAgainst(t, inst, live, "OPENCLAW_INITIAL_DISCORD") {
		t.Error("OPENCLAW_INITIAL_* has a live push path and must not force a restart")
	}
}
