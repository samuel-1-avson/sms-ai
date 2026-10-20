// The domain orchestrator. It turns normalized channel events into stored
// messages, commands, debounced answer jobs, model calls, formatted replies
// and outbound SMS, enforcing quotas, budgets, opt-outs and kill switches.
import type { Logger } from "pino";
import { AIError, AnswerError, buildPrompt, isAIError, isRetryable, type CallRecord, type Client, type Turn as PromptTurn } from "../ai";
import {
  isStatusQuerier,
  sendErrorKindOf,
  BadPayloadError,
  type DeliveryReceipt,
  type InboundSMS,
  type Registry,
} from "../channel/channel";
import { Texts, codeAcceptedText, limitReachedText, parseCommand, type Command, type CommandKind } from "../commands";
import { isDevelopment, type Config } from "../config";
import { SMS_LIMITS, SMS_MORE_SUFFIX, analyze, containsLocalLanguageLetters, sanitize, splitFirst, truncate, type Limits } from "../formatting";
import { Matcher, factOf, type Snippet } from "../grounding";
import { carrier as carrierOf, normalize } from "../msisdn";
import type { Metrics } from "../observability";
import { scrub, truncateQuestion } from "../policy";
import { Flag, Kind, Status, VoucherError, type Conversation, type Job, type Overflow, type Q, type Turn, type User } from "../repository/queries";
import { JOBS_CHANNEL, dayOf, type Store } from "../repository/store";

/** Bounds the user text stored and sent to the model. */
const MAX_QUESTION_CHARS = 600;

export class AIUnavailableError extends Error {
  constructor(detail: string) {
    super(`service: AI unavailable (${detail})`);
  }
}

export interface InboundResult {
  duplicate: boolean;
  command: CommandKind | null;
  jobId?: string;
  coalesced?: boolean;
  dropped?: "rate_limited" | "opted_out" | "over_limit" | "empty";
}

export interface EngineDeps {
  cfg: Config;
  store: Store;
  registry: Registry;
  ai: Client;
  matcher?: Matcher;
  metrics: Metrics;
  log: Logger;
  now?: () => Date;
}

const backoffSeconds = (attempt: number) => attempt * attempt * 5;

/** Removes our own continuation markers before a past answer is replayed to the model. */
function stripPagerSuffix(s: string): string {
  const i = s.lastIndexOf(" ..");
  return i > 0 && s.slice(i).toUpperCase().includes("MORE") ? s.slice(0, i).trim() : s;
}

/** Strips pager suffixes and merges consecutive same-role turns (coalesced texts) so chat templates stay valid. */
function toPromptTurns(turns: Turn[]): PromptTurn[] {
  const out: PromptTurn[] = [];
  for (const t of turns) {
    const content = stripPagerSuffix(t.content);
    if (!content) continue;
    const last = out.at(-1);
    if (last && last.role === t.role) last.content += " " + content;
    else out.push({ role: t.role, content });
  }
  return out;
}

export function normalizeReceiptStatus(s: string): string {
  switch (s.toLowerCase()) {
    case "delivered": case "delivrd": case "success":
      return Status.delivered;
    case "failed": case "undelivered": case "undeliv": case "rejected": case "rejectd": case "blacklisted":
      return Status.failed;
    case "expired":
      return Status.expired;
    case "submitted": case "sent": case "accepted": case "enroute": case "queued":
      return Status.submitted;
  }
  return "";
}

export class Engine {
  readonly cfg: Config;
  readonly store: Store;
  readonly registry: Registry;
  readonly ai: Client;
  readonly matcher: Matcher;
  readonly metrics: Metrics;
  readonly log: Logger;
  now: () => Date;
  /** How old an uncertain send attempt must be before the provider is asked about it. */
  reconcileAfterSeconds = 30;

  private flags: Record<string, boolean> | null = null;
  private flagsAt = 0;
  private lastPurge = 0;
  private lastReload = Date.now();
  private lastOfflineWarning = 0;

  constructor(d: EngineDeps) {
    this.cfg = d.cfg;
    this.store = d.store;
    this.registry = d.registry;
    this.ai = d.ai;
    this.matcher = d.matcher ?? new Matcher();
    this.metrics = d.metrics;
    this.log = d.log;
    this.now = d.now ?? (() => new Date());
  }

  private today(): string {
    return dayOf(this.now());
  }

  /** Reads a kill switch, cached for a few seconds. Fails open if the flags cannot be read at all. */
  async flag(key: string): Promise<boolean> {
    if (this.flags === null || Date.now() - this.flagsAt > 3000) {
      try {
        this.flags = await this.store.q().flags();
        this.flagsAt = Date.now();
      } catch (err) {
        if (this.flags === null) {
          this.log.error({ err }, "loading system flags");
          return true;
        }
      }
    }
    return this.flags![key] ?? true;
  }

  invalidateFlags() {
    this.flags = null;
  }

  // ---------------------------------------------------------------------------
  // Inbound
  // ---------------------------------------------------------------------------

  /**
   * Persists an inbound text and either runs a command or queues a debounced
   * answer job. It does no model or provider I/O, so the webhook can be
   * acknowledged quickly.
   */
  async handleInboundSMS(ev: InboundSMS): Promise<InboundResult> {
    let from: string;
    try {
      from = normalize(ev.from);
    } catch {
      throw new BadPayloadError("sender number");
    }
    const carrier = carrierOf(from);
    this.metrics.channelInbound.inc({ provider: ev.provider, carrier });

    const res: InboundResult = { duplicate: false, command: null };
    const body = ev.body.trim();
    if (!body) {
      res.dropped = "empty";
      return res;
    }
    const cmd = parseCommand(body);
    res.command = cmd.kind;
    const scrubbed = truncateQuestion(scrub(body).text, MAX_QUESTION_CHARS);
    const allowUcs2 = this.cfg.allowUcs2LocalLanguage && containsLocalLanguageLetters(body);
    const serviceId = ev.serviceId || this.cfg.smsServiceId;
    const day = this.today();

    await this.store.tx(async (q) => {
      const { user } = await q.findOrCreateUser(from, carrier);
      const conv = await q.touchConversation(user.id, serviceId, "sms", ev.provider, this.cfg.contextTtlSeconds);
      const msg = await q.insertInbound({
        conversationId: conv.id,
        provider: ev.provider,
        providerMessageId: ev.providerEventId,
        kind: cmd.kind ? Kind.command : Kind.question,
        body: scrubbed,
        status: Status.received,
      });
      if (msg.duplicate) {
        res.duplicate = true;
        return;
      }
      if (this.cfg.perNumberRatePerMinute > 0) {
        const n = await q.countRecentInbound(user.id, new Date(this.now().getTime() - 60_000));
        if (n > this.cfg.perNumberRatePerMinute) {
          res.dropped = "rate_limited";
          await q.setMessageStatus(msg.id, Status.rateLimited);
          return;
        }
      }
      if (user.optedOut && cmd.kind !== "START") {
        res.dropped = "opted_out";
        await q.setMessageStatus(msg.id, Status.ignored);
        return;
      }
      if (cmd.kind) {
        const reply = await this.runCommand(q, user, conv, cmd, day, SMS_LIMITS, SMS_MORE_SUFFIX);
        if (reply) await this.queueSMS(q, conv, "", reply.kind, "command", reply.text);
        return;
      }
      if (!(await q.hasCredit(user.id, day, this.cfg.dailyPerNumberFreeCap))) {
        res.dropped = "over_limit";
        this.metrics.dailyLimitReached.inc();
        await q.setMessageStatus(msg.id, Status.overLimit);
        await this.sendLimitNoticeOnce(q, user.id, conv, day);
        return;
      }
      const job = await q.enqueueAnswer(conv.id, msg.id, {
        debounceMs: this.cfg.inboundDebounceMs,
        extendMs: this.cfg.coalesceExtendMs,
        maxCoalesce: this.cfg.maxCoalesce,
        maxAttempts: this.cfg.jobMaxAttempts,
        allowUcs2,
      });
      res.jobId = job.jobId;
      res.coalesced = job.coalesced;
      if (job.coalesced) this.metrics.smsInboundCoalesced.inc();
    });
    if (res.duplicate) this.metrics.duplicateInbound.inc();
    return res;
  }

  private async sendLimitNoticeOnce(q: Q, userId: string, conv: Conversation, day: string) {
    if (!(await q.markLimitNotice(userId, day))) return;
    const used = await q.questionsUsed(userId, day);
    await this.queueSMS(q, conv, "", Kind.notice, "policy", limitReachedText(used, this.cfg.dailyPerNumberFreeCap));
  }

  /** Executes a keyword command and returns the reply and the message kind to store it as. */
  private async runCommand(
    q: Q,
    user: User,
    conv: Conversation,
    cmd: Command,
    day: string,
    lim: Limits,
    suffix: string,
  ): Promise<{ text: string; kind: string } | null> {
    const reply = (text: string, kind: string = Kind.commandReply) => ({ text, kind });
    switch (cmd.kind) {
      case "HELP":
        return reply(Texts.help);
      case "NEW":
        await q.resetContext(conv.id);
        return reply(Texts.newChat);
      case "MORE": {
        const page = await this.nextOverflowPage(q, conv.id, lim, suffix);
        return page === null ? reply(Texts.noMore) : reply(page, Kind.more);
      }
      case "STOP":
        await q.setOptOut(user.id, true);
        return reply(Texts.stop);
      case "START":
        await q.setOptOut(user.id, false);
        return reply(Texts.start);
      case "DELETE":
        await q.deleteUserContent(user.id);
        return reply(Texts.deleted);
      case "CODE":
        return reply(await this.redeem(q, user.id, cmd.arg ?? "", day));
    }
    return null;
  }

  private async redeem(q: Q, userId: string, code: string, day: string): Promise<string> {
    if ((await q.failedCodeAttempts(userId, day)) >= this.cfg.maxFailedCodeAttempts) return Texts.codeTooMany;
    try {
      const { granted, balance } = await q.redeemVoucher(userId, code);
      this.metrics.voucherSegmentsRedeemed.inc(granted);
      return codeAcceptedText(granted, balance);
    } catch (err) {
      if (!(err instanceof VoucherError)) throw err;
      switch (err.reason) {
        case "invalid":
          await q.incrFailedCode(userId, day);
          return Texts.codeInvalid;
        case "expired":
        case "exhausted":
          return Texts.codeExpired;
        case "used":
          return Texts.codeUsed;
      }
    }
  }

  /** Applies delivery receipts idempotently. */
  async handleDeliveryReceipts(receipts: DeliveryReceipt[]): Promise<void> {
    for (const r of receipts) {
      const status = normalizeReceiptStatus(r.status);
      if (status) await this.store.q().applyReceipt(r.provider, r.providerMessageId, status, r.errorCode);
    }
  }

  // ---------------------------------------------------------------------------
  // Outbound helpers
  // ---------------------------------------------------------------------------

  /** Stores an outbound SMS and a send job for it. NOTIFY inside the transaction wakes a worker on commit. */
  private async queueSMS(q: Q, conv: Conversation, jobId: string, kind: string, source: string, body: string): Promise<string> {
    const sender = this.registry.senderFor(conv.provider);
    const st = analyze(body);
    const msgId = await q.insertOutbound({
      conversationId: conv.id,
      jobId,
      provider: sender.name,
      kind,
      body,
      status: Status.queued,
      answerSource: source,
      encoding: st.encoding,
      units: st.units,
      segments: st.segments,
    });
    if (!jobId) {
      await q.createSendJob(conv.id, msgId, this.cfg.jobMaxAttempts);
      await q.notify(JOBS_CHANNEL);
    }
    return msgId;
  }

  /** Pops the next MORE page, sized for the given limits. */
  private async nextOverflowPage(q: Q, convId: string, lim: Limits, suffix: string): Promise<string | null> {
    const o = await q.lockOverflow(convId);
    if (!o) return null;
    let page: string;
    let rest = "";
    if (o.pages_left <= 1) page = truncate(o.rest, lim);
    else [page, rest] = splitFirst(o.rest, lim, suffix);
    await q.setOverflow(convId, { rest, pages_left: o.pages_left - 1 }, this.cfg.contextTtlSeconds);
    this.metrics.smsMoreBufferServed.inc();
    return page;
  }

  /** The single-segment first page and the MORE remainder. */
  private splitForSMS(text: string): [string, Overflow] {
    if (this.cfg.maxMorePages <= 0) return [truncate(text, SMS_LIMITS), { rest: "", pages_left: 0 }];
    const [first, rest] = splitFirst(text, SMS_LIMITS, SMS_MORE_SUFFIX);
    return [first, { rest, pages_left: this.cfg.maxMorePages }];
  }

  // ---------------------------------------------------------------------------
  // Answers
  // ---------------------------------------------------------------------------

  /**
   * Answers a question: the Local Ghana Grounding Store first (zero tokens),
   * then the model with recent context and any partially matched verified
   * facts. Model calls are recorded even when they fail.
   */
  private async generate(conv: Conversation, jobId: string, question: string, allowUcs2: boolean, signal: AbortSignal) {
    const match = this.matcher.match(question);
    if (match.direct) {
      this.metrics.groundingHits.inc({ category: match.direct.category });
      return { text: match.direct.answer, source: "grounding" };
    }
    this.metrics.groundingMisses.inc();

    if (!(await this.flag(Flag.aiEnabled))) throw new AIUnavailableError("ai_enabled=false");
    const { segments, aiCost } = await this.store.q().daySpend(this.today());
    const spend = segments * this.cfg.smsSegmentCostUsd + aiCost;
    this.metrics.dailySpendUsd.set(spend);
    if (spend >= this.cfg.globalDailySpendCapUsd) {
      this.log.warn({ spend_usd: spend, cap_usd: this.cfg.globalDailySpendCapUsd }, "daily spend cap reached; AI calls paused");
      throw new AIUnavailableError("daily spend cap");
    }

    const turns = await this.store.q().recentTurns(conv.id, jobId, conv.contextStartedAt, this.cfg.contextMaxTurns * 2);
    const msgs = buildPrompt({
      question,
      history: toPromptTurns(turns),
      facts: match.context.map(factOf),
      now: this.now(),
      maxInputTokens: this.cfg.contextMaxInputTokens,
    });

    let text: string;
    try {
      const out = await this.ai.answer(msgs, signal);
      await this.recordModelCalls(conv.channel, jobId, out.calls);
      text = out.text;
    } catch (err) {
      if (err instanceof AnswerError) {
        await this.recordModelCalls(conv.channel, jobId, err.calls);
        if (isAIError(err.error, "circuit_open")) throw new AIUnavailableError(err.message);
        throw err.error;
      }
      throw err;
    }
    const clean = sanitize(text, { allowUCS2: allowUcs2 });
    if (!clean) throw new AIError("empty", "nothing left after sanitizing");
    return { text: clean, source: "llm" };
  }

  /** Writes the token/cost ledger; failures are logged, never fatal. */
  private async recordModelCalls(channel: string, jobId: string, calls: CallRecord[]) {
    if (calls.length === 0) return;
    const q = this.store.q();
    let total = 0;
    for (const c of calls) {
      total += c.costUsd;
      try {
        await q.insertModelCall({ jobId, channel, ...c });
      } catch (err) {
        this.log.error({ err }, "recording model call");
      }
    }
    try {
      await q.addAICost(this.today(), total);
    } catch (err) {
      this.log.error({ err }, "recording AI cost");
    }
  }

  // ---------------------------------------------------------------------------
  // Jobs
  // ---------------------------------------------------------------------------

  /**
   * Runs one claimed job. Expected failures are handled inside (retries,
   * notices, refunds); a thrown error is unexpected and the worker
   * reschedules the job.
   */
  async processJob(job: Job): Promise<void> {
    let result: string;
    if (job.kind === "answer") result = await this.processAnswer(job);
    else if (job.kind === "send") result = await this.processSend(job);
    else {
      result = "unknown_kind";
      await this.store.q().failJob(job.id, "unknown_kind", job.kind);
    }
    this.metrics.jobsProcessed.inc({ kind: job.kind, result });
  }

  async rescheduleAfterError(job: Job, cause: unknown): Promise<void> {
    const msg = cause instanceof Error ? cause.message : String(cause);
    if (job.attempts < job.maxAttempts) await this.store.q().retryJob(job.id, backoffSeconds(job.attempts), msg);
    else await this.store.q().failJob(job.id, "error", msg);
  }

  private async processSend(job: Job): Promise<string> {
    const q = this.store.q();
    if (!job.replyMessageId) {
      await q.failJob(job.id, "missing_message", "send job without message");
      return "missing_message";
    }
    try {
      const result = await this.dispatch(job.replyMessageId);
      await q.completeJob(job.id, result);
      return result;
    } catch (err) {
      const msg = (err as Error).message;
      if (job.attempts < job.maxAttempts) {
        await q.retryJob(job.id, backoffSeconds(job.attempts), msg);
        return "retry";
      }
      await q.failJob(job.id, "send_failed", msg);
      return "send_failed";
    }
  }

  private async processAnswer(job: Job): Promise<string> {
    const q = this.store.q();
    const conv = await q.getConversation(job.conversationId);
    const user = await q.getUser(conv.userId);
    // A previous attempt produced the answer; only delivery is left.
    if (job.replyMessageId) return this.finishAnswerDelivery(job, job.replyMessageId, "answered");
    if (user.optedOut) return this.refundAndComplete(job, user.id, "opted_out");
    const question = await q.jobQuestion(job.id);
    if (!question) return this.refundAndComplete(job, user.id, "empty");

    if (!job.creditSource) {
      const creditDay = this.today();
      const source = await this.store.tx(async (tq) => {
        const s = await tq.consumeCredit(user.id, creditDay, this.cfg.dailyPerNumberFreeCap);
        if (s) await tq.setJobCredit(job.id, s, creditDay);
        return s;
      });
      if (!source) {
        this.metrics.dailyLimitReached.inc();
        await this.store.tx(async (tq) => {
          await this.sendLimitNoticeOnce(tq, user.id, conv, creditDay);
          await tq.completeJob(job.id, "over_limit");
        });
        return "over_limit";
      }
      job.creditSource = source;
      job.creditDay = creditDay;
    }

    let ans: { text: string; source: string };
    try {
      ans = await this.generate(conv, job.id, question, job.allowUcs2, AbortSignal.timeout(this.cfg.llmTimeoutMs));
    } catch (err) {
      const retryable = isRetryable(err) && !(err instanceof AIUnavailableError);
      if (retryable && job.attempts < job.maxAttempts) {
        this.log.warn({ job: job.id, attempt: job.attempts, err: (err as Error).message }, "answer attempt failed; retrying");
        await q.retryJob(job.id, backoffSeconds(job.attempts), (err as Error).message);
        return "retry";
      }
      this.log.error({ job: job.id, err: (err as Error).message }, "answer failed");
      return this.failAnswer(job, user.id, conv, err);
    }

    const [first, overflow] = this.splitForSMS(ans.text);
    const msgId = await this.store.tx(async (tq) => {
      const id = await this.queueSMS(tq, conv, job.id, Kind.answer, ans.source, first);
      await tq.setOverflow(conv.id, overflow, this.cfg.contextTtlSeconds);
      await tq.linkModelCalls(job.id, id);
      await tq.setJobReply(job.id, id);
      return id;
    });
    return this.finishAnswerDelivery(job, msgId, "answered_" + ans.source);
  }

  private async finishAnswerDelivery(job: Job, msgId: string, result: string): Promise<string> {
    const q = this.store.q();
    let sendResult: string;
    try {
      sendResult = await this.dispatch(msgId);
    } catch (err) {
      const msg = (err as Error).message;
      if (job.attempts < job.maxAttempts) {
        await q.retryJob(job.id, backoffSeconds(job.attempts), msg);
        return "retry_send";
      }
      await q.failJob(job.id, "send_failed", msg);
      return "send_failed";
    }
    if (sendResult !== Status.submitted) result += "_" + sendResult;
    await q.completeJob(job.id, result);
    return result;
  }

  private async refundAndComplete(job: Job, userId: string, result: string): Promise<string> {
    await this.store.tx(async (q) => {
      if (job.creditSource && job.creditDay) await q.refundCredit(userId, job.creditSource, job.creditDay);
      await q.completeJob(job.id, result);
    });
    return result;
  }

  /** Refunds the question and sends at most one apology per user per day, so an outage cannot multiply SMS spend. */
  private async failAnswer(job: Job, userId: string, conv: Conversation, cause: unknown): Promise<string> {
    const unavailable = cause instanceof AIUnavailableError;
    const result = unavailable ? "ai_unavailable" : "failed";
    const text = unavailable ? Texts.busy : Texts.failed;
    await this.store.tx(async (q) => {
      if (job.creditSource && job.creditDay) await q.refundCredit(userId, job.creditSource, job.creditDay);
      if (await q.markOutageNotice(userId, this.today())) await this.queueSMS(q, conv, "", Kind.notice, "policy", text);
      await q.failJob(job.id, result, cause instanceof Error ? cause.message : String(cause));
    });
    return result;
  }

  // ---------------------------------------------------------------------------
  // Dispatch
  // ---------------------------------------------------------------------------

  /**
   * Submits one outbound SMS exactly once. The send intent is recorded
   * before the provider call; an uncertain outcome leaves the message
   * 'unknown_timeout' for reconciliation instead of resending (plan §5.4).
   * Returns the resulting status, or throws when a retry is safe.
   */
  async dispatch(msgId: string): Promise<string> {
    const day = this.today();
    const prep = await this.store.tx(async (q) => {
      const info = await q.getForDispatch(msgId, true);
      if (info.status === Status.sending) {
        // A previous attempt crashed mid-send: outcome unknown, never resend blindly.
        await q.updateOutbound(msgId, Status.unknownTimeout, "", "crashed_mid_send");
        return { info, outcome: Status.unknownTimeout, attemptId: "" };
      }
      if (info.status !== Status.queued) return { info, outcome: info.status, attemptId: "" };
      if (!(await this.flag(Flag.smsOutboundEnabled))) {
        await q.updateOutbound(msgId, Status.blockedOff, "", "kill_switch");
        return { info, outcome: Status.blockedOff, attemptId: "" };
      }
      const segments = Math.max(info.segments, 1);
      const ok = await q.reserveSegments(day, segments, this.cfg.globalDailySmsSegmentCap, this.cfg.smsSegmentCostUsd, this.cfg.globalDailySpendCapUsd);
      if (!ok) {
        this.log.warn({ message: msgId }, "global SMS cap reached; message not sent");
        await q.updateOutbound(msgId, Status.blockedCap, "", "daily_cap");
        return { info, outcome: Status.blockedCap, attemptId: "" };
      }
      await q.updateOutbound(msgId, Status.sending);
      return { info, outcome: "", attemptId: await q.insertSendAttempt(msgId, info.provider, msgId) };
    });
    const { info, outcome, attemptId } = prep;
    if (outcome) {
      if (outcome === Status.blockedCap || outcome === Status.blockedOff || outcome === Status.unknownTimeout) {
        this.metrics.smsOutboundFailed.inc({ provider: info.provider, reason: outcome });
      }
      return outcome; // blocked, unknown, or already handled by an earlier attempt
    }

    const q = this.store.q();
    let sender;
    let phone: string;
    try {
      phone = await q.userPhone(info.userId);
      sender = this.registry.senderFor(info.provider);
    } catch (err) {
      return this.releaseAfterFailure(info.id, info.segments, attemptId, day, err);
    }
    try {
      const res = await sender.send(
        { to: phone, senderId: "", body: info.body, encoding: info.encoding, segments: info.segments, clientRef: msgId },
        AbortSignal.timeout(10_000),
      );
      await q.updateSendAttempt(attemptId, "submitted", res.providerMessageId);
      await q.updateOutbound(msgId, Status.submitted, res.providerMessageId);
      this.metrics.observeSubmitted(sender.name, info.encoding, info.segments);
      return Status.submitted;
    } catch (sendErr) {
      const msg = (sendErr as Error).message;
      switch (sendErrorKindOf(sendErr)) {
        case "uncertain":
          this.log.warn({ message: msgId, err: msg }, "SMS submission outcome unknown; will reconcile");
          this.metrics.smsOutboundFailed.inc({ provider: sender.name, reason: "unknown_timeout" });
          await q.updateSendAttempt(attemptId, "unknown_timeout", "", msg);
          await q.updateOutbound(msgId, Status.unknownTimeout, "", "timeout");
          return Status.unknownTimeout;
        case "permanent":
          this.log.error({ message: msgId, err: msg }, "SMS rejected by provider");
          this.metrics.smsOutboundFailed.inc({ provider: sender.name, reason: "rejected" });
          await this.store.tx(async (tq) => {
            await tq.updateSendAttempt(attemptId, "failed", "", msg);
            await tq.releaseSegments(day, Math.max(info.segments, 1));
            await tq.updateOutbound(msgId, Status.failed, "", "rejected");
          });
          return Status.failed;
        default:
          this.metrics.smsOutboundFailed.inc({ provider: sender.name, reason: "retryable" });
          return this.releaseAfterFailure(info.id, info.segments, attemptId, day, sendErr);
      }
    }
  }

  /** Puts a message back in the queue when the provider certainly did not accept it, returning the reserved segments. */
  private async releaseAfterFailure(msgId: string, segments: number, attemptId: string, day: string, cause: unknown): Promise<never> {
    const msg = cause instanceof Error ? cause.message : String(cause);
    await this.store.tx(async (q) => {
      await q.updateSendAttempt(attemptId, "failed", "", msg);
      await q.releaseSegments(day, Math.max(segments, 1));
      await q.updateOutbound(msgId, Status.queued);
    });
    throw new Error(`dispatch: ${msg}`);
  }

  // ---------------------------------------------------------------------------
  // Grounding and maintenance
  // ---------------------------------------------------------------------------

  /**
   * Optionally upserts seed snippets, then loads the matcher from the
   * database. Outside development only reviewed snippets are served.
   */
  async loadGrounding(seed: Snippet[] = []): Promise<void> {
    const q = this.store.q();
    if (seed.length > 0) {
      const n = await q.upsertSnippets(seed);
      this.log.info({ snippets: n }, "grounding seed upserted");
    }
    const reviewedOnly = !isDevelopment(this.cfg) && this.cfg.env !== "test";
    const snippets = await q.loadSnippets(reviewedOnly);
    this.matcher.replace(snippets);
    const unreviewed = snippets.filter((s) => !s.reviewed).length;
    this.log.info({ snippets: snippets.length, unreviewed, reviewed_only: reviewedOnly }, "grounding store loaded");
    this.lastReload = Date.now();
  }

  /** Periodic housekeeping; the worker calls it every minute. */
  async maintenance(): Promise<void> {
    const q = this.store.q();
    const step = async (name: string, fn: () => Promise<void>) => {
      try {
        await fn();
      } catch (err) {
        this.log.error({ err }, name);
      }
    };
    await step("failing exhausted jobs", async () => {
      const n = await q.failExhaustedJobs();
      if (n > 0) this.log.warn({ count: n }, "jobs failed after exhausting attempts");
    });
    await step("reconciling uncertain sends", () => this.reconcileUncertainSends());
    await step("gateway housekeeping", () => this.gatewayMaintenance());
    if (Date.now() - this.lastPurge > 3600_000) {
      await step("retention purge", async () => {
        const r = await q.purge(this.cfg.retentionDays);
        this.lastPurge = Date.now();
        this.log.info(r, "retention purge");
      });
    }
    if (this.cfg.groundingRefreshSeconds > 0 && Date.now() - this.lastReload > this.cfg.groundingRefreshSeconds * 1000) {
      await step("reloading grounding store", () => this.loadGrounding());
    }
    await step("spend gauge", async () => {
      const { segments, aiCost } = await q.daySpend(this.today());
      this.metrics.dailySpendUsd.set(segments * this.cfg.smsSegmentCostUsd + aiCost);
      this.metrics.dailySpendCapUsd.set(this.cfg.globalDailySpendCapUsd);
    });
  }

  /**
   * Gateway outbox upkeep: replies that waited longer than the TTL for an
   * offline phone are given up (segments released, message failed); replies
   * the phone acknowledged but never reported on are flagged for an
   * operator and never resent.
   */
  async gatewayMaintenance(): Promise<void> {
    if (this.cfg.primarySmsProvider !== "gateway") return;
    const q = this.store.q();
    const expired = await this.store.tx(async (tq) => {
      const rows = await tq.gatewayExpire(this.cfg.gatewayOutboxTtlSeconds);
      for (const r of rows) {
        await tq.releaseSegments(dayOf(r.created_at), r.segments);
        await tq.updateOutbound(r.id, Status.failed, "", "phone_offline");
      }
      return rows.length;
    });
    if (expired > 0) {
      this.metrics.gatewayOutboxExpired.inc(expired);
      this.log.warn({ count: expired, ttl_seconds: this.cfg.gatewayOutboxTtlSeconds }, "gateway phone did not collect replies in time; gave them up");
    }
    const stale = await q.gatewayFlagStale(this.cfg.gatewayAckTimeoutSeconds);
    if (stale.length > 0) {
      this.metrics.gatewayOutboxStale.inc(stale.length);
      this.log.error({ messages: stale }, "gateway phone acknowledged replies but never reported them sent; check the phone (never resent automatically)");
    }
    const [device] = await q.gatewayDevices();
    if (device) this.metrics.gatewayLastSeen.set(device.last_seen_at.getTime() / 1000);
    const silent = device ? device.seconds_since_seen : Infinity;
    if (silent > this.cfg.gatewayOfflineAfterSeconds && Date.now() - this.lastOfflineWarning > 15 * 60_000) {
      this.lastOfflineWarning = Date.now();
      this.log.warn({ seconds_since_seen: device ? Math.round(silent) : null }, "gateway phone is offline: no heartbeat");
    }
  }

  /**
   * Resolves submissions whose outcome is unknown by asking the provider,
   * when it supports lookups. Without a lookup API they stay
   * 'unknown_timeout' for an operator to decide.
   */
  async reconcileUncertainSends(): Promise<void> {
    const attempts = await this.store.q().openAttempts(this.reconcileAfterSeconds, 50);
    for (const a of attempts) {
      let sender;
      try {
        sender = this.registry.senderFor(a.provider);
      } catch {
        continue;
      }
      if (!isStatusQuerier(sender)) continue;
      let lookup;
      try {
        lookup = await sender.queryStatus(a.requestId, a.providerMessageId);
      } catch (err) {
        this.log.warn({ attempt: a.id, err: (err as Error).message }, "provider status lookup failed");
        continue;
      }
      try {
        await this.store.tx(async (q) => {
          if (lookup.found) {
            const pid = lookup.receipt?.providerMessageId ?? "";
            await q.updateSendAttempt(a.id, "reconciled_submitted", pid);
            await q.updateOutbound(a.messageId, Status.submitted, pid);
            return;
          }
          const info = await q.getForDispatch(a.messageId, true);
          await q.updateSendAttempt(a.id, "reconciled_not_found");
          await q.releaseSegments(dayOf(a.createdAt), Math.max(info.segments, 1));
          await q.updateOutbound(a.messageId, Status.queued);
          await q.createSendJob(info.conversationId, a.messageId, this.cfg.jobMaxAttempts);
          await q.notify(JOBS_CHANNEL);
        });
        this.log.info({ attempt: a.id, provider_has_it: lookup.found }, "reconciled uncertain send");
      } catch (err) {
        this.log.error({ attempt: a.id, err }, "reconciling send attempt");
      }
    }
  }
}
