import { useCallback, useLayoutEffect, useRef } from "react";
import { createResourceRead } from "@/lib/resource-read";

// The key is the server resource identity, not a polling revision. Callbacks
// may change with a render; a changed key invalidates the previous generation.
export function useResourceRead<T>(key: string, read: (signal: AbortSignal) => Promise<T>, receive: (value: T) => void, fail: (error: unknown) => void) {
    const latest = useRef({ read, receive, fail });
    useLayoutEffect(() => { latest.current = { read, receive, fail }; });
    const resource = useRef<ReturnType<typeof createResourceRead<T>> | null>(null);
    useLayoutEffect(() => {
        const current = createResourceRead<T>((signal) => latest.current.read(signal), (value) => latest.current.receive(value), (error) => latest.current.fail(error));
        resource.current = current;
        return () => { current.dispose(); if (resource.current === current) resource.current = null; };
    }, [key]);
    return useCallback(() => resource.current?.refresh() ?? Promise.resolve(), []);
}
