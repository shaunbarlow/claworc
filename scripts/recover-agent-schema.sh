#!/usr/bin/env bash
# Recover a Claworc agent whose OpenClaw gateway crash-loops after an image
# upgrade because the persisted agent databases on the instance volume are on
# an older SQLite schema than the new OpenClaw expects (SqliteSchemaMismatchError).
#
# The image runs `openclaw doctor --fix` only at build time against the baked
# skeleton, so an existing volume is never migrated on upgrade. This script
# does that migration in place, safely:
#
#   1. confirm the gateway is actually failing on a schema mismatch
#   2. stop the gateway through s6 (doctor requires no active agents)
#   3. back up every agent database on the volume
#   4. run `openclaw doctor --fix --non-interactive` as claworc
#   5. bring the gateway back and wait for "[gateway] ready"
#   6. on any failure, restore the backups and bring the gateway back up
#
# Usage: scripts/recover-agent-schema.sh <container> [--force]
#   --force  run even if no schema mismatch was found in the recent log
set -euo pipefail

CONTAINER="${1:?usage: $0 <container> [--force]}"
FORCE="${2:-}"
S6=/command
SVC=/run/service/svc-openclaw
LOG=/var/log/claworc/openclaw.log
OC_HOME=/home/claworc
READY_TIMEOUT="${READY_TIMEOUT:-300}"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
cx() { docker exec "$CONTAINER" bash -c "$1"; }
cx_oc() {
    docker exec -u claworc -e HOME="$OC_HOME" -e OPENCLAW_SERVICE_REPAIR_POLICY=external \
        "$CONTAINER" bash -c "$1"
}

schema_versions() {
    cx_oc 'for f in '"$OC_HOME"'/.openclaw/agents/*/agent/openclaw-agent.sqlite; do
        v=$(node -e "const {DatabaseSync}=require(\"node:sqlite\");
            const d=new DatabaseSync(process.argv[1],{readOnly:true});
            console.log(d.prepare(\"PRAGMA user_version\").get().user_version)" "$f" 2>&1)
        echo "$f $v"
    done'
}

# 1. Detect.
if [ "$FORCE" != "--force" ]; then
    if ! cx "tail -n 2000 $LOG | grep -qE 'SqliteSchemaMismatchError|uses schema version'"; then
        log "no schema mismatch in recent gateway log for $CONTAINER; nothing to do (use --force to override)"
        exit 0
    fi
fi
log "schema mismatch detected on $CONTAINER; current schema versions:"
schema_versions

# 2. Stop the gateway and wait until s6 reports it down.
log "stopping svc-openclaw"
cx "$S6/s6-svc -d $SVC && $S6/s6-svwait -D -t 60000 $SVC"
if cx "pgrep -u claworc -f 'openclaw-gateway|openclaw gateway run' >/dev/null"; then
    log "gateway process still alive after s6 stop; aborting before touching databases"
    cx "$S6/s6-svc -u $SVC"
    exit 1
fi

# 3. Back up every agent database (plus -wal/-shm) with the service down.
TS="$(date -u +%Y%m%dT%H%M%SZ)"
BACKUP="$OC_HOME/.openclaw/backups/schema-recovery-$TS"
log "backing up agent databases to $BACKUP"
cx_oc "set -e; mkdir -p '$BACKUP'
    cd $OC_HOME/.openclaw/agents
    for d in */agent; do
        mkdir -p '$BACKUP'/\$d
        cp -a \$d/openclaw-agent.sqlite '$BACKUP'/\$d/
        for s in -wal -shm; do [ -e \$d/openclaw-agent.sqlite\$s ] && cp -a \$d/openclaw-agent.sqlite\$s '$BACKUP'/\$d/ || true; done
    done
    cp -a $OC_HOME/.openclaw/openclaw.json '$BACKUP'/ 2>/dev/null || true"

restore_and_restart() {
    log "restoring databases from $BACKUP"
    cx_oc "cd '$BACKUP' && for d in */agent; do cp -a \$d/. $OC_HOME/.openclaw/agents/\$d/; done" || true
    cx "$S6/s6-svc -u $SVC" || true
}

# 4. Migrate.
log "running openclaw doctor --fix --non-interactive"
if ! cx_oc "openclaw doctor --fix --non-interactive 2>&1 | grep -v '^No change\$' | tail -60"; then
    log "doctor failed"
    restore_and_restart
    exit 1
fi
log "schema versions after doctor:"
schema_versions

# 5. Start the gateway and wait for it to report ready (or fail again).
MARK="$(cx "wc -l < $LOG")"
log "starting svc-openclaw; waiting up to ${READY_TIMEOUT}s for [gateway] ready"
cx "$S6/s6-svc -u $SVC"
deadline=$((SECONDS + READY_TIMEOUT))
while [ $SECONDS -lt $deadline ]; do
    new="$(cx "tail -n +$((MARK + 1)) $LOG | grep -v '^{'" || true)"
    if grep -q '\[gateway\] ready' <<<"$new"; then
        log "gateway is ready on $CONTAINER. Backup kept at $BACKUP"
        exit 0
    fi
    if grep -q 'Gateway failed to start' <<<"$new"; then
        log "gateway still failing after migration:"
        grep 'Gateway failed to start' <<<"$new" | tail -1
        # 6. Roll back so the volume is left exactly as we found it.
        cx "$S6/s6-svc -d $SVC && $S6/s6-svwait -D -t 60000 $SVC" || true
        restore_and_restart
        exit 1
    fi
    sleep 5
done
log "timed out waiting for gateway ready; leaving it running, backup at $BACKUP"
exit 1
