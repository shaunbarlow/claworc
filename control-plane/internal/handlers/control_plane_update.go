package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/gluk-w/claworc/control-plane/internal/analytics"
	"github.com/gluk-w/claworc/control-plane/internal/orchestrator"
	"github.com/gluk-w/claworc/control-plane/internal/taskmanager"
)

// SelfUpdateControlPlane handles POST /api/v1/control-plane/self-update.
// Admin-only (see main.go route group). Triggers a pull-and-restart of the
// control-plane's own container/pod on its current image reference (so a
// "latest"-tagged deployment picks up a newer build without an operator
// having to SSH into the host or re-run the installer).
//
// This is fundamentally different from instance image updates: the process
// handling this very request is about to be replaced. The handler kicks off
// the update asynchronously via TaskManager (mirroring UpdateInstanceImage)
// and returns immediately with 202 — by the time a client could poll for
// the task's outcome, the old process may already be gone. The real signal
// to the operator is the dashboard going briefly unreachable and then
// coming back up; the task's terminal state is best-effort, informational
// only for whichever request happens to still be in flight.
func SelfUpdateControlPlane(w http.ResponseWriter, r *http.Request) {
	orch := orchestrator.Get()
	if orch == nil {
		WriteOrchestratorUnavailable(w)
		return
	}

	userID := callerID(r)
	log.Printf("Control-plane self-update requested by user %d", userID)

	analytics.Track(r.Context(), analytics.EventControlPlaneSelfUpdate, nil)

	taskID := ""
	// work returns (updated, err). updated is false when SelfUpdate determined
	// the target image already matches what's running (by registry/local
	// digest comparison -- see orchestrator.SelfUpdate) and skipped the
	// restart entirely.
	work := func(ctx context.Context) (bool, error) {
		// Use a fresh background context for the actual orchestrator call:
		// the HTTP request context (r.Context()) is canceled the moment this
		// handler returns/the connection closes, but for Docker the pull +
		// helper-container launch must complete regardless of whether the
		// client is still around to see the response.
		updated, err := orch.SelfUpdate(context.Background(), "")
		if err != nil {
			log.Printf("Control-plane self-update failed: %v", err)
			return false, err
		}
		return updated, nil
	}

	if TaskMgr != nil {
		taskID = TaskMgr.Start(taskmanager.StartOpts{
			Type:         taskmanager.TaskControlPlaneSelfUpdate,
			UserID:       userID,
			ResourceName: "Control Plane",
			Title:        "Updating Claworc control plane",
			Run: func(ctx context.Context, h *taskmanager.Handle) error {
				h.UpdateMessage("Checking for a newer image...")
				updated, err := work(ctx)
				if err != nil {
					return err
				}
				if !updated {
					// Terminal message shown on the toast once the task finishes;
					// distinguishes "already up to date" from a real restart for
					// whichever request/session is still around to see it.
					h.UpdateMessage("Already running the latest image; no restart needed.")
				}
				return nil
			},
		})
	} else {
		go func() {
			if _, err := work(context.Background()); err != nil {
				log.Printf("Control-plane self-update (no task manager) failed: %v", err)
			}
		}()
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "updating",
		"task_id": taskID,
		"detail":  fmt.Sprintf("Update check initiated by user %d. If a newer image is available the dashboard will be briefly unreachable while it restarts; otherwise nothing happens.", userID),
	})
}
