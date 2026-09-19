// Real Console + SideChat, isolated Chromium profile and GET-only API fixture.
// The test controls open existing fixture conversations; no agent is started.
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { setTimeout as delay } from "node:timers/promises";
import { mkdir } from "node:fs/promises";
import path from "node:path";
import { createServer } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";

const web = fileURLToPath(new URL("..", import.meta.url));
const A = "console:side-projection-a", B = "console:side-projection-b";
const at = (n) => `2026-09-19T00:00:${String(n).padStart(2, "0")}Z`;
const deferred = () => Promise.withResolvers();
const server = await createServer({
    root: web,
    plugins: [{
        name: "conversation-test-controls", enforce: "pre",
        transform(source, id) {
            if (id.endsWith("/src/app.tsx")) return source
                .replace('import { SideChatProvider }', 'import { SideChatProvider, useSideChat }')
                .replace("<SideChatProvider>", "<SideChatProvider><ConversationTestControls />")
                + `\nfunction ConversationTestControls() {
                    const side = useSideChat();
                    const open = (project) => side.open({ material: { id: "source-"+project, project, title: project, kind: "text", mime: "text/plain", size: 8, digest: "d".repeat(64), source: {kind:"reply", conversation:"${A}", reply_id:"fixture"}, created_at:"${at(0)}" }, ref: { id:"source-"+project }, excerpt:"Fixture source", originConversation:"${A}" });
                    return <div style={{position:"fixed",right:0,top:0,zIndex:9999}}>
                        <button onClick={()=>open("p")}>Fixture side A</button>
                        <button onClick={()=>open("q")}>Fixture side B</button>
                    </div>;
                }`;
            if (id.endsWith("/src/lib/conversation-controller.ts")) {
                for (const resource of ["Queue", "Replies"]) {
                    const lower = resource.toLowerCase();
                    const marker = `load${resource} = async () => { await this.read("${lower}"); };`;
                    assert.ok(source.includes(marker), "instrument the actual read completion");
                    source = source.replace(marker, `load${resource} = async () => { try { await this.read("${lower}"); } finally { globalThis.__readsSettled = (globalThis.__readsSettled || 0) + 1; } };`);
                }
                return source;
            }
        },
    }],
    server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
const url = `http://127.0.0.1:${server.httpServer.address().port}`;
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const context = await browser.newContext({ viewport: { width: 1600, height: 1000 }, serviceWorkers: "block" });
const page = await context.newPage();
page.setDefaultTimeout(10000);
await page.clock.install();
const main = () => page.locator(".transcript-messages");
const side = () => page.locator("[data-side-chat]");
const errors = [];
page.on("pageerror", (error) => errors.push(String(error)));
page.on("console", (message) => {
    if (message.type() === "error" && /Encountered two children|Each child.*unique/.test(message.text())) errors.push(message.text());
});
const project = (id) => ({ id, node: "test", path: `/fixture/${id}`, repo: "inplace", level: "public", agents: [], workspaces: [] });
const f = {
    stateReads: 0, contextReads: 0, replyReads: 0, queueReads: 0, observed: 1, eligible: true,
    queue: [{ id: "a1", conversation: A, input: "A turn", state: "running", enqueued_at: at(1), started_at: at(1) }],
    replies: [{ id: "a-history", conversation: A, kind: "reply", at: at(0), text: "Retained A history" },
        { id: "b-history", conversation: B, kind: "reply", at: at(0), text: "Retained B history" }],
    holds: [],
};
await context.route("**/*", async (route) => {
    const request = route.request(), u = new URL(request.url()), p = u.pathname, conversation = u.searchParams.get("conversation");
    if (u.origin !== url) { errors.push(`External request: ${u.origin}`); return route.abort(); }
    if (!p.startsWith("/console/") && !["/state", "/events", "/history"].includes(p)) return route.continue();
    if (request.method() !== "GET") { errors.push(`Unexpected write: ${p}`); return route.abort(); }
    if (p === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false } });
    if (p === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false, agent_count: 0 } });
    if (p === "/state") {
        f.stateReads++;
        return route.fulfill({ json: { at: at(f.observed), hub: { node: "test", started: at(0), version: "fixture" },
            nodes: [{ name: "test", up: true, health: { at: at(f.observed), load1: f.observed } }],
            agents: [{ id: "agent", node: "test", harness: "fixture", eligible: f.eligible, activities: [{ at: at(f.observed) }] }],
            projects: [project("p"), project("q")], tasks: [], plans: [], attempts: [], landings: [] } });
    }
    if (p === "/console/replies" || p === "/console/queue") {
        const resource = p.slice("/console/".length);
        f[resource === "queue" ? "queueReads" : "replyReads"]++;
        const data = structuredClone(resource === "queue" ? { submission_keys: true, material_refs: true, queue: f.queue.filter((entry) => entry.conversation === conversation) }
            : { enabled: true, replies: f.replies.filter((entry) => entry.conversation === conversation) });
        const hold = f.holds.find((h) => h.resource === resource && h.conversation === conversation && !h.used);
        if (hold) {
            hold.used = true; hold.captured.resolve();
            await hold.release.promise;
        }
        if (resource === "replies" && f.failReplies) return route.fulfill({ status: 503, json: { error: "Fixture history temporarily unavailable" } });
        try { await route.fulfill({ json: data }); }
        finally { hold?.delivered.resolve(); }
        return;
    }
    if (p === "/console/context") {
        f.contextReads++;
        return route.fulfill({ json: { enabled: true, context: { conversation, project: { ...project(conversation === A ? "p" : "q"), bound: true }, agents: [] } } });
    }
    if (p === "/console/conversations") return route.fulfill({ json: { enabled: true, conversations: [
        { id: A, title: "Projection A", project: "p", count: 1, last_at: at(1), running: true },
        { id: B, title: "Projection B", project: "q", count: 1, last_at: at(1), running: false },
    ] } });
    if (p === "/console/questions") return route.fulfill({ json: { questions: [] } });
    if (p === "/console/verbs" || p === "/console/suggest") return route.fulfill({ json: { verbs: [], suggestions: [] } });
    errors.push(`Unhandled API: ${p}`);
    return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
});
await context.addInitScript(({ A, B }) => {
    localStorage.setItem("steve.ui.locale", "en");
    sessionStorage.setItem("steve.conversation", A);
    localStorage.setItem("steve.side-conversations", JSON.stringify({
        p: { id: A, project: "p", title: "p", excerpt: "", bindingLocale: "en", bound: true },
        q: { id: B, project: "q", title: "q", excerpt: "", bindingLocale: "en", bound: true },
    }));
    window.sources = [];
    window.EventSource = class {
        constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); }
        close() { window.sources = window.sources.filter((s) => s !== this); }
    };
    window.emit = (event) => { for (const source of window.sources) source.onmessage?.({ data: JSON.stringify(event) }); };
}, { A, B });
async function emit(kind, n, extra = {}) {
    const ev = { kind: `console.${kind}`, at: at(n), conversation: A, ...extra };
    if (kind === "sent") f.queue.push({ id: ev.exchange_id, conversation: ev.conversation, input: ev.text || "", state: "running", enqueued_at: ev.at, started_at: ev.at });
    if (kind === "reply") {
        for (const item of f.queue) if (item.id === ev.exchange_id) item.state = "done";
        const reply = { id: ev.reply_id || `reply-${ev.exchange_id}`, exchange_id: ev.exchange_id, conversation: ev.conversation, at: ev.at, kind: "reply", text: ev.text || "" };
        const index = f.replies.findIndex((r) => r.id === reply.id);
        if (index < 0) f.replies.push(reply);
        else f.replies[index] = reply;
    }
    await page.evaluate((ev) => window.emit(ev), ev);
    await page.clock.runFor(120);
}
async function eventually(predicate, message) {
    for (let attempt = 0; attempt < 100; attempt++) { if (await predicate()) return; await delay(25); }
    assert.fail(message);
}
function hold(resource, conversation) {
    const entry = { resource, conversation, captured: deferred(), release: deferred(), delivered: deferred(), used: false };
    f.holds.push(entry); return entry;
}
async function switchMain(id) {
    await page.evaluate((id) => { location.hash = `/console?conversation=${encodeURIComponent(id)}`; }, id);
    await page.locator("main header").getByText(id === A ? "Projection A" : "Projection B", { exact: true }).waitFor();
}
try {
    await page.goto(`${url}/#/console`);
    await main().getByText("Retained A history", { exact: true }).waitFor();
    await page.getByRole("button", { name: "Fixture side A", exact: true }).click();
    await side().getByText("Retained A history", { exact: true }).waitFor();
    await page.clock.runFor(500);
    await delay(100);

    // Mounting mid-turn uses queue state; progress without another sent event
    // must stream in both real consumers.
    await emit("progress", 2, { exchange_id: "a1", progress: { answer: "Shared streamed answer" } });
    await main().getByText("Shared streamed answer", { exact: true }).waitFor();
    await side().getByText("Shared streamed answer", { exact: true }).waitFor();
    const beforeProgress = [f.queueReads, f.replyReads, f.contextReads];
    for (let n = 3; n < 8; n++) await emit("progress", n, { exchange_id: "a1", progress: { answer: `Shared fragment ${n}` } });
    assert.deepEqual([f.queueReads, f.replyReads, f.contextReads], beforeProgress);
    assert.equal(await main().getByText("Shared fragment 7", { exact: true }).count(), 1);
    assert.equal(await side().getByText("Shared fragment 7", { exact: true }).count(), 1);

    // Hold both surfaces' old queue reads behind the real draft lock, then
    // finish the turn and start another before reconciliation can complete.
    await page.evaluate(() => new Promise((acquired) => navigator.locks.request("steve.console.drafts", async () => {
        acquired(); await new Promise((release) => { window.releaseDraftLock = release; });
    })));
    const q1 = hold("queue", A), q2 = hold("queue", A);
    await emit("queue", 8);
    await Promise.all([q1.captured.promise, q2.captured.promise]);
    q1.release.resolve(); q2.release.resolve();
    await eventually(() => page.evaluate(async () => (await navigator.locks.query()).pending.some((l) => l.name === "steve.console.drafts")), "held reconciliation");
    await emit("reply", 9, { exchange_id: "a1", reply_id: "done-a1", text: "Shared completed answer" });
    await emit("sent", 10, { exchange_id: "a2", reply_id: "sent-a2", text: "Next turn" });
    await emit("progress", 11, { exchange_id: "a2", progress: { answer: "New turn survives old HTTP" } });
    await page.evaluate(() => window.releaseDraftLock());
    await main().getByText("New turn survives old HTTP", { exact: true }).waitFor();
    await side().getByText("New turn survives old HTTP", { exact: true }).waitFor();
    assert.equal(await side().getByText("Shared fragment 7", { exact: true }).count(), 0);
    // A late terminal event for a1 must not close a2.
    await emit("reply", 9, { exchange_id: "a1", reply_id: "done-a1", text: "Shared completed answer" });
    await emit("progress", 4, { exchange_id: "a1", progress: { answer: "Obsolete progress" } });
    assert.equal(await page.getByText("Obsolete progress", { exact: true }).count(), 0);
    assert.equal(await side().getByText("New turn survives old HTTP", { exact: true }).count(), 1);

    const readsBeforeState = f.contextReads, stateBefore = f.stateReads;
    f.observed++;
    await page.evaluate(() => window.emit({ kind: "fixture.activity", at: "" }));
    await page.clock.runFor(400);
    await eventually(() => f.stateReads > stateBefore, "fleet read settled");
    await delay(100);
    assert.equal(f.contextReads, readsBeforeState, "unrelated snapshot observation does not reload context");
    f.eligible = false;
    await page.evaluate(() => window.emit({ kind: "fixture.eligibility", at: "" }));
    await page.clock.runFor(400);
    await eventually(() => f.contextReads > readsBeforeState, "eligibility revision reloads context");

    // A→B→A invalidates old reads rather than accepting them on an ID match.
    const h1 = hold("replies", A), h2 = hold("replies", A);
    await emit("sent", 12, { exchange_id: "a3", reply_id: "sent-a3" });
    await Promise.all([h1.captured.promise, h2.captured.promise]);
    await page.getByRole("button", { name: "Fixture side B", exact: true }).click();
    await switchMain(B);
    await main().getByText("Retained B history", { exact: true }).waitFor();
    await side().getByText("Retained B history", { exact: true }).waitFor();
    assert.equal(await side().getByText("Retained A history", { exact: true }).count(), 0);
    f.replies.push({ id: "after-switch", conversation: A, kind: "notice", at: at(13), text: "A updated during switch" });
    await page.getByRole("button", { name: "Fixture side A", exact: true }).click();
    await switchMain(A);
    await main().getByText("A updated during switch", { exact: true }).waitFor();
    await side().getByText("A updated during switch", { exact: true }).waitFor();
    h1.release.resolve(); h2.release.resolve();
    await Promise.all([h1.delivered.promise, h2.delivered.promise]);
    await page.clock.runFor(120);
    assert.equal(await side().getByText("A updated during switch", { exact: true }).count(), 1);
    assert.equal(await main().getByText("A updated during switch", { exact: true }).count(), 1);

    f.failReplies = true;
    await page.clock.runFor(10100);
    await side().getByRole("alert").filter({ hasText: "Fixture history temporarily unavailable" }).waitFor();
    assert.equal(await side().getByText("A updated during switch", { exact: true }).count(), 1, "read failure preserves the transcript");
    f.failReplies = false;
    await side().getByRole("button", { name: "Retry", exact: true }).click();
    await side().getByRole("alert").filter({ hasText: "Fixture history temporarily unavailable" }).waitFor({ state: "hidden" });
    const message = side().getByRole("textbox", { name: "Side chat message" });
    await message.fill("Draft retained during streaming and resize");
    await message.press("Tab");
    assert.equal(await page.evaluate(() => document.activeElement !== document.body), true, "keyboard focus remains in controls");
    for (const width of [1600, 1000, 780]) {
        await page.setViewportSize({ width, height: 720 });
        assert.equal(await message.inputValue(), "Draft retained during streaming and resize");
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "no horizontal overflow");
        if (process.env.CONVERSATION_SCREENSHOTS) {
            await mkdir(process.env.CONVERSATION_SCREENSHOTS, { recursive: true });
            await page.screenshot({ path: path.join(process.env.CONVERSATION_SCREENSHOTS, `conversation-${width}.png`) });
        }
    }
    assert.deepEqual(errors, []);
    console.log("PASS real Console + SideChat: shared projection, HTTP/lock race, duplicate/late SSE, A→B→A, context revision, read-error recovery, keyboard and narrow windows");
} finally {
    for (const h of f.holds) h.release.resolve();
    await context.close();
    await browser.close();
    await server.close();
}
