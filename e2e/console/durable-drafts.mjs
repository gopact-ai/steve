// Isolated browser origins and local fixtures; no backend or model calls.
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const harness = `import React from 'react';import {createRoot} from 'react-dom/client';import * as drafts from '/src/lib/drafts.ts';function Editor(){const value=drafts.useDraft('console:a');const issue=drafts.useDraftIssue('console:a');return React.createElement('div',null,React.createElement('textarea',{value,onChange:e=>void drafts.updateDraft('console:a',e.target.value)}),React.createElement('p',{role:'status'},issue));}createRoot(document.getElementById('root')).render(React.createElement(Editor));window.drafts=drafts;`;
const server = await createServer({ plugins: [{ name: "draft-hooks", resolveId(id) { if (id === "/__draft-hooks.tsx") return id; }, load(id) { if (id === "/__draft-hooks.tsx") return harness; } }], configFile: path.join(web, "vite.config.ts"), root: web, server: { host: "127.0.0.1", port: 0 } });
await server.listen();
const origin = server.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const context = await browser.newContext();
const errors = [];
context.on("page", (page) => page.on("pageerror", (error) => errors.push(String(error))));
const fixture = '<!doctype html><body><div id="root"></div><script type="module" src="/__draft-hooks.tsx"></script></body>';
await context.route("**/draft-fixture", async (route) => route.fulfill({ contentType: "text/html", body: await server.transformIndexHtml("/draft-fixture", fixture) }));
const open = async () => { const page = await context.newPage(); await page.goto(origin + "draft-fixture"); await page.waitForFunction(() => !!window.drafts); return page; };
const read = (page) => page.evaluate(() => JSON.parse(localStorage.getItem("steve.console.drafts")));
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

    await first.evaluate(() => { window.originalSet = Storage.prototype.setItem; Storage.prototype.setItem = function(key, value) { if (key === "steve.console.drafts") throw new DOMException("Full", "QuotaExceededError"); return window.originalSet.call(this, key, value); }; });
    assert.equal(await first.evaluate((id) => drafts.finishSubmission("console:pending", id), original), false);
    await first.evaluate(() => { Storage.prototype.setItem = window.originalSet; });
    const afterReceiptFailure = await first.evaluate(() => drafts.retrySubmission("console:pending"));
    assert.equal(afterReceiptFailure.id, original, "a failed receipt save cannot leave the finished network request permanently active");
    await first.evaluate(() => { Storage.prototype.setItem = function(key, value) { if (key === "steve.console.drafts") throw new DOMException("Full", "QuotaExceededError"); return window.originalSet.call(this, key, value); }; });
    assert.equal(await first.evaluate((id) => drafts.failSubmission("console:pending", id, "not sent", "rejected"), original), false);
    await first.evaluate(() => { Storage.prototype.setItem = window.originalSet; });
    assert.equal((await first.evaluate(() => drafts.retrySubmission("console:pending"))).id, original, "a failed rejection save retains the original retry identity without a stuck in-flight state");
    const beforeFailure = await read(first);
    await first.evaluate(() => { window.originalSet = Storage.prototype.setItem; Storage.prototype.setItem = function(key, value) { if (key === "steve.console.drafts") throw new DOMException("Full", "QuotaExceededError"); return window.originalSet.call(this, key, value); }; });
    await assert.rejects(first.evaluate(() => drafts.beginSubmission("console:fast", "abc", [])), /Cannot retain pending submission/);
    assert.deepEqual(await read(first), beforeFailure);
    await first.evaluate(() => { Storage.prototype.setItem = window.originalSet; });
    const race = await Promise.all([first.evaluate(() => drafts.beginSubmission("console:fast", "abc", [])), second.evaluate(() => drafts.beginSubmission("console:fast", "abc", []))]);
    assert.equal(race.filter(Boolean).length, 1, "only one window may create a new submission for a conversation");
    assert.equal((await read(first)).submissions["console:fast"].id, race.find(Boolean).id);
    const noLocks = await open();
    await noLocks.evaluate(() => Object.defineProperty(navigator, "locks", { value: undefined }));
    await assert.rejects(noLocks.evaluate(() => drafts.beginSubmission("console:new", "must not send", [])), /locking is unavailable/);
    assert.equal((await read(first)).submissions["console:new"], undefined);
    assert.deepEqual(errors, []);
    console.log("PASS storage failure and missing locking never consume drafts or create a sendable command; competing sends use one identity");
} finally { await context.close(); await browser.close(); await server.close(); }
