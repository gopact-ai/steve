import assert from "node:assert/strict";
import test from "node:test";
import { serviceWatch } from "../src/lib/service-watch.ts";

test("a connection that stays down asks the desktop shell to check the service, at a bounded rate", () => {
    let now = 0;
    const calls = [];
    const watch = serviceWatch({ now: () => now, ensure: () => calls.push(now), after: 10_000, every: 30_000 });
    watch.down();
    now = 3_000; watch.down();
    assert.deepEqual(calls, [], "a short interruption must not start the service");
    now = 10_000; watch.down();
    assert.deepEqual(calls, [10_000]);
    now = 20_000; watch.down();
    assert.deepEqual(calls, [10_000], "checks repeat at most once per interval");
    now = 40_000; watch.down();
    assert.deepEqual(calls, [10_000, 40_000]);
    watch.up();
    now = 45_000; watch.down();
    now = 50_000; watch.down();
    assert.deepEqual(calls, [10_000, 40_000], "a new outage waits its own grace period");
});

test("without the desktop shell nothing is asked", () => {
    const watch = serviceWatch({ now: () => 60_000, ensure: undefined, after: 0, every: 0 });
    watch.down();
    watch.down();
});
