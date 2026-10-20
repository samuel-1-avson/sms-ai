// Runs the Kotlin tests for the phone's answering logic in Docker, so no
// local JDK or Android SDK is needed. Usage: pnpm test:phone
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

const moduleDir = resolve(fileURLToPath(import.meta.url), "../../apps/gateway/modules/sms-gateway");
const args = [
  "run", "--rm",
  "-v", `${moduleDir}:/w`,
  // Gradle's downloads are cached between runs.
  "-v", "smsai-gradle:/home/gradle/.gradle",
  "-w", "/w/jvm",
  "gradle:8.10-jdk17",
  "gradle", "test", "--no-daemon", "--console=plain", "--project-cache-dir", "/tmp/pc",
];
const r = spawnSync("docker", args, { stdio: "inherit", env: { ...process.env, MSYS_NO_PATHCONV: "1" } });
if (r.error) {
  console.error(`Could not run docker (${r.error.message}). Is Docker Desktop running?`);
  process.exit(1);
}
process.exit(r.status ?? 1);
