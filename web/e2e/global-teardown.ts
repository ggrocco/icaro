import { readFileSync } from "node:fs";

export default async function globalTeardown() {
  const home = process.env.ICARO_E2E_HOME;
  if (!home) return;
  try {
    const pid = Number(readFileSync(`${home}/run/icaro.pid`, "utf8").trim());
    if (pid) process.kill(pid, "SIGTERM");
  } catch {
    /* already gone */
  }
}
