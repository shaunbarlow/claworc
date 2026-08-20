package handlers

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gluk-w/claworc/control-plane/internal/database"
	"github.com/gluk-w/claworc/control-plane/internal/orchestrator"
)

// Env vars reach a container exactly once, when its spec is built (create,
// restart, image update). Nothing re-reads them afterwards, so every path that
// writes an env var to the database has to decide for itself whether to
// restart -- and each one used to decide from a change event: "the row
// changed, so restart". That inference fails in both directions:
//
//   - The row can change while the instance is still provisioning or its
//     status column is stale, in which case the restart is skipped and never
//     retried.
//   - Re-saving the same value reports "unchanged" and skips the restart, so
//     a value that missed its window the first time can never be pushed again.
//     Only a manual restart recovers it.
//
// EnsureEnvPropagated replaces the inference with a fact: it compares the env
// the database says an instance should have against the env its container is
// actually running with, and restarts only on a real difference. Because the
// decision is made against live state rather than an event, it is idempotent,
// safe to call from any path, and self-healing on the next call.

// envDriftExempt reports whether an env var may legitimately differ between
// the desired spec and a running container.
//
// OPENCLAW_INITIAL_* only seeds the agent's OpenClaw config at boot, and every
// one of them has a live push path over SSH (pushSlackConfig,
// pushDiscordConfig, ConfigureInstance). A stale value in a running container
// is therefore not worth a restart: the agent's config is already correct, and
// the var is rewritten at the next boot anyway. Counting these as drift would
// turn every channel-config edit into a container restart.
func envDriftExempt(name string) bool {
	return strings.HasPrefix(name, "OPENCLAW_INITIAL_")
}

// envPropagationTimeout bounds the orchestrator round-trips made while
// answering an HTTP request. A slow or unreachable backend must not hold the
// handler open; failing the check only means no restart this time, and the
// next call re-checks from scratch.
const envPropagationTimeout = 10 * time.Second

// instanceEnvDrift reports whether the container running inst is missing an
// env var the database says it should have, or is holding a stale value for
// one.
//
// touched names the env vars the caller just wrote. Those get a stricter,
// two-way check: a key the caller *removed* is absent from the desired map, so
// the one-way "every desired var is present and equal" test cannot notice that
// the container still has it. Removal has to be told rather than inferred --
// any other key in the container may legitimately come from the image.
func instanceEnvDrift(ctx context.Context, orch orchestrator.ContainerOrchestrator, inst database.Instance, touched []string) (bool, error) {
	actual, err := orch.GetInstanceEnv(ctx, inst.Name)
	if err != nil {
		return false, err
	}
	desired := buildCreateParams(inst).EnvVars

	for name, want := range desired {
		if envDriftExempt(name) {
			continue
		}
		if got, ok := actual[name]; !ok || got != want {
			return true, nil
		}
	}
	for _, name := range touched {
		if envDriftExempt(name) {
			continue
		}
		if _, stillDesired := desired[name]; stillDesired {
			continue // already covered by the loop above
		}
		if _, present := actual[name]; present {
			return true, nil // removed in the database, still live in the container
		}
	}
	return false, nil
}

// EnsureEnvPropagated restarts inst when its container does not have the env
// vars the database says it should, and reports whether a restart was started.
// touched lists the env var names the caller just wrote (see instanceEnvDrift)
// and may be omitted for a plain reconciliation pass.
//
// An instance that is not live is left alone: its next create or start builds
// a fresh spec straight from the database, so there is nothing to reconcile.
func EnsureEnvPropagated(ctx context.Context, inst database.Instance, userID uint, touched ...string) bool {
	orch := orchestrator.Get()
	if orch == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, envPropagationTimeout)
	defer cancel()

	if status, err := orch.GetInstanceStatus(ctx, inst.Name); err != nil || status != "running" {
		return false
	}
	drift, err := instanceEnvDrift(ctx, orch, inst, touched)
	if err != nil {
		log.Printf("env-propagation: cannot read container env for instance %d, skipping restart: %v", inst.ID, err)
		return false
	}
	if !drift {
		return false
	}
	restartInstanceAsyncWithToast(inst, userID,
		fmt.Sprintf("Restarting agent %s", inst.DisplayName),
		"Applying updated environment variables")
	return true
}
