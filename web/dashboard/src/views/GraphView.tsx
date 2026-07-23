import { useMemo } from "react";
import { cli, type GraphNodeType } from "../api/cli";
import { useAsync } from "../hooks/useAsync";
import { Empty, ErrorBox, Loading } from "./common";

const COLUMNS: GraphNodeType[] = ["MODULE", "SERVICE", "ENDPOINT"];
const COL_WIDTH = 260;
const ROW_HEIGHT = 56;
const NODE_W = 180;
const NODE_H = 34;
const PAD = 32;

interface Placed {
  id: string;
  type: GraphNodeType;
  x: number;
  y: number;
}

export function GraphView() {
  const graph = useAsync((s) => cli.getServiceDependencyGraph(s), [], 8000);

  const layout = useMemo(() => {
    const nodes = graph.data?.nodes ?? [];
    const edges = graph.data?.edges ?? [];
    const byColumn = new Map<GraphNodeType, number>();
    const placed = new Map<string, Placed>();

    for (const node of nodes) {
      if (!node.id) continue;
      const type = (node.type ?? "SERVICE") as GraphNodeType;
      const col = COLUMNS.indexOf(type);
      const row = byColumn.get(type) ?? 0;
      byColumn.set(type, row + 1);
      placed.set(node.id, {
        id: node.id,
        type,
        x: PAD + (col < 0 ? COLUMNS.length : col) * COL_WIDTH,
        y: PAD + row * ROW_HEIGHT,
      });
    }

    const rows = Math.max(0, ...COLUMNS.map((t) => byColumn.get(t) ?? 0));
    return {
      placed,
      lines: edges
        .map((e) => ({ from: placed.get(e.from ?? ""), to: placed.get(e.to ?? "") }))
        .filter((l): l is { from: Placed; to: Placed } => !!l.from && !!l.to),
      width: PAD * 2 + COLUMNS.length * COL_WIDTH,
      height: PAD * 2 + rows * ROW_HEIGHT,
    };
  }, [graph.data]);

  if (graph.loading && !graph.data) return <Loading />;
  if (graph.error) return <ErrorBox error={graph.error} />;
  if (layout.placed.size === 0) return <Empty>No dependency graph for the active workspace.</Empty>;

  const nodes = [...layout.placed.values()];

  return (
    <div className="graph">
      <svg width={layout.width} height={layout.height} role="img" aria-label="dependency graph">
        {layout.lines.map((line, i) => (
          <line
            key={i}
            x1={line.from.x + NODE_W}
            y1={line.from.y + NODE_H / 2}
            x2={line.to.x}
            y2={line.to.y + NODE_H / 2}
            className="graph-edge"
          />
        ))}
        {nodes.map((node) => (
          <g key={node.id} transform={`translate(${node.x}, ${node.y})`}>
            <rect
              width={NODE_W}
              height={NODE_H}
              rx={6}
              className={`graph-node node-${node.type.toLowerCase()}`}
            />
            <text x={10} y={NODE_H / 2 + 4} className="graph-label">
              {label(node.id)}
            </text>
          </g>
        ))}
      </svg>
    </div>
  );
}

function label(id: string): string {
  const parts = id.split("/");
  const short = parts[parts.length - 1] || id;
  return short.length > 22 ? `${short.slice(0, 21)}…` : short;
}
