// Locates files shipped next to the server: data/ and migrations/ sit in the
// working directory in the Docker image (/app) and at the repository root
// in development (two levels above apps/server).
import { existsSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));

export function resolveAsset(rel: string): string | null {
  const roots = [process.cwd(), path.resolve(process.cwd(), "../.."), path.resolve(here, "../../.."), path.resolve(here, "..")];
  for (const root of roots) {
    const p = path.join(root, rel);
    if (existsSync(p)) return p;
  }
  return null;
}
