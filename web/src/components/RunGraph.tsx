import { useMemo } from "react";
import { ReactFlow, Background, Controls, Position, type Edge, type Node } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import type { StepInfo } from "../api";
import { layout, stepsFromSpec } from "../graph";

interface Props {
  spec: unknown;
  steps?: StepInfo[];
  selected?: string;
  onSelect?: (id: string) => void;
}

// RunGraph draws the pinned workflow version, so steps that have not
// started yet are visible with their status.
export function RunGraph({ spec, steps, selected, onSelect }: Props) {
  const { nodes, edges } = useMemo(() => {
    const status = new Map((steps ?? []).map((s) => [s.id, s]));
    const placed = layout(stepsFromSpec(spec));
    const nodes: Node[] = placed.map((p) => {
      const st = status.get(p.id);
      const state = st?.status ?? (steps ? "pending" : "");
      // Image references are pinned by digest; the tag is what reads.
      const label = p.kind === "uses" || p.kind === "agent" ? p.label : (p.label.split("/").pop() ?? "").replace(/@sha256:[0-9a-f]+$/, "");
      return {
        id: p.id,
        position: { x: p.x, y: p.y },
        data: {
          label: (
            <div className={`node ${state} kind-${p.kind}`} aria-label={`${p.id} ${state}`}>
              <strong title={p.id}>{p.id}</strong>
              <span className="muted small" title={p.label}>
                {label}
              </span>
              {state && (
                <span className={`badge ${state}`}>
                  {state}
                  {st && st.attempt > 1 ? ` · attempt ${st.attempt}` : ""}
                </span>
              )}
            </div>
          ),
        },
        selected: p.id === selected,
        className: state ? `st-${state}` : undefined,
        // The layout is left to right, so edges leave and enter sideways.
        sourcePosition: Position.Right,
        targetPosition: Position.Left,
        style: { width: 200 },
      };
    });
    const edges: Edge[] = placed.flatMap((p) =>
      (p.needs ?? []).map((n) => ({ id: `${n}->${p.id}`, source: n, target: p.id, animated: status.get(p.id)?.status === "running" })),
    );
    return { nodes, edges };
  }, [spec, steps, selected]);

  if (!nodes.length) return <p className="muted">No steps.</p>;
  return (
    <div className="graph" role="figure" aria-label="Workflow graph">
      <ReactFlow
        nodes={nodes}
        edges={edges}
        fitView
        fitViewOptions={{ maxZoom: 1, padding: 0.2 }}
        nodesDraggable={false}
        nodesConnectable={false}
        onNodeClick={(_, n) => onSelect?.(n.id)}
        proOptions={{ hideAttribution: true }}
      >
        <Background gap={18} size={1.2} />
        <Controls showInteractive={false} />
      </ReactFlow>
    </div>
  );
}
