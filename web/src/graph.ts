// Layered DAG layout from `needs`: each step's column is its depth.
export interface GraphStep {
  id: string;
  needs?: string[];
  kind: "image" | "uses";
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
      kind: s.uses ? "uses" : "image",
      label: String(s.uses ?? s.image ?? ""),
    }));
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
