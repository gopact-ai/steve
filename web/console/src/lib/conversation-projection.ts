import { applyLive, type Live } from "./live.ts";
import { applyDelegation, restoreDelegations, type Delegations } from "./delegations.ts";
import type { Event, Exchange, Reply } from "./types";
import { compareEventTime, eventTime } from "./event-time.ts";

type Resource = "replies" | "queue";
interface EventVersion {
    at: string; n: number; value: string;
    completed?: string; recalled?: string;
}
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
    versions: ReadonlyMap<string, EventVersion>;
    pendingProgress: ReadonlyMap<string, Event>;
    replayFloor: bigint;
    repairs: number;
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
        replayFloor: 0n, repairs: 0,
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
const terminal = (exchange: Exchange) => ["done", "failed", "cancelled"].includes(exchange.state);
// The owner retains the previous receipt when retrying an acknowledged stop.
// Its next terminal receipt is confirmed by queue, not old transcript replay.
const retrying = (exchanges: Exchange[], id: string) => exchanges.some((item) => item.id === id && running(item) && !!item.reply_id);
const closeCompleted = (exchanges: Exchange[], completed: ReadonlySet<string>): Exchange[] =>
    exchanges.map((item) => running(item) && completed.has(item.id) ? { ...item, state: "done" } : item);
const lineKinds = new Set(["console.sent", "console.reply", "console.notice", "console.milestone", "console.recalled"]);
const queueKinds = new Set(["console.sent", "console.reply", "console.queue"]);
const projectionKinds = new Set([...lineKinds, ...queueKinds, "console.progress", "step.progress", "delegate.progress", "console.step"]);
const orderReplies = (entries: Reply[]) => entries.sort((a, b) => compareEventTime(a.at, b.at)
    || (a.exchange_id && a.exchange_id === b.exchange_id ? Number(b.kind === "sent") - Number(a.kind === "sent") : 0)).slice(-200);

function eventKey(ev: Event): string {
    if (ev.kind === "delegate.progress" || ev.kind === "console.step") return `delegate:${ev.task_id ? "#" + ev.task_id : ev.step_id || ev.step?.id}`;
    if (ev.kind === "step.progress") return `step:${ev.exchange_id || ""}:${ev.step_id}`;
    return `${ev.kind}:${ev.reply_id || ev.exchange_id || (ev.kind === "console.progress" ? "turn" : ev.at)}`;
}

// Match the feed's bounded replay window. Current live steps can outnumber the
// window; they are current state, not retained session history. Retired events
// carry a time fence, so forgetting a tombstone never authorizes its replay.
const REPLAY_WINDOW = 400;
function compact(state: ConversationProjection): ConversationProjection {
    let replayFloor = state.replayFloor;
    let repair = false;
    // Queue's owner keeps 200 terminal receipts, plus every nonterminal row.
    // SSE must obey the same bound even when HTTP reconciliation is failing.
    let remaining = 200;
    let exchanges: Exchange[] = [];
    for (let i = state.exchanges.length - 1; i >= 0; i--) {
        const item = state.exchanges[i];
        if (!terminal(item) || remaining-- > 0) exchanges.push(item);
        else {
            const retired = eventTime(item.started_at || item.enqueued_at);
            if (retired > replayFloor) replayFloor = retired;
        }
    }
    exchanges.reverse();
    if (exchanges.length === state.exchanges.length) exchanges = state.exchanges;
    const active = new Set(exchanges.filter(running).map((item) => item.id));
    if (state.live?.exchangeID) active.add(state.live.exchangeID);
    const retries = new Set(exchanges.filter((item) => running(item) && item.reply_id).map((item) => item.id));
    const versions = new Map(state.versions);
    for (const [key, version] of versions) {
        if (versions.size <= REPLAY_WINDOW) break;
        if (key === "turn" || (key.startsWith("console.progress:") && active.has(key.slice(17))) || (state.live?.exchangeID && key.startsWith(`step:${state.live.exchangeID}:`))) continue;
        const retired = eventTime(version.at);
        if (retired > replayFloor) replayFloor = retired;
        versions.delete(key);
        if (key.startsWith("console.progress:")) repair = true;
    }
    const completed = new Set<string>(), recalled = new Set<string>();
    for (const version of versions.values()) {
        if (version.completed && !retries.has(version.completed)) completed.add(version.completed);
        if (version.recalled) recalled.add(version.recalled);
    }
    for (const item of exchanges) if (terminal(item)) completed.add(item.id);
    for (const line of state.entries) if (line.kind === "reply" && line.exchange_id && !retries.has(line.exchange_id)) completed.add(line.exchange_id);
    for (const id of completed) {
        const version = versions.get(`console.progress:${id}`);
        if (!version) continue;
        const retired = eventTime(version.at);
        if (retired > replayFloor) replayFloor = retired;
        versions.delete(`console.progress:${id}`);
    }
    const pendingProgress = new Map(state.pendingProgress);
    for (const id of pendingProgress.keys()) if (completed.has(id) || !versions.has(`console.progress:${id}`)) pendingProgress.delete(id);
    const referenced = new Set(state.entries.flatMap((line) => line.process?.steps?.map((step) => step.id) ?? []));
    const children = Object.entries(state.delegations);
    const retainedChildren = children.filter(([id, child]) =>
        child.step.state === "running" || referenced.has(id) || versions.has(`delegate:${id}`));
    const delegations = retainedChildren.length === children.length ? state.delegations : Object.fromEntries(retainedChildren);
    return { ...state, exchanges, completed, recalled, versions, pendingProgress, replayFloor, delegations,
        repairs: state.repairs + Number(repair),
        revisions: repair ? { replies: state.revisions.replies + 1, queue: state.revisions.queue + 1 } : state.revisions };
}

function projectEvent(state: ConversationProjection, ev: Event): ConversationProjection {
    if (ev.conversation !== state.conversation || !projectionKinds.has(ev.kind)) return state;
    const key = eventKey(ev), previous = state.versions.get(key);
    const activeExchange = ev.exchange_id && state.exchanges.find((item) => item.id === ev.exchange_id && running(item));
    const currentWork = activeExchange && !activeExchange.reply_id
        && (ev.kind === "console.progress" || ev.kind === "step.progress")
        && compareEventTime(ev.at, activeExchange.started_at || activeExchange.enqueued_at) >= 0;
    if (!previous && !currentWork && state.replayFloor && eventTime(ev.at) <= state.replayFloor) {
        // Outside retained evidence, read the owner rather than guessing whether
        // this is replay, a recalled line, or a genuinely delayed independent child.
        // The current active queue is retained evidence: its first progress may
        // precede an unrelated retired event, and must not poll on every fragment.
        return { ...state, cursor: Math.max(state.cursor, ev.n ?? 0), repairs: state.repairs + 1, revisions: { replies: state.revisions.replies + 1, queue: state.revisions.queue + 1 } };
    }
    const { n: _arrival, ...wire } = ev;
    const value = JSON.stringify(wire);
    // n is only an arrival cursor, not a server revision. Order each entity
    // by its event time; a late event for a different child still belongs here.
    if (previous && (compareEventTime(ev.at, previous.at) < 0 || (compareEventTime(ev.at, previous.at) === 0 && (ev.n ?? 0) < previous.n) || value === previous.value)) return state;
    if (ev.reply_id && state.recalled.has(ev.reply_id)) return state;
    if (ev.exchange_id && state.completed.has(ev.exchange_id) && (ev.kind === "console.progress" || ev.kind === "step.progress")) return state;

    const retriedReply = ev.kind === "console.reply" && !!ev.exchange_id && retrying(state.exchanges, ev.exchange_id);
    const version: EventVersion = { at: ev.at, n: ev.n ?? 0, value,
        completed: ev.kind === "console.reply" ? ev.exchange_id : undefined,
        recalled: ev.kind === "console.recalled" ? ev.reply_id : undefined };
    const versions = new Map(state.versions).set(key, version);
    let { entries, exchanges, live, completed, recalled, pendingProgress } = state;
    if (ev.kind === "console.reply" && ev.exchange_id && !retriedReply) completed = new Set(completed).add(ev.exchange_id);
    if (ev.kind === "console.recalled" && ev.reply_id) recalled = new Set(recalled).add(ev.reply_id);
    if (ev.kind === "console.progress" && ev.exchange_id && live?.exchangeID !== ev.exchange_id) {
        pendingProgress = new Map(pendingProgress).set(ev.exchange_id, ev);
    }
    if (ev.kind === "console.sent") {
        // Keep the newest turn's start even after its reply closes live.
        const lastTurn = versions.get("turn");
        const newerTurn = !lastTurn || compareEventTime(ev.at, lastTurn.at) > 0
            || (compareEventTime(ev.at, lastTurn.at) === 0 && (ev.n ?? 0) >= lastTurn.n);
        // Duplicate or delayed sent events enrich history, but never reset an
        // already streaming turn or reopen one whose terminal reply is known.
        if (newerTurn && (!ev.exchange_id || !completed.has(ev.exchange_id)) && (!live || !ev.exchange_id || live.exchangeID !== ev.exchange_id)
            && (!live || compareEventTime(ev.at, live.since) >= 0)) {
            live = applyLive(live, ev);
            versions.set("turn", { ...version, value: "" });
            const progress = ev.exchange_id ? pendingProgress.get(ev.exchange_id) : undefined;
            if (progress) live = applyLive(live, progress);
            if (ev.exchange_id) {
                const remaining = new Map(pendingProgress);
                remaining.delete(ev.exchange_id);
                pendingProgress = remaining;
            }
        }
        if (newerTurn && ev.exchange_id && !completed.has(ev.exchange_id)) {
            const existing = exchanges.find((item) => item.id === ev.exchange_id);
            const sent: Exchange = { ...existing, id: ev.exchange_id, conversation: state.conversation, input: ev.text || existing?.input || "", state: "running", enqueued_at: existing?.enqueued_at || ev.at, started_at: ev.at };
            exchanges = [...exchanges.filter((item) => item.id !== sent.id), sent];
        }
    } else if (!retriedReply && (ev.kind !== "step.progress" || !ev.exchange_id || ev.exchange_id === live?.exchangeID)) {
        live = applyLive(live, ev);
    }
    if (ev.kind === "console.reply" && ev.exchange_id && !retriedReply) {
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
    return compact({
        ...state, entries, exchanges, live, completed, recalled, versions, pendingProgress,
        delegations: applyDelegation(state.delegations, ev), cursor: Math.max(state.cursor, ev.n ?? 0),
        revisions: { replies: state.revisions.replies + Number(lineKinds.has(ev.kind)), queue: state.revisions.queue + Number(queueKinds.has(ev.kind)) },
    });
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
        const identity = (line: Reply) => line.id || `${line.exchange_id || ""}:${line.kind}:${line.at}`;
        const retained = new Set(entries.map(identity));
        const versions = new Map(state.versions);
        let replayFloor = state.replayFloor;
        for (const line of state.entries) if (!retained.has(identity(line))) {
            const retired = eventTime(line.at);
            if (retired > replayFloor) replayFloor = retired;
            versions.delete(eventKey({ kind: `console.${line.kind}`, at: line.at, reply_id: line.id, exchange_id: line.exchange_id }));
        }
        const completed = new Set(state.completed);
        for (const r of entries) if (r.kind === "reply" && r.exchange_id && !retrying(state.exchanges, r.exchange_id)) completed.add(r.exchange_id);
        return compact({ ...state, entries, completed, versions, replayFloor, exchanges: closeCompleted(state.exchanges, completed), enabled: action.enabled, loadingReplies: false, replyError: null,
            live: state.live?.exchangeID && completed.has(state.live.exchangeID) ? null : state.live,
            delegations: restoreDelegations(state.delegations, entries, token.cursor) });
    }
    const completed = new Set(state.completed);
    const exchanges = action.exchanges.filter((item) => item.conversation === state.conversation);
    for (const item of exchanges) if (running(item) && item.reply_id) completed.delete(item.id);
    for (const item of exchanges) if (terminal(item)) completed.add(item.id);
    const active = exchanges.filter((item) => running(item) && !completed.has(item.id)).sort((a, b) => compareEventTime(b.started_at || b.enqueued_at, a.started_at || a.enqueued_at));
    // Equal timestamps must not let an older queue row displace the SSE turn.
    const latest = active.find((item) => item.id === state.live?.exchangeID && compareEventTime(item.started_at || item.enqueued_at, active[0]?.started_at || active[0]?.enqueued_at || "") >= 0) || active[0];
    let live = latest ? state.live?.exchangeID === latest.id ? state.live : { since: latest.started_at || latest.enqueued_at, exchangeID: latest.id, steps: {}, order: [] } : null;
    const progress = latest && state.pendingProgress.get(latest.id);
    if (progress && !live?.turn) live = applyLive(live, progress);
    const versions = new Map(state.versions);
    if (latest && (!versions.has("turn") || compareEventTime(latest.started_at || latest.enqueued_at, versions.get("turn")!.at) > 0)) {
        versions.set("turn", { at: latest.started_at || latest.enqueued_at, n: token.cursor, value: "" });
    }
    let replayFloor = state.replayFloor;
    const pendingProgress = new Map(state.pendingProgress);
    for (const [id, event] of pendingProgress) {
        if (id === latest?.id || completed.has(id) || ((event.n ?? 0) <= token.cursor && !active.some((item) => item.id === id))) {
            pendingProgress.delete(id);
            if (id !== latest?.id) {
                versions.delete(`console.progress:${id}`);
                const retired = eventTime(event.at);
                if (retired > replayFloor) replayFloor = retired;
            }
        }
    }
    for (const old of state.exchanges) if (completed.has(old.id) && !exchanges.some((item) => item.id === old.id)) {
        const retired = eventTime(old.started_at || old.enqueued_at);
        if (retired > replayFloor) replayFloor = retired;
    }
    return compact({ ...state, exchanges: closeCompleted(exchanges, completed), live, completed, versions, pendingProgress, replayFloor, queueError: null });
}
