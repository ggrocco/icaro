// The workflow assistant (ADR 0035): a chat that runs the profile's own
// agent CLI under its login and authors workflows through Ícaro's tools.
// Its log is the source of truth; the UI rebuilds a chat's view from it.

import { CSRF_HEADER, ApiError, type Issue, type WorkflowInfo } from "./api";

export interface ChatInfo {
  id: string;
  title: string;
  profile: string;
  agent: string;
  project_id?: string;
  workflow?: string;
  session_id?: string;
  session_status?: string;
  created_by?: string;
  created_at: string;
  updated_at: string;
}

export type TurnStatus = "queued" | "running" | "done" | "error" | "cancelled" | "interrupted" | "rate_limited" | "auth_required";

export interface MessageInfo {
  id: string;
  seq: number;
  role: "user" | "assistant";
  text: string;
  status: TurnStatus;
  error?: string;
  usage?: { input: number; output: number; cache_read?: number; cache_write?: number };
  cost_usd?: number;
  reset_at?: string;
  log_offset: number;
  created_at: string;
  finished_at?: string;
}

export interface DraftInfo {
  yaml: string;
  issues: Issue[];
  source: "user" | "assistant";
  version: number;
  updated_at: string;
}

export interface ChatApproval {
  id: string;
  tool: string;
  request: { workflow?: string; version?: number; trigger?: Record<string, unknown> };
  decision?: "approved" | "declined" | "expired";
  actor?: string;
  result?: string;
  created_at: string;
  expires_at: string;
  decided_at?: string;
}

export interface ChatDetail extends ChatInfo {
  messages: MessageInfo[];
  draft?: DraftInfo;
  approvals: ChatApproval[];
  log_size: number;
}

// StreamEvent is one normalized event of a turn (internal/assist/stream).
export interface StreamEvent {
  kind: "session" | "text" | "tool_call" | "tool_result" | "usage" | "rate_limited" | "auth_required" | "error" | "done";
  text?: string;
  tool_id?: string;
  server?: string;
  tool?: string;
  args?: unknown;
  result?: string;
  is_error?: boolean;
}

// LogEntry is one line of a chat's log.
export interface LogEntry {
  time: string;
  type: "message" | "status" | "event" | "draft" | "approval" | "memory";
  message_id?: string;
  status?: TurnStatus;
  note?: string;
  message?: MessageInfo;
  event?: StreamEvent;
  draft?: { version: number; source: "user" | "assistant"; issues: number };
  approval?: ChatApproval;
  memories?: MemoryRef[];
}

// MemoryRef names a note from earlier chats a conversation started with.
export interface MemoryRef {
  id: string;
  kind: string;
  title: string;
}

// Memory is one memory a chat left (ADR 0036). confidence is "exact" for
// what Ícaro recorded itself and "model" for the small model's summary.
export interface Memory {
  id: string;
  kind: string;
  title: string;
  body?: string;
  source?: string;
  metadata?: { confidence?: string; [k: string]: unknown };
  expires_at?: string;
  created_at: string;
}

// WorkflowNote is the latest intent or resume note chats left about a
// workflow.
export interface WorkflowNote {
  type: "intent" | "resume";
  chat_id: string;
  memory: Memory;
}

export interface ChatMemories {
  used: MemoryRef[];
  produced: Memory[];
  job?: { status: "pending" | "running" | "done" | "failed"; stage2?: string; memories: number; error?: string };
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET") headers[CSRF_HEADER] = "1";
  const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: "same-origin" });
  if (!res.ok) {
    let msg = res.statusText;
    let issues: Issue[] = [];
    try {
      const j = await res.json();
      msg = j.error || msg;
      issues = j.issues || [];
    } catch {
      /* not JSON */
    }
    throw new ApiError(res.status, msg, issues);
  }
  // 204, and the 202 of a queued distill, carry no body.
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

const chat = (id: string) => `/api/v1/chats/${encodeURIComponent(id)}`;

export const assistApi = {
  list: () => call<{ chats: ChatInfo[] }>("GET", "/api/v1/chats").then((r) => r.chats ?? []),
  create: (req: { workflow?: string; yaml?: string; title?: string }) => call<ChatDetail>("POST", "/api/v1/chats", req),
  get: (id: string) => call<ChatDetail>("GET", chat(id)),
  send: (id: string, text: string) => call<{ messages: MessageInfo[] }>("POST", `${chat(id)}/messages`, { text }).then((r) => r.messages),
  retry: (id: string, msg: string) => call<{ messages: MessageInfo[] }>("POST", `${chat(id)}/messages/${encodeURIComponent(msg)}/retry`).then((r) => r.messages),
  cancel: (id: string) => call<void>("POST", `${chat(id)}/cancel`),
  putDraft: (id: string, yaml: string) => call<DraftInfo>("PUT", `${chat(id)}/draft`, { yaml }),
  apply: (id: string, yaml: string) => call<WorkflowInfo>("POST", `${chat(id)}/apply`, { yaml }),
  decide: (id: string, approval: string, approve: boolean) =>
    call<ChatApproval>("POST", `${chat(id)}/approvals/${encodeURIComponent(approval)}/${approve ? "approve" : "decline"}`),
  remove: (id: string) => call<void>("DELETE", chat(id)),
  memories: (id: string) => call<ChatMemories>("GET", `${chat(id)}/memories`),
  forget: (id: string, memory: string) => call<void>("DELETE", `${chat(id)}/memories/${encodeURIComponent(memory)}`),
  distill: (id: string) => call<void>("POST", `${chat(id)}/distill`),
  workflowNotes: (workflow: string) =>
    call<{ notes: WorkflowNote[] }>("GET", `/api/v1/assistant/notes?workflow=${encodeURIComponent(workflow)}`).then((r) => r.notes ?? []),
};

// subscribeChat follows a chat's log over SSE from the start, so the view
// is rebuilt from the log. The browser reconnects by itself with the last
// event id; a resync closes the stream and onResync reopens it.
export function subscribeChat(id: string, onEntry: (e: LogEntry) => void, onState?: (live: boolean) => void): () => void {
  let es: EventSource | undefined;
  let closed = false;
  let offset = 0;
  const open = () => {
    es = new EventSource(`${chat(id)}/events?offset=${offset}`, { withCredentials: true });
    es.onopen = () => onState?.(true);
    es.onerror = () => onState?.(false);
    es.addEventListener("entry", (m) => {
      const ev = m as MessageEvent;
      offset = Number(ev.lastEventId) || offset;
      try {
        onEntry(JSON.parse(ev.data) as LogEntry);
      } catch {
        /* a line the UI does not understand */
      }
    });
    es.addEventListener("resync", () => {
      es?.close();
      if (!closed) window.setTimeout(open, 250);
    });
  };
  open();
  return () => {
    closed = true;
    es?.close();
  };
}

export interface ToolCall {
  id: string;
  tool: string;
  server?: string;
  args?: unknown;
  result?: string;
  isError?: boolean;
  done: boolean;
}

// ChatView is what the panel renders, built from the chat's detail and
// then from its log entries.
export interface ChatView {
  messages: MessageInfo[];
  // live is the text streamed so far for a turn still running.
  live: Record<string, string>;
  tools: Record<string, ToolCall[]>;
  notes: Record<string, string>;
  approvals: ChatApproval[];
  draftVersion: number;
  draftSource?: "user" | "assistant";
  // chatNotes are notes about the chat itself (the container, a save).
  chatNotes: string[];
  // recalled lists, per reply, the notes from earlier chats its
  // conversation started with.
  recalled: Record<string, MemoryRef[]>;
}

export function emptyView(d?: ChatDetail): ChatView {
  return {
    messages: d?.messages ?? [],
    live: {},
    tools: {},
    notes: {},
    approvals: d?.approvals ?? [],
    draftVersion: d?.draft?.version ?? 0,
    draftSource: d?.draft?.source,
    chatNotes: [],
    recalled: {},
  };
}

const settled = (s: TurnStatus) => s !== "queued" && s !== "running";

function upsert(list: MessageInfo[], m: MessageInfo): MessageInfo[] {
  const i = list.findIndex((x) => x.id === m.id);
  if (i < 0) return [...list, m].sort((a, b) => a.seq - b.seq);
  const next = list.slice();
  // A settled message never goes back to running.
  if (settled(next[i].status) && !settled(m.status)) return list;
  next[i] = { ...next[i], ...m };
  return next;
}

// applyEntry folds one log entry into the view. It is pure, so replaying
// a log from the start always builds the same view.
export function applyEntry(v: ChatView, e: LogEntry): ChatView {
  const id = e.message_id ?? "";
  switch (e.type) {
    case "message":
      return e.message ? { ...v, messages: upsert(v.messages, e.message) } : v;
    case "status": {
      if (!id) return e.note ? { ...v, chatNotes: [...v.chatNotes.slice(-4), e.note] } : v;
      let messages = v.messages;
      const cur = messages.find((m) => m.id === id);
      if (e.message) messages = upsert(messages, e.message);
      else if (cur && e.status) messages = upsert(messages, { ...cur, status: e.status, error: settled(e.status) ? e.note || cur.error : cur.error });
      const notes = { ...v.notes };
      if (e.status === "queued" && e.note) notes[id] = e.note;
      else delete notes[id];
      return { ...v, messages, notes };
    }
    case "event": {
      const ev = e.event;
      if (!ev || !id) return v;
      if (ev.kind === "text") return { ...v, live: { ...v.live, [id]: (v.live[id] ?? "") + (ev.text ?? "") } };
      if (ev.kind === "tool_call") {
        const call: ToolCall = { id: ev.tool_id ?? "", tool: ev.tool ?? "", server: ev.server, args: ev.args, done: false };
        return { ...v, tools: { ...v.tools, [id]: [...(v.tools[id] ?? []), call] } };
      }
      if (ev.kind === "tool_result") {
        const calls = (v.tools[id] ?? []).map((c) => (c.id === ev.tool_id ? { ...c, result: ev.result, isError: ev.is_error, done: true } : c));
        return { ...v, tools: { ...v.tools, [id]: calls } };
      }
      return v;
    }
    case "draft":
      return e.draft ? { ...v, draftVersion: Math.max(v.draftVersion, e.draft.version), draftSource: e.draft.source } : v;
    case "memory":
      return id && e.memories?.length ? { ...v, recalled: { ...v.recalled, [id]: e.memories } } : v;
    case "approval": {
      const a = e.approval;
      if (!a) return v;
      const rest = v.approvals.filter((x) => x.id !== a.id);
      return { ...v, approvals: a.decision ? rest : [...rest, a] };
    }
  }
  return v;
}

// shownText is what a message shows: its stored text once settled, the
// streamed text while it runs.
export function shownText(v: ChatView, m: MessageInfo): string {
  if (m.role === "assistant" && !settled(m.status)) return v.live[m.id] ?? m.text;
  return m.text || v.live[m.id] || "";
}

// busy reports whether a turn is queued or running.
export function busy(v: ChatView): boolean {
  return v.messages.some((m) => m.role === "assistant" && !settled(m.status));
}

// blocker is the state the chat must show above the composer: the last
// reply's sign-in, limit, interruption or failure.
export function blocker(v: ChatView): MessageInfo | undefined {
  const last = [...v.messages].reverse().find((m) => m.role === "assistant");
  if (!last || last.status === "done" || !settled(last.status)) return undefined;
  return last;
}
