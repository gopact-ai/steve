import type { Snapshot } from "./types";

// /context depends on the agent choices and project placement, not /state's
// observation time, task traffic, tool activity, repository status or usage.
// This is a client dependency key, not a server-issued concurrency revision.
export function conversationContextRevision(snapshot: Snapshot): string {
    return JSON.stringify({
        agents: snapshot.agents.map(({ id, node, harness, model, eligible, why, reason, reason_detail, default: preferred }) =>
            ({ id, node, harness, model, eligible, why, reason, reason_detail, preferred })).sort((a, b) => a.id.localeCompare(b.id)),
        projects: snapshot.projects.map(({ id, node, path, level, repo, default: preferred, workspaces }) =>
            ({ id, node, path, level, repo, preferred, workspaces: (workspaces || []).map(({ id, node, kind, path, state }) =>
                ({ id, node, kind, path, state })).sort((a, b) => a.id.localeCompare(b.id)) })).sort((a, b) => a.id.localeCompare(b.id)),
    });
}
