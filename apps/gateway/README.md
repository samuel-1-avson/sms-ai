# SMS AI Gateway (Android)

This app turns an Android phone into the SMS front door for SMS AI Ghana. People text the phone's SIM number. The app
forwards each text to the server over the internet, and sends the AI's reply back from the same SIM. The people texting
need no data, only SMS.

## How it works

```text
 user's phone ──SMS──▶ gateway SIM ──▶ SmsReceiver ──▶ local inbox (SQLite)
                                                          │  POST /gateway/v1/inbound
                                                          ▼
                                                    server (apps/server) ──▶ AI
                                                          │  POST /gateway/v1/outbox (long-poll)
                                                          ▼
 user's phone ◀──SMS── SmsManager ◀── local outbox ◀── ack, then send
                                │
                                └── sent / delivered ──▶ POST /gateway/v1/status
```

- **The gateway runs in a native foreground service** (`modules/sms-gateway`, Kotlin), not in JavaScript, so it keeps
  working when the app is closed and restarts after a reboot. The React Native screens only pair the phone, show status
  and walk through setup.
- **Nothing is lost offline.** A received text is written to the phone's database before any network call, and goes up
  when the connection returns. Replies are saved locally before they are acknowledged to the server.
- **No double sends.** The phone's outbox is keyed by the server's message id, so a reply the server offers twice is
  stored once and sent once. A reply that was being sent when the app was killed is never retried; the server flags it
  for the operator instead.
- **Every request is signed** with the device secret (HMAC-SHA256 over the timestamp and the raw body, the same scheme
  as `packages/shared/src/signing.ts`). The secret is kept encrypted with an Android Keystore key.
- **Loop and spam guard:** the server ignores texts from names (network promos like "MTN"), short codes and the gateway's
  own number (`GATEWAY_PHONE_NUMBER`).
- **Dual SIM:** pick the SIM that answers in **Setup**; texts to the other SIM are ignored.

## What you need

- An Android phone (Android 7 or newer) that can stay plugged in, with a SIM that has airtime or an SMS bundle and a data
  connection (Wi-Fi or mobile data).
- The server running somewhere the phone can reach over HTTPS: a Cloudflare tunnel from your laptop while testing
  (`make tunnel`), then the pilot VM ([docs/DEPLOYMENT.md](../../docs/DEPLOYMENT.md)).

This app cannot run in Expo Go (Expo Go cannot touch SMS). It also cannot go on the Play Store, which only allows SMS
permissions for default SMS apps. You install it directly (sideload) as an APK.

## Build and install

You need a **development build** or an APK. Either:

**A. Build in the cloud with EAS** (no Android Studio needed):

```bash
cd apps/gateway
npx eas-cli@latest login
npx eas-cli@latest build -p android --profile preview      # an installable APK
```

Open the link EAS prints on the phone, download the APK and install it (allow "install unknown apps" for your browser).

**B. Build locally** (needs Android Studio, its SDK and JDK 17+):

```bash
cd apps/gateway
npx expo run:android --variant release    # phone connected over USB with USB debugging on
```

For development with hot reload, build the `development` profile instead and run `pnpm start`.

## Set up the phone

1. **Pair.** On the server, run `admin gateway-pair` (`make pair` locally, or
   `scripts/prod-compose.sh exec api node dist/admin.js gateway-pair` on the VM). It prints a QR code containing the
   server URL and the device secret. In the app, tap **Scan QR code**. The app tests the connection, then starts.
2. **Grant permissions** when asked: receive SMS, send SMS, phone state (to list SIMs) and notifications.
3. **Stay awake.** In **Setup → Stay awake**, turn off battery optimisation for the app. On Tecno, Infinix, itel,
   Xiaomi and Samsung phones also allow auto-start and remove the app from "sleeping apps". Without this, Android can
   stop the gateway at night.
4. **Lift Android's SMS limit.** Android asks for confirmation once an app sends about 30 texts in 30 minutes, which would
   stall the gateway. Connect the phone to a computer with USB debugging on, once:

   ```bash
   adb shell settings put global sms_outgoing_check_max_count 100000
   adb shell settings put global sms_outgoing_check_interval_ms 60000
   ```

   (Some phones reset this after an update; check after updates.)
5. **Leave it plugged in**, with signal and data. The persistent notification "SMS AI gateway is running" shows it is
   alive; it changes to "problem" with the reason when something is wrong.

Text the gateway number from another phone. Within a few seconds the reply arrives, and **Status → Recent activity**
shows "Text from +233…" and "Reply to +233…".

## Costs and limits

- Every reply is a normal SMS from your SIM. Long answers use `MORE` (one SMS per page). The server's
  `GLOBAL_DAILY_SMS_SEGMENT_CAP` (default 200) and `SMS_SEGMENT_COST_USD` cap and track what the SIM sends.
- Networks may flag or bar a personal SIM that sends many automated texts. Keep the pilot small, keep **Setup → Sending
  pace** at 2 s or more, and talk to the network before scaling up.
- One phone handles a few texts a minute comfortably. More traffic needs more phones (not supported yet) or a real
  two-way SMS provider.

## Troubleshooting

| What you see | What to do |
|---|---|
| "Server refused the pairing (401): pair again" | The device secret changed on the server. Run `gateway-pair` again, **Setup → Unpair**, scan. |
| `UnknownHostException`, `ConnectException`, timeouts | No data on the phone, or the server/tunnel is down. A quick tunnel's URL changes on every start: re-pair after restarting it. |
| Replies fail with `no_service`, `radio_off` or `generic_failure` | No signal, airplane mode, or no airtime. |
| "This app is sending a large number of SMS messages" | Step 4 above was not done (or was reset). |
| Nothing happens after a while with the screen off | Battery optimisation or the maker's sleeping-apps list stopped the app: step 3. |

Operator runbooks: [docs/RUNBOOKS.md](../../docs/RUNBOOKS.md#gateway-phone-offline).

## Code

| Path | What |
|---|---|
| `App.tsx`, `src/` | Screens: Pair (QR scan), Status, Setup |
| `modules/sms-gateway/index.ts` | Typed bridge to the native module |
| `modules/sms-gateway/android/src/main/java/expo/modules/smsgateway/` | `GatewayService` (the loops), `GatewayClient` (signed HTTP), `GatewayDb` (queues), `SmsSender`, receivers for incoming SMS, send results and boot, `GatewayPrefs` (Keystore-sealed secret), `SmsGatewayModule` (the JS API) |
| `modules/sms-gateway/android/src/main/AndroidManifest.xml` | Permissions, the `remoteMessaging` foreground service and the receivers (merged into the app; no config plugin) |

The service uses the `remoteMessaging` foreground-service type: unlike `dataSync`, Android 15 puts no daily time limit
on it and allows starting it at boot.
