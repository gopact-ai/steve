// Actual production chunks, an isolated browser, and synthetic API fixtures.
// No hub, runtime config, shared optimizer cache, or tracked dist is used.
import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import http from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { build } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";
import { workState } from "../../../e2e/console/work-fixture.mjs";

const root = fileURLToPath(new URL("..", import.meta.url));
const scratch = await mkdtemp(path.join(tmpdir(), "steve-lazy-browser-"));
const at = "2026-09-20T00:00:00Z", conversation = "console:lazy-fixture";
async function waitForChunk(fixture) {
    let timer;
    try {
        await Promise.race([fixture.requested.promise, new Promise((_, reject) => {
            timer = setTimeout(() => reject(new Error("Expected deferred chunk was not requested within 10 seconds")), 10000);
        })]);
    } finally { clearTimeout(timer); }
}
let server, browser;
try {
    const result = await build({
        root, envDir: false, cacheDir: path.join(scratch, "cache"), logLevel: "error",
        build: { outDir: path.join(scratch, "dist"), write: false, reportCompressedSize: false },
    });
    const chunks = new Map(result.output.filter((item) => item.type === "chunk").map((chunk) => [chunk.fileName, chunk]));
    const entry = [...chunks.values()].find((chunk) => chunk.isEntry);
    const initial = new Set();
    function visit(name) {
        if (initial.has(name)) return;
        initial.add(name);
        for (const imported of chunks.get(name)?.imports ?? []) visit(imported);
    }
    visit(entry.fileName);
    function deferredChunk(suffix) {
        const chunk = [...chunks.values()].find((chunk) => Object.keys(chunk.modules).some((id) => id.endsWith(suffix)));
        assert.ok(chunk, `${suffix} remains in the production graph`);
        assert.ok(!initial.has(chunk.fileName), `${suffix} must be demand-loaded before testing delayed/failed loads`);
        return "/" + chunk.fileName;
    }
    const projectsChunk = deferredChunk("/src/pages/projects.tsx");
    const desktopChunk = deferredChunk("/src/components/steve/desktop-setup-dialog.tsx");
    const importChunk = deferredChunk("/src/components/steve/native-session-import.tsx");
    const files = new Map(result.output.map((item) => ["/" + item.fileName, item.type === "chunk" ? item.code : item.source]));
    server = http.createServer((req, res) => {
        const name = new URL(req.url, "http://localhost").pathname;
        const file = name === "/" ? "/index.html" : name;
        if (req.method !== "GET" || !files.has(file)) { res.writeHead(404); res.end(); return; }
        const type = file.endsWith(".js") ? "text/javascript" : file.endsWith(".css") ? "text/css" : file.endsWith(".html") ? "text/html" : file.endsWith(".png") ? "image/png" : "application/octet-stream";
        res.writeHead(200, { "content-type": type, "cache-control": "no-store" });
        res.end(files.get(file));
    });
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    const origin = `http://127.0.0.1:${server.address().port}`;
    browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });

    async function fixture({ desktop = false, locale = "en", chunk, fail = false } = {}) {
        const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
        const page = await context.newPage();
        page.setDefaultTimeout(10000);
        const held = Promise.withResolvers(), requested = Promise.withResolvers();
        const f = {
            context, page, held, requested, errors: [], chunkReads: 0, desktopReads: 0, agentReads: 0, setups: [],
            status: { enabled: desktop, node_id: desktop ? "fixture-node" : "", setup_required: desktop, agent_count: 0, setup: { step: "machines", done: false } },
        };
        page.on("pageerror", (error) => f.errors.push(String(error)));
        await page.addInitScript(({ conversation, locale }) => {
            localStorage.setItem("steve.ui.locale", locale);
            sessionStorage.setItem("steve.conversation", conversation);
            window.lazyDocument = "same-document";
            window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
            // Count only inert-attribute subscriptions, not React Aria's
            // legitimate child-list observers while a modal is mounted.
            const inertObservers = new Set();
            const MutationObserver = window.MutationObserver;
            window.MutationObserver = class extends MutationObserver {
                observe(target, options) {
                    if (options.attributeFilter?.includes("inert")) inertObservers.add(this);
                    return super.observe(target, options);
                }
                disconnect() { inertObservers.delete(this); super.disconnect(); }
            };
            window.lazyInertObserverCount = () => inertObservers.size;
        }, { conversation, locale });
        await page.route("**/*", async (route) => {
            const req = route.request(), url = new URL(req.url()), p = url.pathname;
            if (url.origin !== origin) { f.errors.push(`External request: ${url.origin}`); return route.abort(); }
            if (p === chunk) {
                f.chunkReads++;
                requested.resolve();
                if (fail) return route.abort("failed");
                await held.promise;
                return route.fulfill({ contentType: "text/javascript", body: files.get(p) });
            }
            if (!p.startsWith("/console/") && !["/state", "/events", "/history"].includes(p)) return route.continue();
            if (p === "/console/desktop/setup" && req.method() === "PUT") {
                const body = req.postDataJSON();
                f.setups.push(body);
                f.status = { ...f.status, setup: { step: body.step, done: !!body.done }, setup_required: !body.done };
                return route.fulfill({ json: f.status });
            }
            if (req.method() !== "GET") { f.errors.push(`Unexpected write: ${req.method()} ${p}`); return route.abort(); }
            let json;
            if (p === "/state") json = workState({ at, hub: { node: "fixture-node", version: "fixture", started: at }, nodes: [{ name: "fixture-node", role: "hub", up: true }], agents: [], projects: [], tasks: [], plans: [], attempts: [], landings: [] });
            else if (p === "/console/coordination") json = { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false };
            else if (p === "/console/desktop") { f.desktopReads++; json = f.status; }
            else if (p === "/console/desktop/agents") { f.agentReads++; json = { agents: [] }; }
            else if (p === "/console/context") json = { enabled: true, context: { conversation, agents: [] } };
            else if (p === "/console/conversations") json = { conversations: [{ id: conversation, title: "Lazy fixture", count: 1, last_at: at, running: false }] };
            else if (p === "/console/replies") json = { enabled: true, replies: [{ id: "reply", conversation, kind: "reply", text: "Retained fixture history", at }] };
            else if (p === "/console/queue") json = { queue: [], submission_keys: true };
            else if (p === "/console/verbs" || p === "/console/suggest") json = { verbs: [], suggestions: [] };
            else if (p === "/console/questions") json = { questions: [] };
            else if (p === "/console/home") json = { path: "/fixture/home", files: [], projects: [], total_budget: 0, owner_bytes: 0, guest_bytes: 0, warnings: [] };
            else { f.errors.push(`Unhandled API: ${p}`); return route.fulfill({ status: 500, json: { error: "Unmocked API" } }); }
            return route.fulfill({ json });
        });
        f.open = async (hash = "/console") => {
            await page.goto(origin + "/#" + hash, { waitUntil: "domcontentloaded" });
            await page.getByRole("heading", { name: "Lazy fixture", exact: true }).waitFor();
        };
        f.close = async () => {
            held.resolve();
            const unexpected = f.errors.filter((error) => !fail || !/Failed to fetch dynamically imported module|Importing a module script failed/.test(error));
            assert.deepEqual(unexpected, []);
            await context.close();
        };
        return f;
    }
    const editor = (f) => f.page.locator('.cm-content[contenteditable="true"]');
    async function draft(f) {
        await editor(f).click();
        await f.page.keyboard.type("keep this draft");
    }
    async function draftRestored(f) {
        await f.page.waitForFunction(() => document.querySelector(".cm-content")?.textContent === "keep this draft");
        assert.equal(await f.page.evaluate(() => window.lazyDocument), "same-document", "chunk recovery never reloads the document");
    }
    const paint = (page) => page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));

    const delayed = await fixture({ chunk: projectsChunk });
    try {
        await delayed.open();
        await draft(delayed);
        await editor(delayed).evaluate((element) => { window.retainedLazyEditor = element; });
        await delayed.page.evaluate(() => { location.hash = "/console?fixture=retained"; });
        await paint(delayed.page);
        assert.equal(await editor(delayed).evaluate((element) => element === window.retainedLazyEditor), true, "resetKey changes do not remount healthy Console children");
        assert.equal(delayed.chunkReads, 0);
        await delayed.page.getByRole("link", { name: "Projects", exact: true }).click();
        await waitForChunk(delayed);
        await delayed.page.locator("#main-content").getByText("Loading…", { exact: true }).waitFor();
        await delayed.page.getByRole("link", { name: "Workbench", exact: true }).click();
        await draftRestored(delayed);
        delayed.held.resolve();
        await delayed.page.getByRole("link", { name: "Projects", exact: true }).click();
        await delayed.page.getByRole("heading", { name: "Projects", exact: true }).waitFor();
        await delayed.page.getByRole("link", { name: "Workbench", exact: true }).click();
        await draftRestored(delayed);
        console.log("PASS delayed route permits navigation and retains the Console draft");
    } finally { await delayed.close(); }

    const failed = await fixture({ chunk: projectsChunk, fail: true });
    try {
        await failed.open();
        await draft(failed);
        await failed.page.getByRole("link", { name: "Projects", exact: true }).click();
        await failed.page.locator("#main-content").getByRole("alert").getByText("Unavailable", { exact: true }).waitFor();
        await failed.page.getByRole("link", { name: "Inbox", exact: true }).click();
        await failed.page.getByRole("heading", { name: "Inbox", exact: true }).waitFor();
        await failed.page.getByRole("link", { name: "Projects", exact: true }).click();
        await failed.page.locator("#main-content").getByRole("alert").getByRole("button", { name: "Close", exact: true }).click();
        await draftRestored(failed);
        console.log("PASS failed route resets on navigation and has a non-reloading close exit");
    } finally { await failed.close(); }

    const pendingImport = await fixture({ chunk: importChunk });
    try {
        await pendingImport.open();
        await draft(pendingImport);
        const trigger = pendingImport.page.getByRole("button", { name: "Import past session", exact: true });
        await trigger.click();
        await waitForChunk(pendingImport);
        const loading = pendingImport.page.getByRole("status").filter({ has: pendingImport.page.getByRole("button", { name: "Close", exact: true }) });
        await loading.waitFor();
        assert.equal(await pendingImport.page.getByText("Retained fixture history", { exact: true }).isVisible(), true, "local suspense never hides the transcript");
        assert.equal(await editor(pendingImport).isVisible(), true);
        await loading.getByRole("button", { name: "Close", exact: true }).click();
        await draftRestored(pendingImport);
        await pendingImport.page.waitForFunction(() => document.activeElement?.textContent === "Import past session");
        pendingImport.held.resolve();
        await paint(pendingImport.page);
        assert.equal(await pendingImport.page.getByRole("dialog").count(), 0, "late resolution cannot reopen a closed import");
        console.log("PASS delayed local import closes without hiding history, losing focus or reopening");
    } finally { await pendingImport.close(); }

    const failedImport = await fixture({ chunk: importChunk, fail: true });
    try {
        await failedImport.open();
        await draft(failedImport);
        await failedImport.page.getByRole("button", { name: "Import past session", exact: true }).click();
        const alert = failedImport.page.getByRole("alert").filter({ hasText: "Unavailable" });
        await alert.getByRole("button", { name: "Close", exact: true }).click();
        await draftRestored(failedImport);
        await failedImport.page.waitForFunction(() => document.activeElement?.textContent === "Import past session");
        assert.equal(await failedImport.page.getByText("Retained fixture history", { exact: true }).isVisible(), true);
        console.log("PASS failed local import closes back to its trigger and preserves the workbench");
    } finally { await failedImport.close(); }

    const layers = await fixture({ chunk: importChunk });
    try {
        await layers.open();
        await draft(layers);
        const trigger = layers.page.getByRole("button", { name: "Import past session", exact: true });
        await trigger.click();
        await waitForChunk(layers);
        layers.status = { enabled: true, node_id: "fixture-node", setup_required: false, agent_count: 0, setup: { step: "finished", done: true } };
        await layers.page.evaluate(() => { location.hash = "/console?setup=agents"; });
        const dialog = layers.page.getByRole("dialog", { name: "First-time setup", exact: true });
        await dialog.getByRole("heading", { name: "Local agents", exact: true }).waitFor();
        await layers.page.keyboard.press("Escape");
        await dialog.waitFor({ state: "hidden" });
        const notice = layers.page.getByRole("status").filter({ has: layers.page.getByRole("button", { name: "Close", exact: true }) });
        await notice.waitFor();
        assert.equal(new URL(layers.page.url()).hash, "#/console", "Escape dismisses only the real top modal");
        await notice.getByRole("button", { name: "Close", exact: true }).focus();
        await layers.page.keyboard.press("Escape");
        await notice.waitFor({ state: "hidden" });
        await draftRestored(layers);
        assert.equal(await layers.page.evaluate(() => window.steveCloseLayer()), false, "one Escape leaves no phantom loader layer");
        await trigger.click();
        await notice.waitFor();
        assert.equal(await layers.page.evaluate(() => window.steveCloseLayer()), true, "the pending view uses the existing native close stack");
        await notice.waitFor({ state: "hidden" });
        assert.equal(await layers.page.evaluate(() => window.steveCloseLayer()), false);
        await draftRestored(layers);
        console.log("PASS Escape closes one focused layer and cannot pierce an existing modal");
    } finally { await layers.close(); }

    const reverseLayers = await fixture({ desktop: true, chunk: desktopChunk });
    try {
        await reverseLayers.open();
        await waitForChunk(reverseLayers);
        const notice = reverseLayers.page.locator('[role="status"]').filter({ hasText: "Loading…" });
        await notice.waitFor();
        const trigger = reverseLayers.page.getByRole("button", { name: "Import past session", exact: true });
        const sheet = reverseLayers.page.getByRole("dialog", { name: "Import past session", exact: true });
        for (const close of ["native", "Control+w", "Escape"]) {
            await trigger.click();
            await sheet.waitFor();
            await reverseLayers.page.waitForFunction(() => document.querySelector('[role="dialog"]')?.contains(document.activeElement));
            if (close === "native") assert.equal(await reverseLayers.page.evaluate(() => window.steveCloseLayer()), true);
            else await reverseLayers.page.keyboard.press(close);
            await paint(reverseLayers.page);
            assert.equal(await sheet.count(), 0, `${close} closes the foreground Sheet, not the background setup loader`);
            assert.equal(await notice.count(), 1, `${close} leaves the pending setup intact`);
            assert.equal(await reverseLayers.page.evaluate(() => sessionStorage.getItem("steve.desktop.setup-deferred:fixture-node")), null, `${close} must not defer background setup`);
            await reverseLayers.page.waitForFunction(() => document.activeElement?.textContent === "Import past session");
            assert.deepEqual(reverseLayers.setups, []);
        }
        assert.equal(await reverseLayers.page.evaluate(() => window.steveCloseLayer()), true);
        await notice.waitFor({ state: "hidden" });
        assert.equal(await reverseLayers.page.evaluate(() => sessionStorage.getItem("steve.desktop.setup-deferred:fixture-node")), "1", "only the next close defers setup");
        assert.equal(await reverseLayers.page.evaluate(() => window.steveCloseLayer()), false);
        for (let i = 0; i < 20; i++) {
            await trigger.focus();
            await reverseLayers.page.evaluate(() => { location.hash = "/console?setup=agents"; });
            await notice.waitFor();
            await trigger.click();
            await sheet.waitFor();
            await reverseLayers.page.waitForFunction(() => document.querySelector('[role="dialog"]')?.contains(document.activeElement));
            // Cancel the background region while the actual Sheet still owns
            // focus/inert, as can happen through application-driven dismissal.
            await notice.evaluate((element) => element.querySelector("button").click());
            await notice.waitFor({ state: "hidden" });
            await paint(reverseLayers.page);
            assert.equal(await sheet.count(), 1);
            assert.equal(await sheet.evaluate((element) => element.contains(document.activeElement)), true, "background cancellation cannot steal modal focus");
            assert.equal(await reverseLayers.page.evaluate(() => window.lazyInertObserverCount()), 0, `cancel ${i + 1} leaves no inert observer behind the modal`);
            if (i % 2 === 0) {
                // Navigation removes the Sheet and its Console trigger too.
                await reverseLayers.page.evaluate(() => { location.hash = "/inbox"; });
                await reverseLayers.page.getByRole("heading", { name: "Inbox", exact: true }).waitFor();
                assert.equal(await trigger.count(), 0);
                await reverseLayers.page.getByRole("link", { name: "Workbench", exact: true }).click();
                await trigger.waitFor();
            } else {
                await reverseLayers.page.keyboard.press("Escape");
            }
            await sheet.waitFor({ state: "hidden" });
            await paint(reverseLayers.page);
            assert.equal(await reverseLayers.page.evaluate(() => window.lazyInertObserverCount()), 0, `cancel ${i + 1} leaves no inert observer after close/navigation`);
            assert.equal(await reverseLayers.page.evaluate(() => window.steveCloseLayer()), false);
        }
        assert.deepEqual(reverseLayers.setups, [], "repeated cancel/navigation never writes setup progress");
        reverseLayers.held.resolve();
        await paint(reverseLayers.page);
        assert.equal(await sheet.count(), 0, "late setup resolution does not reopen either layer");
        console.log("PASS native close, Ctrl+W and Escape respect the foreground Sheet; 20 cancel/modal/navigation cycles retain focus without inert observers");
    } finally { await reverseLayers.close(); }

    const pendingSetup = await fixture({ desktop: true, chunk: desktopChunk });
    try {
        await pendingSetup.open();
        await waitForChunk(pendingSetup);
        const loading = pendingSetup.page.getByRole("status").filter({ has: pendingSetup.page.getByRole("button", { name: "Close", exact: true }) });
        await loading.getByRole("button", { name: "Close", exact: true }).click();
        assert.equal(await pendingSetup.page.evaluate(() => sessionStorage.getItem("steve.desktop.setup-deferred:fixture-node")), "1");
        pendingSetup.held.resolve();
        await paint(pendingSetup.page);
        assert.equal(await pendingSetup.page.getByRole("dialog").count(), 0);
        assert.equal(pendingSetup.agentReads, 0, "deferred setup does not discover local agents");
        assert.deepEqual(pendingSetup.setups, [], "closing a loader does not write setup progress");
        await pendingSetup.page.evaluate(() => { location.hash = "/console?setup=agents"; });
        const dialog = pendingSetup.page.getByRole("dialog", { name: "First-time setup", exact: true });
        const heading = dialog.getByRole("heading", { name: "Local agents", exact: true });
        await heading.waitFor();
        await pendingSetup.page.waitForFunction(() => document.activeElement?.textContent === "Local agents");
        await dialog.getByRole("button", { name: "Back", exact: true }).click();
        await dialog.getByRole("heading", { name: "Working directory", exact: true }).waitFor();
        await pendingSetup.page.waitForFunction(() => document.activeElement?.textContent === "Working directory");
        await dialog.getByRole("button", { name: "Finish later", exact: true }).click();
        await dialog.waitFor({ state: "hidden" });
        assert.equal(new URL(pendingSetup.page.url()).hash, "#/console");
        assert.deepEqual(pendingSetup.setups, [{ step: "workspace" }], "Back keeps the original serialized progress write");
        console.log("PASS delayed desktop can defer, explicitly reopen, focus its heading and go Back");
    } finally { await pendingSetup.close(); }

    const loadedSetup = await fixture({ desktop: true, chunk: desktopChunk });
    try {
        loadedSetup.status = { ...loadedSetup.status, setup_required: false, setup: { step: "finished", done: true } };
        await loadedSetup.open();
        const opener = loadedSetup.page.getByRole("link", { name: "Workbench", exact: true });
        await opener.focus();
        await loadedSetup.page.evaluate(() => { location.hash = "/console?setup=agents"; });
        await waitForChunk(loadedSetup);
        await loadedSetup.page.getByRole("status").filter({ has: loadedSetup.page.getByRole("button", { name: "Close", exact: true }) }).waitFor();
        loadedSetup.held.resolve();
        const dialog = loadedSetup.page.getByRole("dialog", { name: "First-time setup", exact: true });
        await dialog.getByRole("heading", { name: "Local agents", exact: true }).waitFor();
        await loadedSetup.page.waitForFunction(() => document.activeElement?.textContent === "Local agents");
        assert.equal(await dialog.getByRole("button", { name: "Back", exact: true }).count(), 0);
        await loadedSetup.page.keyboard.press("Escape");
        await dialog.waitFor({ state: "hidden" });
        await loadedSetup.page.waitForFunction(() => document.activeElement?.getAttribute("href") === "#/console").catch(async (error) => {
            assert.fail(`${error.message}\nFocus after close: ${await loadedSetup.page.evaluate(() => document.activeElement?.outerHTML.slice(0, 500))}`);
        });
        assert.deepEqual(loadedSetup.setups, [], "one-page setup does not change finished progress");
        console.log("PASS delayed desktop dialog owns focus while open and returns it on close");
    } finally { await loadedSetup.close(); }

    const failedSetup = await fixture({ desktop: true, locale: "zh", chunk: desktopChunk, fail: true });
    try {
        await failedSetup.open("/console?setup=agents");
        const alert = failedSetup.page.getByRole("alert").filter({ hasText: "不可用" });
        await alert.getByRole("button", { name: "关闭", exact: true }).click();
        assert.equal(new URL(failedSetup.page.url()).hash, "#/console");
        assert.equal(await failedSetup.page.evaluate(() => window.lazyDocument), "same-document");
        assert.equal(await failedSetup.page.evaluate(() => sessionStorage.getItem("steve.desktop.setup-deferred:fixture-node")), "1");
        assert.deepEqual(failedSetup.setups, []);
        await failedSetup.page.reload();
        await failedSetup.page.getByRole("heading", { name: "Lazy fixture", exact: true }).waitFor();
        await paint(failedSetup.page);
        assert.ok(failedSetup.desktopReads >= 2, "the eager gate still checks availability after reload");
        assert.equal(failedSetup.chunkReads, 1, "defer survives this window's reload without fetching the failed dialog");
        assert.equal(await failedSetup.page.getByRole("dialog").count(), 0);
        console.log("PASS localized desktop failure can defer and clean its setup query without forced reload");
    } finally { await failedSetup.close(); }
} finally {
    await browser?.close();
    if (server) await new Promise((resolve) => server.close(resolve));
    await rm(scratch, { recursive: true, force: true });
}
