import assert from "node:assert/strict";

// A full 80-delivery queue drains to 48 under a 32-claim burst.
// This reproduces the old accumulating-clearance failure at both viewport sizes.
export function queueBurstSnapshots(snapshot) {
    const at = Date.now();
    const before = { ...snapshot, capacity: 80, observed_at: new Date(at).toISOString(), events: [], claims: [], queues: { ...snapshot.queues, pending: { ...snapshot.queues.pending, pending: 80 } } };
    const after = { ...before, observed_at: new Date(at + 1000).toISOString(), queues: { ...before.queues, pending: { ...before.queues.pending, pending: 48 } }, claims: Array.from({ length: 32 }, (_, i) => ({ id: `burst-${i}`, claim: `burst-claim-${i}`, at: new Date(at + 1).toISOString(), until: new Date(at + 60000).toISOString() })) };
    return { before, after };
}

// Browser-only acceptance probe: observe visible lights, not renderer internals.
export function queueBurstProbe() {
    let started = 0, minimumVisible = Infinity, minimumExit = Infinity, maximumSizeError = 0;
    const departures = new Set();
    const sample = (now) => {
        const stage = document.querySelector(".bus-flow-track");
        if (stage) {
            const outgoing = Array.from(stage.querySelectorAll(".bus-flow-particle.is-out"));
            if (outgoing.length && !started) started = now;
            if (started) {
                const track = stage.getBoundingClientRect();
                const queue = stage.querySelector(".bus-flow-buffer").getBoundingClientRect();
                const cells = Array.from(stage.querySelectorAll(".bus-flow-cell"));
                const first = cells[0].getBoundingClientRect();
                let visible = 0;
                for (const cell of cells) {
                    const style = getComputedStyle(cell, "::after"), rect = cell.getBoundingClientRect();
                    const shift = style.transform === "none" ? 0 : new DOMMatrixReadOnly(style.transform).m41;
                    if (Number(style.opacity) >= .99 && rect.right + shift > queue.left + .5 && rect.left + shift < queue.right - .5) visible++;
                }
                minimumVisible = Math.min(minimumVisible, visible);
                for (const el of outgoing) {
                    departures.add(el);
                    const rect = el.getBoundingClientRect();
                    minimumExit = Math.min(minimumExit, (rect.left - track.left) / track.width);
                    maximumSizeError = Math.max(maximumSizeError, Math.abs(rect.width - first.width), Math.abs(rect.height - first.height));
                }
                if (now - started >= 5200) {
                    document.body.dataset.queueBurst = JSON.stringify({ minimumVisible, minimumExit, maximumSizeError, departures: departures.size, settled: cells.filter((cell) => cell.dataset.filled === "true").length, fill: cells.reduce((sum,cell)=>sum+Number(cell.style.getPropertyValue('--bus-fill')),0), packets: stage.querySelectorAll(".bus-flow-particle").length, queueRatio: queue.width / track.width, pending: Number(document.querySelector(".bus-flow-values strong").textContent), overflow: document.documentElement.scrollWidth > innerWidth });
                    return;
                }
            }
        }
        requestAnimationFrame(sample);
    };
    requestAnimationFrame(sample);
}

export function assertQueueBurst(value) {
    assert.equal(value.departures, 32, "the burst did not exercise all 32 departures");
    assert.ok(value.minimumVisible >= 16, "the remaining pile slid out of the queue during rapid consumption");
    assert.ok(value.minimumExit >= .8, "consumer packets originated in the middle of the pile");
    assert.ok(value.maximumSizeError < .1, "outer packets and queue cells have different dimensions");
    assert.ok(value.queueRatio >= .85, "the durable queue is shorter than the requested 85%");
    assert.equal(value.settled, 20, "48 of 80 deliveries should occupy 19 full segments and one partial segment");
    assert.ok(Math.abs(value.fill - 19.2) < .002, "the lit queue length is not the observed 60% occupancy");
    assert.equal(value.packets, 0, "completed claims left detached packets behind");
    assert.equal(value.pending, 48, "cosmetic replay changed the observed backlog");
    assert.equal(value.overflow, false, "the neon queue overflowed the viewport");
}

export async function checkQueueBurst(page, snapshot) {
    await page.unroute("**/console/event-flow");
    await page.addInitScript(queueBurstProbe);
    for (const width of [1280, 390]) {
        const values = queueBurstSnapshots(snapshot);
        let burst = false;
        await page.route("**/console/event-flow", (route) => route.fulfill({ json: burst ? values.after : values.before }));
        await page.setViewportSize({ width, height: 900 });
        await page.reload();
        await page.waitForFunction(() => document.querySelector(".bus-flow-values strong")?.textContent === "80");
        burst = true;
        await page.getByRole("button", { name: "Refresh", exact: true }).first().click();
        await page.waitForFunction(() => document.body.dataset.queueBurst, undefined, { timeout: 15000 });
        assertQueueBurst(await page.evaluate(() => JSON.parse(document.body.dataset.queueBurst)));
        await page.unroute("**/console/event-flow");
        // A few dozen entries must not look full under the real default limit.
        for (const [pending, capacity] of [[40,4096],[128,4096],[2048,4096],[4096,4096],[1,1]]) {
            const value = { ...values.before, capacity, queues: { ...values.before.queues, pending: { pending } } };
            await page.route("**/console/event-flow", (route) => route.fulfill({ json: value }));
            await page.reload();
            await page.waitForFunction(({pending,capacity}) => {
                const cells = Array.from(document.querySelectorAll('.bus-flow-cell'));
                return cells.length === Math.min(32,capacity) && Math.abs(cells.reduce((sum,cell)=>sum+Number(cell.style.getPropertyValue('--bus-fill')),0) / cells.length - pending/capacity) < .00001;
            }, {pending,capacity});
            const fill = await page.evaluate(() => Array.from(document.querySelectorAll('.bus-flow-cell')).map((cell)=>({fill:Number(cell.style.getPropertyValue('--bus-fill')),size:cell.getBoundingClientRect().width,lit:getComputedStyle(cell,'::after').width})));
            const partial = fill.find((cell)=>cell.fill>0&&cell.fill<1);
            if(partial) assert.ok(Math.abs(Number.parseFloat(partial.lit)/partial.size-partial.fill)<.02, "a partial segment lit its full width");
            await page.unroute("**/console/event-flow");
        }
    }
}
