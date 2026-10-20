// Plain-JVM tests for the phone's answering logic (the brain/ package has no
// Android dependencies). Run from the repo root with `pnpm test:phone`, which
// uses Docker, so no local JDK is needed.
plugins {
  kotlin("jvm") version "2.0.21"
}

repositories {
  mavenCentral()
}

dependencies {
  // Android ships org.json; on the JVM it comes from Maven.
  implementation("org.json:json:20240303")
  testImplementation(kotlin("test-junit"))
}

kotlin {
  jvmToolchain(17)
}

sourceSets {
  main {
    kotlin.srcDir("../android/src/main/java/expo/modules/smsgateway/brain")
  }
}

tasks.test {
  testLogging {
    events("failed")
    exceptionFormat = org.gradle.api.tasks.testing.logging.TestExceptionFormat.FULL
    showStandardStreams = true
  }
}
