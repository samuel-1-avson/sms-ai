package expo.modules.smsgateway

import org.junit.Assert.assertEquals
import org.junit.Test

class GatewayClientTest {
  /** The same vector is asserted in packages/shared/src/signing.test.ts, so both sides sign identically. */
  @Test
  fun signMatchesTheServer() {
    val body = """{"messages":[{"body":"Ɛte sɛn?"}]}"""
    assertEquals(
      "sha256=59eb535ca2774660bb686e8c1016d4db4d11c3a40c84a922e32a7ea21294c5f3",
      GatewayClient.sign("test-secret-at-least-24-characters", 1700000000L, body),
    )
  }
}
