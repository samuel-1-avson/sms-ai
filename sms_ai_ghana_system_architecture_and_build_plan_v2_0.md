# SMS AI Ghana — System Architecture, Technical Design, and Build Plan

**Version:** 2.0 (Updated Architecture: Hybrid USSD/SMS, Cost & Reliability Hardening)  
**Prepared:** 17 October 2026  
**Project stage:** Pre-development / proof of concept  
**Initial market:** Ghana  
**Working title:** SMS AI Ghana (replace with the eventual product name)  
**AI provider:** DeepInfra (API key available to project owner; not embedded in this document)  
**Product type:** Two-way, conversational, text-only AI over SMS and USSD

> **Design principle:** A person with a basic mobile phone and mobile-network coverage can ask an AI a question and receive an answer by SMS or USSD, without using mobile internet on the phone. The service backend **does** require internet access to reach the telecom gateway and the DeepInfra API.

---

## What Changed in Version 2.0

1. **Hybrid USSD-Ingress + MT-SMS Architecture:** Added USSD (`*xxx#`) as a primary/parallel inbound channel alongside Two-Way MO/MT SMS. This solves the Ghanaian "zero main airtime" barrier (since shortcode SMS requires main balance rather than bundle credit), eliminates shared-shortcode keyword dropouts, bypasses long two-way shortcode procurement delays, and cuts outbound SMS costs by $60\%$–$80\%$ via inline USSD fast-path replies.
2. **Reasoning-Token Trap Prevention:** Added explicit safeguards for reasoning models (`openai/gpt-oss-20b` / `120b`) so chain-of-thought tokens do not exhaust low `max_tokens` budgets and return empty strings, plus added non-reasoning fast instruct benchmark candidates (`Llama-3.3-70B-Instruct-Turbo`, `Qwen2.5-72B-Instruct`).
3. **Inbound Multipart Coalescing & Debounce ($2.5\text{ s}$):** Added a $2.5\text{-second}$ hold window on inbound SMS jobs to coalesce split multipart handset texts and rapid two-message bursts before calling DeepInfra.
4. **Deterministic GSM-7 Sanitizer & Stateful `MORE` Buffer:** Replaced second-pass LLM re-prompting on length overflow with a deterministic Unicode-to-GSM-7 homoglyph mapper and a cached `overflow_buffer` in PostgreSQL (`..Txt MORE`), delivering continuation segments in $< 50\text{ ms}$ at zero extra AI cost.
5. **Local Ghana Grounding Store (Civic & Curriculum Cache):** Added a deterministic `knowledge_snippets` table (`pg_trgm` / keyword lookup) for verified Ghanaian civic, telco, and educational facts (e.g., NHIS `*929#`, Ghana Card linking `*400#`, ECG prepaid, WASSCE grading) to prevent hallucinations without requiring live web search.
6. **Cryptographic Key Versioning:** Added `key_version` and multi-key HMAC lookup support on `users` and `messages` to allow zero-downtime key rotation.
7. **Sponsored Quota & Voucher System:** Added `sponsored_pools` and `voucher_redemptions` tables (`CODE <VOUCHER>`) so NGOs, schools, and telco CSR programs can fund student SMS pools during peak periods like BECE and WASSCE.

---

## 1. Executive summary

SMS AI Ghana allows people to ask questions such as **"What is photosynthesis?"**, **"Explain Newton's first law"**, **"How do I renew my NHIS on my phone?"**, or **"What documents might I need before booking a flight?"** using any basic feature phone or smartphone in Ghana. The platform receives the question via a Ghanaian two-way SMS shortcode or a USSD session (`*xxx#`), checks a verified local Ghana knowledge cache, retrieves a small window of recent conversation context, requests a concise answer from a language model on DeepInfra, and returns the answer either inline on the USSD screen or via an SMS message.

This is **not** initially a booking agent, web browser, payments service, or automated task executor. It is a low-bandwidth, general-purpose AI assistant deliberately optimized for short, low-cost replies, constrained connectivity, and Ghanaian telecom realities.

### Architecture decision (recommended)

| Layer | Recommended starting choice | Why |
|---|---|---|
| User interface | Native SMS app and/or USSD dialer on any phone | No smartphone app or mobile data needed on user's device |
| Ghana messaging & USSD | **Dual-path:** (1) **USSD Ingress + Alphanumeric Outbound MT SMS** (via Arkesel or Hubtel) for fast launch & zero user airtime friction; (2) **eSMS Africa / Africa's Talking Two-Way SMS** | Shared USSD sub-codes provision rapidly in Ghana and cost users $0$ main airtime; Two-Way SMS supports direct texting where preferred |
| Language/backend | **Go 1.24+**; standard `net/http` or `chi`; `pgx` | Lightweight services, strong concurrency, low resource overhead, straightforward single-binary deployment |
| AI inference | **DeepInfra**, via OpenAI-compatible chat-completions API | Uses the project owner's existing API access; models can be switched by configuration |
| Initial model candidates | `openai/gpt-oss-20b` (with `reasoning_effort="low"`) vs. `meta-llama/Llama-3.3-70B-Instruct-Turbo` | Benchmark reasoning vs. non-reasoning instruct models on latency, token efficiency, and concise GSM-7 adherence |
| Quality fallback candidate | `openai/gpt-oss-120b` or `Qwen/Qwen2.5-72B-Instruct` | Comparison candidates for complex/ambiguous questions, subject to latency and cost |
| Local grounding | PostgreSQL `knowledge_snippets` with `pg_trgm` | Zero-hallucination, $0$-token answers for frequent Ghana civic, telco, and curriculum queries |
| Database | PostgreSQL 16+ | Durable conversation history, `MORE` overflow buffer, users, SMS/USSD events, vouchers, audit, billing metadata |
| Queue/worker | PostgreSQL-backed jobs with $2.5\text{ s}$ debounce **for MVP**; add Redis Streams when load justifies it | Reliable without extra infrastructure at small scale; easy scale-out path |
| Cache/rate limits | PostgreSQL ledger for MVP; Redis 7+ when needed | Fast hot-session caching, USSD session state, rate limits, short-lived locks |
| Admin / observability | Minimal Go admin API + Next.js dashboard later; OpenTelemetry, Prometheus, Grafana | Operability without introducing a complex UI in the first iteration |
| Infrastructure | Docker Compose locally; one small cloud VM for pilot; managed DB later | Low initial cost and reproducible environments |

**Important pricing and telco reality:** On Ghanaian networks (MTN, Telecel, AT), standard user SMS/voice bundles rarely cover messages sent to shortcodes—carriers deduct MO shortcode SMS from the user's **main airtime balance**. Because many students and feature-phone users keep GHS $0.00$ in main airtime to prevent background deductions, pure MO SMS can silently block adoption. Supporting **USSD ingress + Outbound (MT) SMS** removes this barrier for users while slashing operator SMS spend whenever answers are read directly inside the USSD session.

### Success criteria for the first pilot

1. A user on **MTN, Telecel, and AT** can ask a question via USSD or Two-Way SMS and receive an accurate answer within the target latency window.
2. A follow-up message retains enough context to answer correctly without dropping routing affinity.
3. Duplicate delivery webhooks and split multipart inbound messages do **not** trigger duplicate AI work or duplicate billable replies.
4. Default SMS replies fit **one GSM-7 SMS segment** ($\le 160$ characters), with longer answers cleanly buffered behind a zero-LLM-cost `MORE` command.
5. Frequent Ghanaian civic/utility prompts (e.g., NHIS, Ghana Card, ECG, WASSCE) return verified facts from the local grounding store without hallucination.
6. Errors, timeouts, exhausted credit, and unavailable providers are handled cleanly without blind retries.
7. Administrators can monitor message status, AI cost, SMS segment counts, USSD offload savings, failures, and sponsored voucher usage without reading raw user message content.
8. Rate limits, hard budget caps, and kill switches prevent a free pilot from creating unlimited financial liabilities.

---

## 2. Product vision and scope

### Target users

- Students (JHS, SHS/WASSCE, tertiary) who need short educational explanations, definitions, or revision support.
- People using inexpensive feature phones ("yam phones") or limited data packages.
- Small-business owners and market traders seeking quick calculations, definitions, civic procedure steps, or drafting help.
- Residents in areas where mobile internet connectivity or affordability is inconsistent but GSM voice/USSD/SMS works.

### Minimum viable product (MVP)

**Included**
- **Two-way SMS:** inbound MO prompts and outbound MT AI-generated answers.
- **Hybrid USSD + MT-SMS mode:** dial `*xxx#` to type a question for free; read fast answers directly on screen or receive/save longer answers via one-way outbound SMS (`SMS-AI`).
- **Chat history by phone number** with bounded context memory ($4$–$8$ turns, $24\text{-hour}$ active TTL).
- **Stateful `MORE` overflow buffer:** instant retrieval of the second half of an answer from PostgreSQL without re-calling the LLM.
- **Basic commands:** `HELP`, `NEW`, `MORE`, `STOP`, `START` (subject to provider semantics), `CODE <VOUCHER>` (for sponsored student/NGO quotas).
- **Local Ghana Grounding Store:** curated, verified snippet table for top Ghanaian civic, telecom, and curriculum queries.
- **Deterministic GSM-7 sanitizer & segment counter:** homoglyph normalization and strict segment control.
- **Inbound $2.5\text{ s}$ debounce & multipart coalescer:** merges split handset texts before AI execution.
- **Abuse/rate limits, safety checks, error replies, and hard budget caps.**
- **Delivery tracking, dashboards/logs, alerts, cryptographic key versioning, and data deletion/opt-out workflows.**
- **Test mode, provider sandbox, and a simulated SMS/USSD frontend for development.**

**Not included initially**
- Actual flight booking, airline inventory, mobile money payments, live web browsing, social media integration, or long-running agent tasks.
- A required user account or smartphone application.
- Voice calls (IVR), MMS, image analysis, or unrestricted access to live internet data.
- Unverified claims that AI answers are always correct or real-time.
- Paid consumer subscriptions or per-question billing to end users (monetization/sustainability in MVP uses operator budgets and optional NGO/school sponsored voucher pools).

### Explicit product behavior

- **General global query without live data:** A text such as *"Where can I book my next flight?"* receives general guidance: `"Compare flights on an airline website or trusted travel agency. Check passport, visa and baggage rules before paying."` Without a live travel integration, the bot must never fabricate current fares, schedules, or bookings.
- **Local Ghanaian civic/utility query:** A text such as *"How do I check my NHIS status?"* matches the verified Local Grounding Store and replies: `"Dial *929# on any network and select Check Validity, or visit an NHIS district office with your Ghana Card or NHIS card."`

---

## 3. Non-functional requirements and measurable targets

These are **design targets**, not performance claims; establish the real figures during pilot measurements.

| Dimension | MVP target | Later target / strategy |
|---|---|---|
| API availability | $99.5\%$ monthly service-side target | $99.9\%$ with redundant hosting; excludes carrier outages |
| Inbound webhook acknowledgment | $\text{p95} < 500\text{ ms}$ after receipt at our edge | $\text{p95} < 250\text{ ms}$ |
| USSD inline fast-path response | $\le 3.5\text{ s}$ total round-trip (returns answer inside USSD screen) | If AI takes $> 3.5\text{ s}$, close USSD gracefully and deliver via MT SMS |
| SMS inbound to submitted outbound reply | $\text{p50} < 8\text{ s}$ (including $2.5\text{ s}$ debounce); $\text{p95} < 25\text{ s}$ | Tune model, queue, and provider routing; monitor end-to-end separately |
| Delivered SMS reply latency | Measure by network (MTN, Telecel, AT) using carrier receipts | Optimize against observed carrier variation; never guarantee instant delivery |
| Default SMS response | $\le 160$ GSM-7 septets ($1$ segment); overflow stored in `MORE` buffer | Expand beyond $1$ automatic segment only if policy explicitly allows |
| Data integrity | Durable persistence, deduplication, transactional outbox, key versioning | Managed DB with point-in-time recovery and tested restores |
| Scaling | Validate $100$–$1,000$ questions/day for pilot | Load-test for $10\text{k}+$/day, then evaluate higher rates |
| User identity | Phone number (MSISDN) encrypted + keyed HMAC, scoped to channel | Additional identities and accounts only if justified |
| Security | TLS, secret management, signed webhooks, versioned encryption at rest | Dedicated secret manager, compliance audits, enhanced anti-abuse |
| Cost control | Hard daily USD/segment cap, per-number limits, USSD inline offload | Dynamic controls, sponsored voucher pools, unit economics dashboard |

**Critical distinction:** Overall SMS reply delay includes: handset/carrier submission, SMS provider inbound delivery, $2.5\text{ s}$ coalescing window, queue wait, DeepInfra generation, SMS provider outbound submission, and carrier delivery. Optimizing Go alone cannot eliminate SMS network delays—which is why the USSD inline fast path is so valuable.

---

## 4. System architecture

### 4.1 High-level block diagram (Dual-Channel: Two-Way SMS & Hybrid USSD)

```mermaid
flowchart TD
    U[User: basic feature phone] -->|Option A: Dial *xxx# Free| USSD_GW[Ghana USSD Gateway]
    U -->|Option B: Send MO SMS| SMS_GW[Two-Way SMS Provider / Shortcode]
    
    USSD_GW -->|Signed USSD Webhook| EDGE[HTTPS Ingress / Reverse Proxy]
    SMS_GW -->|Signed SMS Webhook| EDGE
    
    EDGE --> API[Go Webhook & Channel API]
    
    API -->|Fast USSD Path <= 3.5s| FAST[Sync Orchestrator with 3.2s Timeout]
    API -->|Async SMS or Slow USSD Fallback| DB[(PostgreSQL 16+)]
    
    DB -->|2.5s Debounce & Claim| WORK[Go AI Worker]
    
    FAST --> GROUND[Local Ghana Grounding Store]
    WORK --> GROUND
    
    GROUND -->|Cache Miss| CTRL[Policy, Quota, Context & Prompt Engine]
    CTRL --> AI[DeepInfra Chat Completions API]
    AI -->|Raw Answer| SAN[GSM-7 Homoglyph Sanitizer & MORE Splitter]
    GROUND -->|Verified Snippet| SAN
    
    SAN -->|Fits USSD & < 3.5s| USSD_GW
    SAN -->|Cache Overflow Remainder| DB
    SAN -->|Send 1-Segment MT SMS| OUT[Go Outbound SMS Adapter]
    
    OUT -->|Reply via Shortcode or Alphanumeric ID| SMS_GW
    SMS_GW -->|Mobile-Terminated SMS| U
    SMS_GW -->|Delivery Receipt Webhook| API
    
    API --> METRICS[Prometheus / Grafana / Alerts]
    WORK --> METRICS
```

**Ownership boundary:** The mobile operator and SMS/USSD gateway transport messages and sessions. Our application owns conversation state, the local Ghana grounding cache, the policy, AI requests, GSM-7 formatting, `MORE` overflow buffering, delivery tracking, privacy, reliability, and expense controls. DeepInfra performs model inference; it does not host our user history database.

### 4.2 Logical components

| Component | Responsibility | Key concerns |
|---|---|---|
| Channel ingress adapters (`sms`, `ussd`) | Normalize provider-specific SMS and USSD webhooks into internal contracts | Replace or combine providers (eSMS, Africa's Talking, Arkesel, Hubtel) without rewriting domain logic |
| Ingress security controller | Verify raw-body HMAC signature, parse request, validate sender, dedupe | Never trust unsigned provider-looking traffic |
| User & key-versioned crypto service | Map channel + normalized MSISDN to pseudonymous user via versioned HMAC | Support zero-downtime key rotation (`key_version`); phone possession is not legal identity |
| Command & `MORE` router | Handle `HELP`, `NEW`, `MORE`, `STOP`, `START`, `CODE` before AI | Serve cached `MORE` continuations in $< 50\text{ ms}$ with $0$ LLM calls |
| Abuse, budget & voucher controller | Per-user daily quotas, sponsored voucher balances, global spend caps | Free-to-use policy requires hard circuit breakers and exam-season surge protection |
| Inbound coalescer / debouncer | Hold new SMS jobs for $2.5\text{ s}$ to merge multipart handset splits or rapid bursts | Prevents $2\times$ LLM calls and $2\times$ MT SMS charges when a user texts a long question |
| Local Ghana grounding store | Match high-frequency Ghanaian civic/telco/curriculum queries via `pg_trgm` | Eliminates hallucinations on local facts (e.g., `*929#`, `*400#`) at $0$ token cost |
| Conversation repository | Fetch recent $4$–$8$ turns, trim token context, manage $24\text{h}$ TTL and $30\text{d}$ retention | Strict tenant isolation so one user's messages never leak to another |
| Prompt & LLM orchestrator | Assemble system prompt, history, and call DeepInfra with reasoning guards | Enforce `reasoning_effort="low"` or fast instruct model so tokens go to visible output |
| GSM-7 sanitizer & `MORE` splitter | Transliterate smart quotes/dashes to GSM-7, count septets, split at $\le 148$ chars | Stores remainder in `conversations.overflow_buffer` and appends `..Txt MORE` |
| Outbound SMS dispatcher | Submit 1-segment MT SMS, record provider ID, reconcile uncertain timeouts | Avoid duplicate billable sends when an HTTP submission times out |
| Delivery receipt consumer | Update delivered/failed states idempotently | Receipts may arrive out of order or be repeated |
| Admin & observability | Dashboards, incident alerts, kill switches, spend & voucher ledgers | Prefer aggregated metrics over raw message content |

### 4.3 Deployable units

**MVP (modular monolith; preferred):** One Go codebase containing the HTTP API and background worker, PostgreSQL 16+, optional Redis, and a reverse proxy (Caddy or Nginx). Use Docker Compose for local development. Run API and worker as separately scalable processes from the **same repository and container image**.

**Scale-out:** Add API replicas, worker replicas, PgBouncer connection pooling, Redis Streams for high-throughput queuing, and read replicas only after pilot measurements justify them. Avoid a premature microservices architecture.

---

## 5. End-to-end user workflows and sequences

### 5.1 Workflow A: Two-Way SMS with $2.5\text{ s}$ Coalescing & `MORE` Buffer

```mermaid
sequenceDiagram
    participant U as SMS User
    participant S as SMS Provider
    participant A as Go Webhook API
    participant D as PostgreSQL
    participant W as AI Worker
    participant L as DeepInfra

    U->>S: Text Part 1: "What is photosynthesis"
    S->>A: Signed inbound webhook (Msg 1)
    A->>D: Save Msg 1 + Enqueue Job (available_at = now + 2.5s)
    A-->>S: HTTP 200 OK (< 200ms)
    U->>S: Text Part 2 (1s later): "and why is sunlight needed?"
    S->>A: Signed inbound webhook (Msg 2)
    A->>D: Coalesce Msg 2 into pending Job & reset timer (+1.5s)
    A-->>S: HTTP 200 OK
    W->>D: Claim matured Job (FOR UPDATE SKIP LOCKED)
    W->>D: Check Local Ghana Grounding Store & Load 4-8 turns context
    W->>L: Request concise answer (with reasoning guard)
    L-->>W: 230-char response + token usage
    W->>W: Sanitize to GSM-7; split Part 1 (145 chars + "..Txt MORE") & Part 2 (85 chars)
    W->>D: Save response, store Part 2 in overflow_buffer, record outbound intent
    W->>S: Send 1-segment Outbound SMS (Part 1)
    S-->>W: Submission ID
    W->>D: Record submission ID
    S->>U: Deliver Part 1 SMS
    Note over U,D: Optional: User wants the rest of the answer
    U->>S: Text "MORE"
    S->>A: Signed inbound webhook
    A->>D: CommandRouter reads overflow_buffer (0 LLM calls, < 50ms)
    A->>S: Send 1-segment Outbound SMS (Part 2)
    S->>U: Deliver Part 2 SMS
```

### 5.2 Workflow B: Hybrid USSD Ingress + Inline Fast-Path / MT-SMS Fallback

```mermaid
sequenceDiagram
    participant U as User (Feature Phone)
    participant G as USSD Gateway (Arkesel/Hubtel)
    participant A as Go Channel API
    participant D as PostgreSQL
    participant L as DeepInfra
    participant S as Outbound MT SMS (Alphanumeric ID)

    U->>G: Dials *xxx# (Free)
    G->>A: Signed USSD Webhook (New Session)
    A-->>G: CON "SMS AI Ghana: Type your question (or 1=Help, 2=New):"
    U->>G: Enters "Explain Ohm's law"
    G->>A: Signed USSD Webhook (Input)
    A->>D: Check Grounding Store & Context; Start 3.2s Timer
    A->>L: Request concise answer
    alt Fast Response (<= 3.2 seconds)
        L-->>A: "Ohm's law states voltage equals current times resistance (V = I x R)."
        A->>D: Save turn (0 SMS cost!)
        A-->>G: CON "Ohm's law: V = I x R (Voltage = Current x Resistance).\n1. Ask follow-up\n2. Save to SMS\n0. Exit"
        G->>U: Displays answer on USSD screen
    else Slow Response (> 3.2 seconds) or User Presses "2. Save to SMS"
        A->>D: Promote to Async Worker Job
        A-->>G: END "Thinking... Your answer will arrive by SMS in a few seconds."
        G->>U: Closes USSD cleanly (No carrier timeout error)
        L-->>D: Worker completes generation & GSM-7 formatting
        D->>S: Dispatch 1-Segment MT SMS from "SMS-AI"
        S->>U: Delivers SMS to phone inbox
    end
```

### 5.3 Conversation continuation

1. Incoming: `What is photosynthesis?`
2. Outgoing: `Photosynthesis is how plants use sunlight, water and carbon dioxide to make food, releasing oxygen.`
3. Incoming: `Why do they need sunlight?`
4. Backend finds recent messages **for the same phone and service** and asks the model to resolve `they` as plants.
5. Outgoing: `Sunlight supplies the energy plants need to turn water and carbon dioxide into sugars.`

Context is **not** automatically remembered by the model API: it must be assembled from stored messages and passed along with the new question. Bound the context to the last **$4$–$8$ turns**, a strict token limit (e.g., $\le 800$ input tokens), and expire active context after $24$ hours of inactivity.

### 5.4 Error and failure behavior

| Situation | Behavior |
|---|---|
| Duplicate inbound webhook | Acknowledge with HTTP 200; do not re-run AI or send a second reply |
| Split multipart inbound SMS | Coalesce within the $2.5\text{ s}$ debounce window into a single prompt |
| Bad webhook signature | Reject with HTTP 401/403, log metadata, alert if repeated |
| Model times out during USSD | At $3.2\text{ s}$, close USSD gracefully (`END Sending answer by SMS...`) and finish via async MT SMS worker |
| Model times out during SMS | One bounded retry if safe; then concise fallback when budget and provider permit |
| DeepInfra quota exhausted | Trip circuit breaker; notify operator and send at most one brief outage notice per user window |
| Provider submission times out | Treat as **uncertain** (`status = 'unknown_timeout'`); reconcile via provider query API before retrying to prevent duplicate billable SMS |
| User exceeds daily free cap ($15$/day) | Send 1-time notice: `"Daily limit reached (15/15). Reply tomorrow or text CODE <voucher> if you have a school/study pass."` Do not respond to subsequent messages that day |
| Dangerous, illegal or sensitive request | Use a safety policy appropriate to all ages; short, non-graphic, supportive responses and referrals where appropriate |

---

## 6. SMS & USSD channel design (the most important product constraint)

### 6.1 Procurement strategy: Two-Way SMS vs. Hybrid USSD + MT SMS

An **alphanumeric sender ID** (e.g., `SMS-AI`) is for outbound MT messages only and cannot receive replies directly. However, when paired with a **USSD code** (`*xxx#`), an alphanumeric sender ID provides a complete, low-cost conversational loop while you validate or wait for a dedicated two-way SMS shortcode.

**Recommended procurement track (run in parallel in Week 1):**

1. **Track A — Fast Pilot Launch (USSD Ingress + Alphanumeric Outbound SMS):**
   - Procure a shared USSD sub-code (e.g., `*713*xxxx#` or `*920*xxxx#`) and register an Alphanumeric Sender ID (`SMS-AI`) with a Ghana aggregator (**Arkesel** or **Hubtel**).
   - **Why:** Fast activation (often days rather than months), works across MTN, Telecel, and AT even when users have GHS $0.00$ main airtime, maintains native USSD session affinity without keyword prefixes, and offloads $60\%$–$80\%$ of outbound SMS fees onto free USSD screens.
2. **Track B — Direct Two-Way SMS (Shared Keyword or Dedicated Shortcode):**
   - Evaluate **eSMS Africa** and **Africa's Talking** for two-way MO/MT SMS in Ghana.
   - Test whether shared-shortcode conversation affinity works across MTN, Telecel, and AT without requiring the user to prefix every follow-up with `ASK`.

**Provider candidates as of document date:**

| Candidate | Documented strengths | What must be confirmed before committing funds |
|---|---|---|
| **Arkesel** | Strong Ghana USSD session webhooks and reliable Ghana outbound MT SMS API + delivery callbacks | Confirm shared USSD sub-code monthly cost, session timeout limits across MTN/Telecel/AT, and inbound MO SMS availability if any |
| **Hubtel** | Established Ghana USSD programmable services and bulk/transactional MT SMS | Confirm USSD sub-code pricing, webhook latency, and whether two-way MO SMS shortcodes are available for general public pilots |
| **eSMS Africa** | Documented Two-Way SMS REST API, signed inbound webhooks (`X-Webhook-Signature`), shared shortcode/keyword handling, sandbox; lists Ghana | Confirm currently active Ghana shortcode/keyword, exact MTN/Telecel/AT carrier coverage, MO+MT tariffs, and reply affinity window |
| **Africa's Talking** | Lists Ghana SMS (bulk & shortcodes) and USSD; good developer sandbox | Confirm whether a true conversational MO+MT SMS route or USSD sub-code is faster/cheaper to provision for all three Ghana MNOs |

### 6.2 GSM-7 character limits, homoglyph sanitization, and the `MORE` buffer

- **GSM-7 encoding:** **$160$ septets** in a single segment, then **$153$ septets per segment** for concatenated multipart messages (due to the User Data Header). Note that GSM-7 extension characters (`^`, `{`, `}`, `\`, `[`, `~`, `]`, `|`, `€`) consume **$2$ septets** each.
- **Unicode / UCS-2 encoding:** **$70$ characters** in one segment, then **$67$ per segment** for concatenated messages.
- **The Homoglyph Trap:** LLMs frequently output typographic curly apostrophes (`’`), smart quotes (`“ ”`), em dashes (`—`), bullet dots (`•`), or non-breaking spaces. **A single curly apostrophe silently downgrades an entire $140\text{-character}$ message from $1$ GSM-7 segment to $3$ UCS-2 segments ($3\times$ the cost!).**

#### Deterministic GSM-7 Sanitizer & `MORE` Splitter Algorithm (`internal/formatting/`)

Instead of re-prompting the LLM when an answer exceeds $160$ characters (which doubles AI latency and token spend), the worker executes this deterministic pipeline in $< 1\text{ ms}$:

```text
raw model answer
  -> 1. Strip conversational filler prefixes ("Sure!", "Here is the answer:", markdown **, #)
  -> 2. Apply GSM-7 Homoglyph Map:
        ’ ‘ ` -> '
        “ ” « » -> "
        — – -> -
        … -> ...
        • · -> -
        × -> x
        ÷ -> /
        ≤ -> <=
        ≥ -> >=
  -> 3. Strip emojis and non-GSM-7 control chars (unless user requested a local language requiring UCS-2)
  -> 4. Count exact GSM-7 septets (counting extension chars [ ] { } \ ^ ~ | € as 2 septets each)
  -> 5. If septet_count <= 160:
        send as 1 segment; clear conversation.overflow_buffer
  -> 6. If septet_count > 160:
        find last sentence or word boundary at <= 148 septets
        part_1 = substring + " ..Txt MORE" (<= 160 septets total)
        part_2 = remaining substring (trimmed to <= 160 septets)
        save part_2 in conversations.overflow_buffer (TTL = 24h)
        send part_1 as 1 segment
```

**Supported User Commands:**
- `MORE` (or `M`): Immediately returns `conversations.overflow_buffer` from PostgreSQL and clears the buffer ($0$ LLM calls, $< 50\text{ ms}$ execution).
- `HELP`: Explains how to use the service, commands, privacy summary, and `Normal SMS rates may apply`.
- `NEW`: Resets active conversation context window (`context_expires_at = NOW()`).
- `CODE <VOUCHER>`: Redeems a school/NGO sponsored SMS quota code for the user's phone number.
- `STOP` / `START`: Opt-out and opt-in (respecting mandatory carrier/aggregator precedence).

---

## 7. AI model, local grounding, and orchestration design

### 7.1 Provider-neutral abstraction

```go
// Illustrative domain interface in internal/ai/provider.go
type ChatModel interface {
    Complete(ctx context.Context, req ChatRequest) (ChatResult, error)
}

type ChatRequest struct {
    Messages        []ChatMessage
    MaxOutputTokens int
    PreferredModel  string
    ReasoningEffort string // e.g., "low" or "" for non-reasoning models
    Temperature     float32
}
```

### 7.2 DeepInfra integration & avoiding the Reasoning-Token Trap

**Documented base URL:** `https://api.deepinfra.com/v1/openai`  
**Endpoint:** `POST /chat/completions`  
**Authentication:** `Authorization: Bearer $DEEPINFRA_API_KEY`

> **CRITICAL ENGINEERING SAFEGUARD (The Reasoning-Token Trap):**  
> Models in the `openai/gpt-oss-20b` and `openai/gpt-oss-120b` family are **reasoning models** that generate internal chain-of-thought tokens before emitting visible text. If you send `"max_tokens": 100` without constraining reasoning effort, the model can spend all $100$ tokens thinking internally and return an **empty string (`""`)** in `choices[0].message.content` while still adding $2$–$3\text{ seconds}$ of latency.
>
> **Mandatory rules in `DeepInfraAdapter`:**
> 1. When calling `gpt-oss-*` models, always pass `"reasoning_effort": "low"` and set `max_tokens` to `$350$` (allowing $\sim 150$ reasoning tokens + $\sim 60$ visible output tokens), relying on the system prompt and GSM-7 splitter to bound the SMS length.
> 2. Benchmark non-reasoning fast instruct models (`meta-llama/Llama-3.3-70B-Instruct-Turbo` and `Qwen/Qwen2.5-72B-Instruct`) where $100\%$ of generated tokens go directly to the visible answer with lower time-to-first-token (vital for the $\le 3.5\text{ s}$ USSD fast path).

```bash
# Development smoke test with reasoning_effort safeguard:
curl https://api.deepinfra.com/v1/openai/chat/completions \
  -H "Authorization: Bearer $DEEPINFRA_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "openai/gpt-oss-20b",
    "reasoning_effort": "low",
    "temperature": 0.2,
    "messages": [
      {"role": "system", "content": "Answer clearly in 1-2 plain ASCII sentences under 145 characters. No markdown, no emojis, no bullet points."},
      {"role": "user", "content": "What is photosynthesis?"}
    ],
    "max_tokens": 300
  }'
```

### 7.3 Model shortlist and benchmark matrix

| Model candidate | Type | Role | Key evaluation criteria |
|---|---|---|---|
| `meta-llama/Llama-3.3-70B-Instruct-Turbo` | Non-reasoning fast instruct | Primary low-latency candidate (ideal for USSD $\le 3.5\text{ s}$ path) | Zero wasted reasoning tokens, fast completion, strong adherence to character limits |
| `openai/gpt-oss-20b` (`reasoning_effort: "low"`) | Reasoning model | Primary low-cost candidate | Very low token price ($0.03$/M in, $0.14$/M out); verify empty-response rate is $0\%$ under low reasoning effort |
| `Qwen/Qwen2.5-72B-Instruct` | Non-reasoning instruct | STEM & calculation comparator | Strong math/science accuracy for SHS/WASSCE queries |
| `openai/gpt-oss-120b` (`reasoning_effort: "low"`) | Large reasoning model | Quality fallback for complex SMS prompts | Compare latency and accuracy against `gpt-oss-20b` |

### 7.4 Suggested system prompt (v2.0 — hardened for GSM-7 & `MORE` splitting)

```text
You are SMS AI Ghana, a concise, accurate text assistant for users in Ghana.
Write plain ASCII text only (standard straight quotes ' and ", standard hyphen -).
Never use markdown, bolding, bullet lists, numbered lists, or emojis.
Put the most important fact or direct answer in the very first sentence (under 145 characters).
If helpful detail is needed, add a second short sentence so total length stays under 260 characters.
Use conversation history only to resolve follow-up pronouns or context.
Do not invent live prices, flight schedules, news, exchange rates, or bookings; state clearly when live info is unavailable.
For health, legal, or financial topics, give general educational facts and advise consulting a qualified professional.
Decline harmful or illegal requests briefly. Never ask for PINs, MoMo OTPs, passwords, or Ghana Card numbers.
```

### 7.5 Local Ghana Grounding Store (Deterministic Civic, Telco & Curriculum Cache)

Because the MVP excludes live web search, generic LLMs often guess or hallucinate Ghanaian USSD shortcodes, government procedures, and exam grading scales.

We solve this by checking a local **`knowledge_snippets`** table in PostgreSQL (and caching it in memory in Go) **before** calling DeepInfra:
- **Contents ($100$–$300$ curated entries):**
  - **Civic & Public Services:** NHIS renewal & check (`*929#`), Ghana Card NIA verification (`*400#`), ECG prepaid & faults (`*226#`), Ghana Water, Passport online portal guidance, DVLA, GRA TIN verification.
  - **Telecom & MoMo Safety:** MTN/Telecel/AT official customer care numbers (`100`), MoMo reversal/fraud reporting procedures, warning never to share MoMo PIN/OTP.
  - **Ghana Education (GES / WAEC):** WASSCE grading scale (`A1` to `F9` definitions), BECE aggregate calculation, common core science/math formulas.
- **Execution Modes:**
  1. **Direct Hit (High-Confidence Match):** Returns the verified $< 160\text{-character}$ GSM-7 answer immediately in $< 20\text{ ms}$ with **$0$ DeepInfra token cost and $100\%$ factual accuracy**.
  2. **RAG Context Injection (Partial Match):** Injects the verified snippet into the system prompt (`Verified Ghana Fact: ...`) so the LLM can answer nuanced follow-up questions accurately.

---

## 8. Data architecture and schema (v2.0)

PostgreSQL 16+ is the single source of truth. All PII columns use **versioned encryption (`key_version`)** so encryption and HMAC secrets can be rotated online without downtime.

```mermaid
erDiagram
    USERS ||--o{ CONVERSATIONS : owns
    USERS ||--o{ VOUCHER_REDEMPTIONS : redeems
    SPONSORED_POOLS ||--o{ VOUCHER_REDEMPTIONS : funds
    CONVERSATIONS ||--o{ MESSAGES : contains
    MESSAGES ||--o{ SEND_ATTEMPTS : has
    MESSAGES ||--o{ MODEL_CALLS : may_trigger
    MESSAGES ||--o{ JOBS : generates

    USERS {
      uuid id PK
      bytea phone_ciphertext
      string phone_lookup_hash
      smallint key_version
      bool opted_out
      int bonus_sms_quota
      timestamp created_at
    }
    CONVERSATIONS {
      uuid id PK
      uuid user_id FK
      string service_id
      string channel_type
      string status
      text overflow_buffer
      bigint next_sequence
      string lease_owner
      timestamp lease_expires_at
      timestamp context_expires_at
    }
    MESSAGES {
      uuid id PK
      uuid conversation_id FK
      string provider_name
      string provider_message_id
      string direction
      bytea body_ciphertext
      smallint key_version
      string encoding_type
      int septet_or_char_count
      int segment_count
      string status
      timestamp created_at
    }
    JOBS {
      uuid id PK
      uuid conversation_id FK
      uuid latest_message_id FK
      string coalesced_prompt
      string kind
      string status
      int attempts
      timestamp available_at
    }
    KNOWLEDGE_SNIPPETS {
      uuid id PK
      string slug
      string category
      text trigger_keywords
      text verified_answer_gsm7
      bool direct_reply_enabled
      timestamp updated_at
    }
    SPONSORED_POOLS {
      uuid id PK
      string voucher_code
      string sponsor_name
      int max_segments
      int used_segments
      int per_user_grant
      timestamp expires_at
    }
    VOUCHER_REDEMPTIONS {
      uuid id PK
      uuid pool_id FK
      uuid user_id FK
      int granted_segments
      timestamp redeemed_at
    }
    MODEL_CALLS {
      uuid id PK
      uuid message_id FK
      string model_name
      int input_tokens
      int output_tokens
      int reasoning_tokens
      decimal estimated_cost_usd
      int latency_ms
    }
    SEND_ATTEMPTS {
      uuid id PK
      uuid message_id FK
      string provider_request_id
      string status
      timestamp created_at
    }
```

### Mandatory database constraints, indexes, and coalescing queries

```sql
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- 1. Prevent duplicate inbound webhook processing
CREATE UNIQUE INDEX ux_inbound_provider_message
  ON messages(provider_name, provider_message_id)
  WHERE direction = 'inbound' AND provider_message_id IS NOT NULL;

-- 2. Support zero-downtime HMAC key rotation lookups
CREATE INDEX ix_users_phone_lookup
  ON users(phone_lookup_hash, key_version);

-- 3. Fast recent context retrieval
CREATE INDEX ix_conversation_recent_messages
  ON messages(conversation_id, created_at DESC);

-- 4. Claimable jobs index (supports 2.5s debounce window)
CREATE INDEX ix_jobs_claimable
  ON jobs(status, available_at)
  WHERE status = 'pending';

-- 5. Fast trigram search on Local Ghana Grounding Store
CREATE INDEX ix_knowledge_snippets_trgm
  ON knowledge_snippets USING gin (trigger_keywords gin_trgm_ops);
```

### Inbound $2.5\text{ s}$ Debounce & Coalescing Strategy

When an inbound MO SMS webhook arrives:
1. Persist the message in `messages`.
2. Check if an unclaimed `pending` job already exists for `conversation_id`:
   - **If no pending job exists:** Insert a new row in `jobs` with `available_at = NOW() + INTERVAL '2500 milliseconds'`.
   - **If a pending job exists (and has been coalesced $< 3$ times):** Append the new message body to `jobs.coalesced_prompt` and extend `available_at = NOW() + INTERVAL '1500 milliseconds'`.
3. The worker polls `WHERE status = 'pending' AND available_at <= NOW() FOR UPDATE SKIP LOCKED`. This guarantees that a 2-segment inbound handset split or a quick two-line burst is answered in a **single** LLM call and a **single** outbound SMS.

---

## 9. API contracts and integration boundaries

### Internal normalized inbound event (SMS & USSD unified)

```json
{
  "channel": "sms",
  "provider": "esms_or_arkesel",
  "provider_event_id": "provider-generated-unique-id",
  "session_id": "optional-ussd-session-id",
  "service_id": "allocated-shortcode-or-ussd-code",
  "from_e164": "+233XXXXXXXXX",
  "body": "What is photosynthesis?",
  "received_at": "2026-10-17T09:00:00Z",
  "environment": "live"
}
```

### Suggested project-controlled endpoints

| Endpoint | Access | Purpose |
|---|---|---|
| `POST /webhooks/sms/{provider}/inbound` | Public URL, provider-signed | Receive MO SMS messages (enqueues with $2.5\text{ s}$ debounce) |
| `POST /webhooks/sms/{provider}/status` | Public URL, provider-signed | Receive MT SMS delivery receipts |
| `POST /webhooks/ussd/{provider}/session` | Public URL, provider-signed | Handle interactive USSD sessions ($\le 3.5\text{ s}$ fast path + MT SMS fallback) |
| `GET /healthz` | Infrastructure | Liveness check |
| `GET /readyz` | Internal infrastructure | Readiness check (DB connectivity, config validation) |
| `GET /metrics` | Restricted | Prometheus metrics, zero PII |
| `GET /admin/usage` | Authenticated admin | Spend ledger, USSD vs. SMS offload ratio, voucher pool usage |
| `POST /admin/simulate/channel` | Development only | Simulate SMS or USSD turns locally without carrier charges |

### Development configuration example (`.env.example`)

```dotenv
APP_ENV=development
HTTP_ADDR=:8080
DATABASE_URL=postgres://smsai:CHANGE_ME@postgres:5432/smsai?sslmode=disable

# Cryptographic Key Versioning (supports comma-separated rotation keys)
CURRENT_KEY_VERSION=1
ENCRYPTION_KEYS_V1=REPLACE_WITH_32_BYTE_BASE64_KEY
HMAC_KEYS_V1=REPLACE_WITH_32_BYTE_BASE64_SECRET

# Channel & Provider Settings
PRIMARY_SMS_PROVIDER=esms
PRIMARY_USSD_PROVIDER=arkesel
SMS_ENVIRONMENT=sandbox
SMS_SERVICE_ID=SET_AFTER_APPROVAL
SMS_OUTBOUND_SENDER_ID=SMS-AI
INBOUND_DEBOUNCE_MS=2500
USSD_FAST_PATH_TIMEOUT_MS=3200

# Provider Secrets
ESMS_API_KEY=REPLACE_VIA_SECRET_MANAGER
ESMS_WEBHOOK_SECRET=REPLACE_VIA_SECRET_MANAGER
ARKESEL_API_KEY=REPLACE_VIA_SECRET_MANAGER
ARKESEL_WEBHOOK_SECRET=REPLACE_VIA_SECRET_MANAGER

# DeepInfra AI Configuration
LLM_PROVIDER=deepinfra
DEEPINFRA_BASE_URL=https://api.deepinfra.com/v1/openai
DEEPINFRA_API_KEY=REPLACE_VIA_SECRET_MANAGER
LLM_MODEL=meta-llama/Llama-3.3-70B-Instruct-Turbo
LLM_FALLBACK_MODEL=openai/gpt-oss-20b
LLM_REASONING_EFFORT=low
LLM_MAX_OUTPUT_TOKENS=300

# Quotas & Financial Safeguards
DAILY_PER_NUMBER_FREE_CAP=15
GLOBAL_DAILY_SMS_SEGMENT_CAP=500
GLOBAL_DAILY_SPEND_CAP_USD=15.00
CONTEXT_TTL_HOURS=24
```

---

## 10. Security, privacy, trust, and Ghana-specific compliance

### 10.1 Threat model (Updated for v2.0)

| Threat | Countermeasure |
|---|---|
| Forged SMS or USSD webhooks | Verify provider HMAC signature/token on raw body before parsing JSON |
| Repeated webhook delivery | Unique `(provider_name, provider_message_id)` constraint + idempotent state transitions |
| Split multipart inbound SMS billing amplification | $2.5\text{ s}$ job coalescing window merges multipart segments before LLM execution |
| Unicode homoglyph billing amplification ($3\times$ segment cost) | Mandatory deterministic GSM-7 sanitizer converts curly quotes/dashes before sending |
| Reasoning-model empty response / token exhaustion | Enforce `reasoning_effort="low"` and benchmark non-reasoning instruct models |
| Encryption/HMAC key rotation downtime | `key_version` column on `users` and `messages`; dual-key HMAC lookup during rotation |
| Exam-season (BECE/WASSCE) quota exhaustion | Per-user daily caps ($15$/day), USSD inline offload ($0$ SMS cost), and sponsored school/NGO voucher pools |
| Prompt injection | No tool execution or operational secrets in model scope; strict output sanitizer |
| Duplicate outbound SMS on timeout | Record `send_attempts` intent first; mark timeouts as `unknown_timeout` and reconcile before retry |
| Phone-number recycling | Short $24\text{h}$ active context window, `NEW` reset command, no sensitive personal profiles stored |

### 10.2 Privacy and retention proposal

- Provide concise privacy notice via `HELP` and USSD menu option `1` so feature-phone users without internet access can read how their data is handled.
- Keep active model context for **$24\text{ hours}$** and encrypted message records for **$30\text{ days}$ or less** by default.
- Strip OTPs, 16-digit card numbers, and Ghana Card (`GHA-...`) patterns via regex before sending prompts to DeepInfra or persisting to disk.
- Support explicit user data deletion requests alongside the `NEW` context reset command.

### 10.3 Regulatory checkpoints (Ghana)

- **National Communications Authority (NCA):** Shortcodes, USSD codes, and Value-Added Services (VAS) fall under NCA numbering and VAS regulatory frameworks. Work through an NCA-licensed aggregator (e.g., Arkesel, Hubtel, eSMS Africa, Africa's Talking) and confirm which authorizations are covered by the aggregator versus the service owner.
- **Data Protection Commission (DPC — Act 843):** Register as a data controller as required, document lawful basis, retention schedule, and cross-border processing disclosures for cloud/AI inference.

---

## 11. Budgeting, unit economics, and USSD offload savings

### 11.1 Cost equation

$$\text{Monthly Cost} = \text{MO SMS Fees} + \text{MT SMS Segment Fees} + \text{Shortcode/USSD Rental} + \text{AI Token Spend} + \text{Infra}$$

At listed `gpt-oss-20b` rates ($\$0.03$ per $1\text{M}$ input tokens, $\$0.14$ per $1\text{M}$ output tokens), $750$ input tokens and $200$ output tokens cost:

$$\text{AI}_{\text{USD}} = \left(\frac{750}{1,000,000} \times 0.03\right) + \left(\frac{200}{1,000,000} \times 0.14\right) = \$0.0000505 \text{ per question}$$

Even on a fast $70\text{B}$ instruct model (approx. $\$0.12$–$\$0.40$ per $1\text{M}$ tokens), $10,000$ questions cost only **$\$1.50$–$\$3.50$** in total AI inference. **Messaging segments dominate unit economics by $> 98\%$.**

### 11.2 Sample expense comparison: Pure Two-Way SMS vs. Hybrid USSD + MT SMS ($10,000$ questions/month)

*(Illustrative planning assumptions: MO SMS billed to platform at $\$0.010$, MT SMS at $\$0.025$ per segment, $70\%$ of USSD queries answered inline within $3.2\text{ s}$ at $\$0.00$ SMS cost, and $20\%$ answered directly by the Local Ghana Grounding Store)*

| Expense item | Scenario A: Pure Two-Way SMS | Scenario B: Hybrid USSD Ingress + 30% MT SMS | Why Scenario B wins |
|---|---:|---:|---|
| Inbound fees to platform | $\$100.00$ | $\$0.00$–$\$20.00$ | Shared USSD sessions are typically bundled or flat-rate |
| Outbound MT SMS segments ($1$ seg enforced by `MORE` splitter) | $\$250.00$ ($10,000$ segs) | $\$75.00$ ($3,000$ segs) | $70\%$ of users read the answer directly on the USSD screen |
| AI token spend ($20\%$ Grounding Store cache hit rate) | $\sim \$0.40$–$\$2.50$ | $\sim \$0.40$–$\$2.50$ | Local Grounding Store answers $2,000$ civic/curriculum queries for $\$0.00$ |
| **Estimated variable subtotal** | **$\sim \$350.40+$** | **$\sim \$75.40$–$\$97.50$** | **$72\%+$ reduction in variable operating cost** |

### 11.3 Financial sustainability via Sponsored Voucher Pools (`sponsored_pools`)

- Free tier defaults to **$15$ questions/day per phone number** within the global daily spend cap.
- Schools, PTA groups, NGOs, foundations, or telecom CSR programs can fund a `sponsored_pool` (e.g., `CODE STEMGH` granting $100$ extra SMS segments to participating students during WASSCE prep).
- When a user texts `CODE STEMGH`, `VoucherService` validates the pool's remaining balance and increments `users.bonus_sms_quota` transactionally.

---

## 12. Performance and scaling strategy

### 12.1 Bottlenecks in expected order

1. **Mobile network + shortcode/USSD gateway latency and carrier session timeouts.**
2. **AI time-to-complete** (especially critical when racing the $3.2\text{ s}$ USSD inline deadline).
3. **SMS provider throughput, callback delays, and submission failure rate.**
4. **Cost per outbound segment** and daily budget exhaustion.
5. Database job contention / conversation ordering at scale.

### 12.2 Growth phases

| Phase | Load (planning range) | Architecture |
|---|---|---|
| **P0 Prototype** | Synthetic, $10$–$50$ test phones | Local Docker Compose, SMS/USSD simulator, DeepInfra, Local Grounding Store |
| **P1 Ghana Pilot** | $100$–$1,000$ questions/day | Single cloud VM, Go API + workers, PostgreSQL 16, Prometheus/Grafana |
| **P2 Regional Traction** | $1\text{k}$–$10\text{k}$ questions/day | Separate app/DB hosting, worker replicas, PgBouncer, Redis for USSD state & rate counters |
| **P3 National Scale** | $10\text{k}$–$100\text{k}$ questions/day | Managed PostgreSQL, Redis Streams queue, autoscaling workers, direct MNO/aggregator throughput SLAs |

---

## 13. Logging, telemetry, dashboards, and operations

### Required metrics

- `channel_inbound_total{channel="sms|ussd",provider,carrier}`
- `ussd_inline_delivery_total` vs. `ussd_async_sms_fallback_total` *(tracks exact SMS cost savings)*
- `grounding_store_hits_total{category}` vs. `grounding_store_misses_total`
- `sms_inbound_coalesced_messages_total` *(tracks how many multipart/burst texts were merged)*
- `sms_more_buffer_served_total` *(tracks how many `MORE` continuations were served from DB)*
- `sms_outbound_submitted_total{provider,encoding="gsm7|ucs2",segments}`
- `ai_requests_total{model,status}`, `ai_empty_reasoning_response_total` *(must remain $0$)*
- `ai_request_latency_seconds` ($\text{p50}/\text{p95}/\text{p99}$)
- `daily_spend_usd_total` vs. `daily_spend_cap_usd` and `voucher_segments_redeemed_total`

---

## 14. Proposed repository and module structure (v2.0)

```text
sms-ai-ghana/
├── README.md
├── docs/
│   ├── ARCHITECTURE.md
│   ├── API_CONTRACTS.md
│   ├── DATA_RETENTION.md
│   ├── SECURITY.md
│   ├── RUNBOOKS.md
│   └── PROVIDER_VALIDATION.md
├── data/
│   └── ghana_grounding_seed.yaml      # Verified Ghana civic, telco & WASSCE snippets
├── cmd/
│   ├── api/main.go
│   ├── worker/main.go
│   └── admin/main.go
├── internal/
│   ├── config/
│   ├── domain/                        # user, message, conversation, job, voucher, snippet
│   ├── httpapi/                       # routes, raw-body HMAC middleware, validators
│   ├── channel/
│   │   ├── sms/
│   │   │   ├── provider.go            # common SMS interface
│   │   │   ├── esms/                  # Two-way SMS candidate
│   │   │   ├── africastalking/        # Two-way SMS candidate
│   │   │   └── arkesel/               # Alphanumeric MT SMS + Two-way adapter
│   │   ├── ussd/
│   │   │   ├── provider.go            # common USSD session interface
│   │   │   ├── arkesel/               # Ghana USSD adapter
│   │   │   └── hubtel/                # Ghana USSD adapter
│   │   └── mock/                      # Local SMS & USSD terminal simulator
│   ├── grounding/                     # Local Ghana Grounding Store (pg_trgm + memory cache)
│   ├── ai/
│   │   ├── provider.go
│   │   ├── deepinfra/                 # Includes reasoning_effort & empty-output guards
│   │   ├── prompt/
│   │   └── evaluation/
│   ├── conversation/                  # Context window, lease fencing, MORE overflow_buffer
│   ├── commands/                      # HELP, NEW, MORE, STOP, START, CODE <VOUCHER>
│   ├── formatting/                    # GSM-7 homoglyph mapper, septet counter, MORE splitter
│   ├── policy/                        # Safety, regex PII scrubber, quotas, voucher ledger
│   ├── queue/                         # DB jobs with 2.5s inbound debounce & coalescing
│   ├── repository/                    # PostgreSQL 16 (pgx)
│   ├── security/                      # Versioned AES-GCM & multi-key HMAC rotation
│   ├── observability/
│   └── service/
├── migrations/
├── tests/
│   ├── integration/
│   ├── load/
│   ├── fixture_webhooks/
│   ├── ai_eval/
│   └── e2e/
├── deployments/
│   ├── docker/
│   ├── compose.yaml
│   └── monitoring/
├── .env.example
├── go.mod
└── Makefile
```

---

## 15. Development roadmap: first six weeks (v2.0)

| Window | Deliverable | Exit criteria |
|---|---|---|
| **Week 1** | Requirements freeze, Dual-Track provider inquiry (USSD + MT SMS via Arkesel/Hubtel AND Two-Way SMS via eSMS/AT), DeepInfra benchmark (`Llama-3.3-70B-Turbo` vs. `gpt-oss-20b`), core Go repo & DB migrations | Model benchmark completed with $0\%$ empty reasoning responses; schema supports key versioning, grounding store, and `MORE` buffer |
| **Week 2** | Signed SMS & USSD webhook handlers, versioned HMAC/AES crypto, $2.5\text{ s}$ inbound debounce/coalescer, local CLI/Web simulator | Split 2-part inbound SMS coalesces into 1 job; duplicate webhooks create only 1 job |
| **Week 3** | Local Ghana Grounding Store (`ghana_grounding_seed.yaml`), DeepInfra adapter, deterministic GSM-7 sanitizer, septet counter, and `MORE` splitter | Curly quotes/dashes never trigger UCS-2; $> 160\text{-char}$ answers split cleanly and serve `MORE` from DB in $< 50\text{ ms}$ |
| **Week 4** | Hybrid USSD fast-path ($\le 3.2\text{ s}$ inline + async MT SMS fallback), Two-Way SMS outbound adapter, `HELP`/`NEW`/`STOP`/`CODE`, voucher pool ledger | End-to-end USSD and SMS flows pass in sandbox; slow AI calls smoothly fall back from USSD to MT SMS |
| **Week 5** | Controlled live pilot with Ghanaian SIMs on **MTN, Telecel, and AT** (testing zero-main-airtime USSD + SMS delivery), telemetry tuning | Verified live delivery, tariff validation, USSD offload ratio, and carrier latency across all 3 networks |
| **Week 6** | Security hardening, key-rotation test, backup restore drill, NCA/DPC compliance checklist, release gate review | Go/no-go decision backed by measured unit economics and signed aggregator agreements |

---

## 16. Testing and acceptance plan (v2.0)

### Functional & Ghana-specific tests

- [ ] **Standard QA:** Send `What is photosynthesis?` via SMS or USSD; get a concise, $1\text{-segment}$ GSM-7 response.
- [ ] **Context Continuity:** Send follow-up `Why do they need sunlight?`; verify pronoun resolution works across turns.
- [ ] **Inbound Multipart Coalescing:** Send a 2-part question $1\text{ second}$ apart (`"Solve 2x + 5 = 15"` then `"and explain each step"`); verify only **one** DeepInfra call and **one** reply are generated.
- [ ] **GSM-7 Homoglyph Sanitizer:** Feed model output containing `’`, `“`, `”`, `—`, and `…`; verify output is converted to strict GSM-7 ($160\text{-septet}$ budget) and never billed as $70\text{-char}$ UCS-2.
- [ ] **Stateful `MORE` Buffer:** Trigger a $240\text{-character}$ answer; verify Part 1 ends with `..Txt MORE` ($\le 160$ septets) and texting `MORE` returns Part 2 from PostgreSQL with **$0$ LLM calls**.
- [ ] **USSD Fast Path vs. Slow Fallback:** Simulate a $1.8\text{ s}$ AI reply (renders directly inside USSD screen at $\$0$ SMS cost) and a $4.5\text{ s}$ AI reply (closes USSD cleanly at $3.2\text{ s}$ and delivers via outbound MT SMS).
- [ ] **Local Ghana Grounding Store:** Query `"How do I check my NHIS renewal?"` and `"How to link Ghana Card to MTN?"`; verify $100\%$ accurate, deterministic replies from `knowledge_snippets` with $0$ AI tokens spent.
- [ ] **Reasoning-Token Guard:** Run $100$ prompts against `gpt-oss-20b` with `reasoning_effort="low"`; verify $0$ empty string responses.
- [ ] **Key Rotation:** Rotate from `CURRENT_KEY_VERSION=1` to `2`; verify existing users are still matched via `HMAC_KEYS_V1` while new writes use `V2`.
- [ ] **Sponsored Voucher Redemption:** Exhaust the $15$/day free limit, text `CODE WASSCE2026`, and verify bonus quota unlocks immediately.
- [ ] **Zero Main Airtime Test:** Using a live Ghana SIM with **GHS $0.00$ main balance** (bundle-only), verify the user can dial the USSD code, ask a question, and receive both inline and MT SMS answers.

---

## 17. Provider due-diligence questions (SMS & USSD)

1. Can a Ghanaian user on **MTN, Telecel, and AT** initiate an interaction via **USSD (`*xxx*xxx#`)** and/or **Two-Way MO SMS**, even when their main airtime balance is GHS $0.00$?
2. For Two-Way SMS, is a **shared keyword shortcode** supported for ongoing conversations, and how long does session/affinity routing last if the user omits the keyword on turn 2?
3. For USSD, what is the exact **carrier session timeout** (in seconds) on MTN, Telecel, and AT before the network drops an awaiting webhook response?
4. What exact fees apply to our business for: (a) USSD session setup and monthly sub-code rental, (b) Inbound MO SMS, (c) Outbound MT SMS per segment (Alphanumeric Sender ID vs. Shortcode)?
5. Which NCA / VAS / Sender ID registration steps are handled by your platform versus our organization, and what is the exact activation timeline?
6. Does your platform supply **signed webhooks**, a **sandbox**, delivery receipts, opt-out events, and a **query-by-ID reconciliation endpoint** for timed-out HTTP submissions?

---

## 18. Architecture decision records (ADRs)

- **ADR-001:** Support basic feature phones with zero end-user mobile data requirement.
- **ADR-002:** **Hybrid USSD-Ingress + MT-SMS** alongside **Two-Way SMS** to eliminate user main-airtime friction, bypass shortcode delays, and cut outbound SMS costs by $60\%$–$80\%$.
- **ADR-003:** **Go** modular monolith with PostgreSQL 16+ and asynchronous workers.
- **ADR-004:** **DeepInfra** LLM integration with explicit reasoning-token guards (`reasoning_effort="low"`) and fast non-reasoning instruct model benchmarking.
- **ADR-005:** **Single-segment GSM-7 replies ($\le 160$ septets)** enforced via a deterministic homoglyph sanitizer and a PostgreSQL-backed `MORE` continuation buffer.
- **ADR-006:** **$2.5\text{ s}$ inbound SMS debounce window** to coalesce split multipart handset texts and rapid bursts before invoking AI.
- **ADR-007:** **Local Ghana Grounding Store** (`knowledge_snippets`) for zero-cost, zero-hallucination answers on Ghanaian civic, telco, and curriculum queries.
- **ADR-008:** **Versioned cryptographic keys** (`key_version`) on all encrypted and HMAC-hashed PII columns for zero-downtime key rotation.
- **ADR-009:** Free-to-use baseline with **strict operator spend caps** and **sponsored voucher pools (`CODE <VOUCHER>`)** for NGO/school funding.
- **ADR-010:** Minimize and time-limit conversation storage ($24\text{h}$ active context, $\le 30\text{d}$ retention); scrub OTPs/PINs before model calls or persistence.

---

## 19. Sources and verification notes

1. **DeepInfra gpt-oss-20b API and listed pricing:** https://deepinfra.com/openai/gpt-oss-20b/api
2. **DeepInfra gpt-oss-120b API and listed pricing:** https://deepinfra.com/openai/gpt-oss-120b/api
3. **DeepInfra OpenAI-compatible integration:** https://deepinfra.com/blog/glm-5-3-deepinfra
4. **eSMS Africa — two-way SMS keywords, messages & signed webhooks:** https://docs.esmsafrica.io/docs/api-reference/sms/two-way/keywords
5. **Africa's Talking — Ghana SMS shortcodes & USSD sandbox:** https://help.africastalking.com/en/articles/2727792-which-countries-are-africa-s-talking-products-in
6. **Arkesel — Ghana SMS & USSD developer APIs:** https://arkesel.com/developer-api/ghana/
7. **NCA Ghana — shortcode/special numbering & VAS:** https://nca.org.gh/numbering/ and https://nca.org.gh/vas/
8. **Ghana Data Protection Commission — Act 843 obligations:** https://dpc.gov.gh/for-organisations/
9. **Twilio — SMS encoding, GSM-7 (160/153) and UCS-2 (70/67) segment limits:** https://www.twilio.com/docs/glossary/what-sms-character-limit