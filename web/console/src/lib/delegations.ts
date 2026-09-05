import type { Event, Reply, StepProcess } from "./types";

// Children have their own lifetime. A turn starting or ending must not
// clear their snapshots or attach an older child to a newer reply.
export type Delegations = Record<string, { step: StepProcess; n: number }>;

export function applyDelegation(cur: Delegations, ev: Event): Delegations {
    if (ev.kind !== "delegate.progress" && ev.kind !== "console.step") return cur;
    const id = ev.task_id ? `#${ev.task_id}` : ev.step_id || ev.step?.id;
    if (!id) return cur;
    const step: StepProcess = { ...ev.progress, ...ev.step, kind: "delegate", id };
    return { ...cur, [id]: { step, n: ev.n ?? 0 } };
}

export function restoreDelegations(cur: Delegations, replies: Reply[], cursor: number): Delegations {
    const next = { ...cur };
    for (const r of replies) for (const step of r.process?.steps || []) {
        // Events received during this fetch are newer than its snapshot.
        if (step.kind === "delegate" && (cur[step.id]?.n ?? 0) <= cursor) next[step.id] = { step, n: cursor };
    }
    return next;
}

export function withDelegations(reply: Reply, children: Delegations): Reply {
    if (!reply.process?.steps?.some((s) => children[s.id])) return reply;
    return { ...reply, process: { ...reply.process, steps: reply.process.steps.map((s) => children[s.id]?.step ?? s) } };
}
