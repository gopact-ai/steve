import { createContext, useContext, useMemo } from "react";
import type { Node } from "./types";

// A machine's identity is its node ID (node-<hash>), which never changes.
// People see the coordination display name when there is one; the ID
// stays visible in secondary text so the two are never confused.
export const nodeLabel = (n: Pick<Node, "name" | "display_name">) => n.display_name || n.name;

export const nodeLabelIn = (nodes: Node[], id: string) => {
    const n = nodes.find((item) => item.name === id);
    return n ? nodeLabel(n) : id;
};

// Machines are named far less often than the fleet snapshot is re-read,
// and the transcript resolves a name on every reply. So the names live
// in their own context: a poll of /state leaves it untouched, and only
// a rename or a machine joining re-renders the readers.
const asGiven = (id: string) => id;
export const NodeNamesContext = createContext<(id: string) => string>(asGiven);

export function useNodeNames(nodes: Node[]): (id: string) => string {
    const named = nodes.map((n) => `${n.name}\u0000${nodeLabel(n)}`).join("\u001f");
    return useMemo(() => {
        const names = new Map(named ? named.split("\u001f").map((pair) => pair.split("\u0000") as [string, string]) : []);
        return (id: string) => names.get(id) || id;
    }, [named]);
}

// useNodeLabel resolves node IDs found on projects, agents and sessions
// to the display name from the fleet snapshot, falling back to the ID.
export function useNodeLabel(): (id: string) => string {
    return useContext(NodeNamesContext);
}

// who is "agent @ machine" the way a person reads it: the agent's name
// and the machine's name, never the machine's ID.
export const whoIs = (label: (id: string) => string, agent?: string, node?: string) =>
    [agent, node ? label(node) : ""].filter(Boolean).join(" @ ");
