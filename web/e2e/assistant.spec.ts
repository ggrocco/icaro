import { expect, test, type Page } from "@playwright/test";

// ADR 0035 acceptance: a chat beside the editor proposes a workflow as a
// diff; the person accepts it and saves it, and the version is theirs
// "via assistant <chat>". The agent is a fake claude CLI that calls the
// real assistant tools (see e2e/global-setup.ts).

async function login(page: Page) {
  await page.goto("/ui/");
  await page.getByLabel("API token").fill(process.env.ICARO_E2E_TOKEN!);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();
}

test("ask the assistant for a workflow, accept its proposal and save it", async ({ page }) => {
  await login(page);
  await page.getByRole("button", { name: "New with assistant" }).click();
  const panel = page.getByRole("complementary", { name: "Workflow assistant" });
  await expect(panel).toBeVisible();

  await panel.getByLabel("Message the assistant").fill("Make a workflow that says hello");
  await panel.getByRole("button", { name: "Send" }).click();
  await expect(panel.getByText("I proposed a hello workflow (valid)")).toBeVisible({ timeout: 60_000 });
  // The tools it used are listed on its reply.
  await expect(panel.getByRole("list", { name: "Tools the assistant used" }).getByText("propose_workflow")).toBeVisible();

  const proposal = panel.getByRole("group", { name: "Proposed change" });
  await expect(proposal).toBeVisible();
  await expect(proposal.getByLabel("Diff from the editor to the proposal")).toContainText("+ name: hello");
  await proposal.getByRole("button", { name: "Accept into the editor" }).click();
  await expect(proposal).toBeHidden();
  await expect(page.getByLabel("Name", { exact: true }).first()).toHaveValue("hello");

  await page.getByRole("button", { name: "Validate & save" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  const chat = decodeURIComponent(new URL(page.url()).hash.split("/").pop()!);
  const versions = await page.evaluate(async () => (await fetch("/api/v1/workflows/hello/versions")).json());
  expect(versions.versions[0].actor).toBe(`token:e2e via assistant ${chat}`);

  // ADR 0036: the chat leaves exact memories (no model is pulled here) and
  // the person can see and remove them.
  const memory = panel.locator("details.memory-section");
  await memory.locator("summary").click();
  await memory.getByRole("button", { name: "Remember now" }).click();
  const list = memory.getByRole("list", { name: "Memories from this chat" });
  await expect(list.getByText(/Saved hello v1/)).toBeVisible({ timeout: 30_000 });
  const before = await list.locator("li").count();
  await list.getByRole("button", { name: /^Remove memory Saved hello v1/ }).click();
  await expect(list.locator("li")).toHaveCount(before - 1);
});
