// Actual production chunks, an isolated browser, and synthetic API fixtures.
// Only the chosen language is fetched; a switch keeps the page as it is,
// in the old language, until the new messages have arrived.
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
const scratch = await mkdtemp(path.join(tmpdir(), "steve-locale-browser-"));
const at = "2026-09-20T00:00:00Z", conversation = "console:locale-fixture";
let server, browser;
try {
    const result = await build({
        root, envDir: false, cacheDir: path.join(scratch, "cache"), logLevel: "error",
        build: { outDir: path.join(scratch, "dist"), write: false, reportCompressedSize: false },
    });
    const chunks = result.output.filter((item) => item.type === "chunk");
    const holding = (text) => {
        const found = chunks.filter((chunk) => chunk.code.includes(text));
        assert.equal(found.length, 1, `one chunk holds ${text}`);
        assert.ok(!found[0].isEntry, `${text} is not part of the entry`);
        return "/" + found[0].fileName;
    };
    const catalog = { en: holding("Devices online: {online}/{total}"), zh: holding("{online}/{total} 台设备在线") };
    assert.notEqual(catalog.en, catalog.zh);
    const files = new Map(result.output.map((item) => ["/" + item.fileName, item.type === "chunk" ? item.code : item.source]));
    server = http.createServer((req, res) => {
        const name = new URL(req.url, "http://localhost").pathname;
        const file = name === "/" ? "/index.html" : name;
        if (req.method !== "GET" || !files.has(file)) { res.writeHead(404); res.end(); return; }
        const type = file.endsWith(".js") ? "text/javascript" : file.endsWith(".css") ? "text/css" : file.endsWith(".html") ? "text/html" : "application/octet-stream";
        res.writeHead(200, { "content-type": type, "cache-control": "no-store" });
        res.end(files.get(file));
    });
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    const origin = `http://127.0.0.1:${server.address().port}`;
    browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
    const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
    const page = await context.newPage();
    page.setDefaultTimeout(10000);
    const errors = [], reads = { en: 0, zh: 0 };
    let hold = null;
    const failing = { en: false, zh: false };
    page.on("pageerror", (error) => errors.push(String(error)));
    await page.addInitScript(({ conversation }) => {
        if (!sessionStorage.getItem("locale-fixture")) { localStorage.setItem("steve.ui.locale", "en"); sessionStorage.setItem("locale-fixture", "1"); }
        sessionStorage.setItem("steve.conversation", conversation);
        window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
    }, { conversation });
    const status = { enabled: true, node_id: "fixture-node", setup_required: true, agent_count: 0, setup: { step: "preferences", done: false } };
    await page.route("**/*", async (route) => {
        const req = route.request(), url = new URL(req.url()), p = url.pathname;
        if (url.origin !== origin) { errors.push(`External request: ${url.origin}`); return route.abort(); }
        for (const locale of ["en", "zh"]) if (p === catalog[locale]) {
            reads[locale]++;
            if (failing[locale]) { failing[locale] = false; return route.abort("failed"); }
            if (locale === "zh" && hold) await hold.promise;
            return route.continue();
        }
        if (!p.startsWith("/console/") && !["/state", "/events", "/history"].includes(p)) return route.continue();
        if (req.method() !== "GET") { errors.push(`Unexpected write: ${req.method()} ${p}`); return route.abort(); }
        let json;
        if (p === "/state") json = workState({ at, hub: { node: "fixture-node", version: "fixture", started: at }, nodes: [{ name: "fixture-node", role: "hub", up: true }], agents: [], projects: [], tasks: [], plans: [], attempts: [], landings: [] });
        else if (p === "/console/coordination") json = { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false };
        else if (p === "/console/desktop") json = status;
        else if (p === "/console/context") json = { enabled: true, context: { conversation, agents: [] } };
        else if (p === "/console/conversations") json = { conversations: [{ id: conversation, title: "Locale fixture", count: 1, last_at: at, running: false }] };
        else if (p === "/console/replies") json = { enabled: true, replies: [{ id: "reply", conversation, kind: "reply", text: "Retained fixture history", at }] };
        else if (p === "/console/queue") json = { queue: [], submission_keys: true };
        else if (p === "/console/verbs" || p === "/console/suggest") json = { verbs: [], suggestions: [] };
        else if (p === "/console/questions") json = { questions: [] };
        else { errors.push(`Unhandled API: ${p}`); return route.fulfill({ status: 500, json: { error: "Unmocked API" } }); }
        return route.fulfill({ json });
    });
    const paint = () => page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));

    await page.goto(origin + "/#/console", { waitUntil: "domcontentloaded" });
    const dialog = page.getByRole("dialog");
    const language = dialog.getByRole("radiogroup", { name: "Language", exact: true });
    await language.waitFor();
    assert.deepEqual(reads, { en: 1, zh: 0 }, "the first paint fetches only the chosen language");
    assert.equal(await page.evaluate(() => document.documentElement.lang), "en");
    await page.evaluate(() => {
        window.keptDialog = document.querySelector('[role="dialog"]');
        window.keptNav = document.querySelector("nav");
        window.lostContent = 0;
        new MutationObserver(() => { if (!window.keptDialog.isConnected || !window.keptNav.isConnected) window.lostContent++; }).observe(document.body, { childList: true, subtree: true });
    });

    const fetched = async (count) => {
        for (let i = 0; i < 200 && reads.zh < count; i++) await new Promise((resolve) => setTimeout(resolve, 25));
        await paint(); await paint();
        assert.equal(reads.zh, count, "switching asks for the new language");
    };
    hold = Promise.withResolvers();
    await language.getByText("中文", { exact: true }).click();
    await fetched(1);
    assert.equal(await language.isVisible(), true, "the page keeps its current language while the new one loads");
    assert.equal(await page.evaluate(() => document.documentElement.lang), "en");
    assert.equal(await page.getByText("Loading…", { exact: true }).count(), 0, "a switch shows no loading fallback");
    hold.resolve();
    await dialog.getByRole("radiogroup", { name: "界面语言", exact: true }).waitFor();
    assert.equal(await page.evaluate(() => document.documentElement.lang), "zh-CN");
    assert.equal(await page.evaluate(() => window.lostContent), 0, "the dialog and navigation stay mounted through the switch");

    await dialog.getByRole("radiogroup", { name: "界面语言", exact: true }).getByText("English", { exact: true }).click();
    await language.waitFor();
    assert.deepEqual(reads, { en: 1, zh: 1 }, "a language already loaded is not fetched again");

    await page.reload({ waitUntil: "domcontentloaded" });
    await language.waitFor();
    await page.evaluate(() => localStorage.setItem("steve.ui.locale", "zh"));
    await page.reload({ waitUntil: "domcontentloaded" });
    await dialog.getByRole("radiogroup", { name: "界面语言", exact: true }).waitFor();
    assert.deepEqual(reads, { en: 2, zh: 2 }, "a saved language is the only one fetched after a reload");

    // The browser keeps a failed module import for the life of the page.
    await page.evaluate(() => localStorage.setItem("steve.ui.locale", "en"));
    await page.reload({ waitUntil: "domcontentloaded" });
    await language.waitFor();
    failing.zh = true;
    await language.getByText("中文", { exact: true }).click();
    await fetched(3);
    assert.equal(await language.isVisible(), true, "messages that cannot be fetched leave the current language in place");
    assert.equal(await page.evaluate(() => document.documentElement.lang), "en");
    assert.equal(await page.evaluate(() => localStorage.getItem("steve.ui.locale")), "en", "a switch that did not happen is not saved");
    await page.getByRole("status").filter({ hasText: "The selected language could not be loaded" }).waitFor();
    // The notice dismisses itself; the settings dialog keeps its close button out of reach.
    await page.getByRole("status").filter({ hasText: "could not be loaded" }).waitFor({ state: "detached" });

    // A language chosen in another window is not one this page selected:
    // its failure says where the change came from.
    await page.evaluate(() => window.dispatchEvent(new StorageEvent("storage", { key: "steve.ui.locale", oldValue: "en", newValue: "zh", storageArea: localStorage })));
    await page.getByRole("status").filter({ hasText: "changed in another window or in the browser" }).waitFor();
    assert.equal(await page.getByRole("status").filter({ hasText: "The selected language" }).count(), 0, "a change from elsewhere is not called the selected language");
    assert.equal(await page.evaluate(() => document.documentElement.lang), "en");

    // The first paint cannot fall back to an old language: it says so in
    // both, without the messages, and offers to load the page again.
    failing.en = true;
    await page.reload({ waitUntil: "domcontentloaded" });
    const failure = page.getByRole("alert");
    await failure.waitFor();
    assert.match(await failure.innerText(), /Steve could not load/);
    assert.match(await failure.innerText(), /无法加载/);
    assert.equal(reads.en, 4, "the failed messages are asked for once, not retried in a loop");
    await failure.getByRole("button", { name: /Reload/ }).click();
    await language.waitFor();
    assert.equal(await page.evaluate(() => document.documentElement.lang), "en");
    assert.deepEqual(errors, []);
    console.log("PASS first paint fetches one language; a failed first paint offers a reload; a failed switch keeps the page and names where it came from; switching keeps the page mounted and in its old language until the new one arrives");
    await context.close();
} finally {
    await browser?.close();
    if (server) await new Promise((resolve) => server.close(resolve));
    await rm(scratch, { recursive: true, force: true });
}
