package expo.modules.smsgateway

import android.content.ContentValues
import android.content.Context
import android.database.sqlite.SQLiteDatabase
import android.database.sqlite.SQLiteOpenHelper

/**
 * The phone's durable queues. Everything is written here before any network
 * call, so nothing is lost while the phone is offline or the app is killed.
 *
 *  inbox    texts received, until the server has acknowledged them
 *  outbox   replies to send, keyed by the server's message id: this key is
 *           what guarantees a reply is never sent twice, however often the
 *           server offers it
 *  statuses send/delivery results waiting to be reported
 *  events   a short activity log for the status screen (no message bodies)
 */
class GatewayDb private constructor(context: Context) :
  SQLiteOpenHelper(context.applicationContext, "smsai_gateway.db", null, 1) {

  override fun onCreate(db: SQLiteDatabase) {
    db.execSQL(
      """CREATE TABLE inbox (
        id TEXT PRIMARY KEY, sender TEXT NOT NULL, body TEXT NOT NULL,
        received_at INTEGER NOT NULL, sim TEXT, uploaded INTEGER NOT NULL DEFAULT 0)""",
    )
    db.execSQL(
      """CREATE TABLE outbox (
        id TEXT PRIMARY KEY, recipient TEXT NOT NULL, body TEXT NOT NULL,
        state TEXT NOT NULL DEFAULT 'pending', parts INTEGER NOT NULL DEFAULT 0,
        sent_parts INTEGER NOT NULL DEFAULT 0, delivered_parts INTEGER NOT NULL DEFAULT 0,
        error TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)""",
    )
    db.execSQL("CREATE TABLE statuses (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL, status TEXT NOT NULL, error TEXT, at INTEGER NOT NULL)")
    db.execSQL("CREATE TABLE events (seq INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, kind TEXT NOT NULL, text TEXT NOT NULL)")
  }

  override fun onUpgrade(db: SQLiteDatabase, oldVersion: Int, newVersion: Int) {}

  data class Inbound(val id: String, val sender: String, val body: String, val receivedAt: Long, val sim: String?)
  data class Outbound(val id: String, val recipient: String, val body: String)
  data class StatusRow(val seq: Long, val id: String, val status: String, val error: String?, val at: Long)
  data class Event(val at: Long, val kind: String, val text: String)

  // ---------------------------------------------------------------------------
  // Inbox
  // ---------------------------------------------------------------------------

  fun insertInbound(m: Inbound) {
    writableDatabase.insertWithOnConflict(
      "inbox",
      null,
      ContentValues().apply {
        put("id", m.id); put("sender", m.sender); put("body", m.body); put("received_at", m.receivedAt); put("sim", m.sim)
      },
      SQLiteDatabase.CONFLICT_IGNORE,
    )
  }

  fun pendingInbound(limit: Int): List<Inbound> =
    readableDatabase.rawQuery(
      "SELECT id, sender, body, received_at, sim FROM inbox WHERE uploaded = 0 ORDER BY received_at LIMIT ?",
      arrayOf(limit.toString()),
    ).use { c ->
      buildList { while (c.moveToNext()) add(Inbound(c.getString(0), c.getString(1), c.getString(2), c.getLong(3), c.getString(4))) }
    }

  fun markUploaded(ids: List<String>) = inTx { db ->
    for (id in ids) db.execSQL("UPDATE inbox SET uploaded = 1 WHERE id = ?", arrayOf(id))
  }

  // ---------------------------------------------------------------------------
  // Outbox
  // ---------------------------------------------------------------------------

  /** Stores offered replies; ids already known are ignored, which is what makes re-offers harmless. */
  fun insertOutbound(rows: List<Outbound>) = inTx { db ->
    val now = System.currentTimeMillis()
    for (r in rows) {
      db.insertWithOnConflict(
        "outbox",
        null,
        ContentValues().apply {
          put("id", r.id); put("recipient", r.recipient); put("body", r.body); put("created_at", now); put("updated_at", now)
        },
        SQLiteDatabase.CONFLICT_IGNORE,
      )
    }
  }

  fun pendingSends(limit: Int): List<Outbound> =
    readableDatabase.rawQuery(
      "SELECT id, recipient, body FROM outbox WHERE state = 'pending' ORDER BY created_at LIMIT ?",
      arrayOf(limit.toString()),
    ).use { c -> buildList { while (c.moveToNext()) add(Outbound(c.getString(0), c.getString(1), c.getString(2))) } }

  /**
   * Claims a pending reply for sending. Returns false if it is no longer
   * pending. A row left in 'sending' after a crash is never retried: its
   * outcome is unknown, and the server flags it for the operator.
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

  /** One part left the phone (or failed to). Queues 'sent' once every part is out, 'failed' on the first failure. */
  fun onPartSent(id: String, ok: Boolean, error: String?) = inTx { db ->
    val (state, parts, sent) = outboxState(db, id) ?: return@inTx
    if (state != "sending") return@inTx
    if (!ok) {
      setState(db, id, "failed", error)
      queueStatus(db, id, "failed", error)
      return@inTx
    }
    db.execSQL("UPDATE outbox SET sent_parts = sent_parts + 1, updated_at = ? WHERE id = ?", arrayOf(System.currentTimeMillis(), id))
    if (sent + 1 >= parts) {
      setState(db, id, "sent", null)
      queueStatus(db, id, "sent", null)
      // Delivery reports can overtake the last "sent" callback.
      val delivered = db.rawQuery("SELECT delivered_parts FROM outbox WHERE id = ?", arrayOf(id)).use { c -> if (c.moveToFirst()) c.getInt(0) else 0 }
      if (delivered >= parts) {
        setState(db, id, "delivered", null)
        queueStatus(db, id, "delivered", null)
      }
    }
  }

  /** A delivery report for one part. Queues 'delivered' once every part arrived. */
  fun onPartDelivered(id: String, ok: Boolean, error: String?) = inTx { db ->
    val (state, parts, _) = outboxState(db, id) ?: return@inTx
    if (state != "sent" && state != "sending") return@inTx
    if (!ok) {
      setState(db, id, "failed", error)
      queueStatus(db, id, "failed", error)
      return@inTx
    }
    db.execSQL("UPDATE outbox SET delivered_parts = delivered_parts + 1, updated_at = ? WHERE id = ?", arrayOf(System.currentTimeMillis(), id))
    val delivered = db.rawQuery("SELECT delivered_parts FROM outbox WHERE id = ?", arrayOf(id)).use { c -> if (c.moveToFirst()) c.getInt(0) else 0 }
    if (state == "sent" && delivered >= parts) {
      setState(db, id, "delivered", null)
      queueStatus(db, id, "delivered", null)
    }
  }

  private fun outboxState(db: SQLiteDatabase, id: String): Triple<String, Int, Int>? =
    db.rawQuery("SELECT state, parts, sent_parts FROM outbox WHERE id = ?", arrayOf(id)).use { c ->
      if (c.moveToFirst()) Triple(c.getString(0), c.getInt(1), c.getInt(2)) else null
    }

  private fun setState(db: SQLiteDatabase, id: String, state: String, error: String?) {
    db.execSQL("UPDATE outbox SET state = ?, error = coalesce(?, error), updated_at = ? WHERE id = ?", arrayOf(state, error, System.currentTimeMillis(), id))
  }

  // ---------------------------------------------------------------------------
  // Statuses
  // ---------------------------------------------------------------------------

  private fun queueStatus(db: SQLiteDatabase, id: String, status: String, error: String?) {
    db.insert("statuses", null, ContentValues().apply { put("id", id); put("status", status); put("error", error); put("at", System.currentTimeMillis()) })
  }

  fun pendingStatuses(limit: Int): List<StatusRow> =
    readableDatabase.rawQuery("SELECT seq, id, status, error, at FROM statuses ORDER BY seq LIMIT ?", arrayOf(limit.toString())).use { c ->
      buildList { while (c.moveToNext()) add(StatusRow(c.getLong(0), c.getString(1), c.getString(2), c.getString(3), c.getLong(4))) }
    }

  fun deleteStatusesThrough(seq: Long) {
    writableDatabase.execSQL("DELETE FROM statuses WHERE seq <= ?", arrayOf(seq))
  }

  // ---------------------------------------------------------------------------
  // Counts, events, upkeep
  // ---------------------------------------------------------------------------

  data class Counts(val inboxPending: Int, val outboxPending: Int, val sending: Int, val statusPending: Int, val sentToday: Int, val receivedToday: Int)

  fun counts(): Counts {
    val db = readableDatabase
    fun n(sql: String, vararg args: String) = db.rawQuery(sql, args).use { c -> if (c.moveToFirst()) c.getInt(0) else 0 }
    val dayStart = (System.currentTimeMillis() / 86_400_000L * 86_400_000L).toString()
    return Counts(
      inboxPending = n("SELECT count(*) FROM inbox WHERE uploaded = 0"),
      outboxPending = n("SELECT count(*) FROM outbox WHERE state = 'pending'"),
      sending = n("SELECT count(*) FROM outbox WHERE state = 'sending'"),
      statusPending = n("SELECT count(*) FROM statuses"),
      sentToday = n("SELECT count(*) FROM outbox WHERE state IN ('sent', 'delivered') AND updated_at >= ?", dayStart),
      receivedToday = n("SELECT count(*) FROM inbox WHERE received_at >= ?", dayStart),
    )
  }

  fun addEvent(kind: String, text: String) = inTx { db ->
    db.insert("events", null, ContentValues().apply { put("at", System.currentTimeMillis()); put("kind", kind); put("text", text) })
    db.execSQL("DELETE FROM events WHERE seq <= (SELECT max(seq) - 200 FROM events)")
  }

  fun recentEvents(limit: Int): List<Event> =
    readableDatabase.rawQuery("SELECT at, kind, text FROM events ORDER BY seq DESC LIMIT ?", arrayOf(limit.toString())).use { c ->
      buildList { while (c.moveToNext()) add(Event(c.getLong(0), c.getString(1), c.getString(2))) }
    }

  /** Drops message copies the server already has (after two days). */
  fun prune() = inTx { db ->
    val cutoff = System.currentTimeMillis() - 2 * 86_400_000L
    db.execSQL("DELETE FROM inbox WHERE uploaded = 1 AND received_at < ?", arrayOf(cutoff))
    db.execSQL("DELETE FROM outbox WHERE state IN ('sent', 'delivered', 'failed') AND updated_at < ?", arrayOf(cutoff))
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
