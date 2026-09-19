import { applyLive, type Live } from "./live.ts";
import { applyDelegation, restoreDelegations, type Delegations } from "./delegations.ts";
import type { Event, Exchange, Reply } from "./types";

type Resource = "replies" | "queue";
export interface ConversationReadToken {
    conversation: string; generation: number; resource: Resource; request: number; revision: number; cursor: number;
}
export interface ConversationProjection {
    conversation: string;
    generation: number;
    entries: Reply[];
    exchanges: Exchange[];
    live: Live | null;
    delegations: Delegations;
    enabled: boolean;
    loadingReplies: boolean;
    replyError: unknown;
    queueError: unknown;
    revisions: Record<Resource, number>;
    requests: Record<Resource, number>;
    cursor: number;
    completed: ReadonlySet<string>;
    recalled: ReadonlySet<string>;
    versions: ReadonlyMap<string, { at: string; n: number; value: string }>;
    pendingProgress: ReadonlyMap<string, Event>;
}
export type ConversationAction =
    | { type: "select"; conversation: string }
    | { type: "events"; events: Event[] }
    | { type: "invalidate"; resource: Resource }
    | { type: "read-started"; token: ConversationReadToken }
    | { type: "read-failed"; token: ConversationReadToken; error: unknown }
    | { type: "replies"; token: ConversationReadToken; entries: Reply[]; enabled: boolean }
    | { type: "queue"; token: ConversationReadToken; exchanges: Exchange[] };

export function createConversationProjection(conversation: string, generation = 0): ConversationProjection {
    return {
        conversation, generation, entries: [], exchanges: [], live: null, delegations: {},
        enabled: true, loadingReplies: true, replyError: null, queueError: null,
        revisions: { replies: 0, queue: 0 }, requests: { replies: 0, queue: 0 }, cursor: 0,
        completed: new Set(), recalled: new Set(), versions: new Map(), pendingProgress: new Map(),
    };
}

export function conversationReadToken(state: ConversationProjection, resource: Resource, request: number): ConversationReadToken {
    return { conversation: state.conversation, generation: state.generation, resource, request, revision: state.revisions[resource], cursor: state.cursor };
}

export function acceptsConversationRead(state: ConversationProjection, token: ConversationReadToken): boolean {
    return token.conversation === state.conversation && token.generation === state.generation
        && token.request === state.requests[token.resource] && token.revision === state.revisions[token.resource];
}

const running = (exchange: Exchange) => ["running", "recovering", "awaiting-user"].includes(exchange.state);
const closeCompleted = (exchanges: Exchange[], completed: ReadonlySet<string>): Exchange[] =>
    exchanges.map((item) => running(item) && completed.has(item.id) ? { ...item, state: "done" } : item);
const lineKinds = new Set(["console.sent", "console.reply", "console.notice", "console.milestone", "console.recalled"]);
const queueKinds = new Set(["console.sent", "console.reply", "console.queue"]);
const projectionKinds = new Set([...lineKinds, ...queueKinds, "console.progress", "step.progress", "delegate.progress", "console.step"]);
const moment = (at: string) => Date.parse(at) || 0;
const orderReplies = (entries: Reply[]) => entries.sort((a, b) => moment(a.at) - moment(b.at)
    || (a.exchange_id && a.exchange_id === b.exchange_id ? Number(b.kind === "sent") - Number(a.kind === "sent") : 0)).slice(-200);

function eventKey(ev: Event): string {
    if (ev.kind === "delegate.progress" || ev.kind === "console.step") return `delegate:${ev.task_id ? "#" + ev.task_id : ev.step_id || ev.step?.id}`;
    if (ev.kind === "step.progress") return `step:${ev.exchange_id || ""}:${ev.step_id}`;
    return `${ev.kind}:${ev.reply_id || ev.exchange_id || (ev.kind === "console.progress" ? "turn" : ev.at)}`;
}

function projectEvent(state: ConversationProjection, ev: Event): ConversationProjection {
    if (ev.conversation !== state.conversation || !projectionKinds.has(ev.kind)) return state;
    const key = eventKey(ev), previous = state.versions.get(key);
    const { n: _arrival, ...wire } = ev;
    const value = JSON.stringify(wire);
    // n is only an arrival cursor, not a server revision. Order each entity
    // by its event time; a late event for a different child still belongs here.
    if (previous && (moment(ev.at) < moment(previous.at) || (ev.at === previous.at && (ev.n ?? 0) < previous.n) || value === previous.value)) return state;
    if (ev.reply_id && state.recalled.has(ev.reply_id)) return state;
    if (ev.exchange_id && state.completed.has(ev.exchange_id) && (ev.kind === "console.progress" || ev.kind === "step.progress")) return state;

    const version = { at: ev.at, n: ev.n ?? 0, value };
    const versions = new Map(state.versions).set(key, version);
    let { entries, exchanges, live, completed, recalled, pendingProgress } = state;
    if (ev.kind === "console.reply" && ev.exchange_id) completed = new Set(completed).add(ev.exchange_id);
    if (ev.kind === "console.recalled" && ev.reply_id) recalled = new Set(recalled).add(ev.reply_id);
    if (ev.kind === "console.progress" && ev.exchange_id && live?.exchangeID !== ev.exchange_id) {
        pendingProgress = new Map(pendingProgress).set(ev.exchange_id, ev);
    }
    if (ev.kind === "console.sent") {
        // Keep the newest turn's start even after its reply closes live.
        const lastTurn = versions.get("turn");
        const newerTurn = !lastTurn || moment(ev.at) > moment(lastTurn.at)
            || (moment(ev.at) === moment(lastTurn.at) && (ev.n ?? 0) >= lastTurn.n);
        // Duplicate or delayed sent events enrich history, but never reset an
        // already streaming turn or reopen one whose terminal reply is known.
        if (newerTurn && (!ev.exchange_id || !completed.has(ev.exchange_id)) && (!live || !ev.exchange_id || live.exchangeID !== ev.exchange_id)
            && (!live || moment(ev.at) >= moment(live.since))) {
            live = applyLive(live, ev);
            versions.set("turn", { ...version, value: "" });
            const progress = ev.exchange_id ? pendingProgress.get(ev.exchange_id) : undefined;
            if (progress) live = applyLive(live, progress);
        }
        if (newerTurn && ev.exchange_id && !completed.has(ev.exchange_id)) {
            const existing = exchanges.find((item) => item.id === ev.exchange_id);
            const sent: Exchange = { ...existing, id: ev.exchange_id, conversation: state.conversation, input: ev.text || existing?.input || "", state: "running", enqueued_at: existing?.enqueued_at || ev.at, started_at: ev.at };
            exchanges = [...exchanges.filter((item) => item.id !== sent.id), sent];
        }
    } else if (ev.kind !== "step.progress" || !ev.exchange_id || ev.exchange_id === live?.exchangeID) {
        live = applyLive(live, ev);
    }
    if (ev.kind === "console.reply" && ev.exchange_id) {
        exchanges = exchanges.map((item) => item.id === ev.exchange_id ? { ...item, state: "done", reply_id: ev.reply_id } : item);
        const remaining = new Map(pendingProgress);
        remaining.delete(ev.exchange_id);
        pendingProgress = remaining;
        versions.delete(`console.progress:${ev.exchange_id}`);
    }
    if (lineKinds.has(ev.kind)) {
        if (ev.kind === "console.recalled") entries = entries.filter((r) => r.id !== ev.reply_id);
        else {
            const kind = ev.kind.slice("console.".length);
            const line: Reply = { id: ev.reply_id, exchange_id: ev.exchange_id, at: ev.at, conversation: state.conversation, kind, title: ev.title, text: kind === "sent" ? "" : ev.text || "", format: ev.format, input: kind === "sent" ? ev.text : undefined, silent: ev.silent };
            const index = entries.findIndex((r) => line.id ? r.id === line.id : line.exchange_id ? r.exchange_id === line.exchange_id && r.kind === kind : r.at === line.at && r.kind === kind);
            entries = [...entries];
            if (index < 0) entries.push(line);
            else entries[index] = { ...entries[index], ...line };
            entries = orderReplies(entries);
        }
    }
    return {
        ...state, entries, exchanges, live, completed, recalled, versions, pendingProgress,
        delegations: applyDelegation(state.delegations, ev), cursor: Math.max(state.cursor, ev.n ?? 0),
        revisions: { replies: state.revisions.replies + Number(lineKinds.has(ev.kind)), queue: state.revisions.queue + Number(queueKinds.has(ev.kind)) },
    };
}

// Pure projection only: no HTTP, timers, draft writes, React or submission
// commands. Both surfaces consume exactly these transitions.
export function reduceConversation(state: ConversationProjection, action: ConversationAction): ConversationProjection {
    if (action.type === "select") return action.conversation === state.conversation ? state : createConversationProjection(action.conversation, state.generation + 1);
    if (action.type === "events") return action.events.reduce(projectEvent, state);
    if (action.type === "invalidate") return { ...state, revisions: { ...state.revisions, [action.resource]: state.revisions[action.resource] + 1 } };
    const { token } = action;
    if (action.type === "read-started") {
        if (token.conversation !== state.conversation || token.generation !== state.generation || token.request < state.requests[token.resource]) return state;
        return { ...state, requests: { ...state.requests, [token.resource]: token.request } };
    }
    if (!acceptsConversationRead(state, token)) return state;
    if (action.type === "read-failed") return token.resource === "queue" ? { ...state, queueError: action.error } : { ...state, replyError: action.error, loadingReplies: false };
    if (action.type === "replies") {
        const entries = action.entries.filter((r) => r.conversation === state.conversation && (!r.id || !state.recalled.has(r.id)));
        const completed = new Set(state.completed);
        for (const r of entries) if (r.kind === "reply" && r.exchange_id) completed.add(r.exchange_id);
        return { ...state, entries, completed, exchanges: closeCompleted(state.exchanges, completed), enabled: action.enabled, loadingReplies: false, replyError: null,
            live: state.live?.exchangeID && completed.has(state.live.exchangeID) ? null : state.live,
            delegations: restoreDelegations(state.delegations, entries, token.cursor) };
    }
    const completed = new Set(state.completed);
    const exchanges = action.exchanges.filter((item) => item.conversation === state.conversation);
    for (const item of exchanges) if (["done", "failed", "cancelled"].includes(item.state)) completed.add(item.id);
    const active = exchanges.filter((item) => running(item) && !completed.has(item.id)).sort((a, b) => moment(b.started_at || b.enqueued_at) - moment(a.started_at || a.enqueued_at));
    // Equal timestamps must not let an older queue row displace the SSE turn.
    const latest = active.find((item) => item.id === state.live?.exchangeID && moment(item.started_at || item.enqueued_at) >= moment(active[0]?.started_at || active[0]?.enqueued_at || "")) || active[0];
    let live = latest ? state.live?.exchangeID === latest.id ? state.live : { since: latest.started_at || latest.enqueued_at, exchangeID: latest.id, steps: {}, order: [] } : null;
    const progress = latest && state.pendingProgress.get(latest.id);
    if (progress && !live?.turn) live = applyLive(live, progress);
    const versions = new Map(state.versions);
    if (latest && (!versions.has("turn") || moment(latest.started_at || latest.enqueued_at) > moment(versions.get("turn")!.at))) {
        versions.set("turn", { at: latest.started_at || latest.enqueued_at, n: token.cursor, value: "" });
    }
    return { ...state, exchanges: closeCompleted(exchanges, completed), live, completed, versions, queueError: null };
}
