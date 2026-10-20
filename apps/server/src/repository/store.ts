// PostgreSQL 16 persistence (postgres.js). The repository owns at-rest
// encryption: callers pass and receive plaintext, and every phone number,
// message body and overflow buffer is sealed with the keyring's current key
// version on write.
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import postgres from "postgres";
import { resolveAsset } from "../paths";
import { Purpose, type Keyring } from "../security/keyring";
import { Q, type DB } from "./queries";

export const JOBS_CHANNEL = "smsai_jobs";
export const GATEWAY_CHANNEL = "smsai_gateway";

export class Store {
  readonly sql: postgres.Sql;

  constructor(
    url: string,
    readonly keyring: Keyring,
    opts: { max?: number } = {},
  ) {
    this.sql = postgres(url, {
      max: opts.max ?? 10,
      onnotice: () => {},
      connection: { TimeZone: "UTC", application_name: "smsai" },
      // count(*) and other int8 results come back as JS numbers (they stay far below 2^53).
      types: { int8: { to: 20, from: [20], serialize: (x: number) => String(x), parse: (x: string) => Number(x) } },
      transform: { undefined: null },
    });
  }

  /** A non-transactional query handle. */
  q(): Q {
    return new Q(this.sql as unknown as DB, this.keyring);
  }

  /** Runs fn in a transaction, committing if it resolves. */
  async tx<T>(fn: (q: Q) => Promise<T>): Promise<T> {
    return (await this.sql.begin((tx) => fn(new Q(tx as unknown as DB, this.keyring)))) as T;
  }

  async ping(): Promise<void> {
    await this.sql`SELECT 1`;
  }

  async close(): Promise<void> {
    await this.sql.end({ timeout: 5 });
  }

  /** Subscribes to a NOTIFY channel; postgres.js re-listens after reconnects. */
  async listen(channel: string, fn: () => void): Promise<{ unlisten: () => Promise<void> }> {
    return this.sql.listen(channel, fn);
  }

  /**
   * Applies migrations/*.sql in order under an advisory lock, so concurrent
   * API and worker start-ups do not race. Returns the versions applied.
   */
  async migrate(dir = resolveAsset("migrations")): Promise<string[]> {
    if (!dir) throw new Error("migrations directory not found");
    const files = readdirSync(dir)
      .filter((f) => f.endsWith(".sql"))
      .sort();
    const applied: string[] = [];
    await this.sql.reserve().then(async (conn) => {
      try {
        await conn`SELECT pg_advisory_lock(hashtext('smsai_migrations'))`;
        await conn`CREATE TABLE IF NOT EXISTS schema_migrations (
          version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`;
        for (const file of files) {
          const version = file.replace(/\.sql$/, "");
          const [row] = await conn`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = ${version}) AS done`;
          if (row?.done) continue;
          const sqlText = readFileSync(path.join(dir, file), "utf8");
          await conn`BEGIN`;
          try {
            await conn.unsafe(sqlText);
            await conn`INSERT INTO schema_migrations (version) VALUES (${version})`;
            await conn`COMMIT`;
          } catch (err) {
            await conn`ROLLBACK`;
            throw new Error(`migration ${file}: ${(err as Error).message}`);
          }
          applied.push(version);
        }
      } finally {
        await conn`SELECT pg_advisory_unlock(hashtext('smsai_migrations'))`.catch(() => {});
        conn.release();
      }
    });
    return applied;
  }

  /** Re-encrypts every row still sealed with an older key version, in batches, so an old key can be retired. */
  async rekey(batch = 500): Promise<Record<string, number>> {
    const cur = this.keyring.current;
    const stats = { users: 0, messages: 0, overflow: 0, gateway_outbox: 0 };
    for (;;) {
      const n = await this.tx(async (q) => {
        const rows = await q.db<{ id: string; phone_ciphertext: Buffer; key_version: number }[]>`
          SELECT id::text, phone_ciphertext, key_version FROM users
          WHERE key_version <> ${cur} LIMIT ${batch} FOR UPDATE SKIP LOCKED`;
        for (const r of rows) {
          const phone = this.keyring.decryptString(Purpose.phone, r.phone_ciphertext, r.key_version);
          await q.rekeyUser(r.id, r.key_version, phone);
        }
        return rows.length;
      });
      stats.users += n;
      if (n < batch) break;
    }
    for (;;) {
      const n = await this.rekeyBlob("messages", "body_ciphertext", "key_version", Purpose.message, batch);
      stats.messages += n;
      if (n < batch) break;
    }
    for (;;) {
      const n = await this.rekeyBlob("conversations", "overflow_buffer", "overflow_key_version", Purpose.overflow, batch);
      stats.overflow += n;
      if (n < batch) break;
    }
    for (;;) {
      const n = await this.tx(async (q) => {
        const rows = await q.db<{ message_id: string; to_ciphertext: Buffer; body_ciphertext: Buffer; key_version: number }[]>`
          SELECT message_id::text, to_ciphertext, body_ciphertext, key_version FROM gateway_outbox
          WHERE key_version <> ${cur} LIMIT ${batch} FOR UPDATE SKIP LOCKED`;
        for (const r of rows) {
          const to = this.keyring.encrypt(Purpose.phone, this.keyring.decrypt(Purpose.phone, r.to_ciphertext, r.key_version));
          const body = this.keyring.encrypt(Purpose.message, this.keyring.decrypt(Purpose.message, r.body_ciphertext, r.key_version));
          await q.db`UPDATE gateway_outbox SET to_ciphertext = ${to.ciphertext}, body_ciphertext = ${body.ciphertext},
            key_version = ${cur} WHERE message_id = ${r.message_id}`;
        }
        return rows.length;
      });
      stats.gateway_outbox += n;
      if (n < batch) break;
    }
    return stats;
  }

  private rekeyBlob(table: string, col: string, verCol: string, purpose: Purpose, batch: number): Promise<number> {
    return this.tx(async (q) => {
      const rows = await q.db.unsafe<{ id: string; ct: Buffer; ver: number }[]>(
        `SELECT id::text, ${col} AS ct, ${verCol} AS ver FROM ${table}
         WHERE ${col} IS NOT NULL AND ${verCol} <> $1 LIMIT $2 FOR UPDATE SKIP LOCKED`,
        [this.keyring.current, batch],
      );
      for (const r of rows) {
        const sealed = this.keyring.encrypt(purpose, this.keyring.decrypt(purpose, r.ct, r.ver));
        await q.db.unsafe(`UPDATE ${table} SET ${col} = $2, ${verCol} = $3 WHERE id = $1`, [r.id, sealed.ciphertext, sealed.version]);
      }
      return rows.length;
    });
  }
}

/** The UTC calendar day of t as YYYY-MM-DD (Ghana uses GMT all year). */
export function dayOf(t: Date): string {
  return t.toISOString().slice(0, 10);
}
