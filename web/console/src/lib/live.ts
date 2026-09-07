import type { Event, Progress, StepInfo } from "./types";

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
