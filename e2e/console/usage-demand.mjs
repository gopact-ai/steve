// Fully isolated browser contract test: static assets only, every API mocked,
// no hub proxy, real configuration, agent process, or persistent browser profile.
// USAGE_DIST can point to a Vite build outside the repository.
import assert from "node:assert/strict";
import http from "node:http";
import { readFile, mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";
import os from "node:os";
import { setTimeout as delay } from "node:timers/promises";
import { usageResponse, usageState } from "./usage-fixture.mjs";

const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || new URL("../../web/console/node_modules/playwright/index.mjs", import.meta.url).href);
const dist = process.env.USAGE_DIST || fileURLToPath(new URL("../../internal/readmodel/web/dist", import.meta.url));
const output = process.env.OUTPUT_DIR || path.join(os.tmpdir(), "steve-usage-demand");
await mkdir(output, { recursive: true });
const mime = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".png": "image/png", ".svg": "image/svg+xml", ".woff2": "font/woff2" };
const server = http.createServer(async (req, res) => {
    const file = path.resolve(dist, "." + (req.url === "/" ? "/index.html" : new URL(req.url, "http://local").pathname));
    if (!file.startsWith(path.resolve(dist) + path.sep)) { res.writeHead(403).end(); return; }
    try { res.setHeader("Content-Type", mime[path.extname(file)] || "application/octet-stream"); res.end(await readFile(file)); }
    catch { res.writeHead(404).end(); }
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const origin = `http://127.0.0.1:${server.address().port}`;
const browser = await chromium.launch({ headless: true });
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
const page = await context.newPage();
const errors = [], writes = [], counts = { state: 0, usage: 0 };
let response = usageResponse(), status = 200, hold = null, active = 0, maxActive = 0;
const gate = () => { let release; const promise = new Promise((resolve) => { release = resolve; }); return { promise, release }; };
const eventually = async (check, label) => {
    for (let i = 0; i < 150; i++) { if (await check()) return; await delay(20); }
    assert.fail(label);
};
const advance = async () => { await page.clock.runFor(600); await delay(150); };
const navigate = async (hash) => { await page.evaluate((hash) => { location.hash = hash; }, hash); await advance(); };
const emit = async (...events) => { await page.evaluate((events) => events.forEach((event) => window.emit(event)), events.map((event) => ({ at: "2026-09-19T12:00:00Z", ...event }))); await advance(); };
const summary = () => page.getByRole("region", { name: "区间用量汇总", exact: true });
page.on("pageerror", (error) => errors.push(error.message));
await page.clock.install();
await page.addInitScript(() => {
    localStorage.setItem("steve.ui.locale", "zh");
    window.sources = [];
    window.EventSource = class {
        constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); }
        close() { window.sources = window.sources.filter((item) => item !== this); }
    };
    window.emit = (event) => { for (const source of window.sources) source.onmessage?.({ data: JSON.stringify(event) }); };
});
await page.route("**/*", async (route) => {
    const request = route.request(), url = new URL(request.url());
    if (url.origin !== origin) { errors.push(`Unexpected origin: ${url.origin}`); return route.abort(); }
    if (url.pathname === "/" || url.pathname.startsWith("/assets/")) return route.continue();
    if (request.method() !== "GET") { writes.push(url.pathname); return route.fulfill({ status: 405 }); }
    if (url.pathname === "/state") {
        counts.state++;
        return route.fulfill({ json: { ...usageState(), at: new Date(Date.UTC(2026, 8, 19, 12, 0, counts.state)).toISOString() } });
    }
    if (url.pathname === "/usage") {
        counts.usage++; active++; maxActive = Math.max(maxActive, active);
        const data = structuredClone(response), code = status;
        try { if (hold) await hold.promise; await route.fulfill({ status: code, json: data }); }
        finally { active--; }
        return;
    }
    const fixtures = {
        "/console/desktop": { enabled: false, setup_required: false, agent_count: 0 },
        "/console/coordination": { enabled: false, nodes: [], events: [], ready: false },
        "/console/conversations": { enabled: true, conversations: [] },
        "/console/queue": { submission_keys: true, queue: [] },
        "/console/questions": { questions: [] },
        "/console/replies": { enabled: true, replies: [] },
        "/console/context": { enabled: true, context: { agents: [] } },
        "/console/setup": { enabled: false },
        "/console/verbs": { verbs: [] },
        "/console/suggest": { suggestions: [] },
        "/history": { entries: [], next: "" },
    };
    if (url.pathname in fixtures) return route.fulfill({ json: fixtures[url.pathname] });
    errors.push(`Unmocked API: ${url.pathname}`);
    return route.fulfill({ status: 500, json: { error: "Unmocked test API" } });
});

try {
    await page.goto(`${origin}/#/dashboard?tab=audit`);
    await advance();
    await eventually(() => counts.state > 0, "state must load");
    assert.equal(counts.usage, 0, "Audit/fleet state must not request usage");

    hold = gate();
    await navigate("/dashboard?tab=overview&range=1d");
    await eventually(() => counts.usage === 1, "Opening usage must request its independent endpoint");
    await page.getByText("读取用量数据…", { exact: true }).waitFor();
    assert.equal(await summary().count(), 0, "Loading cannot show zero usage");
    hold.release(); hold = null;
    await summary().waitFor();
    const first = counts.usage;
    await page.clock.runFor(30000); await advance();
    assert.ok(counts.state > 1, "Fleet polling must still run");
    assert.equal(counts.usage, first, "Changing snap.at must not refetch usage");
    await emit({ kind: "observe.node.up" }, { kind: "console.progress", progress: { answer: "stream" } }, { kind: "delegate.progress", step: { state: "running" } });
    assert.equal(counts.usage, first, "Unrelated domains and streaming fragments cannot refetch usage");
    await page.getByRole("button", { name: "30d", exact: true }).click();
    await advance();
    assert.equal(counts.usage, first, "Range selection uses the same response");

    await emit(...Array.from({ length: 30 }, () => ({ kind: "task.changed" })), { kind: "observe.node.up" });
    await eventually(() => counts.usage === first + 1, "Task events behind an unrelated newest event must coalesce into one read");
    await emit({ kind: "console.reply" });
    await eventually(() => counts.usage === first + 2, "Closed console execution must invalidate usage");
    await emit({ kind: "delegate.progress", step: { state: "done" } });
    await eventually(() => counts.usage === first + 3, "Completed delegated execution must invalidate usage");
    console.log("PASS demand-only reads, no snap.at dependency, domain filtering, burst coalescing");

    hold = gate();
    await emit({ kind: "task.changed" });
    await eventually(() => active === 1, "hold one usage request");
    const during = counts.usage;
    await emit(...Array.from({ length: 10 }, () => ({ kind: "task.changed" })));
    assert.equal(counts.usage, during, "Invalidation during a read must not overlap it");
    hold.release(); hold = null;
    await eventually(() => counts.usage === during + 1 && active === 0, "Pending invalidations must cause exactly one follow-up");
    assert.equal(maxActive, 1, "Usage requests are serialized");

    response = { at: "2026-09-19T12:00:00Z", sources: [{ name: "ledger-usage", wired: true, error: "usage source unavailable" }] };
    await emit({ kind: "task.changed" });
    await page.getByRole("alert").filter({ hasText: "usage source unavailable" }).waitFor();
    assert.equal(await summary().count(), 0, "Source failure must not display old or zero totals");
    response = { ...usageResponse(), sources: [{ name: "ledger-usage", wired: true, error: "partial usage" }] };
    await page.getByRole("button", { name: "重试", exact: true }).click();
    await page.getByRole("alert").filter({ hasText: "partial usage" }).waitFor();
    assert.equal(await summary().count(), 0, "Partial data cannot masquerade as a complete total");
    status = 503; response = { error: "transport failed" };
    await emit({ kind: "attempt.closed" });
    await page.getByRole("alert").filter({ hasText: "transport failed" }).waitFor();
    status = 200; response = usageResponse();
    await page.getByRole("button", { name: "重试", exact: true }).focus();
    await page.keyboard.press("Enter");
    await summary().waitFor();
    response = { at: "2026-09-19T12:00:00Z", sources: [{ name: "ledger-usage", wired: false }] };
    await emit({ kind: "task.changed" });
    await page.getByText("用量数据源尚未接入。", { exact: true }).waitFor();
    assert.equal(await summary().count(), 0);
    response = { at: "2026-09-19T12:00:00Z", sources: [{ name: "ledger-usage", wired: true }] };
    await emit({ kind: "task.changed" });
    await page.getByRole("alert").filter({ hasText: "Invalid usage response" }).waitFor();
    response = usageResponse(); await page.getByRole("button", { name: "重试", exact: true }).click(); await summary().waitFor();
    await page.setViewportSize({ width: 390, height: 844 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1), false);
    await page.screenshot({ path: path.join(output, "usage-narrow.png"), fullPage: true });
    console.log("PASS serialized follow-up, source/partial/HTTP/unwired/malformed states and retry");

    const beforeReconnect = counts.usage;
    await page.evaluate(() => window.sources.forEach((source) => source.onerror?.()));
    await page.clock.runFor(3500); await advance();
    await eventually(() => counts.usage === beforeReconnect + 1, "Reconnect must invalidate missed usage events");
    await navigate("/dashboard?tab=audit");
    const beforeAway = counts.usage;
    await emit({ kind: "task.changed" }, { kind: "console.reply" });
    assert.equal(counts.usage, beforeAway, "Unmounted usage must not read history");

    await navigate("/console?view=board");
    await eventually(() => counts.usage === beforeAway + 1, "Board summary must request usage on demand");
    await page.getByRole("region", { name: "主任务统计", exact: true }).waitFor();
    response = { at: "2026-09-19T12:00:00Z", sources: [{ name: "ledger-usage", wired: true, error: "board usage unavailable" }] };
    await emit({ kind: "task.changed" });
    await page.getByRole("alert").filter({ hasText: "board usage unavailable" }).waitFor();
    assert.equal(await page.getByRole("region", { name: "主任务统计", exact: true }).innerText().then((text) => text.includes("未知")), true, "Unavailable board usage must not be zero");
    await page.screenshot({ path: path.join(output, "board-usage-error.png"), fullPage: true });
    response = usageResponse();
    await page.getByRole("button", { name: "重试", exact: true }).click();
    await eventually(async () => (await page.getByRole("region", { name: "主任务统计", exact: true }).innerText()).includes("tok"), "Board retry restores actual usage");
    const beforeBoardPoll = counts.usage;
    await page.clock.runFor(10000); await advance();
    assert.equal(counts.usage, beforeBoardPoll, "Board usage must not follow state polling either");
    console.log("PASS reconnect, unmount and board independent usage/error state");
    assert.deepEqual(errors, [], "No browser errors or unmocked network access");
    assert.deepEqual(writes, [], "Usage interactions never submit work");
    console.log(`PASS usage-demand; reads=${JSON.stringify(counts)}; screenshots=${output}`);
} catch (error) {
    await page.screenshot({ path: path.join(output, "failure.png"), fullPage: true });
    throw error;
} finally {
    hold?.release();
    await context.close(); await browser.close();
    server.closeAllConnections(); server.close();
}
