import { useEffect, useMemo } from "react";
import { ReactFlow, Background, Controls, Position, useNodesInitialized, useReactFlow, useStore, type Edge, type FitViewOptions, type Node } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import type { StepInfo } from "../api";
import { layout, shortLabel, stepsFromSpec } from "../graph";
import { href } from "../router";

interface Props {
  spec: unknown;
  steps?: StepInfo[];
  selected?: string;
  onSelect?: (id: string) => void;
  // vertical lays steps out top to bottom, for a narrow pane.
  vertical?: boolean;
}

const fit: FitViewOptions = { maxZoom: 1, padding: 0.15 };

// Refit keeps the graph framed when its box changes size (a side pane
// that lays out after the first paint, a window resize) or when steps
// are added or removed. A status change keeps the person's zoom.
function Refit({ topology }: { topology: string }) {
  const { fitView } = useReactFlow();
  const ready = useNodesInitialized();
  const size = useStore((s) => `${s.width}x${s.height}`);
  useEffect(() => {
    if (!ready) return;
    const id = requestAnimationFrame(() => void fitView(fit));
    return () => cancelAnimationFrame(id);
  }, [ready, size, topology, fitView]);
  return null;
}

// RunGraph draws the pinned workflow version, so steps that have not
// started yet are visible with their status.
export function RunGraph({ spec, steps, selected, onSelect, vertical }: Props) {
  // Nodes with a status badge are taller, so their rows need more room.
  const rowHeight = steps ? 112 : vertical ? 96 : 84;
  const { nodes, edges, rows } = useMemo(() => {
    const status = new Map((steps ?? []).map((s) => [s.id, s]));
    // Top to bottom, a step's depth is its row and its rank in the layer
    // its column.
    const placed = layout(stepsFromSpec(spec), 240, rowHeight).map((p) => (vertical ? { ...p, x: (p.y / rowHeight) * 230, y: p.depth * rowHeight } : p));
    const nodes: Node[] = placed.map((p) => {
      const st = status.get(p.id);
      const state = st?.status ?? (steps ? "pending" : "");
      const label = shortLabel(p);
      // A call step opens its child run's graph (ADR 0038).
      const child = p.kind === "call" ? st?.call?.run_id : undefined;
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
              {child && (
                <a className="small" href={href("runs", child)} onClick={(e) => e.stopPropagation()}>
                  Open child run
                </a>
              )}
            </div>
          ),
        },
        selected: p.id === selected,
        className: state ? `st-${state}` : undefined,
        // Edges leave and enter along the layout's direction.
        sourcePosition: vertical ? Position.Bottom : Position.Right,
        targetPosition: vertical ? Position.Top : Position.Left,
        style: { width: 200 },
      };
    });
    const edges: Edge[] = placed.flatMap((p) =>
      (p.needs ?? []).map((n) => ({ id: `${n}->${p.id}`, source: n, target: p.id, animated: status.get(p.id)?.status === "running" })),
    );
    const rows = placed.reduce((m, p) => Math.max(m, p.y / rowHeight + 1), 0);
    return { nodes, edges, rows };
  }, [spec, steps, selected, rowHeight, vertical]);
  const topology = useMemo(() => nodes.map((n) => n.id).join(",") + "|" + edges.map((e) => e.id).join(","), [nodes, edges]);

  if (!nodes.length) return <p className="muted">No steps.</p>;
  // As tall as the widest layer needs, so a short workflow is not lost in
  // an empty canvas and a wide one is not squeezed.
  const height = Math.min(vertical ? 560 : 480, Math.max(220, rows * rowHeight + 110));
  return (
    <div className="graph" role="figure" aria-label="Workflow graph" style={{ height }}>
      <ReactFlow
        nodes={nodes}
        edges={edges}
        fitView
        fitViewOptions={fit}
        minZoom={0.2}
        nodesDraggable={false}
        nodesConnectable={false}
        onNodeClick={(_, n) => onSelect?.(n.id)}
        proOptions={{ hideAttribution: true }}
      >
        <Refit topology={topology} />
        <Background gap={18} size={1.2} />
        <Controls showInteractive={false} />
      </ReactFlow>
    </div>
  );
}
