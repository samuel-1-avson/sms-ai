# SMS AI Ghana

An AI you can text from any basic phone, with no mobile data. People text a normal phone number and get the answer
back by SMS.

**The phone is the server.** Two-way SMS from an aggregator (a number that both receives and replies) is hard to get in
Ghana, so the whole service runs on one Android phone with a SIM and an internet connection:

```text
person's phone (no data) ──SMS──▶ the SMS AI phone (app, online) ──HTTPS──▶ DeepInfra AI
                         ◀──SMS───                                ◀──answer──
```

The app receives the text, asks the AI over the internet, and texts the answer back from its own SIM. Only the SMS AI
phone needs data. There is no separate server, database or cloud machine to run.

## Status

- **Answering logic** (commands, GSM-7 clean-up, the `MORE` pager, PII scrubbing, the AI client with fallback and
  circuit breaker, daily limits, retries): ported to Kotlin from the tested TypeScript server, and checked against
  golden fixtures generated from that server plus its own tests (`pnpm test:phone`, 33 tests).
- **Android app:** type-checks; the APK is built with EAS.
- **Not yet done:** running it on a real phone. Follow the checks in [apps/gateway/README.md](apps/gateway/README.md).

## Layout

```text
apps/gateway/                       The Android app (Expo / React Native screens)
  modules/sms-gateway/android/…/    The native part (Kotlin) that runs in the background:
    brain/                            what to answer: AI client, prompt, commands, SMS formatting (pure Kotlin)
    GatewayService.kt                 the always-on service: answers texts and sends replies
    GatewayDb.kt                      the phone's database (inbox, outbox, people, conversations, daily totals)
  modules/sms-gateway/jvm/          Tests for brain/, run in Docker (no JDK needed)
data/                               Curated Ghana facts (not used by the phone yet)
scripts/test-phone.mjs              Runs the Kotlin tests
```

## How a text is answered

1. The SIM receives an SMS. The app writes it to the phone's database at once, so nothing is lost if the phone is
   offline or the app is killed.
2. After a 2.5 s pause (so a burst or a long text the network split arrives whole), the text is answered:
   - texts from network names and short codes ("MTN", "1515") are ignored, so the phone never replies to promos;
   - commands are answered on the phone with no AI call: `HELP`, `NEW` (fresh conversation), `MORE` (rest of the last
     answer), `STOP` / `START`, `DELETE` (forget my chats);
   - otherwise the question (with PINs, OTPs and card numbers removed) goes to DeepInfra (`openai/gpt-oss-20b`,
     falling back to Llama 3.3 70B) with the last few messages of the conversation.
3. The answer is cleaned to plain GSM-7 text and cut to one SMS (160 characters). Any rest waits for `MORE`.
4. The reply is sent from the SIM. Each reply has a fixed id, so a crash can never send it twice.

If the AI can't be reached (no internet, provider down), the phone keeps retrying. After 30 minutes it texts
"Sorry, I could not answer that right now…", which needs no data. Daily limits per person, on total SMS parts, and on
AI spend protect your airtime and credit; you can change them in the app.

## Costs

- **AI:** DeepInfra bills per token; a typical question costs a small fraction of a US cent with `gpt-oss-20b`. The app
  shows today's spend, and stops asking the AI at the daily cap (US$2 by default).
- **SMS:** every reply is a normal text from your SIM. Use an SMS bundle. The default cap is 200 SMS parts a day.
- Networks may flag a personal SIM that sends many automated texts. Keep the pilot small.

## Run it

See [apps/gateway/README.md](apps/gateway/README.md): build the APK with EAS, install it, paste your DeepInfra API
key, grant the permissions, keep the phone charging.

```bash
pnpm install
pnpm test:phone      # Kotlin tests in Docker
pnpm typecheck       # the app's TypeScript
make apk             # cloud build of the installable APK (EAS)
```

## History

- `main`: the original Go server that was meant to use an SMS aggregator.
- Tag `server-relay-v1`: a TypeScript server with this phone acting only as a relay (laptop or VM + Postgres). It was
  replaced by this phone-only design, which needs no server.
- [`sms_ai_ghana_system_architecture_and_build_plan_v2_0.md`](sms_ai_ghana_system_architecture_and_build_plan_v2_0.md):
  the original design document.
