# SMS AI (Android app)

This app turns an Android phone into an AI you can text. People text the phone's SIM number with no data. The phone
asks the AI over its own internet connection and texts the answer back from the same SIM.

## How it works

```text
 person ──SMS──▶ SIM ──▶ SmsReceiver ──▶ inbox (SQLite on the phone)
                                           │  answerer thread: commands, limits,
                                           │  then the AI over HTTPS (DeepInfra)
                                           ▼
 person ◀──SMS── SmsManager ◀── sender thread ◀── outbox (SQLite)
```

- **The answering runs in a native foreground service** (`modules/sms-gateway`, Kotlin), not in JavaScript. It keeps
  working with the app closed and the screen off, and restarts after a reboot. The React Native screens only set it up
  and show status.
- **Nothing is lost offline.** A text is saved to the phone's database before anything else happens. If the internet is
  down, the phone keeps retrying the AI. After 30 minutes it texts "Sorry, I could not answer that right now…".
- **No double replies.** Each reply's id comes from the text it answers, and the phone's outbox stores an id once. A
  reply that was being sent when the app was killed is never sent again.
- **Loop and spam guard:** texts from names (network promos like "MTN") and short codes are ignored. So is this phone's
  own number, if you enter it in **Settings**.
- **Dual SIM:** pick the SIM that answers in **Phone setup**. Texts to the other SIM are ignored.
- **The API key** is stored encrypted with an Android Keystore key and never leaves the phone except in calls to
  DeepInfra.

## What you need

- An Android phone (Android 7 or newer) that can stay plugged in. Its SIM needs airtime or an SMS bundle, and the phone
  needs Wi-Fi or mobile data.
- A DeepInfra account with credit and an API key (deepinfra.com → Dashboard → API keys).

The app cannot run in Expo Go, which can't use SMS. It also can't go on the Play Store, which only allows SMS
permissions for default SMS apps. You install the APK directly (sideload).

## Build and install

Build in the cloud with EAS. No Android Studio is needed:

```bash
cd apps/gateway
npx eas-cli@latest login
npx eas-cli@latest build -p android --profile preview      # an installable APK
```

Open the link EAS prints on the phone, download the APK and install it (allow "install unknown apps" for your browser).
To install a newer build, install it over the old one; your settings and key are kept.

## Set up the phone

1. **Grant permissions** when asked: receive SMS, send SMS, phone state (to list SIMs) and notifications.
2. **Settings → DeepInfra API key:** paste the key and tap **Save and test**. The app asks the AI "What is the capital
   of Ghana?". When the answer appears, the service starts and the app opens **Status**.
3. **Phone setup → Stay awake:** turn off battery optimisation for the app. On Tecno, Infinix, itel, Xiaomi and Samsung
   phones, also allow auto-start and remove the app from "sleeping apps". Without this, Android can stop the service at
   night.
4. **Lift Android's SMS limit.** Android asks for confirmation once an app sends about 30 texts in 30 minutes, which
   would stall replies. Do this once, from a computer with USB debugging on:

   ```bash
   adb shell settings put global sms_outgoing_check_max_count 100000
   adb shell settings put global sms_outgoing_check_interval_ms 60000
   ```

   Some phones reset this after a system update, so check it after updates.
5. **Leave it plugged in**, with signal and data. The persistent notification "SMS AI is running" shows it is alive. It
   changes to "SMS AI: problem", with the reason, when something is wrong.

## Check that it works

1. **Status → Try a question**: ask something. You see exactly what the first SMS would say. Nothing is sent.
2. From a second phone **with mobile data off**, text a question to this phone's number. The answer arrives as an SMS,
   usually within 5–15 seconds.
3. Text `MORE` after a long answer, `HELP`, then a follow-up such as "and its population?". The follow-up should use the
   earlier question.
4. Text `STOP`: further texts get no reply until you text `START`.
5. Swipe the app away and lock the screen, then text again: it still answers. Reboot: it starts by itself and answers.
6. Turn off this phone's Wi-Fi and data, then text a question. About 30 minutes later the "Sorry…" SMS arrives. Turn the
   data back on; new questions are answered.

## Limits and costs

All limits are in **Settings → Daily limits**. When one is reached, texts wait and are answered the next day.

| Limit | Default |
|---|---|
| Questions per person per day | 15 (the person gets one "Daily limit reached" text) |
| SMS parts sent per day, all people | 200 |
| AI spend per day | US$2 |

- Every reply is a normal SMS from your SIM. Long answers are split with `MORE`, one SMS per page.
- Networks may flag or bar a personal SIM that sends many automated texts. Keep the pilot small, keep **Phone setup →
  Sending pace** at 2 s or more, and talk to the network before scaling up.
- One phone handles a few texts a minute comfortably.

## Troubleshooting

| What you see | What to do |
|---|---|
| "AI: HTTP 401: the API key was rejected" | The key is wrong or was deleted. Paste it again in Settings. |
| "AI: HTTP 402: out of credit" | Top up your DeepInfra account. |
| "AI: UnknownHostException…" or timeouts | The phone has no internet. Texts wait; check Wi-Fi or data. |
| Replies fail with `no_service`, `radio_off` or `generic_failure` | No signal, airplane mode, or no airtime. |
| "This app is sending a large number of SMS messages" | Step 4 above was not done, or a system update reset it. |
| Nothing happens after a while with the screen off | Battery optimisation or the maker's sleeping-apps list stopped the app: step 3. |
| "Daily SMS cap reached" in Recent activity | Raise the cap in Settings, or wait for tomorrow. |

## Code

| Path | What |
|---|---|
| `App.tsx`, `src/` | Screens: Status (with "Try a question"), Settings (key, limits, models), Phone setup |
| `modules/sms-gateway/index.ts` | Typed bridge to the native module |
| `modules/sms-gateway/android/src/main/java/expo/modules/smsgateway/brain/` | What to answer, in pure Kotlin with no Android code: `Brain` (the decision for each text), `AiClient` (DeepInfra, fallback, breaker), `Prompt`, `Commands`, `Sanitize`/`Gsm7`/`Paginate` (SMS formatting), `Pii`, `Sender` |
| `modules/sms-gateway/android/src/main/java/expo/modules/smsgateway/` | `GatewayService` (the two loops), `GatewayDb` (the phone's database), `SmsSender`, receivers for incoming SMS, send results and boot, `GatewayPrefs` (settings, Keystore-sealed key), `SmsGatewayModule` (the JS API) |
| `modules/sms-gateway/jvm/` | JUnit tests for `brain/`, including fixtures generated from the TypeScript implementation it was ported from. Run them with `pnpm test:phone` from the repo root (uses Docker). |

The service uses the `remoteMessaging` foreground-service type. Unlike `dataSync`, Android 15 puts no daily time limit
on it and allows starting it at boot.
