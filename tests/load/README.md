# Load test

`tests/load` is a small Go program that drives a running stack with signed mock inbound SMS webhooks (plan §15
week 6). It sends `POST /webhooks/sms/mock/inbound` at a fixed rate from many distinct phone numbers, then reports:

- **Webhook ack latency:** p50, p95, p99 and max, from each request's scheduled send time to its response. The target
  is p95 < 500 ms.
- **End-to-end throughput:** answers submitted to the SMS gateway per minute, read from `GET /admin/usage` while the
  workers drain the queue. It also reports the best 30-second window and how long the queue took to empty after the
  last webhook.

The generator is open-loop: it keeps to its schedule however slowly the API answers. Latency is measured from the
scheduled time, so a slow API cannot hide its queueing behind a slowed-down generator.

## Running it

Everything runs in Docker. From the repository root:

```bash
docker compose -f deployments/compose.yaml -f tests/load/compose.load.yaml up -d --build
```

```bash
docker compose -f deployments/compose.yaml --profile load run --rm load -rate 20 -duration 1m
```

`compose.load.yaml` makes two changes to the dev stack:

- It lifts the daily caps (500 SMS segments and $15 by default). A load run would hit them within minutes, and later
  answers would then be counted as `blocked_cap` rather than submitted.
- It pins `LLM_PROVIDER=mock`, whatever `.env` says, so a load run never makes paid model calls.

To restore the caps and your `.env` settings, run `up -d` again without the overlay.

The `load` service runs `go run ./tests/load` on the compose network against `http://api:8080`. It reads
`MOCK_WEBHOOK_SECRET` and `ADMIN_API_TOKEN` from the same `.env` as the API. With Go installed, the program also runs
from the host:

```bash
go run ./tests/load -target http://localhost:8088 -rate 20 -duration 1m
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-rate` | 20 | Webhooks per second. |
| `-duration` | 1m | How long to send. |
| `-numbers` | 5000 | Distinct phone numbers (at most 100000). Texts cycle through them. |
| `-drain-timeout` | 5m | How long to wait for the queue to empty after sending. 0 skips the wait and the throughput figure. |
| `-max-p95` | 500ms | The run exits 1 when the ack p95 is above this, when a webhook fails, or when the queue does not drain. |
| `-max-inflight` | 512 | Cap on concurrent webhook requests. |
| `-target` | `$LOAD_TARGET` or `http://localhost:8088` | API base URL. |
| `-secret` | `$MOCK_WEBHOOK_SECRET` or `dev-mock-webhook-secret` | Mock webhook HMAC secret. |
| `-admin-token` | `$ADMIN_API_TOKEN` | Bearer token for `/admin`. Leave it blank for an open development stack. |
| `-allow-real-llm` | false | Run even when the stack uses a paid LLM provider. |

### Safety

- The program refuses to run against `APP_ENV=production`. The mock webhook route does not exist there anyway.
- It refuses to run when the stack's `LLM_PROVIDER` is not `mock`, because every webhook would become a paid model
  call. The overlay pins `mock`. To load-test a real model on purpose, remove that line and pass `-allow-real-llm`.
- Replies to mock inbound texts go through the mock gateway, so no real SMS is sent, whatever
  `PRIMARY_SMS_PROVIDER` is.
- Phone numbers are synthetic, picked from a random block per run. Each number has a daily question cap of 15, and
  the random block means repeated runs on one day rarely reuse numbers.
- Runs leave users, messages and jobs in the dev database, and the mock gateway keeps every sent SMS in the worker's
  memory. Restart the worker after very long runs.

### Reading the result

- `/admin/usage` counts one UTC day, so the program compares snapshots taken before and after the run. Avoid
  running across midnight UTC. Other traffic on the stack during a run, such as the simulator, is counted too.
- "Answers submitted" counts answer jobs whose result starts with `answered` and whose SMS the gateway accepted.
  `results` and `outbound` list every status that changed during the run.
- The end-to-end figure runs from the first webhook to the last answer. It includes the inbound debounce
  (`INBOUND_DEBOUNCE_MS`, 2.5 s) and the 2 s polling interval, so it reads a little low on short runs. The 30-second
  peak is the better capacity figure.

## Baseline

Recorded 2026-10-17 on the dev stack built from this tree, including the `ClaimJob` ordering fix:

- Host: Docker Desktop 29.8.2 on Windows 11 (WSL 2), 16 CPUs, 7.9 GB for Docker.
- Generator: ran on the compose network.
- Config: compose defaults plus `compose.load.yaml`. One API and one worker container, `WORKER_CONCURRENCY=4`,
  `LLM_PROVIDER=mock` with `MOCK_LLM_LATENCY_MS=400`, `INBOUND_DEBOUNCE_MS=2500`, mock delivery receipts posted back
  to the API.

| Offered load | Webhooks | Ack p50 | Ack p95 | Ack p99 | Ack max | Answered | End to end | Best 30 s | Queue empty after last webhook |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 5/s for 2 min | 600 | 10 ms | 15 ms | 18 ms | 34 ms | 600 of 600 | 290/min | 302/min | 4.2 s |
| 20/s for 1 min | 1,200 | 10 ms | 13 ms | 19 ms | 39 ms | 1,200 of 1,200 | 537/min | 560/min | 1 min 14 s |
| 100/s for 30 s | 3,000 | 11 ms | 21 ms | 34 ms | 141 ms | 3,000 of 3,000 | 542/min | 555/min | 5 min 2 s |

Every webhook was acked with 200, every question got exactly one answer, and every answer was delivered. No job
failed or was retried.

- **Webhook acks meet the target with a wide margin:** p95 stays at 13 to 21 ms, about 25 times under 500 ms, even at
  100/s. Acking is decoupled from answering. The inbound handler only stores the text and schedules a debounced job,
  so a growing backlog does not slow it.
- **End-to-end capacity is about 550 answers/min, set by the worker:** 4 goroutines each finish one answer per
  roughly 430 ms (400 ms of mock model plus about 30 ms of database and dispatch work), about 9.3 per second. At 5/s
  the queue keeps up and empties seconds after the last webhook. Above capacity the backlog drains at that same rate.
  Capacity scales with `WORKER_CONCURRENCY` (or more worker containers) and falls as model latency rises. Roughly,
  answers per minute ≈ 60 × `WORKER_CONCURRENCY` ÷ (model latency in seconds + 0.03).

Rerun the 20/s scenario after changes to the inbound path, the queue or the worker, and compare.
