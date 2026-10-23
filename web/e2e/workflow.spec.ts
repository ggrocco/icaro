import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test, type Page } from "@playwright/test";

// Phase 4 acceptance: create, run and cancel a workflow from the UI.

// Tests sign in by pasting an API token; the one-time `icaro ui` link is
// covered by its own test (it is single use).
async function login(page: Page) {
  await page.goto("/ui/");
  await page.getByLabel("API token").fill(process.env.ICARO_E2E_TOKEN!);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();
}

// apiCall uses the browser's cookie session, as the UI does.
async function apiCall(page: Page, method: string, path: string, body?: string) {
  return page.evaluate(
    async ([m, p, b]) => {
      const res = await fetch(p!, { method: m!, headers: { "X-Icaro-CSRF": "1", "Content-Type": "application/yaml" }, body: b ?? undefined });
      return { status: res.status, body: await res.text() };
    },
    [method, path, body ?? null],
  );
}

test("the one-time sign-in link works once", async ({ page }) => {
  await page.goto(process.env.ICARO_E2E_LOGIN!);
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();
  await page.context().clearCookies();
  await page.goto(process.env.ICARO_E2E_LOGIN!);
  await expect(page.getByRole("alert")).toContainText(/unauthorized/i);
});

test("create a workflow in the form editor, run it, and watch it succeed", async ({ page }) => {
  await login(page);
  await page.getByRole("link", { name: "New workflow" }).click();
  const name = page.getByLabel("Name", { exact: true }).first();
  await name.fill("e2e-hello");
  await expect(page.getByText("valid", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Validate & save" }).click();
  await expect(page.getByRole("heading", { name: /e2e-hello/ })).toBeVisible();

  await page.getByRole("button", { name: "Run", exact: true }).click();
  await expect(page.getByRole("heading", { name: /Run/ })).toBeVisible();
  // Status arrives over SSE, not polling.
  await expect(page.locator("h1 .badge")).toHaveText("succeeded", { timeout: 90_000 });
  await page.getByRole("button", { name: /hello/ }).first().click();
  await expect(page.getByLabel("Logs for hello")).toContainText("hello", { timeout: 15_000 });
});

test("an invalid edit shows inline issues and cannot be saved", async ({ page }) => {
  await login(page);
  await page.getByRole("link", { name: "New workflow" }).click();
  await page.getByLabel("Name", { exact: true }).first().fill("Bad Name");
  await expect(page.getByText(/issue\(s\)/)).toBeVisible();
  await expect(page.getByRole("button", { name: "Validate & save" })).toBeDisabled();
  await page.getByRole("tab", { name: "YAML" }).click();
  await expect(page.getByLabel("Validation issues")).toContainText("/name");
});

test("cancel a running workflow", async ({ page }) => {
  await login(page);
  const yaml = [
    "version: 1",
    "name: e2e-slow",
    "steps:",
    "  - id: wait",
    "    image: mirror.gcr.io/library/alpine:3.24",
    "    command: sleep 300",
    "",
  ].join("\n");
  const created = await apiCall(page, "POST", "/api/v1/workflows", yaml);
  expect([200, 201]).toContain(created.status);
  await page.goto("/ui/#/workflows/e2e-slow");
  await page.getByRole("button", { name: "Run", exact: true }).click();
  await expect(page.locator("h1 .badge")).toHaveText("running", { timeout: 60_000 });
  await page.getByRole("button", { name: "Cancel" }).click();
  await expect(page.locator("h1 .badge")).toHaveText("cancelled", { timeout: 60_000 });
});

test("a manifest edit commits to the local pack; an invalid edit is rejected", async ({ page }) => {
  await login(page);
  await page.goto("/ui/#/integrations/webhook");
  await expect(page.getByRole("heading", { name: /webhook/ })).toBeVisible();
  const editor = page.locator(".cm-content");
  await expect(editor).toContainText("name: webhook");
  // Saving the builtin copies it into the local pack as a commit.
  await page.getByRole("button", { name: "Validate & commit" }).click();
  await expect(page.getByRole("status").filter({ hasText: /Committed [0-9a-f]{8} to the local pack/ })).toBeVisible();
  // Break the manifest: the daemon rejects it before writing anything.
  await editor.click();
  await page.keyboard.press("ControlOrMeta+a");
  await page.keyboard.type("version: 1\nname: webhook\nbogus_field: true\n");
  await page.getByRole("button", { name: "Validate & commit" }).click();
  await expect(page.getByLabel("Validation issues").first()).toContainText("bogus_field");
});

test("the usage page shows transcript usage with its source and an estimated cost", async ({ page }) => {
  // A Claude Code transcript in a profile home, as an agent session writes it.
  const dir = join(process.env.ICARO_E2E_HOME!, "home", "e2e", ".claude", "projects", "-workspace");
  mkdirSync(dir, { recursive: true });
  const line = (id: string, model: string) =>
    JSON.stringify({
      type: "assistant",
      timestamp: new Date().toISOString(),
      requestId: `req_${id}`,
      message: { id: `msg_${id}`, model, usage: { input_tokens: 1200, output_tokens: 300 } },
    });
  writeFileSync(join(dir, "session.jsonl"), [line("1", "claude-sonnet-4-20250514"), line("2", "claude-future-9")].join("\n") + "\n");

  await login(page);
  await page.getByRole("link", { name: "Usage" }).click();
  await expect(page.getByRole("heading", { name: "Usage", level: 1 })).toBeVisible();
  const detail = page.getByRole("table", { name: "Usage by source and model" });
  const priced = detail.getByRole("row").filter({ hasText: "claude-sonnet-4-20250514" });
  await expect(priced).toContainText("profile:e2e");
  await expect(priced).toContainText("1,200");
  await expect(priced).toContainText("~$");
  await expect(priced).toContainText("high");
  // An unpriced model never gets a guessed cost.
  await expect(detail.getByRole("row").filter({ hasText: "claude-future-9" })).toContainText("n/a");
  await expect(page.getByRole("table", { name: "Usage by day" }).getByRole("row").nth(1)).toContainText("≥ ~$");
});
