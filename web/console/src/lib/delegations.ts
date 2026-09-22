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
    if (!reply.process?.steps?.some((s) => children[s.id] && children[s.id].step !== s)) return reply;
    return { ...reply, process: { ...reply.process, steps: reply.process.steps.map((s) => children[s.id]?.step ?? s) } };
}

// A delegated child is an event of the thread, not a header above it: it
// belongs where it started. These two keep that ordering out of the page.

// childrenOfTurn splits the children no reply has recorded yet into the
// ones this turn started — they belong inside the line in flight — and
// the older ones, which belong back where they were triggered.
export function childrenOfTurn(children: StepProcess[], since?: string): { current: StepProcess[]; earlier: StepProcess[] } {
    const start = since ? Date.parse(since) : Number.NaN;
    if (Number.isNaN(start)) return { current: [], earlier: children };
    const current: StepProcess[] = [];
    const earlier: StepProcess[] = [];
    for (const child of children) {
        const began = child.since ? Date.parse(child.since) : Number.NaN;
        // A child without a start time is attributed to the running turn
        // only once nothing older can claim it.
        (Number.isNaN(began) || began >= start ? current : earlier).push(child);
    }
    return { current, earlier };
}

// streamWithChildren places each unrecorded child among the replies by
// the time it started, so the transcript reads in the order things
// happened instead of collecting every card at the top.
export function streamWithChildren(replies: Reply[], children: StepProcess[]): { reply?: Reply; child?: StepProcess }[] {
    if (!children.length) return replies.map((reply) => ({ reply }));
    const moment = (value?: string) => (value ? Date.parse(value) : Number.NaN);
    const rest = [...children].sort((a, b) => (moment(a.since) || 0) - (moment(b.since) || 0));
    const out: { reply?: Reply; child?: StepProcess }[] = [];
    for (const reply of replies) {
        const landed = moment(reply.at);
        while (rest.length && moment(rest[0].since) < landed) out.push({ child: rest.shift()! });
        out.push({ reply });
    }
    for (const child of rest) out.push({ child });
    return out;
}
