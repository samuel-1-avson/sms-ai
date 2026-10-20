package expo.modules.smsgateway.brain

import java.util.Calendar
import java.util.Locale
import java.util.TimeZone

/**
 * The phone's answering engine. Each [step] takes the texts of one sender,
 * decides what to do (ignore, run a command, ask the AI, or hold because a cap
 * was reached) and commits the outcome in one go, so a crash at any point can
 * cause at most a repeated AI call, never a duplicate SMS.
 *
 * It is pure Kotlin behind [BrainStore] so it can be tested off the phone.
 */

/** A received text waiting in the inbox. */
data class Inbound(val id: String, val sender: String, val body: String, val receivedAt: Long, val attempts: Int)

/** Per-number state. [day] is the UTC day the counters belong to. */
data class Person(
  val sender: String,
  val optedOut: Boolean = false,
  val day: String = "",
  val questionsToday: Int = 0,
  val limitNoticeDay: String = "",
  /** Turns before this time are not shown to the model (set by NEW). */
  val contextFrom: Long = 0,
  val moreRest: String = "",
  val morePages: Int = 0,
)

data class Usage(val questions: Int = 0, val smsParts: Int = 0, val aiCostUsd: Double = 0.0, val aiCalls: Int = 0, val aiFailures: Int = 0)

/** A reply to send. Its id is derived from the inbox, so committing it twice cannot queue two SMS. */
data class Reply(val id: String, val to: String, val body: String, val parts: Int)

/** Everything one step changes, applied atomically by [BrainStore.commit]. */
data class Commit(
  val inboxIds: List<String>,
  /** "done", "ignored" or "expired". */
  val inboxState: String,
  val reply: Reply? = null,
  val person: Person? = null,
  /** Turns to append for [person]'s sender (user question, then answer). */
  val turns: List<Turn> = emptyList(),
  /** DELETE: remove the sender's turns and stored messages. */
  val forget: Boolean = false,
  val questions: Int = 0,
  val event: Pair<String, String>? = null,
)

interface BrainStore {
  /** A sender with a 'new' text that is at least [debounceMs] old and due ([nextTryAt] <= now). */
  fun nextReadySender(now: Long, debounceMs: Long): String?

  /** The sender's 'new' texts, oldest first. */
  fun pending(sender: String, limit: Int): List<Inbound>

  fun person(sender: String): Person?

  fun usage(day: String): Usage

  /** The sender's turns at or after [since], oldest first, at most [limit]. */
  fun turns(sender: String, since: Long, limit: Int): List<Turn>

  /** Marks texts as being answered (attempts + 1). */
  fun claim(ids: List<String>)

  /** Puts texts back to 'new', due again at [nextTryAt]. */
  fun retryLater(ids: List<String>, nextTryAt: Long)

  /** Adds AI spend as soon as it happens, even if the answer is never sent. */
  fun addAiUsage(day: String, costUsd: Double, calls: Int, failures: Int)

  fun commit(day: String, c: Commit)

  fun event(kind: String, text: String)
}

data class BrainSettings(
  val ownNumber: String? = null,
  val dailyPerNumberCap: Int = 15,
  val dailySmsPartCap: Int = 200,
  val dailySpendCapUsd: Double = 2.0,
  val maxMorePages: Int = 1,
  val contextTtlMs: Long = 24 * 3_600_000L,
  val contextMaxTurns: Int = 6,
  val contextMaxInputTokens: Int = 800,
  val debounceMs: Long = 2_500,
  val maxCoalesce: Int = 3,
  /** After this long without an answer the person is told "Sorry...". */
  val giveUpAfterMs: Long = 30 * 60_000L,
  /** Questions older than this are dropped without a reply (e.g. after the phone was off for a day). */
  val expireAfterMs: Long = 24 * 3_600_000L,
)

/** What a step did, for logs and tests. */
enum class StepResult { IDLE, IGNORED, COMMAND, ANSWERED, LIMITED, HELD, RETRY, FAILED, EXPIRED }

const val MAX_QUESTION_CHARS = 600

class Brain(
  private val store: BrainStore,
  private val settings: () -> BrainSettings,
  private val ai: (List<ChatMessage>) -> Outcome,
  private val now: () -> Long = System::currentTimeMillis,
) {
  /** Why the last AI attempt failed, or null after a successful answer. */
  @Volatile var lastError: String? = null
    private set

  /** Handles the next ready sender, if any. */
  fun step(): StepResult {
    val cfg = settings()
    val t = now()
    val sender = store.nextReadySender(t, cfg.debounceMs) ?: return StepResult.IDLE
    val rows = store.pending(sender, 20)
    if (rows.isEmpty()) return StepResult.IDLE
    val day = dayOf(t)

    val to = replyableSender(sender, cfg.ownNumber)
    if (to == null) {
      store.commit(day, Commit(rows.map { it.id }, "ignored", event = "ignore" to "Ignored a text from ${label(sender)} (not a person)"))
      return StepResult.IGNORED
    }
    var person = (store.person(sender) ?: Person(sender)).let { if (it.day != day) it.copy(day = day, questionsToday = 0) else it }

    val first = rows.first()
    val cmd = parseCommand(first.body)
    if (person.optedOut && cmd != Command.START) {
      store.commit(day, Commit(listOf(first.id), "ignored"))
      return StepResult.IGNORED
    }
    if (t - first.receivedAt > cfg.expireAfterMs) {
      store.commit(day, Commit(listOf(first.id), "expired", event = "expire" to "Dropped a day-old text from ${maskNumber(to)}"))
      return StepResult.EXPIRED
    }
    if (store.usage(day).smsParts >= cfg.dailySmsPartCap) return hold(rows.map { it.id }, "Daily SMS cap (${cfg.dailySmsPartCap} parts) reached")

    if (cmd != null) return runCommand(day, to, person, first, cmd)

    // Consecutive questions (a burst, or a long text the network split) are answered together.
    val group = rows.takeWhile { parseCommand(it.body) == null }.take(cfg.maxCoalesce.coerceAtLeast(1))
    val ids = group.map { it.id }
    val replyId = group.last().id + ":r"

    if (person.questionsToday >= cfg.dailyPerNumberCap) {
      val notice = person.limitNoticeDay != day
      person = person.copy(limitNoticeDay = day)
      val reply = if (notice) reply(group.last().id + ":limit", to, Texts.limitReached(person.questionsToday, cfg.dailyPerNumberCap)) else null
      store.commit(day, Commit(ids, "done", reply = reply, person = person))
      return StepResult.LIMITED
    }
    if (store.usage(day).aiCostUsd >= cfg.dailySpendCapUsd) {
      return hold(ids, "Daily AI spend cap (US\$${String.format(Locale.US, "%.2f", cfg.dailySpendCapUsd)}) reached")
    }

    val question = truncateQuestion(scrub(group.joinToString(" ") { jsTrim(it.body) }), MAX_QUESTION_CHARS)
    val history = store.turns(sender, maxOf(person.contextFrom, t - cfg.contextTtlMs), cfg.contextMaxTurns * 2)
    val messages = buildPrompt(question, history, cfg.contextMaxInputTokens, t)

    store.claim(ids)
    val text: String
    try {
      val out = ai(messages)
      store.addAiUsage(day, out.calls.sumOf { it.costUsd }, out.calls.size, out.calls.count { !it.ok })
      text = sanitize(out.text)
      if (text.isEmpty()) throw AnswerError(AiError(AiCode.EMPTY, "nothing left after sanitizing"), emptyList())
    } catch (e: AnswerError) {
      if (e.calls.isNotEmpty()) store.addAiUsage(day, e.calls.sumOf { it.costUsd }, e.calls.size, e.calls.count { !it.ok })
      return failed(day, to, group, replyId, e.error)
    } catch (e: Exception) {
      return failed(day, to, group, replyId, AiError(AiCode.UPSTREAM, "${e.javaClass.simpleName}: ${e.message}"))
    }

    val (page, rest) = if (cfg.maxMorePages <= 0) truncate(text) to "" else splitFirst(text)
    person = person.copy(
      questionsToday = person.questionsToday + 1,
      moreRest = rest,
      morePages = if (rest.isEmpty()) 0 else cfg.maxMorePages,
    )
    store.commit(
      day,
      Commit(
        ids, "done",
        reply = reply(replyId, to, page),
        person = person,
        turns = listOf(Turn("user", question), Turn("assistant", text)),
        questions = 1,
        event = "answer" to "Answered ${maskNumber(to)}" + if (rest.isNotEmpty()) " (MORE saved)" else "",
      ),
    )
    lastError = null
    return StepResult.ANSWERED
  }

  private fun runCommand(day: String, to: String, p: Person, row: Inbound, cmd: Command): StepResult {
    var person = p
    var forget = false
    val text = when (cmd) {
      Command.HELP -> Texts.HELP
      Command.NEW -> {
        person = person.copy(contextFrom = now(), moreRest = "", morePages = 0)
        Texts.NEW_CHAT
      }
      Command.MORE -> {
        if (person.moreRest.isEmpty() || person.morePages <= 0) {
          Texts.NO_MORE
        } else {
          val (page, rest) = if (person.morePages <= 1) truncate(person.moreRest) to "" else splitFirst(person.moreRest)
          person = person.copy(moreRest = rest, morePages = if (rest.isEmpty()) 0 else person.morePages - 1)
          page
        }
      }
      Command.STOP -> {
        person = person.copy(optedOut = true)
        Texts.STOP
      }
      Command.START -> {
        person = person.copy(optedOut = false)
        Texts.START
      }
      Command.DELETE -> {
        forget = true
        person = Person(person.sender, optedOut = person.optedOut, day = person.day, questionsToday = person.questionsToday, limitNoticeDay = person.limitNoticeDay)
        Texts.DELETED
      }
    }
    store.commit(day, Commit(listOf(row.id), "done", reply = reply(row.id + ":r", to, text), person = person, forget = forget, event = "command" to "${cmd.name} from ${maskNumber(to)}"))
    return StepResult.COMMAND
  }

  private fun failed(day: String, to: String, group: List<Inbound>, replyId: String, error: Throwable): StepResult {
    val cfg = settings()
    val t = now()
    lastError = (error.message ?: error.toString()).removePrefix("ai: ")
    val code = (error as? AiError)?.code
    val hopeless = code == AiCode.UNAUTHORIZED || code == AiCode.QUOTA || (error is AiError && !isRetryable(error))
    val oldest = group.minOf { it.receivedAt }
    if (hopeless || t - oldest >= cfg.giveUpAfterMs) {
      store.commit(
        day,
        Commit(group.map { it.id }, "done", reply = reply(replyId, to, Texts.FAILED), event = "error" to "Could not answer ${maskNumber(to)}: ${error.message}"),
      )
      return StepResult.FAILED
    }
    val attempts = group.maxOf { it.attempts } + 1
    val backoff = (5_000L shl (attempts - 1).coerceIn(0, 6)).coerceAtMost(5 * 60_000L)
    store.retryLater(group.map { it.id }, t + backoff)
    store.event("error", "AI failed, retrying in ${backoff / 1000}s: ${error.message}")
    return StepResult.RETRY
  }

  private fun hold(ids: List<String>, why: String): StepResult {
    store.retryLater(ids, now() + 10 * 60_000L)
    store.event("limit", "$why; texts are waiting")
    return StepResult.HELD
  }

  private fun reply(id: String, to: String, body: String) = Reply(id, to, body, segmentCount(body))

  private fun label(sender: String) = if (sender.any { it.isLetter() }) sender.take(20) else maskNumber(sender)

  companion object {
    /** The UTC day, e.g. "2026-10-20", that daily counters belong to. */
    fun dayOf(ms: Long): String {
      val c = Calendar.getInstance(TimeZone.getTimeZone("UTC")).apply { timeInMillis = ms }
      return String.format(Locale.US, "%04d-%02d-%02d", c.get(Calendar.YEAR), c.get(Calendar.MONTH) + 1, c.get(Calendar.DAY_OF_MONTH))
    }
  }
}
