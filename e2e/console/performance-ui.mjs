// Production-browser regression. Only synthetic reads are allowed; no hub,
// config, agent, or live credentials are used. Timings are diagnostic, while
// deterministic work/behavior budgets gate CI independently of machine speed.
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import http from "node:http";
import { gzipSync } from "node:zlib";
import { fileURLToPath } from "node:url";
import { workState } from "./work-fixture.mjs";
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || new URL("../../web/console/node_modules/playwright/index.mjs", import.meta.url).href);
const dist = path.resolve(process.env.CONSOLE_PERF_DIST || fileURLToPath(new URL("../../internal/readmodel/web/dist", import.meta.url)));
const baseline = process.env.CONSOLE_PERF_BASELINE === "1";
const repetitions = Number(process.env.CONSOLE_PERF_REPETITIONS || 1);
const cpuRate = Number(process.env.CONSOLE_PERF_CPU_RATE || 4);
assert.ok(Number.isInteger(repetitions) && repetitions >= 1 && repetitions <= 20);
assert.ok(Number.isFinite(cpuRate) && cpuRate >= 1 && cpuRate <= 20);
const count = 100, A = "console:performance", B = "console:short", at = "2026-09-20T00:00:00Z";
const project = { id: "scratch", node: "fixture-node", path: "/fixture/scratch", repo: "inplace", level: "public", agents: [], workspaces: [] };
function markdown(prefix, paragraphs) {
    return `## ${prefix}\n\n` + Array.from({ length: paragraphs }, (_, i) => `Paragraph ${i}: retain **meaningful Markdown** and \`exact source\` in this answer.\n\n|Field|Value|\n|---|---|\n|Result|${i}|\n\n`).join("") + "```ts\nconst value = { ready: true };\nconsole.log(value);\n```";
}
const replies = Array.from({ length: count }, (_, i) => [
    { id: `sent-${i}`, kind: "sent", conversation: A, at, input: `Question ${i}: inspect the workspace and explain the result.` },
    { id: `reply-${i}`, kind: "reply", conversation: A, at, text: markdown(`Answer ${i}`, 3), process: {
        timeline: [{ kind: "thought", text: markdown(`Retained process ${i}`, 10), at }, ...Array.from({ length: 6 }, (_, j) => ({ kind: "tool", tool: `tool-${i}-${j}`, at })), { kind: "text", text: `Answer ${i}`, at }],
        tools: Array.from({ length: 6 }, (_, j) => ({ id: `tool-${i}-${j}`, kind: "execute", name: `Check ${i}-${j}`, status: "completed", input: JSON.stringify({ command: `go test ./package${j}` }), output: "Output evidence line\n".repeat(120) })),
    } },
]).flat();
const short = [{ id: "short-reply", kind: "reply", conversation: B, at, text: "Short conversation ready." }];
const state = workState({ at, hub: { node: "fixture-node", started: at, version: "fixture" }, nodes: [], agents: [], projects: [project], tasks: [], plans: [], attempts: [], landings: [] });
const server = http.createServer(async (req, res) => {
    try {
        assert.equal(req.method, "GET");
        const url = new URL(req.url, "http://localhost");
        const file = path.join(dist, url.pathname === "/" ? "index.html" : url.pathname);
        assert.ok(file.startsWith(dist + path.sep));
        const raw = await fs.readFile(file), data = gzipSync(raw);
        const type = file.endsWith(".js") ? "text/javascript" : file.endsWith(".css") ? "text/css" : file.endsWith(".html") ? "text/html" : file.endsWith(".png") ? "image/png" : file.endsWith(".woff2") ? "font/woff2" : "application/octet-stream";
        res.writeHead(200, { "content-type": type, "content-encoding": "gzip", "content-length": data.length, "cache-control": "no-store" }); res.end(data);
    } catch { res.writeHead(404); res.end(); }
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const origin = `http://127.0.0.1:${server.address().port}`;
const browser = await chromium.launch({ headless: process.env.HEADED !== "1" });
const results = [];
const paint = (page) => page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
const percentile = (values, q) => values.toSorted((a, b) => a - b)[Math.min(values.length - 1, Math.floor(values.length * q))];
try {
    for (let run = 0; run < repetitions; run++) {
        const context = await browser.newContext({ viewport: { width: 1600, height: 1000 }, serviceWorkers: "block" });
        try {
            const page = await context.newPage(), errors = [];
            page.setDefaultTimeout(30000);
            page.on("pageerror", (error) => errors.push(String(error)));
            await page.route("**/*", async (route) => {
                const req = route.request(), url = new URL(req.url()), p = url.pathname;
                if (url.origin !== origin) { errors.push(`Unexpected external request: ${url.origin}`); return route.abort(); }
                if (!p.startsWith("/console/") && !["/state", "/events", "/history"].includes(p)) return route.continue();
                if (req.method() !== "GET") { errors.push(`Unexpected write: ${p}`); return route.abort(); }
                let json;
                if (p === "/state") json = state;
                else if (p === "/console/coordination") json = { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false };
                else if (p === "/console/desktop") json = { enabled: false, setup_required: false, agent_count: 0 };
                else if (p === "/console/replies") json = { enabled: true, replies: url.searchParams.get("conversation") === B ? short : replies };
                else if (p === "/console/queue") json = { submission_keys: true, queue: url.searchParams.get("conversation") === A ? [{ id: "stream-turn", conversation: A, input: "Live check", state: "running", enqueued_at: at, started_at: at }] : [] };
                else if (p === "/console/context") json = { enabled: true, context: { conversation: url.searchParams.get("conversation"), agents: [], project: { ...project, bound: true } } };
                else if (p === "/console/conversations") json = { enabled: true, conversations: [{ id: A, title: "Performance history", project: "scratch", count: replies.length, running: true, last_at: at }, { id: B, title: "Short conversation", project: "scratch", count: 1, running: false, last_at: at }] };
                else if (p === "/console/questions") json = { questions: [] };
                else if (p === "/console/verbs" || p === "/console/suggest") json = { verbs: [], suggestions: [] };
                else { errors.push(`Unhandled API: ${p}`); return route.fulfill({ status: 500, json: { error: "Unmocked API" } }); }
                return route.fulfill({ json });
            });
            await page.addInitScript(({ A, count }) => {
                localStorage.setItem("steve.ui.locale", "en"); sessionStorage.setItem("steve.conversation", A);
                window.sources = [];
                window.EventSource = class {
                    addEventListener() {}
                    constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); }
                    close() { window.sources = window.sources.filter((source) => source !== this); }
                };
                window.emit = (event) => window.sources.forEach((source) => source.onmessage?.({ data: JSON.stringify(event) }));
                window.measured = { long: [], input: [] };
                new PerformanceObserver((list) => window.measured.long.push(...list.getEntries().map((entry) => ({ start: entry.startTime, duration: entry.duration })))).observe({ type: "longtask", buffered: true });
                document.addEventListener("input", () => {
                    const start = performance.now();
                    requestAnimationFrame(() => requestAnimationFrame(() => window.measured.input.push(performance.now() - start)));
                }, true);
                const tick = () => {
                    if (document.querySelectorAll(".message-assistant").length === count && document.querySelector('.cm-content[contenteditable="true"]')) requestAnimationFrame(() => requestAnimationFrame(() => window.measured.ready = performance.now()));
                    else requestAnimationFrame(tick);
                };
                requestAnimationFrame(tick);
            }, { A, count });
            const cdp = await context.newCDPSession(page);
            await cdp.send("Emulation.setCPUThrottlingRate", { rate: cpuRate });
            await cdp.send("Performance.enable");
            await page.goto(origin + "/#/console", { waitUntil: "domcontentloaded" });
            await page.waitForFunction(() => window.measured.ready);
            const startup = await page.evaluate(() => ({ readyMs: window.measured.ready, dom: document.querySelectorAll("*").length,
                longTasks: window.measured.long.filter((entry) => entry.start < window.measured.ready),
                js: performance.getEntriesByType("resource").filter((entry) => entry.name.split("?")[0].endsWith(".js")).map((entry) => ({ url: new URL(entry.name).pathname, decoded: entry.decodedBodySize, encoded: entry.encodedBodySize })),
                images: performance.getEntriesByType("resource").filter((entry) => entry.initiatorType === "img").map((entry) => ({ url: new URL(entry.name).pathname, encoded: entry.encodedBodySize, decoded: entry.decodedBodySize })),
            }));
            const metrics = Object.fromEntries((await cdp.send("Performance.getMetrics")).metrics.map((entry) => [entry.name, entry.value]));
            assert.equal(await page.locator(".message-user").count(), count, "retain every user message");
            assert.equal(await page.locator(".message-assistant").count(), count, "retain every answer, not only the viewport");
            if (!baseline) {
                assert.equal(await page.locator(".message-assistant [data-timeline]").count(), 0, "closed traces must not mount or parse their hidden histories");
                assert.ok(startup.dom < 20000, `closed trace DOM budget exceeded: ${startup.dom}`);
                const icon = page.locator("img.app-mark");
                assert.equal(await icon.evaluate((el) => el.naturalWidth), 108, "36px chrome uses a 3x web icon, not the full desktop artwork");
            }
            const editor = page.locator(".cm-content");
            await editor.click();
            const draft = "performance check 1234";
            for (const key of draft) { await page.keyboard.type(key); await paint(page); }
            assert.equal(await editor.innerText(), draft);
            const input = await page.evaluate(() => window.measured.input);
            assert.equal(input.length, draft.length, "input measurements observe every real editor input");
            const stream = [];
            async function progress(n) {
                const answer = markdown("Live answer", n * 3) + `\n\nSTREAM_MARKER_${n}`;
                // Measure in the page, not Playwright's selector polling/network
                // round-trip. The observer watches only the active turn: the last
                // message, above the execution hint that closes the transcript.
                return page.evaluate(({ event, marker }) => new Promise((resolve, reject) => {
                    const start = performance.now(), root = document.querySelector(".transcript-messages");
                    const live = () => { let el = root.lastElementChild; while (el?.matches("[role=status]")) el = el.previousElementSibling; return el; };
                    const timeout = setTimeout(() => { observer.disconnect(); reject(new Error(`Stream did not paint ${marker}`)); }, 10000);
                    const observer = new MutationObserver(() => {
                        if (live()?.textContent.includes(marker)) {
                            observer.disconnect(); clearTimeout(timeout); requestAnimationFrame(() => requestAnimationFrame(() => resolve(performance.now() - start)));
                        }
                    });
                    observer.observe(root, { childList: true, subtree: true, characterData: true }); window.emit(event);
                }), { marker: `STREAM_MARKER_${n}`, event: { kind: "console.progress", at, conversation: A, exchange_id: "stream-turn", progress: { phase: "running", answer, timeline: [{ kind: "text", text: answer, at }] } } });
            }
            for (let n = 1; n <= 12; n++) stream.push(await progress(n));
            const scroll = page.locator(".transcript-scroll");
            assert.ok(await scroll.evaluate((el) => el.scrollHeight - el.clientHeight - el.scrollTop < 5), "streaming follows the tail");
            await scroll.evaluate((el) => { el.scrollTop = 0; }); await paint(page);
            await progress(13);
            assert.ok(await scroll.evaluate((el) => el.scrollTop < 5), "reading history must not jump back to the live tail");
            const trace = page.locator(".message-assistant").first().locator("details").first();
            await trace.locator(":scope > summary").focus(); await page.keyboard.press("Enter");
            await trace.getByRole("heading", { name: "Retained process 0", exact: true }).waitFor();
            await trace.locator('[data-span-kind="tool"] > summary').click();
            const tool = trace.locator("details.group\\/row").first();
            await tool.locator(":scope > summary").click();
            await tool.locator("pre").filter({ hasText: "Output evidence line" }).waitFor();
            await trace.locator(":scope > summary").click(); await trace.locator(":scope > summary").click();
            assert.equal(await tool.getAttribute("open"), "", "folding and reopening a trace preserves nested tool state");
            for (let turn = 0; turn < 2; turn++) {
                await page.locator("button.conversation-row").filter({ hasText: "Short conversation" }).click();
                await page.getByText("Short conversation ready.", { exact: true }).waitFor();
                assert.equal(await editor.evaluate((el) => { const copy = el.cloneNode(true); copy.querySelectorAll(".cm-placeholder").forEach((node) => node.remove()); return copy.textContent; }), "", "drafts belong to their conversation");
                await page.locator("button.conversation-row").filter({ hasText: "Performance history" }).click();
                await page.waitForFunction((count) => document.querySelectorAll(".message-assistant").length === count, count);
                await page.waitForFunction((draft) => document.querySelector(".cm-content")?.textContent === draft, draft);
            }
            await page.setViewportSize({ width: 390, height: 844 }); await paint(page);
            assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "long content must not overflow a narrow workbench");
            await editor.click(); await page.keyboard.press("End"); await page.keyboard.type("x"); await paint(page);
            assert.equal(await editor.innerText(), draft + "x");
            assert.deepEqual(errors, []);
            const result = { run, startup, metrics, input, stream };
            results.push(result);
            console.log(JSON.stringify({ run, cpuRate, replies: replies.length, readyMs: startup.readyMs, dom: startup.dom, initialJSBytes: startup.js.reduce((sum, entry) => sum + entry.decoded, 0), initialJSGzip: startup.js.reduce((sum, entry) => sum + entry.encoded, 0), inputToPaintP95: percentile(input, .95), streamToPaintP95: percentile(stream, .95) }));
        } finally { await context.close(); }
    }
} finally {
    await browser.close(); await new Promise((resolve) => server.close(resolve));
    if (process.env.CONSOLE_PERF_OUTPUT) await fs.writeFile(process.env.CONSOLE_PERF_OUTPUT, JSON.stringify({ cpuRate, replies: replies.length, results }, null, 2));
}
console.log("Production console performance and interaction budgets passed");
