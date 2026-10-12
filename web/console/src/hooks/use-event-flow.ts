import { useCallback, useEffect, useRef, useState } from "react";
import { useResourceRead } from "@/hooks/use-resource-read";
import { message, request } from "@/lib/http";
import type { EventFlowSnapshot } from "@/lib/assistant-types";

export interface FlowParticle { id: string; direction: "in" | "out"; delay: number }
export function useEventFlow(motion: boolean) {
    const [snapshot, setSnapshot] = useState<EventFlowSnapshot | null>(null);
    const [error, setError] = useState("");
    const [particles, setParticles] = useState<FlowParticle[]>([]);
    const baseline = useRef<EventFlowSnapshot | null>(null);
    const enabled = useRef(motion);
    useEffect(() => { enabled.current = motion; if (!motion) setParticles([]); }, [motion]);
    const refresh = useResourceRead("event-flow", async (signal) => {
        const value = await request<EventFlowSnapshot>("/console/event-flow", { signal, cache: "no-store" });
        if (!Number.isFinite(Date.parse(value.observed_at)) || !Number.isSafeInteger(value.capacity) || value.capacity <= 0 || !Array.isArray(value.events) || !Array.isArray(value.claims) || value.events.length > 64 || value.claims.length > 64 ||
            ["routing", "pending", "claimed", "accepted", "unknown"].some((key) => !Number.isInteger(value.queues?.[key as keyof typeof value.queues]?.pending) || value.queues[key as keyof typeof value.queues].pending < 0)) throw new Error("Invalid event flow response");
        return value;
    }, (value) => {
        const before = baseline.current;
        baseline.current = value;
        setSnapshot(value); setError("");
        if (before && before.capacity !== value.capacity) { setParticles([]); return; }
        if (before && enabled.current && !document.hidden) {
            const events = new Set(before.events.map((item) => item.id));
            const claims = new Set(before.claims.map((item) => item.claim));
            // Older history re-entering a bounded window is not new activity.
            const eventTime = Math.max(0, ...before.events.map((item) => Date.parse(item.at)));
            const claimTime = Math.max(0, ...before.claims.map((item) => Date.parse(item.until)));
            const delay = (at: string) => Math.max(0, Math.min(1800, Date.parse(at) - Date.parse(before.observed_at)));
            const added: FlowParticle[] = [
                ...value.events.filter((item) => !events.has(item.id) && Date.parse(item.at) >= eventTime).map((item) => ({ id: `event:${item.id}`, direction: "in" as const, delay: delay(item.at) })),
                ...value.claims.filter((item) => !claims.has(item.claim) && Date.parse(item.until) >= claimTime).map((item) => ({ id: `claim:${item.claim}`, direction: "out" as const, delay: delay(item.at) })),
            ];
            if (added.length) setParticles((old) => [...old, ...added].slice(-192));
        }
    }, (cause) => {
        baseline.current = null; setSnapshot(null); setError(message(cause));
        setParticles([]);
    });
    useEffect(() => {
        const visible = () => {
            baseline.current = null;
            setParticles([]);
            if (!document.hidden) void refresh();
        };
        if (!document.hidden) void refresh();
        const timer = window.setInterval(() => { if (!document.hidden) void refresh(); }, 2000);
        document.addEventListener("visibilitychange", visible);
        return () => { window.clearInterval(timer); document.removeEventListener("visibilitychange", visible); };
    }, [refresh]);
    const finish = useCallback((id: string) => setParticles((old) => old.filter((item) => item.id !== id)), []);
    return { snapshot, error, particles, finish, refresh };
}
