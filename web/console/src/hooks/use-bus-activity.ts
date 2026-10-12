import { useEffect, useState } from "react";
import { useResourceRead } from "@/hooks/use-resource-read";
import { message, request } from "@/lib/http";
import type { EventActivitySnapshot } from "@/lib/assistant-types";

export function useBusActivity(enabled = true) {
    const [value, setValue] = useState<EventActivitySnapshot | null>(null);
    const [error, setError] = useState("");
    const refresh = useResourceRead("event-activity", async (signal) => {
        const value = await request<EventActivitySnapshot>("/console/event-activity", { signal, cache: "no-store" });
        if (!Array.isArray(value.recent) || value.recent.length > 32 || !Array.isArray(value.attention) || value.attention.length > 4 || typeof value.more_attention !== "boolean") throw new Error("Invalid event activity response");
        return value;
    }, (value) => { setValue(value); setError(""); }, (cause) => { setValue(null); setError(message(cause)); });
    useEffect(() => {
        if (!enabled) return;
        if (!document.hidden) void refresh();
        const visible = () => { if (!document.hidden) void refresh(); };
        const timer = window.setInterval(visible, 10000);
        document.addEventListener("visibilitychange", visible);
        return () => { window.clearInterval(timer); document.removeEventListener("visibilitychange", visible); };
    }, [refresh, enabled]);
    return { value, error, refresh };
}
