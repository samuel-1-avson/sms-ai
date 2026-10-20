# Pilot deployment (single VM)

This is the plan's phase **P1 Ghana Pilot** (§12.2): one cloud VM runs everything for 100–1,000 questions a day. It
covers weeks 5–6 of §15: live pilot hosting, backups and restore drills, monitoring, and key rotation. Day-to-day
incidents are in [RUNBOOKS.md](RUNBOOKS.md).

## 1. What runs

`deployments/compose.prod.yaml` starts these containers from one env file, `deployments/.env.prod`:

| Service | Image | Role | Reachable from |
|---|---|---|---|
| `caddy` | `caddy:2.10.2-alpine` | TLS with automatic Let's Encrypt certificates, routing, IP allowlist | Internet: **80 and 443 only** |
| `api` | `smsai-ghana:<tag>` (built on the VM) | The phone gateway protocol, admin API, `/metrics` | Caddy |
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
| `/gateway/*` | api | Anyone. Every request must carry the device secret's HMAC signature (`GATEWAY_DEVICE_SECRET`). |
| `/healthz` | api | Anyone (for an external uptime monitor) |
| `/admin/*`, `/metrics`, `/readyz` | api | `OPS_ALLOWED_CIDRS` only (others get 403), and then the bearer tokens |
| `/grafana/*` | grafana | `OPS_ALLOWED_CIDRS` only, and then the Grafana login |
| anything else | 404 | The development simulator is never exposed |

Networks: `backend` and `ops` are Docker `internal` networks with no route to the internet. Postgres, Prometheus,
Grafana and node-exporter sit only on those. `api`, `worker` (DeepInfra), `caddy` (ACME) and `alertmanager`
(notifications) also join `public`, which has egress.

Caddy writes no access log; the API logs method, path, status and latency itself, never bodies or phone numbers.

**SMS goes through an Android phone, not an aggregator.** People text the gateway phone's SIM. The app on that phone
(`apps/gateway`, see its README) forwards each text to `/gateway/v1/inbound`, long-polls `/gateway/v1/outbox` for the
replies and sends them from the same SIM. The phone needs power, signal, mobile data or Wi-Fi, and airtime or an SMS
bundle. Every reply costs what an SMS costs on that SIM, so keep `GLOBAL_DAILY_SMS_SEGMENT_CAP` and
`SMS_SEGMENT_COST_USD` realistic, and keep the volume modest: networks may flag a personal SIM that sends a lot of
automated SMS.

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

- `CURRENT_KEY_VERSION`, `ENCRYPTION_KEYS_V1`, `HMAC_KEYS_V1`, `ADMIN_API_TOKEN`, `METRICS_TOKEN`,
  `MOCK_WEBHOOK_SECRET` and `GATEWAY_DEVICE_SECRET`, from `admin genkey`. That runs offline in a throwaway container:
  `docker run --rm --network none smsai-ghana:pilot node dist/admin.js genkey`.
- `POSTGRES_PASSWORD` and `GRAFANA_ADMIN_PASSWORD`, as random hex.

Then edit every line marked `EDIT`:

| Setting | Value |
|---|---|
| `DOMAIN`, `ACME_EMAIL` | Your hostname and a monitored mailbox |
| `OPS_ALLOWED_CIDRS` | Space-separated public IPs/CIDRs of the operators (`curl -4 ifconfig.me` from each). To reach Grafana through an SSH tunnel instead, see §9. |
| `ALERT_SLACK_WEBHOOK_URL` | A Slack-compatible incoming webhook (§8) |
| `GATEWAY_PHONE_NUMBER` | The gateway SIM's own number, so texts from it are never answered |
| `GATEWAY_PUBLIC_URL` | `https://<DOMAIN>`: the address printed in the pairing QR code |
| `DEEPINFRA_API_KEY`, `LLM_PRICES` | From DeepInfra; check current prices |

Production refuses mock providers, placeholder secrets (`REPLACE_…`, `CHANGE_ME…`, `SET_AFTER…`), insecure keys and
short tokens (`apps/server/src/config.ts`, `validate`). With anything wrong, `api` and `worker` exit with a list of
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
scripts/prod-compose.sh exec api node dist/admin.js seed-grounding
scripts/prod-compose.sh restart api worker      # both processes reload the store
```

**Pair the phone.** Install the gateway app on the phone (apps/gateway/README.md), then print the pairing code on the VM
and scan it in the app:

```bash
scripts/prod-compose.sh exec api node dist/admin.js gateway-pair      # QR code with https://<DOMAIN> and the device secret
```

The QR code contains the device secret: show it only on your own screen. For the first live check, text the gateway SIM
from a team phone and watch the app's **Recent activity**, then `/admin/status` → `gateway` (the phone's last heartbeat
and the outbox counts). Pause replies in the app, or turn `sms_outbound_enabled` off, while testing anything you do not
want sent.

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

`migrate` applies new migrations before `api` and `worker` restart. The phone gets errors for the few seconds the API
is down and retries with backoff; it keeps every text it receives meanwhile, and duplicate deliveries are ignored.

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
   `docker run --rm --network none smsai-ghana:pilot node dist/admin.js genkey --version 2`.
2. In `deployments/.env.prod`, **add** `ENCRYPTION_KEYS_V2` and `HMAC_KEYS_V2`, set `CURRENT_KEY_VERSION=2`, and
   **keep** the V1 lines. Ignore the token lines in the output unless you are rotating those too. Save the V2 keys in
   the password manager.
3. `scripts/prod-compose.sh up -d`. Compose recreates `api` and `worker`. New writes use V2, and users with V1 lookups
   are still found and re-keyed lazily.
4. Re-encrypt everything now: `scripts/prod-compose.sh exec api node dist/admin.js rekey`. It prints the rows it re-encrypted.
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
| `GATEWAY_DEVICE_SECRET` | Generate the new value (`openssl rand -hex 24`) and set `new,old` (**new first**; both are accepted), `up -d`. Run `admin gateway-pair` (it encodes the first value), unpair and re-pair the phone, check that its heartbeats arrive, then set `new` only and `up -d` |
| `DEEPINFRA_API_KEY` | Create the new key at DeepInfra, update the env file, `up -d`, then revoke the old key |
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
| `GatewayPhoneOffline` | No heartbeat from the gateway phone for 5 min | critical |
| `GatewayRepliesExpired` | Any reply given up because the phone never collected it, in 15 min | warning |
| `GatewayRepliesUnconfirmed` | Any reply the phone took but never reported on, in 15 min | warning |

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

This checks the whole stack on a workstation without public DNS, real certificates or a phone.

```bash
scripts/init_env_prod.sh /tmp/smsai-dryrun.env
```

Then set these in that file:

```text
APP_ENV=staging                  # staging allows the mock gateway and model
PRIMARY_SMS_PROVIDER=mock
LLM_PROVIDER=mock
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
