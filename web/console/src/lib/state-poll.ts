// statePoll is the floor under the snapshot's event-driven reads. An open
// stream has missed nothing: the hub ends the stream of a reader that falls
// behind rather than skip events, and the reconnect re-reads /state. So
// while the stream is up the long floor only covers what no event announces
// — fields derived from time, such as an activity's freshness window, which
// is longer than the floor — and a connection that died without an error.
// While the stream is down the short floor stands in for it. A hidden page
// stops the floor and reads once when it is shown again; events that arrive
// while it is hidden still re-read /state.
export const STATE_POLL_DOWN = 10_000;
export const STATE_POLL_LIVE = 60_000;

export interface PollTimers {
    set: (run: () => void, ms: number) => number;
    clear: (timer: number) => void;
}

const windowTimers: PollTimers = {
    set: (run, ms) => window.setTimeout(run, ms),
    clear: (timer) => window.clearTimeout(timer),
};

export function statePoll(load: () => void, timers: PollTimers = windowTimers) {
    let live = false;
    let shown = true;
    let stopped = false;
    let timer: number | null = null;
    const arm = () => {
        if (timer !== null) timers.clear(timer);
        if (stopped) { timer = null; return; }
        timer = shown ? timers.set(() => { timer = null; load(); arm(); }, live ? STATE_POLL_LIVE : STATE_POLL_DOWN) : null;
    };
    arm();
    return {
        /** The stream opened (true) or failed (false). */
        live(up: boolean) {
            if (up === live) return;
            live = up;
            arm();
        },
        /** A snapshot just arrived; the floor counts from here. */
        fresh() { if (timer !== null) arm(); },
        visible(next: boolean) {
            if (stopped || next === shown) return;
            shown = next;
            if (shown) load();
            arm();
        },
        stop() {
            if (timer !== null) timers.clear(timer);
            timer = null;
            stopped = true;
        },
    };
}
