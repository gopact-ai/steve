import { useCallback, useEffect, useRef, useState } from "react";
import { HTTPError, message } from "@/lib/http";
import type { WorkPage } from "@/lib/types";

// A user-opened history is independent of /state polling. Scope changes abort
// requests; 409 never falls back to an old or first-page cursor implicitly.
export function useWorkPage<T>(key: string, read: (cursor: string, signal: AbortSignal) => Promise<WorkPage<T>>, enabled = true, initial?: WorkPage<T>) {
    const reader = useRef(read);
    reader.current = read;
    const identity = `${enabled}:${key}`;
    const current = useRef(identity);
    current.current = identity;
    const flight = useRef<AbortController | null>(null);
    const [state, setState] = useState({ identity, page: initial, loading: !initial && enabled, error: "", stale: false });
    const stateRef = useRef(state);
    stateRef.current = state;
    const load = useCallback(async (reset = false) => {
        if (!enabled || flight.current) return;
        const previous = stateRef.current.identity === identity ? stateRef.current.page : undefined;
        const cursor = reset ? "" : previous?.next_cursor || "";
        const publish = (next: typeof state) => { stateRef.current = next; setState(next); };
        const controller = new AbortController();
        flight.current = controller;
        publish({ identity, page: reset ? undefined : previous, loading: true, error: "", stale: false });
        try {
            const next = await reader.current(cursor, controller.signal);
            if (controller.signal.aborted || current.current !== identity) return;
            publish({ identity, page: { ...next, items: cursor ? [...(previous?.items || []), ...next.items] : next.items }, loading: false, error: "", stale: false });
        } catch (error) {
            if (!controller.signal.aborted && current.current === identity) publish({ identity, page: reset ? undefined : previous, loading: false, error: message(error), stale: error instanceof HTTPError && error.status === 409 });
        } finally { if (flight.current === controller) flight.current = null; }
    }, [enabled, identity]);
    useEffect(() => {
        setState({ identity, page: initial, loading: !initial && enabled, error: "", stale: false });
        stateRef.current = { identity, page: initial, loading: false, error: "", stale: false };
        if (enabled && !initial) void load(true);
        return () => { flight.current?.abort(); flight.current = null; };
        // initial belongs to the selected detail; callers remount on refresh.
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [identity, load]);
    const visible = state.identity === identity ? state : { page: undefined, loading: enabled, error: "", stale: false };
    return { items: visible.page?.items || [], total: visible.page?.total, hasMore: !!visible.page?.next_cursor, loading: visible.loading, error: visible.error, stale: visible.stale,
        more: () => { if (visible.page?.next_cursor && !visible.stale) void load(); },
        retry: () => { if (!visible.stale) void load(); }, refresh: () => void load(true) };
}
