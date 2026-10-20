import { useMemo } from "react";
import { ReactFlow, Background, Controls, type Edge, type Node } from "@xyflow/react";
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
      return {
        id: p.id,
        position: { x: p.x, y: p.y },
        data: {
          label: (
            <div className={`node ${state}`} aria-label={`${p.id} ${state}`}>
              <strong>{p.id}</strong>
              <span className="muted small">{p.kind === "uses" ? p.label : p.label.split("/").pop()}</span>
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
        nodesDraggable={false}
        nodesConnectable={false}
        onNodeClick={(_, n) => onSelect?.(n.id)}
        proOptions={{ hideAttribution: true }}
      >
        <Background />
        <Controls showInteractive={false} />
      </ReactFlow>
    </div>
  );
}
