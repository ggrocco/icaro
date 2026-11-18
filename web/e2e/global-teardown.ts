import { execFileSync } from "node:child_process";
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
  // Assist containers outlive the daemon (a restart re-attaches to them);
  // remove the ones of the e2e-only assistant profile.
  try {
    const ids = execFileSync("docker", ["ps", "-aq", "--filter", "label=icaro.profile=assistant-e2e"]).toString().split(/\s+/).filter(Boolean);
    if (ids.length) execFileSync("docker", ["rm", "-f", ...ids]);
  } catch {
    /* no docker */
  }
}
