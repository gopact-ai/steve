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
        if (id.endsWith("/src/pages/console.tsx")) {
            // A completion barrier for held HTTP/lock races, including rejected
            // snapshots. No wall-clock delay can establish this boundary.
            const start = "const loadQueue = useCallback(async () => {";
            const end = "}, [conversation]);\n    const loadReplies";
            assert.ok(source.includes(start) && source.includes(end), "Queue completion instrumentation must wrap the real loadQueue");
            return source.replace(start, `${start} try {`).replace(end, "} finally { globalThis.__queueReadsSettled = (globalThis.__queueReadsSettled || 0) + 1; }\n    " + end);
        }
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
    // The exchange exists before console.sent is published. Initial/reconnect
    // reads must describe the same turn as the event stream.
    const f = { page, context, errors: [], stateReads: 0, replyReads: 0, queueReads: 0, queue: [{ id: "stream-turn", conversation: A, input: "Streaming test", state: "running", enqueued_at: at, started_at: at }], replies: [] };
    page.on("pageerror", (e) => f.errors.push(String(e)));
    const project = { id: "scratch", node: "test-node", path: "/test/scratch", repo: "inplace", level: "public", agents: [], workspaces: [] };
    f.replies = Array.from({ length: 35 }, (_, i) => ({ id: `reply-${i}`, kind: "reply", conversation: A, at, text: `Conversation A history ${i}\n\nA retained **Markdown answer** with enough detail to occupy its own row.\n\n|Field|Value|\n|---|---|\n|Result|${i}|` }));
    f.emit = async (event) => {
        const ev = { at, conversation: A, ...event };
        if (ev.kind === "console.sent" || ev.kind === "console.reply") ev.reply_id ||= `${ev.exchange_id}-${ev.kind.slice("console.".length)}`;
        if (ev.kind === "console.sent") {
            const exchange = { id: ev.exchange_id, conversation: ev.conversation, input: ev.text, state: "running", enqueued_at: ev.at, started_at: ev.at };
            const existing = f.queue.findIndex((item) => item.id === exchange.id);
            if (existing < 0) f.queue.push(exchange);
            else f.queue[existing] = exchange;
        }
        if (ev.kind === "console.reply") {
            const exchange = f.queue.find((item) => item.id === ev.exchange_id);
            if (exchange) { exchange.state = "done"; exchange.reply_id = ev.reply_id; }
        }
        if (ev.kind === "console.sent" || ev.kind === "console.reply") {
            const kind = ev.kind.slice("console.".length);
            const reply = { id: ev.reply_id, kind, exchange_id: ev.exchange_id, conversation: ev.conversation, at: ev.at, ...(kind === "sent" ? { input: ev.text, text: "" } : { text: ev.text }) };
            const existing = f.replies.findIndex((item) => item.id === reply.id);
            if (existing < 0) f.replies.push(reply);
            else f.replies[existing] = reply;
        }
        await page.evaluate((event) => window.emit(event), ev);
    };
    await page.route("**/*", async (route) => {
        const req = route.request(), url = new URL(req.url()), pathname = url.pathname;
        if (url.origin !== app.url) { f.errors.push(`Unexpected external request: ${url.origin}`); return route.abort(); }
        if (!["/state", "/events", "/history"].includes(pathname) && !pathname.startsWith("/console/")) return route.continue();
        if (req.method() !== "GET") { f.errors.push(`Unexpected write: ${pathname}`); return route.abort(); }
        if (pathname === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false } });
        if (pathname === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false, agent_count: 0 } });
        if (pathname === "/state") { f.stateReads++; return route.fulfill({ json: { at, hub: { node: "test-node", started: at, version: "test" }, nodes: [], agents: [], tasks: [], plans: [], projects: [project], attempts: [], landings: [] } }); }
        if (pathname === "/console/replies") { f.replyReads++; return route.fulfill({ json: { enabled: true, replies: f.replies.filter((item) => item.conversation === url.searchParams.get("conversation")) } }); }
        if (pathname === "/console/queue") {
            f.queueReads++;
            const json = structuredClone({ submission_keys: true, queue: f.queue.filter((item) => item.conversation === url.searchParams.get("conversation")) });
            const hold = f.holdQueue;
            f.holdQueue = null;
            if (hold) { hold.captured(); await hold.released; }
            return route.fulfill({ json });
        }
        if (pathname === "/console/context") return route.fulfill({ json: { enabled: true, context: { conversation: A, agents: [], project: { ...project, bound: true } } } });
        if (pathname === "/console/conversations") return route.fulfill({ json: { enabled: true, conversations: [{ id: A, title: "Conversation A", project: "scratch", count: f.replies.filter((item) => item.conversation === A).length, running: f.queue.some((item) => item.conversation === A && item.state === "running"), last_at: at }] } });
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

async function staleQueueRegression(terminal, duringReconciliation = false) {
    const f = await fixture(), { page } = f;
    try {
        await f.emit({ kind: "console.sent", exchange_id: "stream-turn", text: "Streaming test" });
        await f.emit({ kind: "console.progress", exchange_id: "stream-turn", progress: { phase: "running", answer: "Original active turn" } });
        await page.getByText("Original active turn", { exact: true }).waitFor();
        const captured = Promise.withResolvers(), released = Promise.withResolvers();
        f.holdQueue = { captured: captured.resolve, released: released.promise };
        await f.emit({ kind: "console.queue" });
        await captured.promise;
        if (duringReconciliation) {
            // Hold the actual cross-window draft lock so the old snapshot has
            // passed its HTTP guard but cannot yet finish reconciliation.
            await page.evaluate(() => new Promise((acquired) => {
                navigator.locks.request("steve.console.drafts", async () => {
                    acquired();
                    await new Promise((release) => { window.releaseDraftLock = release; });
                });
            }));
            released.resolve();
            await eventually(() => page.evaluate(async () => (await navigator.locks.query()).pending.some((lock) => lock.name === "steve.console.drafts")), "Queue reconciliation must be waiting on the held draft lock");
        }
        await f.emit({ kind: "console.reply", exchange_id: "stream-turn", text: "Original turn completed" });
        await page.getByText("Original turn completed", { exact: true }).waitFor();
        if (!terminal) {
            await f.emit({ kind: "console.sent", exchange_id: "new-turn", text: "A newer turn" });
            await f.emit({ kind: "console.progress", exchange_id: "new-turn", progress: { phase: "running", answer: "New turn is streaming" } });
            await page.getByText("New turn is streaming", { exact: true }).waitFor();
        }
        const beforeRelease = await page.evaluate(() => window.__queueReadsSettled || 0);
        if (duringReconciliation) await page.evaluate(() => window.releaseDraftLock());
        else released.resolve();
        await eventually(() => page.evaluate((before) => (window.__queueReadsSettled || 0) > before, beforeRelease), "The held queue load must settle before checking newer events");
        if (!terminal) {
            await f.emit({ kind: "console.progress", exchange_id: "new-turn", progress: { phase: "running", answer: "New turn survived the stale queue response" } });
            await page.getByText("New turn survived the stale queue response", { exact: true }).waitFor();
        } else {
            await f.emit({ kind: "console.progress", exchange_id: "stream-turn", progress: { phase: "running", answer: "Obsolete progress must not resurrect the completed turn" } });
            await f.emit({ kind: "console.notice", text: "Terminal race assertion boundary" });
            await page.getByText("Terminal race assertion boundary", { exact: true }).waitFor();
            assert.equal(await page.getByText("Obsolete progress must not resurrect the completed turn", { exact: true }).count(), 0, "A stale queue response must not reopen a completed exchange");
        }
        assert.deepEqual(f.errors, []);
    } finally { await f.context.close(); }
}

try {
    if (process.env.BASELINE !== "1") {
        await staleQueueRegression(false);
        await staleQueueRegression(true);
        await staleQueueRegression(false, true);
        await staleQueueRegression(true, true);
    }
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
    await f.emit({ kind: "console.progress", exchange_id: "stream-turn", progress: { phase: "running", answer: "Answer with empty trace", timeline: [{ kind: "thought", text: " ", at }] } });
    await page.getByText("Answer with empty trace", { exact: true }).waitFor();
    assert.equal(await page.getByText("Answer with empty trace", { exact: true }).count(), 1, "A trace without text spans must not hide the answer snapshot");
    await f.emit({ kind: "console.reply", exchange_id: "stream-turn", text: "Streaming test completed" });
    await page.getByText("Streaming test completed", { exact: true }).waitFor();
    // Task/node lifecycle events still invalidate state.
    for (const kind of ["task.changed", "node.updated", "console.reply", "delegate.progress"]) {
        const previous = f.stateReads;
        await f.emit({ kind, conversation: "console:other", exchange_id: "other-turn", step: kind === "delegate.progress" ? { state: "done" } : undefined });
        await page.clock.runFor(300);
        await eventually(() => f.stateReads > previous, `${kind} must refresh fleet state`);
    }
    if (process.env.BASELINE !== "1") {
        await f.emit({ kind: "console.sent", exchange_id: "phase-turn", text: "Phase feedback test" });
        for (const [phase, text] of [["waking", "正在准备会话…"], ["running", "正在处理…"], ["finishing", "正在整理结果…"], ["saving", "正在保存结果…"]]) {
            await f.emit({ kind: "console.progress", exchange_id: "phase-turn", progress: { phase, agent: "test-agent", answer: "Generated reply before durable completion" } });
            await page.getByText(text, { exact: true }).waitFor();
            await page.getByText("Generated reply before durable completion", { exact: true }).waitFor();
            assert.equal(await page.locator(".message-assistant").filter({ hasText: "Generated reply before durable completion" }).count(), 0, "Saving progress must not claim durable completion");
        }
        const final = { id: "phase-reply", conversation: A, at, kind: "reply", exchange_id: "phase-turn", text: "Result could not be saved. Retry after restoring storage." };
        await f.emit({ ...final, reply_id: final.id, kind: "console.reply" });
        await page.getByText("Result could not be saved. Retry after restoring storage.", { exact: true }).waitFor();
        assert.equal(await page.getByText("正在保存结果…", { exact: true }).count(), 0, "The terminal result must close the progress indicator");
        await f.emit({ kind: "console.sent", exchange_id: "narrow-turn", text: "Narrow window status" });
        await f.emit({ kind: "console.progress", exchange_id: "narrow-turn", progress: { phase: "saving", agent: "test-agent-with-a-long-name", model: "a-long-model-name-to-check-wrapping", answer: "Generated reply" } });
        for (const width of [1600, 1000, 780]) {
            await page.setViewportSize({ width, height: 650 });
            await page.getByText("正在保存结果…", { exact: true }).waitFor();
            assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "Phase feedback must not overflow the window");
        }
        // Keep recovery in this active turn. An older queue snapshot from a
        // reconnect/poll must not be raced against an unrelated new test turn.
        const beforeReconnect = { state: f.stateReads, queue: f.queueReads, replies: f.replyReads };
        await page.evaluate(() => { for (const source of [...window.sources]) source.onerror?.(); });
        await page.clock.runFor(3100);
        await eventually(() => f.stateReads > beforeReconnect.state && f.queueReads > beforeReconnect.queue && f.replyReads > beforeReconnect.replies, "Reconnect must recover fleet, queue and transcript without waiting for polling");
        await f.emit({ kind: "console.progress", exchange_id: "narrow-turn", progress: { phase: "saving", answer: "Stream continued after reconnect" } });
        await page.getByText("Stream continued after reconnect", { exact: true }).waitFor();
        // A missed event must converge through polling, without submitting work.
        f.replies.push({ id: "missed-reply", kind: "reply", conversation: A, at, text: "Completed while disconnected" });
        const readsBeforePoll = f.stateReads;
        await page.clock.runFor(10100);
        await page.getByText("Completed while disconnected", { exact: true }).waitFor();
        assert.ok(f.stateReads > readsBeforePoll, "The periodic fleet recovery read must remain active");
        await f.emit({ kind: "console.progress", exchange_id: "narrow-turn", progress: { phase: "saving", answer: "Stream continued after recovery polling" } });
        await page.getByText("Stream continued after recovery polling", { exact: true }).waitFor();
    }
    assert.deepEqual(f.errors, [], "No JS errors or unexpected API calls");
    await f.context.close();
} finally { await browser.close(); await app.close(); }
