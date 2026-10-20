// Claims and processes jobs: debounced answer generation, outbound SMS
// dispatch, reconciliation, gateway upkeep and retention housekeeping.
//
// With WORKER_METRICS_PORT set (e.g. 9091) it also serves GET /metrics: job,
// dispatch, AI and spend metrics are counted in this process.
import { createServer } from "node:http";
import { constantTimeEqual } from "@smsai/shared";
import { buildApp } from "../app";
import { loadConfig, validate } from "../config";
import { Worker } from "../worker";

async function main() {
  const cfg = loadConfig();
  const problems = validate(cfg);
  if (problems.length > 0) throw new Error("invalid configuration:\n  " + problems.join("\n  "));
  const app = await buildApp(cfg);
  const worker = new Worker(app);
  await worker.start();

  const metricsPort = Number(process.env.WORKER_METRICS_PORT ?? "");
  const metricsServer =
    metricsPort > 0
      ? createServer(async (req, res) => {
          if (req.url !== "/metrics") return void res.writeHead(404).end();
          const auth = req.headers.authorization ?? "";
          if (cfg.metricsToken && !constantTimeEqual(auth.replace(/^Bearer /, ""), cfg.metricsToken)) return void res.writeHead(401).end();
          res.writeHead(200, { "Content-Type": app.metrics.registry.contentType }).end(await app.metrics.registry.metrics());
        }).listen(metricsPort)
      : null;

  let stopping = false;
  const shutdown = async () => {
    if (stopping) return;
    stopping = true;
    metricsServer?.close();
    await worker.stop();
    await app.close();
    process.exit(0);
  };
  process.on("SIGINT", shutdown);
  process.on("SIGTERM", shutdown);
}

main().catch((err) => {
  console.error("worker:", err instanceof Error ? err.message : err);
  process.exit(1);
});
