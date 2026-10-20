import expo.modules.smsgateway.brain.AiCode
import expo.modules.smsgateway.brain.AiError
import expo.modules.smsgateway.brain.AnswerError
import expo.modules.smsgateway.brain.Brain
import expo.modules.smsgateway.brain.BrainSettings
import expo.modules.smsgateway.brain.BrainStore
import expo.modules.smsgateway.brain.CallRecord
import expo.modules.smsgateway.brain.ChatMessage
import expo.modules.smsgateway.brain.Commit
import expo.modules.smsgateway.brain.Encoding
import expo.modules.smsgateway.brain.Inbound
import expo.modules.smsgateway.brain.Outcome
import expo.modules.smsgateway.brain.Person
import expo.modules.smsgateway.brain.Reply
import expo.modules.smsgateway.brain.StepResult
import expo.modules.smsgateway.brain.Texts
import expo.modules.smsgateway.brain.Turn
import expo.modules.smsgateway.brain.Usage
import expo.modules.smsgateway.brain.measure
import expo.modules.smsgateway.brain.segmentCount
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

/** An in-memory BrainStore with the same semantics as the phone's SQLite one. */
class FakeStore : BrainStore {
  data class Row(val m: Inbound, var state: String = "new", var nextTryAt: Long = 0, var attempts: Int = 0)

  val inbox = mutableListOf<Row>()
  val outbox = linkedMapOf<String, Reply>()
  val people = mutableMapOf<String, Person>()
  val turns = mutableMapOf<String, MutableList<Pair<Long, Turn>>>()
  val usage = mutableMapOf<String, Usage>()
  val events = mutableListOf<String>()
  var clock = 0L

  fun receive(id: String, sender: String, body: String, at: Long) {
    inbox += Row(Inbound(id, sender, body, at, 0))
  }

  override fun nextReadySender(now: Long, debounceMs: Long): String? =
    inbox.filter { it.state == "new" }.groupBy { it.m.sender }
      .filter { (_, rows) -> rows.maxOf { it.m.receivedAt } <= now - debounceMs && rows.minOf { it.nextTryAt } <= now }
      .minByOrNull { (_, rows) -> rows.minOf { it.m.receivedAt } }?.key

  override fun pending(sender: String, limit: Int) =
    inbox.filter { it.state == "new" && it.m.sender == sender }.sortedBy { it.m.receivedAt }.take(limit).map { it.m.copy(attempts = it.attempts) }

  override fun person(sender: String) = people[sender]

  override fun usage(day: String) = usage[day] ?: Usage()

  override fun turns(sender: String, since: Long, limit: Int) =
    (turns[sender] ?: mutableListOf()).filter { it.first >= since }.map { it.second }.takeLast(limit)

  override fun claim(ids: List<String>) = inbox.filter { it.m.id in ids }.forEach { it.state = "answering"; it.attempts++ }

  override fun retryLater(ids: List<String>, nextTryAt: Long) = inbox.filter { it.m.id in ids }.forEach { it.state = "new"; it.nextTryAt = nextTryAt }

  override fun addAiUsage(day: String, costUsd: Double, calls: Int, failures: Int) {
    val u = usage(day)
    usage[day] = u.copy(aiCostUsd = u.aiCostUsd + costUsd, aiCalls = u.aiCalls + calls, aiFailures = u.aiFailures + failures)
  }

  override fun commit(day: String, c: Commit) {
    inbox.filter { it.m.id in c.inboxIds }.forEach { it.state = c.inboxState }
    if (c.reply != null && !outbox.containsKey(c.reply.id)) {
      outbox[c.reply.id] = c.reply
      val u = usage(day)
      usage[day] = u.copy(smsParts = u.smsParts + c.reply.parts)
    }
    if (c.person != null) {
      people[c.person.sender] = c.person
      if (c.forget) turns.remove(c.person.sender)
      if (c.turns.isNotEmpty()) turns.getOrPut(c.person.sender) { mutableListOf() } += c.turns.map { clock to it }
    }
    if (c.questions > 0) usage[day] = usage(day).let { it.copy(questions = it.questions + c.questions) }
    c.event?.let { events += "${it.first}: ${it.second}" }
  }

  override fun event(kind: String, text: String) {
    events += "$kind: $text"
  }
}

class BrainTest {
  private val store = FakeStore()
  private var settings = BrainSettings()
  private val asked = mutableListOf<List<ChatMessage>>()
  private var answer: (List<ChatMessage>) -> Outcome = { Outcome("Accra is the capital of Ghana.", "m", listOf(CallRecord("m", true, 0.0001, 100))) }
  private val brain = Brain(store, { settings }, { msgs -> asked += msgs; answer(msgs) }, { store.clock })

  private val alice = "0244123456"
  private val aliceE164 = "+233244123456"
  private var seq = 0

  private fun text(body: String, from: String = alice): String {
    val id = "in${++seq}"
    store.receive(id, from, body, store.clock)
    return id
  }

  private fun advance(ms: Long = 3_000) {
    store.clock += ms
  }

  private fun runAll(): List<StepResult> {
    val out = mutableListOf<StepResult>()
    while (true) {
      val r = brain.step()
      if (r == StepResult.IDLE) return out
      out += r
    }
  }

  private fun sent() = store.outbox.values.map { it.body }

  @Test fun answersAQuestionWithOneSms() {
    store.clock = 1_760_000_000_000
    val id = text("What is the capital of Ghana?")
    assertEquals(listOf(), runAll(), "debounce: too fresh")
    advance()
    assertEquals(listOf(StepResult.ANSWERED), runAll())
    val reply = store.outbox["$id:r"]!!
    assertEquals(aliceE164, reply.to)
    assertEquals("Accra is the capital of Ghana.", reply.body)
    assertEquals(1, reply.parts)
    assertEquals(1, store.people[alice]!!.questionsToday)
    assertTrue(asked.single().last().content == "What is the capital of Ghana?")
  }

  @Test fun coalescesABurstAndKeepsContext() {
    text("What is the")
    text("capital of Ghana?")
    advance()
    runAll()
    assertEquals("What is the capital of Ghana?", asked.single().last().content)
    assertEquals(1, store.outbox.size)
    advance(60_000)
    text("And its population?")
    advance()
    runAll()
    val msgs = asked.last()
    assertEquals(listOf("system", "user", "assistant", "user"), msgs.map { it.role })
    assertEquals("Accra is the capital of Ghana.", msgs[2].content)
  }

  @Test fun ignoresNetworkSendersAndShortCodes() {
    text("Your bundle expires today", from = "MTN")
    text("Win big!", from = "1515")
    advance()
    assertEquals(listOf(StepResult.IGNORED, StepResult.IGNORED), runAll())
    assertTrue(store.outbox.isEmpty())
    assertTrue(asked.isEmpty())
  }

  @Test fun ignoresOwnNumber() {
    settings = settings.copy(ownNumber = "+233 24 412 3456")
    text("loop?")
    advance()
    assertEquals(listOf(StepResult.IGNORED), runAll())
  }

  @Test fun moreSendsTheRestThenNothing() {
    val long = "Photosynthesis is the process plants use to turn sunlight, water and carbon dioxide into glucose and oxygen. " +
      "It happens in the chloroplasts of leaf cells. Chlorophyll absorbs the light energy that powers the reaction for growth."
    answer = { Outcome(long, "m", emptyList()) }
    text("Explain photosynthesis")
    advance()
    runAll()
    assertTrue(sent().last().endsWith(" ..Txt MORE"))
    text("more")
    advance()
    assertEquals(listOf(StepResult.COMMAND), runAll())
    assertTrue(long.endsWith(sent().last()))
    text("MORE")
    advance()
    runAll()
    assertEquals(Texts.NO_MORE, sent().last())
    assertEquals(1, asked.size, "MORE makes no AI call")
  }

  @Test fun stopSilencesUntilStart() {
    text("STOP")
    advance()
    runAll()
    assertEquals(Texts.STOP, sent().last())
    text("hello?")
    advance()
    assertEquals(listOf(StepResult.IGNORED), runAll())
    assertEquals(1, store.outbox.size)
    text("start")
    advance()
    runAll()
    assertEquals(Texts.START, sent().last())
    text("hello?")
    advance()
    assertEquals(listOf(StepResult.ANSWERED), runAll())
  }

  @Test fun newAndDeleteDropContext() {
    text("What is the capital of Ghana?")
    advance()
    runAll()
    advance(1_000)
    text("new")
    advance()
    runAll()
    assertEquals(Texts.NEW_CHAT, sent().last())
    text("And its population?")
    advance()
    runAll()
    assertEquals(listOf("system", "user"), asked.last().map { it.role })
    text("delete my data")
    advance()
    runAll()
    assertEquals(Texts.DELETED, sent().last())
    assertTrue(store.turns[alice].isNullOrEmpty())
  }

  @Test fun dailyLimitNoticeOnce() {
    settings = settings.copy(dailyPerNumberCap = 2)
    repeat(4) {
      text("question $it")
      advance()
      runAll()
    }
    assertEquals(2, asked.size)
    assertEquals(3, store.outbox.size)
    assertEquals(Texts.limitReached(2, 2), sent().last())
  }

  @Test fun retriesThenSaysSorry() {
    answer = { throw AnswerError(AiError(AiCode.UPSTREAM, "UnknownHostException"), emptyList()) }
    val id = text("Are you there?")
    advance()
    assertEquals(listOf(StepResult.RETRY), runAll())
    assertTrue(store.outbox.isEmpty())
    advance(6_000)
    assertEquals(listOf(StepResult.RETRY), runAll())
    advance(31 * 60_000L)
    assertEquals(listOf(StepResult.FAILED), runAll())
    assertEquals(Texts.FAILED, store.outbox["$id:r"]!!.body)
    advance(60 * 60_000L)
    assertEquals(listOf(), runAll())
  }

  @Test fun recoversAfterARetry() {
    var calls = 0
    answer = {
      if (calls++ == 0) throw AnswerError(AiError(AiCode.TIMEOUT, "slow"), emptyList())
      Outcome("Yes.", "m", emptyList())
    }
    text("Are you there?")
    advance()
    runAll()
    advance(10_000)
    assertEquals(listOf(StepResult.ANSWERED), runAll())
    assertEquals(listOf("Yes."), sent())
  }

  @Test fun rejectedKeyFailsFast() {
    answer = { throw AnswerError(AiError(AiCode.UNAUTHORIZED, "HTTP 401"), emptyList()) }
    text("hi")
    advance()
    assertEquals(listOf(StepResult.FAILED), runAll())
    assertEquals(listOf(Texts.FAILED), sent())
  }

  @Test fun smsCapHoldsTexts() {
    settings = settings.copy(dailySmsPartCap = 1)
    text("one")
    advance()
    runAll()
    text("two")
    advance()
    assertEquals(listOf(StepResult.HELD), runAll())
    assertEquals(1, store.outbox.size)
  }

  @Test fun spendCapHoldsQuestions() {
    settings = settings.copy(dailySpendCapUsd = 0.00005)
    text("one")
    advance()
    runAll()
    text("two")
    advance()
    assertEquals(listOf(StepResult.HELD), runAll())
    assertEquals(1, asked.size)
    advance(11 * 60_000L)
    assertEquals(listOf(StepResult.HELD), runAll(), "still held when due again")
  }

  @Test fun commandsAreHandledInOrderWithQuestions() {
    text("What is 2+2?")
    text("more")
    text("What is 3+3?")
    advance()
    runAll()
    assertEquals(2, asked.size)
    assertEquals(3, store.outbox.size)
    assertEquals(Texts.NO_MORE, sent()[1])
  }

  @Test fun sanitizesAndScrubs() {
    answer = { Outcome("Sure! **It’s** done — ok 👍", "m", emptyList()) }
    text("my PIN is 1234, is it safe?")
    advance()
    runAll()
    assertEquals("It's done - ok", sent().single())
    assertEquals("my PIN is [removed], is it safe?", asked.single().last().content)
  }

  @Test fun everyFixedReplyFitsOneGsm7Sms() {
    for (s in listOf(Texts.HELP, Texts.NEW_CHAT, Texts.NO_MORE, Texts.STOP, Texts.START, Texts.DELETED, Texts.FAILED, Texts.limitReached(150, 150))) {
      assertEquals(Encoding.GSM7, measure(s).first, s)
      assertEquals(1, segmentCount(s), s)
    }
  }

  @Test fun expiresDayOldTexts() {
    text("old question")
    advance(25 * 3_600_000L)
    assertEquals(listOf(StepResult.EXPIRED), runAll())
    assertTrue(store.outbox.isEmpty())
  }
}
