// Isolated Vite + mocked APIs only: never connects to a hub or runs an agent.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";

const app = await createServer({ root: fileURLToPath(new URL("../../web/console", import.meta.url)), server: { host: "127.0.0.1", port: 0 }, logLevel: "error" });
await app.listen();
const origin = `http://127.0.0.1:${app.httpServer.address().port}`;
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const at = "2026-09-19T10:00:00Z";
const entry = (text) => ({ at, kind: "observe.fixture", text });
const long = `Last record ${"long-observation-".repeat(20)}`;
const cursors = ["opaque+/=&?", "another_opaque-cursor"];
const gate = () => { let resolve; const promise = new Promise((done) => { resolve = done; }); return { promise, resolve }; };
async function eventually(check, label) { for (let i = 0; i < 100; i++) { if (await check()) return; await new Promise((resolve) => setTimeout(resolve, 20)); } assert.fail(label); }

try {
    const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
    const page = await context.newPage();
    page.setDefaultTimeout(10000);
    const f = { requests: [], errors: [], hold: null, status: 0, empty: false, refreshed: false };
    page.on("pageerror", (err) => f.errors.push(String(err)));
    await page.addInitScript(() => {
        localStorage.setItem("steve.ui.locale", "en");
        window.sources = [];
        window.EventSource = class { constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); } close() { window.sources = window.sources.filter((s) => s !== this); } };
        window.emit = (event) => window.sources.forEach((source) => source.onmessage?.({ data: JSON.stringify(event) }));
    });
    await page.route("**/*", async (route) => {
        const request = route.request(), url = new URL(request.url()), pathname = url.pathname;
        if (url.origin !== origin) { f.errors.push(`Unexpected external request: ${url.origin}`); return route.abort(); }
        if (!["/state", "/events", "/history"].includes(pathname) && !pathname.startsWith("/console/")) return route.continue();
        assert.equal(request.method(), "GET", "History tests must not write to any service");
        if (pathname === "/state") return route.fulfill({ json: { at, hub: { node: "fixture", version: "test", started: at }, nodes: [], agents: [], tasks: [], plans: [], projects: [], attempts: [], landings: [] } });
        if (pathname === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false } });
        if (pathname === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false, agent_count: 0 } });
        if (pathname === "/console/queue") return route.fulfill({ json: { submission_keys: true, queue: [] } });
        if (pathname === "/history") {
            const cursor = url.searchParams.get("cursor") || "";
            f.requests.push({ cursor, before: url.searchParams.has("before") });
            const status = f.status;
            const json = f.empty ? { entries: [], next: "" } : cursor === cursors[1] ? { entries: [entry(long)], next: "" } : cursor === cursors[0] ? { entries: [entry("Middle record")], next: cursors[1] } : { entries: [entry(f.refreshed ? "Refreshed record" : "First record")], next: cursors[0] };
            if (f.hold) { const hold = f.hold; f.hold = null; await hold.promise; }
            return route.fulfill(status ? { status, json: { error: status === 409 ? "History expired; reload" : "History unavailable" } } : { json }).catch(() => {});
        }
        f.errors.push(`Unexpected API: ${pathname}`);
        return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
    });
    await page.goto(`${origin}/#/dashboard?tab=timeline`);
    await page.getByText("First record", { exact: true }).waitFor();
    const screenshotDir = process.env.HISTORY_SCREENSHOTS;
    if (screenshotDir) { await mkdir(screenshotDir, { recursive: true }); await page.screenshot({ path: `${screenshotDir}/history-desktop.png`, fullPage: true }); }
    if (process.env.INSPECT_ONLY === "1") {
        await page.setViewportSize({ width: 390, height: 844 });
        if (screenshotDir) await page.screenshot({ path: `${screenshotDir}/history-narrow.png`, fullPage: true });
        console.log("PASS inspected current history UI with isolated fixture");
    } else {
        assert.equal(f.requests[0].before, false, "No legacy before query");
        const more = () => page.getByRole("button", { name: "Load earlier records", exact: true });
        // A held page must disable repeat actions synchronously and preserve
        // the prefix. Keyboard activation is the same pagination operation.
        const hold = gate(); f.hold = hold;
        const start = f.requests.length;
        await more().focus(); await page.keyboard.press("Enter");
        await eventually(() => f.requests.length === start + 1, "continuation request missing");
        await page.keyboard.press("Enter"); await page.keyboard.press("Enter");
        assert.equal(f.requests.length, start + 1, "Repeated activation duplicated the page");
        hold.resolve();
        await page.getByText("Middle record", { exact: true }).waitFor();
        assert.equal(f.requests.at(-1).cursor, cursors[0]);
        assert.equal(await page.getByText("First record", { exact: true }).count(), 1);
        await more().click();
        await page.getByText(long, { exact: true }).waitFor();
        assert.equal(f.requests.at(-1).cursor, cursors[1]);
        assert.equal(await page.getByRole("button", { name: "All records shown", exact: true }).isDisabled(), true);
        await page.setViewportSize({ width: 390, height: 844 });
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, "Long history must wrap without horizontal overflow");
        if (screenshotDir) await page.screenshot({ path: `${screenshotDir}/history-narrow-long.png`, fullPage: true });
        const emit = () => page.evaluate((at) => window.emit({ kind: "fixture.changed", at }), at);
        // A live refresh supersedes a held old continuation; the old page
        // must never append to the new cursor chain.
        await emit(); await page.getByText("First record", { exact: true }).waitFor();
        await eventually(async () => await page.getByText("Middle record", { exact: true }).count() === 0, "refresh did not replace the traversal");
        const stale = gate(); f.hold = stale;
        const prior = f.requests.length;
        await more().click();
        await eventually(() => f.requests.length > prior, "stale page not requested");
        f.refreshed = true; await emit();
        await page.getByText("Refreshed record", { exact: true }).waitFor();
        stale.resolve(); await page.waitForTimeout(100);
        assert.equal(await page.getByText("Middle record", { exact: true }).count(), 0, "Old page appended after refresh");
        // Transport errors retain both prefix and cursor for retry.
        f.status = 503; await more().click();
        await page.getByRole("alert").filter({ hasText: "History unavailable" }).waitFor();
        assert.equal(await page.getByText("Refreshed record", { exact: true }).count(), 1);
        f.status = 0; await page.getByRole("button", { name: "Retry", exact: true }).click();
        await page.getByText("Middle record", { exact: true }).waitFor();
        // Expired cursors explicitly restart; they are never treated as an
        // empty terminal page or appended to the old traversal.
        f.status = 409; await more().click();
        await page.getByRole("alert").filter({ hasText: "History expired" }).waitFor();
        f.status = 0; await page.getByRole("button", { name: "Retry", exact: true }).click();
        await eventually(() => f.requests.at(-1).cursor === "", "Expired cursor was retried instead of restarting");
        await eventually(async () => await page.getByText("Middle record", { exact: true }).count() === 0, "Expired retry did not replace prefix");
        // Many invalidations during one refresh coalesce rather than
        // aborting every read and starving the timeline under a busy stream.
        const refreshHold = gate(); f.hold = refreshHold;
        const refreshStart = f.requests.length;
        await emit();
        await eventually(() => f.requests.length === refreshStart + 1, "refresh not started");
        for (let i = 0; i < 6; i++) await emit();
        assert.equal(f.requests.length, refreshStart + 1, "Live events started overlapping refreshes");
        refreshHold.resolve();
        await eventually(() => f.requests.length === refreshStart + 2, "Refresh invalidations did not collapse into one follow-up");
        await eventually(async () => !(await more().isDisabled()), "Refresh follow-up did not complete");
        f.empty = true; await emit();
        await eventually(async () => await page.getByRole("button", { name: "All records shown", exact: true }).isDisabled(), "Empty page must terminate");
        assert.equal(await page.getByText("Refreshed record", { exact: true }).count(), 0);
        f.status = 503; await emit();
        await page.getByRole("alert").filter({ hasText: "History unavailable" }).waitFor();
        f.status = 0; await page.getByRole("button", { name: "Retry", exact: true }).click();
        await eventually(async () => await page.getByRole("button", { name: "All records shown", exact: true }).isDisabled(), "Empty first-page retry did not recover");
        assert.deepEqual(f.errors, []);
        console.log("PASS opaque paging, keyboard repeats, terminal/empty pages, stale refresh, retry/expiry and narrow long content");
    }
    await context.close();
} finally { await browser.close(); await app.close(); }
