import { acceptsConversationRead, conversationReadToken, createConversationProjection, reduceConversation, type ConversationAction } from "./conversation-projection.ts";
import type { Event, Exchange, Reply } from "./types";

interface ConversationPorts {
    fetchReplies: (conversation: string, signal: AbortSignal) => Promise<{ enabled: boolean; replies: Reply[] }>;
    fetchQueue: (conversation: string, signal: AbortSignal) => Promise<{ queue: Exchange[] }>;
    reconcileSubmission: (conversation: string, exchanges: Exchange[]) => Promise<unknown>;
}

// One mounted conversation owns its requests. The controller only reads and
// reconciles receipts; sending, stopping and editing drafts remain commands
// owned by the surfaces, outside the public state projection.
export class ConversationController {
    readonly conversation: string;
    private ports: ConversationPorts;
    private state: ReturnType<typeof createConversationProjection>;
    private active = true;
    private listeners = new Set<() => void>();
    private requests = { queue: 0, replies: 0 };
    private aborts = new Set<AbortController>();

    constructor(conversation: string, ports: ConversationPorts) {
        this.conversation = conversation;
        this.ports = ports;
        this.state = createConversationProjection(conversation);
    }

    getSnapshot = () => this.state;
    subscribe = (listener: () => void) => { this.listeners.add(listener); return () => { this.listeners.delete(listener); }; };

    activate = () => {
        if (this.active) return;
        this.active = true;
        this.state = createConversationProjection(this.conversation, this.state.generation + 1);
        for (const listener of this.listeners) listener();
    };

    dispose = () => {
        this.active = false;
        for (const controller of this.aborts) controller.abort();
        this.aborts.clear();
    };

    private dispatch(action: ConversationAction) {
        if (!this.active) return;
        const next = reduceConversation(this.state, action);
        if (next === this.state) return;
        this.state = next;
        for (const listener of this.listeners) listener();
    }

    receive = (events: Event[]) => {
        if (!this.active) return;
        const before = this.state;
        this.dispatch({ type: "events", events });
        if (this.state.revisions.queue !== before.revisions.queue) void this.loadQueue();
        if (this.state.revisions.replies !== before.revisions.replies
            && (this.state.repairs !== before.repairs || events.some((ev) => ev.conversation === this.conversation && (ev.kind === "console.sent" || ev.kind === "console.reply")))) void this.loadReplies();
    };

    invalidateReplies = () => {
        this.dispatch({ type: "invalidate", resource: "replies" });
        return this.loadReplies();
    };
    loadQueue = async () => { await this.read("queue"); };
    loadReplies = async () => { await this.read("replies"); };
    reload = async () => { await Promise.all([this.loadQueue(), this.loadReplies()]); };

    private async read(resource: "queue" | "replies") {
        if (!this.active) return;
        const abort = new AbortController();
        this.aborts.add(abort);
        const request = ++this.requests[resource];
        const generation = this.state.generation;
        const current = () => this.active && !abort.signal.aborted && request === this.requests[resource] && generation === this.state.generation;
        // A lifecycle event may overtake HTTP or the asynchronous draft lock.
        // Retry once; polling supplies recovery during continuous mutations.
        try {
            for (let attempt = 0; attempt < 2 && current(); attempt++) {
                const token = conversationReadToken(this.state, resource, request);
                this.dispatch({ type: "read-started", token });
                try {
                    if (resource === "queue") {
                        const data = await this.ports.fetchQueue(this.conversation, abort.signal);
                        if (!current()) return;
                        if (!acceptsConversationRead(this.state, token)) continue;
                        await this.ports.reconcileSubmission(this.conversation, data.queue || []);
                        if (!current()) return;
                        if (!acceptsConversationRead(this.state, token)) continue;
                        this.dispatch({ type: "queue", token, exchanges: data.queue || [] });
                    } else {
                        const data = await this.ports.fetchReplies(this.conversation, abort.signal);
                        if (!current()) return;
                        if (!acceptsConversationRead(this.state, token)) continue;
                        this.dispatch({ type: "replies", token, entries: data.replies || [], enabled: data.enabled });
                    }
                    return;
                } catch (error) {
                    if (!current()) return;
                    if (!acceptsConversationRead(this.state, token)) continue;
                    this.dispatch({ type: "read-failed", token, error });
                    return;
                }
            }
        } finally { this.aborts.delete(abort); }
    }
}
