// Isolated browser origins and local fixtures; no backend or model calls.
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const harness = `import React from 'react';import {createRoot} from 'react-dom/client';import * as drafts from '/src/lib/drafts.ts';import {LocaleProvider} from '/src/providers/locale-provider.tsx';import {SideChatProvider,useSideChat} from '/src/providers/side-chat-provider.tsx';const ids=(new URLSearchParams(location.search).get('ids')||'console:a').split(',');function Editor({id}){const value=drafts.useDraft(id);const issue=drafts.useDraftIssue(id);return React.createElement('div',null,React.createElement('textarea',{'aria-label':id,value,onChange:e=>void drafts.updateDraft(id,e.target.value)}),React.createElement('p',{role:'status','aria-label':id},issue));}function Side(){const side=useSideChat();window.sideChat=side;window.sideSession=side.session;return null;}createRoot(document.getElementById('root')).render(React.createElement(LocaleProvider,null,React.createElement(SideChatProvider,null,React.createElement(Side),ids.map((id)=>React.createElement(Editor,{key:id,id})))));window.drafts=drafts;`;
const server = await createServer({ plugins: [{ name: "draft-hooks", resolveId(id) { if (id === "/__draft-hooks.tsx") return id; }, load(id) { if (id === "/__draft-hooks.tsx") return harness; } }], configFile: path.join(web, "vite.config.ts"), root: web, server: { host: "127.0.0.1", port: 0 } });
await server.listen();
const origin = server.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const context = await browser.newContext();
const errors = [];
context.on("page", (page) => page.on("pageerror", (error) => errors.push(String(error))));
const fixture = '<!doctype html><body><div id="root"></div><script type="module" src="/__draft-hooks.tsx"></script></body>';
await context.route("**/draft-fixture*", async (route) => route.fulfill({ contentType: "text/html", body: await server.transformIndexHtml("/draft-fixture", fixture) }));
// Models a window whose localStorage cache lags other windows by delay ms,
// as separate renderer processes may: its own writes apply immediately, other
// windows' writes (and their storage events) become visible only later.
const lagStorage = (delay) => {
    const store = window.localStorage, proto = Storage.prototype;
    const real = { get: proto.getItem, set: proto.setItem, remove: proto.removeItem, key: proto.key, length: Object.getOwnPropertyDescriptor(proto, "length").get };
    const view = new Map();
    const reload = () => { view.clear(); for (let i = 0; i < real.length.call(store); i++) { const key = real.key.call(store, i); view.set(key, real.get.call(store, key)); } };
    reload();
    proto.getItem = function (key) { return this === store ? view.get(String(key)) ?? null : real.get.call(this, key); };
    proto.setItem = function (key, value) { real.set.call(this, key, value); if (this === store) view.set(String(key), String(value)); };
    proto.removeItem = function (key) { real.remove.call(this, key); if (this === store) view.delete(String(key)); };
    proto.key = function (index) { return this === store ? [...view.keys()][index] ?? null : real.key.call(this, index); };
    Object.defineProperty(proto, "length", { configurable: true, get() { return this === store ? view.size : real.length.call(this); } });
    window.addEventListener("storage", (event) => {
        if (event.storageArea !== store || event.lagged) return;
        event.stopImmediatePropagation();
        setTimeout(() => {
            // By then the window has caught up with the stored value.
            if (event.key === null) reload();
            else { const value = real.get.call(store, event.key); if (value === null) view.delete(event.key); else view.set(event.key, value); }
            const late = new StorageEvent("storage", { key: event.key, oldValue: event.oldValue, newValue: event.newValue, url: event.url, storageArea: store });
            late.lagged = true; window.dispatchEvent(late);
        }, delay);
    }, true);
};
const open = async (ids, lag) => { const page = await context.newPage(); if (lag) await page.addInitScript(lagStorage, lag); await page.goto(origin + "draft-fixture" + (ids ? "?ids=" + encodeURIComponent(ids.join(",")) : "")); await page.waitForFunction(() => !!window.drafts && !!window.sideChat); return page; };
// Collects the per-item draft keys into one object, or null when there are none.
const read = (page) => page.evaluate(() => {
    const kinds = { text: "drafts", materials: "materials", submission: "submissions", rewind: "rewinds" }, saved = { drafts: {}, materials: {}, submissions: {}, rewinds: {}, quotes: [] };
    let found = false;
    for (let index = 0; index < localStorage.length; index++) {
        const key = localStorage.key(index), match = /^steve\.console\.draft\.(text|materials|submission|rewind|quote):/.exec(key);
        if (!match) continue;
        const id = key.slice(match[0].length), raw = localStorage.getItem(key);
        found = true;
        if (match[1] === "text") saved.drafts[id] = raw; else if (match[1] === "quote") saved.quotes.push(JSON.parse(raw).quote); else saved[kinds[match[1]]][id] = JSON.parse(raw);
    }
    return found ? saved : null;
});
// Every draft write or removal fails, as when storage is full or unavailable.
const failStorage = (page) => page.evaluate(() => {
    const proto = Storage.prototype, set = proto.setItem, remove = proto.removeItem, fail = (key) => { if (String(key).startsWith("steve.console.draft.")) throw new DOMException("Full", "QuotaExceededError"); };
    window.restoreStorage = () => { proto.setItem = set; proto.removeItem = remove; };
    proto.setItem = function (key, value) { fail(key); return set.call(this, key, value); };
    proto.removeItem = function (key) { fail(key); return remove.call(this, key); };
});
const restoreStorage = (page) => page.evaluate(() => window.restoreStorage());
const material = { id: "material-snapshot", title: "retry.py", project: "p", kind: "text", mime: "text/plain", size: 123, selector: { kind: "lines", start: 3, end: 3 } };
try {
    let first = await open();
    await first.evaluate(async (material) => {
        await drafts.updateDraft("console:a", "unsent text");
        await drafts.addDraftMaterial("console:a", material);
        await drafts.updateDraft("console:pending", "original pending work");
        await drafts.addDraftMaterial("console:pending", material);
        const pending = await drafts.beginSubmission("console:pending", "original pending work", [], true, "zh");
        window.originalID = pending.id;
        await drafts.updateDraft("console:pending", "newer unsent text");
    }, material);
    const original = await first.evaluate(() => window.originalID);
    await first.close();
    first = await open();
    const reloaded = await read(first);
    assert.equal(reloaded.drafts["console:a"], "unsent text");
    assert.deepEqual(reloaded.materials["console:a"], [material]);
    const retried = await first.evaluate(() => drafts.retrySubmission("console:pending"));
    assert.equal(retried.id, original); assert.deepEqual(retried.refs, [material]);
    assert.equal(retried.locale, "zh");
    assert.equal((await read(first)).drafts["console:pending"], "newer unsent text");
    const isolated = await context.newPage();
    await isolated.goto(origin.replace("127.0.0.1", "localhost") + "draft-fixture");
    await isolated.waitForFunction(() => !!window.drafts);
    assert.equal(await read(isolated), null, "a different service origin cannot read these drafts");
    await isolated.close();
    console.log("PASS closing every page retains text, snapshot selectors and original pending command identity");

    const second = await open();
    await first.evaluate(() => { window.ready = false; navigator.locks.request("steve.console.drafts", async () => { window.ready = true; await new Promise((resolve) => window.unlock = resolve); }); });
    await first.waitForFunction(() => window.ready);
    await first.evaluate(() => { window.edit = drafts.updateDraft("console:a", "first window edit"); });
    await second.evaluate(() => { window.edit = drafts.updateDraft("console:a", "second window edit"); });
    await first.evaluate(() => window.unlock());
    assert.equal(await first.evaluate(() => window.edit), true);
    assert.equal(await second.evaluate(() => window.edit), false);
    assert.equal((await read(first)).drafts["console:a"], "first window edit");
    assert.equal(await second.locator("textarea").inputValue(), "second window edit", "conflicting local input remains available for copying or explicit resolution");
    assert.equal(await second.getByRole("status").innerText(), "conflict");
    await assert.rejects(second.evaluate(() => drafts.beginSubmission("console:a", "second window edit", [])), /Review the unsaved draft/);
    assert.equal(await second.evaluate(() => drafts.resolveDraftConflict("console:a", "local")), true);
    assert.equal((await read(first)).drafts["console:a"], "second window edit");
    const merged = await Promise.all([
        first.evaluate(() => drafts.updateDraft("console:b", "other conversation")),
        second.evaluate(() => drafts.addDraftMaterial("console:a", { id: "upload", title: "notes.txt", project: "p", kind: "text", mime: "text/plain", size: 12 })),
    ]);
    assert.deepEqual(merged, [true, true]);
    assert.equal((await read(first)).drafts["console:b"], "other conversation");
    assert.equal((await read(first)).materials["console:a"].length, 2);
    await first.evaluate(async () => { await Promise.all([drafts.updateDraft("console:fast", "a"), drafts.updateDraft("console:fast", "ab"), drafts.updateDraft("console:fast", "abc")]); });
    assert.equal((await read(first)).drafts["console:fast"], "abc");
    console.log("PASS concurrent windows preserve conflicts and unrelated edits; rapid typing cannot overwrite newer local input");

    await failStorage(first);
    assert.equal(await first.evaluate((id) => drafts.finishSubmission("console:pending", id), original), false);
    await restoreStorage(first);
    const afterReceiptFailure = await first.evaluate(() => drafts.retrySubmission("console:pending"));
    assert.equal(afterReceiptFailure.id, original, "a failed receipt save cannot leave the finished network request permanently active");
    await failStorage(first);
    assert.equal(await first.evaluate((id) => drafts.failSubmission("console:pending", id, "not sent", "rejected"), original), false);
    await restoreStorage(first);
    assert.equal((await first.evaluate(() => drafts.retrySubmission("console:pending"))).id, original, "a failed rejection save retains the original retry identity without a stuck in-flight state");
    const beforeFailure = await read(first);
    await failStorage(first);
    await assert.rejects(first.evaluate(() => drafts.beginSubmission("console:fast", "abc", [])), /Cannot retain pending submission/);
    assert.deepEqual(await read(first), beforeFailure);
    await restoreStorage(first);
    const race = await Promise.all([first.evaluate(() => drafts.beginSubmission("console:fast", "abc", [])), second.evaluate(() => drafts.beginSubmission("console:fast", "abc", []))]);
    assert.equal(race.filter(Boolean).length, 1, "only one window may create a new submission for a conversation");
    assert.equal((await read(first)).submissions["console:fast"].id, race.find(Boolean).id);
    const noLocks = await open();
    await noLocks.evaluate(() => Object.defineProperty(navigator, "locks", { value: undefined }));
    await assert.rejects(noLocks.evaluate(() => drafts.beginSubmission("console:new", "must not send", [])), /locking is unavailable/);
    assert.equal((await read(first)).submissions["console:new"], undefined);

    // Pages opened independently run in separate renderer processes. Their
    // localStorage caches converge asynchronously, so a lock held while one
    // window reads cannot guarantee it sees the other window's last save.
    for (const [lag, rounds] of [[0, 30], [150, 10]]) {
        const prefix = `lag${lag}-`, shared = `console:${prefix}shared`, left = await open([shared], lag), right = await open([shared], lag);
        const edit = (page, side, round) => page.evaluate(({ side, round, prefix }) => drafts.updateDraft(`console:${prefix}${side}-${round}`, `${side} ${round}`), { side, round, prefix });
        const openSide = async (page, side, round, title = side) => { const project = `${prefix}${side}-${round}`; await page.evaluate(({ project, title }) => sideChat.open({ material: { id: project, project, title, kind: "text", mime: "text/plain", size: 1 }, ref: { id: project }, excerpt: project }), { project, title }); return (await page.waitForFunction((project) => window.sideSession?.project === project && window.sideSession, project)).jsonValue(); };
        for (let round = 0; round < rounds; round++) assert.deepEqual(await Promise.all([edit(left, "left", round), edit(right, "right", round)]), [true, true], `round ${round} saves both drafts`);
        const opened = [];
        for (let round = 0; round < rounds; round++) opened.push(...await Promise.all([openSide(left, "left", round), openSide(right, "right", round)]));
        const ids = Array.from({ length: rounds }, (_, round) => [`console:${prefix}left-${round}`, `console:${prefix}right-${round}`]).flat();
        const observer = await open(ids), lost = [];
        for (const id of ids) if (await observer.getByRole("textbox", { name: id, exact: true }).inputValue() !== id.slice(`console:${prefix}`.length).replace("-", " ")) lost.push(id);
        assert.deepEqual(lost, [], `lag ${lag}: separate windows editing different drafts lose none of ${ids.length}`);
        // Reopening an existing side conversation keeps its identity and title.
        for (let round = 0; round < rounds; round++) for (const [index, side] of ["left", "right"].entries()) {
            const session = await openSide(observer, side, round, "observer");
            if (session.id !== opened[round * 2 + index].id || session.title !== side) lost.push(`${side}-${round}`);
        }
        assert.deepEqual(lost, [], `lag ${lag}: separate windows opening different side conversations lose none of ${rounds * 2}`);
        // One draft edited in both windows ends with a single stored value;
        // each window shows it unless it reports the conflict for review.
        const writes = await Promise.all([left.evaluate((id) => drafts.updateDraft(id, "from left"), shared), right.evaluate((id) => drafts.updateDraft(id, "from right"), shared)]);
        assert.ok(writes.includes(true));
        const reader = await open([shared]), winner = await reader.getByRole("textbox", { name: shared, exact: true }).inputValue();
        assert.ok(["from left", "from right"].includes(winner));
        for (const page of [left, right]) await page.waitForFunction(({ id, winner }) => document.querySelector(`textarea[aria-label="${id}"]`).value === winner || document.querySelector(`p[aria-label="${id}"]`).textContent === "conflict", { id: shared, winner });
        await Promise.all([left.close(), right.close(), observer.close(), reader.close()]);
    }
    console.log("PASS simultaneous edits from separate renderers, with and without delayed propagation, keep every draft and side conversation");

    // A side conversation another window changes is shown as stored, without
    // waiting for this window's next write, and this window writes nothing
    // back. A binding this window has in flight stays visible meanwhile.
    for (const lag of [0, 150]) {
        const project = `side-sync-${lag}`, source = (excerpt) => ({ material: { id: project, project, title: "notes", kind: "text", mime: "text/plain", size: 1 }, ref: { id: project }, excerpt });
        const here = await open(undefined, lag), there = await open(undefined, lag);
        await here.evaluate((input) => sideChat.open(input), source("first"));
        const id = await here.waitForFunction((project) => window.sideSession?.project === project && window.sideSession.id, project).then((handle) => handle.jsonValue());
        await here.route("**/console/**", () => {}); // The hub never answers, so binding stays in flight.
        await here.evaluate(() => { void sideChat.ensureBound(window.sideSession).catch(() => {}); });
        await here.waitForFunction(() => window.sideSession?.binding === true);
        await here.evaluate(() => { const set = Storage.prototype.setItem, remove = Storage.prototype.removeItem, count = (key) => { if (String(key).startsWith("steve.side-conversation:")) window.sideWrites++; }; window.sideWrites = 0; Storage.prototype.setItem = function (key, value) { count(key); return set.call(this, key, value); }; Storage.prototype.removeItem = function (key) { count(key); return remove.call(this, key); }; });
        await there.waitForFunction(({ project, id }) => JSON.parse(localStorage.getItem("steve.side-conversation:" + project))?.id === id, { project, id });
        await there.evaluate((input) => sideChat.open(input), source("second"));
        await here.waitForFunction(() => window.sideSession?.excerpt === "second", undefined, { timeout: 3000 }).catch(() => assert.fail(`lag ${lag}: this window shows the side conversation another window changed`));
        await new Promise((resolve) => setTimeout(resolve, lag + 300));
        assert.deepEqual(await here.evaluate(() => ({ id: window.sideSession.id, binding: window.sideSession.binding, writes: window.sideWrites })), { id, binding: true, writes: 0 }, `lag ${lag}: the same conversation, still binding, and nothing written back`);
        assert.equal(await there.evaluate((project) => JSON.parse(localStorage.getItem("steve.side-conversation:" + project)).excerpt, project), "second");
        await Promise.all([here.close(), there.close()]);
    }
    console.log("PASS a side conversation changed in another window is shown without being written back, and an in-flight binding survives it");

    // Values stored in the former single-key layout, or unreadable entries, are ignored.
    const legacy = await context.newPage();
    await legacy.addInitScript(() => {
        if (sessionStorage.getItem("seeded")) return;
        sessionStorage.setItem("seeded", "1");
        localStorage.setItem("steve.console.drafts", JSON.stringify({ drafts: { "console:legacy": "old layout" }, submissions: {}, quotes: [], materials: {}, rewinds: {} }));
        localStorage.setItem("steve.side-conversations", "{not json");
        localStorage.setItem("steve.console.draft.submission:console:legacy", "{not json");
        localStorage.setItem("steve.console.draft.quote:broken", JSON.stringify({ quote: { conversation: 1 } }));
    });
    await legacy.goto(origin + "draft-fixture?ids=console:legacy");
    await legacy.waitForFunction(() => !!window.drafts && !!window.sideChat);
    assert.equal(await legacy.getByRole("textbox", { name: "console:legacy", exact: true }).inputValue(), "");
    assert.deepEqual(await legacy.evaluate(() => [localStorage.getItem("steve.console.drafts"), localStorage.getItem("steve.side-conversations")]), [null, null], "the former layout is cleared");
    assert.equal(await legacy.evaluate(() => drafts.updateDraft("console:legacy", "new layout")), true);
    assert.equal(await legacy.evaluate(() => localStorage.getItem("steve.console.draft.text:console:legacy")), "new layout");
    await legacy.close();
    console.log("PASS values from the former storage layout or unreadable entries are ignored without errors");

    assert.deepEqual(errors, []);
    console.log("PASS storage failure and missing locking never consume drafts or create a sendable command; competing sends use one identity");
} finally { await context.close(); await browser.close(); await server.close(); }
