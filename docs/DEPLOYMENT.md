# Pilot deployment (single VM)

This is the plan's phase **P1 Ghana Pilot** (§12.2): one cloud VM runs everything for 100–1,000 questions a day. It
covers weeks 5–6 of §15: live pilot hosting, backups and restore drills, monitoring, and key rotation. Day-to-day
incidents are in [RUNBOOKS.md](RUNBOOKS.md).

## 1. What runs

`deployments/compose.prod.yaml` starts these containers from one env file, `deployments/.env.prod`:

| Service | Image | Role | Reachable from |
|---|---|---|---|
| `caddy` | `caddy:2.10.2-alpine` | TLS with automatic Let's Encrypt certificates, routing, IP allowlist | Internet: **80 and 443 only** |
| `api` | `smsai-ghana:<tag>` (built on the VM) | Webhooks, admin API, `/metrics` | Caddy |
| `worker` | same image | Jobs, SMS dispatch, maintenance, `/metrics` on :9091 | Prometheus |
| `migrate` | same image | Runs `admin migrate` and exits; `api` and `worker` wait for it | — |
| `postgres` | `postgres:16-alpine` | Database (`pgdata` volume) | Internal only |
| `backup` | `postgres:16-alpine` | Nightly `pg_dump` with retention (`deployments/backup/backup.sh`) | Internal only |
| `prometheus` | `prom/prometheus:v3.5.0` | Scrapes api, worker and node-exporter; evaluates `deployments/monitoring/alerts.yml` | Internal only |
| `alertmanager` | `prom/alertmanager:v0.28.1` | Sends alert notifications | Internal only |
| `grafana` | `grafana/grafana:12.1.1` | Dashboard "SMS AI Ghana: pilot" at `https://<DOMAIN>/grafana/` | Through Caddy, allowlisted IPs |
| `node-exporter` | `prom/node-exporter:v1.9.1` | VM disk, memory and CPU, plus backup timestamps | Internal only |

Caddy routes:

| Path | Goes to | Who can reach it |
|---|---|---|
| `/webhooks/*` | api | Anyone. The app checks each callback's token (mNotify authenticates nothing). Caddy overwrites `X-Real-IP` with the caller's address. |
| `/healthz` | api | Anyone (for an external uptime monitor) |
| `/admin/*`, `/metrics`, `/readyz` | api | `OPS_ALLOWED_CIDRS` only (others get 403), and then the bearer tokens |
| `/grafana/*` | grafana | `OPS_ALLOWED_CIDRS` only, and then the Grafana login |
| anything else | 404 | The development simulator is never exposed |

Networks: `backend` and `ops` are Docker `internal` networks with no route to the internet. Postgres, Prometheus,
Grafana and node-exporter sit only on those. `api`, `worker` (DeepInfra, mNotify), `caddy` (ACME) and `alertmanager`
(notifications) also join `public`, which has egress.

Caddy writes no access log on purpose: mNotify's USSD callback token travels in the query string
([PROVIDER_VALIDATION.md](PROVIDER_VALIDATION.md)). The API logs method, path, status and latency without query
strings. mNotify's API key is also in a query string, but only on outbound calls (`https://api.mnotify.com/api/...?key=`)
that never pass through Caddy; the app never logs those URLs either.

The SMS/USSD provider is **mNotify** (mnotify.com, API host `api.mnotify.com`): its **Quick SMS** API sends the answers and
its **shared or dedicated USSD** product delivers the sessions, so one account and one API key serve both. The owner
chose it on 2026-10-18 after MTN's own developer platform failed (two OAuth 401s, and production access that needs a
registered company). The MTN, Hubtel and Arkesel adapters are still in the code but dormant, and nothing in this guide
uses them.

**Read the provider caveats before planning a launch date.** The adapter is built from mNotify's public OpenAPI file and
has never run against mNotify: there is no sandbox, the file documents no error responses, and the USSD price, how a
short code and callback URL are provisioned, callback source addresses, the response deadline and whether an individual
(not a company) can get a USSD code are all unpublished ([PROVIDER_VALIDATION.md](PROVIDER_VALIDATION.md), "Read this first"
and the open list). There are **no delivery reports**: messages stay `submitted`. Start with the free signup credits and
`sms_outbound_enabled=false`, and settle the open questions with mNotify support (support@mnotify.com, +233 54 150 9394)
before real users depend on the service (§4).

## 2. Prepare the VM

**Size.** 2 vCPU, 4 GB RAM and a 50 GB SSD are plenty for the pilot. Ubuntu 24.04 LTS is assumed below. Set the time
zone to UTC. Ghana is UTC+0, and the daily caps reset at 00:00 UTC.

**DNS.** Create an A record for your hostname (`DOMAIN`, e.g. `sms-ai.example.com`) pointing at the VM's public IPv4
address. Caddy needs it before the first start to get a certificate. The stack publishes on IPv4 only, so leave out
an AAAA record. Docker's IPv6 proxy would hide client addresses from the allowlist.

**Firewall.** In the cloud provider's firewall/security group, allow inbound TCP 22 (your IPs only), 80 and 443.
Nothing else. Docker-published ports bypass `ufw`, so the cloud firewall is the control that matters. The stack only
publishes 80 and 443 anyway.

**Base setup.**

```bash
sudo apt update && sudo apt -y upgrade
sudo apt -y install unattended-upgrades git
# Docker Engine + Compose plugin from Docker's apt repository:
#   https://docs.docker.com/engine/install/ubuntu/
sudo usermod -aG docker "$USER"   # log out and back in
docker compose version            # v2.24 or later
```

Use SSH keys only (`PasswordAuthentication no` in `/etc/ssh/sshd_config`). Membership of the `docker` group is
root-equivalent, so only operators belong in it.

**Code.** Put the repository at `/opt/smsai` (`git clone`, or `rsync -a --exclude .env --exclude 'deployments/.env*'`
from a workstation):

```bash
sudo mkdir -p /opt/smsai && sudo chown "$USER" /opt/smsai
# ...copy the code in...
cd /opt/smsai
chmod +x scripts/*.sh        # files copied from Windows lose the executable bit
sudo mkdir -p /var/backups/smsai && sudo chmod 700 /var/backups/smsai
```

## 3. Create the env file and keys

```bash
scripts/init_env_prod.sh
```

This builds the image (`smsai-ghana:pilot`) and writes `deployments/.env.prod` (mode 600) from
[`deployments/env.prod.example`](../deployments/env.prod.example). It fills in these values:

- `CURRENT_KEY_VERSION`, `ENCRYPTION_KEYS_V1`, `HMAC_KEYS_V1`, `ADMIN_API_TOKEN`, `METRICS_TOKEN` and
  `MOCK_WEBHOOK_SECRET`, from `/app/admin genkey`. That runs offline in a throwaway container:
  `docker run --rm --network none smsai-ghana:pilot /app/admin genkey`.
- `POSTGRES_PASSWORD` and `GRAFANA_ADMIN_PASSWORD`, as random hex.
- `MNOTIFY_USSD_TOKEN` (48 hex characters), the shared secret in the USSD callback URL you give mNotify.

Then edit every line marked `EDIT`:

| Setting | Value |
|---|---|
| `DOMAIN`, `ACME_EMAIL` | Your hostname and a monitored mailbox |
| `OPS_ALLOWED_CIDRS` | Space-separated public IPs/CIDRs of the operators (`curl -4 ifconfig.me` from each). To reach Grafana through an SSH tunnel instead, see §9. |
| `ALERT_SLACK_WEBHOOK_URL` | A Slack-compatible incoming webhook (§8) |
| `MNOTIFY_API_KEY` | The API key from the mNotify dashboard (Settings > Developer, <https://app.bms.africa/settings/developer>). A secret: it is sent as `?key=` on every outbound call and the app never logs it or a request URL |
| `SMS_ENVIRONMENT` | Stays `live`: mNotify documents no sandbox, so validation insists on `live` and every SMS is real and billed |
| `USSD_SERVICE_CODE`, `SMS_SERVICE_ID`, `SMS_OUTBOUND_SENDER_ID` | The USSD code mNotify provisions (shared or dedicated) and a sender ID mNotify has approved for your account (1-11 characters; `admin mnotify-sender-status` shows Pending or Approved) |
| `MNOTIFY_USSD_MODE` | `auto` (the template) serves both USSD payload shapes on one route; `shared` or `dedicated` pins one and refuses the other |
| `MNOTIFY_USSD_ALLOWED_IPS` | Optional. mNotify's callback source addresses are not published; fill it in if they tell you |
| `DEEPINFRA_API_KEY`, `LLM_PRICES` | From DeepInfra; check current prices |

`MNOTIFY_USSD_TOKEN` is already generated; leave it. mNotify documents no callback authentication, so this token in the
callback URL is the only proof that a callback came from mNotify (§4).

**Still needed before the pilot:** an mNotify account and its API key (the free signup includes 10 SMS credits; bundles
cost about GHS 0.028-0.031 per SMS), an approved sender ID, a USSD code and mNotify's quote for it, and an answer to
whether an individual account can get them. The adapter is built from mNotify's public OpenAPI file and **not
confirmed** against any real account; the open questions are listed in
[PROVIDER_VALIDATION.md](PROVIDER_VALIDATION.md#not-confirmed-ask-mnotify-or-verify-with-a-real-account-before-the-pilot).
Settle them with mNotify before real users depend on the service.

Production refuses mock providers, placeholder secrets (`REPLACE_…`, `CHANGE_ME…`, `SET_AFTER…`), insecure keys and
short tokens (`internal/config/config.go`, `Validate`). With anything wrong, `api` and `worker` exit with a list of
problems in `scripts/prod-compose.sh logs api`. Compose itself refuses to start when a deployment secret
(`POSTGRES_PASSWORD`, `GRAFANA_ADMIN_PASSWORD`, `METRICS_TOKEN`, `ALERT_SLACK_WEBHOOK_URL`, `DOMAIN`, …) is empty.

**Back up the keys now.** Copy `ENCRYPTION_KEYS_V1` and `HMAC_KEYS_V1` (and later key versions) into a password
manager or secret store **off the VM**. Phone numbers and message bodies are encrypted with them. Without the keys, the
database and every dump are unreadable.

Every compose command needs the env file. `scripts/prod-compose.sh` adds it, so the rest of this guide uses it:

```bash
scripts/prod-compose.sh <compose arguments>
# = docker compose -f deployments/compose.prod.yaml --env-file deployments/.env.prod <compose arguments>
scripts/prod-compose.sh config --quiet    # validate
```

## 4. First deploy

```bash
scripts/prod-compose.sh up -d --build
scripts/prod-compose.sh ps            # migrate "exited (0)", everything else "running"
scripts/prod-compose.sh logs -f api worker caddy
curl -fsS https://sms-ai.example.com/healthz
```

From an allowlisted IP:

```bash
curl -fsS -H "Authorization: Bearer $ADMIN_API_TOKEN" https://sms-ai.example.com/admin/status
```

The response must show `"env":"production"` and `"insecure_dev_keys":false`. Then open
`https://sms-ai.example.com/grafana/`, log in as `admin` with `GRAFANA_ADMIN_PASSWORD`, and open
**Dashboards → SMS AI Ghana → SMS AI Ghana: pilot**.

**Grounding facts.** Production serves only reviewed snippets ([GROUNDING_REVIEW.md](GROUNDING_REVIEW.md)). After the
review lands in the image:

```bash
scripts/prod-compose.sh exec api /app/admin seed-grounding
scripts/prod-compose.sh restart api worker      # both processes reload the store
```

**Provider callback (mNotify).** mNotify documents no callback authentication, so the URL carries a token. The OpenAPI file
also does not say how a callback URL is registered, so there is no `admin` command for it: ask mNotify (dashboard or
support@mnotify.com, +233 54 150 9394) to point your USSD service at:

| Callback | URL | Registered by |
|---|---|---|
| USSD (shared or dedicated code) | `https://<DOMAIN>/webhooks/ussd/mnotify/session?token=<MNOTIFY_USSD_TOKEN>` | mNotify, on request or in the dashboard (not published) |

Nothing else is registered: mNotify has no delivery-report or inbound-SMS webhook, so `/webhooks/sms/mnotify/*` refuses
everything. One URL serves both USSD products; the payload shape tells the adapter which one it got (`MNOTIFY_USSD_MODE`).
The URL is a secret: anyone holding it can drive sessions as any phone number, and mNotify stores it in full. If mNotify
strips or rewrites the query string, the token check refuses every callback (401, `WebhookRejectedSpike`); the token would
then have to move into the path, which needs a code change ([PROVIDER_VALIDATION.md](PROVIDER_VALIDATION.md), "Not
confirmed" item 3). Check the account from the VM; each command makes at most one call and never prints the key:

```bash
scripts/prod-compose.sh exec api /app/admin mnotify-status                      # configuration only; secrets show as set / NOT SET
scripts/prod-compose.sh exec api /app/admin mnotify-status -check-credentials   # ONE call, GET /balance/sms: proves the key, prints the balance
scripts/prod-compose.sh exec api /app/admin mnotify-sender-status               # ONE call, POST /senderid/status: Pending or Approved
```

**There is no mNotify sandbox.** Every SMS is real and billed (a new account has 10 free credits). For the first live
checks, keep SMS off and test USSD with the team's own phones:

```bash
scripts/prod-compose.sh exec -T api /app/admin flag sms_outbound_enabled false
# ...dial the USSD code from a team phone; inline answers work without SMS...
scripts/prod-compose.sh exec -T api /app/admin flag sms_outbound_enabled true    # when ready to send real SMS
```

Then send one answer by SMS to a team phone and find it in the mNotify dashboard's delivery report. **Delivery reports do
not arrive**, so messages stay `submitted` for good: during the pilot compare the dashboard report with the `submitted`
rows, and watch the credit balance (`mnotify-status -check-credentials`; the API logs a warning below 100 credits and a
zero balance makes sends fail permanently). Also watch `webhook_rejected_total{provider="mnotify"}` (callbacks the parser
cannot read or that carry a wrong token) and messages in `unknown_timeout` (sends whose outcome is unknown: nothing
resends them, see [RUNBOOKS.md](RUNBOOKS.md#unknown-timeout-sends)).

**External uptime check.** Alerts come from this VM, so they stop if the VM dies. Point a free external monitor
(UptimeRobot, Better Stack, Healthchecks) at `https://<DOMAIN>/healthz` every minute.

## 5. Upgrades

```bash
cd /opt/smsai
git pull                     # or rsync the new code
scripts/prod-compose.sh exec -T backup sh /opt/backup/backup.sh once      # fresh dump first
# Edit deployments/.env.prod: SMSAI_IMAGE_TAG=<git commit or date>, so the previous image stays for rollback
scripts/prod-compose.sh up -d --build
scripts/prod-compose.sh ps && scripts/prod-compose.sh logs --tail 50 api worker
```

`migrate` applies new migrations before `api` and `worker` restart. Caddy answers webhooks with 502 for the few
seconds the API is down. Whether mNotify retries a callback is not documented ([PROVIDER_VALIDATION.md](PROVIDER_VALIDATION.md)),
and a USSD session in progress may fail, so upgrade at a quiet hour. Duplicate deliveries are ignored, so
retries are safe.

**Rollback:** set `SMSAI_IMAGE_TAG` back to the previous tag and run `scripts/prod-compose.sh up -d`. Migrations only
go forward. If the new version changed the schema in a way the old code cannot handle, restore the pre-upgrade dump
instead ([RUNBOOKS.md#restore](RUNBOOKS.md#restore)). Remove old images now and then with `docker image ls smsai-ghana`
and `docker image rm`.

Monitoring images (Caddy, Prometheus, Grafana, …) are pinned in `compose.prod.yaml`. Bump them there deliberately,
then `scripts/prod-compose.sh pull && scripts/prod-compose.sh up -d`. `postgres:16-alpine` follows PostgreSQL 16
minor releases, which are drop-in security fixes.

## 6. Key and secret rotation

### Encryption and HMAC keys (plan ADR-008)

Run this when a key may have leaked, when a person with access leaves, or yearly.

1. Generate version 2 offline:
   `docker run --rm --network none smsai-ghana:pilot /app/admin genkey -version 2`.
2. In `deployments/.env.prod`, **add** `ENCRYPTION_KEYS_V2` and `HMAC_KEYS_V2`, set `CURRENT_KEY_VERSION=2`, and
   **keep** the V1 lines. Ignore the token lines in the output unless you are rotating those too. Save the V2 keys in
   the password manager.
3. `scripts/prod-compose.sh up -d`. Compose recreates `api` and `worker`. New writes use V2, and users with V1 lookups
   are still found and re-keyed lazily.
4. Re-encrypt everything now: `scripts/prod-compose.sh exec api /app/admin rekey`. It prints the rows it re-encrypted.
   Run it again; it should report zero.
5. Check `GET /admin/status`, which should show `"key_version":2`. Take a dump and run `scripts/restore_drill.sh`.
6. **Keep the V1 keys while any dump made before step 4 exists**, on the VM or off-site: those dumps need V1. After
   `BACKUP_RETENTION_DAYS` (and your off-site retention) have passed, remove the V1 lines and run
   `scripts/prod-compose.sh up -d`. Keep V1 in the password manager until the old off-site copies are gone too.

### Tokens and passwords

| Secret | How to rotate |
|---|---|
| `ADMIN_API_TOKEN` | New value (`openssl rand -hex 24`) in the env file, then `scripts/prod-compose.sh up -d` |
| `METRICS_TOKEN` | Same. Compose recreates `api`, `worker` and `prometheus` together. |
| `MNOTIFY_USSD_TOKEN` | Generate the new value (`openssl rand -hex 24`) and set `new,old` (**new first**; both are accepted), `up -d`. Ask mNotify (or use the dashboard) to change the registered callback URL to the new token (how is not published). Confirm USSD sessions still work and `webhook_rejected_total{provider="mnotify"}` stays flat, then set `new` only and `up -d`. With both tokens in the list, nothing is lost while mNotify switches |
| `MNOTIFY_API_KEY`, `DEEPINFRA_API_KEY` | Create the new key at the provider, update the env file, `up -d`, then revoke the old key. For mNotify the key is read at process start, so the restart is the switch; a send in flight during the restart can end as `unknown_timeout` (RUNBOOKS), so pick a quiet moment. Rotate the mNotify key at once if it may have leaked (it is in the URL of every outbound call) |
| `POSTGRES_PASSWORD` | `scripts/prod-compose.sh exec postgres psql -U smsai -d smsai -c "ALTER USER smsai PASSWORD '<new hex>'"`, then update the env file and `up -d`. The variable only sets the password when the data volume is first created. |
| `GRAFANA_ADMIN_PASSWORD` | `scripts/prod-compose.sh exec grafana grafana cli admin reset-admin-password '<new>'`, then update the env file to match |
| `ALERT_SLACK_WEBHOOK_URL` | Update the env file, `scripts/prod-compose.sh up -d alertmanager` |

## 7. Backups

`backup` dumps the database at `BACKUP_HOUR_UTC` (default 02:00) into `BACKUP_DIR` (default `/var/backups/smsai`),
and once at startup when there is no dump from the last 24 hours. Each `smsai-<UTC time>.dump` (pg_dump custom format)
has a `.counts` manifest: every table's row count, read in the same snapshot as the dump. Dumps older than
`BACKUP_RETENTION_DAYS` (default 14) are deleted. A failed dump is retried hourly and raises `BackupFailed`. No
successful dump for 26 hours raises `BackupStale`.

```bash
scripts/prod-compose.sh exec -T backup sh /opt/backup/backup.sh once     # dump now
scripts/restore_drill.sh                                                # prove the newest dump restores
```

**Copy dumps off the VM.** A dump on the same disk does not survive losing the VM. Copy `BACKUP_DIR` daily to
object storage or another machine, for example with `rclone` from the host's crontab:

```cron
# m h dom mon dow  command
30 3 * * *  rclone copy /var/backups/smsai remote:smsai-backups --max-age 48h
```

Dumps contain encrypted phone numbers and messages, plus plaintext metadata such as timestamps, networks and usage
counts. Keep the bucket private. Database retention is 30 days (`RETENTION_DAYS`) plus 14 days of dumps; state the
total in the DPC filing (plan §10.3).

**Weekly restore drill** (plan §15 week 6). Add it to the host's crontab. `RestoreDrillStale` fires when no drill has
passed for 8 days.

```cron
0 4 * * 1  cd /opt/smsai && scripts/restore_drill.sh >>/var/log/smsai-restore-drill.log 2>&1
```

## 8. Alert notifications

Prometheus evaluates `deployments/monitoring/alerts.yml`. Alertmanager groups the alerts and posts them to
`ALERT_SLACK_WEBHOOK_URL`, a Slack-compatible incoming webhook: Slack, Mattermost, Rocket.Chat, or a Discord webhook
URL with `/slack` appended. Critical alerts repeat hourly, warnings every 4 hours, and info alerts daily. Each alert
names its runbook section.

| Alert | Fires when | Severity |
|---|---|---|
| `AIEmptyReasoningResponse` | Any empty reasoning-model response in 15 min (must stay 0) | warning |
| `DailySpendAbove80Percent` / `DailySpendCapReached` | Spend today > 80% / ≥ 100% of `GLOBAL_DAILY_SPEND_CAP_USD` | warning / critical |
| `WebhookRejectedSpike` | > 10 rejected webhooks per provider and reason in 10 min | warning |
| `SMSOutboundUnknownTimeout` | Any SMS send with an unknown outcome in 15 min | warning |
| `JobsFailing` | Any job ending `failed` or `send_failed` in 15 min | warning |
| `SMSAIProcessDown` / `MonitoringTargetDown` | api or worker / node-exporter or Alertmanager unscrapeable | critical / warning |
| `BackupFailed` / `BackupStale` | Last dump failed / no dump for 26 h | warning / critical |
| `RestoreDrillStale` | No passed restore drill for 8 days | info |
| `DiskSpaceLow` / `DiskSpaceCritical` | < 15% / < 5% free on a disk | warning / critical |

The "any events" rules also catch the first event after a restart; `alerts.yml` explains how.

To use **Telegram** instead, replace `slack_configs` in `deployments/monitoring/alertmanager.yml` with:

```yaml
    telegram_configs:
      - bot_token_file: /tmp/slack_webhook_url   # put the bot token in ALERT_SLACK_WEBHOOK_URL
        chat_id: -1001234567890                  # your group's chat id
        send_resolved: true
```

For **email**, use `email_configs` with your SMTP relay (`smarthost`, `from`, `to`, `auth_username`, and
`auth_password_file: /tmp/slack_webhook_url` holding the password). Validate with
`docker run --rm -v "$PWD/deployments/monitoring:/m:ro" --entrypoint amtool prom/alertmanager:v0.28.1 check-config /m/alertmanager.yml`,
then run `scripts/prod-compose.sh up -d --force-recreate alertmanager`.

## 9. Monitoring access

- **Grafana:** `https://<DOMAIN>/grafana/` from an allowlisted IP. The dashboard is provisioned from
  `deployments/monitoring/grafana/dashboards/smsai-pilot.json`. Edit that file, because UI changes are not saved.
  Silence alerts under **Alerting → Silences**, choosing the "Alertmanager" source.
- **Changing the allowlist:** edit `OPS_ALLOWED_CIDRS`, then `scripts/prod-compose.sh up -d caddy`.
- **From the VM itself / SSH tunnel:** connections from the VM reach Caddy from the Docker network gateway. Find it
  with `docker network inspect smsai-prod_public -f '{{(index .IPAM.Config 0).Gateway}}'` and add it as `/32`.
  Then, from your laptop, run `ssh -L 8443:127.0.0.1:443 <vm>`, add `127.0.0.1 <DOMAIN>` to your hosts file, and
  browse `https://<DOMAIN>:8443/grafana/`. Set `GRAFANA_ROOT_URL=https://<DOMAIN>:8443/grafana/` if you only use the
  tunnel.
- **Prometheus and Alertmanager** have no public route. Query them from the VM:
  `scripts/prod-compose.sh exec prometheus wget -qO- 'http://localhost:9090/api/v1/alerts'`.

## 10. Local dry run

This checks the whole stack on a workstation without public DNS, real certificates or provider accounts. It was run
on 2026-10-17 with Docker Desktop.

```bash
scripts/init_env_prod.sh /tmp/smsai-dryrun.env
```

Then set these in that file:

```text
APP_ENV=staging                  # staging allows the mock gateway and model
PRIMARY_SMS_PROVIDER=mock
PRIMARY_USSD_PROVIDER=mock
LLM_PROVIDER=mock
SMS_SERVICE_ID=SIM-SHORTCODE
MOCK_DLR_CALLBACK_URL=http://api:8080/webhooks/sms/mock/status
DOMAIN=localhost
CADDY_GLOBAL_OPTIONS=local_certs   # Caddy's own CA, no ACME
PUBLIC_BIND_ADDR=127.0.0.1
HTTP_PORT=8480
HTTPS_PORT=8443
GRAFANA_ROOT_URL=https://localhost:8443/grafana/
ALERT_SLACK_WEBHOOK_URL=http://127.0.0.1:9/dry-run   # nothing listens; notifications fail locally
BACKUP_DIR=/tmp/smsai-dryrun-backups
OPS_ALLOWED_CIDRS=172.21.0.1/32  # the gateway of smsai-prod_public; check after the first `up`
```

Run it:

```bash
SMSAI_ENV_FILE=/tmp/smsai-dryrun.env scripts/prod-compose.sh up -d
curl -k https://localhost:8443/healthz
SMSAI_ENV_FILE=/tmp/smsai-dryrun.env scripts/restore_drill.sh
SMSAI_ENV_FILE=/tmp/smsai-dryrun.env scripts/prod-compose.sh down -v
```

On Windows Git Bash, use a `C:/...` path for `SMSAI_ENV_FILE` and `BACKUP_DIR`. Docker Desktop's VM disks also show
up in `DiskSpaceLow`. On a Linux VM only the real disks do.
