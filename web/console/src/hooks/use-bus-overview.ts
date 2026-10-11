import { useEffect, useState } from "react";
import { useResourceRead } from "@/hooks/use-resource-read";
import { message, request } from "@/lib/http";
import type { ServiceBacklog } from "@/lib/assistant-types";

export function useBusOverview() {
    const [value, setValue] = useState<ServiceBacklog | null>(null);
    const [error, setError] = useState("");
    const [updated, setUpdated] = useState("");
    const refresh = useResourceRead("service-backlog", async (signal) => {
        const stats = await request<ServiceBacklog>("/console/service-backlog", { signal, cache: "no-store" });
        if (!stats.queues || !stats.limits?.events || !stats.storage ||
            ["event_outbox", "event_pending", "event_claimed", "event_accepted", "event_unknown", "event_inbox"].some((key) => !Number.isFinite(stats.queues[key]?.pending)) ||
            !Number.isFinite(stats.limits.events.total) || !Number.isFinite(stats.limits.events.per_source) ||
            !Number.isFinite(stats.storage.hot_bytes) || !Number.isFinite(stats.storage.cold_bytes)) throw new Error("Invalid service backlog response");
        return stats;
    }, (stats) => { setValue(stats); setError(""); setUpdated(new Date().toISOString()); },
    (cause) => { setValue(null); setError(message(cause)); });
    useEffect(() => {
        void refresh();
        const visible = () => { if (!document.hidden) void refresh(); };
        const timer = window.setInterval(visible, 10000);
        document.addEventListener("visibilitychange", visible);
        return () => { window.clearInterval(timer); document.removeEventListener("visibilitychange", visible); };
    }, [refresh]);
    return { value, error, updated, refresh };
}
