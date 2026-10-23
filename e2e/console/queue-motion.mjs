import assert from "node:assert/strict";

// A full 80-delivery queue drains to 48 under a 32-claim burst.
// This reproduces the old accumulating-clearance failure at both viewport sizes.
export function queueBurstSnapshots(snapshot, initial = 80, departures = 32) {
    const at = Date.now();
    const before = { ...snapshot, capacity: Math.max(80,initial), observed_at: new Date(at).toISOString(), events: [], claims: [], queues: { ...snapshot.queues, pending: { ...snapshot.queues.pending, pending: initial }, claimed: { pending: 0 }, accepted: { pending: 0 }, unknown: { pending: 0 } } };
    const after = { ...before, observed_at: new Date(at + 1000).toISOString(), queues: { ...before.queues, pending: { ...before.queues.pending, pending: initial - departures } }, claims: Array.from({ length: departures }, (_, i) => ({ id: `burst-${i}`, claim: `burst-claim-${i}`, at: new Date(at + 1).toISOString(), until: new Date(at + 60000).toISOString() })) };
    return { before, after };
}

// Cross the original 32-column threshold while producers and consumers overlap.
export function queueGrowthSnapshots(snapshot) {
    const at=Date.now();
    const before={...snapshot,capacity:4096,observed_at:new Date(at).toISOString(),events:[],claims:[],queues:{...snapshot.queues,pending:{pending:24}}};
    const after={...before,observed_at:new Date(at+1800).toISOString(),queues:{...before.queues,pending:{pending:32}},events:Array.from({length:12},(_,i)=>({id:'grow-in-'+i,at:new Date(at+1+i*140).toISOString()})),claims:Array.from({length:4},(_,i)=>({id:'grow-out-'+i,claim:'grow-claim-'+i,at:new Date(at+400+i*200).toISOString(),until:new Date(at+60000+i*200).toISOString()}))};
    return {before,after};
}

// Browser-only probes read the renderer's drawn state, not its internals.
export function queueGrowthProbe() {
    let started=0,smallest=Infinity,largest=0,maximumSizeError=0,overlap=false;
    const arrivals=new Set(),departures=new Set();
    const sample=(now)=>{
        const stage=document.querySelector('.bus-flow-track');
        const view=stage?.flowState?.();
        if(view){
            if(view.packets.length&&!started) started=now;
            if(started){
                smallest=Math.min(smallest,view.slots);largest=Math.max(largest,view.slots);
                for(const item of view.packets){
                    (item.direction==='in'?arrivals:departures).add(item.id);
                    maximumSizeError=Math.max(maximumSizeError,Math.abs(item.width-view.width),Math.abs(item.height-view.height));
                }
                overlap ||= view.packets.some(item=>item.direction==='in')&&view.packets.some(item=>item.direction==='out');
                if(now-started>=5200){
                    document.body.dataset.queueGrowth=JSON.stringify({arrivals:arrivals.size,departures:departures.size,smallest,largest,maximumSizeError,overlap,fill:view.fill,units:view.units,packets:view.packets.length,overflow:document.documentElement.scrollWidth>innerWidth});
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

export function queueBurstProbe() {
    let started = 0, minimumVisible = Infinity, minimumExit = Infinity, maximumSizeError = 0, minimumGap = Infinity;
    const departures = new Set();
    const sample = (now) => {
        const stage = document.querySelector(".bus-flow-track");
        const view = stage?.flowState?.();
        if (view) {
            const outgoing = view.packets.filter((item) => item.direction === "out");
            if (outgoing.length && !started) started = now;
            if (started) {
                minimumVisible = Math.min(minimumVisible, view.lit.length);
                for (const item of outgoing) {
                    departures.add(item.id);
                    minimumExit = Math.min(minimumExit, item.left / view.length);
                    maximumSizeError = Math.max(maximumSizeError, Math.abs(item.width - view.width), Math.abs(item.height - view.height));
                }
                const visible = outgoing.filter((item) => item.opacity >= .2 && item.left < view.length).sort((a, b) => a.left - b.left);
                for (let i = 1; i < visible.length; i++) minimumGap = Math.min(minimumGap, visible[i].left - visible[i - 1].left - visible[i - 1].width);
                if (now - started >= 5200 && view.packets.length === 0 && Number(stage.querySelector("[data-flow-taken]").dataset.flowTaken) === 0) {
                    document.body.dataset.queueBurst = JSON.stringify({ minimumVisible, minimumExit, maximumSizeError, minimumGap, departures: departures.size, slots: view.slots, units: view.units, settled: view.lit.length, fill: view.fill, packets: view.packets.length, queueRatio: view.span / view.length, pending: Number(document.querySelector(".bus-flow-values strong").textContent.replace(/[^0-9]/g,"")), overflow: document.documentElement.scrollWidth > innerWidth });
                    return;
                }
            }
        }
        requestAnimationFrame(sample);
    };
    requestAnimationFrame(sample);
}

export function assertQueueBurst(value, expected = { initial:80, pending:48, departures:32 }) {
    assert.equal(value.departures, expected.departures, "the burst did not exercise every departure");
    assert.ok(value.minimumGap >= 1, "outgoing light cores overlapped during continuous consumption");
    assert.ok(value.minimumVisible >= expected.pending - 2, "the remaining pile slid out of the queue during rapid consumption");
    assert.ok(value.minimumExit >= .8, "consumer packets originated in the middle of the pile");
    assert.ok(value.maximumSizeError < .1, "outer packets and queue cells have different dimensions");
    assert.ok(value.queueRatio >= .85, "the durable queue is shorter than the requested 85%");
    assert.equal(value.units, 1, "an ordinary backlog should retain one delivery per light");
    assert.equal(value.settled, expected.pending, "waiting deliveries did not retain one light each");
    assert.ok(Math.abs(value.fill - expected.pending) < .002, "the pile lost waiting deliveries");
    assert.ok(value.slots > expected.initial, "the display has no headroom for the initial backlog");
    assert.equal(value.packets, 0, "completed claims left detached packets behind");
    assert.equal(value.pending, expected.pending, "cosmetic replay changed the observed backlog");
    assert.equal(value.overflow, false, "the neon queue overflowed the viewport");
}

// Each case owns a fresh page from open(width), so independent cases run on
// a bounded pool of concurrent contexts; the probes keep their real-time
// animation windows. The adaptive density walk stays ordered within a width.
export async function checkQueueBurst(open, target, snapshot, concurrency = 2) {
    const cases = [];
    for (const width of [1280, 390]) {
        const fresh = async (flow) => {
            const page = await open(width);
            await page.addInitScript(queueBurstProbe);
            await page.addInitScript(queueGrowthProbe);
            await page.route("**/console/event-flow", flow);
            await page.goto(target);
            return page;
        };
        for(const [initial,departures] of [[80,32],[24,16]]) cases.push(async () => {
            const values=queueBurstSnapshots(snapshot,initial,departures);
            let burst=false;
            const page = await fresh(route=>route.fulfill({json:burst?values.after:values.before}));
            await page.waitForFunction(initial=>document.querySelector('.bus-flow-values strong')?.textContent===String(initial),initial);
            burst=true;
            await page.getByRole("button",{name:"Refresh",exact:true}).first().click();
            await page.waitForFunction(()=>document.body.dataset.queueBurst,undefined,{timeout:15000});
            assertQueueBurst(await page.evaluate(()=>JSON.parse(document.body.dataset.queueBurst)),{initial,pending:initial-departures,departures});
            await page.context().close();
        });
        cases.push(async () => {
            const growth=queueGrowthSnapshots(snapshot);
            let growing=false;
            const page = await fresh(route=>route.fulfill({json:growing?growth.after:growth.before}));
            await page.waitForFunction(()=>document.querySelector('.bus-flow-values strong')?.textContent==='24');
            growing=true;
            await page.getByRole("button",{name:"Refresh",exact:true}).first().click();
            await page.waitForFunction(()=>document.body.dataset.queueGrowth,undefined,{timeout:15000});
            assertQueueGrowth(await page.evaluate(()=>JSON.parse(document.body.dataset.queueGrowth)));
            await page.context().close();
        });
        // Actual counts remain visible as the grid grows; server capacity must
        // not collapse 40 deliveries into a single aggregate light.
        cases.push(async () => {
            const values=queueBurstSnapshots(snapshot);
            let value;
            const page = await fresh((route) => route.fulfill({ json: value }));
            let previousSlots = 0;
            for (const pending of [0,1,40,80,128,2048,4096]) {
                value = { ...values.before, capacity:4096, queues: { ...values.before.queues, pending: { pending } } };
                await page.reload();
                await page.waitForFunction((pending) => {
                    const view=document.querySelector('.bus-flow-track')?.flowState?.();
                    return view && Math.abs(view.fill * view.units - pending) < .001;
                }, pending);
                const view = await page.evaluate(() => {
                    const state=document.querySelector('.bus-flow-track').flowState();
                    return {slots:state.slots, units:state.units, lit:state.lit.length, width:state.width, fill:state.fill, marker:document.querySelector('.bus-flow-scale')?.textContent, queueRatio:state.span/state.length, overflow:document.documentElement.scrollWidth>innerWidth};
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
            }
            await page.context().close();
        });
    }
    await Promise.all(Array.from({ length: Math.max(1, Math.min(concurrency, cases.length)) }, async () => {
        while (cases.length) await cases.shift()();
    }));
}
