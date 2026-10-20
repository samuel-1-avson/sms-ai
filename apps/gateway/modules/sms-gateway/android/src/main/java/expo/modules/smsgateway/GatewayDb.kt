package expo.modules.smsgateway

import android.content.ContentValues
import android.content.Context
import android.database.sqlite.SQLiteDatabase
import android.database.sqlite.SQLiteOpenHelper
import expo.modules.smsgateway.brain.BrainStore
import expo.modules.smsgateway.brain.Commit
import expo.modules.smsgateway.brain.Inbound
import expo.modules.smsgateway.brain.Person
import expo.modules.smsgateway.brain.Turn
import expo.modules.smsgateway.brain.Usage

/**
 * Everything the phone knows, in one SQLite file. Texts are written here the
 * moment they arrive, so nothing is lost while the phone is offline or the
 * app is killed.
 *
 *  inbox    received texts: new -> answering -> done | ignored | expired
 *  outbox   replies to send, keyed by an id derived from the inbox; this key
 *           is what guarantees a reply is never queued or sent twice
 *  people   per-number state: opt-out, daily count, the saved MORE text
 *  turns    conversation memory shown to the AI
 *  usage    daily totals behind the caps
 *  events   a short activity log for the status screen (no message bodies)
 */
class GatewayDb private constructor(context: Context) :
  SQLiteOpenHelper(context.applicationContext, "smsai_gateway.db", null, 2), BrainStore {

  override fun onCreate(db: SQLiteDatabase) {
    db.execSQL(
      """CREATE TABLE inbox (
        id TEXT PRIMARY KEY, sender TEXT NOT NULL, body TEXT NOT NULL, received_at INTEGER NOT NULL, sim TEXT,
        state TEXT NOT NULL DEFAULT 'new', attempts INTEGER NOT NULL DEFAULT 0, next_try_at INTEGER NOT NULL DEFAULT 0)""",
    )
    db.execSQL("CREATE INDEX inbox_state ON inbox (state, sender)")
    db.execSQL(
      """CREATE TABLE outbox (
        id TEXT PRIMARY KEY, recipient TEXT NOT NULL, body TEXT NOT NULL,
        state TEXT NOT NULL DEFAULT 'pending', parts INTEGER NOT NULL DEFAULT 0,
        sent_parts INTEGER NOT NULL DEFAULT 0, delivered_parts INTEGER NOT NULL DEFAULT 0,
        error TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)""",
    )
    db.execSQL(
      """CREATE TABLE people (
        sender TEXT PRIMARY KEY, opted_out INTEGER NOT NULL DEFAULT 0, day TEXT NOT NULL DEFAULT '',
        questions_today INTEGER NOT NULL DEFAULT 0, limit_notice_day TEXT NOT NULL DEFAULT '',
        context_from INTEGER NOT NULL DEFAULT 0, more_rest TEXT NOT NULL DEFAULT '', more_pages INTEGER NOT NULL DEFAULT 0)""",
    )
    db.execSQL("CREATE TABLE turns (seq INTEGER PRIMARY KEY AUTOINCREMENT, sender TEXT NOT NULL, role TEXT NOT NULL, text TEXT NOT NULL, at INTEGER NOT NULL)")
    db.execSQL("CREATE INDEX turns_sender ON turns (sender, at)")
    db.execSQL(
      """CREATE TABLE usage (
        day TEXT PRIMARY KEY, questions INTEGER NOT NULL DEFAULT 0, sms_parts INTEGER NOT NULL DEFAULT 0,
        ai_cost_usd REAL NOT NULL DEFAULT 0, ai_calls INTEGER NOT NULL DEFAULT 0, ai_failures INTEGER NOT NULL DEFAULT 0)""",
    )
    db.execSQL("CREATE TABLE events (seq INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, kind TEXT NOT NULL, text TEXT NOT NULL)")
  }

  // Version 1 was the relay build, which only queued texts for a server and
  // never ran for real: start again.
  override fun onUpgrade(db: SQLiteDatabase, oldVersion: Int, newVersion: Int) {
    for (t in listOf("inbox", "outbox", "statuses", "events", "people", "turns", "usage")) db.execSQL("DROP TABLE IF EXISTS $t")
    onCreate(db)
  }

  data class Outbound(val id: String, val recipient: String, val body: String)
  data class Event(val at: Long, val kind: String, val text: String)

  // ---------------------------------------------------------------------------
  // Inbox
  // ---------------------------------------------------------------------------

  fun insertInbound(id: String, sender: String, body: String, receivedAt: Long, sim: String?) {
    writableDatabase.insertWithOnConflict(
      "inbox",
      null,
      ContentValues().apply {
        put("id", id); put("sender", sender); put("body", body); put("received_at", receivedAt); put("sim", sim)
      },
      SQLiteDatabase.CONFLICT_IGNORE,
    )
  }

  /** After a crash or restart: texts that were mid-answer are answered again (no reply had been queued yet). */
  fun resetAnswering() {
    writableDatabase.execSQL("UPDATE inbox SET state = 'new' WHERE state = 'answering'")
  }

  /** When the next held or retried text becomes due, or null if none is waiting. */
  fun nextDueAt(): Long? =
    readableDatabase.rawQuery("SELECT min(next_try_at) FROM inbox WHERE state = 'new'", null).use { c ->
      if (c.moveToFirst() && !c.isNull(0)) c.getLong(0) else null
    }

  // ---------------------------------------------------------------------------
  // BrainStore
  // ---------------------------------------------------------------------------

  override fun nextReadySender(now: Long, debounceMs: Long): String? =
    readableDatabase.rawQuery(
      """SELECT sender FROM inbox WHERE state = 'new' GROUP BY sender
         HAVING max(received_at) <= ? AND min(next_try_at) <= ? ORDER BY min(received_at) LIMIT 1""",
      arrayOf((now - debounceMs).toString(), now.toString()),
    ).use { c -> if (c.moveToFirst()) c.getString(0) else null }

  override fun pending(sender: String, limit: Int): List<Inbound> =
    readableDatabase.rawQuery(
      "SELECT id, sender, body, received_at, attempts FROM inbox WHERE state = 'new' AND sender = ? ORDER BY received_at, rowid LIMIT ?",
      arrayOf(sender, limit.toString()),
    ).use { c -> buildList { while (c.moveToNext()) add(Inbound(c.getString(0), c.getString(1), c.getString(2), c.getLong(3), c.getInt(4))) } }

  override fun person(sender: String): Person? =
    readableDatabase.rawQuery(
      "SELECT opted_out, day, questions_today, limit_notice_day, context_from, more_rest, more_pages FROM people WHERE sender = ?",
      arrayOf(sender),
    ).use { c ->
      if (!c.moveToFirst()) null
      else Person(sender, c.getInt(0) != 0, c.getString(1), c.getInt(2), c.getString(3), c.getLong(4), c.getString(5), c.getInt(6))
    }

  override fun usage(day: String): Usage =
    readableDatabase.rawQuery("SELECT questions, sms_parts, ai_cost_usd, ai_calls, ai_failures FROM usage WHERE day = ?", arrayOf(day)).use { c ->
      if (c.moveToFirst()) Usage(c.getInt(0), c.getInt(1), c.getDouble(2), c.getInt(3), c.getInt(4)) else Usage()
    }

  override fun turns(sender: String, since: Long, limit: Int): List<Turn> =
    readableDatabase.rawQuery(
      "SELECT role, text FROM (SELECT seq, role, text FROM turns WHERE sender = ? AND at >= ? ORDER BY seq DESC LIMIT ?) ORDER BY seq",
      arrayOf(sender, since.toString(), limit.toString()),
    ).use { c -> buildList { while (c.moveToNext()) add(Turn(c.getString(0), c.getString(1))) } }

  override fun claim(ids: List<String>) = inTx { db ->
    for (id in ids) db.execSQL("UPDATE inbox SET state = 'answering', attempts = attempts + 1 WHERE id = ?", arrayOf(id))
  }

  override fun retryLater(ids: List<String>, nextTryAt: Long) = inTx { db ->
    for (id in ids) db.execSQL("UPDATE inbox SET state = 'new', next_try_at = ? WHERE id = ?", arrayOf(nextTryAt, id))
  }

  override fun addAiUsage(day: String, costUsd: Double, calls: Int, failures: Int) = inTx { db ->
    ensureDay(db, day)
    db.execSQL(
      "UPDATE usage SET ai_cost_usd = ai_cost_usd + ?, ai_calls = ai_calls + ?, ai_failures = ai_failures + ? WHERE day = ?",
      arrayOf(costUsd, calls, failures, day),
    )
  }

  override fun commit(day: String, c: Commit) = inTx { db ->
    val now = System.currentTimeMillis()
    for (id in c.inboxIds) db.execSQL("UPDATE inbox SET state = ? WHERE id = ?", arrayOf(c.inboxState, id))
    ensureDay(db, day)
    if (c.reply != null) {
      val inserted = db.insertWithOnConflict(
        "outbox",
        null,
        ContentValues().apply {
          put("id", c.reply.id); put("recipient", c.reply.to); put("body", c.reply.body); put("created_at", now); put("updated_at", now)
        },
        SQLiteDatabase.CONFLICT_IGNORE,
      )
      if (inserted != -1L) db.execSQL("UPDATE usage SET sms_parts = sms_parts + ? WHERE day = ?", arrayOf(c.reply.parts, day))
    }
    c.person?.let { p ->
      if (c.forget) {
        db.execSQL("DELETE FROM turns WHERE sender = ?", arrayOf(p.sender))
        db.execSQL("DELETE FROM inbox WHERE sender = ? AND state NOT IN ('new', 'answering')", arrayOf(p.sender))
        db.execSQL("DELETE FROM outbox WHERE recipient = ? AND state IN ('sent', 'delivered', 'failed')", arrayOf(c.reply?.to ?: p.sender))
      }
      db.insertWithOnConflict(
        "people",
        null,
        ContentValues().apply {
          put("sender", p.sender); put("opted_out", if (p.optedOut) 1 else 0); put("day", p.day); put("questions_today", p.questionsToday)
          put("limit_notice_day", p.limitNoticeDay); put("context_from", p.contextFrom); put("more_rest", p.moreRest); put("more_pages", p.morePages)
        },
        SQLiteDatabase.CONFLICT_REPLACE,
      )
      for (t in c.turns) {
        db.insert("turns", null, ContentValues().apply { put("sender", p.sender); put("role", t.role); put("text", t.content); put("at", now) })
      }
    }
    if (c.questions > 0) db.execSQL("UPDATE usage SET questions = questions + ? WHERE day = ?", arrayOf(c.questions, day))
    c.event?.let { insertEvent(db, it.first, it.second) }
  }

  override fun event(kind: String, text: String) = addEvent(kind, text)

  private fun ensureDay(db: SQLiteDatabase, day: String) {
    db.execSQL("INSERT OR IGNORE INTO usage (day) VALUES (?)", arrayOf(day))
  }

  // ---------------------------------------------------------------------------
  // Outbox
  // ---------------------------------------------------------------------------

  fun pendingSends(limit: Int): List<Outbound> =
    readableDatabase.rawQuery(
      "SELECT id, recipient, body FROM outbox WHERE state = 'pending' ORDER BY created_at LIMIT ?",
      arrayOf(limit.toString()),
    ).use { c -> buildList { while (c.moveToNext()) add(Outbound(c.getString(0), c.getString(1), c.getString(2))) } }

  /**
   * Claims a pending reply for sending. Returns false if it is no longer
   * pending. A row left in 'sending' after a crash is never retried: it may
   * already have gone out, and a second copy is worse than a missing one.
   */
  fun markSending(id: String, parts: Int): Boolean {
    var claimed = false
    inTx { db ->
      db.compileStatement("UPDATE outbox SET state = 'sending', parts = ?, updated_at = ? WHERE id = ? AND state = 'pending'").use { st ->
        st.bindLong(1, parts.toLong()); st.bindLong(2, System.currentTimeMillis()); st.bindString(3, id)
        claimed = st.executeUpdateDelete() == 1
      }
    }
    return claimed
  }

  /** One part left the phone (or failed to). The reply is 'sent' once every part is out, 'failed' on the first failure. */
  fun onPartSent(id: String, ok: Boolean, error: String?) = inTx { db ->
    val (state, parts, sent) = outboxState(db, id) ?: return@inTx
    if (state != "sending") return@inTx
    if (!ok) {
      setState(db, id, "failed", error)
      return@inTx
    }
    db.execSQL("UPDATE outbox SET sent_parts = sent_parts + 1, updated_at = ? WHERE id = ?", arrayOf(System.currentTimeMillis(), id))
    if (sent + 1 >= parts) {
      // Delivery reports can overtake the last "sent" callback.
      val delivered = db.rawQuery("SELECT delivered_parts FROM outbox WHERE id = ?", arrayOf(id)).use { c -> if (c.moveToFirst()) c.getInt(0) else 0 }
      setState(db, id, if (delivered >= parts) "delivered" else "sent", null)
    }
  }

  /** A delivery report for one part. The reply is 'delivered' once every part arrived. */
  fun onPartDelivered(id: String, ok: Boolean, error: String?) = inTx { db ->
    val (state, parts, _) = outboxState(db, id) ?: return@inTx
    if (state != "sent" && state != "sending") return@inTx
    if (!ok) {
      setState(db, id, "failed", error)
      return@inTx
    }
    db.execSQL("UPDATE outbox SET delivered_parts = delivered_parts + 1, updated_at = ? WHERE id = ?", arrayOf(System.currentTimeMillis(), id))
    val delivered = db.rawQuery("SELECT delivered_parts FROM outbox WHERE id = ?", arrayOf(id)).use { c -> if (c.moveToFirst()) c.getInt(0) else 0 }
    if (state == "sent" && delivered >= parts) setState(db, id, "delivered", null)
  }

  private fun outboxState(db: SQLiteDatabase, id: String): Triple<String, Int, Int>? =
    db.rawQuery("SELECT state, parts, sent_parts FROM outbox WHERE id = ?", arrayOf(id)).use { c ->
      if (c.moveToFirst()) Triple(c.getString(0), c.getInt(1), c.getInt(2)) else null
    }

  private fun setState(db: SQLiteDatabase, id: String, state: String, error: String?) {
    db.execSQL("UPDATE outbox SET state = ?, error = coalesce(?, error), updated_at = ? WHERE id = ?", arrayOf(state, error, System.currentTimeMillis(), id))
  }

  // ---------------------------------------------------------------------------
  // Counts, events, upkeep
  // ---------------------------------------------------------------------------

  data class Counts(val waiting: Int, val outboxPending: Int, val sending: Int, val sentToday: Int, val failedToday: Int, val receivedToday: Int)

  fun counts(): Counts {
    val db = readableDatabase
    fun n(sql: String, vararg args: String) = db.rawQuery(sql, args).use { c -> if (c.moveToFirst()) c.getInt(0) else 0 }
    val dayStart = (System.currentTimeMillis() / 86_400_000L * 86_400_000L).toString()
    return Counts(
      waiting = n("SELECT count(*) FROM inbox WHERE state IN ('new', 'answering')"),
      outboxPending = n("SELECT count(*) FROM outbox WHERE state = 'pending'"),
      sending = n("SELECT count(*) FROM outbox WHERE state = 'sending'"),
      sentToday = n("SELECT count(*) FROM outbox WHERE state IN ('sent', 'delivered') AND updated_at >= ?", dayStart),
      failedToday = n("SELECT count(*) FROM outbox WHERE state = 'failed' AND updated_at >= ?", dayStart),
      receivedToday = n("SELECT count(*) FROM inbox WHERE received_at >= ?", dayStart),
    )
  }

  fun addEvent(kind: String, text: String) = inTx { db -> insertEvent(db, kind, text) }

  private fun insertEvent(db: SQLiteDatabase, kind: String, text: String) {
    db.insert("events", null, ContentValues().apply { put("at", System.currentTimeMillis()); put("kind", kind); put("text", text) })
    db.execSQL("DELETE FROM events WHERE seq <= (SELECT max(seq) - 200 FROM events)")
  }

  fun recentEvents(limit: Int): List<Event> =
    readableDatabase.rawQuery("SELECT at, kind, text FROM events ORDER BY seq DESC LIMIT ?", arrayOf(limit.toString())).use { c ->
      buildList { while (c.moveToNext()) add(Event(c.getLong(0), c.getString(1), c.getString(2))) }
    }

  /** Keeps the database small: handled texts for 2 days, conversations for 30, daily totals for 90. */
  fun prune() = inTx { db ->
    val now = System.currentTimeMillis()
    val twoDays = now - 2 * 86_400_000L
    db.execSQL("DELETE FROM inbox WHERE state IN ('done', 'ignored', 'expired') AND received_at < ?", arrayOf(twoDays))
    db.execSQL("DELETE FROM outbox WHERE state IN ('sent', 'delivered', 'failed') AND updated_at < ?", arrayOf(twoDays))
    db.execSQL("DELETE FROM turns WHERE at < ?", arrayOf(now - 30 * 86_400_000L))
    db.execSQL("DELETE FROM usage WHERE day < ?", arrayOf(expo.modules.smsgateway.brain.Brain.dayOf(now - 90 * 86_400_000L)))
  }

  private inline fun inTx(block: (SQLiteDatabase) -> Unit) {
    synchronized(this) {
      val db = writableDatabase
      db.beginTransaction()
      try {
        block(db)
        db.setTransactionSuccessful()
      } finally {
        db.endTransaction()
      }
    }
  }

  companion object {
    @Volatile private var instance: GatewayDb? = null

    fun get(context: Context): GatewayDb =
      instance ?: synchronized(this) { instance ?: GatewayDb(context).also { instance = it } }
  }
}
