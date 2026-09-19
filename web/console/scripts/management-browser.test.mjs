// Exercise the real management pages and route imports without a running hub.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";

const server = await createServer({ root: fileURLToPath(new URL("..", import.meta.url)), logLevel: "error", server: { host: "127.0.0.1", port: 0 } });
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
const page = await context.newPage();
page.setDefaultTimeout(10000);
await page.clock.install();
const counts = { skills: 0, mcp: 0, plugins: 0 }, errors = [], modules = new Set();
let observation = 1, online = true, readError = false, mutations = 0, enabled = true;
const at = (n) => `2026-09-19T00:00:${String(n).padStart(2, "0")}Z`;
page.on("pageerror", (error) => errors.push(String(error)));
page.on("request", (request) => { if (request.url().includes("/src/pages/")) modules.add(new URL(request.url()).pathname); });
await page.addInitScript(() => {
    localStorage.setItem("steve.ui.locale", "en");
    window.sources = [];
    window.EventSource = class {
        constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); }
        close() { window.sources = window.sources.filter((source) => source !== this); }
    };
    window.emit = (event) => window.sources.forEach((source) => source.onmessage?.({ data: JSON.stringify(event) }));
});
await context.route("**/*", async (route) => {
    const request = route.request(), url = new URL(request.url()), p = url.pathname;
    if (url.origin !== origin) { errors.push(`External request: ${url.origin}`); return route.abort(); }
    if (!p.startsWith("/console/") && !["/state", "/events", "/history"].includes(p)) return route.continue();
    if (p === "/console/skills/review" && request.method() === "PUT") {
        enabled = request.postDataJSON().enabled; mutations++;
        return route.fulfill({ json: { ok: true } });
    }
    if (request.method() !== "GET") { errors.push(`Unexpected write: ${p}`); return route.abort(); }
    if (p === "/state") return route.fulfill({ json: {
        at: at(observation), hub: { node: "fixture", started: at(0) },
        nodes: [{ name: "fixture", up: online, health: { at: at(observation), load1: observation } }],
        agents: [], tasks: [], projects: [], plans: [], attempts: [], landings: [],
    } });
    if (p === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [] } });
    if (p === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false } });
    if (p === "/console/queue") return route.fulfill({ json: { submission_keys: true, queue: [] } });
    const domain = p.slice("/console/".length);
    if (Object.hasOwn(counts, domain)) {
        counts[domain]++;
        if (readError) return route.fulfill({ status: 503, json: { error: "Fixture read unavailable" } });
        const views = {
            skills: { fingerprint: "owner-fingerprint", search_paths: [], sources: [], nodes: [], skills: [
                { name: "review", path: "/fixture/review", title: "Fixture skill", enabled, builtin: true, description: "Fixture", agents: [], projects: [] },
            ] },
            mcp: { deployments: [], machines: [], platform: [] },
            plugins: { revision: "owner-revision", packages: [], installations: [], operations: [] },
        };
        return route.fulfill({ json: views[domain] });
    }
    return route.fulfill({ json: { enabled: true, machines: [], conversations: [], replies: [], questions: [], verbs: [], suggestions: [], entries: [], next: "" } });
});
const settle = async () => { await page.clock.runFor(500); await new Promise((resolve) => setTimeout(resolve, 100)); };
async function waitReads(domain, before) {
    for (let i = 0; i < 60; i++) { if (counts[domain] > before) { await settle(); return; } await settle(); }
    assert.fail(`${domain} did not refresh after its dependency changed`);
}
try {
    await page.goto(`${origin}/#/console`);
    await page.locator(".console-workspace").waitFor().catch(() => page.locator(".app-main").waitFor());
    await settle();
    for (const route of ["dashboard", "fleet", "settings", "skills", "mcp", "plugins"]) {
        assert.equal(modules.has(`/src/pages/${route}.tsx`), false, `${route} not requested on Console`);
    }
    for (const domain of ["skills", "mcp", "plugins"]) {
        await page.evaluate((domain) => { location.hash = `/${domain}`; }, domain);
        await waitReads(domain, 0);
        assert.ok(modules.has(`/src/pages/${domain}.tsx`), `${domain} imported on navigation`);
        let before = counts[domain];
        observation++;
        await page.evaluate((at) => window.emit({ at, kind: "observe.task.idle", text: "unrelated" }), at(observation));
        await settle();
        assert.equal(counts[domain], before, `${domain}: task traffic and snapshot clock do not reread`);
        // The existing ten-second fleet poll also must not drive management I/O.
        await page.clock.runFor(10000); await settle();
        assert.equal(counts[domain], before, `${domain}: fleet recovery poll does not invalidate`);
        online = !online;
        await page.evaluate((at) => window.emit({ at, kind: "observe.node.down" }), at(++observation));
        await waitReads(domain, before);
        if (domain !== "plugins") {
            before = counts[domain];
            const event = domain === "skills"
                ? { at: at(++observation), kind: "observe.node.skills", data: { hash: "changed" } }
                : { at: at(++observation), kind: "observe.node.manifest", data: { changes: "mcp:search\t\tavailable" } };
            await page.evaluate((event) => window.emit(event), event);
            await waitReads(domain, before);
            before = counts[domain];
            await page.evaluate((event) => window.emit(event), event); await settle();
            assert.equal(counts[domain], before, "duplicate domain observations are not new revisions");
        }
        if (domain === "skills") {
            before = counts.skills;
            await page.getByRole("switch", { name: /review/ }).focus();
            await page.keyboard.press("Space");
            await waitReads(domain, before);
            assert.equal(mutations, 1, "mutation explicitly reloads its own resource");
        }
        before = counts[domain];
        readError = true;
        await page.clock.runFor(30000); await settle();
        assert.ok(counts[domain] > before, "bounded page-local recovery remains");
        await page.getByRole("alert").filter({ hasText: "Fixture read unavailable" }).waitFor();
        readError = false; before = counts[domain];
        await page.evaluate(() => window.sources.forEach((source) => source.onerror?.()));
        await page.clock.runFor(3100); await settle();
        await waitReads(domain, before);
        await page.getByRole("alert").filter({ hasText: "Fixture read unavailable" }).waitFor({ state: "hidden" });
        await page.setViewportSize({ width: 390, height: 844 });
        await page.keyboard.press("Tab");
        assert.equal(await page.locator(".app-main").evaluate((el) => el.scrollWidth <= el.clientWidth), true);
        if (process.env.PHASE4_SCREENSHOTS) {
            await mkdir(process.env.PHASE4_SCREENSHOTS, { recursive: true });
            await page.screenshot({ path: path.join(process.env.PHASE4_SCREENSHOTS, `${domain}-390.png`), fullPage: true });
        }
        await page.setViewportSize({ width: 1440, height: 1000 });
    }
    assert.deepEqual(errors, []);
    console.log("Management domains: no snapshot-clock reads; topology/event/mutation/recovery and lazy navigation passed", counts);
} finally {
    await context.close(); await browser.close(); await server.close();
}
