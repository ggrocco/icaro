import { expect, test, type Page } from "@playwright/test";

// Phase 4 acceptance: create, run and cancel a workflow from the UI.

// The first sign-in uses the one-time link from `icaro ui`; it is single
// use, so later ones paste an API token into the login form.
let linkUsed = false;
async function login(page: Page) {
  if (!linkUsed) {
    linkUsed = true;
    await page.goto(process.env.ICARO_E2E_LOGIN!);
  } else {
    await page.goto("/ui/");
    await page.getByLabel("API token").fill(process.env.ICARO_E2E_TOKEN!);
    await page.getByRole("button", { name: "Sign in" }).click();
  }
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();
}

test("a reused login link is refused", async ({ page }) => {
  await login(page);
  await page.context().clearCookies();
  await page.goto(process.env.ICARO_E2E_LOGIN!);
  await expect(page.getByRole("alert")).toContainText(/unauthorized/i);
});

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
    "    image: mirror.gcr.io/library/alpine:3.20",
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
