import { describe, expect, it } from "vitest";
import { applyEntry, blocker, busy, emptyView, shownText, type ChatDetail, type LogEntry, type MessageInfo } from "./assist";

const msg = (id: string, seq: number, role: "user" | "assistant", status: MessageInfo["status"], text = ""): MessageInfo => ({
  id,
  seq,
  role,
  status,
  text,
  log_offset: 0,
  created_at: "2026-11-10T10:00:00Z",
});

const replay = (entries: LogEntry[], d?: ChatDetail) => entries.reduce(applyEntry, emptyView(d));
const at = "2026-11-10T10:00:00Z";

describe("applyEntry", () => {
  const log: LogEntry[] = [
    { time: at, type: "message", message_id: "u1", message: msg("u1", 1, "user", "done", "post to slack") },
    { time: at, type: "message", message_id: "m1", message: msg("m1", 2, "assistant", "queued") },
    { time: at, type: "status", message_id: "m1", status: "queued", note: "Waiting for a free assistant slot." },
    { time: at, type: "status", message_id: "m1", status: "running" },
    { time: at, type: "event", message_id: "m1", event: { kind: "session" } },
    { time: at, type: "event", message_id: "m1", event: { kind: "text", text: "Checking " } },
    { time: at, type: "event", message_id: "m1", event: { kind: "tool_call", tool_id: "t1", tool: "validate_workflow", args: { yaml: "x" } } },
    { time: at, type: "event", message_id: "m1", event: { kind: "tool_result", tool_id: "t1", result: '{"valid":true}' } },
    { time: at, type: "event", message_id: "m1", event: { kind: "tool_result", tool_id: "other" } },
    { time: at, type: "draft", draft: { version: 2, source: "assistant", issues: 0 } },
    { time: at, type: "approval", approval: { id: "a1", tool: "run_workflow", request: { workflow: "hello" }, created_at: at, expires_at: at } },
    { time: at, type: "event", message_id: "m1", event: { kind: "text", text: "the schema." } },
  ];

  it("builds a running turn from the log", () => {
    const v = replay(log);
    expect(v.messages.map((m) => m.id)).toEqual(["u1", "m1"]);
    expect(v.messages[1].status).toBe("running");
    expect(v.notes.m1).toBeUndefined(); // the queued note goes once it runs
    expect(shownText(v, v.messages[1])).toBe("Checking the schema.");
    expect(v.tools.m1).toEqual([{ id: "t1", tool: "validate_workflow", server: undefined, args: { yaml: "x" }, result: '{"valid":true}', isError: undefined, done: true }]);
    expect(v.draftVersion).toBe(2);
    expect(v.draftSource).toBe("assistant");
    expect(v.approvals.map((a) => a.id)).toEqual(["a1"]);
    expect(busy(v)).toBe(true);
    expect(blocker(v)).toBeUndefined();
  });

  it("settles a turn and keeps it settled", () => {
    const v = replay([
      ...log,
      { time: at, type: "approval", approval: { id: "a1", tool: "run_workflow", request: {}, decision: "approved", created_at: at, expires_at: at } },
      { time: at, type: "status", message_id: "m1", status: "done", message: { ...msg("m1", 2, "assistant", "done", "Checking the schema."), usage: { input: 10, output: 2 } } },
      // A late, stale status never moves a settled message back.
      { time: at, type: "status", message_id: "m1", status: "running" },
      { time: at, type: "message", message_id: "m1", message: msg("m1", 2, "assistant", "queued") },
    ]);
    expect(v.messages[1].status).toBe("done");
    expect(v.messages[1].usage?.input).toBe(10);
    expect(shownText(v, v.messages[1])).toBe("Checking the schema.");
    expect(v.approvals).toEqual([]);
    expect(busy(v)).toBe(false);
    expect(blocker(v)).toBeUndefined();
  });

  it("surfaces a reply that did not finish", () => {
    const v = replay([
      { time: at, type: "message", message_id: "m1", message: msg("m1", 1, "assistant", "running") },
      { time: at, type: "status", message_id: "m1", status: "auth_required", note: "Please run /login" },
    ]);
    expect(blocker(v)?.status).toBe("auth_required");
    expect(blocker(v)?.error).toBe("Please run /login");
  });

  it("ignores what it does not understand and keeps chat notes short", () => {
    let v = emptyView();
    for (let i = 0; i < 8; i++) v = applyEntry(v, { time: at, type: "status", note: `note ${i}` });
    expect(v.chatNotes).toEqual(["note 3", "note 4", "note 5", "note 6", "note 7"]);
    const same = applyEntry(v, { time: at, type: "event" });
    expect(same).toBe(v);
    expect(applyEntry(v, { time: at, type: "message" })).toBe(v);
    expect(applyEntry(v, { time: at, type: "draft" })).toBe(v);
    expect(applyEntry(v, { time: at, type: "approval" })).toBe(v);
    expect(applyEntry(v, { time: at, type: "status" })).toBe(v);
    expect(applyEntry(v, { time: at, type: "event", message_id: "m", event: { kind: "usage" } })).toBe(v);
    expect(applyEntry(v, { time: at, type: "status", message_id: "ghost", status: "done" }).messages).toEqual([]);
  });

  it("records the notes a conversation started with", () => {
    const refs = [{ id: "x", kind: "lesson", title: "steps need ids" }];
    const v = replay([
      { time: at, type: "memory", message_id: "m1", memories: refs },
      { time: at, type: "memory", message_id: "m2", memories: [] },
      { time: at, type: "memory", memories: refs },
    ]);
    expect(v.recalled).toEqual({ m1: refs });
  });

  it("starts from the chat's detail", () => {
    const d: ChatDetail = {
      id: "c1",
      title: "t",
      profile: "claude",
      agent: "claude",
      created_at: at,
      updated_at: at,
      messages: [msg("u1", 1, "user", "done", "hi")],
      draft: { yaml: "name: x", issues: [], source: "user", version: 4, updated_at: at },
      approvals: [],
      log_size: 0,
    };
    const v = emptyView(d);
    expect(v.draftVersion).toBe(4);
    expect(v.messages).toHaveLength(1);
    // A draft entry older than the detail does not lower the version.
    expect(applyEntry(v, { time: at, type: "draft", draft: { version: 2, source: "user", issues: 0 } }).draftVersion).toBe(4);
  });
});
