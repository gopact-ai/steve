import assert from "node:assert/strict";

// A full 80-delivery queue drains to 48 under a 32-claim burst.
// This reproduces the old accumulating-clearance failure at both viewport sizes.
export function queueBurstSnapshots(snapshot) {
    const at = Date.now();
    const before = { ...snapshot, capacity: 80, observed_at: new Date(at).toISOString(), events: [], claims: [], queues: { ...snapshot.queues, pending: { ...snapshot.queues.pending, pending: 80 }, claimed: { pending: 0 }, accepted: { pending: 0 }, unknown: { pending: 0 } } };
    const after = { ...before, observed_at: new Date(at + 1000).toISOString(), queues: { ...before.queues, pending: { ...before.queues.pending, pending: 48 } }, claims: Array.from({ length: 32 }, (_, i) => ({ id: `burst-${i}`, claim: `burst-claim-${i}`, at: new Date(at + 1).toISOString(), until: new Date(at + 60000).toISOString() })) };
    return { before, after };
}

// Cross the original 32-column threshold while producers and consumers overlap.
export function queueGrowthSnapshots(snapshot) {
    const at=Date.now();
    const before={...snapshot,capacity:4096,observed_at:new Date(at).toISOString(),events:[],claims:[],queues:{...snapshot.queues,pending:{pending:24}}};
    const after={...before,observed_at:new Date(at+1800).toISOString(),queues:{...before.queues,pending:{pending:32}},events:Array.from({length:12},(_,i)=>({id:'grow-in-'+i,at:new Date(at+1+i*140).toISOString()})),claims:Array.from({length:4},(_,i)=>({id:'grow-out-'+i,claim:'grow-claim-'+i,at:new Date(at+400+i*200).toISOString(),until:new Date(at+60000+i*200).toISOString()}))};
    return {before,after};
}

export function queueGrowthProbe() {
    let started=0,smallest=Infinity,largest=0,maximumSizeError=0,overlap=false;
    const arrivals=new Set(),departures=new Set();
    const sample=(now)=>{
        const stage=document.querySelector('.bus-flow-track');
        if(stage){
            const packets=Array.from(stage.querySelectorAll('.bus-flow-particle'));
            if(packets.length&&!started) started=now;
            if(started){
                const cells=Array.from(stage.querySelectorAll('.bus-flow-cell'));
                smallest=Math.min(smallest,cells.length);largest=Math.max(largest,cells.length);
                const first=cells[0].getBoundingClientRect();
                for(const el of packets){
                    (el.classList.contains('is-in')?arrivals:departures).add(el.dataset.activity);
                    const rect=el.getBoundingClientRect();
                    maximumSizeError=Math.max(maximumSizeError,Math.abs(rect.width-first.width),Math.abs(rect.height-first.height));
                }
                overlap ||= packets.some(el=>el.classList.contains('is-in'))&&packets.some(el=>el.classList.contains('is-out'));
                if(now-started>=5200){
                    document.body.dataset.queueGrowth=JSON.stringify({arrivals:arrivals.size,departures:departures.size,smallest,largest,maximumSizeError,overlap,fill:cells.reduce((sum,cell)=>sum+Number(cell.style.getPropertyValue('--bus-fill')),0),units:Number(stage.dataset.unitScale),packets:packets.length,overflow:document.documentElement.scrollWidth>innerWidth});
                    return;
                }
            }
        }
        requestAnimationFrame(sample);
    };
    requestAnimationFrame(sample);
}

export function assertQueueGrowth(value) {
    assert.equal(value.arrivals,12,"density growth dropped incoming activity");
    assert.equal(value.departures,4,"density growth dropped outgoing activity");
    assert.equal(value.overlap,true,"intake and consumption did not run concurrently");
    assert.ok(value.largest>32,"the grid stopped growing at its old display ceiling");
    assert.ok(value.maximumSizeError<.1,"density growth broke equal packet/cell dimensions");
    assert.equal(value.units,1,"density growth unnecessarily grouped ordinary deliveries");
    assert.equal(value.fill,32,"density growth lost or duplicated waiting deliveries");
    assert.equal(value.packets,0,"density growth left incomplete packets");
    assert.equal(value.overflow,false,"density growth overflowed the page");
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
                    document.body.dataset.queueBurst = JSON.stringify({ minimumVisible, minimumExit, maximumSizeError, departures: departures.size, slots: cells.length, units: Number(stage.dataset.unitScale), settled: cells.filter((cell) => cell.dataset.filled === "true").length, fill: cells.reduce((sum,cell)=>sum+Number(cell.style.getPropertyValue('--bus-fill')),0), packets: stage.querySelectorAll(".bus-flow-particle").length, queueRatio: queue.width / track.width, pending: Number(document.querySelector(".bus-flow-values strong").textContent.replace(/[^0-9]/g,"")), overflow: document.documentElement.scrollWidth > innerWidth });
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
    assert.ok(value.minimumVisible >= 46, "the remaining pile slid out of the queue during rapid consumption");
    assert.ok(value.minimumExit >= .8, "consumer packets originated in the middle of the pile");
    assert.ok(value.maximumSizeError < .1, "outer packets and queue cells have different dimensions");
    assert.ok(value.queueRatio >= .85, "the durable queue is shorter than the requested 85%");
    assert.equal(value.units, 1, "an ordinary backlog should retain one delivery per light");
    assert.equal(value.settled, 48, "48 waiting deliveries should leave 48 lights");
    assert.ok(Math.abs(value.fill - 48) < .002, "the pile lost waiting deliveries");
    assert.ok(value.slots > 80, "the display did not grow beyond the original 32 slots");
    assert.equal(value.packets, 0, "completed claims left detached packets behind");
    assert.equal(value.pending, 48, "cosmetic replay changed the observed backlog");
    assert.equal(value.overflow, false, "the neon queue overflowed the viewport");
}

export async function checkQueueBurst(page, snapshot) {
    await page.unroute("**/console/event-flow");
    await page.addInitScript(queueBurstProbe);
    await page.addInitScript(queueGrowthProbe);
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
        const growth=queueGrowthSnapshots(snapshot);
        let growing=false;
        await page.route("**/console/event-flow",route=>route.fulfill({json:growing?growth.after:growth.before}));
        await page.reload();
        await page.waitForFunction(()=>document.querySelector('.bus-flow-values strong')?.textContent==='24');
        growing=true;
        await page.getByRole("button",{name:"Refresh",exact:true}).first().click();
        await page.waitForFunction(()=>document.body.dataset.queueGrowth,undefined,{timeout:15000});
        assertQueueGrowth(await page.evaluate(()=>JSON.parse(document.body.dataset.queueGrowth)));
        await page.unroute("**/console/event-flow");
        // Actual counts remain visible as the grid grows; server capacity must
        // not collapse 40 deliveries into a single aggregate light.
        let previousSlots = 0;
        for (const pending of [0,1,40,80,128,2048,4096]) {
            const value = { ...values.before, capacity:4096, queues: { ...values.before.queues, pending: { pending } } };
            await page.route("**/console/event-flow", (route) => route.fulfill({ json: value }));
            await page.reload();
            await page.waitForFunction((pending) => {
                const stage=document.querySelector('.bus-flow-track');
                const cells=Array.from(document.querySelectorAll('.bus-flow-cell'));
                return stage && Math.abs(cells.reduce((sum,cell)=>sum+Number(cell.style.getPropertyValue('--bus-fill')),0) * Number(stage.dataset.unitScale) - pending) < .001;
            }, pending);
            const view = await page.evaluate(() => {
                const stage=document.querySelector('.bus-flow-track'), cells=Array.from(stage.querySelectorAll('.bus-flow-cell'));
                const queue=stage.querySelector('.bus-flow-buffer').getBoundingClientRect();
                return {slots:cells.length, units:Number(stage.dataset.unitScale), lit:cells.filter(cell=>cell.dataset.filled==='true').length, width:cells[0].getBoundingClientRect().width, fill:cells.reduce((sum,cell)=>sum+Number(cell.style.getPropertyValue('--bus-fill')),0), marker:document.querySelector('.bus-flow-scale')?.textContent, queueRatio:queue.width/stage.getBoundingClientRect().width, overflow:document.documentElement.scrollWidth>innerWidth};
            });
            assert.ok(view.slots>=previousSlots, "more backlog produced fewer display columns");
            previousSlots=view.slots;
            if(pending<=128) {
                assert.equal(view.units,1,"normal backlog was prematurely aggregated");
                assert.equal(view.lit,pending,"one delivery did not produce one resting light");
                assert.equal(view.marker,undefined,"normal backlog retained an explanatory scale legend");
            } else assert.equal(view.marker,'×'+view.units,"unavoidable grouping was undisclosed");
            assert.ok(view.fill<view.slots,"the adaptive display has no space for another arrival");
            assert.ok(view.width>=1,"adaptive lights shrank below a discernible pixel");
            assert.ok(view.queueRatio>=.85,"adaptive columns shortened the queue");
            assert.equal(view.overflow,false,"adaptive columns overflowed the viewport");
            await page.unroute("**/console/event-flow");
        }
    }
}
