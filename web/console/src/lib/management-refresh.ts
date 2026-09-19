import type { Event, Snapshot } from "./types";

export type ManagementDomain = "skills" | "mcp" | "plugins";

const sorted = <T>(items: T[]) => items.sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b)));

// These are client dependency keys, not server concurrency revisions.
// /state omits skill assignments, MCP settings/probes and plugin configuration;
// their owners still need mutation refreshes and bounded recovery reads.
export function managementDependencyKey(domain: ManagementDomain, snapshot: Snapshot): string {
    const kind = domain === "skills" ? "skill" : domain === "mcp" ? "mcp" : "";
    return JSON.stringify({
        hub: [snapshot.hub.node, snapshot.hub.started],
        nodes: sorted(snapshot.nodes.map((node) => ({
            name: node.name, up: node.up,
            features: kind ? [...(node.features ?? [])].sort() : undefined,
            offers: kind ? sorted((node.snapshot?.offers ?? []).filter((offer) => offer.kind === kind).map((offer) => ({
                id: offer.id, scope: offer.scope, availability: offer.availability, version: offer.version,
                attrs: sorted(Object.entries(offer.attrs ?? {})),
                evidence: sorted((offer.evidence ?? []).map(({ kind, method, result, ok }) => ({ kind, method, result, ok }))),
            }))) : undefined,
        }))),
        agents: sorted(snapshot.agents.map((agent) => ({
            id: agent.id, node: agent.node,
            mcp: domain !== "skills" ? [...(agent.mcp_servers ?? [])].sort() : undefined,
            harness: domain === "plugins" ? agent.harness : undefined,
            model: domain === "plugins" ? agent.model : undefined,
            options: domain === "plugins" ? sorted(Object.entries(agent.options ?? {})) : undefined,
        }))),
    });
}

export function managementEventAffects(domain: ManagementDomain, event: Pick<Event, "kind" | "data">): boolean {
    // SkillShipper -> Model.Observe; manifest keys come from Capability.Key.
    if (domain === "skills" && event.kind === "observe.node.skills") return true;
    if (domain === "plugins" || event.kind !== "observe.node.manifest") return false;
    const prefix = domain === "skills" ? "skill:" : "mcp:";
    return (event.data?.changes ?? "").split("\n").some((line) => line.startsWith(prefix));
}
