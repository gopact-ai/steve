// Production React consumers, isolated storage and fixture-only network.
import assert from "node:assert/strict";
import { mkdtemp, writeFile, readFile, rm } from "node:fs/promises";
import http from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { build } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";
import ts from "../node_modules/typescript/lib/typescript.js";
import { workState } from "../../../e2e/console/work-fixture.mjs";

const web = fileURLToPath(new URL("..", import.meta.url));
const scratch = await mkdtemp(path.join(tmpdir(), "steve-fleet-render-"));
let server, browser;
try {
    await writeFile(path.join(scratch, "index.html"), '<div id="root"></div><script type="module" src="/entry.tsx"></script>');
    await writeFile(path.join(scratch, "entry.tsx"), `
        import { memo } from "react";
        import { createRoot } from "react-dom/client";
        import { flushSync } from "react-dom";
        import { MemoryRouter } from "react-router";
        import { FleetProvider, IntentProvider, useFleet } from "@/lib/fleet";
        import { CoordinationProvider, useCoordination } from "@/lib/coordination";
        import { MaterialProvider, useMaterial } from "@/providers/material-provider";
        import { LocaleProvider } from "@/providers/locale-provider";
        import { BoardPage } from "@/pages/board";
        const hit = key => window.renders[key] = (window.renders[key] || 0) + 1;
        const Snapshot = memo(function Snapshot() { const { snap } = useFleet(); hit("snapshot"); return <output id="ready">{snap.hub.node}</output>; });
        const Connection = memo(function Connection() { const { live } = useFleet(); hit("connection"); return <output id="connection">{live}</output>; });
        const Actions = memo(function Actions() { const { refresh } = useFleet(); hit("actions"); window.refreshFleet = refresh; return null; });
        const Material = memo(function Material() { const value = useMaterial(); hit("material"); window.material = value; return <output id="pins">{value.pins("p").map(pin => pin.title).join(",")}</output>; });
        const Coordination = memo(function Coordination() { const value = useCoordination(); hit("coordination"); window.coordination = value; return <output id="revision">{value.view?.revision}</output>; });
        window.emit = event => flushSync(() => window.source.onmessage({ data: JSON.stringify(event) }));
        createRoot(document.getElementById("root")).render(
            <LocaleProvider><MemoryRouter><FleetProvider><CoordinationProvider><MaterialProvider><IntentProvider onNavigate={() => {}}>
                <Snapshot /><Connection /><Actions /><Material /><Coordination /><BoardPage />
            </IntentProvider></MaterialProvider></CoordinationProvider></FleetProvider></MemoryRouter></LocaleProvider>
        );
    `);
    await build({
        root: scratch, configFile: false, envDir: false, cacheDir: path.join(scratch, "cache"), logLevel: "error",
        resolve: { alias: { "@": path.join(web, "src"), "react-router": path.join(web, "node_modules/react-router"), "react-dom": path.join(web, "node_modules/react-dom"), "react": path.join(web, "node_modules/react") } },
        plugins: [{
            name: "count-real-board-cards", enforce: "pre",
            transform(source, id) {
                if (!id.endsWith("/src/pages/board.tsx")) return;
                const ast = ts.createSourceFile(id, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
                const card = ast.statements.find((node) => ts.isFunctionDeclaration(node) && node.name?.text === "Card");
                assert.ok(card?.body, "Instrument the real Card, not a stand-in");
                const at = card.body.getStart(ast) + 1;
                return source.slice(0, at) + 'window.renders.card = (window.renders.card || 0) + 1;' + source.slice(at);
            },
        }],
        esbuild: { jsx: "automatic" },
        build: { outDir: path.join(scratch, "dist"), minify: true, reportCompressedSize: false, chunkSizeWarningLimit: 10000 },
    });
    server = http.createServer(async (req, res) => {
        const url = new URL(req.url, "http://fixture");
        if (url.pathname !== "/" && !url.pathname.startsWith("/assets/")) { res.writeHead(404).end(); return; }
        try {
            const file = path.join(scratch, "dist", url.pathname === "/" ? "index.html" : url.pathname);
            res.setHeader("Content-Type", file.endsWith(".js") ? "application/javascript" : "text/html");
            res.end(await readFile(file));
        } catch { res.writeHead(404).end(); }
    });
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    const origin = `http://127.0.0.1:${server.address().port}`;
    browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
    const context = await browser.newContext({ serviceWorkers: "block" });
    const page = await context.newPage();
    const errors = [], reads = { state: 0, coordination: 0, usage: 0 };
    const at = "2026-09-20T00:00:00Z";
    let hub = "first", revision = 1;
    const tasks = Array.from({ length: 200 }, (_, i) => ({
        id: String(i), transport: "console", channel: "fixture", goal: `Task ${i}`,
        state: "running", lifecycle: "running", execution: "running", lane: "running",
        attention: 0, turns: 0, max_turns: 0,
    }));
    page.on("pageerror", (error) => errors.push(String(error)));
    await page.clock.install();
    await page.addInitScript(() => {
        window.renders = {};
        localStorage.setItem("steve.ui.locale", "en");
        window.EventSource = class {
            constructor() { window.source = this; setTimeout(() => this.onopen?.(), 0); }
            close() {}
        };
        for (const hub of ["first", "second"]) {
            localStorage.setItem(`steve.material.pins:${location.origin}:${hub}:p`,
                JSON.stringify([{ id: hub, title: hub, project: "p", kind: "text", mime: "text/plain", size: 1 }]));
        }
    });
    await context.route("**/*", (route) => {
        const request = route.request(), url = new URL(request.url());
        if (url.origin !== origin || request.method() !== "GET") {
            errors.push(`Unexpected request: ${request.method()} ${url.pathname}`); return route.abort();
        }
        if (url.pathname === "/" || url.pathname.startsWith("/assets/")) return route.continue();
        if (url.pathname === "/state") {
            reads.state++;
            return route.fulfill({ json: workState({ at, hub: { node: hub, started: at }, nodes: [], agents: [], tasks, plans: [], projects: [], attempts: [], landings: [] }) });
        }
        if (url.pathname === "/console/coordination") {
            reads.coordination++;
            return route.fulfill({ json: { enabled: false, revision, nodes: [], events: [] } });
        }
        if (url.pathname === "/usage") {
            reads.usage++;
            return route.fulfill({ json: { at, sources: [{ name: "ledger-usage", wired: false }] } });
        }
        errors.push(`Unknown fixture request: ${url.pathname}`); return route.abort();
    });
    const settle = async () => { await page.clock.runFor(500); await page.waitForFunction(() => document.querySelector("#ready")?.textContent); };
    const reset = () => page.evaluate(() => { window.renders = {}; });
    const counts = () => page.evaluate(() => window.renders);
    await page.goto(origin);
    await settle();
    await page.waitForFunction(() => document.querySelector("#revision")?.textContent === "1");
    await page.waitForFunction(() => document.querySelector("#pins")?.textContent === "first");
    assert.equal(await page.locator(".workbench-task-card").count(), 200);
    await reset();
    const before = { ...reads };
    for (let i = 0; i < 10; i++) await page.evaluate((i) => window.emit({ kind: "observe.agent.idle", at: String(i) }), i);
    const eventOnly = await counts();
    console.log("10 unrelated events before snapshot refresh:", JSON.stringify({ renders: eventOnly, reads }));
    for (const key of ["snapshot", "connection", "actions", "material", "coordination", "card"]) {
        assert.equal(eventOnly[key] || 0, 0, `${key} must not render for unrelated events`);
    }
    assert.deepEqual(reads, before, "Events still debounce the existing state request");

    await settle();
    assert.equal(reads.state, before.state + 1, "The existing 250ms snapshot refresh still runs");
    assert.equal(reads.usage, before.usage, "Unrelated events do not invalidate usage");
    assert.equal((await counts()).material || 0, 0, "An unchanged hub node does not republish the material context");
    assert.equal((await counts()).coordination || 0, 0, "An unchanged coordination view does not republish");
    await reset();
    const streaming = { ...reads };
    for (let i = 0; i < 10; i++) await page.evaluate((i) => window.emit({ kind: "step.progress", at: String(i), step: { state: "running" } }), i);
    await settle();
    assert.deepEqual(reads, streaming, "Streaming progress does not reread fleet, coordination or usage");
    assert.equal((await counts()).card || 0, 0, "Streaming progress does not redraw the board");
    await reset();
    await page.evaluate(() => window.emit({ kind: "task.updated", at: "task" }));
    await settle();
    assert.equal(reads.usage, before.usage + 1, "Relevant usage invalidation is retained");
    revision++;
    await page.evaluate(() => window.emit({ kind: "node.updated", at: "node" }));
    await settle();
    await page.waitForFunction(() => document.querySelector("#revision")?.textContent === "2");
    assert.ok((await counts()).coordination > 0, "A real coordination update reaches its consumers");

    hub = "second";
    await page.evaluate(() => window.refreshFleet());
    await settle();
    await page.waitForFunction(() => document.querySelector("#pins")?.textContent === "second");
    await page.evaluate(() => window.material.unpin("p", { id: "second" }));
    await page.waitForFunction(() => document.querySelector("#pins")?.textContent === "");
    assert.equal(await page.evaluate(() => JSON.parse(localStorage.getItem(`steve.material.pins:${location.origin}:first:p`)).length), 1, "Stable actions use the latest hub, not the initial closure");

    const reconnect = { ...reads };
    await page.evaluate(() => window.source.onerror());
    await page.waitForFunction(() => document.querySelector("#connection")?.textContent === "reconnecting");
    await page.clock.runFor(3100);
    await settle();
    await page.waitForFunction(() => document.querySelector("#connection")?.textContent === "live");
    assert.ok(reads.state > reconnect.state && reads.coordination > reconnect.coordination && reads.usage > reconnect.usage, "Reconnect recovery is retained");

    // Settle the reconnect burst, then measure the floor alone.
    await page.clock.runFor(1000);
    const quiet = reads.state;
    await page.clock.runFor(59_000);
    assert.equal(reads.state, quiet, "A live stream does not re-read /state on the 10s floor");
    await page.clock.runFor(1500);
    assert.equal(reads.state, quiet + 1, "A live stream still bounds a silent drop with a long floor");
    await page.evaluate(() => window.source.onerror());
    await page.waitForFunction(() => document.querySelector("#connection")?.textContent === "reconnecting");
    // Hold the stream down: the retry fails again as soon as it opens.
    await page.evaluate(() => { window.EventSource = class { constructor() { window.source = this; setTimeout(() => this.onerror?.(), 0); } close() {} }; });
    const down = reads.state;
    await page.clock.runFor(20_500);
    assert.equal(reads.state, down + 2, "A dropped stream falls back to the 10s floor");
    const setVisible = (visible) => page.evaluate((visible) => {
        Object.defineProperty(document, "visibilityState", { configurable: true, get: () => visible ? "visible" : "hidden" });
        Object.defineProperty(document, "hidden", { configurable: true, get: () => !visible });
        document.dispatchEvent(new Event("visibilitychange"));
    }, visible);
    await setVisible(false);
    const hidden = reads.state;
    await page.clock.runFor(120_000);
    assert.equal(reads.state, hidden, "A hidden page does not poll /state");
    await setVisible(true);
    await page.waitForFunction(() => document.querySelector("#ready")?.textContent);
    await page.clock.runFor(100);
    assert.equal(reads.state, hidden + 1, "Showing the page refreshes /state at once");
    assert.deepEqual(errors, []);
    console.log("Fleet read surfaces: event-only renders 0; state, usage, node.updated, hub pins and reconnect recovery passed");
    await context.close();
} finally {
    await browser?.close();
    if (server) { server.closeAllConnections(); await new Promise((resolve) => server.close(resolve)); }
    await rm(scratch, { recursive: true, force: true });
}
