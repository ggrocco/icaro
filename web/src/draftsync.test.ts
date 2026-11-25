import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { draftSync } from "./draftsync";

describe("draftSync", () => {
  let puts: string[];
  let errors: Error[];
  const put = (yaml: string) => {
    puts.push(yaml);
    return Promise.resolve();
  };
  beforeEach(() => {
    vi.useFakeTimers();
    puts = [];
    errors = [];
  });
  afterEach(() => vi.useRealTimers());

  it("sends an edit after a quiet period, once", () => {
    const s = draftSync(put, "a", (e) => errors.push(e), 800);
    s.edit("a"); // what the draft already holds
    s.edit("b");
    vi.advanceTimersByTime(500);
    s.edit("c");
    vi.advanceTimersByTime(799);
    expect(puts).toEqual([]);
    vi.advanceTimersByTime(1);
    expect(puts).toEqual(["c"]);
    s.edit("c");
    vi.advanceTimersByTime(1000);
    expect(puts).toEqual(["c"]);
  });

  // Regression: a new chat's editor opened on the template and sent it only
  // after the quiet period. A message sent sooner started a turn that read
  // an empty draft, and the late write landed on top of the assistant's
  // proposal as a newer "user" draft, so the proposal never showed (the
  // flaky e2e/assistant.spec.ts on CI).
  it("flush sends the pending edit at once, before a message", async () => {
    const s = draftSync(put, undefined, (e) => errors.push(e), 800);
    s.edit("template");
    await s.flush();
    expect(puts).toEqual(["template"]);
    // The debounced write does not follow it.
    vi.advanceTimersByTime(1000);
    expect(puts).toEqual(["template"]);
    await s.flush();
    expect(puts).toEqual(["template"]);
  });

  it("flush reports a failed write to the caller", async () => {
    const s = draftSync(() => Promise.reject(new Error("down")), "a", (e) => errors.push(e), 800);
    s.edit("b");
    await expect(s.flush()).rejects.toThrow("down");
    expect(errors).toEqual([]);
  });

  it("reports a failed debounced write", async () => {
    const s = draftSync(() => Promise.reject(new Error("down")), "a", (e) => errors.push(e), 800);
    s.edit("b");
    await vi.advanceTimersByTimeAsync(800);
    expect(errors.map((e) => e.message)).toEqual(["down"]);
  });

  it("drops a pending edit the draft already holds, and on cancel", async () => {
    const s = draftSync(put, "a", (e) => errors.push(e), 800);
    s.edit("b");
    s.synced("b"); // an accepted proposal: the chat's draft holds it
    vi.advanceTimersByTime(1000);
    await s.flush();
    s.edit("c");
    s.cancel();
    vi.advanceTimersByTime(1000);
    expect(puts).toEqual([]);
  });
});
