# Provider validation

What each aggregator's public documentation confirms, what the adapters assume, and what still has to be confirmed
with the provider (plan §6.1, §17) before a pilot. Re-check this file whenever a provider updates its docs.

**Chosen provider (2026-10-18): mNotify** (mnotify.com, on the BMS platform at bms.africa), for both USSD and outbound SMS.
The owner, an individual without a registered company, dropped MTN's own developer platform on 2026-10-18 after two OAuth
token requests returned HTTP 401 and MTN's production access turned out to need company vetting (that history stays in the
MTN section below), and chose mNotify: a Ghanaian bulk-messaging and USSD vendor with a public API and a free account. The
MTN, Arkesel and Hubtel adapters stay in the repository, dormant: they are built, tested and documented below, but nothing
selects them by default. (The choice before this one, made on 2026-10-17, was MTN.)

## mNotify (USSD + MT SMS): the chosen provider

Adapter: `internal/channel/mnotify`. Researched 2026-10-18. The only authority is mNotify's public OpenAPI file,
<https://developer.bms.africa/openapi14.yaml> (rendered at <https://developer.bms.africa/>; `readthedocs.mnotify.com`
redirects there). It was downloaded and read in full. **No mNotify endpoint has been called**: there is no account and no
API key yet, and nothing in this repository sends to mNotify except an operator running `admin mnotify-status
-check-credentials` or `admin mnotify-sender-status` (one call each) or the running service. Everything below is either
taken from that file or marked as an assumption.

**Read this first.**

1. **The file documents success only.** It prints no error body, no error code, no HTTP status semantics, no rate limit,
   no size limit and no timeout. The error classification below is therefore built on plain-language words and on HTTP
   status classes, and is the least certain part of the adapter. The first real calls must be watched (see
   [Not confirmed](#not-confirmed-ask-mnotify-or-verify-with-a-real-account-before-the-pilot)).
2. **The API key travels in the URL** (`?key=...`) of every outbound call. It would show up in any Go error, log line or test
   output that prints a URL, so the adapter never logs request URLs and scrubs the key from every error and response excerpt
   (details below).
3. **A send cannot be looked up afterwards.** The request has no client reference or idempotency field, and the response
   (the campaign id) is the only handle. A send whose answer was lost therefore waits for an operator; it is never resent.
4. **There is no delivery-report webhook.** Messages stay `submitted`.
5. **Callbacks carry no documented authentication**, and the file does not say how a callback URL is registered. The callback
   URL carries a secret token instead (Hubtel and MTN adapters do the same).

### Sources

- The OpenAPI file above (`info.contact`: support@mnotify.com, <https://bms.africa/contact>; terms:
  <https://bms.africa/terms-of-service/>). Tags used: Authentication, SMS, Reports and Stats, USSD. Voice, IVR, templates,
  groups and contacts are not used. API keys come from <https://app.bms.africa/settings/developer>.
- Pricing and support, from the owner's notes on mNotify's public pages (not in the OpenAPI file, not re-verified here): SMS at
  GHS 0.028-0.031 per message in bundles (GHS 50 buys 1,612 SMS, GHS 500 buys 17,857), credits never expire, 10 free SMS on
  signup. Support: support@mnotify.com, +233 54 150 9394. These are well below the `SMS_SEGMENT_COST_USD=0.025` default, so
  the spend caps are conservative; revisit them with real credit usage.

### Confirmed from the OpenAPI file

| Topic | What the file says |
|---|---|
| Authentication | The API key is "included in all API requests ... as a GET parameter": `https://api.mnotify.com/api/template?key=YOUR_API_KEY`. Every sample, GET and POST, does so. |
| Quick SMS | `POST https://api.mnotify.com/api/sms/quick?key=...` with JSON `recipient` (array, e.g. `["0241234567","0201234567"]`), `sender` ("at most 11 characters"), `message`, `is_schedule` (bool), `schedule_date` (`YYYY-MM-DD hh:mm`, `""` when not scheduled). Optional `sms_type`: with `"otp"` "a charge of 0.035 per campaign will be deducted from your main wallet"; the file says not to include it unless the blast is for OTP. |
| Quick SMS response | `{"status":"success","code":"2000","message":"messages sent successfully","summary":{"_id":"A59CCB70-662D-45EF-9976-1EFAD249793D","type":"API QUICK SMS","total_sent":2,"contacts":2,"total_rejected":0,"numbers_sent":["0241234567","0201234567"],"credit_used":2,"credit_left":1483}}`. `_id` "is the id of the message sent ... also called campaign id". `numbers_sent` echoes the numbers as sent (local form). |
| Delivery status (pull only) | `GET /status/<id>` for one message: "id here is not the campaign id but rather the `_id` returned when you query for delivery report". `GET /campaign/<id>/<status>` for a campaign: `id` is the campaign id returned by the send; `status` defaults to all, or `delivered`, `undelivered`, `submitted`, `failed`, `rejected`. `GET /report?from=YYYY-MM-DD&to=YYYY-MM-DD` for a period. `POST /report/summary` (`sender_id`, `from`, `to`, at most 30 days) returns counts per status. Report rows: `_id`, `recipient` (written `233241234567`), `message`, `sender`, `status` (`DELIVERED` in the examples, `SUBMITTED` in the summary), `date_sent`, `campaign_id`, `retries`. **No delivery-report webhook or callback URL is documented for SMS.** |
| Balance | `GET /balance/sms` returns `{"status":"success","balance":1000,"bonus":471}`. |
| Sender IDs | `POST /senderid/register` with `sender_name` (at most 11 characters) and `purpose`; the response summary shows `status: Pending`. `POST /senderid/status` with `sender_name`; the example summary is `{"sender name":"mNotify","status":"Approved"}`. |
| Shared USSD | "Applications can be hosted on a common short code without the extra cost and time of acquiring a dedicated one". mNotify POSTs JSON `{msisdn, sequenceID, data, timestamp}` to your callback URL (`data` is "the request body submitted by the user"; the example is the dialled string `*899*86#`). You reply `{msisdn, sequenceID, message, timestamp, continueFlag}` echoing `msisdn` and `sequenceID`; `message` uses `\r\n` for line breaks; `continueFlag` is `0` to continue and `1` to end. |
| Dedicated USSD | "When users dial your dedicated shortcode ... they interact directly with your USSD app." mNotify POSTs `{shortCode, msIsdn, text, sessId, ussdGwId, language?, imsi?, optional?}`; `ussdGwId` names the operator ("MTN, Vodafone, AirtelTigo"), `sessId` is a "unique USSD session ID". You reply `{responseExitCode: 200, shouldClose, ussdMenu, responseMessage}`; `ussdMenu` uses `\r\n`; `shouldClose` true ends the session. |

### Quirks and contradictions in the file

1. **No error documentation at all** (see "Read this first").
2. **Auth scheme.** `components.securitySchemes.api_key` says "apiKey in header, named api_key", while the description and every
   sample put `key` in the query string. The adapter follows the samples.
3. **Recipient format.** Every example sends local numbers (`0241234567`). The send response echoes them as sent, but the
   report rows show `233...`. The adapter converts our E.164 numbers to the local form the examples use, and sends to Ghana
   numbers only (a non-Ghana number is refused as permanent before any request).
4. **Two different ids.** The send response gives the *campaign* id; `/status/<id>` wants the per-message `_id`, which only
   appears in a delivery report. So the id we store (the campaign id) fits `GET /campaign/<id>`, not `/status/<id>`.
5. **Swapped example values.** In the shared-USSD response example `sequenceID` holds a timestamp-like number
   (`20180801110921`) and `timestamp` holds the request's `sequenceID` (`6789940010`). The adapter echoes the request's
   `sequenceID` and sends an RFC 3339 UTC timestamp like the request's (`2025-09-01T11:22:33Z`).
6. **What `sequenceID` identifies.** The file says it "distinguishes each USSD transaction", which could mean each request or
   each session. Shared mode has no session id and no network field, so the adapter keys the session by phone number and
   only echoes `sequenceID` back, per request. That works under either reading.
7. **Dedicated `text`.** It is "user input during session", with the example `1`. Whether it is the latest entry or an
   accumulated `1*2*x` history, and what the first request of a session holds, is not said; there is no new-session flag.
8. **Space in a key.** The sender-status summary is keyed `"sender name"` (with a space), next to `"status"`.
9. **Trailing slash.** The `senderid/status` samples call `/senderid/status/?key=...`; the documented endpoint has no slash. The
   adapter uses the documented one; redirects are never followed (the key is in the URL), so a redirect would show as an
   error naming the status.
10. **`sms_otp` vs `sms_type`.** The warning prose talks about an `sms_otp` field, the example and charge note about
    `sms_type: "otp"`. The adapter sends neither.
11. **`info.version` is empty** and the file is named `openapi14.yaml`; nothing says how mNotify versions or retires the API.
    Re-fetch the file when planning a release.

### How the adapter uses this

| Concern | Adapter behaviour |
|---|---|
| Selection | `PRIMARY_SMS_PROVIDER=mnotify` and/or `PRIMARY_USSD_PROVIDER=mnotify`. The mock gateway is not loaded alongside; `mock` is accepted only in development and test. Config validation needs `MNOTIFY_API_KEY`, a 1-11 character `SMS_OUTBOUND_SENDER_ID` and **`SMS_ENVIRONMENT=live`** for SMS (mNotify documents no sandbox, so every send is real and billed, as with Hubtel), and `MNOTIFY_USSD_TOKEN` (24+ characters) for USSD. |
| The API key | Sent only as `?key=` on outbound calls. The client never logs a request URL; transport errors are rebuilt without Go's URL echo; every error text and response excerpt is scrubbed of the key, raw and URL-escaped; redirects are not followed, so the key cannot be replayed to another host. Config validation messages never repeat a value. Tests assert that no returned error, wrapped error, log line or admin output contains the key, across refused connections, timeouts, dropped connections, hostile servers that echo the URL, invalid base URLs and the test guard. |
| MT SMS | `POST {MNOTIFY_BASE_URL}/sms/quick?key=...`, JSON `{recipient: ["0541234567"], sender: SMS_OUTBOUND_SENDER_ID, message, is_schedule: false, schedule_date: ""}`: one recipient per call, never `sms_type`. Sender IDs of 1-11 characters only. |
| Provider message id | `summary._id` (the campaign id), or our message id if a success body lacks one. |
| Send error kinds | *No response:* nothing written (no key, refused connection, cancelled context) is Retryable; a request that left and got no answer (timeout, dropped connection, body cut off) is Uncertain. *HTTP 5xx:* Uncertain. *2xx:* `status` success with `code` 2000 (or no code but an `_id`) is accepted; success with `total_rejected` and no `total_sent` is Permanent (the number was refused); success with another code, `total_sent` 0, an unreadable body or no status is Uncertain; an explicit non-success status is classified by its words (below), otherwise Uncertain. *3xx, 4xx:* the words first, then 401/403/404/405/407/408/429 and every redirect are Retryable (credentials, permissions, rate limit, wrong base URL: nothing was accepted), and any other 4xx (including 402) is Permanent. *Words:* "api key", "unauthenticated", "forbidden" and similar are Retryable; "sender" is Permanent (unregistered sender ID); "balance", "insufficient", "credit", "fund" and similar are Permanent (insufficient balance); "recipient", "invalid number", "phone number" are Permanent. Error texts keep mNotify's words, with digit runs of 7 or more masked so a phone number it echoes never reaches a log. All of this is a best guess from the file's silence; see item 9 of the open list. |
| No lookup of uncertain sends | `Provider` does **not** implement `channel.StatusQuerier`, and a test pins that. The only handle on a send is the campaign id in the response, which a lost response does not give us. The report calls take the campaign id, a date range, or the per-message `_id`; none takes our reference. A date-range search by recipient, message text and time would be a heuristic, and a miss (report lag, two identical answers to one number) would be read as "not sent" and resend a billed SMS. Uncertain sends stay `unknown_timeout` for an operator, who checks the mNotify dashboard report (docs/RUNBOOKS.md). |
| Delivery status | None. `ParseStatus` and `ParseInbound` refuse every request (`400`, nothing applied), so `submitted` is final. A future poller can use `GET /campaign/<campaign id>`, which fits what we store; it would need a repository query for old `submitted` rows plus a worker hook, so none is built in this iteration. The first live tests should compare the dashboard's delivery report with the database by hand. |
| Credit warning | When a send's `credit_left` drops below 100 the adapter logs a warning (at most once per 10 minutes). At zero, sends are rejected and the dispatcher treats an insufficient-balance rejection as permanent: the answer SMS is lost (the user can ask again). |
| USSD authentication | mNotify documents no callback authentication, so the callback URL carries a shared secret: `https://<host>/webhooks/ussd/mnotify/session?token=<MNOTIFY_USSD_TOKEN>`. Constant-time comparison; several comma-separated tokens are accepted for rotation; optional `MNOTIFY_USSD_ALLOWED_IPS` (with `MNOTIFY_CLIENT_IP_HEADER` behind a proxy) adds a second check. With no token configured every request is refused (`401`), and config validation fails when `PRIMARY_USSD_PROVIDER=mnotify`. The check runs before the body is read. The API key is not a callback credential. |
| USSD modes | One route serves both products. A body with `sessId`, `shortCode` or `ussdGwId` is **dedicated**; one with `sequenceID` or `data` is **shared**; one with both, or neither, is refused (`400`). Keys are matched case-insensitively (the file spells the phone `msisdn` in one product and `msIsdn` in the other, which Go's decoder would conflate). `MNOTIFY_USSD_MODE=shared` or `dedicated` pins one shape and refuses the other. Replies use the format of the mode the request came in. |
| Shared USSD input | `data` starting with `*` and holding no whitespace (a dialled string such as `*899*86#`) maps to `NewSession` with no input. Any other `data` is the user's entry, as is. The session is keyed by the phone number's digits (there is no session id), `FromE164` is the `msisdn` as received, and `ServiceCode` is left empty so `USSD_SERVICE_CODE` keys the conversation. **A request after a session we ended also starts a new session**, whatever it holds: the adapter remembers, for 5 minutes and in this process only, which numbers our last reply ended (`continueFlag` 1). With several API replicas that memory can be missed, and then only a dialled string starts a new session (the engine keeps an ended session's state for `USSD_SESSION_TTL_SECONDS`). |
| Dedicated USSD input | `sessId` is the session id; `msIsdn` the phone; `text` the input, unless it looks like a dialled string (starts with `*`, no whitespace), which is `NewSession` with no input. A session id the engine has not seen is a new session anyway. `Network` carries `ussdGwId`. There is no way to pass an operator "release" or "timeout" notice: none is documented. |
| USSD reply | Shared: `{"msisdn": <as received>, "sequenceID": <echo>, "message": <text>, "timestamp": <RFC 3339 UTC>, "continueFlag": 0 or 1}`. Dedicated: `{"responseExitCode": 200, "shouldClose": <End>, "ussdMenu": <text>, "responseMessage": "OK"}`. Every line break is converted to `\r\n` (the engine writes `\n`), text is not HTML-escaped, and the callback token is never echoed. The values echoed back (sequence id or mode) travel from `ParseSession` to `WriteReply` in `channel.USSDRequest.Network`, which nothing else reads. |
| Admin commands | `admin mnotify-status [-check-credentials]` prints the configuration (secrets only as set / NOT SET, never the callback token) and, with the flag, makes ONE call, `GET /balance/sms`, and prints the balance. `admin mnotify-sender-status [-sender NAME]` makes ONE call, `POST /senderid/status`, and prints Pending / Approved. Neither prints the key or a request URL. |
| Tests never reach mNotify | `internal/channel/mnotify/testguard.go` refuses any request to `mnotify.com`, `bms.africa` or their subdomains when the process is a test binary (error text names the host only). Unit and integration tests use `httptest` fakes. |

Fixtures in `tests/fixture_webhooks/mnotify_*.json`; each file also carries a `_note` saying where it came from:

| Fixture | Provenance |
|---|---|
| `mnotify_ussd_shared_request_openapi_example.json`, `mnotify_ussd_shared_response_openapi_example.json` | Copied from the shared-USSD request and response examples (the response example has the swapped values noted above; the tests compare its field set, `msisdn`, `message` and `continueFlag`, not its `sequenceID` or `timestamp`) |
| `mnotify_ussd_dedicated_request_openapi_example.json`, `mnotify_ussd_dedicated_response_openapi_example.json` | Copied from the dedicated-USSD examples; the tests reproduce the example `ussdMenu` byte for byte |
| `mnotify_sms_quick_request_openapi_example.json`, `mnotify_sms_quick_response_openapi_example.json` | Copied from the Quick SMS curl and 200 examples |
| `mnotify_balance_sms_response_openapi_example.json`, `mnotify_senderid_status_response_openapi_example.json` | Copied from the balance and sender-status 200 examples |
| `mnotify_ussd_shared_continue_constructed.json` | **Constructed**: a continuation of the shared request example; values invented |
| `mnotify_ussd_dedicated_begin_constructed.json` | **Constructed**: the dedicated example with the dialled code as `text`, a guess at a first request |

Error bodies used in the tests (a rejected sender ID, insufficient balance, an invalid key, a validation error, gateway
errors) are constructed inline: the file has no error examples.

**Do not replay the examples at a live stack unchanged.** The example phone number `233541509394` is the same number as mNotify's support line
(+233 54 150 9394, noted under Sources); with SMS live, an answer could be texted to it. The tests use fakes, and
the README's replay commands substitute your own number.

### Not confirmed: ask mNotify, or verify with a real account before the pilot

1. **USSD price.** Per session, per month, shared versus dedicated, setup fees: not published. (SMS prices are above.)
2. **Short code provisioning, and how the callback URL is registered.** Who allocates a shared or dedicated code, how long it
   takes, the code format, and whether the callback URL is set in the dashboard, by support or by an API call (none is in
   the file). `USSD_SERVICE_CODE` must be set to whatever is assigned.
3. **Callback authentication and source IPs.** Whether any signature or header exists, whether mNotify preserves the
   `?token=` query string of the registered URL, and which addresses it calls from (for `MNOTIFY_USSD_ALLOWED_IPS`).
4. **Response deadline and session timeout.** How long mNotify waits for our reply (the engine answers within
   `USSD_FAST_PATH_TIMEOUT_MS`, 3.2 s by default, then ends the session and sends the answer by SMS), how long a session
   lives, and what the user sees on a timeout or on `5xx`.
5. **Network coverage of shared USSD.** Whether the common short code works on MTN, Telecel (Vodafone) and AT (AirtelTigo),
   or only on some. The dedicated product names all three in `ussdGwId`.
6. **Individual versus business accounts.** Whether an individual can get SMS sender IDs, a shared or a dedicated USSD code,
   or whether KYC (a registered company) is required for any of them. The owner has no registered company; this was the
   reason MTN was dropped, so ask before paying for anything.
7. **Sender ID approval.** How long approval takes, what is required, and whether a default sender (the examples use
   `mNotify`) can be used before then. An unapproved sender may be rejected at send time or silently later.
8. **Credits for long messages.** What one SMS credit means for more than 160 characters or UCS-2 text, and how
   `credit_used` counts them. The engine's segment accounting assumes GSM/UCS-2 segment rules.
9. **Error responses.** The error body shape, `code` values, HTTP statuses (validation, authentication, balance, sender,
   rate limit) and whether a 2xx can carry a failure. The classification table above is a guess until this is known; check
   `send_attempts` and the logs after the first failures and tighten it.
10. **Limits.** Rate limits, throughput, maximum message length, key restrictions (IP binding, regeneration, scopes).
11. **Delivery reports.** Whether a push option exists that the file does not show (ask support), and the status vocabulary
    (`DELIVERED`, `SUBMITTED`, ... in the examples; `UNDELIVERED`, `FAILED`, `REJECTED` are filter names).
12. **Dedicated USSD input.** Latest entry versus accumulated history in `text`, the first request's contents, whether
    release/timeout notices are ever sent, and what `optional` carries. If `text` turns out to be a history, input handling
    needs a change like MTN's `MTN_USSD_INPUT_IS_HISTORY` (not built, because nothing says it is needed).
13. **`sequenceID` meaning**, and whether mNotify retries a callback that times out (a retried request would replay a menu step).
14. **Screen length with `\r\n`.** `USSD_MAX_CHARS` (182) counts a line break as one character; mNotify requires two. If
    its gateway counts both against the limit, lower `USSD_MAX_CHARS` (about 170).
15. **International numbers.** Whether numbers outside Ghana can be sent to, and in what format; the adapter refuses them.
16. **Idempotency.** Whether sending the same request twice is deduplicated (nothing says so; the design assumes not).
17. **Terms of service** (<https://bms.africa/terms-of-service/>): permitted use of an AI answering service, content rules,
    and opt-out obligations. Not read here.

### Operational risks

- **The API key is in URLs.** Anything that records an outbound URL records the key: mNotify's own logs, a corporate or egress
  proxy, `curl` commands in shell history or in process listings on a shared machine. The app and the admin commands avoid
  it; people must too. Rotate the key in the dashboard if it leaks, then update `MNOTIFY_API_KEY` and restart.
- **Callback shared-secret tokens.** Anyone who learns the USSD URL can drive sessions as any phone number until the token is
  rotated (comma-separated values `new,old`, then drop `old`). Mitigations: 24+ character random tokens
  (`scripts/init_env_prod.sh` generates 48 hex characters), TLS only, the optional IP allowlist. The app never logs query
  strings (`logRequests` logs only the path); **the TLS reverse proxy must not log them for `/webhooks/ussd/mnotify/*`
  either** (the pilot Caddyfile keeps no access log), and mNotify itself will store the full URL.
- **IP allowlist behind a proxy.** Set `MNOTIFY_CLIENT_IP_HEADER` (e.g. `X-Real-IP`) only if the proxy overwrites that header;
  the pilot Caddyfile does.
- **Every live send is real.** There is no sandbox. Keep the real key out of development `.env` files that run against a
  shared database, and use `sms_outbound_enabled=false` (`admin flag`) while testing USSD only.
- **Uncertain sends need a human.** They stay `unknown_timeout`; nothing resends them. Check the mNotify dashboard report for the
  number and time (docs/RUNBOOKS.md) before requeueing.
- **Failures can be invisible.** With no delivery reports, a message mNotify accepted but could not deliver stays `submitted`.
  Watch the credit balance and compare the dashboard report with `submitted` rows during the pilot.
- **An empty balance loses answers.** Rejections for balance are permanent. Watch the low-credit warning and top up early;
  `admin mnotify-status -check-credentials` shows the balance.
- **One vendor for both channels.** An mNotify outage or account suspension ends both USSD and SMS.
- **Ended-session memory is per process.** See "Shared USSD input" above.

## MTN developer platform (USSD + MT SMS) (dormant: the first choice, dropped 2026-10-18)

**Status: dormant.** On 2026-10-18 the owner dropped MTN after the two token 401s described below and after learning that
MTN's production access is vetted and needs a registered company, which the owner does not have. The adapter, its admin
commands and this section stay as they were (nothing selects MTN by default), so the history and the findings are not lost
if a registered company later makes MTN possible. Everything below is written in the present tense of 2026-10-17/18.

Adapter: `internal/channel/mtn`. Researched 2026-10-17. The contract below comes from MTN's public Swagger/OpenAPI
files and getting-started pages, or is marked as an assumption. The owner created the developer app and entered credentials
privately. **One explicitly authorized OAuth token request on 2026-10-17 returned HTTP 401 Unauthorized** at
`https://api.mtn.com/v1/oauth/access_token`; the CLI reported no readable error detail. No token was issued or displayed,
no SMS was sent, and outgoing SMS remains disabled. Authentication and the configured sender `SMS-AI` are unverified;
MTN sender approval has not been confirmed. **A second explicitly authorized request on 2026-10-18,
`admin mtn-status -check-credentials` against the sandbox host `https://sandbox.api.mtn.com/v1/oauth/access_token`, also
returned HTTP 401 with an empty body.** The stored client id (32 characters) and secret (16) are well formed: no whitespace,
quotes or carriage returns. That rules out "the request went to the production host" as the cause. The remaining
explanations are on MTN's side or in the portal: an app or product still pending approval, or a key pair that is not the
active one for the sandbox. No further MTN API requests are authorized; both one-request exceptions are used up.
SMS and USSD still need sandbox validation before real traffic.

**Read this first.**

1. The Swagger files confirm the *shape* of the contract: paths, bodies, field names, auth schemes. They say nothing about
   price, onboarding, short-code allocation, callback source addresses or deadlines (list below).
2. **The developer portal's own terms exclude production.** The "Terms of Use for MTN Developer Portal (sandbox version)"
   say the APIs are usable only in a sandbox, that the terms give no right to use them in live production or for commercial
   purposes, that MTN is not obliged to conclude any further agreement even after a successful test, that MTN may cap the
   number of end users, and that it may suspend access at any time without notice. The FAQ says the way to production is
   the portal's contact form, after which MTN "starts vetting you for production access", and that pricing is discussed
   only then. So **a production agreement with MTN is a separate, unpublished step, and nothing here proves MTN will grant
   it for this service.** Start that conversation in parallel with the sandbox work.
3. MTN's network only (above).

### Sources

| Source | What it covers |
|---|---|
| <https://developers.mtn.com/products/ussd> and <https://developers.mtn.com/products/ussd/download/swagger> | "MTN USSD interface" product page (Ghana is in its country list) and its Swagger 2.0 file (`host: api.mtn.com`, `basePath: /v1/`, `x-api-key`). The file is YAML despite the URL |
| <https://developers.mtn.com/products/sms-v3-api> and <https://developers.mtn.com/products/sms-v3-api/download/swagger> | "SMS v3 API" (Ghana listed), Swagger 2.0. Comparison context only; this app uses SMS V2 |
| <https://developers.mtn.com/products/mtn-sms-interface> and <https://developers.mtn.com/products/mtn-sms-interface/download/swagger> | "SMS V2 interface with callback support" (Ghana listed): OpenAPI 3.0.1 "Messaging SMS" v3.0.0, last updated 2026-09-23, `servers: https://api.mtn.com/v1`. **The SMS product selected by the owner and used by the adapter** |
| <https://developers.mtn.com/products/oauth-v1> and <https://developers.mtn.com/products/oauth-v1/download/swagger> | Selected OAuth V1 product: `/v1/oauth/access_token`, form `client_id`/`client_secret`, query `grant_type=client_credentials`; Swagger examples for success and invalid client |
| <https://developers.mtn.com/getting-started/understanding-oauth-20> | The OAuth token request and a sample response |
| <https://developers.mtn.com/getting-started/things-every-developer-should-know> | API key vs OAuth, hosts, `transactionId`, callbacks, sandbox host, MSISDN format |
| <https://developers.mtn.com/getting-started/response-and-error-codes> | The 4-digit canonical codes and their HTTP mapping |
| <https://developers.mtn.com/faq> | "Moving to production", costs, the API key being the Consumer Key |
| <https://developers.mtn.com/terms-and-conditions> | The sandbox-version terms quoted in "Read this first" |

Every product page carries the banner "Warning to 'GROUP' users (not applicable to Nigeria, South Africa, Uganda, Rwanda,
Ghana): portal moved to appx.developers.mtn.com". Ghana is in the exempt list, so `developers.mtn.com` is the right portal
for it; this is an inference from the banner, not a statement about Ghana's onboarding.

### Which SMS product, and why

| | SMS v3 API (comparison only) | SMS V2 interface with callback support (**chosen**) |
|---|---|---|
| Send | `POST /v3/sms/messages/sms/outbound`, our `clientCorrelatorId` (max 36 characters) | `POST /v1/messages/sms/outbound`, our `clientCorrelator` |
| Send response | Status only (`PENDING` in the example). **No message id** | `data.requestId` assigned by MTN, plus our `clientCorrelator` |
| Delivery notification body | **Published** (`DeliveryNotificationRequest`), echoing our `clientCorrelatorId`, with a status enum | **Not published** (only the subscription call is) |
| Delivery-report registration | One subscription: `callbackUrl` (MO), `deliveryReportUrl`, `serviceCode`, `targetSystem`; PATCH to change | Per `senderAddress`, `notifyUrl`; needs headers `transactionId` and `x-country-code` (value format unstated) |
| Lookup of a sent message | None | `GET /messages/sms/outbound/{senderAddress}/{requestId}/deliveryStatus` |
| Token URL in its Swagger | `.../v1/oauth/access_token/accesstoken?grant_type=client_credentials` | `.../v1/oauth/access_token` |

The owner selected **SMS V2**, **OAuth V1** and **MTN USSD interface** in their developer app.
The adapter follows that choice. The separate v3 product remains comparison context only.

V2's status lookup takes the `requestId` from the send response. A send whose response was lost has no such id.
Whether MTN also accepts our `clientCorrelator` in that path is undocumented and untested.
`MTN_SMS_LOOKUP_BY_CLIENT_REF=false` therefore keeps automatic reconciliation disabled. Enable it only after a
sandbox check proves lookup by our reference and that the result echoes that same reference. Even when enabled,
an unreadable response, missing/mismatched reference, HTTP 404 or other failure is an error, never a definitive
"not found": that must never cause a possibly billed SMS to be resent.

### Confirmed from MTN's published files

**USSD (Swagger 2.0, `host: api.mtn.com`, `basePath: /v1/`, header `x-api-key`)**

- Register a callback: `POST /messages/ussd/subscription` with `{serviceCode, callbackUrl, targetSystem}` (all required;
  examples `*1234*356#`, an internal `http://10.138.40.69:11400/...` URL, `AYO`). `201` returns `data.subscriptionId`.
  `DELETE /messages/ussd/subscription/{subscriptionId}` removes it (`200`). There is no operation to list or update a
  subscription.
- MTN's gateway sends the "MO-generated message" through MADAPI (MTN's API platform) to the callback. The request schema
  (`InboundRequest`) has `sessionId`, `messageType`, `msisdn`, `serviceCode`, `ussdString` (all required) and optional
  `cellId`, `language`, `imsi`. `messageType` is "0-Begin|1-Continue|2-End|3-Notification|4-Cancel|5-Timeout" and is
  typed as a string (example `'0'`). `msisdn` is "Mobile number of the message recipient" with the example `2252312345`
  (no `+`, an Ivory Coast number).
- The documented answer (`InboundResponse`): `statusCode` (`'0000'`), `statusMessage`, `transactionId`, `data` and `_link`
  required; `data` has `inboundResponse` (the text), `userInputRequired` (boolean), `messageType` (integer, "the
  messageType to send", example 1), `serviceCode` and `msisdn` ("the msisdn to send the response to").
- A push call exists: `POST /messages/ussd/outbound` with `{sessionId, messageType, msisdn, serviceCode, ussdString}`
  (`201`). The adapter does not use it: the engine answers inline or ends the session and sends the answer by SMS.
- Error bodies (`Error`): `statusCode` (a 4-character MADAPI code, "not the HTTP status"), `statusMessage`,
  `supportMessage`, `transactionId`, `timestamp`, `path`, `method`.

**SMS V2 (OpenAPI 3.0.1, server `https://api.mtn.com/v1`, OAuth 2.0 client credentials)**

- Send: `POST /messages/sms/outbound` with `{senderAddress, receiverAddress: [...], message, clientCorrelator}`.
  `senderAddress`, `message` and `clientCorrelator` are required; `cpAddress` is optional and unused.
  The response contains `data.requestId` assigned by MTN and `data.clientCorrelator`.
- Delivery subscription: `POST /messages/sms/outbound/{senderAddress}/subscription` with
  `{notifyUrl, targetSystem}`, headers `transactionId` and `x-country-code`, returning `data.id`.
  `DELETE /messages/sms/outbound/{senderAddress}/subscription/{subscriptionId}` removes it.
  No listing or update operation is published. **The notification body sent to notifyUrl is not published.**
- Status lookup: `GET /messages/sms/outbound/{senderAddress}/{requestId}/deliveryStatus` returns
  `data: {requestId, clientCorrelator, deliveryStatus: [{receiverAddress, status}]}` (the `DeliveryInfo` model).
  Status is free text with no enum. The path is documented by MTN request id, not our client reference.
- V2 has no SMS `serviceCode`, `requestDeliveryReceipt` flag or mobile-originating callback registration.
  Its success-code values and complete success payloads have no examples; `0000` is inferred from other MADAPI files.

**Platform (getting-started pages and FAQ)**

- OAuth: `POST https://api.mtn.com/v1/oauth/access_token?grant_type=client_credentials` with the app's Consumer Key and
  Secret as form fields `client_id` and `client_secret`. The sample response has `access_token`, `expires_in: "3599"`,
  `token_type: "BearerToken"`, `status: "approved"`. Calls carry `Authorization: Bearer <token>`.
- Two authorisation mechanisms exist: "most of the MTN APIs use API Key today", sent as `x-api-key`, which "you can get from
  the apps section ... under Consumer Key". Each product page says which it uses; the USSD file declares only the API key,
  the SMS files only OAuth.
- **Callbacks cannot be authenticated:** in the "Callbacks and polling" notes, MTN says its platform does not support
  authentication on the client endpoint it calls back. (The sentence is about asynchronous-operation callbacks in general;
  applying it to the USSD and delivery callbacks is an inference, consistent with the Swagger files describing none.)
- Hosts: global `api.mtn.com`; per-country `{opco}.api.mtn.com`; a sandbox host `sandbox.api.mtn.com` "responds with dummy
  account data". The base path of a product is the same on every host.
- Clients may send a `transactionId` header "for traceability". MSISDNs use E.123. Only the current and previous API
  version are maintained ("Versions older than n-1 will NOT be supported").
- Canonical codes: `0000` success; `1013` and `3006` rate exceeded (HTTP 429); `3000` service not available (503); `3001`
  system error (500); `4000` unauthorised (401); `5000` invalid parameters (400); `6000` billing failed and `6001`
  insufficient funds (both 403).

### Quirks and contradictions in MTN's files

1. **USSD reply name.** `InboundResponse.data.required` lists `outboundResponse`, but the defined property is
   `inboundResponse`. The adapter sends the latter; verify what Ghana's gateway reads.
2. **USSD callback reachability.** Its example URL is an internal plain HTTP address; public HTTPS reachability is untested.
3. **Credentials disagree.** USSD Swagger declares only `x-api-key`; SMS V2 declares only OAuth. The OAuth V1 page
   calls OAuth "the preferred access method for all API products"; the FAQ says most APIs use API keys and the key
   is the Consumer Key. Defaults follow the product schemas. `MTN_SMS_SEND_API_KEY=true` adds the API key to all
   SMS calls; `MTN_USSD_SEND_BEARER=true` adds the OAuth token to USSD registration/removal. Neither is validated
   against MTN; enable only according to MTN's instructions or a sandbox result.
4. **Two token URLs in different products.** OAuth V1 and SMS V2 agree on `/v1/oauth/access_token`; the separate
   v3 file adds `/accesstoken`. Use OAuth V1 for this app. `MTN_TOKEN_URL` is an explicit override only if MTN
   supplies a different endpoint.
5. **No notification schema or id guarantee.** We assume V2's `DeliveryInfo` shape. The message row stores
   `data.requestId`; a callback might carry only `clientCorrelator`. If sandbox evidence shows that, change
   the stored provider id to the client reference before pilot traffic. Emitting receipts for both ids helps only
   when the notification actually carries both.
6. **Country header.** SMS subscription creation/removal requires `x-country-code` but gives no format.
   `MTN_COUNTRY_CODE=GH` assumes ISO alpha-2. Confirm whether Ghana instead expects `GHA`, a number or another value.
7. **Types and examples.** USSD `messageType` is a string on input and an integer on output (both parse).
   Phone examples are from other countries; Ghana's real input and accepted destination format remain untested.

### How the adapter uses this

| Concern | Adapter behaviour |
|---|---|
| USSD authentication | MTN authenticates nothing, so the registered callback URL carries a shared secret: `https://<host>/webhooks/ussd/mtn/session?token=<MTN_USSD_TOKEN>`. Constant-time comparison; several comma-separated tokens are accepted for rotation; optional `MTN_USSD_ALLOWED_IPS` adds a second check. With no token configured every request is refused (`401`) and config validation fails when `PRIMARY_USSD_PROVIDER=mtn`. The check runs before the body is read. |
| Delivery-notification authentication | The same scheme with its own token: `https://<host>/webhooks/sms/mtn/status?token=<MTN_DLR_TOKEN>` and `MTN_DLR_ALLOWED_IPS`. Separate tokens mean a leaked notification URL cannot drive USSD sessions. With no `MTN_DLR_TOKEN` every notification is refused and delivery subscription registration is blocked. V2 has no per-send receipt flag. |
| USSD input | `messageType` 0 (Begin) → `NewSession`, `Input = ""` (`ussdString` is the dialled string, not a menu entry). 1 (Continue) → `Input = ussdString` as is, never split on `*`, unless `MTN_USSD_INPUT_IS_HISTORY=true`, which keeps only the part after the last `*`. 2 (End), 4 (Cancel), 5 (Timeout) → `Input = "0"`, the engine's *Exit*, because `channel.USSDRequest` has no "ended" flag: the engine closes its session without booking a question or spending quota, and the reply is not shown. An End for an unknown or expired session makes the engine open a fresh session (3-minute TTL) that nobody uses; the Hubtel adapter has the same behaviour. 3 (Notification) and unknown types are refused with `400`. |
| USSD identity | `SessionID = sessionId`; `FromE164 = msisdn` exactly as received (the engine normalizes `2332...`, `02...` and `+2332...`), `Network = "mtn"`. `ServiceCode` is left empty so the engine's `USSD_SERVICE_CODE` keys the conversation however MTN spells the code. |
| USSD reply | In the HTTP response body, status `200`: `{"statusCode":"0000","statusMessage":"Successful","transactionId":"","data":{"inboundResponse":<text>,"userInputRequired":<!End>,"messageType":1 (CON) or 2 (END),"serviceCode":<MTN_USSD_SERVICE_CODE>,"msisdn":<as received>},"_link":{"self":{"href":"/webhooks/ussd/mtn/session"}}}`. The callback token is never echoed. |
| MT SMS | `POST {MTN_BASE_URL}/v1/messages/sms/outbound`, OAuth bearer (plus `x-api-key` if `MTN_SMS_SEND_API_KEY=true`); JSON `{senderAddress: SMS_OUTBOUND_SENDER_ID, receiverAddress: ["<country-code digits>"], message, clientCorrelator: <our message id>}`. Header `transactionId` equals the client reference. Sender IDs: 1–11 characters; reference: 1–36 bytes. |
| OAuth token | `internal/channel/mtn/token.go`: `POST` to `MTN_TOKEN_URL` (default `<MTN_BASE_URL>/v1/oauth/access_token`) with `grant_type=client_credentials` in the query and `client_id`/`client_secret` as a form body, exactly as MTN's page shows. The token is cached until 60 seconds (or a quarter of a short lifetime) before `expires_in`, replaced under a single in-flight request however many senders ask at once, and dropped when MTN answers a call made with it `401`. Redirects are never followed, so the secret cannot be replayed to another host. Errors never contain the credentials. |
| Provider message id | Store `data.requestId` from the send response; fall back to our client reference if missing or unusable. Confirm the callback id in the sandbox: a notification carrying only `clientCorrelator` cannot match a row stored under a different requestId. In that case change the stored id to the client reference before pilot traffic. |
| Send error kinds | Token failure or transport failure before writing: Retryable. Timeout/loss after writing and `5xx`: Uncertain. `3xx` (never followed), `401/402/403/404/405/407/408/429`: Retryable. Other `4xx`: Permanent. A readable `2xx` with canonical code `0000`, or no code with a requestId, is accepted; other `2xx` bodies are Uncertain. No v3 send-status field is interpreted. Errors exclude request bodies and credentials. |
| Delivery notifications | The assumed V2 `DeliveryInfo` shape, data wrappers, arrays, OMA nesting and flat v3-shaped notifications are accepted. Require `requestId` or `clientCorrelator` (also `clientCorrelatorId`) and a status. Emit one receipt per distinct id. DELIVERED/DeliveredToTerminal → delivered; ACCEPTD/ENROUTE/PENDING/DeliveredToNetwork → submitted; UNDELIVERED/DELETED/DeliveryImpossible → failed; REJECTED → rejected; EXPIRED → expired. Unknown statuses are logged and ignored. Error code is at most 64 bytes; readable `completedDate` sets time, else arrival time. |
| Reconciliation | `Provider` itself never implements `channel.StatusQuerier`. `Sender()` adds it only with `MTN_SMS_LOOKUP_BY_CLIENT_REF=true`. `Lookup` uses the sender and request-id path. A found result requires MTN to echo our client reference; all failures are errors, never definitive not-found results. Default: uncertain sends remain `unknown_timeout` for an operator. |
| Callback registration | `admin mtn-subscribe` registers USSD normally and SMS V2 `notifyUrl` per `SMS_OUTBOUND_SENDER_ID`, with `MTN_COUNTRY_CODE` (default `GH`) in `x-country-code` and a generated `transactionId`. SMS id is `data.id`. `mtn-unsubscribe` needs the original sender and saved ids; `mtn-status` shows local configuration and knobs. URLs are redacted; `-dry-run` sends nothing. Tests refuse every `*.mtn.com` URL. |
| Inbound MO SMS | Unused and refused. SMS V2 has no mobile-originating callback to register. |
| Environments | MTN has no sandbox flag; the environment is the host. With `PRIMARY_SMS_PROVIDER=mtn`, config validation insists that `SMS_ENVIRONMENT` agrees with `MTN_BASE_URL`: `live` for `api.mtn.com` and country hosts, `sandbox` for `sandbox.api.mtn.com`; `live` on the sandbox host and `sandbox` on a production host are errors. A local fake URL fits either. |

Fixtures in `tests/fixture_webhooks/mtn_*.json` and where each came from:

| Fixture | Provenance |
|---|---|
| `mtn_oauth_token_response_doc_example.json` | OAuth documentation sample; not a credential |
| `mtn_oauth_token_response_swagger_example.json` | OAuth V1 per-field Swagger examples; not a credential |
| `mtn_ussd_begin_swagger_example.json`, `mtn_ussd_subscription_response_swagger_example.json`, `mtn_error_response_swagger_example.json` | Assembled from published per-field Swagger examples |
| `mtn_ussd_begin_constructed.json`, `mtn_ussd_continue_constructed.json`, `mtn_ussd_end_constructed.json`, `mtn_ussd_timeout_constructed.json`, `mtn_ussd_response_constructed.json` | Constructed USSD input/reply examples; Ghana values are assumptions |
| `mtn_sms_send_response_constructed.json`, `mtn_sms_subscription_response_constructed.json`, `mtn_sms_delivery_status_response_constructed.json` | V2 response schemas with invented values; success code borrowed from other MADAPI files |
| `mtn_sms_delivery_notification_constructed.json`, `mtn_sms_delivery_notification_undelivered_constructed.json` | Assumed DeliveryInfo-shaped callbacks, one wrapped in data with invented error text. MTN publishes no V2 callback schema |
| `mtn_sms_delivery_notification_v3_shaped_constructed.json` | Constructed compatibility case from the separate v3 notification schema; not evidence of V2's callback body |

Each fixture file also carries a `_note` field saying so.

### Not confirmed: ask MTN, or verify in MTN's sandbox, before the pilot

1. **Production access, onboarding and KYC.** The portal terms cover sandbox only. Confirm a separate MTN agreement,
   eligibility for this service, review documents, timelines and approval for the three selected products.
2. **Pricing and limits.** Per USSD session, per SMS segment, receipts, code rental, minimum spend/deposit, quotas,
   rate limits and user caps are unpublished. Update spend assumptions once MTN supplies its quote.
3. **USSD code provisioning.** Dedicated/shared code, who provisions it, cost, and accepted `serviceCode` spelling.
4. **Sandbox availability.** Whether SMS V2, OAuth V1 and USSD work for Ghana on `sandbox.api.mtn.com`, and whether
   sandbox calls notifyUrl or the USSD callback.
5. **Sender and phone format.** Approved `SMS_OUTBOUND_SENDER_ID`, sender rules, accepted destination format,
   and the MSISDN Ghana sends. We send country-code digits without `+` and echo inbound MSISDNs unchanged.
6. **Country header.** Verify `MTN_COUNTRY_CODE=GH` for both subscription operations.
7. **Callback security and reachability.** Confirm MTN preserves the `?token=` query string, public HTTPS/TLS,
   ports, redirects and callback source IPs; no source ranges are published.
8. **USSD contract.** Does MADAPI relay `InboundRequest` to our callback and accept our inline `InboundResponse`?
   Confirm `inboundResponse` vs `outboundResponse`, deadlines, session/screen limits, encoding, retries and failure screens.
9. **USSD input.** Begin and dial-through contents, latest entry vs cumulative history
   (`MTN_USSD_INPUT_IS_HISTORY`), Notification (3), and whether End/Cancel/Timeout replies are consumed.
10. **Delivery body and ids.** Obtain a real notification sample: `DeliveryInfo` is only an assumption.
    Determine `requestId` vs `clientCorrelator`; if only the latter arrives, store the client reference as provider id.
    Confirm status vocabulary, batching, method, timestamps, retries, expected acknowledgement and late updates.
11. **Credentials.** Verify OAuth V1 token endpoint, scope (none requested), bearer format, Consumer Key as API key,
    and whether the SMS/USSD credential knobs are needed. Keep them false until verified.
12. **Subscriptions.** Allowed `targetSystem` (default `sms-ai-ghana`), uniqueness, expiry, sender changes,
    re-registration and callbacks during delete/create. There is no listing call: retain ids and the original sender.
13. **Idempotency.** Whether repeated `clientCorrelator` or `transactionId` deduplicates a send.
    V2 gives no reference length/character limit; the adapter keeps its 36-byte UUID bound.
14. **Encoding and size.** GSM/UCS-2 detection, concatenated SMS, per-segment billing and content limits.
15. **Lookup by client reference.** Prove that the request-id path accepts our `clientCorrelator` and echoes it.
    `MTN_SMS_LOOKUP_BY_CLIENT_REF` is untested and false by default. Failed lookups never mean "not found".
16. **Version support.** Confirm MTN Ghana supports the selected V2 product and its lifecycle, independently of
    the Swagger schema's own version number.

### Operational risks

- **Callback shared-secret tokens.** Anyone who learns the USSD URL can drive sessions as any phone number until the token
  is rotated; anyone who learns the notification URL can mark messages delivered or failed. Mitigations: 24+ character
  random tokens (`scripts/init_env_prod.sh` generates 48 hex characters), TLS only, separate tokens, an optional IP
  allowlist, and rotation through comma-separated values. The app never logs query strings (`logRequests` logs only the
  path), but **the TLS reverse proxy must not log them for `/webhooks/ussd/mtn/*` and `/webhooks/sms/mtn/*` either** (the
  pilot Caddyfile keeps no access log), and **MTN's own platform will store the full URL** in its subscription.
- **IP allowlist behind a proxy.** The allowlists check the TCP peer. Behind a reverse proxy, set `MTN_CLIENT_IP_HEADER`
  (e.g. `X-Real-IP`) **only if the proxy overwrites that header**. Otherwise a client can forge it.
- **Every live send is real.** With `SMS_ENVIRONMENT=live` and the production host, a stack texts and bills real phones.
  Keep the MTN credentials out of development `.env` files, or point them at the sandbox host.
- **Receipts can be lost when they are fast.** The message row gets its `provider_message_id` after the send call returns;
  a notification that arrives before that commit is applied to nothing and the message stays `submitted`. This applies to
  every provider and is rare.
- **Uncertain sends need a human.** They stay in `unknown_timeout` by default. Optional, sandbox-verified client-reference lookup
  can confirm submission; failed lookups never authorize a resend. Check unresolved attempts with MTN before requeueing.
- **Receipts may never arrive.** If the delivery URL is not registered, or the body differs from the assumed shape, messages stay
  `submitted`. Watch `webhook_rejected_total{provider="mtn"}` and the "status not mapped" log line during the first tests.
- **One side can silently stop working.** MTN is the single provider for both USSD and SMS, and only MTN subscribers can
  use either. An MTN outage or a suspended account (the terms allow suspension without notice) ends the whole service.
- **Release handling is a workaround**, as for Hubtel: End, Cancel and Timeout reach the engine as the *Exit* entry.

## Arkesel (Track A: USSD ingress + alphanumeric MT SMS) (dormant)

Adapter: `internal/channel/arkesel`. Researched 2026-10-17. No live Arkesel API was called. Every statement below
comes from the published documentation or is marked as an assumption.

### Sources

| Source | What it covers |
|---|---|
| <https://arkesel.com/developer-api/ghana/> | Marketing overview: `POST https://sms.arkesel.com/api/v2/sms/send` with an `api-key` header, delivery status by webhook |
| <https://developers.arkesel.com/> (Redoc), sections *SMS V2*, *Webhook Signature Verification*, *USSD Section → Overview / API Reference* | Developer reference |
| <https://developers.arkesel.com/spec/api_spec.v2.4.0.yaml> | The OpenAPI 3.0 spec behind the Redoc page (spec file version 2.4.0). All quoted payloads come from here |
| <https://github.com/ArkeselDev/express-ussd-sample> (`app.js`), plus PHP/Laravel/FastAPI/.NET samples linked from the USSD overview | Official USSD sample apps |
| <https://docs.page/nexlabstudio/starhash-docs/providers/arkesel> | **Third-party** integration notes (StarHash). Used only for timeout hints and treated as unconfirmed |

### Confirmed from Arkesel's documentation

**SMS v2 send**

- `POST https://sms.arkesel.com/api/v2/sms/send`, header `api-key: <key>`, JSON body.
- Body fields: `sender` (required, **max 11 characters including spaces**, "anything more … will result in your messages
  failing"), `message` (required), `recipients` (required, array of numbers such as `"233544919953"`: country code,
  no `+`), `callback_url` (optional, per-message delivery webhook), `scheduled_date` (optional, `Y-m-d H:i A`),
  `use_case` (`promotional`/`transactional`, **Nigeria only**), `sandbox` (optional boolean).
- `sandbox: true` is not billed, and the message is not forwarded to the networks. It only shows in the SMS history report.
- Success is `200 {"status":"success","data":[{"recipient":"233544919953","id":"9b752841-…"}, …, {"invalid numbers":["22354674948"]}]}`.
  The `id` is the message UUID used by delivery callbacks and lookups.
- Documented error statuses, each with a `{"status":"error","message":…}` body:
  `401` authentication failed, `402` "Insufficient balance or invalid coverage!", `403` "Inactive SMS Gateway!",
  `422` validation errors (e.g. "The message field is required."), `500` "SMS request failed!".
- One page is 160 characters ("A 200 characters messages will be 2 pages").

**Message lookup**

- `GET https://sms.arkesel.com/api/v2/sms/{uuid}` returns
  `{"status":"success","data":{"ID":…,"status":"DELIVERED","sender":…,"recipient":…,"message":…,"message_count":1,"sent_at_time":"2021-04-09 18:44:05"}}`.
- `POST /api/v2/sms/message-reports` with `{"msg_ids":[…]}` (1 to 1000 UUIDs) returns a map of UUID to details.
  Unknown IDs come back as `{"status":"error","response":"message does not exist"}`.
- Both endpoints look messages up **by Arkesel's UUID only**. The send API has no client-reference or idempotency
  field.

**Delivery reports**

- Arkesel calls the per-message `callback_url` with **two query parameters**, `sms_id` and `status`. The status
  values are documented as "such as DELIVERED, SUBMITTED, PROHIBITED, QUEUED, NOT_DELIVERED, EXPIRED".
- The `callback_url` description says to exempt the URL from authentication. This is superseded by the signing scheme below.

**Webhook signing (SMS and Voice SMS callbacks)**

- Headers: `X-Arkesel-Webhook-Timestamp` (unix seconds), `X-Arkesel-Webhook-Signature` (`v1=<hex>`; during rotation
  two comma-separated values `v1=<hex>,v1=<hex>`, so accept if any matches), `X-Arkesel-Webhook-Id` (unique per
  callback, usable for de-duplication).
- Signed string: `{timestamp}.{canonical_json}`, where the canonical JSON is the callback's **query parameters** with
  keys sorted recursively, list order kept, and PHP `json_encode(..., JSON_UNESCAPED_SLASHES)` escaping (non-ASCII as
  `\uXXXX`). HMAC-SHA256 with the raw secret, hex digest, constant-time comparison, and rejection of timestamps
  older than 5 minutes. The docs' example is
  `1783425600.{"sms_id":"abc123","status":"DELIVERED"}`, and the adapter's tests check that exact string.
- The secret comes from `GET /api/v2/webhooks/secret`. **Signed headers are only sent after a secret has been
  generated** with `POST /api/v2/webhooks/secret/rotate`. Rotating keeps the previous secret valid for 24 hours,
  and callbacks carry both signatures in that window.
- The docs' example callback is `GET /callback?sms_id=abc123&status=DELIVERED`.

**USSD**

- Arkesel `POST`s JSON to the endpoint URL registered during USSD setup:
  `{"sessionID":"2005506191900168","userID":"USSD_DOCUMENTATION","newSession":true,"msisdn":"233271231234","userData":"*928*1#","network":"AIRTELTIGO"}`.
- The response must be JSON with **all** of `sessionID`, `userID` and `msisdn` echoed exactly, plus `message` (min
  length 1) and `continueSession` (`true` keeps the session open for input, `false` ends it). This is the CON/END
  equivalent.
- The first request of a session has `newSession: true` and the dialled code in `userData`. In the official samples,
  later requests carry only the latest entry (e.g. `userData == "1"`), not a `1*2*…` history.
- **USSD callbacks are not signed**: "USSD callback signing is not currently available in this flow. USSD signing will
  be announced separately when the gateway-side work is available."

### How the adapter uses this

| Concern | Adapter behaviour |
|---|---|
| USSD authentication | Arkesel does not sign USSD, so the registered endpoint URL carries a shared secret: `https://<host>/webhooks/ussd/arkesel/session?token=<ARKESEL_USSD_TOKEN>`. Comparison is constant-time. Several comma-separated tokens are accepted so the URL can be rotated without downtime. An optional `ARKESEL_USSD_ALLOWED_IPS` allowlist adds a second check. With no token configured, every request is refused and config validation fails. |
| USSD reply | `{"sessionID","userID","msisdn","message","continueSession": !End}`. `userID` is carried inside the internal session key (`<escaped userID>:<sessionID>`) so it can be echoed without adapter state, and `msisdn` is passed through as received. |
| USSD input | `newSession: true` gives `Input = ""` (the dialled code is not a menu entry). Otherwise `Input = userData` as-is. It is never split on `*`, because questions may contain `*`. |
| USSD service code | Left empty so the engine uses `USSD_SERVICE_CODE` on every step (Arkesel sends the dialled code only on the first request). |
| MT SMS | `POST {ARKESEL_BASE_URL}/api/v2/sms/send`, header `api-key`, JSON `{sender, message, recipients: ["<E.164 without +>"], callback_url, sandbox}`. Stores Arkesel's returned message UUID. Invalid numbers are Permanent errors; an accepted response without a usable message id is Uncertain. |
| Send error kinds | A transport failure before the request was fully written is Retryable; a timeout or connection loss after writing is Uncertain. `3xx` (not followed), `401`, `402`, `403`, `404`, `405`, `408`, `429` are Retryable; `5xx` are Uncertain; other `4xx` are Permanent. Unreadable or incomplete success responses are Uncertain. |
| Delivery reports | `ParseStatus` reads `sms_id`/`status` from the query string. It falls back to a JSON body if the query is empty. It verifies the `v1=` signature(s) with `ARKESEL_WEBHOOK_SECRET` and always rejects unsigned callbacks. Mapping: `DELIVERED`→delivered; `SUBMITTED`/`QUEUED`/`SENT`/`ACCEPTED`→submitted; `NOT_DELIVERED`/`UNDELIVERED`/`FAILED`→failed; `PROHIBITED`/`REJECTED`→rejected; `EXPIRED`→expired. Anything else becomes `unknown` and is logged and ignored. The raw status is kept as the error code on non-delivered outcomes. |
| Reconciliation | `Lookup` can query by Arkesel's message UUID, but the adapter does not implement `channel.StatusQuerier`: a timed-out send has no UUID and there is no lookup by our client reference. Uncertain sends wait in `unknown_timeout` for an operator. |
| Inbound MO SMS | Unused and refused. Track A uses Arkesel for USSD ingress and MT SMS only. |

### Not confirmed: ask Arkesel or verify in the sandbox before the pilot

1. **Delivery-callback HTTP method.** The signing example shows `GET`, and the parameters are in the query string.
   `/webhooks/sms/{provider}/status` accepts both `GET` and `POST` (same handler, covered by the integration test).
   Confirm the method with one real sandbox callback.
2. **Full delivery-status vocabulary.** The docs say "such as". Unknown values are logged as `arkesel delivery status not mapped`.
   Add them to `statusMap` as they appear.
3. **Canonical JSON edge cases.** Arkesel signs a PHP array. The adapter rebuilds it from the received query string as
   flat string values, using the last value for repeated keys. Keys with `[]`, PHP's `.`/space-to-`_` key mangling, and
   numeric-key ordering are not reproduced, so such callbacks fail verification (safe, but they would be lost).
   Confirm with one real signed callback.
4. **Retries and de-duplication of callbacks.** Whether Arkesel retries a callback after a non-2xx response, how often,
   and whether `X-Arkesel-Webhook-Id` stays the same across retries. Receipts are applied idempotently either way.
5. **`sms_id` format.** The docs call it "a 16 character unique identifier (UUID)" but every example is a 36-character UUID.
   The adapter treats it as opaque.
6. **Sender ID registration.** The length limit (11) is documented. The approval process, the allowed characters (is
   `SMS-AI` accepted?), and per-network (MTN/Telecel/AT) registration in Ghana are not.
7. **Unicode / UCS-2.** The v2 API has no encoding field. Whether non-GSM text is auto-detected, sent as UCS-2, and
   billed per 70/67 characters is undocumented. Until confirmed, keep `ALLOW_UCS2_LOCAL_LANGUAGE` in mind when
   estimating cost.
8. **Idempotency.** No client reference exists on `/api/v2/sms/send`. Ask whether one can be added, or whether messages can be
   searched by recipient and time. Either would allow automatic reconciliation (`channel.StatusQuerier`).
9. **Single-recipient rejections.** Whether an invalid single recipient returns `200` with only `invalid numbers` (as
   the multi-recipient example suggests) or a `422`. Both are handled as Permanent.
10. **Rate limits** on the send and lookup APIs: undocumented.
11. **Sandbox behaviour.** Whether sandbox sends return message IDs and trigger (signed) delivery callbacks.
12. **USSD source IPs.** Needed to fill `ARKESEL_USSD_ALLOWED_IPS`. Not published.
13. **USSD response deadline and session length.** Arkesel does not document them. A third-party guide suggests answering
    in under 25 s and notes a 2.5-minute inactivity timeout. MNO limits differ. The 3.2 s fast path
    (`USSD_FAST_PATH_TIMEOUT_MS`) is well inside any of these, but confirm Arkesel's own timeout and its retry behaviour.
14. **USSD screen length.** Undocumented. `USSD_MAX_CHARS=182` is the common GSM limit. Confirm for MTN, Telecel and AT
    through Arkesel.
15. **USSD `network` values.** Only `AIRTELTIGO` appears in the docs. The adapter lower-cases whatever arrives and does not
    rely on it (the carrier is derived from the number).
16. **USSD `userData` semantics.** "Latest entry only" is inferred from the official samples rather than stated.
    Dial-through strings (e.g. `*928*1*5#`) are untested.
17. **Error replies.** What the handset shows when the endpoint answers `401`/`400`/`5xx`, and whether Arkesel retries a USSD
    request.
18. **Commercial items from plan §6.1:** shared USSD sub-code monthly cost, MO SMS availability, and NCA authorisations covered
    by Arkesel.

### Operational risks

- **USSD shared-secret token.** Anyone who learns the endpoint URL can drive sessions as any phone number until the
  token is rotated. Mitigations: 48-hex-character tokens, TLS only, an optional IP allowlist, and token rotation
  through comma-separated `ARKESEL_USSD_TOKEN` values. The app never logs query strings (`logRequests` logs only the
  path), but **the TLS reverse proxy must not log query strings for `/webhooks/ussd/arkesel/*` either**. Switch to
  signature checks when Arkesel ships USSD signing.
- **IP allowlist behind a proxy.** `ARKESEL_USSD_ALLOWED_IPS` checks the TCP peer. Behind a reverse proxy, set
  `ARKESEL_CLIENT_IP_HEADER` (e.g. `X-Real-IP`) **only if the proxy overwrites that header**. Otherwise a client can forge it.
- **Uncertain sends need a human.** Without a lookup by our reference, messages whose submission timed out stay in
  `unknown_timeout`. Check them in the Arkesel dashboard before requeueing.
- **Webhook secret bootstrapping.** Until `POST /api/v2/webhooks/secret/rotate` has been called once, Arkesel sends
  unsigned callbacks, and this service rejects all of them (`401`).

## Hubtel (Track A: USSD ingress + alphanumeric MT SMS) (dormant)

Adapter: `internal/channel/hubtel`. Researched 2026-10-17 as the planned replacement for Arkesel (the owner chose not to
use Arkesel). No Hubtel API was called, no account was created and no SMS was sent. Every statement below comes from the
sources listed here or is marked as an assumption.

**Read this first: the SMS half is much less certain than the USSD half.** Hubtel's USSD contract is documented on
`dev-docs.hubtel.com` and in its official SDK source, and was read in full. Hubtel's SMS API reference
(`businessdocs-developers.hubtel.com`, `help.hubtel.com`) **could not be read**: neither host has a DNS record any more
(NXDOMAIN or no address from 1.1.1.1, 8.8.8.8 and the tool resolver on 2026-10-17), `developers.hubtel.com/docs/*` returns
the portal's 404 page, and the Wayback Machine was offline. The SMS details below come from Hubtel's own (older) SMS SDKs,
search-result excerpts of those unreachable pages, and community wrappers. The delivery-report callback is not documented
in anything that could be read. Treat the SMS rows as a draft to confirm in a test account or with Hubtel support before
the pilot.

### Sources

| Source | What it covers | Read |
|---|---|---|
| <https://dev-docs.hubtel.com/backend/programmable-services/building-PS.html> | "Building a Programmable Service": Service Interaction URL request/response tables and a sample USSD session. The USSD payloads quoted here come from it | in full |
| <https://dev-docs.hubtel.com/backend/programmable-services/overview.html> | What Programmable Services are (USSD, Hubtel App, Hubtel Mall; Interaction, Fulfillment and Callback stages) | in full |
| <https://dev-docs.hubtel.com/backend/sdk/programmable-service.html> | Hubtel's SDK page: sample initiation request and response, "USSD responses must be delivered within 7 seconds" | in full |
| <https://github.com/hubtel/programmable-services-sdk-dotnet> (`README.md`, `Models/*.cs`, `Core/HubtelProgrammableService.cs`, `Core/ProgrammableServicesMvcPipelineHandler.cs`) | Hubtel's own SDK source: request and response models, the request-type switch (Timeout, Query, Favorite), "<= 5s for USSD applications", no authentication in the pipeline | the files named |
| <https://dev-docs.hubtel.com/tools/programmable-service-simulator.html> and <https://programmable-simulator.hubtel.com> | The browser-based Programmable Service simulator (USSD, mobile, desktop) | docs page only; the simulator itself was not opened |
| <https://dev-docs.hubtel.com/backend/sdk/ussd-admin.html> | USSD Admin SDK: create and list USSD codes, session balance and top-up, base URL `https://api.hubtel.com/ussd` | in full |
| <https://news.hubtel.com/how-to-get-ussd-codes-in-ghana/> (WordPress post 5776, published 2019-08-01, modified 2023-08-01) | Dedicated, semi-dedicated and shared USSD codes, and how to get them | in full, through the WordPress API |
| <https://designs.hubtel.com/v3/docs/examples/messaging/reports.html> | A dashboard mock-up whose "Delivery Status Meanings" table lists Hubtel's SMS delivery statuses | in full |
| <https://github.com/hubtel/hubtel-sms-csharp> and <https://github.com/hubtel/hubtel-sms-java> | Hubtel's official but older SMS SDKs: Basic auth, the message and response models, send and get-by-id calls | `MessagingApi.cs`, `Message.cs`, `MessageResponse.cs`, `ApiHost.cs`, `BasicAuth.cs`, both READMEs |
| <https://github.com/KwabenBerko/hubtel-sms>, <https://github.com/idadzie/pyhubtel-sms> | **Community** wrappers: example send responses, `api.hubtel.com/v1/messages` URLs | READMEs and source |
| Search excerpts of `businessdocs-developers.hubtel.com/docs/{api-reference-messaging-1, api-reference-messaging, getting-started-with-sms, simple-messaging, authentication, api-reference-prog-services}` and `help.hubtel.com/hubtel-for-developers/sms-api/*` | Mentions of `POST /v1/messages/send` on `sms.hubtel.com`, mandatory `From`/`To`/`Content`, a mandatory `RegisteredDelivery` flag, `ClientReference` "sent back as part of delivery report notifications", Basic auth, a status lookup by `messageId`/`batchId` | **excerpts only; the pages were never opened** |

### Confirmed from Hubtel's own material

**USSD (Programmable Services)**

- Hubtel `POST`s JSON to the **Service Interaction URL** you register. Request fields: `Type` (`Initiation` first message,
  `Response` later message, `Timeout` "session canceled by the user", `Release` "last message in the session"; the SDK also
  knows `Query` and `Favorite`), `Platform` (`USSD`, `Webstore`, `Hubtel-App`), `Message`, `ServiceCode`, `Operator`
  (`tigo`, `airtel`, `mtn`, `vodafone`; the SDK also accepts `-gh` variants), `ClientState`, `SessionId`, `Sequence`. The
  samples and the SDK also carry `Mobile` (`233208074263`), and the samples a `device` field; the field table lists neither.
- The documentation prints keys in camelCase (`sessionId`, `type`) while the SDK sample uses PascalCase (`SessionId`,
  `Type`). The SDK parses either, as does Go's `encoding/json`.
- **`Message` is the user's latest entry only.** The sample session sends `"1"`, then `"2"`, then `"1"`, each as its own
  request, never `1*2*1`.
- On initiation, `Message` is the dialled shortcode in the field table and the SDK sample (`*711*23#`), but `""` in the
  documentation's sample request. `ServiceCode` is `*712#` in the docs and a GUID (`7c920dc4…`) in the SDK sample.
- The docs print `"type": "Response”` (a typographic closing quote) in three sample requests. That is a documentation typo;
  `tests/fixture_webhooks/hubtel_ussd_response.json` uses a plain quote.
- Reply: JSON with `Type` (`Response` keeps the session open, `Release` ends it, `AddToCart` starts checkout), `Message`
  (options separated by `\n`), and `SessionId`, `Label`, `DataType` (`display`, `input`, `menu`, `confirm`, `select`),
  `FieldType` (`text`, `phone`, `email`, `number`, `decimal`) marked mandatory, plus optional `ClientState`, `Item`, `Data`,
  `ServiceCode`, `Sequence`. The documented sample reply writes `"SessionId"` in PascalCase and the other keys in camelCase.
  The SDK's own replies carry no session id at all.
- A `Timeout` request "can be safely ignored, since a response does not affect user's interaction. Especially for USSD
  channel, this is a signal that your service took too long to respond" (SDK source comment). The SDK answers it with a
  `Release`.
- **Time limit:** "USSD responses must be delivered within 7 seconds" (SDK page), but "<= 5s for USSD applications" (SDK
  README). Assume 5 s. The 3.2 s fast path (`USSD_FAST_PATH_TIMEOUT_MS`) is inside both.
- **Callbacks are not authenticated by Hubtel.** None of the pages describes a signature, shared secret, header or source
  list, and the SDK's sample controller and middleware accept any `POST`.
- Hubtel provides a browser-based simulator at <https://programmable-simulator.hubtel.com>. It saves several service
  configurations, logs requests and responses, and warns about CORS for local endpoints (use a CORS-disabling extension or
  a tunnel such as ngrok). The request format it sends is not documented.
- The older Hubtel USSD framework was "discontinued since March 31st, 2021" and replaced by Programmable Services (SDK
  README).

**USSD codes**

- Three types (news post): a **dedicated** code is owned by one account, has one segment (`*714#`) and is the most
  expensive, bought through sales (`sales@hubtel.com`); a **semi-dedicated** code is dedicated to one account but shared
  with others, also through sales; a **shared** code is the cheapest and "most preferred", can be searched for and bought in
  a Hubtel account, and looks like `*713*20*xxx#`. All three can have "as many" extensions (sub-codes).
- Rental period and prices are **not** in anything that could be read. The "quarterly rental, extensions free" terms in the
  brief that started this work were not verified here. A search summary of an older SMSGH/Hubtel service agreement
  mentioned a 90-day lump-sum rental, automatic deactivation if renewal is not paid, and pay-as-you-go session charges,
  but that agreement itself was not located.

**SMS (older SDKs; see the warning above)**

- Authentication is HTTP Basic with the SMS API client ID and client secret (`BasicAuth.cs`: `Basic base64(id:secret)`). The
  Hubtel dashboard creates the key pair. A "Quick Send" variant puts the credentials in the query string; the adapter does
  not use it.
- A message has `From`, `To`, `Content`, `RegisteredDelivery` (request a delivery report), `ClientReference` ("a reference
  set by the client for uniquely identifying this message. The client reference will be sent as part of delivery report
  notifications"), `Type` (0 text, 1 binary, 2 Unicode), `Udh`, `FlashMessage`, `Time` (scheduling) and `BillingInfo`.
- The send response has `Status` (int), `MessageId` (GUID), `Rate`, `NetworkId` (e.g. `62002`), `ClientReference` and
  `Detail` (`MessageResponse.cs`). The C# SDK treats **HTTP 201** as success. A community wrapper prints
  `{'MessageId': 'f2665231-…', 'Rate': 1, 'NetworkId': '62002', 'Status': 0}`.
- A sent message can be fetched by **Hubtel's `MessageId`**: `GET …/{messageId}` returns `Status` as text, `Rate`,
  `Units`, `NetworkId`, `UpdateTime`. No lookup by `ClientReference` appears anywhere.
- Delivery statuses (dashboard table): `Rejected` (refused by the network), `Delivered` (handset acknowledged),
  `Undeliverable` (handset off or unreachable), `Pending` (not yet forwarded to the network), `Sent` (sent to the mobile
  network), `Failed` (failed Hubtel's checks: invalid number, blacklisted, sender ID, no route), `Acknowledged` (reached the
  Hubtel SMS gateway), `Reprocessing` (resent to the gateway).
- The SDK's `ApiHost` says: "Contact Hubtel support (tel:0307000577 or support@hubtel.com) for your base URL (e.g.
  `https://sms-api.hubtel-test.com/v1`)". The base URL may therefore be account-specific.

### How the adapter uses this

| Concern | Adapter behaviour |
|---|---|
| USSD authentication | Hubtel signs nothing, so the Service Interaction URL carries a shared secret: `https://<host>/webhooks/ussd/hubtel/session?token=<HUBTEL_USSD_TOKEN>`. Comparison is constant-time. Several comma-separated tokens are accepted for rotation. An optional `HUBTEL_USSD_ALLOWED_IPS` allowlist adds a second check. With no token configured every request is refused, and config validation fails when `PRIMARY_USSD_PROVIDER=hubtel`. The check runs before the body is read. |
| Delivery-report authentication | The same scheme with its own token, `https://<host>/webhooks/sms/hubtel/status?token=<HUBTEL_DLR_TOKEN>`, and `HUBTEL_DLR_ALLOWED_IPS`. The two tokens are separate, so a leaked report URL cannot drive USSD sessions. With no `HUBTEL_DLR_TOKEN`, every report is refused (`401`) and the app logs a warning at start-up. |
| USSD input | `Initiation` → `NewSession`, `Input = ""` (the dialled code is not a menu entry). `Response` → `Input = Message` as is, never split on `*`. `Release` and `Timeout` → `Input = "0"`, the engine's *Exit*, because `channel.USSDRequest` has no "ended" flag: the engine ends its session without booking a question or spending quota, and Hubtel ignores the reply. A release for an unknown or expired session makes the engine open a fresh session (3-minute TTL) that nobody uses. Other types (`Query`, `Favorite`, unknown) and other platforms (`Webstore`, `Hubtel-App`) are refused with `400`. |
| USSD identity | `SessionID = SessionId`. `FromE164 = Mobile` as received. `Network = lower(Operator)`. `ServiceCode` is left empty so the engine uses `USSD_SERVICE_CODE` on every step (Hubtel sends a shortcode in some samples and a GUID in others). |
| USSD reply | `{"type": "Response"\|"Release", "message", "SessionId", "clientState": null, "label": "SMS AI Ghana", "dataType": "input"\|"display", "fieldType": "text", "fieldName": "", "item": null, "data": null}`. Continue → `Response`; end → `Release`. The casing follows the documentation's sample reply. |
| MT SMS | `POST {HUBTEL_SMS_BASE_URL}/v1/messages/send`, HTTP Basic with client id/secret, JSON `{From, To, Content, RegisteredDelivery: true, ClientReference}`. Sends country-code digits without `+`, validates sender at 1–11 characters and stores Hubtel's returned `MessageId` (also accepted under `Data`). |
| Send error kinds | A transport failure before the request was fully written is Retryable; a timeout or connection loss after writing is Uncertain. `3xx` (not followed), `401`, `402`, `403`, `404`, `405`, `408`, `429` are Retryable; `5xx` are Uncertain; other `4xx` are Permanent. Unreadable or incomplete success responses are Uncertain. |
| Delivery reports | The format is unknown (see below), so `ParseStatus` accepts a JSON object, a JSON array, objects wrapped in `Data`, query parameters, or a form body, with case-insensitive keys. Each report needs a `MessageId` (the id from the send response) and a `Status`. Mapping: `Delivered`→delivered; `Sent`/`Pending`/`Acknowledged`/`Reprocessing`/`Queued`/`Accepted`→submitted; `Undeliverable`/`Failed`/`Undelivered`→failed; `Rejected`/`Blacklisted`→rejected; `Expired`→expired. Anything else becomes `unknown` and is logged and ignored. The raw status is kept as the error code on non-delivered outcomes. |
| Reconciliation | The adapter does not implement `channel.StatusQuerier`: Hubtel's published lookup uses its own `MessageId`, which an uncertain send never received; no lookup by our client reference is documented. Uncertain sends wait in `unknown_timeout` for an operator. |
| Inbound MO SMS | Unused and refused. Track A uses Hubtel for USSD ingress and MT SMS only. |

Fixtures in `tests/fixture_webhooks/hubtel_*.json` and where each came from:

| Fixture | Provenance |
|---|---|
| `hubtel_ussd_initiation.json`, `hubtel_ussd_response.json` | Copied from the sample requests on the "Building a Programmable Service" page (typo in `type` fixed) |
| `hubtel_ussd_initiation_sdk.json` | Copied from the SDK README / SDK page sample request |
| `hubtel_ussd_release_constructed.json`, `hubtel_ussd_timeout_constructed.json` | **Constructed.** The docs name `Release` and `Timeout` but print no sample |
| `hubtel_sms_send_response.json` | Shaped after the response fields in `MessageResponse.cs` and the community wrapper's example output |
| `hubtel_sms_status_callback_assumed.json` | **Assumed.** No delivery-report payload was found in anything readable |

### Not confirmed: ask Hubtel or verify with a real (test) account before the pilot

1. **The SMS API reference itself.** Exact host (`sms.hubtel.com`, `api.hubtel.com`, an account-specific one), path
   (`/v1/messages/send` vs `/v1/messages`), success status (`200` or `201`), whether `Status: 0` means accepted, and the
   error status codes and body format. The adapter treats every `2xx` with a `MessageId` as accepted and reads error text
   from `Message`, `Detail`, `Error` or `Response`.
2. **Delivery reports.** Whether the callback URL is set per message, per API key or in the dashboard (the adapter sends
   none, so register the URL with Hubtel), the HTTP method, the payload, whether `MessageId` or only `ClientReference` is
   included (a report without `MessageId` is refused with `400`), retries, and whether Hubtel keeps a query string in a
   registered URL. If Hubtel strips or rewrites it, the token has to move elsewhere (a path segment would need a new
   route in `internal/httpapi`).
3. **How callbacks are authenticated.** Nothing is documented. Ask whether Hubtel can send a header or sign requests, and
   publish its source IP ranges (needed for `HUBTEL_USSD_ALLOWED_IPS` and `HUBTEL_DLR_ALLOWED_IPS`).
4. **Registering the Service Interaction URL.** How a Programmable Service is created, who approves it, and whether a
   free question-and-answer service (no purchase, no `AddToCart`) is allowed on a platform described as a storefront for
   purchasable services. Whether the URL may contain a query string.
5. **USSD semantics.** The real response deadline (5 s or 7 s), maximum session length, maximum screen length and character
   set, what the handset shows when the endpoint returns `4xx`/`5xx`, and whether Hubtel retries a request.
6. **`Release` and `Timeout` requests.** The docs describe `Timeout` as "canceled by the user" while the SDK calls it a
   service-too-slow signal, and print no sample of either. What `Message` holds in them, and whether `Release` can arrive
   with an input that matters, is unknown. The adapter treats both as the end of the session.
7. **Initiation details.** Whether `Message` is empty or the dialled code, whether `ServiceCode` is a shortcode or a GUID,
   whether `Mobile` and `Platform` are always present, key casing, and how a dial-through string such as `*713*20*5#`
   arrives. The adapter ignores `Message` on initiation.
8. **`Sequence` and `ClientState`.** Not used. Whether skipping them loses anything (for example resumed sessions) is
   untested.
9. **The simulator.** Its request format and whether it can reach a token-protected URL. It is browser-based, our endpoint
   sends no CORS headers, and anything typed into its configuration is saved in the browser: use a throwaway token. Replay
   the fixtures with `curl` instead (see the README).
10. **Sender ID registration.** The 1–11 character check is the GSM alphanumeric limit, not a Hubtel rule. Approval process,
    allowed characters (is `SMS-AI` accepted?), and per-network (MTN, Telecel, AT) registration in Ghana are not documented
    in anything readable.
11. **Recipient format.** The adapter sends digits with the country code and no `+` (`233544919953`). The SDK examples use
    `+233…` and local `050…` forms. Confirm which the API accepts.
12. **Unicode.** The legacy `Type` field (0/1/2) selects text, binary or Unicode. The adapter sends none. Whether non-GSM
    text is detected automatically, and how it is billed per segment, is unconfirmed; keep `ALLOW_UCS2_LOCAL_LANGUAGE` in
    mind when estimating cost.
13. **Idempotency.** Whether Hubtel rejects, de-duplicates or ignores a repeated `ClientReference`, and whether messages can
    be searched by it (either would allow `channel.StatusQuerier`). The maximum length and allowed characters of
    `ClientReference` (ours is a 36-character UUID).
14. **Rate limits** on the send API, and the effect of `RegisteredDelivery` on price.
15. **Sandbox.** None is documented, so the adapter has no dry-run mode and config validation demands
    `SMS_ENVIRONMENT=live`. The `sms-api.hubtel-test.com` host in the C# SDK suggests a test environment exists. Ask.
16. **Commercial items from plan §6.1:** USSD code prices and rental period (dedicated, semi-dedicated, shared; sub-code
    cost), per-session USSD charges, the per-SMS price (about GHS 0.033 in the earlier research, unverified), whether
    two-way MO SMS shortcodes are available (the SDK has `MoKeyWord` and `NumberPlan` classes, not investigated), webhook
    latency commitments, and NCA authorisations covered by Hubtel.

### Operational risks

- **Callback shared-secret tokens.** Anyone who learns the USSD URL can drive sessions as any phone number until the token
  is rotated; anyone who learns the report URL can mark messages delivered or failed. Mitigations: 24+ character random
  tokens (48 hex characters is good), TLS only, separate tokens, an optional IP allowlist, and rotation through
  comma-separated values. The app never logs query strings (`logRequests` logs only the path), but **the TLS reverse proxy
  must not log query strings for `/webhooks/ussd/hubtel/*` and `/webhooks/sms/hubtel/*` either**, and **Hubtel's own
  dashboards and the browser-based simulator will store the full URL**. Move to signature checks if Hubtel ever offers them.
- **IP allowlist behind a proxy.** The allowlists check the TCP peer. Behind a reverse proxy, set `HUBTEL_CLIENT_IP_HEADER`
  (e.g. `X-Real-IP`) **only if the proxy overwrites that header**. Otherwise a client can forge it.
- **Every send is real.** Hubtel documents no sandbox, so a development stack with `PRIMARY_SMS_PROVIDER=hubtel` and a
  live key texts and bills real phones. Use `mock` locally and keep the Hubtel credentials out of development `.env` files.
- **Uncertain sends need a human.** Without a lookup by our reference, messages whose submission timed out stay in
  `unknown_timeout`. Check them in the Hubtel dashboard (its report table lists recipient, status and time) before
  requeueing.
- **Receipts may never arrive.** If Hubtel's callback is not configured, or sends a shape the parser does not recognise,
  messages stay `submitted`. Watch `webhook_rejected_total{provider="hubtel"}` and the "status not mapped" log line during
  the first tests.
- **Release handling is a workaround.** `Release` and `Timeout` are fed to the engine as the *Exit* entry because the
  channel contract has no way to say "the session ended". A cleaner fix is a field on `channel.USSDRequest` that the
  engine honours without writing a reply.
