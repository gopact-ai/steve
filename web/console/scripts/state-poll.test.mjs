import assert from "node:assert/strict";
import test from "node:test";
import { statePoll, STATE_POLL_DOWN, STATE_POLL_LIVE } from "../src/lib/state-poll.ts";

function fakeClock() {
    let now = 0, id = 0;
    const timers = new Map();
    return {
        set(run, ms) { timers.set(++id, { at: now + ms, run }); return id; },
        clear(timer) { timers.delete(timer); },
        advance(ms) {
            const end = now + ms;
            for (;;) {
                const due = [...timers].filter(([, t]) => t.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
                if (!due) break;
                timers.delete(due[0]);
                now = due[1].at;
                due[1].run();
            }
            now = end;
        },
        pending: () => timers.size,
    };
}

test("a disconnected stream falls back to the short floor", () => {
    const clock = fakeClock(), loads = [];
    const poll = statePoll(() => loads.push("load"), clock);
    clock.advance(STATE_POLL_DOWN * 3);
    assert.equal(loads.length, 3);
    poll.stop();
    assert.equal(clock.pending(), 0);
});

test("a healthy stream stretches the floor and a drop restores it", () => {
    const clock = fakeClock();
    let loads = 0;
    const poll = statePoll(() => loads++, clock);
    poll.live(true);
    clock.advance(STATE_POLL_LIVE - 1);
    assert.equal(loads, 0, "the stream announces changes; /state is not re-read on the short floor");
    clock.advance(1);
    assert.equal(loads, 1, "a silent drop is still bounded by the long floor");
    poll.live(false);
    clock.advance(STATE_POLL_DOWN);
    assert.equal(loads, 2);
});

test("a fresh snapshot restarts the floor", () => {
    const clock = fakeClock();
    let loads = 0;
    const poll = statePoll(() => loads++, clock);
    clock.advance(STATE_POLL_DOWN - 1000);
    poll.fresh();
    clock.advance(STATE_POLL_DOWN - 1);
    assert.equal(loads, 0);
    clock.advance(1);
    assert.equal(loads, 1);
});

test("a hidden page stops polling and reads once when it is shown again", () => {
    const clock = fakeClock();
    let loads = 0;
    const poll = statePoll(() => loads++, clock);
    poll.visible(false);
    clock.advance(STATE_POLL_LIVE * 10);
    assert.equal(loads, 0);
    assert.equal(clock.pending(), 0);
    poll.visible(true);
    assert.equal(loads, 1, "returning to the tab refreshes immediately");
    poll.visible(true);
    assert.equal(loads, 1, "a repeated visible signal is not another read");
    clock.advance(STATE_POLL_DOWN);
    assert.equal(loads, 2, "the floor resumes after the refresh");
});
