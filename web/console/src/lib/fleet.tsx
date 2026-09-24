import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { desktopEnsureService } from "./api/desktop";
import { emptySnapshot, fetchState } from "./api/fleet";
import { eventsURL, HTTPError } from "./http";
import { useResourceRead } from "@/hooks/use-resource-read";
import type { Event, Snapshot } from "./types";
import { NodeNamesContext, useNodeNames } from "./node-name";
import { serviceWatch } from "./service-watch";
import { statePoll } from "./state-poll";

export type Live = "connecting" | "live" | "reconnecting" | "unauthorized";

interface FleetState {
    snap: Snapshot;
    live: Live;
    refresh: () => void;
    hubUpdated: boolean;
}

const FleetContext = createContext<FleetState | null>(null);
const FleetEventsContext = createContext<Event[] | null>(null);
const ConsoleFeedContext = createContext<ConsoleFeed | null>(null);

// A streaming turn sends a fragment of text many times a second. Held in
// React state those fragments re-rendered every reader on every fragment,
// which is what made the console drop frames while work ran. The buffer
// lives outside React instead: readers subscribe, and once a frame they
// are handed whatever arrived since they last looked, in one batch.
const KEPT = 400;

// Text arrives faster than anyone can read it, and every update re-lays
// out the whole answer so far. Fragments are handed over at a rate a
// reader can follow; anything that is not just more text — a reply, a
// question, a change to the queue — is not held back.
const TEXT_INTERVAL = 80;

export type ConsoleReader = (batch: Event[]) => void;

export class ConsoleFeed {
    private kept: Event[] = [];
    private arrived = 0;
    private readers = new Map<ConsoleReader, number>();
    private frame: number | null = null;
    private timer: number | null = null;
    private urgent = false;
    private holding = false;
    private delivered = 0;

    push(event: Event) {
        this.kept.push({ ...event, n: ++this.arrived });
        if (this.kept.length > KEPT) this.kept.splice(0, this.kept.length - KEPT);
        if (!isStreamingProgress(event)) {
            this.urgent = true;
            // A reply does not wait behind the rate text is handed over at.
            if (this.holding && this.timer !== null) { clearTimeout(this.timer); this.timer = null; this.holding = false; }
        }
        this.wake();
    }

    // A reader that replays sees what is already buffered, so a page opened
    // in the middle of a turn still knows about the turn it walked in on.
    read(reader: ConsoleReader, replay: boolean): () => void {
        this.readers.set(reader, replay ? 0 : this.arrived);
        if (replay && this.kept.length) { this.urgent = true; this.wake(); }
        return () => { this.readers.delete(reader); };
    }

    stop() {
        if (this.frame !== null) cancelAnimationFrame(this.frame);
        if (this.timer !== null) clearTimeout(this.timer);
        this.frame = this.timer = null;
        this.holding = false;
    }

    private wake() {
        if (this.frame !== null || this.timer !== null) return;
        // A hidden tab is given no frames; its readers still need the events.
        if (typeof document !== "undefined" && document.hidden) {
            this.timer = window.setTimeout(() => { this.timer = null; this.deliver(); }, 250);
            return;
        }
        const wait = this.urgent ? 0 : Math.max(0, TEXT_INTERVAL - (performance.now() - this.delivered));
        if (wait > 0) { this.holding = true; this.timer = window.setTimeout(() => { this.timer = null; this.holding = false; this.wake(); }, wait); return; }
        this.frame = requestAnimationFrame(() => { this.frame = null; this.deliver(); });
    }

    private deliver() {
        this.urgent = false;
        this.delivered = performance.now();
        for (const [reader, cursor] of [...this.readers]) {
            if (!this.readers.has(reader)) continue;
            const batch = this.kept.filter((event) => (event.n ?? 0) > cursor);
            if (!batch.length) continue;
            this.readers.set(reader, batch[batch.length - 1].n ?? cursor);
            reader(batch);
        }
    }
}

// The snapshot is re-read when the stream says something moved, with a
// polling floor in case the stream drops silently (see statePoll).
export function FleetProvider({ children }: { children: ReactNode }) {
    const [snap, setSnap] = useState<Snapshot>(emptySnapshot);
    const [live, setLive] = useState<Live>("connecting");
    const [events, setEvents] = useState<Event[]>([]);
    const [feed] = useState(() => new ConsoleFeed());
    const pending = useRef<number | null>(null);
    const firstVersion = useRef<string | null>(null);
    const [hubUpdated, setHubUpdated] = useState(false);
    const poll = useRef<ReturnType<typeof statePoll> | null>(null);

    const load = useResourceRead("fleet", fetchState, (snapshot) => {
        if (snapshot.hub.version) {
            firstVersion.current ??= snapshot.hub.version;
            if (snapshot.hub.version !== firstVersion.current) setHubUpdated(true);
        }
        setSnap(snapshot);
        poll.current?.fresh();
        setLive((state) => state === "unauthorized" ? "connecting" : state);
    }, (error) => {
        if (error instanceof HTTPError && error.status === 401) setLive("unauthorized");
    });

    const refresh = useCallback(() => {
        if (pending.current) return;
        pending.current = window.setTimeout(() => { pending.current = null; void load(); }, 250);
    }, [load]);

    useEffect(() => {
        void load();
        const floor = poll.current = statePoll(() => void load());
        const shown = () => floor.visible(document.visibilityState !== "hidden");
        shown();
        document.addEventListener("visibilitychange", shown);
        let source: EventSource | null = null;
        let retry: number | null = null;
        let reconnecting = false;
        const watch = serviceWatch({ now: Date.now, ensure: desktopEnsureService(), after: 10_000, every: 30_000 });
        const connect = () => {
            source = new EventSource(eventsURL());
            source.onopen = () => {
                watch.up();
                floor.live(true);
                setLive("live");
                if (reconnecting) { reconnecting = false; void load(); }
            };
            source.onmessage = (e) => {
                const ev = JSON.parse(e.data) as Event;
                // The console follows its own traffic and the progress of
                // work asked from it; everything else is the activity feed.
                if (ev.kind.startsWith("console.") || ev.kind === "step.progress" || ev.kind === "delegate.progress") feed.push(ev);
                else setEvents((list) => [ev, ...list].slice(0, 300));
                // Text snapshots carry no fleet lifecycle changes. Re-reading
                // /state here also invalidates every snapshot consumer.
                if (!isStreamingProgress(ev)) refresh();
            };
            source.onerror = () => {
                watch.down();
                floor.live(false);
                reconnecting = true;
                setLive("reconnecting");
                source?.close();
                retry = window.setTimeout(connect, 3000);
            };
        };
        connect();
        return () => { floor.stop(); poll.current = null; document.removeEventListener("visibilitychange", shown); source?.close(); feed.stop(); if (retry) window.clearTimeout(retry); if (pending.current) window.clearTimeout(pending.current); };
    }, [load, refresh, feed]);

    const value = useMemo(() => ({ snap, live, refresh, hubUpdated }), [snap, live, refresh, hubUpdated]);
    const names = useNodeNames(snap.nodes);
    return <FleetContext.Provider value={value}><FleetEventsContext.Provider value={events}><NodeNamesContext.Provider value={names}><ConsoleFeedContext.Provider value={feed}>{children}</ConsoleFeedContext.Provider></NodeNamesContext.Provider></FleetEventsContext.Provider></FleetContext.Provider>;
}

export function useFleet(): FleetState {
    const ctx = useContext(FleetContext);
    if (!ctx) throw new Error("useFleet outside FleetProvider");
    return ctx;
}

export function useFleetEvents(): Event[] {
    const events = useContext(FleetEventsContext);
    if (!events) throw new Error("useFleetEvents outside FleetProvider");
    return events;
}

// useConsoleEvents hands a component the console events that arrived since
// its last turn at them, a frame at a time. The reader may close over
// anything it likes: the latest one is always the one called.
export function useConsoleEvents(reader: ConsoleReader, options?: { replay?: boolean }) {
    const feed = useContext(ConsoleFeedContext);
    if (!feed) throw new Error("useConsoleEvents outside FleetProvider");
    const latest = useRef(reader);
    useLayoutEffect(() => { latest.current = reader; });
    const replay = options?.replay !== false;
    useEffect(() => feed.read((batch) => latest.current(batch), replay), [feed, replay]);
}

export function isStreamingProgress(event: Event): boolean {
    // Delegation completion also changes its durable step and task state.
    if (event.step?.state && event.step.state !== "running") return false;
    return event.kind === "console.progress" || event.kind === "step.progress" || event.kind === "delegate.progress";
}

// Verbs sent from other pages land on the console: this is the hand-off.
interface Intent { text: string; mode: "run" | "fill"; n: number }
const IntentContext = createContext<{ intent: Intent | null; act: (t: string) => void; fill: (t: string) => void; consume: (n: number) => void } | null>(null);

export function IntentProvider({ children, onNavigate }: { children: ReactNode; onNavigate: () => void }) {
    const [intent, setIntent] = useState<Intent | null>(null);
    const act = useCallback((text: string) => { setIntent({ text, mode: "run", n: Date.now() }); onNavigate(); }, [onNavigate]);
    const fill = useCallback((text: string) => { setIntent({ text, mode: "fill", n: Date.now() }); onNavigate(); }, [onNavigate]);
    const consume = useCallback((n: number) => setIntent((current) => current?.n === n ? null : current), []);
    const value = useMemo(() => ({ intent, act, fill, consume }), [intent, act, fill, consume]);
    return <IntentContext.Provider value={value}>{children}</IntentContext.Provider>;
}

export function useIntent() {
    const ctx = useContext(IntentContext);
    if (!ctx) throw new Error("useIntent outside IntentProvider");
    return ctx;
}
