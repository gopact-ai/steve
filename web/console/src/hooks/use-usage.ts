import { useCallback, useEffect, useRef, useState } from "react";
import { useResourceRead } from "./use-resource-read";
import { fetchUsage } from "@/lib/api/usage";
import { useConsoleEvents, useFleet, useFleetEvents } from "@/lib/fleet";
import { message } from "@/lib/http";
import type { Event, UsageResponse } from "@/lib/types";

// Usage depends on durable execution facts and task ownership/metadata, not
// fleet polling timestamps, node connectivity or streaming token fragments.
function invalidatesUsage(event: Event): boolean {
    return event.kind.startsWith("task.") || event.kind.startsWith("attempt.") ||
        event.kind.startsWith("plan.") || event.kind.startsWith("run.") ||
        (event.kind.startsWith("node.") && !!(event.task_id || event.plan_id)) ||
        event.kind === "console.reply" || event.kind === "observe.task.idle" ||
        (!!event.step?.state && event.step.state !== "running");
}

export function useUsage(enabled = true) {
    const live = useFleet((fleet) => fleet.live);
    const events = useFleetEvents();
    const [response, setResponse] = useState<UsageResponse | null>(null);
    const [error, setError] = useState<string | null>(null);
    const [loading, setLoading] = useState(enabled);
    const pending = useRef<ReturnType<typeof setTimeout> | null>(null);
    const newest = useRef(events[0]);
    const reconnect = useRef(false);
    const load = useResourceRead(enabled ? "usage" : "usage:disabled", (signal) => {
        setLoading(true);
        return fetchUsage(signal);
    }, (value) => { setResponse(value); setError(null); setLoading(false); },
    (cause) => { setError(message(cause)); setLoading(false); });

    const refresh = useCallback(() => {
        if (enabled) void load();
    }, [enabled, load]);
    const invalidate = useCallback(() => {
        if (!enabled || pending.current !== null) return;
        pending.current = setTimeout(() => { pending.current = null; void load(); }, 250);
    }, [enabled, load]);

    useEffect(() => {
        if (enabled) refresh();
        return () => { if (pending.current !== null) clearTimeout(pending.current); pending.current = null; };
    }, [enabled, refresh]);
    useEffect(() => {
        const previous = newest.current;
        newest.current = events[0];
        const index = previous ? events.indexOf(previous) : events.length;
        // A truncated buffer may have lost a relevant event; read once rather
        // than assume that the remaining unrelated notices are complete.
        if (index < 0 || events.slice(0, index).some(invalidatesUsage)) invalidate();
    }, [events, invalidate]);
    useConsoleEvents((batch) => { if (batch.some(invalidatesUsage)) invalidate(); }, { replay: false });
    useEffect(() => {
        if (live === "reconnecting" || live === "unauthorized") reconnect.current = true;
        if (live === "live" && reconnect.current) { reconnect.current = false; invalidate(); }
    }, [live, invalidate]);
    useEffect(() => {
        const visible = () => { if (!document.hidden) invalidate(); };
        window.addEventListener("focus", visible);
        document.addEventListener("visibilitychange", visible);
        return () => {
            window.removeEventListener("focus", visible);
            document.removeEventListener("visibilitychange", visible);
        };
    }, [invalidate]);
    return { response, error, loading, refresh };
}
