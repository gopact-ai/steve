// statePoll is the floor under the snapshot's event-driven reads. While the
// stream is up it announces every change, so /state is only re-read on a
// long floor that bounds a stream which went quiet without an error. While
// it is down the short floor stands in for it. A hidden page reads nothing
// and catches up once when it is shown again.
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
