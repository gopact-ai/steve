// Real hook/React lifecycle, fake clock and in-memory HTTP. No hub, app UI or writes.
import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";

const entry = `
import React, { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { useChannelConversation } from "/src/hooks/use-channel-conversation.ts";
function Owner({ id }) {
    const state = useChannelConversation(id);
    useEffect(() => { window.historyState = state; window.earlier = state.loadEarlier; });
    return React.createElement("pre", null, JSON.stringify(state));
}
function App() {
    const [id, setID] = useState("a");
    window.switchHistory = setID;
    return React.createElement(Owner, { id, key: id });
}
createRoot(document.getElementById("root")).render(React.createElement(React.StrictMode, null, React.createElement(App)));
`;
const cacheDir = await mkdtemp(path.join(tmpdir(), "channel-history-hook-"));
const server = await createServer({
    configFile: false,
    root: fileURLToPath(new URL("..", import.meta.url)), logLevel: "error",
    cacheDir,
    resolve: { alias: { "@": fileURLToPath(new URL("../src", import.meta.url)) } },
    optimizeDeps: { noDiscovery: true, include: ["react", "react-dom/client"] },
    plugins: [{
        name: "isolated-channel-history-hook",
        configureServer(server) {
            server.middlewares.use((req, res, next) => {
                if (req.url !== "/history-hook.html") return next();
                res.setHeader("Content-Type", "text/html");
                res.end('<div id="root"></div><script type="module" src="/@id/__x00__history-hook"></script>');
            });
        },
        resolveId(id) { if (id === "\0history-hook") return id; },
        load(id) { if (id === "\0history-hook") return entry; },
    }],
    server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const page = await browser.newPage();
page.setDefaultTimeout(5000);
const errors = [];
page.on("pageerror", error => errors.push(String(error)));
await page.route("**/*", route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
await page.clock.install({ time: new Date("2026-09-21T00:00:00Z") });
await page.clock.pauseAt(new Date("2026-09-21T00:00:00Z"));
await page.addInitScript(() => {
    const row = n => ({ id: `t${n}`, exchange_id: `t${n}`, kind: "sent", input: `input ${n}`, text: "", at: "2026-09-21T00:00:00Z" });
    const fixture = window.fixture = {
        turns: Array.from({ length: 151 }, (_, i) => [row(i + 1)]),
        requests: [], failOlder: false, hold: false, held: [], malformed: false, repeatCursor: false,
        add() { this.turns.push([row(this.turns.length + 1)]); },
        answer(n, delivery = "confirmed") { this.turns[n - 1] = [row(n), { ...row(n), id: `t${n}/reply`, kind: "reply", text: `answer ${n}`, delivery }]; },
    };
    const original = window.fetch;
    window.fetch = async (url, init = {}) => {
        const parsed = new URL(url, location.href);
        if (!parsed.pathname.startsWith("/console/channel-conversations/")) return original(url, init);
        if (init.method && init.method !== "GET") throw new Error("Unexpected write");
        const id = decodeURIComponent(parsed.pathname.split("/").pop());
        const cursor = parsed.searchParams.get("cursor");
        const request = { id, cursor, signal: init.signal };
        fixture.requests.push(request);
        const before = cursor ? Number(cursor) : fixture.turns.length + 1;
        const turns = id === "a" ? fixture.turns.slice(Math.max(0, before - 51), before - 1) : [[{ ...row(1), id: "other" }]];
        const data = structuredClone({
            conversation: { id: fixture.malformed && cursor ? "wrong" : id, transport: "feishu", read_only: true, running: false },
            replies: turns.flat().map(reply => ({ ...reply, conversation: id })),
            next_cursor: fixture.repeatCursor && cursor ? cursor : id === "a" && before > 51 ? String(before - 50) : undefined,
        });
        if (cursor && fixture.hold) {
            fixture.hold = false;
            // Deliberately ignore AbortSignal: verify the hook rejects late completions itself.
            await new Promise(resolve => fixture.held.push({ release: resolve, request }));
        }
        return new Response(JSON.stringify(cursor && fixture.failOlder ? { error: "Older read failed" } : data), {
            status: cursor && fixture.failOlder ? 503 : 200, headers: { "Content-Type": "application/json" },
        });
    };
});
const settle = () => page.waitForFunction(() => window.historyState && !window.historyState.refreshing);
const state = () => page.evaluate(() => JSON.parse(JSON.stringify(window.historyState)));
const poll = async () => { await page.clock.runFor(3000); await settle(); };
try {
    await page.goto(`${origin}/history-hook.html`);
    await settle();
    await page.evaluate(() => window.earlier());
    await settle();
    assert.equal((await state()).data.replies[0].id, "t52");
    assert.equal((await state()).data.next_cursor, "52");
    await page.evaluate(() => { fixture.answer(52, "unconfirmed"); fixture.add(); fixture.requests = []; });
    // One pass visits at most one old page per poll, even after the latest window slides.
    await poll(); await poll();
    let got = await state();
    assert.ok(got.data.replies.some(reply => reply.id === "t52/reply"), "late reply outside latest 50 turns must refresh");
    assert.equal(got.data.replies[0].id, "t52", "never import unloaded older turns");
    assert.equal(got.data.next_cursor, "52", "revalidation must not consume the pagination cursor");
    assert.ok((await page.evaluate(() => fixture.requests.length)) <= 4, "at most two reads per poll");
    assert.equal(got.data.replies[1].id, "t52/reply", "late answer stays beside its input");
    await page.evaluate(() => fixture.answer(52));
    await poll(); await poll();
    assert.equal((await state()).data.replies.find(reply => reply.id === "t52/reply").delivery, "confirmed");

    // A failed old-page refresh cannot partially publish new latest data or advance its scan.
    const beforeFailure = (await state()).data;
    await page.evaluate(() => { fixture.failOlder = true; fixture.add(); });
    await poll();
    assert.equal((await state()).error, "Older read failed");
    assert.deepEqual((await state()).data, beforeFailure);
    await page.evaluate(() => { fixture.failOlder = false; fixture.malformed = true; });
    await poll();
    assert.match((await state()).error, /Invalid channel conversation response/);
    assert.deepEqual((await state()).data, beforeFailure);
    await page.evaluate(() => { fixture.malformed = false; fixture.repeatCursor = true; });
    await poll();
    assert.match((await state()).error, /cursor did not advance/);
    assert.deepEqual((await state()).data, beforeFailure);
    await page.evaluate(() => { fixture.repeatCursor = false; });
    await poll(); await poll();
    assert.equal((await state()).error, "");
    assert.ok((await state()).data.replies.some(reply => reply.id === "t153"));

    // Manual pagination failure retains the cursor; retry extends the revalidation boundary.
    await page.evaluate(() => { fixture.failOlder = true; window.earlier(); });
    await settle();
    assert.equal((await state()).pageError, "Older read failed");
    assert.equal((await state()).data.next_cursor, "52");
    await page.evaluate(() => { fixture.failOlder = false; window.earlier(); });
    await settle();
    assert.equal((await state()).data.replies[0].id, "t2");
    assert.equal((await state()).data.next_cursor, "2");
    await page.evaluate(() => fixture.answer(2));
    for (let i = 0; i < 4; i++) await poll();
    assert.ok((await state()).data.replies.some(reply => reply.id === "t2/reply"));
    assert.equal((await state()).data.replies[0].id, "t2");

    // A timed-out fetch that resolves successfully afterward is still rejected.
    const beforeAbort = (await state()).data;
    await page.evaluate(() => { fixture.hold = true; fixture.add(); });
    await page.clock.runFor(3000);
    await page.waitForFunction(() => fixture.held.length === 1);
    await page.clock.runFor(15000);
    assert.equal(await page.evaluate(() => fixture.held[0].request.signal.aborted), true);
    await page.evaluate(() => fixture.held.shift().release());
    await settle();
    assert.ok((await state()).error);
    assert.deepEqual((await state()).data, beforeAbort);
    await poll();
    assert.equal((await state()).error, "");

    // Both refresh and manual-pagination results are ignored across A -> B -> A.
    for (const manual of [false, true]) {
        await page.evaluate(manual => { fixture.hold = true; if (manual) window.earlier(); }, manual);
        if (!manual) await page.clock.runFor(3000);
        await page.waitForFunction(() => fixture.held.length === 1);
        await page.evaluate(() => window.switchHistory("b"));
        await settle();
        await page.waitForFunction(() => historyState.data?.conversation.id === "b");
        assert.equal(await page.evaluate(() => fixture.held[0].request.signal.aborted), true);
        await page.evaluate(() => window.switchHistory("a"));
        await settle();
        await page.waitForFunction(() => historyState.data?.conversation.id === "a");
        const fresh = (await state()).data;
        await page.evaluate(() => fixture.held.shift().release());
        await page.clock.runFor(16);
        assert.deepEqual((await state()).data, fresh);
    }
    // An unbridgeable latest window resets instead of walking all unseen history.
    await page.evaluate(() => { for (let i = 0; i < 60; i++) fixture.add(); fixture.requests = []; });
    await poll();
    assert.equal((await state()).reset, true);
    assert.equal(await page.evaluate(() => fixture.requests.length), 1);
    assert.equal((await state()).data.replies.length, 50);

    // Finishing manual pagination does not stop old-turn receipt revalidation.
    await page.evaluate(() => { fixture.turns = fixture.turns.slice(0, 60); });
    await poll();
    await page.evaluate(() => window.earlier());
    await settle();
    assert.equal((await state()).data.next_cursor, undefined);
    await page.evaluate(() => { fixture.answer(1); fixture.requests = []; });
    await poll();
    assert.ok((await state()).data.replies.some(reply => reply.id === "t1/reply"));
    assert.equal((await state()).data.next_cursor, undefined);
    assert.equal(await page.evaluate(() => fixture.requests.length), 2);
    assert.deepEqual(errors, []);
    console.log("Channel history hook: bounded revalidation, late receipts, pagination, failure atomicity, Abort and switch isolation PASS");
} finally {
    await browser.close();
    await server.close();
    await rm(cacheDir, { recursive: true, force: true });
}
