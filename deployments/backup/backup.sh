#!/bin/sh
# Nightly logical backup for the pilot (plan §15 week 6). The `backup`
# service in deployments/compose.prod.yaml runs it; PG* variables select the
# database.
#
#   backup.sh         daemon: dump now unless a dump from the last 24 h
#                     exists, then every night at BACKUP_HOUR_UTC
#   backup.sh once    one dump, then exit
#
# Each smsai-<UTC time>.dump (pg_dump custom format) gets a .counts manifest:
# the row count of every table, read in the same snapshot as the dump, so
# scripts/restore_drill.sh can prove a restore is complete. Dumps older than
# BACKUP_RETENTION_DAYS are deleted after each successful dump. Results go to
# node-exporter's textfile collector for the BackupStale alert.
#
# Message bodies and phone numbers are stored encrypted, so a dump is useless
# without the ENCRYPTION_KEYS_V*/HMAC_KEYS_V* from the env file. Keep those
# backed up separately from the dumps.
set -eu

BACKUP_DIR=/backups
TEXTFILE_DIR=/textfile
HOUR=${BACKUP_HOUR_UTC:-2}
KEEP_DAYS=${BACKUP_RETENTION_DAYS:-14}

log() { echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) backup: $*"; }

# metric NAME VALUE HELP: (re)write one gauge file for node-exporter.
metric() {
	tmp="$TEXTFILE_DIR/.$1.$$"
	{
		echo "# HELP $1 $3"
		echo "# TYPE $1 gauge"
		echo "$1 $2"
	} >"$tmp"
	chmod 644 "$tmp"
	mv "$tmp" "$TEXTFILE_DIR/$1.prom"
}

dump() {
	stamp=$(date -u +%Y%m%dT%H%M%SZ)
	base="$BACKUP_DIR/smsai-$stamp"
	work=$(mktemp -d)
	rm -f "$BACKUP_DIR"/*.partial

	# A holder session exports a snapshot and keeps its transaction open
	# while pg_dump and the row counts both read that snapshot.
	mkfifo "$work/ctl"
	psql -X -q -At -v ON_ERROR_STOP=1 <"$work/ctl" >"$work/snapshot" &
	holder=$!
	exec 3>"$work/ctl"
	printf 'BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;\nSELECT pg_export_snapshot();\n' >&3
	tries=0
	while [ ! -s "$work/snapshot" ]; do
		tries=$((tries + 1))
		if [ "$tries" -gt 150 ] || ! kill -0 "$holder" 2>/dev/null; then
			log "could not export a snapshot"
			return 1
		fi
		sleep 0.2
	done
	snapshot=$(head -n 1 "$work/snapshot")

	pg_dump -Fc --snapshot="$snapshot" -f "$base.dump.partial"
	psql -X -q -At -F ' ' -v ON_ERROR_STOP=1 >"$base.counts.partial" <<-SQL
		BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
		SET TRANSACTION SNAPSHOT '$snapshot';
		\i /opt/backup/row_counts.sql
		COMMIT;
	SQL
	printf 'COMMIT;\n' >&3
	exec 3>&-
	wait "$holder" || true
	rm -rf "$work"

	# A dump pg_restore cannot list is not a backup.
	pg_restore --list "$base.dump.partial" >/dev/null
	chmod 600 "$base.dump.partial" "$base.counts.partial"
	mv "$base.counts.partial" "$base.counts"
	mv "$base.dump.partial" "$base.dump"

	size=$(wc -c <"$base.dump" | tr -d ' ')
	tables=$(wc -l <"$base.counts" | tr -d ' ')
	log "wrote $(basename "$base.dump") ($size bytes, $tables tables)"
	metric smsai_backup_last_success_timestamp_seconds "$(date +%s)" "Unix time of the last successful pg_dump."
	metric smsai_backup_last_size_bytes "$size" "Size of the last successful pg_dump in bytes."

	find "$BACKUP_DIR" -maxdepth 1 \( -name 'smsai-*.dump' -o -name 'smsai-*.counts' \) -mtime +"$KEEP_DAYS" \
		-print -exec rm -f {} \; | sed 's/^/pruned /'
}

# Each dump runs in a fresh shell: `set -e` is ignored inside functions
# called from `if`/`until`, so the daemon must not call dump() directly.
run_dump() {
	sh "$0" once
}

seconds_until_next_run() {
	now=$(($(date -u +%H | sed 's/^0//') * 3600 + $(date -u +%M | sed 's/^0//') * 60 + $(date -u +%S | sed 's/^0//')))
	wait_s=$((HOUR * 3600 - now))
	[ "$wait_s" -gt 0 ] || wait_s=$((wait_s + 86400))
	echo "$wait_s"
}

# Sleep in the background so `docker stop` (SIGTERM) ends the wait at once.
pause() {
	sleep "$1" &
	wait $! || true
}

mkdir -p "$BACKUP_DIR" "$TEXTFILE_DIR"
umask 077

on_exit() {
	rc=$?
	rm -rf "${work:-}"
	if [ "$rc" -ne 0 ]; then
		log "FAILED (exit $rc)"
		metric smsai_backup_last_failure_timestamp_seconds "$(date +%s)" "Unix time of the last failed pg_dump."
	fi
}

if [ "${1:-}" = "once" ]; then
	trap on_exit EXIT
	dump
	exit 0
fi

trap 'log "stopping"; exit 0' TERM INT
log "nightly at ${HOUR}:00 UTC, keeping ${KEEP_DAYS} days in $BACKUP_DIR"
if [ -z "$(find "$BACKUP_DIR" -maxdepth 1 -name 'smsai-*.dump' -mmin -1440)" ]; then
	run_dump || true
fi
while true; do
	pause "$(seconds_until_next_run)"
	until run_dump; do
		log "retrying in 1 h"
		pause 3600
	done
done
