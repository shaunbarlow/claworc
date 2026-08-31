#!/bin/bash
# Runs `openclaw doctor --fix --non-interactive` against the persisted PVC
# state before svc-openclaw (the gateway, i.e. the SQLite writer) starts.
#
# Why this exists: init-openclaw-seed only materializes /home/claworc/.openclaw
# on a *fresh* PVC (copies the image-baked, already-doctored skeleton). On an
# *existing* PVC carried across an image upgrade, seed is a no-op -- the
# directory already exists -- so an older-schema config/state tree meets a
# newer openclaw binary with no repair step in between. Doctor itself is only
# ever run at image build time (see the bake step in Dockerfile), never
# against runtime state, so a persisted PVC never got the newer binary's
# migrations applied.
#
# That gap is exactly what produces startup failures like:
#   "OpenClaw startup migrations did not complete cleanly; refusing to report
#   the gateway ready ... Agent identity migration requires stopped-writer
#   maintenance; stop active agents and run openclaw doctor --fix."
# svc-openclaw (the writer) hasn't started yet at this point in the s6
# dependency chain, so this is exactly the "stopped-writer" window doctor
# needs -- satisfied automatically on every boot instead of requiring a
# manual SSH-in-and-run-doctor step per upgraded instance.
#
# --non-interactive: no prompts; safe automatic migrations still apply,
# restart/service/sandbox actions requiring human confirmation are skipped
# (irrelevant here -- there's no service running yet and no supervisor other
# than s6 itself).
#
# Idempotent and cheap when there's nothing to fix. Failure here does not
# hard-fail the oneshot: leaving svc-openclaw to start (and report its own
# fail-fast migration error with actionable guidance) is safer than blocking
# every other s6 service in the container over a doctor hiccup.

set -u

TARGET=/home/claworc/.openclaw

if [ ! -d "$TARGET" ]; then
    echo "init-openclaw-doctor: $TARGET missing, skipping (init-openclaw-seed should have run first)"
    exit 0
fi

echo "init-openclaw-doctor: running openclaw doctor --fix --non-interactive against $TARGET"
# OPENCLAW_SERVICE_REPAIR_POLICY=external: s6 owns the gateway process
# lifecycle in this image, not a systemd/launchd unit doctor could detect and
# stop/restart itself. Without this, doctor's service-repair posture looks
# for a "matching managed Gateway service" and can refuse the maintenance
# window entirely when service inspection finds nothing to match against.
# This keeps doctor to state/config repairs only (exactly what we need here)
# and skips service install/start/restart/bootstrap it has no business doing
# inside this container.
s6-setuidgid claworc env OPENCLAW_SERVICE_REPAIR_POLICY=external \
    openclaw doctor --fix --non-interactive || \
    echo "init-openclaw-doctor: doctor exited non-zero (continuing; svc-openclaw will report any unresolved migration error)"
