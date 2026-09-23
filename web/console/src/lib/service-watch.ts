// serviceWatch decides when the page asks the desktop shell to make sure the
// local service is running: only once the live connection has stayed down
// for a grace period, and then at a bounded rate. The shell starts a service
// that has exited; one that is merely restarting is left alone.
export interface ServiceWatchOptions { now: () => number; ensure: (() => void) | undefined; after: number; every: number }

export function serviceWatch({ now, ensure, after, every }: ServiceWatchOptions) {
    let downSince: number | null = null;
    let lastCheck: number | null = null;
    return {
        down() {
            const at = now();
            downSince ??= at;
            if (!ensure || at - downSince < after || (lastCheck !== null && at - lastCheck < every)) return;
            lastCheck = at;
            ensure();
        },
        up() { downSince = null; lastCheck = null; },
    };
}
