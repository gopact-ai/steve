import { useCallback } from "react";
import { useFleet } from "./fleet";
import type { Node } from "./types";

// A machine's identity is its node ID (node-<hash>), which never changes.
// People see the coordination display name when there is one; the ID
// stays visible in secondary text so the two are never confused.
export const nodeLabel = (n: Pick<Node, "name" | "display_name">) => n.display_name || n.name;

export const nodeLabelIn = (nodes: Node[], id: string) => {
    const n = nodes.find((item) => item.name === id);
    return n ? nodeLabel(n) : id;
};

// useNodeLabel resolves node IDs found on projects, agents and sessions
// to the display name from the fleet snapshot, falling back to the ID.
export function useNodeLabel(): (id: string) => string {
    const { snap } = useFleet();
    const nodes = snap.nodes;
    return useCallback((id: string) => nodeLabelIn(nodes, id), [nodes]);
}
