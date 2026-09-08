import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { emptySnapshot, fetchState } from "./api/fleet";
import { eventsURL, HTTPError } from "./http";
import { useResourceRead } from "@/hooks/use-resource-read";
import type { Event, Snapshot } from "./types";

export type Live = "connecting" | "live" | "reconnecting" | "unauthorized";

interface FleetState {
    snap: Snapshot;
    live: Live;
    events: Event[];
    refresh: () => void;
    hubUpdated: boolean;
}

const FleetContext = createContext<FleetState | null>(null);
const ConsoleEventsContext = createContext<Event[] | null>(null);

// The snapshot is re-read when the stream says something moved, with a
// polling floor in case the stream drops silently.
export function FleetProvider({ children }: { children: ReactNode }) {
    const [snap, setSnap] = useState<Snapshot>(emptySnapshot);
    const [live, setLive] = useState<Live>("connecting");
    const [events, setEvents] = useState<Event[]>([]);
    const [consoleEvents, setConsoleEvents] = useState<Event[]>([]);
    const pending = useRef<number | null>(null);
    const firstVersion = useRef<string | null>(null);
    const [hubUpdated, setHubUpdated] = useState(false);

    const load = useResourceRead("fleet", fetchState, (snapshot) => {
        if (snapshot.hub.version) {
            firstVersion.current ??= snapshot.hub.version;
            if (snapshot.hub.version !== firstVersion.current) setHubUpdated(true);
        }
        setSnap(snapshot);
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
        const floor = window.setInterval(() => void load(), 10000);
        let source: EventSource | null = null;
        let retry: number | null = null;
        let arrived = 0;
        let reconnecting = false;
        const connect = () => {
            source = new EventSource(eventsURL());
            source.onopen = () => {
                setLive("live");
                if (reconnecting) { reconnecting = false; void load(); }
            };
            source.onmessage = (e) => {
                const ev = JSON.parse(e.data) as Event;
                // The console follows its own traffic and the progress of
                // work asked from it; everything else is the activity feed.
                if (ev.kind.startsWith("console.") || ev.kind === "step.progress" || ev.kind === "delegate.progress") setConsoleEvents((list) => [...list.slice(-399), { ...ev, n: ++arrived }]);
                else setEvents((list) => [ev, ...list].slice(0, 300));
                // Text snapshots carry no fleet lifecycle changes. Re-reading
                // /state here also invalidates every snapshot consumer.
                if (!isStreamingProgress(ev)) refresh();
            };
            source.onerror = () => {
                reconnecting = true;
                setLive("reconnecting");
                source?.close();
                retry = window.setTimeout(connect, 3000);
            };
        };
        connect();
        return () => { window.clearInterval(floor); source?.close(); if (retry) window.clearTimeout(retry); if (pending.current) window.clearTimeout(pending.current); };
    }, [load, refresh]);

    const value = useMemo(() => ({ snap, live, events, refresh, hubUpdated }), [snap, live, events, refresh, hubUpdated]);
    return <FleetContext.Provider value={value}><ConsoleEventsContext.Provider value={consoleEvents}>{children}</ConsoleEventsContext.Provider></FleetContext.Provider>;
}

export function useFleet(): FleetState {
    const ctx = useContext(FleetContext);
    if (!ctx) throw new Error("useFleet outside FleetProvider");
    return ctx;
}

export function useConsoleEvents(): Event[] {
    const events = useContext(ConsoleEventsContext);
    if (!events) throw new Error("useConsoleEvents outside FleetProvider");
    return events;
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
