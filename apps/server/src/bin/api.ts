// Serves the webhooks, the phone gateway protocol, the admin API and
// metrics. With EMBEDDED_WORKER=true it also runs the job worker
// in-process (handy for a single-container pilot or local development).
import { serve } from "@hono/node-server";
import { buildApp } from "../app";
import { httpPort, loadConfig, validate } from "../config";
import { createServer } from "../http/server";
import { Worker } from "../worker";

async function main() {
  const cfg = loadConfig();
  const problems = validate(cfg);
  if (problems.length > 0) throw new Error("invalid configuration:\n  " + problems.join("\n  "));

  const app = await buildApp(cfg);
  const server = await createServer(app);
  const worker = cfg.embeddedWorker ? new Worker(app) : null;
  await worker?.start();

  const port = httpPort(cfg);
  const http = serve({ fetch: server.app.fetch, port }, () => {
    app.log.info({ port, env: cfg.env, sms_provider: cfg.primarySmsProvider, embedded_worker: cfg.embeddedWorker }, "api listening");
  });

  let stopping = false;
  const shutdown = async () => {
    if (stopping) return;
    stopping = true;
    app.log.info("shutting down");
    // Long-polling phones are cut off; they reconnect to the next instance.
    await new Promise<void>((resolve) => {
      http.close(() => resolve());
      (http as { closeAllConnections?: () => void }).closeAllConnections?.();
    });
    await worker?.stop();
    await server.close();
    await app.close();
    process.exit(0);
  };
  process.on("SIGINT", shutdown);
  process.on("SIGTERM", shutdown);
}

main().catch((err) => {
  console.error("api:", err instanceof Error ? err.message : err);
  process.exit(1);
});
