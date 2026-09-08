// Isolated browser regression against the real React source. Every API is
// intercepted; this test never starts an agent or contacts an existing node.
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { setTimeout as delay } from "node:timers/promises";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";

const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || new URL("../../web/console/node_modules/playwright/index.mjs", import.meta.url).href);
// Instrument the real rendering path in this test server only. Counts expose
// repeated parsing of unchanged history independently of machine timing.
const server = await createServer({
    root: fileURLToPath(new URL("../../web/console", import.meta.url)),
    plugins: [{ name: "streaming-render-measurements", enforce: "pre", transform(source, id) {
        if (id.endsWith("/components/steve/markdown.tsx")) return source.replace('const raw = String(file.value);', 'const raw = String(file.value); globalThis.__markdownParses ??= {}; globalThis.__markdownParses[raw] = (globalThis.__markdownParses[raw] || 0) + 1;');
        if (id.endsWith("/src/app.tsx")) return source.replace('function Shell() {', 'function Shell() { globalThis.__shellRenders = (globalThis.__shellRenders || 0) + 1;');
    } }], server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
const app = { url: `http://127.0.0.1:${server.httpServer.address().port}`, close: () => server.close() };
const browser = await chromium.launch({ headless: process.env.HEADED !== "1", channel: process.env.BROWSER_CHANNEL });
const at = "2026-09-06T10:00:00Z";
const A = "console:streaming-test";
async function eventually(predicate, message) {
    for (let i = 0; i < 80; i++) { if (await predicate()) return; await delay(25); }
    assert.fail(message);
}

async function fixture() {
    const context = await browser.newContext({ viewport: { width: 1600, height: 1000 }, serviceWorkers: "block" });
    const page = await context.newPage();
    page.setDefaultTimeout(10000);
    await page.clock.install();
    const f = { page, context, errors: [], stateReads: 0, replyReads: 0, queueReads: 0, queue: [], replies: [] };
    page.on("pageerror", (e) => f.errors.push(String(e)));
    const project = { id: "scratch", node: "test-node", path: "/test/scratch", repo: "inplace", level: "public", agents: [], workspaces: [] };
    f.replies = Array.from({ length: 35 }, (_, i) => ({ id: `reply-${i}`, kind: "reply", conversation: A, at, text: `Conversation A history ${i}\n\nA retained **Markdown answer** with enough detail to occupy its own row.\n\n|Field|Value|\n|---|---|\n|Result|${i}|` }));
    f.emit = (event) => page.evaluate((event) => window.emit(event), { at, conversation: A, ...event });
    await page.route("**/*", async (route) => {
        const req = route.request(), url = new URL(req.url()), pathname = url.pathname;
        if (url.origin !== app.url) { f.errors.push(`Unexpected external request: ${url.origin}`); return route.abort(); }
        if (!["/state", "/events", "/history"].includes(pathname) && !pathname.startsWith("/console/")) return route.continue();
        if (req.method() !== "GET") { f.errors.push(`Unexpected write: ${pathname}`); return route.abort(); }
        if (pathname === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false } });
        if (pathname === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false, agent_count: 0 } });
        if (pathname === "/state") { f.stateReads++; return route.fulfill({ json: { at, hub: { node: "test-node", started: at, version: "test" }, nodes: [], agents: [], tasks: [], plans: [], projects: [project], attempts: [], landings: [] } }); }
        if (pathname === "/console/replies") { f.replyReads++; return route.fulfill({ json: { enabled: true, replies: f.replies } }); }
        if (pathname === "/console/queue") { f.queueReads++; return route.fulfill({ json: { submission_keys: true, queue: f.queue } }); }
        if (pathname === "/console/context") return route.fulfill({ json: { enabled: true, context: { conversation: A, agents: [], project: { ...project, bound: true } } } });
        if (pathname === "/console/conversations") return route.fulfill({ json: { enabled: true, conversations: [{ id: A, title: "Conversation A", project: "scratch", count: f.replies.length, running: false, last_at: at }] } });
        if (pathname === "/console/questions") return route.fulfill({ json: { questions: [] } });
        if (pathname === "/console/verbs" || pathname === "/console/suggest") return route.fulfill({ json: { verbs: [], suggestions: [] } });
        f.errors.push(`Unhandled API: ${req.method()} ${pathname}`);
        return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
    });
    await page.addInitScript((conversation) => {
        localStorage.setItem("steve.ui.locale", "zh");
        sessionStorage.setItem("steve.conversation", conversation);
        window.sources = [];
        window.EventSource = class {
            constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); }
            close() { window.sources = window.sources.filter((s) => s !== this); }
        };
        window.emit = (ev) => { for (const source of window.sources) source.onmessage?.({ data: JSON.stringify(ev) }); };
    }, A);
    await page.goto(`${app.url}/#/console`);
    await page.locator("main header").getByText("Conversation A", { exact: true }).waitFor();
    await page.locator("main header").getByText("scratch · test-node", { exact: true }).waitFor();
    await page.clock.runFor(500);
    await delay(100);
    return f;
}

try {
    const f = await fixture();
    const { page } = f;
    await f.emit({ kind: "console.sent", exchange_id: "stream-turn", text: "Streaming test" });
    await page.clock.runFor(500);
    await delay(100);
    const before = await page.evaluate(() => ({ parses: { ...window.__markdownParses }, shell: window.__shellRenders }));
    assert.ok(Object.keys(before.parses).filter((text) => text.startsWith("Conversation A history")).length === 35, "Instrumentation must observe the actual Markdown parser");
    assert.ok(before.shell > 0, "Instrumentation must observe the actual shell renderer");
    const reads = f.stateReads;
    const start = performance.now();
    for (let index = 1; index <= 12; index++) {
        await f.emit({ kind: "console.progress", exchange_id: "stream-turn", progress: { agent: "test-agent", phase: "running", answer: `Streaming answer ${index}`, timeline: [{ kind: "text", text: `Streaming answer ${index}`, at }] } });
        await page.getByText(`Streaming answer ${index}`, { exact: true }).first().waitFor();
        assert.equal(await page.getByText(`Streaming answer ${index}`, { exact: true }).count(), 1, "A live answer must appear once, not again inside its timeline");
        assert.equal(await page.locator("summary").getByText("过程", { exact: true }).count(), 0, "A plain text stream must not create an empty or duplicate process disclosure");
        await page.clock.runFor(300);
    }
    await delay(100);
    const after = await page.evaluate(() => ({ parses: { ...window.__markdownParses }, shell: window.__shellRenders }));
    const historyParses = Object.entries(after.parses).filter(([text]) => text.startsWith("Conversation A history")).reduce((sum, [text, count]) => sum + count - (before.parses[text] || 0), 0);
    const measurements = { streamMilliseconds: Math.round(performance.now() - start), stateReads: f.stateReads - reads, historicalMarkdownParses: historyParses, shellRenders: after.shell - before.shell };
    console.log(JSON.stringify(measurements));
    if (process.env.BASELINE !== "1") {
        assert.equal(measurements.stateReads, 0, "Streaming text must not refetch the fleet snapshot");
        assert.equal(historyParses, 0, "Streaming text must not reparse unchanged Markdown history");
        assert.equal(measurements.shellRenders, 0, "Streaming text must not rerender the fleet shell");
    }
    const intermediate = "I will inspect the configuration.";
    const thought = "Checking the selected workspace.";
    const finalAnswer = "The configuration is valid.";
    const timeline = [{ kind: "text", text: intermediate, at }, { kind: "thought", text: thought, at }, { kind: "tool", tool: "config-read", at }, { kind: "text", text: finalAnswer, at }];
    await f.emit({ kind: "console.progress", exchange_id: "stream-turn", progress: { phase: "running", answer: intermediate + finalAnswer, reasoning: thought, tools: [{ id: "config-read", kind: "read", name: "Read configuration", status: "completed", output: "Configuration content" }], timeline } });
    await page.getByText(finalAnswer, { exact: true }).waitFor();
    assert.equal(await page.getByText(finalAnswer, { exact: true }).count(), 1, "The final narration must not be repeated in the process");
    assert.equal(await page.getByText(intermediate, { exact: true }).count(), 1, "Earlier narration must remain once in its timeline");
    await page.locator('[data-span-kind="thought"]').getByText(thought, { exact: true }).waitFor();
    assert.equal(await page.locator('[data-span-kind="tool"]').count(), 1, "The tool activity remains in the process timeline");
    assert.equal(await page.getByText(intermediate + finalAnswer, { exact: true }).count(), 0, "Do not repeat the concatenated narration beneath the timeline");
    // A transcript-only server still sends a usable answer without timeline data.
    await f.emit({ kind: "console.progress", exchange_id: "stream-turn", progress: { phase: "running", answer: "Answer without timeline" } });
    await page.getByText("Answer without timeline", { exact: true }).waitFor();
    // Task/node lifecycle events and an SSE reconnect still invalidate state.
    for (const kind of ["task.changed", "node.updated", "console.reply", "delegate.progress"]) {
        const previous = f.stateReads;
        await f.emit({ kind, exchange_id: "other-turn", step: kind === "delegate.progress" ? { state: "done" } : undefined });
        await page.clock.runFor(300);
        await eventually(() => f.stateReads > previous, `${kind} must refresh fleet state`);
    }
    const previous = f.stateReads;
    await page.evaluate(() => { for (const source of [...window.sources]) source.onerror?.(); });
    await page.clock.runFor(3100);
    await delay(100);
    if (process.env.BASELINE !== "1") assert.ok(f.stateReads > previous, "Reconnect must immediately recover fleet state without waiting for polling");
    if (process.env.BASELINE !== "1") {
        await f.emit({ kind: "console.sent", exchange_id: "phase-turn", text: "Phase feedback test" });
        for (const [phase, text] of [["waking", "正在准备会话…"], ["running", "正在处理…"], ["finishing", "正在整理结果…"], ["saving", "正在保存结果…"]]) {
            await f.emit({ kind: "console.progress", exchange_id: "phase-turn", progress: { phase, agent: "test-agent", answer: "Generated reply before durable completion" } });
            await page.getByText(text, { exact: true }).waitFor();
            await page.getByText("Generated reply before durable completion", { exact: true }).waitFor();
            assert.equal(await page.locator(".message-assistant").filter({ hasText: "Generated reply before durable completion" }).count(), 0, "Saving progress must not claim durable completion");
        }
        const final = { id: "phase-reply", conversation: A, at, kind: "reply", exchange_id: "phase-turn", text: "Result could not be saved. Retry after restoring storage." };
        f.replies.push(final);
        await f.emit({ ...final, reply_id: final.id, kind: "console.reply" });
        await page.getByText("Result could not be saved. Retry after restoring storage.", { exact: true }).waitFor();
        assert.equal(await page.getByText("正在保存结果…", { exact: true }).count(), 0, "The terminal result must close the progress indicator");
        // A missed event must converge through polling, without submitting work.
        f.replies.push({ id: "missed-reply", kind: "reply", conversation: A, at, text: "Completed while disconnected" });
        const readsBeforePoll = f.stateReads;
        await page.clock.runFor(10100);
        await page.getByText("Completed while disconnected", { exact: true }).waitFor();
        assert.ok(f.stateReads > readsBeforePoll, "The periodic fleet recovery read must remain active");
        await f.emit({ kind: "console.sent", exchange_id: "narrow-turn", text: "Narrow window status" });
        await f.emit({ kind: "console.progress", exchange_id: "narrow-turn", progress: { phase: "saving", agent: "test-agent-with-a-long-name", model: "a-long-model-name-to-check-wrapping", answer: "Generated reply" } });
        for (const width of [1600, 1000, 780]) {
            await page.setViewportSize({ width, height: 650 });
            await page.getByText("正在保存结果…", { exact: true }).waitFor();
            assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "Phase feedback must not overflow the window");
        }
    }
    assert.deepEqual(f.errors, [], "No JS errors or unexpected API calls");
    await f.context.close();
} finally { await browser.close(); await app.close(); }
