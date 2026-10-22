import { describe, expect, it } from "vitest";
import { diffLines } from "./diff";
import { layout, stepsFromSpec } from "./graph";
import { canonicalJSON, parse, pointerToPath, rangeFor, setIn } from "./yamldoc";
import { pathAtLine, propertiesAt } from "./schema";
import { triggerFields } from "./pages/WorkflowDetail";
import { fmtCost, summarizeUsage } from "./usage";

const wf = `version: 1
name: demo
x-unknown: keep me # comment survives
steps:
  - id: a
    image: alpine
  - id: b
    image: alpine
    needs: [a]
    command: echo \${{ steps.a.outputs.x }} \${{ trigger.pr_number }}
`;

describe("yamldoc", () => {
  it("maps JSON pointers to source ranges", () => {
    const doc = parse(wf);
    const [from] = rangeFor(doc, "/steps/1/needs/0");
    expect(wf.slice(from, from + 1)).toBe("a");
    const [k] = rangeFor(doc, "/steps/0/nope");
    expect(k).toBeGreaterThan(0);
    expect(pointerToPath("/a~1b/0")).toEqual(["a/b", 0]);
  });
  it("round-trips unknown fields and comments through edits", () => {
    const doc = parse(wf);
    setIn(doc, ["steps", 0, "timeout"], "5m");
    setIn(doc, ["description"], "");
    const out = doc.toString();
    expect(out).toContain("x-unknown: keep me # comment survives");
    expect(out).toContain("timeout: 5m");
    expect(JSON.parse(canonicalJSON(parse(out)))["x-unknown"]).toBe("keep me");
  });
});

describe("graph", () => {
  it("layers steps by needs depth", () => {
    const placed = layout(stepsFromSpec(parse(wf).toJS()));
    expect(placed.map((p) => [p.id, p.depth])).toEqual([
      ["a", 0],
      ["b", 1],
    ]);
  });
  it("survives cycles", () => {
    const placed = layout([
      { id: "a", needs: ["b"], kind: "image", label: "" },
      { id: "b", needs: ["a"], kind: "image", label: "" },
    ]);
    expect(placed).toHaveLength(2);
  });
});

describe("diff", () => {
  it("marks adds and deletes", () => {
    expect(diffLines("a\nb\nc", "a\nc\nd").map((l) => l.kind)).toEqual(["same", "del", "same", "add"]);
  });
});

describe("schema", () => {
  const schema = {
    type: "object",
    properties: { name: { type: "string" }, steps: { type: "array", items: { $ref: "#/$defs/step" } } },
    $defs: { step: { type: "object", properties: { id: { description: "step id" }, image: {} } } },
  };
  it("completes keys at the cursor path", () => {
    const src = "name: x\nsteps:\n  - id: a\n    \n";
    const path = pathAtLine(src, 3, 4);
    expect(path).toEqual(["steps", 0]);
    expect(propertiesAt(schema, path!).map((p) => p.name)).toEqual(["id", "image"]);
    expect(propertiesAt(schema, []).map((p) => p.name)).toEqual(["name", "steps"]);
  });
});

describe("trigger fields", () => {
  it("collects trigger.* references for manual runs", () => {
    expect(triggerFields(parse(wf).toJS())).toEqual(["pr_number"]);
  });
});

describe("usage", () => {
  const row = (day: string, model: string, cost?: number) => ({
    provider: "claude-code",
    source: "profile:claude",
    day,
    model,
    input_tokens: 10,
    output_tokens: 5,
    cache_read_tokens: 2,
    cache_write_tokens: 1,
    messages: 1,
    cost_usd_estimate: cost,
    confidence: "high",
  });
  it("totals per day, newest first, and never sums unavailable rows", () => {
    const s = summarizeUsage([
      row("2025-03-01", "a", 0.5),
      row("2025-03-02", "a", 1.25),
      row("2025-03-02", "unpriced"),
      { ...row("", ""), source: "*", confidence: "unavailable", error: "boom" },
    ]);
    expect(s.days.map((d) => d.day)).toEqual(["2025-03-02", "2025-03-01"]);
    expect(s.days[0]).toMatchObject({ messages: 2, input: 20, cost: 1.25, unpriced: true });
    expect(s.days[1].unpriced).toBe(false);
    expect(s.total).toMatchObject({ messages: 3, cost: 1.75, unpriced: true });
    expect(s.unavailable).toHaveLength(1);
  });
  it("labels costs as estimates", () => {
    expect(fmtCost(undefined)).toBe("n/a");
    expect(fmtCost(0.01234)).toBe("~$0.0123");
    expect(fmtCost(12.5, true)).toBe("≥ ~$12.50");
  });
});
