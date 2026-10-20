# Runbooks

What to do when something goes wrong in the pilot stack ([DEPLOYMENT.md](DEPLOYMENT.md)). Every alert in
`deployments/monitoring/alerts.yml` links to a section here.

Run commands on the VM from `/opt/smsai`. `scripts/prod-compose.sh` is `docker compose` with the production env file.
Examples use `https://$DOMAIN`, and the admin API needs an allowlisted IP plus
`-H "Authorization: Bearer $ADMIN_API_TOKEN"`.

## Quick reference

| Task | Command |
|---|---|
| State of every container | `scripts/prod-compose.sh ps -a` |
| Recent logs | `scripts/prod-compose.sh logs --since 30m api worker` (JSON lines; no message text or phone numbers) |
| Flags, AI breaker, key version | `curl -fsS -H "Authorization: Bearer $ADMIN_API_TOKEN" https://$DOMAIN/admin/status` |
| Flags without HTTP | `scripts/prod-compose.sh exec -T postgres psql -U smsai -d smsai -c "SELECT * FROM system_flags"` |
| Today's usage and spend | `scripts/prod-compose.sh exec -T api node dist/admin.js usage` (`--day YYYY-MM-DD` for another UTC day) |
| SQL shell | `scripts/prod-compose.sh exec postgres psql -U smsai -d smsai` |
| Firing alerts | Grafana dashboard ("Firing alerts" table), or `scripts/prod-compose.sh exec -T alertmanager amtool --alertmanager.url=http://localhost:9093 alert query` |
| Silence an alert | Grafana **Alerting → Silences** (source "Alertmanager"), or `scripts/prod-compose.sh exec -T alertmanager amtool --alertmanager.url=http://localhost:9093 silence add alertname=JobsFailing --duration=2h --author=<you> --comment="<why>"` |
| Gateway phone health | `scripts/prod-compose.sh exec -T api node dist/admin.js gateway-status` |
| Restart the app | `scripts/prod-compose.sh up -d` (recreates only what changed) or `scripts/prod-compose.sh restart api worker` |

Phone numbers and message bodies are encrypted in the database. Nothing below needs them. Do not decrypt user data to
debug an incident.

## Kill switches

Two flags in `system_flags` stop spending without stopping the service. Use them before stopping containers.

| Flag | Off means | Users see | Still works |
|---|---|---|---|
| `ai_enabled` | No model calls | One "SMS AI is busy right now…" per user per day, credit refunded | Grounding-store answers, commands (`HELP`, `MORE`, `STOP`, …) |
| `sms_outbound_enabled` | No reply is handed to the phone | No reply | Inbound texts are still stored |

```bash
scripts/prod-compose.sh exec -T api node dist/admin.js flag ai_enabled false
scripts/prod-compose.sh exec -T api node dist/admin.js flag sms_outbound_enabled false
# or over HTTP (takes effect in the API immediately):
curl -fsS -X POST -H "Authorization: Bearer $ADMIN_API_TOKEN" -d '{"ai_enabled": false}' https://$DOMAIN/admin/flags
```

- Changes take effect within 3 seconds; each process caches flags that long. The kill-switch change is logged at WARN.
- With only `sms_outbound_enabled=false`, questions are still answered by the model (AI spend continues), but the
  replies are marked `blocked_disabled` and **not sent later**. To stop all spend, turn off both.
- Turn a flag back on with `true`. Nothing queued while it was off is replayed.
- To stop the phone sending without touching the server, use **Pause replies** in the gateway app: replies wait on the
  server (up to `GATEWAY_OUTBOX_TTL_SECONDS`) and go out when you resume.
- Last resort: `scripts/prod-compose.sh stop api worker`. The phone keeps every text it receives and forwards it when the
  API is back.

## Gateway phone offline

Alerts: `GatewayPhoneOffline` (no heartbeat for 5 minutes), `GatewayRepliesExpired` (replies given up after
`GATEWAY_OUTBOX_TTL_SECONDS`, default 6 h). The phone is the only way texts come in and replies go out, so while it is
offline nobody gets answers. Nothing is lost on the phone: texts it receives while offline are saved and forwarded
later. Replies wait on the server until the TTL, then are marked `failed` (`phone_offline`) and their segments refunded.

1. Look at the phone. The persistent notification says "SMS AI gateway is running" or names the problem. Open the app:
   **Status** shows the last contact time and the last error, **Recent activity** the last events.
2. Match the error:
   - *Server refused the pairing (401)*: the device secret no longer matches `GATEWAY_DEVICE_SECRET` (rotated without
     re-pairing). Re-pair: `scripts/prod-compose.sh exec api node dist/admin.js gateway-pair`, then **Setup → Unpair**
     and scan the new code.
   - *UnknownHostException / ConnectException / SocketTimeoutException*: no data on the phone, or the server is down.
     Check `/healthz`, Wi-Fi or mobile data, and the data bundle.
   - No notification at all: the service was killed. Open the app (it restarts), then check **Setup → Stay awake**:
     battery optimisation must be off, and the maker's auto-start / sleeping-apps settings must allow the app.
3. Check the phone is charging, has signal, and has airtime or an SMS bundle (`sim_label`, `battery` and `signal` are in
   `/admin/status` → `gateway.devices` from its last heartbeat).
4. `scripts/prod-compose.sh exec -T api node dist/admin.js gateway-status` shows the last heartbeat and the outbox
   counts for the last day.
5. A reboot of the phone is safe: the service starts again on boot, and the local queues survive.

## Gateway acked but no status

Alert: `GatewayRepliesUnconfirmed`. The phone took a reply (acknowledged it) more than `GATEWAY_ACK_TIMEOUT_SECONDS` ago
but never reported it sent or failed. The usual cause is the app being killed between handing the text to the radio and
hearing back, so the reply may or may not have gone out. The service never resends these automatically, to avoid
duplicate texts.

1. Open the app's **Recent activity**: a "Reply to +233…" line at that time means the phone did try to send it.
2. List the affected replies (no content):

   ```sql
   SELECT message_id, acked_at, offered_count FROM gateway_outbox
   WHERE state = 'acked' AND stale_flagged ORDER BY acked_at DESC;
   ```

3. Usually do nothing: the user can text again. Many at once means the phone's process is being killed; fix that as in
   [gateway phone offline](#gateway-phone-offline), step 2.

## Provider outage

### The SIM or the network

Signs: the phone is online (heartbeats arrive) but replies end `failed`, with errors such as `no_service`,
`radio_off` or `generic_failure` in the app's activity log, or texts stop arriving while the phone looks healthy.

1. Check the phone has signal and airtime/bundle. A `generic_failure` on every send often means no credit.
2. If the network flagged the SIM for sending many automated texts, sends fail or the line is barred: call the network.
   Keep `GLOBAL_DAILY_SMS_SEGMENT_CAP` modest and the app's sending pace at 2 s or more.
3. If Android shows "This app is sending a large number of SMS messages" on the phone, the outgoing SMS limit was not
   lifted: run `adb shell settings put global sms_outgoing_check_max_count 100000` (apps/gateway/README.md).

### AI provider (DeepInfra)

Signs: `ai_requests_total` with error statuses, `"ai_breaker":"open"` in `/admin/status` or `/readyz`, slow answers.

The AI client already retries, falls back to `LLM_FALLBACK_MODEL` and opens a circuit breaker. While the breaker is
open, users get the "busy" text once a day and their credit back.

1. Check DeepInfra's status page. Check for HTTP 401/402 in the worker log, which mean a key or billing problem.
2. If one model is down but DeepInfra is up, point `LLM_MODEL`/`LLM_FALLBACK_MODEL` at another model, then run
   `scripts/prod-compose.sh up -d`.
3. If the outage is long, turn `ai_enabled` off so questions get one clear notice instead of retries. Grounding answers
   continue. Turn it back on when DeepInfra recovers.

## AI empty reasoning responses

Alert: `AIEmptyReasoningResponse`. A reasoning model spent its whole token budget on hidden reasoning and returned no
visible text. Plan §7.2 requires this to stay at zero. The user got the fallback model's answer, or the apology.

1. Check `LLM_REASONING_EFFORT=low` and `LLM_MAX_OUTPUT_TOKENS` in the env file. gpt-oss needs at least 350, and the
   client enforces that.
2. If `openai/gpt-oss-20b` keeps returning empty answers, swap `LLM_MODEL` and `LLM_FALLBACK_MODEL` so the
   non-reasoning `meta-llama/Llama-3.3-70B-Instruct-Turbo` answers first, then run `scripts/prod-compose.sh up -d`.

## Webhook rejections

Alert: `WebhookRejectedSpike`, labelled with `provider` and `reason`. For `provider="gateway"` these are requests to
`/gateway/v1/*`.

- **`reason="signature"`.** The request was not signed with a current `GATEWAY_DEVICE_SECRET`, or its timestamp is more
  than 5 minutes off. Either someone is sending forged requests (they are rejected, so no harm is done), or:
  - The secret was rotated without re-pairing the phone. During a rotation list both values, new first, re-pair, then
    drop the old one.
  - The phone's clock is wrong. Turn on automatic date and time on the phone.
  - If the secret may have leaked, rotate it: whoever holds it can read queued replies and inject texts.
- **`reason="payload"`.** The app sent a body the server does not accept: usually an app and server from different
  versions of `packages/shared`. Update both.

## Unknown-timeout sends

Alert: `SMSOutboundUnknownTimeout`. With the gateway this should not happen: handing a reply to the phone is a database
insert, which either succeeds or is retried. The phone-side equivalent is [acked but no status](#gateway-acked-but-no-status).
If the alert fires, look for database errors in the worker log.

## Failing jobs

Alert: `JobsFailing`, labelled with `kind` and `result`.

- **`answer` / `failed`:** the model kept erroring after `JOB_MAX_ATTEMPTS` tries, and the user got the apology text
  (once a day). Often part of an [AI provider outage](#ai-provider-deepinfra).
- **`answer` / `send_failed`:** an answer was generated but could not be queued for sending after retries. Look for
  database errors.
- **`send` / `failed`:** the provider permanently rejected an SMS, e.g. an invalid number or sender ID.
- `ai_unavailable` results (kill switch, spend cap, open breaker) are expected and do not alert.

```sql
SELECT kind, result, left(last_error, 120) AS error, count(*)
FROM jobs WHERE status = 'failed' AND updated_at > now() - interval '1 hour'
GROUP BY 1, 2, 3 ORDER BY 4 DESC;
```

Also check `scripts/prod-compose.sh logs --since 1h worker | grep -E '"level":"(ERROR|WARN)"'`.

## Process down

Alerts: `SMSAIProcessDown` (api or worker), `MonitoringTargetDown` (node-exporter, Alertmanager).

1. `scripts/prod-compose.sh ps -a`. A restarting or exited container is the culprit.
2. `scripts/prod-compose.sh logs --tail 100 <service>`. Common causes:
   - `invalid configuration:` followed by a list: an env-file edit broke validation. Fix the listed settings, then run
     `scripts/prod-compose.sh up -d`.
   - `connecting to database`: Postgres is down or full. Check `scripts/prod-compose.sh logs postgres` and
     [disk space](#disk-space).
   - `migrate` exited non-zero: `api` and `worker` wait for it. Read its log. A bad migration means
     [rolling back](DEPLOYMENT.md#5-upgrades).
   - Out of memory: `docker inspect -f '{{.State.OOMKilled}}' smsai-prod-api-1`.
3. `scripts/prod-compose.sh up -d` starts whatever is missing. Containers restart on their own (`unless-stopped`) after
   crashes and reboots.

If the whole VM is down, nothing here alerts. The external uptime check on `/healthz` does
([DEPLOYMENT.md §4](DEPLOYMENT.md#4-first-deploy)). Restart the VM from the cloud console. If it is gone,
[restore onto a new VM](#b-onto-a-new-vm).

## Backup failed

Alerts: `BackupFailed` (the last dump failed), `BackupStale` (no successful dump for 26 h).

1. `scripts/prod-compose.sh logs --since 26h backup`. Look for `FAILED`.
2. Common causes: a full disk ([disk space](#disk-space)), Postgres down, or a changed `POSTGRES_PASSWORD` that was not
   applied everywhere (`password authentication failed`).
3. After fixing it, dump now: `scripts/prod-compose.sh exec -T backup sh /opt/backup/backup.sh once`. The alerts
   resolve within a few minutes.
4. If the backup container itself is not running: `scripts/prod-compose.sh up -d backup`.

## Disk space

Alerts: `DiskSpaceLow` (< 15% free), `DiskSpaceCritical` (< 5%). Postgres stops accepting writes when its disk is full.

```bash
df -h
sudo du -sh /var/backups/smsai /var/lib/docker/volumes/* | sort -h | tail
docker system df
```

- **Dumps:** check they have been copied off the VM, then lower `BACKUP_RETENTION_DAYS` or delete the oldest
  `smsai-*.dump`/`.counts` pairs.
- **Prometheus:** lower `PROMETHEUS_RETENTION_SIZE` / `PROMETHEUS_RETENTION_TIME`, then run
  `scripts/prod-compose.sh up -d prometheus`.
- **Old images and build cache:** `docker image prune` (dangling only) and `docker builder prune`. Keep the previous
  `smsai-ghana` tag for rollback.
- Container logs are capped at 5 × 10 MB per container.
- **Postgres:** the worker purges messages after `RETENTION_DAYS` every hour. If the database is still growing, check
  the largest tables:
  `SELECT relname, pg_size_pretty(pg_total_relation_size(relid)) FROM pg_stat_user_tables ORDER BY 2 DESC LIMIT 5;`.
- Long term: grow the disk in the cloud console, or move `BACKUP_DIR` to a separate volume.

## Restore drill

Alert: `RestoreDrillStale` (no passed drill for 8 days). Run it weekly from cron ([DEPLOYMENT.md §7](DEPLOYMENT.md#7-backups)).

```bash
scripts/restore_drill.sh                              # newest dump
scripts/restore_drill.sh smsai-20261017T020000Z.dump  # a specific one
```

The drill restores the dump into a scratch database, `smsai_restore_drill`, on the pilot's Postgres. It compares every
table's row count with the dump's `.counts` manifest, checks the schema version and the kill-switch rows, prints a
table, and drops the scratch database. The live database is only read. Exit status 0 means it passed, and the pass is
recorded for Prometheus.

If it fails:

- **Row-count mismatch:** the manifest and the dump come from the same snapshot, so this is real. Take a fresh dump
  (`backup.sh once`) and drill it. If the fresh one passes, delete the bad dump and check the disk for errors
  (`dmesg`). If it fails too, stop and investigate before trusting any dump.
- **`pg_restore` error:** often a full disk, or a dump from a newer PostgreSQL major version. The output names the
  failing object.

## Restore

The newest good dump replaces the database. Everything written after the dump time is lost: up to 24 hours with
nightly dumps. The keys in the env file must be the ones in use when the dump was taken
([DEPLOYMENT.md §6](DEPLOYMENT.md#6-key-and-secret-rotation)); with other keys the restored data cannot be decrypted.

### A. In place (bad migration, corrupted or wrongly modified data)

```bash
# 1. Stop the writers. Webhooks get 502 until step 5.
scripts/prod-compose.sh stop api worker

# 2. Keep the current state, if the database still works.
scripts/prod-compose.sh exec -T backup sh /opt/backup/backup.sh once

# 3. Restore the chosen dump into a new database and check it.
DUMP=smsai-20261017T020000Z.dump
scripts/restore_drill.sh "$DUMP"
scripts/prod-compose.sh run --rm --entrypoint sh backup -c "
  psql -v ON_ERROR_STOP=1 -d postgres -c 'DROP DATABASE IF EXISTS smsai_new' -c 'CREATE DATABASE smsai_new' &&
  pg_restore --exit-on-error --no-owner --no-privileges -d smsai_new /backups/$DUMP"

# 4. Swap. Nothing may be connected to smsai.
scripts/prod-compose.sh exec -T postgres psql -U smsai -d postgres \
  -c "ALTER DATABASE smsai RENAME TO smsai_before_restore_$(date -u +%Y%m%d)" \
  -c "ALTER DATABASE smsai_new RENAME TO smsai"
```

Before step 5, decide what to do with jobs that were still open in the dump. Answer jobs pending at dump time run again
and may answer a question the user already got an answer to:

```sql
-- scripts/prod-compose.sh exec postgres psql -U smsai -d smsai
SELECT kind, status, count(*) FROM jobs WHERE status IN ('pending', 'running') GROUP BY 1, 2;
-- To drop them instead:
-- UPDATE jobs SET status = 'cancelled', result = 'restored_stale', updated_at = now()
--   WHERE status IN ('pending', 'running');
```

Messages that were mid-send at dump time are marked `unknown_timeout` on the next attempt, never resent.

```bash
# 5. Start. migrate applies any newer migrations first.
scripts/prod-compose.sh up -d
curl -fsS -H "Authorization: Bearer $ADMIN_API_TOKEN" https://$DOMAIN/admin/status
```

After a few days without problems, drop the old database:
`DROP DATABASE smsai_before_restore_<date>;`.

### B. Onto a new VM

1. Build the new VM with [DEPLOYMENT.md](DEPLOYMENT.md) §2. Do not run `init_env_prod.sh`. Restore
   `deployments/.env.prod` from your secret store instead: the same keys, tokens and `POSTGRES_PASSWORD`.
2. Copy the newest dump and its `.counts` file from off-site storage into `BACKUP_DIR` (`/var/backups/smsai`).
3. Restore into the empty database before the app starts:

   ```bash
   scripts/prod-compose.sh build api
   scripts/prod-compose.sh up -d postgres
   scripts/prod-compose.sh run --rm --entrypoint sh backup -c \
     "pg_restore --exit-on-error --no-owner --no-privileges -d smsai /backups/<dump>"
   ```

4. Review the open jobs as in A, then run `scripts/prod-compose.sh up -d`.
5. Point the DNS A record at the new IP. Provider callbacks keep working because the hostname is unchanged. Run
   `scripts/restore_drill.sh` once the first nightly dump exists, and check Grafana.
