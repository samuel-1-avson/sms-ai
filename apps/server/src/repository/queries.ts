// Queries, ported from the Go repository. A Q runs against the pool or
// inside a transaction (Store.tx).
import type postgres from "postgres";
import type { Snippet } from "../grounding";
import { Purpose, type Keyring } from "../security/keyring";

export type DB = postgres.Sql;

export class NotFoundError extends Error {
  constructor(what = "row") {
    super(`repository: ${what} not found`);
  }
}

// Message kinds.
export const Kind = {
  question: "question",
  command: "command",
  answer: "answer",
  more: "more",
  commandReply: "command_reply",
  notice: "notice",
} as const;

// Message statuses.
export const Status = {
  received: "received",
  rateLimited: "rate_limited",
  ignored: "ignored",
  overLimit: "over_limit",
  queued: "queued",
  sending: "sending",
  submitted: "submitted",
  delivered: "delivered",
  failed: "failed",
  expired: "expired",
  unknownTimeout: "unknown_timeout",
  blockedCap: "blocked_cap",
  blockedOff: "blocked_disabled",
} as const;

export const Flag = { aiEnabled: "ai_enabled", smsOutboundEnabled: "sms_outbound_enabled" } as const;

export type CreditSource = "free" | "bonus";

export interface User {
  id: string;
  keyVersion: number;
  carrier: string;
  optedOut: boolean;
  bonusQuota: number;
  createdAt: Date;
}

export interface Conversation {
  id: string;
  userId: string;
  serviceId: string;
  channel: string;
  provider: string;
  contextStartedAt: Date;
  contextExpiresAt: Date;
  hasOverflow: boolean;
}

export interface Job {
  id: string;
  conversationId: string;
  kind: "answer" | "send";
  status: string;
  latestMessageId: string | null;
  replyMessageId: string | null;
  coalesceCount: number;
  allowUcs2: boolean;
  creditSource: CreditSource | null;
  creditDay: string | null;
  attempts: number;
  maxAttempts: number;
  createdAt: Date;
}

export interface Overflow {
  rest: string;
  pages_left: number;
}

export interface DispatchInfo {
  id: string;
  conversationId: string;
  userId: string;
  convProvider: string;
  serviceId: string;
  provider: string;
  kind: string;
  body: string;
  status: string;
  encoding: string;
  segments: number;
  providerMessageId: string;
}

export interface Turn {
  role: "user" | "assistant";
  kind: string;
  content: string;
}

export interface Pool {
  id?: string;
  voucher_code: string;
  sponsor_name: string;
  max_segments: number;
  used_segments?: number;
  per_user_grant: number;
  active?: boolean;
  expires_at?: Date | null;
  created_at?: Date;
  redemptions?: number;
}

export class VoucherError extends Error {
  constructor(readonly reason: "invalid" | "expired" | "exhausted" | "used") {
    super(`voucher: ${reason}`);
  }
}

export interface SendAttempt {
  id: string;
  messageId: string;
  provider: string;
  requestId: string;
  providerMessageId: string;
  status: string;
  createdAt: Date;
}

export interface GatewayRow {
  id: string;
  to: string;
  body: string;
}

const userCols = (db: DB) => db`id::text, key_version, carrier, opted_out, bonus_sms_quota, created_at`;

type UserRow = { id: string; key_version: number; carrier: string; opted_out: boolean; bonus_sms_quota: number; created_at: Date };
const toUser = (r: UserRow): User => ({
  id: r.id,
  keyVersion: r.key_version,
  carrier: r.carrier,
  optedOut: r.opted_out,
  bonusQuota: r.bonus_sms_quota,
  createdAt: r.created_at,
});

type ConvRow = {
  id: string;
  user_id: string;
  service_id: string;
  channel_type: string;
  provider_name: string;
  context_started_at: Date;
  context_expires_at: Date;
  has_overflow: boolean;
};
const toConv = (r: ConvRow): Conversation => ({
  id: r.id,
  userId: r.user_id,
  serviceId: r.service_id,
  channel: r.channel_type,
  provider: r.provider_name,
  contextStartedAt: r.context_started_at,
  contextExpiresAt: r.context_expires_at,
  hasOverflow: r.has_overflow,
});
const convCols = (db: DB) => db`id::text, user_id::text, service_id, channel_type, provider_name, context_started_at, context_expires_at,
  (overflow_buffer IS NOT NULL AND coalesce(overflow_expires_at > now(), false)) AS has_overflow`;

type JobRow = {
  id: string;
  conversation_id: string;
  kind: "answer" | "send";
  status: string;
  latest_message_id: string | null;
  reply_message_id: string | null;
  coalesce_count: number;
  allow_ucs2: boolean;
  credit_source: CreditSource | null;
  credit_day: string | null;
  attempts: number;
  max_attempts: number;
  created_at: Date;
};
const toJob = (r: JobRow): Job => ({
  id: r.id,
  conversationId: r.conversation_id,
  kind: r.kind,
  status: r.status,
  latestMessageId: r.latest_message_id,
  replyMessageId: r.reply_message_id,
  coalesceCount: r.coalesce_count,
  allowUcs2: r.allow_ucs2,
  creditSource: r.credit_source,
  creditDay: r.credit_day,
  attempts: r.attempts,
  maxAttempts: r.max_attempts,
  createdAt: r.created_at,
});
const jobCols = (db: DB) => db`jobs.id::text, jobs.conversation_id::text, jobs.kind, jobs.status, jobs.latest_message_id::text,
  jobs.reply_message_id::text, jobs.coalesce_count, jobs.allow_ucs2, jobs.credit_source, jobs.credit_day::text,
  jobs.attempts, jobs.max_attempts, jobs.created_at`;

const orNull = (s: string | undefined | null) => (s ? s : null);

export class Q {
  constructor(
    readonly db: DB,
    private readonly kr: Keyring,
  ) {}

  private seal(purpose: Purpose, plaintext: string) {
    return this.kr.encrypt(purpose, plaintext);
  }

  private open(purpose: Purpose, ct: Uint8Array, version: number): string {
    return this.kr.decryptString(purpose, ct, version);
  }

  // -------------------------------------------------------------------------
  // Users
  // -------------------------------------------------------------------------

  /**
   * Resolves a phone number to its pseudonymous user. It matches the lookup
   * hash under every configured key version, so users written before a key
   * rotation are still found, and lazily re-keys them to the current version.
   */
  async findOrCreateUser(e164: string, carrier: string): Promise<{ user: User; created: boolean }> {
    const hashes = this.kr.lookupHashes(e164);
    const rows = await this.db<(UserRow & { phone_lookup_hash: string })[]>`
      SELECT ${userCols(this.db)}, phone_lookup_hash FROM users WHERE phone_lookup_hash = ANY(${hashes.map((h) => h.hash)}::text[])`;
    let found: User | null = null;
    for (const r of rows) {
      const u = toUser(r);
      if (hashes.some((h) => h.hash === r.phone_lookup_hash && h.version === u.keyVersion) && (found === null || u.keyVersion === this.kr.current)) {
        found = u;
      }
    }
    const current = this.kr.current;
    if (found) {
      if (found.keyVersion !== current) {
        await this.rekeyUser(found.id, found.keyVersion, e164);
        found.keyVersion = current;
      }
      return { user: found, created: false };
    }
    const sealed = this.seal(Purpose.phone, e164);
    const hash = this.kr.lookupHash(current, e164);
    const inserted = await this.db<UserRow[]>`
      INSERT INTO users (phone_ciphertext, phone_lookup_hash, key_version, carrier)
      VALUES (${sealed.ciphertext}, ${hash}, ${sealed.version}, ${carrier})
      ON CONFLICT (phone_lookup_hash, key_version) DO NOTHING
      RETURNING ${userCols(this.db)}`;
    if (inserted[0]) return { user: toUser(inserted[0]), created: true };
    // A concurrent request created the user first.
    const [existing] = await this.db<UserRow[]>`
      SELECT ${userCols(this.db)} FROM users WHERE phone_lookup_hash = ${hash} AND key_version = ${current}`;
    if (!existing) throw new NotFoundError("user");
    return { user: toUser(existing), created: false };
  }

  async rekeyUser(id: string, oldVersion: number, e164: string): Promise<void> {
    const sealed = this.seal(Purpose.phone, e164);
    const hash = this.kr.lookupHash(sealed.version, e164);
    await this.db`UPDATE users SET phone_ciphertext = ${sealed.ciphertext}, phone_lookup_hash = ${hash},
      key_version = ${sealed.version}, updated_at = now() WHERE id = ${id} AND key_version = ${oldVersion}`;
  }

  async getUser(id: string): Promise<User> {
    const [r] = await this.db<UserRow[]>`SELECT ${userCols(this.db)} FROM users WHERE id = ${id}`;
    if (!r) throw new NotFoundError("user");
    return toUser(r);
  }

  /** Looks a user up without creating one. */
  async findUserByPhone(e164: string): Promise<User | null> {
    for (const h of this.kr.lookupHashes(e164)) {
      const [r] = await this.db<UserRow[]>`
        SELECT ${userCols(this.db)} FROM users WHERE phone_lookup_hash = ${h.hash} AND key_version = ${h.version}`;
      if (r) return toUser(r);
    }
    return null;
  }

  /** Decrypts the user's E.164 number (needed to send SMS). */
  async userPhone(userId: string): Promise<string> {
    const [r] = await this.db<{ phone_ciphertext: Buffer; key_version: number }[]>`
      SELECT phone_ciphertext, key_version FROM users WHERE id = ${userId}`;
    if (!r) throw new NotFoundError("user");
    return this.open(Purpose.phone, r.phone_ciphertext, r.key_version);
  }

  async setOptOut(userId: string, optedOut: boolean): Promise<void> {
    await this.db`UPDATE users SET opted_out = ${optedOut},
      opted_out_at = CASE WHEN ${optedOut}::boolean THEN now() ELSE NULL END, updated_at = now() WHERE id = ${userId}`;
  }

  /**
   * DELETE command: removes every stored message and overflow buffer. The
   * pseudonymous user row, its opt-out flag and quota ledger stay so limits
   * cannot be reset this way.
   */
  async deleteUserContent(userId: string): Promise<void> {
    await this.db`UPDATE jobs SET status = 'cancelled', result = 'user_deleted', updated_at = now()
      WHERE status = 'pending' AND conversation_id IN (SELECT id FROM conversations WHERE user_id = ${userId})`;
    await this.db`DELETE FROM messages WHERE conversation_id IN (SELECT id FROM conversations WHERE user_id = ${userId})`;
    await this.db`UPDATE conversations SET overflow_buffer = NULL, overflow_key_version = NULL, overflow_expires_at = NULL,
      context_started_at = now(), context_expires_at = now(), updated_at = now() WHERE user_id = ${userId}`;
  }

  // -------------------------------------------------------------------------
  // Conversations
  // -------------------------------------------------------------------------

  /**
   * Gets or creates the conversation for (user, service, channel) and
   * extends its context window. If the window had lapsed a fresh context
   * starts now.
   */
  async touchConversation(userId: string, serviceId: string, channel: string, provider: string, ttlSeconds: number): Promise<Conversation> {
    const [r] = await this.db<ConvRow[]>`
      INSERT INTO conversations (user_id, service_id, channel_type, provider_name, context_started_at, context_expires_at)
      VALUES (${userId}, ${serviceId}, ${channel}, ${provider}, now(), now() + make_interval(secs => ${ttlSeconds}::float8))
      ON CONFLICT (user_id, service_id, channel_type) DO UPDATE SET
        provider_name      = EXCLUDED.provider_name,
        context_started_at = CASE WHEN conversations.context_expires_at < now() THEN now()
                                  ELSE conversations.context_started_at END,
        context_expires_at = now() + make_interval(secs => ${ttlSeconds}::float8),
        last_activity_at   = now(),
        updated_at         = now()
      RETURNING ${convCols(this.db)}`;
    return toConv(r!);
  }

  async getConversation(id: string): Promise<Conversation> {
    const [r] = await this.db<ConvRow[]>`SELECT ${convCols(this.db)} FROM conversations WHERE id = ${id}`;
    if (!r) throw new NotFoundError("conversation");
    return toConv(r);
  }

  /** NEW: history before now is no longer sent to the model, and any MORE buffer is dropped. */
  async resetContext(convId: string): Promise<void> {
    await this.db`UPDATE conversations SET context_started_at = now(), context_expires_at = now(),
      overflow_buffer = NULL, overflow_key_version = NULL, overflow_expires_at = NULL, updated_at = now() WHERE id = ${convId}`;
  }

  /** Stores (or clears, when empty) the MORE buffer. */
  async setOverflow(convId: string, o: Overflow, ttlSeconds: number): Promise<void> {
    if (!o.rest || o.pages_left <= 0) {
      await this.db`UPDATE conversations SET overflow_buffer = NULL, overflow_key_version = NULL,
        overflow_expires_at = NULL, updated_at = now() WHERE id = ${convId}`;
      return;
    }
    const sealed = this.seal(Purpose.overflow, JSON.stringify(o));
    await this.db`UPDATE conversations SET overflow_buffer = ${sealed.ciphertext}, overflow_key_version = ${sealed.version},
      overflow_expires_at = now() + make_interval(secs => ${ttlSeconds}::float8), updated_at = now() WHERE id = ${convId}`;
  }

  /** Reads the MORE buffer with a row lock; call inside a transaction and follow with setOverflow. */
  async lockOverflow(convId: string): Promise<Overflow | null> {
    const [r] = await this.db<{ overflow_buffer: Buffer | null; overflow_key_version: number | null; live: boolean }[]>`
      SELECT overflow_buffer, overflow_key_version, coalesce(overflow_expires_at > now(), false) AS live
      FROM conversations WHERE id = ${convId} FOR UPDATE`;
    if (!r) throw new NotFoundError("conversation");
    if (!r.overflow_buffer || r.overflow_key_version === null || !r.live) return null;
    const o = JSON.parse(this.open(Purpose.overflow, r.overflow_buffer, r.overflow_key_version)) as Overflow;
    return o.rest ? o : null;
  }

  // -------------------------------------------------------------------------
  // Messages
  // -------------------------------------------------------------------------

  private async nextSequence(convId: string): Promise<number> {
    const [r] = await this.db<{ seq: number }[]>`
      UPDATE conversations SET next_sequence = next_sequence + 1 WHERE id = ${convId} RETURNING next_sequence - 1 AS seq`;
    return r!.seq;
  }

  /**
   * Stores an inbound message. duplicate is true when the same
   * (provider, provider_message_id) was already stored: the webhook is a
   * retry and must not trigger any work (plan §5.4).
   */
  async insertInbound(m: {
    conversationId: string;
    provider: string;
    providerMessageId: string;
    kind: string;
    body: string;
    status: string;
  }): Promise<{ id: string; duplicate: boolean }> {
    const seq = await this.nextSequence(m.conversationId);
    const sealed = this.seal(Purpose.message, m.body);
    const rows = await this.db<{ id: string }[]>`
      INSERT INTO messages (conversation_id, sequence, provider_name, provider_message_id, direction, kind,
        delivery_channel, body_ciphertext, key_version, status)
      VALUES (${m.conversationId}, ${seq}, ${m.provider}, ${orNull(m.providerMessageId)}, 'inbound', ${m.kind},
        'sms', ${sealed.ciphertext}, ${sealed.version}, ${m.status})
      ON CONFLICT (provider_name, provider_message_id) WHERE direction = 'inbound' AND provider_message_id IS NOT NULL
      DO NOTHING
      RETURNING id::text`;
    return rows[0] ? { id: rows[0].id, duplicate: false } : { id: "", duplicate: true };
  }

  async setMessageStatus(id: string, status: string): Promise<void> {
    await this.db`UPDATE messages SET status = ${status}, updated_at = now() WHERE id = ${id}`;
  }

  async insertOutbound(m: {
    conversationId: string;
    jobId?: string;
    provider: string;
    kind: string;
    body: string;
    status: string;
    answerSource?: string;
    encoding: string;
    units: number;
    segments: number;
  }): Promise<string> {
    const seq = await this.nextSequence(m.conversationId);
    const sealed = this.seal(Purpose.message, m.body);
    const [r] = await this.db<{ id: string }[]>`
      INSERT INTO messages (conversation_id, sequence, job_id, provider_name, direction, kind, delivery_channel,
        body_ciphertext, key_version, encoding_type, septet_or_char_count, segment_count, answer_source, status)
      VALUES (${m.conversationId}, ${seq}, ${orNull(m.jobId)}, ${m.provider}, 'outbound', ${m.kind}, 'sms',
        ${sealed.ciphertext}, ${sealed.version}, ${orNull(m.encoding)}, ${m.units}, ${m.segments}, ${orNull(m.answerSource)}, ${m.status})
      RETURNING id::text`;
    return r!.id;
  }

  /** Loads an outbound message, locking it when forUpdate. */
  async getForDispatch(id: string, forUpdate: boolean): Promise<DispatchInfo> {
    const lock = forUpdate ? this.db`FOR UPDATE OF m` : this.db``;
    const [r] = await this.db<
      {
        id: string;
        conversation_id: string;
        user_id: string;
        conv_provider: string;
        service_id: string;
        provider_name: string;
        kind: string;
        body_ciphertext: Buffer;
        key_version: number;
        status: string;
        encoding: string;
        segment_count: number;
        provider_message_id: string;
      }[]
    >`
      SELECT m.id::text, m.conversation_id::text, c.user_id::text, c.provider_name AS conv_provider, c.service_id,
        m.provider_name, m.kind, m.body_ciphertext, m.key_version, m.status, coalesce(m.encoding_type, 'gsm7') AS encoding,
        m.segment_count, coalesce(m.provider_message_id, '') AS provider_message_id
      FROM messages m JOIN conversations c ON c.id = m.conversation_id
      WHERE m.id = ${id} AND m.direction = 'outbound' ${lock}`;
    if (!r) throw new NotFoundError("message");
    return {
      id: r.id,
      conversationId: r.conversation_id,
      userId: r.user_id,
      convProvider: r.conv_provider,
      serviceId: r.service_id,
      provider: r.provider_name,
      kind: r.kind,
      body: this.open(Purpose.message, r.body_ciphertext, r.key_version),
      status: r.status,
      encoding: r.encoding,
      segments: r.segment_count,
      providerMessageId: r.provider_message_id,
    };
  }

  async updateOutbound(id: string, status: string, providerMessageId = "", errorCode = ""): Promise<void> {
    await this.db`UPDATE messages SET status = ${status},
      provider_message_id = coalesce(${orNull(providerMessageId)}, provider_message_id),
      error_code = coalesce(${orNull(errorCode)}, error_code), updated_at = now()
      WHERE id = ${id}`;
  }

  /** Records a delivery receipt idempotently: stale receipts never move a message to a lower-ranked status. */
  async applyReceipt(provider: string, providerMessageId: string, status: string, errorCode = ""): Promise<boolean> {
    const res = await this.db`UPDATE messages SET status = ${status}, error_code = coalesce(${orNull(errorCode)}, error_code), updated_at = now()
      WHERE provider_name = ${provider} AND provider_message_id = ${providerMessageId} AND direction = 'outbound'
        AND message_status_rank(status) < message_status_rank(${status})`;
    return res.count > 0;
  }

  /**
   * Up to limit question/answer messages of the current context window,
   * oldest first, excluding messages that belong to the job being answered.
   */
  async recentTurns(convId: string, excludeJobId: string, since: Date, limit: number): Promise<Turn[]> {
    const rows = await this.db<{ direction: string; kind: string; body_ciphertext: Buffer; key_version: number }[]>`
      SELECT direction, kind, body_ciphertext, key_version FROM messages
      WHERE conversation_id = ${convId} AND created_at >= ${since}
        AND kind IN ('question', 'answer', 'more')
        AND (${orNull(excludeJobId)}::uuid IS NULL OR job_id IS DISTINCT FROM ${orNull(excludeJobId)}::uuid)
        AND status NOT IN ('rate_limited', 'ignored', 'over_limit', 'failed', 'blocked_cap', 'blocked_disabled')
      ORDER BY sequence DESC
      LIMIT ${limit}`;
    return rows
      .map((r) => ({
        role: (r.direction === "inbound" ? "user" : "assistant") as Turn["role"],
        kind: r.kind,
        content: this.open(Purpose.message, r.body_ciphertext, r.key_version),
      }))
      .reverse();
  }

  /** Joins the inbound texts coalesced into a job, in order. */
  async jobQuestion(jobId: string): Promise<string> {
    const rows = await this.db<{ body_ciphertext: Buffer; key_version: number }[]>`
      SELECT body_ciphertext, key_version FROM messages WHERE job_id = ${jobId} AND direction = 'inbound' ORDER BY sequence`;
    return rows
      .map((r) => this.open(Purpose.message, r.body_ciphertext, r.key_version).trim())
      .filter((s) => s !== "")
      .join(" ");
  }

  async attachToJob(messageId: string, jobId: string): Promise<void> {
    await this.db`UPDATE messages SET job_id = ${jobId} WHERE id = ${messageId}`;
  }

  /** A user's inbound messages since t (rate limit). */
  async countRecentInbound(userId: string, since: Date): Promise<number> {
    const [r] = await this.db<{ n: number }[]>`SELECT count(*)::int AS n FROM messages m JOIN conversations c ON c.id = m.conversation_id
      WHERE c.user_id = ${userId} AND m.direction = 'inbound' AND m.created_at >= ${since}`;
    return r!.n;
  }

  /** Recent outbound messages for a user (development simulator). */
  async outbox(userId: string, limit: number) {
    const rows = await this.db<
      {
        id: string;
        kind: string;
        body_ciphertext: Buffer;
        key_version: number;
        status: string;
        segment_count: number;
        encoding: string;
        source: string;
        created_at: Date;
      }[]
    >`
      SELECT m.id::text, m.kind, m.body_ciphertext, m.key_version, m.status, m.segment_count,
        coalesce(m.encoding_type, '') AS encoding, coalesce(m.answer_source, '') AS source, m.created_at
      FROM messages m JOIN conversations c ON c.id = m.conversation_id
      WHERE c.user_id = ${userId} AND m.direction = 'outbound'
      ORDER BY m.created_at DESC, m.sequence DESC LIMIT ${limit}`;
    return rows.map((r) => ({
      id: r.id,
      kind: r.kind,
      body: this.open(Purpose.message, r.body_ciphertext, r.key_version),
      status: r.status,
      segments: r.segment_count,
      encoding: r.encoding,
      answer_source: r.source,
      created_at: r.created_at,
    }));
  }

  // -------------------------------------------------------------------------
  // Jobs
  // -------------------------------------------------------------------------

  /**
   * The inbound debounce/coalescing strategy (plan §8): with no pending
   * answer job, insert one available after debounce; otherwise attach the
   * message to the pending job and push its start back by extend (at most
   * maxCoalesce times, so a chatty sender cannot postpone the answer
   * forever). Must run in the same transaction as insertInbound, which locks
   * the conversation row and so serialises concurrent webhooks per user.
   */
  async enqueueAnswer(
    convId: string,
    messageId: string,
    o: { debounceMs: number; extendMs: number; maxCoalesce: number; maxAttempts: number; allowUcs2: boolean },
  ): Promise<{ jobId: string; coalesced: boolean }> {
    const [pending] = await this.db<{ id: string; coalesce_count: number }[]>`
      SELECT id::text, coalesce_count FROM jobs
      WHERE conversation_id = ${convId} AND kind = 'answer' AND status = 'pending' FOR UPDATE`;
    let jobId: string;
    let coalesced = false;
    if (pending) {
      const extend = pending.coalesce_count >= o.maxCoalesce ? 0 : o.extendMs / 1000;
      await this.db`UPDATE jobs SET
          coalesce_count = coalesce_count + 1,
          available_at = GREATEST(available_at, now() + make_interval(secs => ${extend}::float8)),
          latest_message_id = ${messageId}, allow_ucs2 = allow_ucs2 OR ${o.allowUcs2}::boolean, updated_at = now()
        WHERE id = ${pending.id}`;
      jobId = pending.id;
      coalesced = true;
    } else {
      const [r] = await this.db<{ id: string }[]>`
        INSERT INTO jobs (conversation_id, kind, status, latest_message_id, allow_ucs2, max_attempts, available_at)
        VALUES (${convId}, 'answer', 'pending', ${messageId}, ${o.allowUcs2}, ${o.maxAttempts},
          now() + make_interval(secs => ${o.debounceMs / 1000}::float8)) RETURNING id::text`;
      jobId = r!.id;
    }
    await this.attachToJob(messageId, jobId);
    return { jobId, coalesced };
  }

  /** Queues delivery of an already stored outbound message. */
  async createSendJob(convId: string, messageId: string, maxAttempts: number): Promise<string> {
    const [r] = await this.db<{ id: string }[]>`
      INSERT INTO jobs (conversation_id, kind, status, reply_message_id, max_attempts, available_at)
      VALUES (${convId}, 'send', 'pending', ${messageId}, ${maxAttempts}, now()) RETURNING id::text`;
    return r!.id;
  }

  /**
   * Leases the next runnable job (FOR UPDATE SKIP LOCKED): pending and
   * matured, or running with an expired lease (crashed worker or scheduled
   * retry). Answer jobs for one conversation run one at a time so replies
   * keep their order; see the Go ClaimJob comment for the race this closes.
   */
  async claimJob(owner: string, leaseSeconds: number): Promise<Job | null> {
    const [r] = await this.db<JobRow[]>`
      WITH next AS (
        SELECT j.id FROM jobs j
        WHERE ((j.status = 'pending' AND j.available_at <= now())
            OR (j.status = 'running' AND j.lease_expires_at < now() AND j.attempts < j.max_attempts))
          AND (j.kind <> 'answer' OR NOT EXISTS (
                SELECT 1 FROM jobs r
                WHERE r.conversation_id = j.conversation_id AND r.id <> j.id AND r.kind = 'answer'
                  AND r.status = 'running'
                  AND (r.lease_expires_at >= now()
                    OR (r.attempts < r.max_attempts
                        AND (j.status = 'pending' OR (r.created_at, r.id) < (j.created_at, j.id))))))
        ORDER BY j.available_at
        LIMIT 1
        FOR UPDATE SKIP LOCKED
      )
      UPDATE jobs SET status = 'running', attempts = jobs.attempts + 1, lease_owner = ${owner},
        lease_expires_at = now() + make_interval(secs => ${leaseSeconds}::float8), updated_at = now()
      FROM next WHERE jobs.id = next.id
      RETURNING ${jobCols(this.db)}`;
    return r ? toJob(r) : null;
  }

  async getJob(id: string): Promise<Job> {
    const [r] = await this.db<JobRow[]>`SELECT ${jobCols(this.db)} FROM jobs WHERE id = ${id}`;
    if (!r) throw new NotFoundError("job");
    return toJob(r);
  }

  async completeJob(id: string, result: string): Promise<void> {
    await this.db`UPDATE jobs SET status = 'done', result = ${result}, completed_at = now(),
      lease_owner = NULL, lease_expires_at = NULL, updated_at = now() WHERE id = ${id}`;
  }

  async failJob(id: string, result: string, lastError: string): Promise<void> {
    await this.db`UPDATE jobs SET status = 'failed', result = ${result}, last_error = ${lastError}, completed_at = now(),
      lease_owner = NULL, lease_expires_at = NULL, updated_at = now() WHERE id = ${id}`;
  }

  /**
   * Schedules another attempt. The job stays 'running' with its lease set to
   * the retry time, so it keeps its place in conversation order.
   */
  async retryJob(id: string, delaySeconds: number, lastError: string): Promise<void> {
    await this.db`UPDATE jobs SET lease_owner = NULL, lease_expires_at = now() + make_interval(secs => ${delaySeconds}::float8),
      last_error = ${lastError}, updated_at = now() WHERE id = ${id}`;
  }

  async setJobReply(jobId: string, messageId: string): Promise<void> {
    await this.db`UPDATE jobs SET reply_message_id = ${messageId}, updated_at = now() WHERE id = ${jobId}`;
  }

  async setJobCredit(jobId: string, source: CreditSource, day: string): Promise<void> {
    await this.db`UPDATE jobs SET credit_source = ${source}, credit_day = ${day}::date, updated_at = now() WHERE id = ${jobId}`;
  }

  /** Marks running jobs whose lease expired after their last attempt as failed. */
  async failExhaustedJobs(): Promise<number> {
    const res = await this.db`UPDATE jobs SET status = 'failed', result = coalesce(result, 'exhausted'),
      completed_at = now(), lease_owner = NULL, updated_at = now()
      WHERE status = 'running' AND lease_expires_at < now() AND attempts >= max_attempts`;
    return res.count;
  }

  /** Wakes idle workers (delivered on commit when called in a transaction). */
  async notify(channel: string): Promise<void> {
    await this.db`SELECT pg_notify(${channel}, '')`;
  }

  // -------------------------------------------------------------------------
  // Usage, quotas and budgets
  // -------------------------------------------------------------------------

  private async ensureUsageRow(userId: string, day: string) {
    await this.db`INSERT INTO usage_daily (user_id, day) VALUES (${userId}, ${day}::date) ON CONFLICT DO NOTHING`;
  }

  /** A cheap pre-check before queueing a question. */
  async hasCredit(userId: string, day: string, freeCap: number): Promise<boolean> {
    const [r] = await this.db<{ ok: boolean }[]>`SELECT
      coalesce((SELECT questions_used FROM usage_daily WHERE user_id = ${userId} AND day = ${day}::date), 0) < ${freeCap}
      OR coalesce((SELECT bonus_sms_quota > 0 FROM users WHERE id = ${userId}), false) AS ok`;
    return r!.ok;
  }

  /** Spends one question from the daily free allowance, then from bonus credit. null when neither is available. */
  async consumeCredit(userId: string, day: string, freeCap: number): Promise<CreditSource | null> {
    await this.ensureUsageRow(userId, day);
    const free = await this.db`UPDATE usage_daily SET questions_used = questions_used + 1
      WHERE user_id = ${userId} AND day = ${day}::date AND questions_used < ${freeCap} RETURNING questions_used`;
    if (free.length > 0) return "free";
    const bonus = await this.db`UPDATE users SET bonus_sms_quota = bonus_sms_quota - 1, updated_at = now()
      WHERE id = ${userId} AND bonus_sms_quota > 0 RETURNING bonus_sms_quota`;
    if (bonus.length === 0) return null;
    await this.db`UPDATE usage_daily SET bonus_used = bonus_used + 1 WHERE user_id = ${userId} AND day = ${day}::date`;
    return "bonus";
  }

  /** Returns a credit when no answer could be produced. */
  async refundCredit(userId: string, source: CreditSource, day: string): Promise<void> {
    if (source === "free") {
      await this.db`UPDATE usage_daily SET questions_used = greatest(questions_used - 1, 0)
        WHERE user_id = ${userId} AND day = ${day}::date`;
    } else {
      await this.db`UPDATE users SET bonus_sms_quota = bonus_sms_quota + 1 WHERE id = ${userId}`;
      await this.db`UPDATE usage_daily SET bonus_used = greatest(bonus_used - 1, 0) WHERE user_id = ${userId} AND day = ${day}::date`;
    }
  }

  async questionsUsed(userId: string, day: string): Promise<number> {
    const [r] = await this.db<{ n: number }[]>`
      SELECT coalesce((SELECT questions_used FROM usage_daily WHERE user_id = ${userId} AND day = ${day}::date), 0) AS n`;
    return r!.n;
  }

  // Flips a boolean usage flag; true only the first time.
  private async markOnce(column: "limit_notice_sent" | "outage_notice_sent", userId: string, day: string): Promise<boolean> {
    await this.ensureUsageRow(userId, day);
    const res = await this.db`UPDATE usage_daily SET ${this.db(column)} = true
      WHERE user_id = ${userId} AND day = ${day}::date AND NOT ${this.db(column)}`;
    return res.count === 1;
  }

  /** The once-per-day "Daily limit reached" notice. */
  markLimitNotice(userId: string, day: string) {
    return this.markOnce("limit_notice_sent", userId, day);
  }

  /** The once-per-day outage notice. */
  markOutageNotice(userId: string, day: string) {
    return this.markOnce("outage_notice_sent", userId, day);
  }

  async failedCodeAttempts(userId: string, day: string): Promise<number> {
    const [r] = await this.db<{ n: number }[]>`
      SELECT coalesce((SELECT failed_code_attempts FROM usage_daily WHERE user_id = ${userId} AND day = ${day}::date), 0) AS n`;
    return r!.n;
  }

  async incrFailedCode(userId: string, day: string): Promise<void> {
    await this.ensureUsageRow(userId, day);
    await this.db`UPDATE usage_daily SET failed_code_attempts = failed_code_attempts + 1 WHERE user_id = ${userId} AND day = ${day}::date`;
  }

  private async ensureGlobalRow(day: string) {
    await this.db`INSERT INTO global_usage_daily (day) VALUES (${day}::date) ON CONFLICT DO NOTHING`;
  }

  /** Atomically books n SMS segments against the global daily segment cap and spend cap. */
  async reserveSegments(day: string, n: number, segmentCap: number, segmentCost: number, spendCap: number): Promise<boolean> {
    await this.ensureGlobalRow(day);
    const res = await this.db`UPDATE global_usage_daily SET sms_segments = sms_segments + ${n}
      WHERE day = ${day}::date AND sms_segments + ${n} <= ${segmentCap}
        AND (sms_segments + ${n}) * ${segmentCost}::numeric + ai_cost_usd <= ${spendCap}::numeric`;
    return res.count === 1;
  }

  async releaseSegments(day: string, n: number): Promise<void> {
    await this.db`UPDATE global_usage_daily SET sms_segments = greatest(sms_segments - ${n}, 0) WHERE day = ${day}::date`;
  }

  async addAICost(day: string, usd: number): Promise<void> {
    await this.ensureGlobalRow(day);
    await this.db`UPDATE global_usage_daily SET ai_cost_usd = ai_cost_usd + ${usd}::numeric WHERE day = ${day}::date`;
  }

  /** The day's booked segments and AI spend. */
  async daySpend(day: string): Promise<{ segments: number; aiCost: number }> {
    const [r] = await this.db<{ segments: number; ai_cost: number }[]>`
      SELECT sms_segments AS segments, ai_cost_usd::float8 AS ai_cost FROM global_usage_daily WHERE day = ${day}::date`;
    return { segments: r?.segments ?? 0, aiCost: r?.ai_cost ?? 0 };
  }

  // -------------------------------------------------------------------------
  // Vouchers
  // -------------------------------------------------------------------------

  /** Grants a pool's per-user credit to the user, once per pool, with the pool row locked (plan §11.3). */
  async redeemVoucher(userId: string, code: string): Promise<{ granted: number; balance: number }> {
    const [p] = await this.db<
      { id: string; per_user_grant: number; max_segments: number; used_segments: number; active: boolean; expires_at: Date | null }[]
    >`SELECT id::text, per_user_grant, max_segments, used_segments, active, expires_at
      FROM sponsored_pools WHERE voucher_code = ${code.toUpperCase()} FOR UPDATE`;
    if (!p) throw new VoucherError("invalid");
    if (!p.active || (p.expires_at && Date.now() > p.expires_at.getTime())) throw new VoucherError("expired");
    if (p.used_segments + p.per_user_grant > p.max_segments) throw new VoucherError("exhausted");
    const ins = await this.db`INSERT INTO voucher_redemptions (pool_id, user_id, granted_segments)
      VALUES (${p.id}, ${userId}, ${p.per_user_grant}) ON CONFLICT (pool_id, user_id) DO NOTHING`;
    if (ins.count === 0) throw new VoucherError("used");
    await this.db`UPDATE sponsored_pools SET used_segments = used_segments + ${p.per_user_grant} WHERE id = ${p.id}`;
    const [u] = await this.db<{ bonus_sms_quota: number }[]>`UPDATE users SET bonus_sms_quota = bonus_sms_quota + ${p.per_user_grant},
      updated_at = now() WHERE id = ${userId} RETURNING bonus_sms_quota`;
    return { granted: p.per_user_grant, balance: u!.bonus_sms_quota };
  }

  async createPool(p: Pool): Promise<Pool> {
    const code = p.voucher_code.toUpperCase();
    const [r] = await this.db<{ id: string; active: boolean; created_at: Date }[]>`
      INSERT INTO sponsored_pools (voucher_code, sponsor_name, max_segments, per_user_grant, expires_at)
      VALUES (${code}, ${p.sponsor_name}, ${p.max_segments}, ${p.per_user_grant}, ${p.expires_at ?? null})
      RETURNING id::text, active, created_at`;
    return { ...p, voucher_code: code, id: r!.id, active: r!.active, created_at: r!.created_at, used_segments: 0 };
  }

  async listPools(): Promise<Pool[]> {
    return this.db<Pool[]>`SELECT p.id::text, p.voucher_code, p.sponsor_name, p.max_segments, p.used_segments,
        p.per_user_grant, p.active, p.expires_at, p.created_at,
        (SELECT count(*)::int FROM voucher_redemptions r WHERE r.pool_id = p.id) AS redemptions
      FROM sponsored_pools p ORDER BY p.created_at DESC`;
  }

  async setPoolActive(code: string, active: boolean): Promise<void> {
    const res = await this.db`UPDATE sponsored_pools SET active = ${active} WHERE voucher_code = ${code.toUpperCase()}`;
    if (res.count === 0) throw new NotFoundError("pool");
  }

  // -------------------------------------------------------------------------
  // Grounding snippets
  // -------------------------------------------------------------------------

  /** Writes seed snippets, replacing rows with the same slug. */
  async upsertSnippets(snippets: Snippet[]): Promise<number> {
    for (const s of snippets) {
      await this.db`
        INSERT INTO knowledge_snippets (slug, category, trigger_keywords, verified_answer_gsm7, context_note,
          direct_reply_enabled, reviewed, source, enabled)
        VALUES (${s.slug}, ${s.category}, ${s.triggers.join("\n")}, ${s.answer}, ${orNull(s.context)},
          ${s.direct_reply ?? false}, ${s.reviewed ?? false}, ${orNull(s.source)}, ${s.enabled !== false})
        ON CONFLICT (slug) DO UPDATE SET category = EXCLUDED.category, trigger_keywords = EXCLUDED.trigger_keywords,
          verified_answer_gsm7 = EXCLUDED.verified_answer_gsm7, context_note = EXCLUDED.context_note,
          direct_reply_enabled = EXCLUDED.direct_reply_enabled, reviewed = EXCLUDED.reviewed,
          source = EXCLUDED.source, enabled = EXCLUDED.enabled, updated_at = now()`;
    }
    return snippets.length;
  }

  /** Enabled snippets; reviewedOnly drops unverified ones. */
  async loadSnippets(reviewedOnly: boolean): Promise<Snippet[]> {
    const rows = await this.db<
      { slug: string; category: string; triggers: string; answer: string; context: string; direct: boolean; reviewed: boolean; source: string }[]
    >`SELECT slug, category, trigger_keywords AS triggers, verified_answer_gsm7 AS answer, coalesce(context_note, '') AS context,
        direct_reply_enabled AS direct, reviewed, coalesce(source, '') AS source
      FROM knowledge_snippets WHERE enabled AND (reviewed OR NOT ${reviewedOnly}::boolean) ORDER BY slug`;
    return rows.map((r) => ({
      slug: r.slug,
      category: r.category,
      triggers: r.triggers
        .split("\n")
        .map((t) => t.trim())
        .filter(Boolean),
      answer: r.answer,
      context: r.context || undefined,
      direct_reply: r.direct,
      reviewed: r.reviewed,
      source: r.source || undefined,
    }));
  }

  /** A pg_trgm word-similarity search (admin curation aid). */
  async searchSnippets(query: string, limit: number) {
    return this.db<{ slug: string; category: string; answer: string; reviewed: boolean; score: number }[]>`
      SELECT slug, category, verified_answer_gsm7 AS answer, reviewed, word_similarity(${query}, trigger_keywords)::float8 AS score
      FROM knowledge_snippets WHERE enabled AND ${query} <% trigger_keywords
      ORDER BY score DESC LIMIT ${limit}`;
  }

  // -------------------------------------------------------------------------
  // Kill switches
  // -------------------------------------------------------------------------

  async flags(): Promise<Record<string, boolean>> {
    const out: Record<string, boolean> = { [Flag.aiEnabled]: true, [Flag.smsOutboundEnabled]: true };
    for (const r of await this.db<{ key: string; value: boolean }[]>`SELECT key, value FROM system_flags`) out[r.key] = r.value;
    return out;
  }

  async setFlag(key: string, value: boolean): Promise<void> {
    if (key !== Flag.aiEnabled && key !== Flag.smsOutboundEnabled) throw new Error(`unknown flag "${key}"`);
    await this.db`INSERT INTO system_flags (key, value) VALUES (${key}, ${value})
      ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`;
  }

  // -------------------------------------------------------------------------
  // Model calls and send attempts
  // -------------------------------------------------------------------------

  async insertModelCall(m: {
    jobId?: string;
    channel: string;
    model: string;
    status: string;
    inputTokens: number;
    outputTokens: number;
    reasoningTokens: number;
    costUsd: number;
    latencyMs: number;
    error?: string;
  }): Promise<void> {
    await this.db`INSERT INTO model_calls (job_id, channel, model_name, status, input_tokens,
        output_tokens, reasoning_tokens, estimated_cost_usd, latency_ms, error)
      VALUES (${orNull(m.jobId)}, ${m.channel}, ${m.model}, ${m.status}, ${m.inputTokens}, ${m.outputTokens},
        ${m.reasoningTokens}, ${m.costUsd}::numeric, ${Math.round(m.latencyMs)}, ${orNull(m.error)})`;
  }

  async linkModelCalls(jobId: string, messageId: string): Promise<void> {
    await this.db`UPDATE model_calls SET message_id = ${messageId} WHERE job_id = ${jobId} AND message_id IS NULL`;
  }

  async insertSendAttempt(messageId: string, provider: string, requestId: string): Promise<string> {
    const [r] = await this.db<{ id: string }[]>`INSERT INTO send_attempts (message_id, provider_name, provider_request_id, status)
      VALUES (${messageId}, ${provider}, ${requestId}, 'pending') RETURNING id::text`;
    return r!.id;
  }

  async updateSendAttempt(id: string, status: string, providerMessageId = "", errMsg = ""): Promise<void> {
    await this.db`UPDATE send_attempts SET status = ${status}, provider_message_id = coalesce(${orNull(providerMessageId)}, provider_message_id),
      error = coalesce(${orNull(errMsg)}, error), updated_at = now() WHERE id = ${id}`;
  }

  /** Attempts whose outcome is not known, oldest first. */
  async openAttempts(olderThanSeconds: number, limit: number): Promise<SendAttempt[]> {
    const rows = await this.db<
      { id: string; message_id: string; provider_name: string; provider_request_id: string; provider_message_id: string; status: string; created_at: Date }[]
    >`SELECT id::text, message_id::text, provider_name, provider_request_id, coalesce(provider_message_id, '') AS provider_message_id,
        status, created_at
      FROM send_attempts
      WHERE status IN ('pending', 'unknown_timeout') AND created_at <= now() - make_interval(secs => ${olderThanSeconds}::float8)
      ORDER BY created_at LIMIT ${limit}`;
    return rows.map((r) => ({
      id: r.id,
      messageId: r.message_id,
      provider: r.provider_name,
      requestId: r.provider_request_id,
      providerMessageId: r.provider_message_id,
      status: r.status,
      createdAt: r.created_at,
    }));
  }

  // -------------------------------------------------------------------------
  // Gateway outbox (the Android phone)
  // -------------------------------------------------------------------------

  /** Hands a message to the phone. Idempotent on the message id. */
  async gatewayEnqueue(messageId: string, to: string, body: string, segments: number): Promise<void> {
    const toSealed = this.seal(Purpose.phone, to);
    const bodySealed = this.seal(Purpose.message, body);
    await this.db`INSERT INTO gateway_outbox (message_id, to_ciphertext, body_ciphertext, key_version, segments)
      VALUES (${messageId}, ${toSealed.ciphertext}, ${bodySealed.ciphertext}, ${toSealed.version}, ${Math.max(segments, 1)})
      ON CONFLICT (message_id) DO NOTHING`;
  }

  /** Queued rows, oldest first; marks them offered. Rows stay queued until the phone acknowledges them. */
  async gatewayOffer(limit: number): Promise<GatewayRow[]> {
    const rows = await this.db<{ id: string; to_ciphertext: Buffer; body_ciphertext: Buffer; key_version: number; created_at: Date }[]>`
      UPDATE gateway_outbox SET offered_count = offered_count + 1, offered_at = now(), updated_at = now()
      WHERE message_id IN (
        SELECT message_id FROM gateway_outbox WHERE state = 'queued' ORDER BY created_at LIMIT ${limit} FOR UPDATE SKIP LOCKED)
      RETURNING message_id::text AS id, to_ciphertext, body_ciphertext, key_version, created_at`;
    return rows
      .sort((a, b) => a.created_at.getTime() - b.created_at.getTime())
      .map((r) => ({
        id: r.id,
        to: this.open(Purpose.phone, r.to_ciphertext, r.key_version),
        body: this.open(Purpose.message, r.body_ciphertext, r.key_version),
      }));
  }

  /** The phone saved these rows and now owns sending them. Returns how many moved to acked. */
  async gatewayAck(ids: string[]): Promise<number> {
    const res = await this.db`UPDATE gateway_outbox SET state = 'acked', acked_at = now(), updated_at = now()
      WHERE message_id = ANY(${ids}::uuid[]) AND state = 'queued'`;
    return res.count;
  }

  /** Applies a phone status; states only move forward. Returns false for unknown ids. */
  async gatewayStatus(id: string, status: "sent" | "delivered" | "failed", error = ""): Promise<boolean> {
    const res = await this.db`UPDATE gateway_outbox SET
        state = CASE WHEN state = 'delivered' THEN state
                     WHEN ${status} = 'sent' AND state IN ('delivered', 'failed') THEN state
                     ELSE ${status} END,
        acked_at = coalesce(acked_at, now()),
        sent_at = CASE WHEN ${status} IN ('sent', 'delivered') THEN coalesce(sent_at, now()) ELSE sent_at END,
        finished_at = CASE WHEN ${status} IN ('delivered', 'failed') THEN coalesce(finished_at, now()) ELSE finished_at END,
        error = coalesce(${orNull(error)}, error), updated_at = now()
      WHERE message_id = ${id}`;
    return res.count > 0;
  }

  /** Expires queued rows older than ttl. Returns what was expired, for refunds. */
  async gatewayExpire(ttlSeconds: number): Promise<{ id: string; segments: number; created_at: Date }[]> {
    return this.db<{ id: string; segments: number; created_at: Date }[]>`
      UPDATE gateway_outbox SET state = 'expired', finished_at = now(), error = 'phone_offline', updated_at = now()
      WHERE state = 'queued' AND created_at < now() - make_interval(secs => ${ttlSeconds}::float8)
      RETURNING message_id::text AS id, segments, created_at`;
  }

  /** Acked rows with no status for too long: flagged once for an operator, never resent. */
  async gatewayFlagStale(afterSeconds: number): Promise<string[]> {
    const rows = await this.db<{ id: string }[]>`
      UPDATE gateway_outbox SET stale_flagged = true, updated_at = now()
      WHERE state = 'acked' AND NOT stale_flagged AND acked_at < now() - make_interval(secs => ${afterSeconds}::float8)
      RETURNING message_id::text AS id`;
    return rows.map((r) => r.id);
  }

  async gatewayCounts(): Promise<Record<string, number>> {
    const out: Record<string, number> = {};
    for (const r of await this.db<{ state: string; n: number }[]>`
      SELECT state, count(*)::int AS n FROM gateway_outbox WHERE created_at > now() - interval '1 day' GROUP BY state`) {
      out[r.state] = r.n;
    }
    const [stale] = await this.db<{ n: number }[]>`SELECT count(*)::int AS n FROM gateway_outbox WHERE state = 'acked' AND stale_flagged`;
    out.stale_acked = stale!.n;
    return out;
  }

  async gatewayHeartbeat(h: {
    device_id: string;
    app_version: string;
    battery: number | null;
    charging: boolean | null;
    signal: number | null;
    sim_label: string | null;
    inbox_pending: number;
    outbox_pending: number;
    status_pending: number;
    paused: boolean;
  }): Promise<void> {
    await this.db`INSERT INTO gateway_devices ${this.db(h)}
      ON CONFLICT (device_id) DO UPDATE SET app_version = EXCLUDED.app_version, battery = EXCLUDED.battery,
        charging = EXCLUDED.charging, signal = EXCLUDED.signal, sim_label = EXCLUDED.sim_label,
        inbox_pending = EXCLUDED.inbox_pending, outbox_pending = EXCLUDED.outbox_pending,
        status_pending = EXCLUDED.status_pending, paused = EXCLUDED.paused, last_seen_at = now()`;
  }

  /** Known gateway phones, most recently seen first. */
  async gatewayDevices() {
    return this.db<
      {
        device_id: string;
        app_version: string;
        battery: number | null;
        charging: boolean | null;
        signal: number | null;
        sim_label: string | null;
        inbox_pending: number;
        outbox_pending: number;
        status_pending: number;
        paused: boolean;
        last_seen_at: Date;
        seconds_since_seen: number;
      }[]
    >`SELECT *, extract(epoch FROM now() - last_seen_at)::float8 AS seconds_since_seen FROM gateway_devices ORDER BY last_seen_at DESC`;
  }

  // -------------------------------------------------------------------------
  // Retention and reports
  // -------------------------------------------------------------------------

  /** Enforces the retention policy (plan §10.2). */
  async purge(retentionDays: number) {
    const messages = await this.db`DELETE FROM messages WHERE created_at < now() - make_interval(days => ${retentionDays})`;
    const jobs = await this.db`DELETE FROM jobs WHERE status IN ('done', 'failed', 'cancelled')
      AND updated_at < now() - make_interval(days => ${retentionDays})`;
    const modelCalls = await this.db`DELETE FROM model_calls WHERE created_at < now() - make_interval(days => ${retentionDays})`;
    const overflow = await this.db`UPDATE conversations SET overflow_buffer = NULL, overflow_key_version = NULL, overflow_expires_at = NULL
      WHERE overflow_expires_at < now()`;
    // Finished gateway rows hold an encrypted copy of the body: keep them a day for the status page only.
    const gateway = await this.db`DELETE FROM gateway_outbox WHERE state IN ('delivered', 'failed', 'expired', 'sent')
      AND updated_at < now() - interval '1 day'`;
    return { messages: messages.count, jobs: jobs.count, modelCalls: modelCalls.count, overflow: overflow.count, gateway: gateway.count };
  }

  /** One UTC day for the operator (counts and costs only, never content). */
  async usageReport(day: string, segmentCost: number, spendCap: number, segmentCap: number) {
    const start = `${day}T00:00:00Z`;
    const end = new Date(new Date(start).getTime() + 86_400_000).toISOString();
    const r = {
      day,
      questions: 0,
      questions_by_result: {} as Record<string, number>,
      coalesced_messages: 0,
      grounding_direct_answers: 0,
      more_served: 0,
      outbound_by_status: {} as Record<string, number>,
      sms_segments_booked: 0,
      sms_segment_cap: segmentCap,
      sms_cost_usd: 0,
      ai_calls: 0,
      ai_empty_responses: 0,
      ai_errors: 0,
      ai_avg_latency_ms: 0,
      ai_cost_usd: 0,
      total_spend_usd: 0,
      spend_cap_usd: spendCap,
      users_total: 0,
      users_new: 0,
      users_opted_out: 0,
      voucher_redemptions: 0,
      voucher_credit_granted: 0,
    };
    for (const row of await this.db<{ k: string; n: number; coalesced: number }[]>`
      SELECT coalesce(result, status) AS k, count(*)::int AS n, coalesce(sum(coalesce_count), 0)::int AS coalesced
      FROM jobs WHERE kind = 'answer' AND created_at >= ${start} AND created_at < ${end} GROUP BY 1`) {
      r.questions_by_result[row.k] = row.n;
      r.questions += row.n;
      r.coalesced_messages += row.coalesced;
    }
    for (const row of await this.db<{ kind: string; status: string; source: string; n: number }[]>`
      SELECT kind, status, coalesce(answer_source, '') AS source, count(*)::int AS n FROM messages
      WHERE direction = 'outbound' AND created_at >= ${start} AND created_at < ${end} GROUP BY 1, 2, 3`) {
      r.outbound_by_status[row.status] = (r.outbound_by_status[row.status] ?? 0) + row.n;
      if (row.kind === Kind.more) r.more_served += row.n;
      if (row.kind === Kind.answer && row.source === "grounding") r.grounding_direct_answers += row.n;
    }
    const [ai] = await this.db<{ calls: number; empty: number; errors: number; latency: number; cost: number }[]>`
      SELECT count(*)::int AS calls, (count(*) FILTER (WHERE status = 'empty'))::int AS empty,
        (count(*) FILTER (WHERE status = 'error'))::int AS errors,
        coalesce(avg(latency_ms), 0)::float8 AS latency, coalesce(sum(estimated_cost_usd), 0)::float8 AS cost
      FROM model_calls WHERE created_at >= ${start} AND created_at < ${end}`;
    r.ai_calls = ai!.calls;
    r.ai_empty_responses = ai!.empty;
    r.ai_errors = ai!.errors;
    r.ai_avg_latency_ms = ai!.latency;
    r.ai_cost_usd = ai!.cost;
    const spend = await this.daySpend(day);
    r.sms_segments_booked = spend.segments;
    r.sms_cost_usd = spend.segments * segmentCost;
    r.ai_cost_usd = Math.max(r.ai_cost_usd, spend.aiCost);
    r.total_spend_usd = r.sms_cost_usd + r.ai_cost_usd;
    const [users] = await this.db<{ total: number; fresh: number; out: number }[]>`
      SELECT count(*)::int AS total, (count(*) FILTER (WHERE created_at >= ${start} AND created_at < ${end}))::int AS fresh,
        (count(*) FILTER (WHERE opted_out))::int AS out FROM users`;
    r.users_total = users!.total;
    r.users_new = users!.fresh;
    r.users_opted_out = users!.out;
    const [v] = await this.db<{ n: number; granted: number }[]>`
      SELECT count(*)::int AS n, coalesce(sum(granted_segments), 0)::int AS granted FROM voucher_redemptions
      WHERE redeemed_at >= ${start} AND redeemed_at < ${end}`;
    r.voucher_redemptions = v!.n;
    r.voucher_credit_granted = v!.granted;
    return r;
  }
}
