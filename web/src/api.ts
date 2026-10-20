// Thin client for the daemon's HTTP API. The UI authenticates with an
// HttpOnly cookie session created by pasting a token; every mutation also
// sends the CSRF header the daemon requires for cookie-authenticated calls.

export interface Issue {
  path: string;
  message: string;
  hint?: string;
}

export class ApiError extends Error {
  status: number;
  issues: Issue[];
  constructor(status: number, message: string, issues: Issue[] = []) {
    super(message);
    this.status = status;
    this.issues = issues;
  }
}

export const CSRF_HEADER = "X-Icaro-CSRF";

async function request<T>(method: string, path: string, body?: unknown, raw = false): Promise<T> {
  const headers: Record<string, string> = {};
  let payload: BodyInit | undefined;
  if (body !== undefined) {
    if (typeof body === "string") {
      headers["Content-Type"] = "application/yaml";
      payload = body;
    } else {
      headers["Content-Type"] = "application/json";
      payload = JSON.stringify(body);
    }
  }
  if (method !== "GET") headers[CSRF_HEADER] = "1";
  const res = await fetch(path, { method, headers, body: payload, credentials: "same-origin" });
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
  if (raw) return (await res.text()) as unknown as T;
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export interface WorkflowInfo {
  name: string;
  description?: string;
  version: number;
  status: string;
  steps: string[];
  triggers?: string[];
  created_at: string;
  updated_at: string;
  yaml?: string;
  spec?: unknown;
}

export interface VersionInfo {
  version: number;
  actor: string;
  created_at: string;
  yaml?: string;
}

export interface StepInfo {
  id: string;
  status: string;
  attempt: number;
  exit_code?: number;
  outputs: Record<string, unknown>;
  error?: string;
  log_size: number;
  started_at?: string;
  finished_at?: string;
}

export interface RunInfo {
  id: string;
  workflow: string;
  version: number;
  status: string;
  trigger: string;
  trigger_payload?: Record<string, unknown>;
  error?: string;
  created_at: string;
  started_at?: string;
  finished_at?: string;
  steps?: StepInfo[];
}

export interface ValidateResult {
  valid: boolean;
  name?: string;
  issues: Issue[];
}

export interface InputInfo {
  type: string;
  required?: boolean;
  description?: string;
  default?: unknown;
}

export interface ActionInfo {
  ref: string;
  description?: string;
  inputs: Record<string, InputInfo>;
  outputs: string[];
  hosts: string[];
}

export interface EventInfo {
  integration: string;
  event: string;
  description?: string;
  fields: string[];
}

export interface IntegrationInfo {
  name: string;
  pack: string;
  description?: string;
  events: EventInfo[];
  actions: ActionInfo[];
  hosts?: string[];
  error?: string;
  shadowed?: string;
}

export interface Delivery {
  id: string;
  integration: string;
  event?: string;
  verified: boolean;
  verify_error?: string;
  received_at: string;
  matches?: { workflow: string; run_id?: string; skipped?: string; deduplicated?: boolean; filtered?: boolean }[];
  headers?: Record<string, string>;
  body?: string;
}

export const api = {
  me: () => request<{ name: string; scope: string }>("GET", "/api/v1/ui/me"),
  login: (token: string) => request<{ name: string; scope: string }>("POST", "/api/v1/ui/login", { token }),
  logout: () => request<void>("POST", "/api/v1/ui/logout"),

  schema: () => request<object>("GET", "/api/v1/schema/workflow"),
  workflows: () => request<{ workflows: WorkflowInfo[] }>("GET", "/api/v1/workflows").then((r) => r.workflows ?? []),
  workflow: (name: string) => request<WorkflowInfo>("GET", `/api/v1/workflows/${encodeURIComponent(name)}`),
  validate: (yaml: string) => request<ValidateResult>("POST", "/api/v1/workflows/validate", yaml),
  apply: (yaml: string) => request<WorkflowInfo>("POST", "/api/v1/workflows", yaml),
  versions: (name: string) =>
    request<{ versions: VersionInfo[] }>("GET", `/api/v1/workflows/${encodeURIComponent(name)}/versions`).then((r) => r.versions ?? []),
  version: (name: string, v: number) => request<VersionInfo>("GET", `/api/v1/workflows/${encodeURIComponent(name)}/versions/${v}`),
  setEnabled: (name: string, enabled: boolean) =>
    request<WorkflowInfo>("POST", `/api/v1/workflows/${encodeURIComponent(name)}/${enabled ? "enable" : "disable"}`),

  run: (name: string, trigger: Record<string, unknown>) =>
    request<RunInfo>("POST", `/api/v1/workflows/${encodeURIComponent(name)}/runs`, { trigger }),
  runs: (workflow?: string) =>
    request<{ runs: RunInfo[] }>("GET", `/api/v1/runs${workflow ? `?workflow=${encodeURIComponent(workflow)}` : ""}`).then((r) => r.runs ?? []),
  getRun: (id: string) => request<RunInfo>("GET", `/api/v1/runs/${id}`),
  cancel: (id: string) => request<RunInfo>("POST", `/api/v1/runs/${id}/cancel`),
  rerun: (id: string) => request<RunInfo>("POST", `/api/v1/runs/${id}/rerun`),
  stepLogs: (id: string, step: string) => request<string>("GET", `/api/v1/runs/${id}/steps/${encodeURIComponent(step)}/logs`, undefined, true),

  integrations: () => request<{ integrations: IntegrationInfo[] }>("GET", "/api/v1/integrations").then((r) => r.integrations ?? []),
  manifest: (name: string) => request<{ yaml: string; pack: string; writable: boolean }>("GET", `/api/v1/integrations/${encodeURIComponent(name)}/manifest`),
  saveManifest: (name: string, yaml: string) =>
    request<{ commit: string }>("PUT", `/api/v1/integrations/${encodeURIComponent(name)}/manifest`, { yaml }),
  pushPack: (pack: string) => request<{ output: string }>("POST", `/api/v1/packs/${encodeURIComponent(pack)}/push`),

  deliveries: () => request<{ deliveries: Delivery[] }>("GET", "/api/v1/deliveries").then((r) => r.deliveries ?? []),
  delivery: (id: string) => request<Delivery>("GET", `/api/v1/deliveries/${id}`),
  redeliver: (id: string) => request<Delivery>("POST", `/api/v1/deliveries/${id}/redeliver`),
};

export type RunEvent =
  | { type: "status"; run: RunInfo }
  | { type: "log"; step: string; attempt: number; offset: number; text: string };

// subscribeRun opens the per-run SSE stream; the browser reconnects with
// Last-Event-ID, so the stream resumes instead of polling.
export function subscribeRun(id: string, onEvent: (e: RunEvent) => void, onError?: () => void): () => void {
  const es = new EventSource(`/api/v1/runs/${id}/events`, { withCredentials: true });
  es.addEventListener("status", (m) => onEvent({ type: "status", run: JSON.parse((m as MessageEvent).data) }));
  es.addEventListener("log", (m) => onEvent({ type: "log", ...JSON.parse((m as MessageEvent).data) }));
  if (onError) es.onerror = onError;
  return () => es.close();
}

export const terminal = (status: string) => ["succeeded", "failed", "cancelled"].includes(status);
