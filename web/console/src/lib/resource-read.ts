// Reads of one resource are serialized. Refreshes during a read collapse into
// one follow-up; disposing the resource invalidates every outstanding result.
export function createResourceRead<T>(read: (signal: AbortSignal) => Promise<T>, receive: (value: T) => void, fail: (error: unknown) => void) {
    let active = true;
    let pending: Promise<void> | null = null;
    let queued: { promise: Promise<void>; resolve: () => void } | null = null;
    let controller: AbortController | null = null;
    function refresh(): Promise<void> {
        if (!active) return Promise.resolve();
        if (pending) {
            if (!queued) {
                let resolve!: () => void;
                const promise = new Promise<void>((done) => { resolve = done; });
                queued = { promise, resolve };
            }
            return queued.promise;
        }
        const current = new AbortController();
        controller = current;
        pending = (async () => {
            try {
                const value = await read(current.signal);
                if (active) receive(value);
            } catch (error) {
                if (active) fail(error);
            }
        })().finally(() => {
            const next = queued;
            queued = null;
            pending = null;
            if (next) {
                if (active) void refresh().then(next.resolve);
                else next.resolve();
            }
        });
        return pending;
    }
    return { refresh, dispose() { active = false; controller?.abort(); queued?.resolve(); queued = null; } };
}
