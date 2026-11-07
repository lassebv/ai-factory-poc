// Builds ./cmd/torrentd and runs it exactly as a user would: no arguments,
// default address 127.0.0.1:8080. HOME points at a throwaway directory so the
// default data dir is isolated. Used as Playwright's webServer.
import { execFileSync, spawn } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const root = resolve(import.meta.dirname, "..");
const work = mkdtempSync(join(tmpdir(), "torrentd-e2e-"));
const bin = join(work, "torrentd");
execFileSync("go", ["build", "-o", bin, "./cmd/torrentd"], {
  cwd: root, stdio: "inherit", env: { ...process.env, CGO_ENABLED: "0" },
});
const env = { ...process.env, HOME: join(work, "home"), XDG_DATA_HOME: "" };
for (const k of Object.keys(env)) if (k.startsWith("TORRENTD_")) delete env[k];
const child = spawn(bin, [], { cwd: work, env, stdio: "inherit" });
const stop = () => child.kill("SIGTERM");
process.on("SIGTERM", stop);
process.on("SIGINT", stop);
child.on("exit", (code) => process.exit(code ?? 0));
