import { execFileSync } from "node:child_process";
import { chmodSync, copyFileSync, mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

// Starts an isolated daemon (its own ICARO_HOME), opens the loopback UI
// listener, and stores a one-time login link for the tests.
export default async function globalSetup() {
  const bin = process.env.ICARO_BIN ?? resolve(process.cwd(), "../bin/icaro");
  const home = mkdtempSync(join(tmpdir(), "icaro-e2e-"));
  const port = 18000 + Math.floor(Math.random() * 1000);
  const env = { ...process.env, ICARO_HOME: home, ICARO_PASSWORD: "e2e-password", ICARO_DAEMON__UI_LISTEN: `127.0.0.1:${port}` };
  // The workflow assistant runs a fake claude CLI (hack/e2e/testdata/
  // fake-agent) that calls the real assistant tools over its MCP socket.
  const ctx = mkdtempSync(join(tmpdir(), "icaro-e2e-agent-"));
  const fake = resolve(process.cwd(), "../hack/e2e/testdata/fake-agent");
  copyFileSync(join(fake, "Dockerfile"), join(ctx, "Dockerfile"));
  copyFileSync(join(fake, "claude"), join(ctx, "claude"));
  copyFileSync(bin, join(ctx, "icaro"));
  chmodSync(join(ctx, "claude"), 0o755);
  execFileSync("docker", ["build", "-q", "-t", "icaro-e2e-fake-agent:dev", ctx], { stdio: "inherit" });
  mkdirSync(join(home, "config"), { recursive: true });
  writeFileSync(
    join(home, "config", "icaro.yaml"),
    ["version: 1", "assistant:", "  profile: assistant-e2e", "profiles:", "  assistant-e2e:", "    agent: claude", "    image: icaro-e2e-fake-agent:dev", "    egress: []", ""].join("\n"),
  );
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
