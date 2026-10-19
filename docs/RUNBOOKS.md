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
| Today's usage and spend | `scripts/prod-compose.sh exec -T api /app/admin usage` (`-day YYYY-MM-DD` for another UTC day) |
| SQL shell | `scripts/prod-compose.sh exec postgres psql -U smsai -d smsai` |
| Firing alerts | Grafana dashboard ("Firing alerts" table), or `scripts/prod-compose.sh exec -T alertmanager amtool --alertmanager.url=http://localhost:9093 alert query` |
| Silence an alert | Grafana **Alerting → Silences** (source "Alertmanager"), or `scripts/prod-compose.sh exec -T alertmanager amtool --alertmanager.url=http://localhost:9093 silence add alertname=JobsFailing --duration=2h --author=<you> --comment="<why>"` |
| Restart the app | `scripts/prod-compose.sh up -d` (recreates only what changed) or `scripts/prod-compose.sh restart api worker` |

Phone numbers and message bodies are encrypted in the database. Nothing below needs them. Do not decrypt user data to
debug an incident.

## Kill switches

Two flags in `system_flags` stop spending without stopping the service. Use them before stopping containers.

| Flag | Off means | Users see | Still works |
|---|---|---|---|
| `ai_enabled` | No model calls | SMS: one "SMS AI is busy right now…" per user per day, credit refunded. USSD: the same text, session ends. | Grounding-store answers, commands (`HELP`, `MORE`, `STOP`, …) |
| `sms_outbound_enabled` | No MT SMS is sent | SMS users get no reply. USSD: answers too slow for the screen and "Save to SMS" get "SMS answers are paused…". | USSD answers shown inline on the screen |

```bash
scripts/prod-compose.sh exec -T api /app/admin flag ai_enabled false
scripts/prod-compose.sh exec -T api /app/admin flag sms_outbound_enabled false
# or over HTTP (takes effect in the API immediately):
curl -fsS -X POST -H "Authorization: Bearer $ADMIN_API_TOKEN" -d '{"ai_enabled": false}' https://$DOMAIN/admin/flags
```

- Changes take effect within 3 seconds; each process caches flags that long. The kill-switch change is logged at WARN.
- With only `sms_outbound_enabled=false`, SMS questions are still answered by the model (AI spend continues), but the
  replies are marked `blocked_disabled` and **not sent later**. To stop all spend, turn off both.
- Turn a flag back on with `true`. Nothing queued while it was off is replayed.
- Last resort: `scripts/prod-compose.sh stop api worker`. Caddy then answers webhooks with 502 and users get nothing.

## Spend cap hit

Alerts: `DailySpendAbove80Percent` (warning), `DailySpendCapReached` (critical).

The worker estimates spend today as SMS segments × `SMS_SEGMENT_COST_USD` plus model tokens priced with `LLM_PRICES`.
At `GLOBAL_DAILY_SPEND_CAP_USD`:

- model calls stop: answer jobs end as `ai_unavailable`, and users get the "busy" text once a day;
- outbound SMS that would cross the cap are blocked (`blocked_cap`);
- USSD answers from the grounding store still work.

`GLOBAL_DAILY_SMS_SEGMENT_CAP` blocks SMS the same way. Everything resets at 00:00 UTC.

1. Find where the money went: `scripts/prod-compose.sh exec -T api /app/admin usage`, and the dashboard's "Spend and
   quotas" and "Outbound SMS" rows.
2. Look for abnormal traffic: one carrier or one burst in "Inbound per minute", a high UCS-2 share (UCS-2 costs up to
   3× the segments of GSM-7), many multi-segment answers, or a wrong `LLM_PRICES` entry inflating the estimate. Each
   number is capped at `DAILY_PER_NUMBER_FREE_CAP` questions a day, so a spike means many numbers.
3. Decide:
   - **Expected growth:** raise `GLOBAL_DAILY_SPEND_CAP_USD` (and `GLOBAL_DAILY_SMS_SEGMENT_CAP` if needed) in
     `deployments/.env.prod`, then run `scripts/prod-compose.sh up -d`. This recreates `api` and `worker`, with a few
     seconds of 502s. The dashboards pick up the new cap within a minute.
   - **Abuse or a bug:** leave the cap. Turn off `ai_enabled` if the traffic continues after midnight. Investigate
     before raising anything.
4. Note the cause in the pilot log. The pilot's go/no-go relies on measured unit economics (plan §15 week 6).

## Provider outage

### SMS/USSD provider (mNotify)

Signs: `SMSOutboundUnknownTimeout`, `JobsFailing` with `send_failed`, a rising `retryable` series in "Failed or
blocked sends", or USSD inbound dropping to zero in "Inbound per minute by channel". mNotify is the only provider for both
USSD and SMS, so an mNotify problem stops the whole service.

1. Confirm it is mNotify. Check the worker log for send errors
   (`scripts/prod-compose.sh logs --since 15m worker | grep -i mnotify`). The log line and `send_attempts.error` carry
   mNotify's own words (digit runs masked, never the API key). How the adapter sorts failures:
   - *Retried* (nothing was accepted; points at our account or configuration, not an outage): HTTP 401, 403, 404, 405,
     407, 408 and 429, any redirect, and messages about the API key. 401/403 means the key was rotated or revoked or the
     account is suspended, 404 or a redirect means a wrong `MNOTIFY_BASE_URL`, 429 a rate limit.
   - *Failed for good* (`send` / `failed`, never retried): the sender ID is not registered or approved
     (`admin mnotify-sender-status`), the balance is too low (top up; `admin mnotify-status -check-credentials` shows it),
     or the number was rejected. Messages that failed this way are not resent afterwards.
   - *Unknown outcome* (`unknown_timeout`): timeouts, dropped connections, `5xx`, and `2xx` answers the adapter cannot read.
     See [unknown-timeout sends](#unknown-timeout-sends).

   `scripts/prod-compose.sh exec api /app/admin mnotify-status -check-credentials` separates a key or account problem from an
   outage with one call. The OpenAPI file documents no error codes, so a new kind of error text means tightening the
   classification in `internal/channel/mnotify/sms.go`: tell the developers. This runbook knows no public status page for
   mNotify: ask support@mnotify.com or +233 54 150 9394, and check whether `/healthz` and the other dashboards look normal.
2. If sends keep failing, turn off `sms_outbound_enabled` to stop retries and segment reservations. The affected
   replies are not resent later. USSD inline answers keep working if USSD ingress is up.
3. If USSD ingress is down, nothing reaches us and there is nothing to switch on our side. Tell the pilot users through
   your agreed channel. If USSD traffic stopped right after a token rotation, a redeploy to another host or a change at
   mNotify, see [webhook rejections](#webhook-rejections) instead. mNotify's callback URL is registered by mNotify (dashboard
   or support; there is no API for it), so a lost or changed registration is fixed there.
4. When mNotify recovers, turn `sms_outbound_enabled` back on and watch "Submitted per hour" recover. Then handle any
   [unknown-timeout sends](#unknown-timeout-sends). With no delivery reports, "submitted" means mNotify accepted the message,
   not that a phone got it: for a doubtful stretch compare the mNotify dashboard's delivery report with the `submitted` rows.

### MTN provider (dormant)

Not in use: the owner chose mNotify on 2026-10-18 (see [PROVIDER_VALIDATION.md](PROVIDER_VALIDATION.md)). The steps below
apply only if `PRIMARY_*_PROVIDER=mtn` is ever set again.


Signs: `SMSOutboundUnknownTimeout`, `JobsFailing` with `send_failed`, a rising `retryable` series in "Failed or
blocked sends", or USSD inbound dropping to zero in "Inbound per minute by channel". MTN is the only provider for both
USSD and SMS, and only for MTN subscribers, so an MTN problem stops the whole service.

1. Confirm it is MTN. Check the worker log for send errors
   (`scripts/prod-compose.sh logs --since 15m worker | grep -i mtn`). The adapter retries HTTP 401, 402, 403, 404 and 429
   (and a failed token request), but those point at our account or configuration, not an outage: 401 means the
   `MTN_CLIENT_ID`/`MTN_CLIENT_SECRET` pair or a revoked app (the log line says "token request answered HTTP 401" when the
   token endpoint itself refused), 403 an unapproved service code, sender ID or app (MTN's canonical code `6000`/`6001`
   means billing or balance), 404 a wrong `MTN_BASE_URL`, 429 a rate limit (`1013`/`3006`). `5xx` answers are logged as
   uncertain, not retried: see [unknown-timeout sends](#unknown-timeout-sends). `scripts/prod-compose.sh exec api
   /app/admin mtn-status -check-credentials` separates a credential problem from an outage with one token request. Then
   check MTN's status page (<https://madapi.statuspage.io/>, linked from developers.mtn.com "Getting help") and whether `/healthz` and the other
   dashboards look normal. MTN may also suspend developer access without notice (its terms say so): ask MTN.
   The selected products are SMS V2 + OAuth V1 and MTN USSD interface. SMS defaults to OAuth, USSD registration to
   `x-api-key`; `MTN_SMS_SEND_API_KEY` and `MTN_USSD_SEND_BEARER` add the other credential only after MTN/sandbox confirmation.
2. If sends keep failing, turn off `sms_outbound_enabled` to stop retries and segment reservations. The affected
   replies are not resent later. USSD inline answers keep working if USSD ingress is up.
3. If USSD ingress is down, nothing reaches us and there is nothing to switch on our side. Tell the pilot users through
   your agreed channel. If USSD traffic stopped right after a token rotation, a redeploy to another host or a change at
   MTN, see [webhook rejections](#webhook-rejections) instead. If MTN's subscription was lost or removed, re-register
   with `admin mtn-subscribe` (it needs `MTN_CALLBACK_BASE_URL`; MTN cannot list subscriptions, so check the ids in the
   env file with `admin mtn-status`). SMS V2 registers `notifyUrl` per `SMS_OUTBOUND_SENDER_ID`, with
   `MTN_COUNTRY_CODE` in `x-country-code` (default `GH`, format unconfirmed). Keep the original sender when removing it.
4. When MTN recovers, turn `sms_outbound_enabled` back on and watch "Submitted per hour" recover. Then handle any
   [unknown-timeout sends](#unknown-timeout-sends).

### AI provider (DeepInfra)

Signs: `ai_requests_total` with error statuses, `"ai_breaker":"open"` in `/admin/status` or `/readyz`, latency above
the 3.2 s USSD deadline, and more USSD SMS fallbacks.

The AI client already retries, falls back to `LLM_FALLBACK_MODEL` and opens a circuit breaker. While the breaker is
open, users get the "busy" text once a day and their credit back.

1. Check DeepInfra's status page. Check for HTTP 401/402 in the worker log, which mean a key or billing problem.
2. If one model is down but DeepInfra is up, point `LLM_MODEL`/`LLM_FALLBACK_MODEL` at another benchmarked model
   (README "Benchmarking models"), then run `scripts/prod-compose.sh up -d`.
3. If the outage is long, turn `ai_enabled` off so questions get one clear notice instead of retries. Grounding answers
   continue. Turn it back on when DeepInfra recovers.

## AI empty reasoning responses

Alert: `AIEmptyReasoningResponse`. A reasoning model spent its whole token budget on hidden reasoning and returned no
visible text. Plan §7.2 requires this to stay at zero. The user got the fallback model's answer, or the apology.

1. Check `LLM_REASONING_EFFORT=low` and `LLM_MAX_OUTPUT_TOKENS` in the env file. gpt-oss needs at least 350, and the
   client enforces that.
2. Check which `model` label is affected. The reasoning model `openai/gpt-oss-20b` is primary because it fit the 3.2 s
   USSD deadline in 99% of benchmark answers (Llama: 70%; DeepInfra benchmark, 2026-10-17). If it keeps returning
   empty answers, swap `LLM_MODEL` and `LLM_FALLBACK_MODEL` so the non-reasoning
   `meta-llama/Llama-3.3-70B-Instruct-Turbo` answers first, then run `scripts/prod-compose.sh up -d`. Expect more
   USSD answers to fall back to SMS (`ussd_async_sms_fallback_total`), which costs SMS segments.
3. Re-run the benchmark before changing models (README "Benchmarking models"); its exit criterion is `empty = 0`.

## Webhook rejections

Alert: `WebhookRejectedSpike`, labelled with `provider` and `reason`. mNotify has one callback, the USSD session URL
(`/webhooks/ussd/mnotify/session`); it documents no delivery-report or inbound-SMS webhook.

- **`reason="signature"`.** mNotify authenticates nothing, so this means the callback's `?token=` was missing or wrong, or
  the caller is outside `MNOTIFY_USSD_ALLOWED_IPS` when that is set. Either someone is sending forged requests (they are
  rejected, so no harm is done), or the URL mNotify calls no longer matches our token:
  - Did someone just rotate `MNOTIFY_USSD_TOKEN`? Compare the env file with what mNotify was told. `admin mnotify-status`
    shows whether tokens are set but never prints one; the full URL lives only in mNotify's configuration and in your
    password manager. During a rotation both tokens must be listed, new first
    ([DEPLOYMENT.md §6](DEPLOYMENT.md#6-key-and-secret-rotation)). If the registered token is no longer in the env file, ask
    mNotify to register the current URL.
  - Did every callback fail from the first day? mNotify may strip the query string from registered URLs, which this design
    cannot work with ([PROVIDER_VALIDATION.md](PROVIDER_VALIDATION.md), "Not confirmed" item 3). Ask mNotify whether it
    keeps `?token=`; the fix is a code change that moves the token into the path.
  - Are the rejections only from mNotify's addresses? If `MNOTIFY_USSD_ALLOWED_IPS` is set and mNotify changed its
    source addresses, legitimate callbacks fail: empty the setting until you know the new ranges.
  - Is legitimate traffic dropping at the same time? If so, users are affected. Fix the token first.
  - Has the token leaked? Rotate it: the USSD URL lets anyone drive sessions as any phone number.
  - For a flood of forged requests from a few addresses, block them in the cloud firewall.
- **`reason="payload"`.** mNotify sent a body we cannot use: a shared-USSD request without `msisdn`, `sequenceID` and `data`,
  a dedicated one without `msIsdn`, `sessId` and `text`, a body mixing both shapes, a shape that `MNOTIFY_USSD_MODE` pins
  away, or anything posted to `/webhooks/sms/mnotify/*` (nothing is accepted there). Check the API log for `webhook
  rejected` lines. The payloads follow mNotify's OpenAPI file, but several details are unconfirmed (dedicated `text`,
  session ids), so expect differences during the first tests. Compare with
  [PROVIDER_VALIDATION.md](PROVIDER_VALIDATION.md) and the `mnotify_*` fixtures in `tests/fixture_webhooks`.
- **Dormant providers.** The same two reasons apply to the MTN, Hubtel and Arkesel adapters with their own tokens
  (`MTN_USSD_TOKEN`, `MTN_DLR_TOKEN`, ...), if one of them is ever selected again; `admin mtn-status` and
  `admin mtn-subscribe` then take the place of the mNotify steps above.

## Unknown-timeout sends

Alert: `SMSOutboundUnknownTimeout`. The provider did not answer a send in time (or answered in a way that does not say
whether the SMS was accepted), so we do not know whether it went out. The service never resends these automatically, to
avoid double-billing and duplicate texts.

**mNotify sends cannot be looked up by our reference.** The Quick SMS request has no client reference or idempotency field,
and the only handle on a send, the campaign id, comes in the response that was lost. The adapter therefore does not
implement a status lookup (`channel.StatusQuerier`), and the reconciler leaves these attempts `unknown_timeout` until an
operator acts ([PROVIDER_VALIDATION.md](PROVIDER_VALIDATION.md)). A lookup that found nothing would be indistinguishable
from report lag, and a wrong "not found" would resend a billed SMS. The unknown outcomes are timeouts, dropped
connections, every `5xx`, and `2xx` answers that are unreadable or do not say success or failure clearly.

1. Count them and check that the count is not growing:

   ```sql
   SELECT provider_name, date_trunc('hour', created_at) AS hour, count(*)
   FROM send_attempts WHERE status = 'unknown_timeout'
   GROUP BY 1, 2 ORDER BY 2 DESC;
   ```

2. Many at once points to a [provider outage](#smsussd-provider-mnotify). A few spread out are usually slow
   provider responses.
3. **Check the mNotify dashboard report** (log in at <https://app.bms.africa/>, the delivery or sent-messages report for the
   day), because the database cannot say: phone numbers and bodies are encrypted. List the attempts and their times, and
   look for messages in the dashboard around those times:

   ```sql
   SELECT a.id, a.created_at, left(a.error, 160) AS error
   FROM send_attempts a WHERE a.status = 'unknown_timeout'
   ORDER BY a.created_at DESC;
   ```

   A message the dashboard shows was accepted and billed; its row stays `unknown_timeout`, and nothing will update it
   because mNotify sends no delivery reports. A time with nothing in the dashboard (and the error text, which is mNotify's
   own words without the API key) is the evidence for "not sent". Contact support@mnotify.com (+233 54 150 9394) if
   many cluster in one period.
4. Usually do nothing: the user can text again or ask on USSD. There is no admin command yet to mark these resolved or
   resend them.

**Dormant providers.** The MTN adapter has an optional lookup by our client reference (`MTN_SMS_LOOKUP_BY_CLIENT_REF`,
undocumented, off by default); Arkesel and Hubtel have none. The rule is the same: a failed or negative lookup never
permits a resend.
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
