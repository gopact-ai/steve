import { useEffect, useState } from "react";
import { useSearchParams } from "react-router";
import { message, request } from "@/lib/http";
import type { Page } from "@/lib/assistant-types";

// A page-local, bounded projection. Business consumption remains on the server;
// browser visibility, reloads and SSE reconnects cannot consume an event.
export function useAssistantPage<T>(path: string, every = 15000, cursorKey = "cursor", enabled = true) {
    const [params, setParams] = useSearchParams();
    const cursor = params.get(cursorKey) || "";
    const setCursor = (value: string) => setParams((before) => { const next = new URLSearchParams(before); if (value) next.set(cursorKey, value); else next.delete(cursorKey); return next; });
    const [version, setVersion] = useState(0);
    const [page, setPage] = useState<Page<T> | null>(null);
    const [error, setError] = useState("");
    const [loading, setLoading] = useState(true);
    useEffect(() => {
        if (!enabled) { setPage(null); setError(""); setLoading(false); return; }
        const controller = new AbortController(); let active = true, pending = false;
        setPage(null); setLoading(true);
        const load = async () => {
            if (pending || !active) return;
            pending = true;
            try {
                const join = path.includes("?") ? "&" : "?";
                const value = await request<Page<T>>(`${path}${join}cursor=${encodeURIComponent(cursor)}`, { signal: controller.signal, cache: "no-store" });
                if (!Array.isArray(value.items) || typeof value.next !== "string") throw new Error("Invalid service page response");
                if (active) { setPage(value); setError(""); }
            } catch (cause) { if (active && !controller.signal.aborted) setError(message(cause)); }
            finally { pending = false; if (active) setLoading(false); }
        };
        void load();
        const visible = () => { if (!document.hidden) void load(); };
        const timer = window.setInterval(visible, every);
        document.addEventListener("visibilitychange", visible);
        return () => { active = false; controller.abort(); window.clearInterval(timer); document.removeEventListener("visibilitychange", visible); };
    }, [path, cursor, version, every, enabled]);
    return { page, error, loading, cursor, setCursor, refresh: () => setVersion((value) => value + 1) };
}
