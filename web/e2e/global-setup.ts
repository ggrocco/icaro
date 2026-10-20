import { execFileSync } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

// Starts an isolated daemon (its own ICARO_HOME), opens the loopback UI
// listener, and stores a one-time login link for the tests.
export default async function globalSetup() {
  const bin = process.env.ICARO_BIN ?? resolve(process.cwd(), "../bin/icaro");
  const home = mkdtempSync(join(tmpdir(), "icaro-e2e-"));
  const port = 18000 + Math.floor(Math.random() * 1000);
  const env = { ...process.env, ICARO_HOME: home, ICARO_PASSWORD: "e2e-password", ICARO_DAEMON__UI_LISTEN: `127.0.0.1:${port}` };
  execFileSync(bin, ["init"], { env, stdio: "inherit" });
  execFileSync(bin, ["serve", "--background"], { env, stdio: "inherit" });
  const token = execFileSync(bin, ["token", "create", "--name", "e2e", "--scope", "write"], { env }).toString().trim();
  process.env.ICARO_E2E_TOKEN = token;
  const out = execFileSync(bin, ["ui", "--print"], { env }).toString();
  const url = out.split("\n")[0].trim();
  const base = new URL(url).origin;
  process.env.ICARO_E2E_HOME = home;
  process.env.ICARO_E2E_URL = base;
  process.env.ICARO_E2E_LOGIN = url;
  writeFileSync(join(home, "e2e.json"), JSON.stringify({ home, base, url, bin }));
}
