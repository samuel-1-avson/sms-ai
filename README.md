# SMS AI Ghana

Conversational AI over plain SMS for basic phones in Ghana: people text a phone number and get the answer by text,
with no mobile data. It implements the MVP in
[`sms_ai_ghana_system_architecture_and_build_plan_v2_0.md`](sms_ai_ghana_system_architecture_and_build_plan_v2_0.md)
(plan v2.0) as a TypeScript monorepo on PostgreSQL 16.

**How SMS gets in and out.** Two-way SMS from an aggregator (a number that both receives and replies) is hard to get in
Ghana, so this branch uses **an Android phone as the gateway**: people text the phone's SIM; the
[gateway app](apps/gateway/README.md) forwards each text to the server over the internet and sends the AI's reply from
the same SIM. Only the gateway phone needs data.

**Status.** The server is ported from the original Go service (still on `main`) as a lean core for SMS: everything the
plan's §16 checklist covers except USSD. Its unit and integration tests pass against PostgreSQL, and the phone protocol
is tested end to end over HTTP with separate API and worker processes. The Android app type-checks, bundles and passes
`expo-doctor`, but **the native Kotlin module has not been compiled or run on a phone yet** (no Android toolchain was
available); build it with EAS and follow the checks in [apps/gateway/README.md](apps/gateway/README.md).

## Layout

```text
apps/server/       Node 22 + TypeScript: API, worker and admin CLI (Hono, postgres.js, prom-client, pino)
apps/gateway/      Expo (React Native) Android app + local Kotlin module that does the SMS work
packages/shared/   The phone <-> server protocol: zod schemas and HMAC request signing, used by both sides
migrations/        SQL schema (0001 from the Go service, 0002 for the gateway)
data/              Ghana grounding seed (curated facts answered without the model)
deployments/       Docker image, compose files (local + single-VM pilot), Caddy, Prometheus, Grafana
docs/              DEPLOYMENT.md (pilot VM), RUNBOOKS.md (incidents), GROUNDING_REVIEW.md
```

## How a text is answered

1. The gateway phone receives an SMS, saves it locally and `POST`s it to `/gateway/v1/inbound` (HMAC-signed).
2. The API stores it encrypted, runs keyword commands (`HELP NEW MORE STOP START CODE DELETE`) at once, or queues a
   debounced answer job. Texts arriving within ~2.5 s are merged into one question.
3. The worker answers from the **Local Ghana Grounding Store** (zero tokens) when a curated fact matches, otherwise asks
   the model (DeepInfra `gpt-oss-20b`, Llama 3.3 70B as fallback) with recent context.
4. The answer is sanitized to GSM-7 and cut to one SMS. The rest waits behind `MORE`, which costs no extra model call.
5. The reply is queued in `gateway_outbox`. The phone, long-polling `/gateway/v1/outbox`, collects it within a second,
   saves and acknowledges it, sends it from the SIM and reports `sent` / `delivered`.

## What is built

| Feature | Where |
|---|---|
| Phone gateway protocol: signed inbound, long-poll outbox, ack, status, heartbeat | `apps/server/src/http/server.ts`, `apps/server/src/channel/gateway.ts`, `packages/shared` |
| Exactly-once replies: phone-side dedup by message id, re-offer of unacknowledged rows, never-resend of unconfirmed ones | `gateway_outbox`, `GatewayDb.kt` |
| Offline phone handling: replies given up after `GATEWAY_OUTBOX_TTL_SECONDS` with segments refunded; stale and offline alerts | `Engine.gatewayMaintenance`, `deployments/monitoring/alerts.yml` |
| Spam and loop guard: texts from names, short codes and the gateway's own number are never answered | `GatewayChannel.parseInbound` |
| Raw-body HMAC webhook signatures, timestamp tolerance, secret rotation | `packages/shared/src/signing.ts` |
| Duplicate-delivery idempotency, 2.5 s debounce and multipart coalescing | `messages` unique index, `Q.enqueueAnswer` |
| GSM-7 sanitizer (homoglyph map), septet/segment counter, `MORE` pager | `apps/server/src/formatting` |
| Grounding store with direct answers and "Verified Ghana Fact" prompt injection | `apps/server/src/grounding.ts`, `data/` |
| DeepInfra client: reasoning-token guard, fallback model, circuit breaker, cost ledger | `apps/server/src/ai` |
| Quotas, voucher pools, global segment and spend caps, kill switches | `usage_daily`, `global_usage_daily`, `sponsored_pools`, `system_flags` |
| Exactly-once dispatch with recorded intent; uncertain sends reconciled, never resent blindly | `Engine.dispatch` |
| Versioned AES-256-GCM encryption and HMAC lookups with online key rotation | `apps/server/src/security`, `admin rekey` |
| PII scrubbing, 30-day retention, `DELETE` | `apps/server/src/policy.ts`, `Q.purge` |
| Metrics, admin API, health checks, browser simulator | `/metrics`, `/admin/*`, `/healthz`, `/readyz`, `/simulator` |

The SQL schema is unchanged from the Go service apart from migration `0002`, and ciphertexts use the same layout
(nonce, ciphertext, tag, with the column's purpose as GCM additional data) and the same lookup-hash scheme. Reading a
database written by the Go service should therefore work, but it has not been tested.

Not carried over from `main`: USSD, the mNotify / MTN / Hubtel / Arkesel adapters, the model benchmark (`cmd/aibench`)
and the Go load test.

## Quick start: browser simulator (no phone)

```bash
cp .env.example .env
# in .env: PRIMARY_SMS_PROVIDER=mock and LLM_PROVIDER=mock (or keep deepinfra and set DEEPINFRA_API_KEY)
docker compose -f deployments/compose.yaml up -d --build
```

Open <http://localhost:8088/simulator>. The mock model has hooks: "slow", "long" (exercises `MORE`), "empty" (the
reasoning-token guard) and "fail".

Without Docker for the app (Postgres still from compose):

```bash
corepack enable && pnpm install
docker compose -f deployments/compose.yaml up -d postgres
cd apps/server
PRIMARY_SMS_PROVIDER=mock LLM_PROVIDER=mock EMBEDDED_WORKER=true HTTP_ADDR=:8088 pnpm dev
```

## With a real phone

1. Generate secrets: `make genkey` (or `pnpm --filter @smsai/server admin genkey`). Put the keys and
   `GATEWAY_DEVICE_SECRET` in `.env`, with `PRIMARY_SMS_PROVIDER=gateway`, `GATEWAY_PHONE_NUMBER` (the gateway SIM's
   number) and `DEEPINFRA_API_KEY`.
2. Start the stack and a public HTTPS tunnel: `make up` then `make tunnel`, which prints a
   `https://….trycloudflare.com` URL. Set `GATEWAY_PUBLIC_URL` to it and `make up` again.
3. Build and install the app ([apps/gateway/README.md](apps/gateway/README.md)), then `make pair` and scan the QR code.
4. Text the gateway number from another phone.

For the pilot, run the same stack on a VM with TLS, backups and monitoring: [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md).

## Tests

```bash
make test                                  # in Docker: shared + server unit and integration tests
```

Or on the host, against the compose Postgres:

```bash
SMSAI_TEST_DATABASE_URL=postgres://smsai:smsai_dev_password@localhost:5442/postgres pnpm test
```

Integration tests are skipped when `SMSAI_TEST_DATABASE_URL` is unset. They cover the §16 checklist minus USSD (context
continuity, coalescing, duplicate webhooks, homoglyphs, `MORE` with zero model calls, grounding with zero tokens, the
reasoning-token fallback, key rotation, vouchers, STOP/START, uncertain-send reconciliation, monotonic receipts, the
segment cap, the AI kill switch, DELETE, PII), plus the gateway protocol: the full inbound → reply → ack → delivered
loop, signatures and secret rotation, duplicate batches, the spam/loop guard, long-poll wake-up, TTL expiry with refund
and stale acknowledgements. The Kotlin signer has a unit test with the same vector as the TypeScript one.

Type-check everything with `pnpm typecheck`; the app also has `pnpm --filter @smsai/gateway doctor`.

## Operations

| Command (`node dist/admin.js …` in the image, `pnpm --filter @smsai/server admin …` on the host) | Purpose |
|---|---|
| `genkey [--version N]` | Fresh encryption/HMAC keys, admin/metrics tokens and a gateway device secret |
| `gateway-pair [--url https://host]` | QR code that pairs the gateway phone (contains the device secret) |
| `gateway-status` | The phone's last heartbeat and the outbox counts |
| `migrate` | Apply migrations (automatic when `AUTO_MIGRATE=true`, the development default) |
| `seed-grounding [--file path]` | Upsert grounding snippets |
| `create-pool --code STEMGH --sponsor "…" --max 1000 --grant 100 [--expires 2026-12-31]` | Sponsored voucher pool |
| `flag ai_enabled false` | Kill switch (also `sms_outbound_enabled`) |
| `rekey [--batch N]` | Re-encrypt everything to `CURRENT_KEY_VERSION` after a rotation |
| `usage [--day YYYY-MM-DD]` | Daily usage and spend report |
| `send-webhook --url … --file tests/fixture_webhooks/mock_sms_inbound.json` | POST a fixture signed with `MOCK_WEBHOOK_SECRET` |

**Admin HTTP API** (`Authorization: Bearer $ADMIN_API_TOKEN`; open in development when unset): `GET /admin/usage`,
`GET /admin/status` (includes the gateway phone's health), `GET|POST /admin/flags`, `GET|POST /admin/pools`,
`POST /admin/pools/{code}/deactivate`, `GET /admin/grounding/search?q=`, `POST /admin/grounding/reload`.

**Key rotation:** add `ENCRYPTION_KEYS_V2` and `HMAC_KEYS_V2`, set `CURRENT_KEY_VERSION=2` (keep V1), restart, run
`admin rekey`, then remove V1. **Device secret rotation:** set `GATEWAY_DEVICE_SECRET=new,old`, re-pair the phone, then
drop the old value.

## Grounding facts need human review

Every entry in `data/ghana_grounding_seed.yaml` is `reviewed: false`. Development serves them, but staging and
production only serve reviewed entries. Check each answer against its cited authority and flip `reviewed: true` before
the pilot ([docs/GROUNDING_REVIEW.md](docs/GROUNDING_REVIEW.md)).

## Not built yet

- Running the Android module on real hardware: build with EAS, then work through the checks in the gateway README
  (dual-SIM receive, delivery reports on each network, behaviour with the screen off overnight, the SMS rate limit).
- More than one gateway phone (load sharing and failover).
- The model benchmark and load test from the Go service, and Grafana panels for the gateway (the pilot dashboard still
  has its USSD panels, which now stay empty).
- A secret manager for production; Redis for rate limits; an admin dashboard.
