import { workState } from "../../../e2e/console/work-fixture.mjs";
// Reading the coordination view probes every member of the cluster. The
// view is re-read every few seconds only while its panel is on screen;
// elsewhere a long floor keeps the shell's coordinator current.
import assert from "node:assert/strict";
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
const at = "2026-09-28T00:00:00Z", errors = [];
let reads = 0;
const node = (id, name) => ({ id, name, online: true, voter: true, auto_eligible: false, ready: true, local: id === "a" });
const view = { enabled: true, cluster_id: "cluster-one", node_id: "a", coordinator_id: "a", epoch: 1, revision: 1, authoritative: true, observed_at: at, auto_failover: false, ready: false, reason: "", nodes: [node("a", "laptop"), node("b", "dev-box")], events: [] };
page.on("pageerror", (error) => errors.push(String(error)));
await page.addInitScript(() => {
    localStorage.setItem("steve.ui.locale", "en");
    window.EventSource = class {
        addEventListener() {}
        constructor() { setTimeout(() => this.onopen?.(), 0); }
        close() {}
    };
});
await context.route("**/*", async (route) => {
    const request = route.request(), url = new URL(request.url()), p = url.pathname;
    if (url.origin !== origin) { errors.push(`External request: ${url.origin}`); return route.abort(); }
    if (!p.startsWith("/console/") && !["/state", "/events", "/history"].includes(p)) return route.continue();
    if (request.method() !== "GET") { errors.push(`Unexpected write: ${p}`); return route.abort(); }
    if (p === "/state") return route.fulfill({ json: workState({ at, hub: { node: "laptop", started: at }, nodes: [], agents: [], tasks: [], projects: [], plans: [], attempts: [], landings: [] }) });
    if (p === "/console/coordination") { reads++; return route.fulfill({ json: view }); }
    if (p === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false } });
    if (p === "/console/queue") return route.fulfill({ json: { submission_keys: true, queue: [] } });
    return route.fulfill({ json: { enabled: true, machines: [], conversations: [], replies: [], questions: [], verbs: [], suggestions: [], entries: [], next: "" } });
});
const settle = async () => { await page.clock.runFor(500); await new Promise((resolve) => setTimeout(resolve, 100)); };
// The clock advances a second at a time, so that each read completes before
// the next is due instead of being folded into the one still in flight.
async function readsDuring(ms) {
    const before = reads;
    for (let spent = 0; spent < ms; spent += 1000) { await page.clock.runFor(1000); await new Promise((resolve) => setTimeout(resolve, 20)); }
    await settle();
    return reads - before;
}
try {
    await page.goto(`${origin}/#/console`);
    await page.getByRole("link", { name: "Coordinated by laptop", exact: true }).first().waitFor({ state: "attached" });
    await settle();
    assert.equal(await readsDuring(50_000), 0, "away from the coordination panel the view is not re-read every few seconds");
    assert.ok(await readsDuring(15_000) >= 1, "away from the coordination panel a long floor still re-reads the view");

    await page.goto(`${origin}/#/fleet`);
    const panel = page.getByRole("region", { name: "Coordination and failover", exact: true });
    await panel.waitFor();
    await settle();
    const watched = await readsDuring(20_000);
    assert.ok(watched >= 3 && watched <= 5, `while the coordination panel is on screen the view is re-read every few seconds: ${watched} reads in 20s`);

    await page.getByRole("tab", { name: "Machines" }).click();
    await panel.waitFor({ state: "detached" });
    await settle();
    assert.equal(await readsDuring(50_000), 0, "once the panel is closed the view is no longer re-read every few seconds");
    assert.deepEqual(errors, []);
    console.log("Coordination view: re-read every few seconds only while its panel is on screen");
} finally {
    await context.close(); await browser.close(); await server.close();
}
