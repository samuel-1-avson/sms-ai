import { defineConfig } from "tsup";

// One self-contained file per entry point, so the runtime image needs no
// node_modules. CommonJS dependencies (qrcode, prom-client) call require(),
// which the banner provides inside the ESM bundle.
export default defineConfig({
  entry: { api: "src/bin/api.ts", worker: "src/bin/worker.ts", admin: "src/bin/admin.ts" },
  format: ["esm"],
  platform: "node",
  target: "node22",
  outDir: "dist",
  clean: true,
  splitting: false,
  sourcemap: true,
  noExternal: [/.*/],
  banner: { js: "import { createRequire as __createRequire } from 'node:module'; const require = __createRequire(import.meta.url);" },
});
