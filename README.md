# SMS AI Ghana

Conversational AI over SMS and USSD for basic phones in Ghana. This repository implements the MVP described in
[`sms_ai_ghana_system_architecture_and_build_plan_v2_0.md`](sms_ai_ghana_system_architecture_and_build_plan_v2_0.md)
(plan v2.0) as a Go modular monolith on PostgreSQL 16.

**Status:** plan weeks 1–4 are built and tested against a signed **mock** SMS/USSD gateway and a mock or real DeepInfra
model. The chosen provider is **mNotify** (mnotify.com), for both USSD and outbound SMS: its Quick SMS API sends the
answers and its shared or dedicated USSD product delivers the sessions. The `mnotify` adapters are written from mNotify's
public OpenAPI file and tested against fakes, and have never run against mNotify because no account or API key exists
yet. mNotify documents no sandbox (every SMS is real and billed), no delivery reports (messages stay `submitted`), no
error responses and no callback authentication (callbacks carry a secret token in the URL); its USSD price, how a short
code and callback URL are provisioned, and whether an individual account suffices are unpublished. The owner picked
mNotify on 2026-10-18 after MTN's own developer platform kept failing (two OAuth 401s) and wanted a registered company
for production. The **MTN**, **Hubtel** and **Arkesel** adapters are kept, tested against fakes and documented, but
**dormant**: nothing selects them by default.
[`docs/PROVIDER_VALIDATION.md`](docs/PROVIDER_VALIDATION.md) lists what each provider's docs confirm and what is still
open (plan §6.1, §17). eSMS Africa and Africa's Talking are not written yet.

## What is built

| Plan item | Where | Notes |
|---|---|---|
| Signed webhooks, raw-body HMAC before parsing (§10.1) | `internal/channel`, `internal/channel/mock`, `internal/httpapi` | Timestamped `sha256=` signature; forged/tampered bodies get 401 |
| Duplicate-webhook idempotency (§5.4) | `messages` unique index, `repository.InsertInbound` | Retries are acknowledged and ignored |
| 2.5 s inbound debounce and multipart coalescing (ADR-006) | `repository.EnqueueAnswer`, `ux_jobs_one_pending_answer` | One pending answer job per conversation; capped extensions |
| Deterministic GSM-7 sanitizer, septet counter, `MORE` pager (ADR-005) | `internal/formatting` | Homoglyph map, extension chars = 2 septets, sentence-aware splitting |
| Stateful `MORE` buffer, zero LLM calls (§6.2) | `conversations.overflow_buffer` (encrypted) | Pages are sized per channel when served |
| Local Ghana Grounding Store (ADR-007) | `internal/grounding`, `data/ghana_grounding_seed.yaml`, `knowledge_snippets` + `pg_trgm` | Direct 0-token answers or "Verified Ghana Fact" prompt injection |
| DeepInfra adapter with reasoning-token guard (ADR-004) | `internal/ai`, `internal/ai/deepinfra` | `reasoning_effort=low` and `max_tokens>=350` for gpt-oss; empty-answer fallback model; circuit breaker |
| Prompt and bounded context, 24 h TTL (§5.3, §7.4) | `internal/ai/prompt`, `repository.RecentTurns` | Token-budgeted history; `NEW` resets |
| Hybrid USSD fast path with MT-SMS fallback (ADR-002) | `internal/service/ussd.go` | Inline answer within `USSD_FAST_PATH_TIMEOUT_MS`, else clean `END` and the worker texts the answer |
| Commands `HELP NEW MORE STOP START CODE DELETE` | `internal/commands`, `internal/service/inbound.go` | Every reply fits one GSM-7 segment (tested) |
| Quotas, voucher pools, spend and segment caps, kill switches (ADR-009) | `usage_daily`, `global_usage_daily`, `sponsored_pools`, `system_flags` | One limit notice and one outage notice per user per day; refunds when no answer is given |
| Exactly-once outbound dispatch (§5.4) | `service.Dispatch`, `send_attempts` | Intent recorded first; timeouts become `unknown_timeout` and are reconciled, never resent blindly |
| Versioned AES-GCM and multi-key HMAC, online rotation (ADR-008) | `internal/security`, `repository.FindOrCreateUser`, `admin rekey` | Old-version users are found and re-keyed lazily |
| PII scrubbing (§10.2) | `internal/policy` | Ghana Card numbers, Luhn-valid card numbers, PIN/OTP/password values |
| Retention and deletion (ADR-010) | `repository.Purge`, `DELETE` command | 30-day messages, expired buffers and sessions |
| Metrics (§13), admin usage API, health checks | `internal/observability`, `/metrics`, `/admin/*`, `/healthz`, `/readyz` | The usage report contains counts only |
| mNotify USSD + MT SMS adapters (Track A, §6.1; the chosen provider) | `internal/channel/mnotify`, `docs/PROVIDER_VALIDATION.md` | Shared and dedicated USSD on one route, detected from the payload shape, with CRLF line breaks in replies and a token-in-URL callback check (plus an optional IP allowlist); Quick SMS sends with the API key scrubbed from every error and log; classified send errors; no status lookup and no delivery reports, so uncertain sends wait for an operator and messages stay `submitted`; `admin mnotify-status` / `mnotify-sender-status`; tests refuse mNotify hosts |
| MTN USSD + MT SMS adapters (Track A, §6.1; dormant: dropped 2026-10-18) | `internal/channel/mtn`, `docs/PROVIDER_VALIDATION.md` | Inline USSD replies; SMS V2 sends with cached OAuth V1 tokens, our message id as `clientCorrelator`, MTN's `requestId` stored for receipts; separate callback tokens and optional IP allowlists; admin callback registration per sender; classified send errors; optional, untested client-reference lookup disabled by default |
| Arkesel USSD + MT SMS adapters (dormant; Track A, §6.1) | `internal/channel/arkesel`, `docs/PROVIDER_VALIDATION.md` | USSD callbacks are unsigned, so they need a shared `?token=`; delivery reports carry Arkesel's `v1=` HMAC; send errors are classified as retryable, uncertain or permanent |
| Hubtel USSD + MT SMS adapters (dormant; Track A, §6.1) | `internal/channel/hubtel`, `docs/PROVIDER_VALIDATION.md` | Hubtel signs nothing, so USSD and delivery-report callbacks each need their own `?token=` (plus an optional IP allowlist); `Release`/`Timeout` end the session; Basic-auth sends with `ClientReference`; send errors are classified as retryable, uncertain or permanent; no sandbox, so `SMS_ENVIRONMENT=live` is required |
| Simulator (§9 `POST /admin/simulate/channel`) | `/simulator` (development only) | SMS thread, USSD screen, live usage |
| Model benchmark (§7.3, §15 week 1) | `cmd/aibench`, `tests/ai_eval/prompts.txt` | Empty-response rate, latency against the USSD deadline, 1-segment fit, cost |

### Deliberate differences from the plan's schema sketch

- `jobs.coalesced_prompt` is replaced by `messages.job_id`. The prompt is rebuilt from the attached (encrypted) messages, so user text is never stored twice.
- `overflow_buffer` is encrypted `bytea` holding the remaining text plus a page budget, so the same buffer can serve 160-character SMS pages or smaller USSD screens.
- Leases live on `jobs` rather than `conversations`. Answer jobs for one conversation still run one at a time.
- Domain types sit in `internal/repository`. Provider adapters live in one package per provider (`internal/channel/mock`, `internal/channel/mnotify`, `internal/channel/mtn`, `internal/channel/arkesel`, `internal/channel/hubtel`) rather than in the plan's separate `sms/` and `ussd/` folders, because one mNotify, MTN, Arkesel or Hubtel account serves both roles.

## Quick start (Docker only: no Go install needed)

The AI model defaults to the real provider (`LLM_PROVIDER=deepinfra`). There is **no default SMS/USSD gateway**:
`PRIMARY_SMS_PROVIDER` and `PRIMARY_USSD_PROVIDER` must name a real provider, and the stack will not start until they do.
`mnotify` (mNotify, mnotify.com) is the chosen provider; `mtn`, `hubtel` and `arkesel` are built too but dormant.

```bash
cp .env.example .env     # then fill in DEEPINFRA_API_KEY and choose the SMS/USSD provider
docker compose -f deployments/compose.yaml up -d --build
```

Postgres is exposed on `localhost:5442` and the API on `localhost:8088`. Verify `LLM_PRICES` against DeepInfra's
current pricing (unknown models are priced conservatively at $0.60/M tokens). Without `ENCRYPTION_KEYS_V*` the
development environment uses built-in **insecure keys**. Validation refuses that outside `APP_ENV=development`.

### Simulators (development and test only)

The mock gateway, the mock model and the simulator page at <http://localhost:8088/simulator> exist so the suite can run
offline, free of charge and without texting real phones. They are enabled only by setting `PRIMARY_SMS_PROVIDER=mock`,
`PRIMARY_USSD_PROVIDER=mock` and/or `LLM_PROVIDER=mock`. Configuration validation rejects them when `APP_ENV` is
`staging` or `production`, and the app refuses to build the mock gateway or model there. Selecting a real SMS/USSD
provider replaces the mock gateway completely: it is not loaded and the simulator page disappears.

### Switching the local stack to the real gateway (mNotify)

Put your mNotify API key in `.env` as `MNOTIFY_API_KEY`, plus `MNOTIFY_USSD_TOKEN` (24+ random characters), then run:

```bash
scripts/use_real_gateway.sh                    # mNotify: asks you to type "live" (there is no sandbox)
scripts/use_real_gateway.sh --provider mtn     # a dormant provider: mnotify (default), mtn, hubtel or arkesel
scripts/use_real_gateway.sh --mock             # back to the offline mock gateway
```

The script refuses until the chosen provider's credentials are set and never prints secrets. mNotify has no sandbox, so
it always selects `SMS_ENVIRONMENT=live`, where every SMS is real and billed. (For the dormant MTN it follows
`MTN_BASE_URL`: the sandbox host selects `sandbox`, any other host `live`.) mNotify can only reach callbacks over public
HTTPS, so USSD needs a deployed host or a tunnel.

The mock model has hooks for demos. Include "slow" in a USSD question to trigger the SMS fallback, "long" to exercise
`MORE`, "empty" to trigger the reasoning-token guard, or "fail" for an upstream error.

### Use mNotify (USSD + MT SMS)

mNotify (mnotify.com, platform bms.africa) is the chosen provider. Read [`docs/PROVIDER_VALIDATION.md`](docs/PROVIDER_VALIDATION.md)
first: it separates what mNotify's public OpenAPI file confirms from what it leaves open (USSD price and short-code
provisioning, how the callback URL is registered, callback source addresses, response deadlines, error responses, and
whether an individual account suffices). **Nothing has run against mNotify yet** and there is **no sandbox**: every SMS
the stack sends is real and billed (a new account has 10 free SMS credits; bundles cost about GHS 0.028-0.031 per SMS).
Set these in `.env`:

- `PRIMARY_SMS_PROVIDER=mnotify` and/or `PRIMARY_USSD_PROVIDER=mnotify`
- `MNOTIFY_API_KEY`: the key from the mNotify dashboard (Settings > Developer). It travels as `?key=` in the URL of every outbound call, so treat it as a password: the app never logs request URLs and scrubs the key from every error, and no admin command prints it
- `SMS_ENVIRONMENT=live`: required for mNotify SMS, because there is nothing to sandbox. The stack refuses to start with mNotify SMS otherwise
- `SMS_OUTBOUND_SENDER_ID`: a sender ID mNotify has approved for your account, 11 characters at most (`admin mnotify-sender-status` asks mNotify whether it is Pending or Approved)
- `USSD_SERVICE_CODE`: the USSD code mNotify provisions for you, shared or dedicated
- `MNOTIFY_USSD_TOKEN`: 24+ random characters, e.g. a `METRICS_TOKEN`-style value from `admin genkey`. mNotify documents no callback authentication, so this token in the callback URL is the only proof a callback came from mNotify; with none set every callback is refused. Comma-separate two values to rotate
- `MNOTIFY_USSD_MODE`: `auto` (default) serves both USSD products on one route, telling them apart by payload shape (shared: `msisdn`/`sequenceID`/`data`; dedicated: `msIsdn`/`sessId`/`text`/`ussdGwId`); `shared` or `dedicated` pins one
- Optional: `MNOTIFY_USSD_ALLOWED_IPS` (mNotify does not publish its addresses), `MNOTIFY_CLIENT_IP_HEADER`, `MNOTIFY_BASE_URL` (default `https://api.mnotify.com/api`)

mNotify cannot reach a laptop: it needs a public HTTPS host or a tunnel. How a callback URL is registered is not
published, so ask mNotify (dashboard or support@mnotify.com, +233 54 150 9394) to point your USSD service at
`https://<host>/webhooks/ussd/mnotify/session?token=<MNOTIFY_USSD_TOKEN>`. There is nothing else to register: mNotify has no
delivery-report or inbound-SMS webhook, so messages stay `submitted` and `/webhooks/sms/mnotify/*` refuses everything.
Make sure the reverse proxy does not log query strings for `/webhooks/ussd/mnotify/*` (the pilot Caddyfile keeps no access log).

The admin commands need no database and print no secret (the key shows only as `set` / `NOT SET`):

```bash
docker compose -f deployments/compose.yaml run --rm api /app/admin mnotify-status                      # configuration only, no network call
docker compose -f deployments/compose.yaml run --rm api /app/admin mnotify-status -check-credentials   # ONE call, GET /balance/sms: proves the key, prints the SMS balance
docker compose -f deployments/compose.yaml run --rm api /app/admin mnotify-sender-status               # ONE call, POST /senderid/status: Pending or Approved
```

An mNotify send has no client reference, so an uncertain one (a timeout after the request left, a `5xx`) cannot be looked
up and is never resent: it waits in `unknown_timeout` until an operator checks the mNotify dashboard report
([`docs/RUNBOOKS.md`](docs/RUNBOOKS.md#unknown-timeout-sends)).

To replay mNotify's documented USSD requests against a local stack (replace `<token>` with your `MNOTIFY_USSD_TOKEN`).
**The examples use `233541509394`, which is mNotify's own support line, so substitute your own number first** or a live
stack may text an answer to them:

```bash
sed 's/233541509394/233XXXXXXXXX/' tests/fixture_webhooks/mnotify_ussd_shared_request_openapi_example.json | curl -X POST "http://localhost:8088/webhooks/ussd/mnotify/session?token=<token>" -H "Content-Type: application/json" --data @-
sed 's/233541509394/233XXXXXXXXX/' tests/fixture_webhooks/mnotify_ussd_dedicated_request_openapi_example.json | curl -X POST "http://localhost:8088/webhooks/ussd/mnotify/session?token=<token>" -H "Content-Type: application/json" --data @-
```

### Use MTN (USSD + MT SMS) (dormant)

Dormant: the owner dropped MTN on 2026-10-18 after two OAuth token requests returned HTTP 401 and its production access
needed a registered company (history in [`docs/PROVIDER_VALIDATION.md`](docs/PROVIDER_VALIDATION.md)). Nothing selects it by default.

Read [`docs/PROVIDER_VALIDATION.md`](docs/PROVIDER_VALIDATION.md) first: it separates what MTN's Swagger files confirm from
the long list of things MTN does not publish (pricing, onboarding, short-code allocation, callback source addresses,
deadlines, the MSISDN format Ghana sends). MTN's developer-portal terms cover the **sandbox only**, so a production
agreement with MTN is a separate step. Set these in `.env`:

- `PRIMARY_SMS_PROVIDER=mtn` and/or `PRIMARY_USSD_PROVIDER=mtn` (MTN subscribers only)
- `MTN_CLIENT_ID` and `MTN_CLIENT_SECRET`: the app's Consumer Key and Secret from developers.mtn.com. OAuth V1 exchanges them as form fields at `/v1/oauth/access_token?grant_type=client_credentials` for the SMS V2 bearer token. `MTN_API_KEY` is the USSD API's `x-api-key`; empty means "use `MTN_CLIENT_ID`", as MTN's FAQ describes
- `MTN_USSD_TOKEN`: 24+ random characters, e.g. a `METRICS_TOKEN`-style value from `admin genkey`. MTN authenticates no callbacks
- `MTN_DLR_TOKEN`: a different 24+ character token for delivery notifications. Without it they are refused and delivery-subscription registration is blocked
- `MTN_BASE_URL` and `SMS_ENVIRONMENT`, which must agree: `https://api.mtn.com` with `live` (every message is real and billed), `https://sandbox.api.mtn.com` with `sandbox`. The stack refuses to start otherwise
- `MTN_CALLBACK_BASE_URL`: the public https origin MTN calls (used by `admin mtn-subscribe`)
- `SMS_OUTBOUND_SENDER_ID`: your registered sender, 11 characters at most; `USSD_SERVICE_CODE` (or `MTN_USSD_SERVICE_CODE`): the dialled code. V2 has no SMS serviceCode or MO callback registration
- `MTN_COUNTRY_CODE=GH`: required `x-country-code` for sender-specific SMS subscription calls; the country-code format is unconfirmed
- `MTN_SMS_SEND_API_KEY=false` and `MTN_USSD_SEND_BEARER=false`: add the extra credential only if MTN/sandbox verification requires it; general MTN auth guidance differs from the product schemas
- `MTN_SMS_LOOKUP_BY_CLIENT_REF=false`: enable only after sandbox proves the requestId lookup accepts our client reference and echoes it; lookup errors never authorize a resend
- Optional: `MTN_USSD_ALLOWED_IPS`, `MTN_DLR_ALLOWED_IPS` (MTN does not publish its addresses), `MTN_CLIENT_IP_HEADER`, `MTN_TOKEN_URL`, `MTN_TARGET_SYSTEM`, `MTN_USSD_INPUT_IS_HISTORY`

Register the callbacks with MTN's API instead of by hand. The commands need no database, print the URLs with the token
redacted, and never print a credential:

```bash
docker compose -f deployments/compose.yaml run --rm api /app/admin mtn-status -check-credentials   # configuration; one OAuth token request proves the key pair
docker compose -f deployments/compose.yaml run --rm api /app/admin mtn-subscribe -dry-run          # show the two registrations without sending them
docker compose -f deployments/compose.yaml run --rm api /app/admin mtn-subscribe                   # POST /v1/messages/ussd/subscription and /v1/messages/sms/outbound/{sender}/subscription
docker compose -f deployments/compose.yaml run --rm api /app/admin mtn-unsubscribe                 # DELETE them again (ids from MTN_USSD_SUBSCRIPTION_ID / MTN_SMS_SUBSCRIPTION_ID)
```

SMS V2 registers `{notifyUrl, targetSystem}` for each sender address; keep the original sender when removing its subscription.
The registered URLs are `https://<host>/webhooks/ussd/mtn/session?token=<MTN_USSD_TOKEN>` and
`https://<host>/webhooks/sms/mtn/status?token=<MTN_DLR_TOKEN>`. MTN has no call that lists subscriptions: copy the ids
`mtn-subscribe` prints into `MTN_USSD_SUBSCRIPTION_ID` and `MTN_SMS_SUBSCRIPTION_ID`. Make sure the reverse proxy does not
log query strings for those paths. Comma-separate two tokens to rotate a URL without downtime.

To replay constructed MTN-shaped USSD requests against a local stack (replace `<token>` with your `MTN_USSD_TOKEN`):

```bash
curl -X POST "http://localhost:8088/webhooks/ussd/mtn/session?token=<token>" -H "Content-Type: application/json" --data @tests/fixture_webhooks/mtn_ussd_begin_constructed.json
curl -X POST "http://localhost:8088/webhooks/ussd/mtn/session?token=<token>" -H "Content-Type: application/json" --data @tests/fixture_webhooks/mtn_ussd_continue_constructed.json
```

### Use Hubtel (USSD + MT SMS)

Dormant: the owner chose MTN's own platform. Read [`docs/PROVIDER_VALIDATION.md`](docs/PROVIDER_VALIDATION.md) first, especially its list of unconfirmed Hubtel details.
Hubtel has **no sandbox**: with `PRIMARY_SMS_PROVIDER=hubtel` every message goes to a real phone and is billed, so keep
Hubtel credentials out of development `.env` files. Set these in `.env`:

- `PRIMARY_SMS_PROVIDER=hubtel` and/or `PRIMARY_USSD_PROVIDER=hubtel`
- `HUBTEL_CLIENT_ID` and `HUBTEL_CLIENT_SECRET`: the SMS API key pair from the Hubtel dashboard
- `HUBTEL_USSD_TOKEN`: 24+ random characters, e.g. a `METRICS_TOKEN`-style value from `admin genkey`. Hubtel does not sign USSD callbacks
- `HUBTEL_DLR_TOKEN`: a different 24+ character token for delivery reports. Without it every report is refused and messages stay `submitted`
- `SMS_OUTBOUND_SENDER_ID`: your registered sender, 11 characters at most
- `SMS_ENVIRONMENT=live`: required, because there is nothing to sandbox. The stack refuses to start with Hubtel SMS otherwise
- Optional: `HUBTEL_USSD_ALLOWED_IPS`, `HUBTEL_DLR_ALLOWED_IPS` (Hubtel does not publish its addresses), `HUBTEL_CLIENT_IP_HEADER`, and `HUBTEL_SMS_BASE_URL` (default `https://sms.hubtel.com`)

Register the **Service Interaction URL** of your Programmable Service as
`https://<host>/webhooks/ussd/hubtel/session?token=<HUBTEL_USSD_TOKEN>`. Ask Hubtel to post delivery reports to
`https://<host>/webhooks/sms/hubtel/status?token=<HUBTEL_DLR_TOKEN>`; the adapter sends no callback URL itself, because
no documented request field for one was found. Make sure the reverse proxy does not log query strings for those paths.
Comma-separate two tokens to rotate a URL without downtime. Hubtel's browser-based simulator
(<https://programmable-simulator.hubtel.com>) stores the URL you give it, so use a throwaway token there.

To replay Hubtel's documented USSD requests against a local stack (replace `<token>` with your `HUBTEL_USSD_TOKEN`):

```bash
curl -X POST "http://localhost:8088/webhooks/ussd/hubtel/session?token=<token>" -H "Content-Type: application/json" --data @tests/fixture_webhooks/hubtel_ussd_initiation.json
curl -X POST "http://localhost:8088/webhooks/ussd/hubtel/session?token=<token>" -H "Content-Type: application/json" --data @tests/fixture_webhooks/hubtel_ussd_response.json
```

### Use Arkesel (USSD + MT SMS)

Dormant: the owner chose not to use Arkesel (nor Hubtel). Read [`docs/PROVIDER_VALIDATION.md`](docs/PROVIDER_VALIDATION.md) first. Then set these in `.env`:

- `PRIMARY_SMS_PROVIDER=arkesel` and/or `PRIMARY_USSD_PROVIDER=arkesel`
- `ARKESEL_API_KEY`
- `ARKESEL_USSD_TOKEN`: 24+ random characters, e.g. a `METRICS_TOKEN`-style value from `admin genkey`
- `SMS_OUTBOUND_SENDER_ID`: your registered sender, 11 characters at most
- `SMS_ENVIRONMENT`: keep `sandbox` (not delivered, not billed) until you mean to send for real, then `live`

Register the USSD endpoint with Arkesel as `https://<host>/webhooks/ussd/arkesel/session?token=<ARKESEL_USSD_TOKEN>`.
Arkesel does not sign USSD callbacks, so make sure the reverse proxy does not log query strings for that path.

For delivery reports, call Arkesel's `POST /api/v2/webhooks/secret/rotate` once. Copy the secret into
`ARKESEL_WEBHOOK_SECRET` and set `ARKESEL_DLR_CALLBACK_URL=https://<host>/webhooks/sms/arkesel/status`, but see
"Not built yet" about the `GET` route.

To replay Arkesel's documented USSD payload against a local stack (replace `<token>` with your `ARKESEL_USSD_TOKEN`):

```bash
curl -X POST "http://localhost:8088/webhooks/ussd/arkesel/session?token=<token>" -H "Content-Type: application/json" --data @tests/fixture_webhooks/arkesel_ussd_new_session.json
```

## Running tests

Unit and integration tests (the integration tests create throwaway databases on the Compose Postgres):

```bash
docker compose -f deployments/compose.yaml --profile test run --rm test
```

The integration suite covers the plan §16 checklist: standard QA, context continuity, multipart coalescing, duplicate
webhooks, homoglyph sanitizing, `MORE` with zero LLM calls, grounding with zero tokens, the reasoning-token fallback,
USSD inline vs. slow fallback, key rotation, voucher redemption after the daily cap, STOP/START, uncertain-send
reconciliation, monotonic delivery receipts, the global segment cap, the AI kill switch, DELETE and PII scrubbing.

## Benchmarking models (plan week 1)

```bash
docker compose -f deployments/compose.yaml --profile test run --rm -e LLM_PROVIDER=deepinfra -e DEEPINFRA_API_KEY=your-key test go run ./cmd/aibench -n 100 -models openai/gpt-oss-20b,meta-llama/Llama-3.3-70B-Instruct-Turbo
```

The week-1 exit criterion is `empty = 0` for reasoning models. Add `-v` to print every answer that needs more than one
SMS and the non-GSM characters the model emitted, which helps with tuning the prompt and the sanitizer.

Results on 2026-10-17 led to `openai/gpt-oss-20b` as the primary model, with Llama 3.3 70B as fallback. Across 400 calls
there were 0 empty answers and 0 errors. With the tightened prompt, gpt-oss-20b's p95 was 1.7 s, every answer beat the
3.2 s USSD deadline, and 78% fit one SMS.

## Operations

`admin` ships in the image:

```bash
docker compose -f deployments/compose.yaml run --rm api /app/admin genkey
```

| Command | Purpose |
|---|---|
| `admin genkey [-version N]` | Fresh encryption/HMAC keys and admin/metrics tokens for `.env` |
| `admin migrate` | Apply migrations (also automatic when `AUTO_MIGRATE=true`, the development default) |
| `admin seed-grounding [-file path]` | Upsert grounding snippets |
| `admin create-pool -code STEMGH -sponsor "..." -max 1000 -grant 100 [-expires 2026-12-31]` | Sponsored voucher pool |
| `admin flag ai_enabled false` | Kill switch (also `sms_outbound_enabled`) |
| `admin rekey` | Re-encrypt everything to `CURRENT_KEY_VERSION` after a rotation |
| `admin usage [-day YYYY-MM-DD]` | Daily usage and spend report |
| `admin send-webhook -url ... -file tests/fixture_webhooks/....json` | POST a fixture signed with `MOCK_WEBHOOK_SECRET` |
| `admin mtn-subscribe [-ussd] [-sms] [-dry-run]` | Register the MTN USSD and SMS delivery callbacks (calls MTN's real API; no database needed) |
| `admin mtn-unsubscribe [-ussd-id ID] [-sms-id ID] [-dry-run]` | Remove them again |
| `admin mtn-status [-check-credentials]` | MTN configuration and callback URLs (token redacted); `-check-credentials` requests one OAuth token |

**Key rotation:** add `ENCRYPTION_KEYS_V2` and `HMAC_KEYS_V2`, set `CURRENT_KEY_VERSION=2` (keep the V1 keys), restart,
run `admin rekey`, then remove the V1 keys.

**Admin HTTP API** (requires `Authorization: Bearer $ADMIN_API_TOKEN`; open in development when unset):
`GET /admin/usage`, `GET /admin/status`, `GET|POST /admin/flags`, `GET|POST /admin/pools`,
`POST /admin/pools/{code}/deactivate`, `GET /admin/grounding/search?q=`, `POST /admin/grounding/reload`.

## Grounding facts need human review

Every entry in `data/ghana_grounding_seed.yaml` is marked `reviewed: false`. Development serves them, but staging and
production only serve reviewed entries. Before the pilot, check each answer against the cited authority and flip
`reviewed: true`. In particular, the plan lists `*400#` for Ghana Card–SIM linking, but the NCA re-registration drive
used `*404#`. Confirm the right code before enabling that snippet.

## Project layout

```text
cmd/api, cmd/worker, cmd/admin, cmd/aibench   entry points (one image)
internal/app            wiring shared by all commands
internal/config         environment configuration and validation
internal/security       versioned AES-GCM, HMAC lookup hashes, webhook signatures
internal/msisdn         E.164 normalisation, network prefix detection
internal/formatting     GSM-7 sanitizer, septet/segment counter, MORE pager
internal/grounding      Local Ghana Grounding Store matcher and seed loader
internal/ai             model contract, fallback + breaker; deepinfra/, mock/, prompt/
internal/policy         PII scrubber
internal/commands       keyword commands and user-facing texts
internal/channel        provider-neutral SMS/USSD contracts; mock/ gateway, mtn/ (chosen), hubtel/ and arkesel/ (dormant) adapters
internal/repository     PostgreSQL (pgx) with at-rest encryption, migrations runner
internal/service        orchestration: inbound SMS, USSD, answers, dispatch, maintenance
internal/worker         job loop with LISTEN/NOTIFY wake-ups
internal/httpapi        routes, webhooks, admin API, simulator page
internal/testenv        integration-test harness
migrations/             SQL schema
data/                   grounding seed (embedded)
deployments/            Dockerfile, compose.yaml, Prometheus config
tests/                  webhook fixtures, AI evaluation prompts
```

## Not built yet

- mNotify follow-ups (the chosen provider): **nothing has been sent to or received from mNotify yet**. The owner still has to
  create the account, supply `MNOTIFY_API_KEY` in `.env`, get a sender ID approved and ask for a USSD code. Then work
  through the numbered "not confirmed" list in [`docs/PROVIDER_VALIDATION.md`](docs/PROVIDER_VALIDATION.md): USSD price
  and provisioning, how the callback URL is registered, callback authentication and source addresses, the response
  deadline and session timeout, whether shared USSD works on all three networks, whether an individual account suffices,
  sender ID approval, what one SMS credit means for long messages, and mNotify's error responses (its OpenAPI file
  documents none, so the send-error classification is a best guess to tighten after the first real failures). There are
  **no delivery reports** (mNotify documents no webhook): messages stay `submitted`; a polling reconciler could use
  `GET /campaign/<campaign id>` but needs a repository query and a worker hook, so it is not built. Uncertain sends wait in
  `unknown_timeout` for an operator, who checks the mNotify dashboard report: a send has no client reference, so
  there is no lookup and no automatic resend. The shared-USSD "session ended" memory is per process.
- MTN follow-ups (dormant: the owner dropped MTN on 2026-10-18; kept for the record): the owner has created an MTN app and the local stack is configured for MTN's sandbox
  (`MTN_BASE_URL=https://sandbox.api.mtn.com`, `SMS_ENVIRONMENT=sandbox`, outgoing SMS switched off), but **the only MTN
  calls made so far were two OAuth token requests, to the production host on 2026-10-17 and to the sandbox host on
  2026-10-18, and both returned HTTP 401** with the cause unresolved. Ask MTN to confirm that OAuth V1 and SMS V2 are active for the app and which sender address is approved, then work
  through the numbered "not confirmed" list in [`docs/PROVIDER_VALIDATION.md`](docs/PROVIDER_VALIDATION.md):
  production access (the portal's terms are sandbox-only), pricing, onboarding and KYC, the USSD code and who provisions it,
  whether MTN keeps the `?token=` query string and from which addresses it calls, the real callback and reply shapes (the
  `inboundResponse`/`outboundResponse` mismatch in the USSD schema), the MSISDN format, sender ID rules, response
  deadlines, country-header format and which credentials each product requires. V2 publishes no notification body;
  confirm requestId vs clientCorrelator receipt matching before a pilot. Telecel and AT users cannot be served by this
  provider. Uncertain sends wait in `unknown_timeout` unless the optional, sandbox-verified client-reference lookup
  confirms submission; a failed lookup never permits automatic resending.
  `End`/`Cancel`/`Timeout` messages reach the engine as the *Exit* entry because `channel.USSDRequest` has no "session
  ended" flag.
- Arkesel follow-ups (dormant): validate against Arkesel's sandbox and get answers to the open questions in
  [`docs/PROVIDER_VALIDATION.md`](docs/PROVIDER_VALIDATION.md). The status webhook accepts both `GET` (Arkesel's
  documented form) and `POST`, but no real signed callback has been received yet. Uncertain Arkesel sends cannot be
  reconciled automatically: Arkesel has no lookup by our reference, so they wait in `unknown_timeout` for an operator.
- Hubtel follow-ups (dormant): nothing has been tried against Hubtel's live or test API, and its SMS API reference could not be
  read (its docs hosts no longer resolve). Confirm the SMS endpoint and error format, the delivery-report callback (how
  it is configured, its payload, whether it keeps the `?token=` query string), the USSD deadline and how a Programmable
  Service is registered, using the numbered list in [`docs/PROVIDER_VALIDATION.md`](docs/PROVIDER_VALIDATION.md).
  Uncertain Hubtel sends cannot be reconciled automatically (no lookup by our `ClientReference` was found), so they wait
  in `unknown_timeout` for an operator. `Release`/`Timeout` requests reach the engine as the *Exit* entry because
  `channel.USSDRequest` has no "session ended" flag; a proper flag would be cleaner.
- Other aggregator adapters: eSMS Africa/Africa's Talking two-way SMS (Track B). Each implements `channel.SMSInbound`,
  `channel.Sender` (ideally `channel.StatusQuerier`) and/or `channel.USSDGateway`, then registers in `internal/app`.
- A secret manager for production. The single-VM pilot stack (TLS reverse proxy, backups and restore drills, Grafana dashboards and alerts) is built: see [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) and [`docs/RUNBOOKS.md`](docs/RUNBOOKS.md).
- Redis for USSD state and rate limits, and the Next.js admin dashboard. Load tests are in [`tests/load/README.md`](tests/load/README.md) and the `-race` suite runs with `docker compose -f deployments/compose.yaml --profile test-race run --rm test-race`.
