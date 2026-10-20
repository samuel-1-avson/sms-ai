package expo.modules.smsgateway.brain

import java.util.Calendar
import java.util.TimeZone

/** Assembles the model input: the hardened system prompt, a bounded window of recent turns and the question. */

/**
 * Tuned in the 2026-10-17 benchmark: a reply normally fits one 160-character
 * SMS. Joined explicitly so a CRLF checkout cannot change the prompt.
 */
val SYSTEM_PROMPT: String = listOf(
  "You are SMS AI Ghana, a concise, accurate text assistant for users in Ghana.",
  "Every reply is sent as ONE text message of at most 150 characters (about 25 words). Count carefully.",
  "Give the direct answer in one or two short sentences. Leave out greetings, background and examples unless asked.",
  "Only when the user asks for steps or a worked solution may you go longer, up to 300 characters, still with the key result first.",
  "Write plain ASCII only: straight quotes ' and \", hyphen -, x for times, / for divide, ^ for powers, GHS for cedis. No other symbols.",
  "Never use markdown, bold, lists, line breaks, or emojis.",
  "Use conversation history only to resolve follow-up pronouns or context.",
  "Do not invent live prices, flight schedules, news, exchange rates, or bookings; state clearly when live info is unavailable.",
  "Do not name specific companies, agencies, websites or phone numbers unless they are official and you are sure they exist; give general guidance instead.",
  "For health, legal, or financial topics, give general educational facts and advise consulting a qualified professional.",
  "Decline harmful or illegal requests briefly. Never ask for PINs, MoMo OTPs, passwords, or Ghana Card numbers.",
).joinToString("\n")

data class ChatMessage(val role: String, val content: String)

/** One earlier message in the conversation: role is "user" or "assistant". */
data class Turn(val role: String, val content: String)

/** A conservative chars/4 approximation; exact counts come back from the provider. */
fun estimateTokens(s: String): Int = (s.toByteArray(Charsets.UTF_8).size + 3) / 4 + 4

private val MONTHS = listOf("January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December")

/**
 * Builds the chat messages, dropping the oldest history turns until the
 * estimate fits maxInputTokens. [history] is oldest first.
 */
fun buildPrompt(question: String, history: List<Turn>, maxInputTokens: Int, nowMs: Long? = null): List<ChatMessage> {
  var sys = SYSTEM_PROMPT
  if (nowMs != null) {
    val c = Calendar.getInstance(TimeZone.getTimeZone("UTC")).apply { timeInMillis = nowMs }
    sys += "\nToday's date is ${c.get(Calendar.DAY_OF_MONTH)} ${MONTHS[c.get(Calendar.MONTH)]} ${c.get(Calendar.YEAR)}."
  }
  val system = ChatMessage("system", sys)
  val q = ChatMessage("user", question)

  val budget = if (maxInputTokens > 0) maxInputTokens else 800
  var start = 0
  var total = estimateTokens(system.content) + estimateTokens(q.content) + history.sumOf { estimateTokens(it.content) }
  while (start < history.size && total > budget) {
    total -= estimateTokens(history[start].content)
    start++
  }
  // Never start the window on an assistant turn without its question.
  while (start < history.size && history[start].role != "user") start++

  return listOf(system) + history.subList(start, history.size).map { ChatMessage(it.role, it.content) } + q
}
