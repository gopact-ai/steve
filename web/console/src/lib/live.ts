import type { Activity, Event, Progress, StepInfo } from "./types";

// Live is what the current line is doing: the turn's own progress, and
// each plan step's, until the reply lands.
export interface Live { since: string; exchangeID?: string; turn?: Progress; steps: Record<string, Progress>; order: string[]; info?: Record<string, StepInfo> }

// applyLive folds one event into the live view: a sent line opens it, a
// reply closes it, progress fills it in.
export function applyLive(cur: Live | null, ev: Event): Live | null {
    switch (ev.kind) {
        case "console.sent":
            return { since: ev.at, exchangeID: ev.exchange_id, steps: {}, order: [] };
        case "console.reply":
            return ev.exchange_id && cur?.exchangeID && ev.exchange_id !== cur.exchangeID ? cur : null;
        case "console.progress":
            if (ev.exchange_id && (!cur || ev.exchange_id !== cur.exchangeID)) return cur;
            return { ...(cur ?? { since: ev.at, exchangeID: ev.exchange_id, steps: {}, order: [] }), turn: ev.progress };
        case "step.progress": {
            // Delegations are folded separately by task ID, independent
            // of this turn's lifetime. Only plan steps belong here.
            if (!cur) return null;
            const base = cur;
            const id = ev.step_id || "?";
            const info = ev.step ? { ...(base.info ?? {}), [id]: ev.step } : base.info;
            return { ...base, steps: { ...base.steps, [id]: ev.progress || {} }, order: base.order.includes(id) ? base.order : [...base.order, id], info };
        }
        default:
            return cur;
    }
}

// LiveActivity is the latest streamed progress of one agent: what it is
// doing right now, ahead of the snapshot the fleet page re-reads on its
// floor. It mirrors what the hub notes from the same events.
export interface LiveActivity { agent: string; taskID?: string; tool?: string; detail?: string; at: string }

export function applyActivity(cur: Record<string, LiveActivity>, ev: Event): Record<string, LiveActivity> {
    const agent = ev.progress?.agent;
    if (!agent || (ev.kind !== "console.progress" && ev.kind !== "step.progress" && ev.kind !== "delegate.progress")) return cur;
    let tool: string | undefined;
    let detail: string | undefined;
    for (const call of ev.progress?.tools ?? []) {
        tool = call.kind;
        detail = call.name;
        if (call.status === "running") break;
    }
    return { ...cur, [agent]: { agent, taskID: ev.task_id, tool, detail, at: ev.at || new Date().toISOString() } };
}

// withLiveActivity overlays the streamed activity on the snapshot's: a
// newer tool call replaces the recorded one, and an agent the snapshot
// has not caught up with yet shows its task rather than "idle".
export function withLiveActivity(activities: Activity[] | undefined, live: LiveActivity | undefined): Activity[] {
    const list = activities ?? [];
    if (!live) return list;
    const index = list.findIndex((x) => !live.taskID || !x.task_id || x.task_id === live.taskID);
    if (index < 0) {
        if (list.length) return list;
        return [{ agent: live.agent, attempt_id: "live:" + live.agent, task_id: live.taskID, tool: live.tool, detail: live.detail, since: live.at, at: live.at }];
    }
    const current = list[index];
    if (current.at && current.at >= live.at) return list;
    return list.map((item, i) => (i === index ? { ...item, tool: live.tool ?? item.tool, detail: live.detail ?? item.detail, at: live.at } : item));
}
