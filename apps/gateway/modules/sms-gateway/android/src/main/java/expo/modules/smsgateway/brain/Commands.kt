package expo.modules.smsgateway.brain

/** Keyword commands handled on the phone before any AI call. */
enum class Command { HELP, NEW, MORE, STOP, START, DELETE }

private val keywords = mapOf(
  "HELP" to Command.HELP, "?" to Command.HELP, "INFO" to Command.HELP, "MENU" to Command.HELP,
  "NEW" to Command.NEW, "RESET" to Command.NEW, "NEW CHAT" to Command.NEW,
  "MORE" to Command.MORE, "M" to Command.MORE,
  "STOP" to Command.STOP, "STOPALL" to Command.STOP, "UNSUBSCRIBE" to Command.STOP, "CANCEL" to Command.STOP,
  "OPTOUT" to Command.STOP, "OPT OUT" to Command.STOP,
  "START" to Command.START, "UNSTOP" to Command.START, "SUBSCRIBE" to Command.START,
  "DELETE" to Command.DELETE, "DELETE MY DATA" to Command.DELETE, "FORGET ME" to Command.DELETE,
)

private val reWords = Regex("$SP+")
private val reTrailing = Regex("""(?:$SP|[.!?])+$""")

/** Classifies a whole message. A command must be the entire text, so "more examples of verbs" is a question. */
fun parseCommand(body: String): Command? {
  var s = jsTrim(body).uppercase().split(reWords).filter { it.isNotEmpty() }.joinToString(" ")
  if (s != "?") s = reTrailing.replace(s, "")
  return keywords[s]
}

/** Replies the phone sends itself. Every one fits a single GSM-7 SMS (checked by tests). */
object Texts {
  const val HELP =
    "SMS AI Ghana: text any question. MORE=rest of an answer, NEW=new chat, STOP=opt out. " +
      "AI can be wrong. Chats kept 30 days. SMS rates may apply."
  const val NEW_CHAT = "New chat started. Ask me anything."
  const val NO_MORE = "Nothing more for your last answer. Send a new question anytime."
  const val STOP = "You will get no more messages from SMS AI Ghana. Text START to rejoin."
  const val START = "Welcome to SMS AI Ghana! Text any question. Text HELP for commands."
  const val DELETED = "Your chat history has been deleted. Text any question to start again."
  const val FAILED = "Sorry, I could not answer that right now. Please try again shortly."

  /** The once-a-day notice when a number reaches its daily question limit. */
  fun limitReached(used: Int, limit: Int) = "Daily limit reached ($used/$limit). Please text again tomorrow."
}
