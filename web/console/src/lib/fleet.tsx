import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { emptySnapshot, eventsURL, fetchState } from "./api";
import type { Event, Snapshot } from "./types";

export type Live = "connecting" | "live" | "reconnecting" | "unauthorized";

interface FleetState {
    snap: Snapshot;
    live: Live;
    events: Event[];
    consoleEvents: Event[];
    refresh: () => void;
}

const FleetContext = createContext<FleetState | null>(null);

// The snapshot is re-read when the stream says something moved, with a
// polling floor in case the stream drops silently.
export function FleetProvider({ children }: { children: ReactNode }) {
    const [snap, setSnap] = useState<Snapshot>(emptySnapshot);
    const [live, setLive] = useState<Live>("connecting");
    const [events, setEvents] = useState<Event[]>([]);
    const [consoleEvents, setConsoleEvents] = useState<Event[]>([]);
    const pending = useRef<number | null>(null);

    const load = useCallback(async () => {
        try {
            setSnap(await fetchState());
            setLive((s) => (s === "unauthorized" ? "connecting" : s));
        } catch (e) {
            if (/401|unauthorized/i.test(String(e))) setLive("unauthorized");
        }
    }, []);

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
        const connect = () => {
            source = new EventSource(eventsURL());
            source.onopen = () => setLive("live");
            source.onmessage = (e) => {
                const ev = JSON.parse(e.data) as Event;
                // The console follows its own traffic and the progress of
                // work asked from it; everything else is the activity feed.
                if (ev.kind.startsWith("console.") || ev.kind === "step.progress" || ev.kind === "delegate.progress") setConsoleEvents((list) => [...list.slice(-399), { ...ev, n: ++arrived }]);
                else setEvents((list) => [ev, ...list].slice(0, 300));
                refresh();
            };
            source.onerror = () => {
                setLive("reconnecting");
                source?.close();
                retry = window.setTimeout(connect, 3000);
            };
        };
        connect();
        return () => { window.clearInterval(floor); source?.close(); if (retry) window.clearTimeout(retry); };
    }, [load, refresh]);

    const value = useMemo(() => ({ snap, live, events, consoleEvents, refresh }), [snap, live, events, consoleEvents, refresh]);
    return <FleetContext.Provider value={value}>{children}</FleetContext.Provider>;
}

export function useFleet(): FleetState {
    const ctx = useContext(FleetContext);
    if (!ctx) throw new Error("useFleet outside FleetProvider");
    return ctx;
}

// Verbs sent from other pages land on the console: this is the hand-off.
interface Intent { text: string; mode: "run" | "fill"; n: number }
const IntentContext = createContext<{ intent: Intent | null; act: (t: string) => void; fill: (t: string) => void } | null>(null);

export function IntentProvider({ children, onNavigate }: { children: ReactNode; onNavigate: () => void }) {
    const [intent, setIntent] = useState<Intent | null>(null);
    const act = useCallback((text: string) => { setIntent({ text, mode: "run", n: Date.now() }); onNavigate(); }, [onNavigate]);
    const fill = useCallback((text: string) => { setIntent({ text, mode: "fill", n: Date.now() }); onNavigate(); }, [onNavigate]);
    const value = useMemo(() => ({ intent, act, fill }), [intent, act, fill]);
    return <IntentContext.Provider value={value}>{children}</IntentContext.Provider>;
}

export function useIntent() {
    const ctx = useContext(IntentContext);
    if (!ctx) throw new Error("useIntent outside IntentProvider");
    return ctx;
}
