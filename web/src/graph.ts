// Layered DAG layout from `needs`: each step's column is its depth.
export interface GraphStep {
  id: string;
  needs?: string[];
  kind: "image" | "uses" | "approval" | "wait" | "agent" | "call" | "await";
  label: string;
}

export interface Placed extends GraphStep {
  x: number;
  y: number;
  depth: number;
}

export function stepsFromSpec(spec: unknown): GraphStep[] {
  const steps = (spec as { steps?: unknown[] })?.steps;
  if (!Array.isArray(steps)) return [];
  return steps
    .filter((s): s is Record<string, unknown> => !!s && typeof s === "object")
    .map((s, i) => ({
      id: typeof s.id === "string" && s.id ? s.id : `step-${i + 1}`,
      needs: Array.isArray(s.needs) ? s.needs.filter((n): n is string => typeof n === "string") : [],
      kind: s.uses
        ? "uses"
        : s.approval !== undefined
          ? "approval"
          : s.wait !== undefined
            ? "wait"
            : s.agent !== undefined
              ? "agent"
              : s.call !== undefined
                ? "call"
                : s.await !== undefined
                  ? "await"
                  : "image",
      label:
        s.approval !== undefined
          ? "approval"
          : s.wait !== undefined
            ? `wait ${String(s.wait)}`
            : s.agent !== undefined
              ? agentLabel(s.agent)
              : s.call !== undefined
                ? callLabel(s.call)
                : s.await !== undefined
                  ? `await ${awaitLabel(s.await)}`
                  : String(s.uses ?? s.image ?? ""),
    }));
}

// shortLabel is what a step shows at a glance: an image reference is
// pinned by digest, so its last path segment and tag are what reads.
export function shortLabel(step: Pick<GraphStep, "kind" | "label">): string {
  if (step.kind !== "image") return step.label;
  return (step.label.split("/").pop() ?? "").replace(/@sha256:[0-9a-f]+$/, "");
}

// stepSummary is the one line a collapsed step card shows: what the step
// runs and what it waits for ("alpine:3.24 · after lint, test").
export function stepSummary(step: unknown): string {
  const [g] = stepsFromSpec({ steps: [step] });
  if (!g) return "";
  return [shortLabel(g), g.needs?.length ? `after ${g.needs.join(", ")}` : ""].filter(Boolean).join(" · ");
}

// callLabel names a call step by the workflow it calls (ADR 0038).
export function callLabel(c: unknown): string {
  const w = c && typeof c === "object" ? (c as { workflow?: unknown }).workflow : undefined;
  return typeof w === "string" && w ? `call · ${w}` : "call";
}

// agentLabel names an agent step by its profile (ADR 0037).
function agentLabel(a: unknown): string {
  const p = a && typeof a === "object" ? (a as { profile?: unknown }).profile : undefined;
  return typeof p === "string" && p ? `agent · ${p}` : "agent";
}

function awaitLabel(a: unknown): string {
  const { integration, event } = (a ?? {}) as { integration?: unknown; event?: unknown };
  return [integration, event].filter((x) => typeof x === "string" && x).join(".");
}

export function layout(steps: GraphStep[], colWidth = 240, rowHeight = 96): Placed[] {
  const byId = new Map(steps.map((s) => [s.id, s]));
  const depth = new Map<string, number>();
  const visiting = new Set<string>();
  const d = (id: string): number => {
    if (depth.has(id)) return depth.get(id)!;
    if (visiting.has(id)) return 0; // cycle: validation reports it
    visiting.add(id);
    const s = byId.get(id);
    const v = s && s.needs && s.needs.length ? 1 + Math.max(...s.needs.filter((n) => byId.has(n)).map(d), -1) : 0;
    visiting.delete(id);
    depth.set(id, v);
    return v;
  };
  const rows = new Map<number, number>();
  return steps.map((s) => {
    const dep = d(s.id);
    const row = rows.get(dep) ?? 0;
    rows.set(dep, row + 1);
    return { ...s, depth: dep, x: dep * colWidth, y: row * rowHeight };
  });
}
