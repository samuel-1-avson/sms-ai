// The job loop: claim (FOR UPDATE SKIP LOCKED), process, repeat. Workers
// wake on Postgres LISTEN/NOTIFY for immediate jobs and poll for debounced
// ones; maintenance runs once a minute.
import { randomBytes } from "node:crypto";
import { hostname } from "node:os";
import type { App } from "./app";
import { JOBS_CHANNEL } from "./repository/store";

export class Worker {
  readonly id = `${hostname()}-${randomBytes(4).toString("hex")}`;
  private stopping = false;
  private waiters: (() => void)[] = [];
  private loops: Promise<void>[] = [];
  private maintenanceTimer: NodeJS.Timeout | null = null;
  private unlisten: (() => Promise<void>) | null = null;

  constructor(private readonly app: App) {}

  async start(): Promise<void> {
    const { cfg, log } = this.app;
    log.info({ id: this.id, concurrency: cfg.workerConcurrency }, "worker started");
    const sub = await this.app.store.listen(JOBS_CHANNEL, () => this.wakeAll());
    this.unlisten = sub.unlisten;
    for (let i = 0; i < cfg.workerConcurrency; i++) this.loops.push(this.loop());
    const runMaintenance = () => void this.app.engine.maintenance().catch((err) => log.error({ err }, "maintenance"));
    runMaintenance();
    this.maintenanceTimer = setInterval(runMaintenance, 60_000);
  }

  /** Stops claiming new jobs and waits for in-flight ones. */
  async stop(): Promise<void> {
    this.stopping = true;
    if (this.maintenanceTimer) clearInterval(this.maintenanceTimer);
    this.wakeAll();
    await Promise.all(this.loops);
    await this.unlisten?.().catch(() => {});
    this.app.log.info({ id: this.id }, "worker stopped");
  }

  /** Claims and runs jobs until none is ready (tests drive the worker with this). */
  async drain(): Promise<number> {
    let n = 0;
    while (await this.runOne()) n++;
    return n;
  }

  private wakeAll() {
    const w = this.waiters;
    this.waiters = [];
    for (const fn of w) fn();
  }

  private sleep(): Promise<void> {
    return new Promise((resolve) => {
      const t = setTimeout(done, this.app.cfg.workerPollMs);
      function done() {
        clearTimeout(t);
        resolve();
      }
      this.waiters.push(done);
    });
  }

  private async loop(): Promise<void> {
    while (!this.stopping) {
      let ran = false;
      try {
        ran = await this.runOne();
      } catch (err) {
        this.app.log.error({ err }, "claiming job");
      }
      if (!ran && !this.stopping) await this.sleep();
    }
  }

  private async runOne(): Promise<boolean> {
    const { engine, store, cfg, log } = this.app;
    const job = await store.q().claimJob(this.id, cfg.jobLeaseSeconds);
    if (!job) return false;
    try {
      await engine.processJob(job);
    } catch (err) {
      log.error({ job: job.id, kind: job.kind, attempt: job.attempts, err }, "job failed");
      await engine.rescheduleAfterError(job, err).catch((rerr) => log.error({ job: job.id, err: rerr }, "rescheduling job"));
    }
    return true;
  }
}
