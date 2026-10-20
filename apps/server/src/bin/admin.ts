// Operator tasks:
//
//   admin genkey [--version N]          print fresh keys and tokens for .env
//   admin migrate                       apply database migrations
//   admin seed-grounding [--file path]  upsert grounding snippets (default: data/ghana_grounding_seed.yaml)
//   admin create-pool --code X --sponsor Y --max N --grant N [--expires YYYY-MM-DD]
//   admin flag ai_enabled|sms_outbound_enabled true|false
//   admin rekey [--batch N]             re-encrypt rows to CURRENT_KEY_VERSION
//   admin usage [--day YYYY-MM-DD]      print the day's usage report as JSON
//   admin send-webhook --url URL --file payload.json
//                                       POST a payload signed with MOCK_WEBHOOK_SECRET
//   admin gateway-pair [--url https://host]
//                                       print the QR code that pairs the Android gateway app
//   admin gateway-status                the phone's last heartbeat and outbox counts
import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { parseArgs } from "node:util";
import { signedHeaders, type Pairing } from "@smsai/shared";
import QRCode from "qrcode";
import { newKeyring } from "../app";
import { loadConfig } from "../config";
import { defaultSeedPath, loadSeedFile } from "../grounding";
import { Store, dayOf } from "../repository/store";

const randB64 = (n: number) => randomBytes(n).toString("base64");
const randHex = (n: number) => randomBytes(n).toString("hex");
const print = (v: unknown) => console.log(JSON.stringify(v, null, 2));

const USAGE =
  "usage: admin <genkey|migrate|seed-grounding|create-pool|flag|rekey|usage|send-webhook|gateway-pair|gateway-status> [options]";

async function main(cmd: string | undefined, args: string[]): Promise<void> {
  switch (cmd) {
    case "genkey": {
      const { values } = parseArgs({ args, options: { version: { type: "string", default: "1" } } });
      const v = Number(values.version);
      console.log(`CURRENT_KEY_VERSION=${v}`);
      console.log(`ENCRYPTION_KEYS_V${v}=${randB64(32)}`);
      console.log(`HMAC_KEYS_V${v}=${randB64(32)}`);
      console.log(`ADMIN_API_TOKEN=${randHex(24)}`);
      console.log(`METRICS_TOKEN=${randHex(24)}`);
      console.log(`MOCK_WEBHOOK_SECRET=${randHex(24)}`);
      console.log(`GATEWAY_DEVICE_SECRET=${randHex(24)}`);
      return;
    }
    case "send-webhook": {
      const { values } = parseArgs({
        args,
        options: { url: { type: "string", default: "http://localhost:8088/webhooks/sms/mock/inbound" }, file: { type: "string" } },
      });
      if (!values.file) throw new Error("--file is required");
      const body = readFileSync(values.file, "utf8");
      const secret = process.env.MOCK_WEBHOOK_SECRET || "dev-mock-webhook-secret";
      const res = await fetch(values.url!, { method: "POST", headers: signedHeaders(secret, body), body });
      console.log(`HTTP ${res.status}\n${await res.text()}`);
      return;
    }
    case "gateway-pair": {
      // Needs no database: it only encodes the URL and the device secret.
      const cfg = loadConfig();
      const { values } = parseArgs({ args, options: { url: { type: "string" } } });
      const url = (values.url ?? cfg.gatewayPublicUrl).replace(/\/+$/, "");
      if (!url) throw new Error("set GATEWAY_PUBLIC_URL or pass --url https://<public host>");
      const secret = cfg.gatewayDeviceSecrets[0];
      if (!secret) throw new Error("GATEWAY_DEVICE_SECRET is not set (admin genkey prints one)");
      const pairing: Pairing = { v: 1, url, secret };
      const text = JSON.stringify(pairing);
      console.log(await QRCode.toString(text, { type: "terminal", small: true, errorCorrectionLevel: "M" }));
      console.log(`Scan this in the gateway app (Pair). It contains the device secret: do not share or screenshot it.`);
      console.log(`Server: ${url}`);
      return;
    }
  }

  const cfg = loadConfig();
  const store = new Store(cfg.databaseUrl, newKeyring(cfg), { max: 2 });
  try {
    switch (cmd) {
      case "migrate": {
        const applied = await store.migrate();
        console.log(`applied ${applied.length} migration(s): ${applied.join(", ")}`);
        return;
      }
      case "seed-grounding": {
        const { values } = parseArgs({ args, options: { file: { type: "string" } } });
        const file = values.file ?? defaultSeedPath();
        if (!file) throw new Error("no seed file found; pass --file");
        const snippets = loadSeedFile(file);
        const n = await store.q().upsertSnippets(snippets);
        const reviewed = snippets.filter((s) => s.reviewed).length;
        console.log(`upserted ${n} snippet(s); ${reviewed} reviewed (only reviewed snippets are served outside development)`);
        return;
      }
      case "create-pool": {
        const { values } = parseArgs({
          args,
          options: {
            code: { type: "string" },
            sponsor: { type: "string" },
            max: { type: "string" },
            grant: { type: "string" },
            expires: { type: "string" },
          },
        });
        const max = Number(values.max);
        const grant = Number(values.grant);
        if (!values.code || !values.sponsor || !(max > 0) || !(grant > 0)) throw new Error("create-pool needs --code, --sponsor, --max and --grant");
        let expires: Date | null = null;
        if (values.expires) {
          if (!/^\d{4}-\d{2}-\d{2}$/.test(values.expires)) throw new Error("--expires must be YYYY-MM-DD");
          expires = new Date(`${values.expires}T23:59:59Z`);
        }
        print(await store.q().createPool({ voucher_code: values.code, sponsor_name: values.sponsor, max_segments: max, per_user_grant: grant, expires_at: expires }));
        return;
      }
      case "flag": {
        if (args.length !== 2 || !["true", "false"].includes(args[1]!)) {
          throw new Error("usage: admin flag <ai_enabled|sms_outbound_enabled> <true|false>");
        }
        await store.q().setFlag(args[0]!, args[1] === "true");
        console.log(`${args[0]}=${args[1]}`);
        return;
      }
      case "rekey": {
        const { values } = parseArgs({ args, options: { batch: { type: "string", default: "500" } } });
        const st = await store.rekey(Number(values.batch));
        console.log(`re-encrypted to v${cfg.currentKeyVersion}: ${JSON.stringify(st)}`);
        return;
      }
      case "usage": {
        const { values } = parseArgs({ args, options: { day: { type: "string", default: dayOf(new Date()) } } });
        print(await store.q().usageReport(values.day!, cfg.smsSegmentCostUsd, cfg.globalDailySpendCapUsd, cfg.globalDailySmsSegmentCap));
        return;
      }
      case "gateway-status": {
        print({ devices: await store.q().gatewayDevices(), outbox_last_24h: await store.q().gatewayCounts() });
        return;
      }
      default:
        console.error(USAGE);
        throw new Error(`unknown command "${cmd ?? ""}"`);
    }
  } finally {
    await store.close();
  }
}

main(process.argv[2], process.argv.slice(3)).catch((err) => {
  console.error("admin:", err instanceof Error ? err.message : err);
  process.exit(1);
});
