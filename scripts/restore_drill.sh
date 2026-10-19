#!/bin/sh
# Restore drill (plan §15 week 6). Restores the newest nightly dump, or the
# one named, into a scratch database (smsai_restore_drill) on the pilot's
# Postgres. It then compares every table's row count with the manifest that
# backup.sh wrote from the dump's own snapshot. The live database is only
# read from. The scratch database is dropped afterwards.
#
#   scripts/restore_drill.sh
#   scripts/restore_drill.sh smsai-20261017T020000Z.dump
#
# Runs inside the `backup` container, which has the dumps and the Postgres
# client tools. Exit status 0 means the drill passed. A pass is published
# for Prometheus (RestoreDrillStale alert). Schedule it weekly; see
# docs/RUNBOOKS.md.
set -eu
export MSYS_NO_PATHCONV=1
here=$(cd "$(dirname "$0")" && pwd)

sh "$here/prod-compose.sh" exec -T backup sh -s -- "${1:-}" <<'DRILL'
set -eu
export PGOPTIONS='-c client_min_messages=warning'
db=smsai_restore_drill
log() { echo "$(date -u +%H:%M:%S) drill: $*"; }

dump=${1:-}
if [ -z "$dump" ]; then
	# Names sort by their UTC timestamp.
	dump=$(find /backups -maxdepth 1 -name 'smsai-*.dump' | sort | tail -n 1)
fi
case $dump in /*) ;; *) dump="/backups/$dump" ;; esac
if [ ! -f "$dump" ]; then
	log "no dump found in /backups (has the backup service run yet?)"
	exit 1
fi
manifest="${dump%.dump}.counts"
if [ ! -f "$manifest" ]; then
	log "missing row-count manifest $manifest"
	exit 1
fi

trap 'psql -X -q -d postgres -c "DROP DATABASE IF EXISTS $db" >/dev/null 2>&1 || true' EXIT
log "restoring $(basename "$dump") ($(wc -c <"$dump" | tr -d ' ') bytes) into $db"
psql -X -q -d postgres -v ON_ERROR_STOP=1 -c "DROP DATABASE IF EXISTS $db" -c "CREATE DATABASE $db"
started=$(date +%s)
pg_restore --exit-on-error --no-owner --no-privileges -d "$db" "$dump"
took=$(($(date +%s) - started))
psql -X -q -At -F ' ' -v ON_ERROR_STOP=1 -d "$db" -f /opt/backup/row_counts.sql >/tmp/restored.counts

echo
printf "%-24s %12s %12s\n" "table" "in dump" "restored"
awk 'NR == FNR { want[$1] = $2; next }
	{ got[$1] = $2 }
	END {
		for (t in want) printf "%-24s %12s %12s%s\n", t, want[t], (t in got ? got[t] : "MISSING"), (want[t] == got[t] ? "" : "   <-- MISMATCH")
		for (t in got) if (!(t in want)) printf "%-24s %12s %12s   <-- UNEXPECTED\n", t, "-", got[t]
	}' "$manifest" /tmp/restored.counts | sort
echo

if ! diff -q "$manifest" /tmp/restored.counts >/dev/null; then
	log "FAILED: restored row counts differ from the dump's manifest"
	exit 1
fi
version=$(psql -X -At -d "$db" -c "SELECT max(version) FROM schema_migrations")
flags=$(psql -X -At -d "$db" -c "SELECT count(*) FROM system_flags WHERE key IN ('ai_enabled', 'sms_outbound_enabled')")
if [ "$flags" != 2 ]; then
	log "FAILED: system_flags is incomplete"
	exit 1
fi
tables=$(wc -l <"$manifest" | tr -d ' ')
log "PASSED: $tables tables match, schema version $version, pg_restore took ${took}s"

tmp=/textfile/.drill.$$
{
	echo "# HELP smsai_restore_drill_last_success_timestamp_seconds Unix time of the last passed restore drill."
	echo "# TYPE smsai_restore_drill_last_success_timestamp_seconds gauge"
	echo "smsai_restore_drill_last_success_timestamp_seconds $(date +%s)"
	echo "# HELP smsai_restore_drill_restore_seconds pg_restore duration in the last passed drill."
	echo "# TYPE smsai_restore_drill_restore_seconds gauge"
	echo "smsai_restore_drill_restore_seconds $took"
} >"$tmp"
chmod 644 "$tmp"
mv "$tmp" /textfile/smsai_restore_drill.prom
DRILL
