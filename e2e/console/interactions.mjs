import { workState, workDetail, nativeHistory } from "./work-fixture.mjs";
// Build web/console first. Uses an existing Playwright installation;
// PLAYWRIGHT_MODULE accepts its absolute module path. CHECK selects comma-
// separated scenarios. Every API is mocked; no request reaches a real hub.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { draftOf } from "./composer.mjs";
import { preview } from "../childcard/preview.mjs";
import { usageDurationFixture, usageFixture, usageState, usageResponse } from "./usage-fixture.mjs";

const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || new URL("../../web/console/node_modules/playwright/index.mjs", import.meta.url).href);
const output = process.env.OUTPUT_DIR || path.join(os.tmpdir(), "steve-console-interactions");
await mkdir(output, { recursive: true });
const app = await preview();
const browser = await chromium.launch({ headless: process.env.HEADED !== "1", channel: process.env.BROWSER_CHANNEL });
const at = "2026-09-06T10:00:00Z";
const A = "console:interaction-a", B = "console:interaction-b";
const project = (id) => ({ id, node: "test-node", path: `/test/${id}`, repo: "inplace", level: "public", home: id === "home", agents: [], workspaces: [] });
const task = (id, channel, projectID) => ({ id, transport: "console", channel, project_id: projectID, goal: `Task ${id}`, state: "running", lifecycle: "running", execution: "running", lane: "running", attention: 0, turns: 1, max_turns: 10, member: "test-agent", node: "test-node", updated_at: at });
const gate = () => { let release; const promise = new Promise((resolve) => { release = resolve; }); return { promise, release }; };
async function eventually(predicate, message) {
    for (let i = 0; i < 80; i++) { if (await predicate()) return; await delay(25); }
    assert.fail(message);
}

async function fixture({ history = false, running = false, sandboxed = false } = {}) {
    const context = await browser.newContext({ viewport: { width: 1600, height: 1000 }, serviceWorkers: sandboxed ? "allow" : "block" });
    // Playwright's serviceWorkers:"block" init script reads this getter without
    // a guard and throws in opaque-origin previews. Keep registration blocked
    // without masking application pageerrors or changing the frame's sandbox.
    if (sandboxed) await context.addInitScript(() => {
        let workers;
        try { workers = navigator.serviceWorker; } catch (error) { if (error.name !== "SecurityError") throw error; }
        if (workers) workers.register = async () => { throw new Error("Service workers are blocked in interaction fixtures"); };
    });
    const page = await context.newPage();
    await page.addInitScript(() => { if (window === window.top) localStorage.setItem("steve.ui.locale", "zh"); });
    page.setDefaultTimeout(2500);
    await page.clock.install();
    const f = { page, context, calls: [], errors: [], releases: [], binding: null, enqueue: null, cancel: null, failBinding: false, failEnqueue: false, replyReads: 0 };
    page.on("pageerror", (e) => f.errors.push(String(e)));
    const conversations = [{ id: A, title: "Conversation A", project: "scratch" }, { id: B, title: "Conversation B", project: "home" }];
    f.conversations = conversations;
    const projects = [project("scratch"), project("home")];
    const replies = Object.fromEntries(conversations.map((c) => [c.id, Array.from({ length: history && c.id === A ? 35 : 1 }, (_, i) => ({ id: `${c.id}-${i}`, kind: "reply", conversation: c.id, at, text: `${c.title} history ${i}\n\nA retained answer with enough detail to occupy its own row.` }))]));
    f.replies = replies;
    const queue = running ? [{ id: "running-a", conversation: A, state: "running", input: "Running task", started_at: at, enqueued_at: at }] : [];
    const emit = (event) => page.evaluate((event) => window.emit(event), { at, conversation: A, ...event });
    f.emit = emit;
    f.startRunning = async () => {
        queue.push({ id: "design-running", conversation: A, state: "running", input: "Running layout check", started_at: at, enqueued_at: at });
        await emit({ kind: "console.queue" });
    };
    f.hold = (kind) => { const g = gate(); f[kind] = g.promise; f.releases.push(g.release); return g.release; };
    await page.route("**/*", async (route) => {
        const req = route.request(), url = new URL(req.url()), pathname = url.pathname;
        if (url.origin !== app.url) { f.errors.push(`Unexpected external request: ${url.origin}`); return route.abort(); }
        if (!["/state", "/usage", "/events", "/history"].includes(pathname) && !pathname.startsWith("/console/")) return route.continue();
        const input = req.postDataJSON();
        const call = { method: req.method(), path: pathname, ...input };
        if (req.method() !== "GET") f.calls.push(call);
        const initialization = pathname.match(/^\/console\/conversations\/([^/]+)\/initialize$/);
        const conversation = initialization ? decodeURIComponent(initialization[1]) : input?.conversation || url.searchParams.get("conversation") || A;
        let current = conversations.find((c) => c.id === conversation);
        if (pathname === "/usage") return route.fulfill({ json: usageResponse() });
        if (/^\/console\/tasks\/[^/]+$/.test(pathname)) {
            const id = decodeURIComponent(pathname.split("/")[3]);
            const task = f.snapshot?.tasks.find((task) => task.id === id);
            return task ? route.fulfill({ json: workDetail(task, f.snapshot.tasks, f.snapshot.plans) }) : route.fulfill({ status:404,body:"task not found" });
        }
        if (pathname === "/console/tasks") return route.fulfill({ json: { items: f.snapshot?.tasks || [], total: f.snapshot?.tasks.length || 0 } });
        if (pathname === "/console/plans") return route.fulfill({ json: { items: [], total: 0 } });

        if (pathname === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false } });
        if (pathname === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false, agent_count: 0 } });
        if (pathname === "/state") return route.fulfill({ json: f.snapshot = workState({ at, hub: { node: "test-node", started: at, version: "test" }, nodes: [], agents: [], tasks: [task("11", A, "scratch"), task("22", B, "home")], plans: [], projects, attempts: [], landings: [] }) });
        if (initialization && req.method() === "PUT") {
            if (f.binding) await f.binding;
            if (f.failBinding) return route.fulfill({ status: 503, json: { error: "Project binding unavailable" } });
            if (!current) { current = { id: conversation, title: "", project: "home" }; conversations.push(current); }
            current.project = input.project;
            return route.fulfill({ json: { ok: true } });
        }
        if (pathname === "/console/send") {
            if (input.input === "/cancel" && f.cancel) await f.cancel;
            return route.fulfill({ json: { reply: { kind: "reply", conversation, at, text: "Accepted" } } });
        }
        if (pathname === "/console/queue" && req.method() === "POST") {
            call.projectAtEnqueue = current?.project || "home";
            if (f.enqueue) await f.enqueue;
            if (f.failEnqueue) return route.fulfill({ status: 400, body: "Enqueue rejected" });
            return route.fulfill({ json: { id: `queued-${f.calls.length}`, key: input.command_id ? `client:${input.command_id}` : undefined, conversation, input: input.input, state: "queued", enqueued_at: at } });
        }
        if (pathname === "/console/replies") {
            f.replyReads++;
            const snapshot = structuredClone(replies[conversation] || []);
            if (f.replyResponse) await f.replyResponse;
            return route.fulfill({ json: { enabled: true, replies: snapshot } });
        }
        if (pathname === "/console/queue") return route.fulfill({ json: { submission_keys: true, queue: queue.filter((q) => q.conversation === conversation) } });
        if (pathname === "/console/context") return route.fulfill({ json: { enabled: true, context: { conversation, agents: [], project: { ...project(current?.project || "home"), bound: !!current } } } });
        if (pathname === "/console/conversations") return route.fulfill({ json: { enabled: true, conversations: conversations.map((c) => ({ ...c, count: replies[c.id]?.length || 0, running: running && c.id === A, last_at: at })) } });
        if (pathname.startsWith("/console/conversations/") && req.method() === "PUT") {
            const changed = conversations.find((c) => c.id === decodeURIComponent(pathname.slice("/console/conversations/".length)));
            if (changed) Object.assign(changed, input);
            return route.fulfill({ json: { ok: true } });
        }
        if (pathname === "/console/setup") return route.fulfill({ json: { enabled: true, setup: f.setup ?? { agent: "test-agent", node: "test-node", harness: "test", applied: true, instructions: "", sections: [], mcp_servers: [] } } });
        if (pathname === "/console/verbs" || pathname === "/console/suggest") return route.fulfill({ json: { verbs: [], suggestions: [] } });
        f.errors.push(`Unhandled API: ${req.method()} ${pathname}`);
        return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
    });
    await page.addInitScript((conversation) => {
        if (window !== window.top) return;
        if (!sessionStorage.getItem("steve.conversation")) sessionStorage.setItem("steve.conversation", conversation);
        window.sources = [];
        window.EventSource = class {
            addEventListener() {}
            constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); }
            close() { window.sources = window.sources.filter((s) => s !== this); }
        };
        window.emit = (ev) => { for (const source of window.sources) source.onmessage?.({ data: JSON.stringify(ev) }); };
    }, A);
    await page.goto(`${app.url}/#/console`);
    await page.locator("main header").getByText("Conversation A", { exact: true }).waitFor();
    await page.locator("main header").getByText("scratch · test-node", { exact: true }).waitFor();
    f.box = page.getByRole("textbox", { name: "消息", exact: true });
    f.queued = () => f.calls.filter((c) => c.path === "/console/queue" && c.method === "POST");
    f.pick = async (name) => { await page.getByRole("button", { name: new RegExp(`^Conversation ${name}`) }).click(); await page.locator("main header").getByText(`Conversation ${name}`, { exact: true }).waitFor(); };
    return f;
}

const checks = {
    // Editing a line already sent is not a second question: the send that
    // follows takes the thread back to that line, so the box says what it
    // will undo and the submission names it.
    async "edit-rewinds-the-thread"(f) {
        const sent = { id: "sent-1", conversation: A, exchange_id: "ex-1", kind: "sent", at, input: "原来的问题" };
        const answer = { id: "answer-1", conversation: A, exchange_id: "ex-1", kind: "reply", at, text: "原来的回答" };
        f.replies[A].push(sent, answer);
        await f.page.reload();
        // Until the conversation list arrives the header falls back to the
        // first sent line, so wait on the header and the unique answer.
        await f.page.locator("main header").getByText("Conversation A", { exact: true }).waitFor();
        await f.page.getByText("原来的回答", { exact: true }).waitFor();

        await f.box.click();
        await f.page.keyboard.type("没发出去的草稿");
        await eventually(async () => await draftOf(f.box) === "没发出去的草稿", "The draft must reach the editor");

        const edit = f.page.getByRole("button", { name: "改写这条消息，从这里重来" });
        await edit.click();
        // The box already holds text, so it is not thrown away silently.
        await f.page.getByRole("button", { name: "改写", exact: true }).click();
        await eventually(async () => await draftOf(f.box) === "原来的问题", "Editing must put the sent line back in the box");
        await f.page.getByText("发送后会从这条消息重来，它之后的 1 条对话会被移除，agent 也会重开一轮。", { exact: true }).waitFor();

        // Cancelling gives back what was being typed, so an edit costs nothing.
        await f.page.getByRole("button", { name: "取消改写" }).click();
        await eventually(async () => await draftOf(f.box) === "没发出去的草稿", "Cancelling must restore the earlier draft");
        assert.equal(await f.page.getByText("取消改写", { exact: true }).count(), 0, "Cancelling must take the notice away");

        await edit.click();
        await f.page.getByRole("button", { name: "改写", exact: true }).click();
        await eventually(async () => await draftOf(f.box) === "原来的问题", "Editing must put the sent line back in the box");
        await f.box.click();
        await f.page.keyboard.press("ControlOrMeta+a");
        await f.page.keyboard.type("改过的问题");
        await eventually(async () => await draftOf(f.box) === "改过的问题", "The rewritten line must reach the editor");
        f.replies[A] = f.replies[A].filter((r) => r.id !== sent.id && r.id !== answer.id);
        await f.page.keyboard.press("Enter");

        await eventually(async () => f.queued().length === 1, "The rewritten line must be submitted");
        const call = f.queued()[0];
        assert.equal(call.input, "改过的问题");
        assert.equal(call.rewind_to, sent.id, "The submission must name the line it replaces");
        await eventually(async () => await f.page.getByText("取消改写", { exact: true }).count() === 0, "A sent rewrite must clear the notice");
        // The removed lines come back from the hub's own reading of the
        // thread, so this waits for that read rather than the click.
        await eventually(async () => await f.page.getByText("原来的回答", { exact: true }).count() === 0, "The answer to the replaced line must leave the transcript");
    },
    async "channel-milestone-lifecycle"(f) {
        const milestone = { id: "progress-side", conversation: A, exchange_id: "exchange-side", kind: "milestone", at, text: "First channel milestone", format: "markdown", title: "builder · test" };
        f.replies[A].push(milestone);
        await f.emit({ ...milestone, reply_id: milestone.id, kind: "console.milestone" });
        await f.page.getByText(milestone.text, { exact: true }).waitFor();
        milestone.text = "Updated channel milestone";
        await f.emit({ ...milestone, reply_id: milestone.id, kind: "console.milestone" });
        await f.page.getByText(milestone.text, { exact: true }).waitFor();
        assert.equal(await f.page.getByText("First channel milestone", { exact: true }).count(), 0, "Update must replace the original milestone");
        assert.equal(await f.page.getByText(milestone.text, { exact: true }).count(), 1, "Update must retain one milestone line");
        await f.page.reload();
        await f.page.getByText(milestone.text, { exact: true }).waitFor();
        await f.emit({ kind: "console.recalled", conversation: B, reply_id: milestone.id });
        assert.equal(await f.page.getByText(milestone.text, { exact: true }).count(), 1, "Another conversation cannot recall this line");
        f.replies[A] = f.replies[A].filter((r) => r.id !== milestone.id);
        await f.emit({ kind: "console.recalled", reply_id: milestone.id });
        await eventually(async () => await f.page.getByText(milestone.text, { exact: true }).count() === 0, "Recall must remove its milestone immediately");
        await f.page.reload();
        await f.page.getByText("Conversation A history 0", { exact: false }).waitFor();
        assert.equal(await f.page.getByText(milestone.text, { exact: true }).count(), 0, "Refresh must keep the recalled milestone absent");
        assert.equal(f.calls.length, 0, "Displaying lifecycle events must never submit work");
    },
    async "channel-message-format"(f) {
        const literal = "**Literal text**\n[not a link](https://example.test)";
        const text = { id: "plain-progress", conversation: A, kind: "milestone", at, text: literal, format: "text" };
        f.replies[A].push(text);
        await f.emit({ ...text, reply_id: text.id, kind: "console.milestone" });
        const message = f.page.locator(".message-assistant").filter({ hasText: "Literal text" });
        await message.waitFor();
        assert.ok((await message.innerText()).includes(literal), "Text format must preserve Markdown punctuation and newlines");
        assert.equal(await message.locator("a,strong").count(), 0, "Text format must not parse Markdown");
        await f.page.reload();
        await message.waitFor();
        assert.ok((await message.innerText()).includes(literal), "Reload must retain plain-text format");
        await f.emit({ kind: "console.milestone", reply_id: "legacy-progress", text: "**Default markdown**" });
        await f.page.locator(".message-assistant strong").getByText("Default markdown", { exact: true }).waitFor();
    },
    async "channel-milestone-late-snapshot"(f) {
        const milestone = { id: "late-progress", conversation: A, kind: "milestone", at, text: "Before update" };
        f.replies[A].push(milestone);
        await f.emit({ ...milestone, reply_id: milestone.id, kind: "console.milestone" });
        await f.page.getByText(milestone.text, { exact: true }).waitFor();
        for (const action of ["update", "recall"]) {
            const release = f.hold("replyResponse");
            const reads = f.replyReads;
            await f.page.clock.runFor(10100);
            await eventually(() => f.replyReads > reads, "Polling must capture a snapshot before the next event");
            if (action === "update") {
                milestone.text = "After update";
                await f.emit({ ...milestone, reply_id: milestone.id, kind: "console.milestone" });
                await f.page.getByText(milestone.text, { exact: true }).waitFor();
            } else {
                f.replies[A] = f.replies[A].filter((r) => r.id !== milestone.id);
                await f.emit({ kind: "console.recalled", reply_id: milestone.id });
                await eventually(async () => await f.page.getByText(milestone.text, { exact: true }).count() === 0, "Recall must update the page while polling waits");
            }
            f.replyResponse = null;
            release();
            await delay(200);
            assert.equal(await f.page.getByText("Before update", { exact: true }).count(), 0, "Late snapshots must not undo a milestone update");
            assert.equal(await f.page.getByText("After update", { exact: true }).count(), action === "update" ? 1 : 0, "Late snapshots must not resurrect a recalled milestone");
        }
    },
    async "channel-milestone-snapshot-convergence"(f) {
        const milestone = { id: "converging-progress", conversation: A, kind: "milestone", at, text: "Initial snapshot" };
        f.replies[A].push(milestone);
        await f.emit({ ...milestone, reply_id: milestone.id, kind: "console.milestone" });
        await f.page.getByText(milestone.text, { exact: true }).waitFor();
        const releaseFirst = f.hold("replyResponse"), reads = f.replyReads;
        await f.page.clock.runFor(10100);
        await eventually(() => f.replyReads > reads, "Polling must begin");
        milestone.text = "First concurrent update";
        await f.emit({ ...milestone, reply_id: milestone.id, kind: "console.milestone" });
        await f.page.getByText(milestone.text, { exact: true }).waitFor();
        const releaseRetry = f.hold("replyResponse");
        releaseFirst();
        await eventually(() => f.replyReads === reads + 2, "A stale snapshot should trigger one fresh read");
        milestone.text = "Latest concurrent update";
        await f.emit({ ...milestone, reply_id: milestone.id, kind: "console.milestone" });
        await f.page.getByText(milestone.text, { exact: true }).waitFor();
        f.replyResponse = null;
        releaseRetry();
        await delay(200);
        assert.equal(f.replyReads, reads + 2, "Continuous events must not create an unbounded immediate retry loop");
        assert.equal(await f.page.getByText(milestone.text, { exact: true }).count(), 1, "The latest event must survive both stale responses");
        await f.page.clock.runFor(10100);
        await eventually(() => f.replyReads > reads + 2, "The next normal poll must resume snapshot recovery");
        const releaseSwitch = f.hold("replyResponse"), beforeSwitch = f.replyReads;
        await f.page.clock.runFor(10100);
        await eventually(() => f.replyReads > beforeSwitch, "A snapshot must be in flight during the conversation switch");
        await f.pick("B");
        f.replyResponse = null;
        releaseSwitch();
        await f.page.getByText("Conversation B history 0", { exact: false }).waitFor();
        assert.equal(await f.page.getByText(milestone.text, { exact: true }).count(), 0, "Late A snapshots must not enter B");
        await f.pick("A");
        await f.page.getByText(milestone.text, { exact: true }).waitFor();
    },
    async "project-inheritance"(f) {
        await f.page.getByRole("button", { name: "新会话", exact: true }).click();
        await eventually(() => f.calls.some((c) => c.path.endsWith("/initialize") && c.project === "scratch"), "Generic new conversation must bind the current project");
        await f.page.locator("main header").getByText("新会话", { exact: true }).waitFor();
        assert.equal(await draftOf(f.box), "", "New conversation title must not populate the composer");
        assert.equal(f.calls.filter((c) => c.path === "/console/send").length, 0, "Automatic binding must not send a chat command");
        await f.box.fill("First work");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "First work must be accepted after binding");
        assert.equal(f.queued()[0].projectAtEnqueue, "scratch");
        const id = f.queued()[0].conversation;
        f.replies[id] = [{ id: "first-work", kind: "sent", conversation: id, input: "First work", at }];
        await f.emit({ kind: "console.sent", conversation: id, reply_id: "first-work", text: "First work" });
        await f.page.locator("main header").getByText("First work", { exact: true }).waitFor();
    },
    async "new-session-title-i18n"(f) {
        await f.page.getByRole("button", { name: "新会话", exact: true }).click();
        await eventually(() => f.conversations.length === 3, "Initialization must create an empty conversation");
        await f.page.locator("main header").getByText("新会话", { exact: true }).waitFor();
        const id = f.conversations.at(-1).id;
        assert.equal(await draftOf(f.box), "", "The title placeholder must not become a draft");
        // Old explicit controls remain readable, but never become the title.
        f.replies[id] = [{ id: "old-control", kind: "sent", conversation: id, input: "/project use scratch", at }];
        await f.page.reload();
        await f.page.locator("main header").getByText("新会话", { exact: true }).waitFor();
        await f.page.getByText("/project use scratch", { exact: true }).waitFor();
        await f.page.evaluate(() => {
            localStorage.setItem("steve.ui.locale", "en");
            window.dispatchEvent(new StorageEvent("storage", { key: "steve.ui.locale", newValue: "en", storageArea: localStorage }));
        });
        await f.page.locator("main header").getByText("New conversation", { exact: true }).waitFor();
        assert.equal(await draftOf(f.page.getByRole("textbox", { name: "Message", exact: true })), "");
    },
    async "binding-pending"(f) {
        const release = f.hold("binding");
        await f.page.getByRole("button", { name: "在 scratch 下新会话", exact: true }).click();
        await eventually(() => f.calls.some((c) => c.path.endsWith("/initialize") && c.project === "scratch"), "Project binding must begin");
        if (await f.box.isEnabled()) { await f.box.fill("First work during binding"); await f.box.press("Enter"); }
        await delay(150);
        assert.equal(f.queued().length, 0, "Work must not be submitted before project binding returns");
        release();
        await eventually(() => f.box.isEnabled(), "Composer must recover when binding completes");
        if (!f.queued().length) { await f.box.fill("First work after binding"); await f.box.press("Enter"); }
        await eventually(() => f.queued().length === 1, "Work should be accepted after binding");
        assert.equal(f.queued()[0].projectAtEnqueue, "scratch");
    },
    async "binding-failure"(f) {
        f.failBinding = true;
        await f.box.fill("Keep this existing draft");
        await f.page.getByRole("button", { name: "在 scratch 下新会话", exact: true }).click();
        await eventually(() => f.calls.some((c) => c.path.endsWith("/initialize") && c.project === "scratch"), "Project binding must begin");
        await delay(150);
        assert.equal(await draftOf(f.box), "Keep this existing draft", "Binding failure must preserve the original draft");
        assert.equal(await f.page.locator("main header").getByText("Conversation A", { exact: true }).count(), 1, "Binding failure must retain the original conversation");
        await f.page.reload();
        await f.box.waitFor();
        await f.page.locator("main header").getByText("Conversation A", { exact: true }).waitFor();
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Original draft remains usable after failed creation and reload");
        assert.equal(f.queued()[0].conversation, A, "Reload must not activate an unbound new conversation");
        assert.equal(f.queued()[0].projectAtEnqueue, "scratch");
    },
    async "draft-switch"(f) {
        await f.box.fill("Draft for A");
        await f.pick("B");
        assert.equal(await draftOf(f.box), "", "A draft must not leak into B");
        await f.box.fill("Draft for B");
        await f.pick("A");
        assert.equal(await draftOf(f.box), "Draft for A");
        await f.pick("B");
        assert.equal(await draftOf(f.box), "Draft for B");
    },
    async "draft-persistence"(f) {
        await f.box.fill("Unsent work survives navigation");
        await f.page.locator('a[href="#/projects"]').click();
        await f.page.getByRole("button", { name: "添加项目", exact: true }).waitFor();
        await f.page.locator('a[href="#/console"]').click();
        assert.equal(await draftOf(f.box), "Unsent work survives navigation", "Leaving the console must preserve its draft");
        await f.page.reload();
        await f.box.waitFor();
        assert.equal(await draftOf(f.box), "Unsent work survives navigation", "Reload must preserve the unsent draft");
    },
    async "send-continuation"(f) {
        const release = f.hold("enqueue");
        await f.box.fill("First instruction");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Enqueue request must start");
        await f.box.pressSequentially("Next draft");
        assert.equal(await draftOf(f.box), "Next draft", "Typing during a slow enqueue must begin a fresh draft");
        release();
        await delay(150);
        assert.equal(await draftOf(f.box), "Next draft", "The delayed receipt must not clear the new draft");
        assert.equal(f.queued()[0].input, "First instruction");
    },
    async "send-failure"(f) {
        const release = f.hold("enqueue");
        f.failEnqueue = true;
        await f.box.fill("Unsent first instruction");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Enqueue request must start");
        await f.box.pressSequentially("New draft typed during submission");
        release();
        await f.page.getByText("Enqueue rejected", { exact: true }).waitFor();
        assert.match(await draftOf(f.box), /^Unsent first instruction\n+New draft typed during submission$/, "Failed submission must restore the first instruction without losing newer typing");
        assert.equal(f.queued().length, 1, "Failed submission must not retry automatically");
    },
    async "send-switch"(f) {
        const release = f.hold("enqueue");
        await f.box.fill("A instruction");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Enqueue request must start");
        await f.pick("B");
        await f.box.fill("B unsent draft");
        release();
        await delay(150);
        assert.equal(await draftOf(f.box), "B unsent draft", "A's receipt must not alter B's draft");
        await f.pick("A");
        assert.equal(await draftOf(f.box), "", "A's submitted instruction must not reappear as a draft");
    },
    async "send-unmount-failure"(f) {
        const release = f.hold("enqueue");
        f.failEnqueue = true;
        await f.box.fill("Original instruction");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Enqueue request must start");
        await f.page.locator('a[href="#/projects"]').click();
        await f.page.getByRole("button", { name: "添加项目", exact: true }).waitFor();
        await f.page.locator('a[href="#/console"]').click();
        await f.box.fill("New draft after remount");
        release();
        await delay(200);
        assert.match(await draftOf(f.box), /New draft after remount/, "An old failure must not overwrite the new mounted draft");
        await f.page.reload();
        await f.box.waitFor();
        assert.match(await draftOf(f.box), /New draft after remount/, "Newer typing must remain persisted after an unmounted request fails");
        assert.match(await draftOf(f.box), /Original instruction/, "The failed instruction must remain recoverable");
        assert.equal(f.queued().length, 1, "Remounting must not automatically resend the failed instruction");
    },
    async "send-reload-pending"(f) {
        const release = f.hold("enqueue");
        await f.box.fill("Instruction before network delivery");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Enqueue request must start");
        await f.box.fill("Newer unsent draft");
        await f.page.reload();
        await f.box.waitFor();
        await f.page.getByText("发送结果尚未确认，请先查看会话记录", { exact: false }).waitFor();
        assert.equal(await f.page.getByText("Instruction before network delivery", { exact: true }).count(), 1, "Recovery must show the original unacknowledged instruction");
        assert.equal(await draftOf(f.box), "Newer unsent draft", "Reload must keep new typing separate from an uncertain submission");
        assert.equal(f.queued().length, 1, "Reload must not resend an instruction that may already have been accepted");
        await f.box.press("Enter");
        await delay(150);
        assert.equal(f.queued().length, 1, "Enter must not bypass unresolved submission recovery");
        assert.equal(await draftOf(f.box), "Newer unsent draft");
        assert.equal(await f.page.getByRole("button", { name: "恢复为草稿", exact: true }).count(), 0, "An unresolved keyed submission must retain idempotency protection");
        await f.page.getByRole("button", { name: "重试这次发送", exact: true }).click();
        await eventually(() => f.queued().length === 2, "Explicit retry must be submitted");
        assert.equal(f.queued()[0].command_id, f.queued()[1].command_id, "Retry must preserve the original identity");
        release();
        await eventually(async () => await f.page.getByRole("button", { name: "重试这次发送", exact: true }).count() === 0, "Receipt must settle the pending submission");
        assert.equal(await draftOf(f.box), "Newer unsent draft", "Retry must preserve newer typing");
    },
    async "recovery-storage-failure"(f) {
        f.hold("enqueue");
        await f.box.fill("Original uncertain instruction");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Enqueue request must start");
        await f.box.fill("Newer persisted draft");
        await f.page.reload();
        const recover = f.page.getByRole("button", { name: "重试这次发送", exact: true });
        await recover.waitFor();
        await f.page.evaluate(() => {
            Storage.prototype.setItem = () => { throw new DOMException("Quota exceeded", "QuotaExceededError"); };
        });
        await recover.click();
        await f.page.reload();
        await f.box.waitFor();
        await recover.waitFor();
        assert.equal(await f.page.getByText("Original uncertain instruction", { exact: true }).count(), 1, "Failed draft persistence must retain the original pending submission");
        assert.equal(await draftOf(f.box), "Newer persisted draft", "Failed recovery must preserve the previously saved draft");
        assert.equal(f.queued().length, 1, "Failed recovery must not submit work");
    },
    async "quotes-unmount-failure"(f) {
        const release = f.hold("enqueue");
        f.failEnqueue = true;
        await f.page.getByRole("button", { name: "引用", exact: true }).click();
        await f.box.fill("Question about this quote");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Enqueue request must start");
        assert.deepEqual(f.queued()[0].quotes, [{ conversation: A, reply_id: `${A}-0` }]);
        await f.page.locator('a[href="#/projects"]').click();
        await f.page.getByRole("button", { name: "添加项目", exact: true }).waitFor();
        await f.page.locator('a[href="#/console"]').click();
        await f.box.waitFor();
        release();
        await eventually(() => f.page.getByRole("button", { name: "去掉引用", exact: true }).count(), "An unmounted failed submission must restore its quote in the current composer");
        assert.equal(await draftOf(f.box), "Question about this quote");
        assert.equal(f.queued().length, 1, "Restoring the quoted draft must not submit it automatically");
    },
    async "repeat-enter"(f) {
        f.hold("enqueue");
        await f.box.fill("Only once");
        await f.box.press("Enter");
        await f.box.press("Enter");
        await f.box.press("Enter");
        await delay(150);
        assert.equal(f.queued().length, 1, "Repeated Enter must enqueue the instruction only once");
    },
    async "stop-double-click"(f) {
        f.hold("cancel");
        const stop = f.page.getByRole("button", { name: "停止", exact: true });
        await stop.dblclick();
        await delay(150);
        assert.equal(f.calls.filter((c) => c.input === "/cancel").length, 1, "A double click must issue only one cancellation");
    },
    async "stop-with-draft"(f) {
        await f.box.fill("Draft written while a task is running");
        assert.equal(await f.page.getByRole("button", { name: "停止", exact: true }).count(), 1, "Stop must remain reachable while a draft is present");
    },
    async "stop-enter"(f) {
        f.hold("cancel");
        await f.page.getByRole("button", { name: "停止", exact: true }).click();
        await f.box.fill("New instruction during cancellation");
        assert.equal(await f.page.locator('button[aria-label="排队"]').isDisabled(), true, "Send must be disabled while cancellation is pending");
        await f.box.press("Enter");
        await delay(150);
        assert.equal(f.queued().length, 0, "Enter must obey the same pending-cancel guard as the send button");
        assert.equal(await draftOf(f.box), "New instruction during cancellation", "A blocked Enter must preserve the draft");
    },
    async "scroll-poll"(f) {
        const scroller = f.page.locator("main .overflow-y-auto.overflow-x-hidden");
        await scroller.hover();
        await f.page.mouse.wheel(0, -20000);
        await eventually(() => scroller.evaluate((el) => el.scrollTop < 20), "Mouse wheel must reach older history");
        const reads = f.replyReads;
        await f.page.clock.runFor(10500);
        await eventually(() => f.replyReads > reads, "Real console polling timer must fetch replies");
        await delay(150);
        assert.ok(await scroller.evaluate((el) => el.scrollTop < 20), "Unchanged polling must not pull the reader to the bottom");
    },
    async "scroll-stream"(f) {
        const scroller = f.page.locator("main .overflow-y-auto.overflow-x-hidden");
        await scroller.hover();
        await f.page.mouse.wheel(0, -20000);
        await eventually(() => scroller.evaluate((el) => el.scrollTop < 20), "Mouse wheel must reach older history");
        await f.emit({ kind: "console.progress", exchange_id: "running-a", progress: { reasoning: "Another streamed reasoning update" } });
        await delay(150);
        assert.ok(await scroller.evaluate((el) => el.scrollTop < 20), "Stream updates must preserve the manual reading position");
    },
    async "task-navigation"(f) {
        await f.page.getByRole("button", { name: "看板", exact: true }).click();
        await f.page.getByRole("button", { name: "打开任务 #22 Task 22", exact: true }).click();
        await f.page.getByRole("dialog").getByRole("button", { name: "在工作台里看", exact: true }).click();
        await eventually(() => f.page.locator("main header").getByText("Conversation B", { exact: true }).isVisible(), "Opening B's task must navigate to conversation B");
        assert.equal(f.queued().length, 0, "Viewing a task must not submit /tasks into the current conversation");
    },
    async "task-action"(f) {
        await f.page.getByRole("button", { name: "看板", exact: true }).click();
        await f.page.getByRole("button", { name: "打开任务 #22 Task 22", exact: true }).click();
        await f.page.getByRole("dialog").getByRole("button", { name: "暂停", exact: true }).click();
        await eventually(() => f.calls.some((c) => c.input === "/tasks pause 22"), "Task pause command must be submitted");
        assert.equal(f.calls.find((c) => c.input === "/tasks pause 22").conversation, B, "Task action must target its owning conversation");
    },
    async "task-logical-failure"(f) {
        const rejection = "任务 #22 现在是「失败」，这一步做不了。";
        await f.page.route("**/console/send", (route) => route.fulfill({ json: { reply: { kind: "reply", conversation: B, at, text: rejection } } }));
        await f.page.getByRole("button", { name: "看板", exact: true }).click();
        await f.page.getByRole("button", { name: "打开任务 #22 Task 22", exact: true }).click();
        await f.page.getByRole("dialog").getByRole("button", { name: "暂停", exact: true }).click();
        await f.page.getByRole("dialog").getByText(rejection, { exact: true }).waitFor();
    },
    async "intent-remount-draft"(f) {
        await f.page.getByRole("button", { name: "看板", exact: true }).click();
        await f.page.getByRole("button", { name: /^(新计划|新建计划)$/ }).click();
        await eventually(async () => (await draftOf(f.box)).startsWith("/plan"), "New plan must fill the composer");
        await f.box.fill("Plan carefully drafted after fill");
        await f.page.locator('a[href="#/projects"]').click();
        await f.page.getByRole("button", { name: "添加项目", exact: true }).waitFor();
        await f.page.locator('a[href="#/console"]').click();
        assert.equal(await draftOf(f.box), "Plan carefully drafted after fill", "An already-consumed fill intent must not overwrite the persisted draft");
    },
    async "conversation-rename-once"(f) {
        const edit = async (title) => {
            await f.page.getByRole("button", { name: new RegExp(`^${title}`) }).locator("xpath=ancestor::li[1]").getByRole("button", { name: "更多", exact: true }).click();
            await f.page.getByRole("menuitem", { name: "重命名", exact: true }).click();
            return f.page.getByRole("textbox", { name: "会话名称", exact: true });
        };
        const name = await edit("Conversation A");
        await name.fill("Renamed with Enter");
        await name.press("Enter");
        await f.box.click();
        await f.page.locator("main header").getByText("Renamed with Enter", { exact: true }).waitFor();
        const writes = () => f.calls.filter((c) => c.method === "PUT" && c.path.startsWith("/console/conversations/"));
        assert.equal(writes().length, 1, "Enter followed by blur must rename only once");
        const blurred = await edit("Renamed with Enter");
        await blurred.fill("Renamed with blur");
        await f.box.click();
        await f.page.locator("main header").getByText("Renamed with blur", { exact: true }).waitFor();
        assert.equal(writes().length, 2, "A blur-only edit must issue exactly one additional rename");
    },
    async "reasoning-preferences"(f) {
        let reads = 0, rejectRead = true, rejectSave = false, empty = false;
        const selected = "medium";
        const preferences = new Map();
        const discovery = gate(); f.releases.push(discovery.release);
        await f.page.route("**/console/context?*", (route) => {
            const conversation = new URL(route.request().url()).searchParams.get("conversation");
            return route.fulfill({ json: { enabled: true, context: { conversation, project: { ...project("scratch"), bound: true }, agents: [], agent: { id: "test-agent", node: "test-node", harness: "codex", model: "gpt-6-astra", ready: true, usable: true } } } });
        });
        await f.page.route("**/console/selectors?*", async (route) => {
            reads++;
            await discovery.promise;
            if (rejectRead) return route.fulfill({ status: 503, body: "Choices unavailable" });
            if (empty) return route.fulfill({ json: {} }); // Go omits empty option arrays.
            const conversation = new URL(route.request().url()).searchParams.get("conversation");
            const preferred = preferences.get(conversation) || {};
            return route.fulfill({ json: { model: preferred.model || "gpt-6-astra", preferred, models: [{ Value: "no-effort", Label: "Model without effort" }], options: preferred.model === "no-effort" ? [] : [{ ID: "reasoning_effort", Name: "Reasoning effort", Category: "thought_level", Current: preferred.reasoning_effort || selected, Choices: [{ Value: "low", Label: "Low" }, { Value: "medium", Label: "Medium" }, { Value: "high", Label: "High" }] }] } });
        });
        await f.page.route("**/console/preferences", (route) => {
            const input = route.request().postDataJSON();
            f.calls.push({ method: "PUT", path: "/console/preferences", ...input });
            if (rejectSave) return route.fulfill({ status: 503, body: "Preferences unavailable" });
            preferences.set(input.conversation, { ...preferences.get(input.conversation), ...input.patch });
            return route.fulfill({ json: { ok: true } });
        });
        await f.page.reload();
        const chip = f.page.getByRole("button", { name: "思考强度", exact: true });
        await chip.waitFor();
        assert.equal(reads, 0, "Rendering the effort chip must not open a harness session");
        await f.page.setViewportSize({ width: 560, height: 900 });
        await f.page.clock.runFor(350);
        await visibleControl(chip, "Reasoning chip in a narrow window");
        await noHorizontalOverflow(f.page);
        await f.page.setViewportSize({ width: 1600, height: 1000 });
        await chip.focus(); await f.page.keyboard.press("Enter");
        await f.page.getByText("读取可选项…", { exact: true }).waitFor();
        discovery.release();
        await f.page.getByRole("alert").getByText("Choices unavailable", { exact: true }).waitFor();
        await f.page.keyboard.press("Escape");
        rejectRead = false;
        await chip.click();
        await f.page.getByRole("menuitem", { name: "High", exact: true }).click();
        await eventually(async () => (await chip.innerText()).includes("High"), "Saved effort must appear on the chip");
        const write = f.calls.find((c) => c.path === "/console/preferences");
        assert.deepEqual(write.patch, { reasoning_effort: "high" });
        assert.equal(write.conversation, A); assert.equal(write.agent, "test-agent");
        rejectSave = true;
        await chip.click(); await f.page.getByRole("menuitem", { name: "Low", exact: true }).click();
        await f.page.getByRole("alert").getByText("Preferences unavailable", { exact: true }).waitFor();
        assert.ok((await chip.innerText()).includes("High"), "Rejected save must preserve the previous effort");
        rejectSave = false; rejectRead = true;
        await chip.click(); await f.page.getByRole("menuitem", { name: "Low", exact: true }).click();
        await f.page.getByRole("alert").getByText("Choices unavailable", { exact: true }).waitFor();
        rejectRead = false;
        await chip.click(); await f.page.getByRole("menuitem", { name: "Low", exact: true }).waitFor();
        assert.ok((await chip.innerText()).includes("Low"), "Reopening after a failed refresh must recover the saved effort");
        await f.page.keyboard.press("Escape");
        await f.pick("B");
        assert.equal(await chip.innerText(), "思考强度", "A different conversation must not inherit cached choices");
        await chip.click(); await f.page.getByRole("menuitem", { name: "Medium", exact: true }).waitFor();
        assert.ok((await chip.innerText()).includes("Medium"));
        await f.page.keyboard.press("Escape");
        await f.pick("A");
        await chip.click(); await f.page.getByRole("menuitem", { name: "Low", exact: true }).waitFor();
        assert.ok((await chip.innerText()).includes("Low"), "Returning must reload the conversation's saved effort");
        await f.page.keyboard.press("Escape");
        await f.page.getByRole("button", { name: "模型", exact: true }).click();
        await f.page.getByRole("menuitem", { name: "Model without effort", exact: true }).click();
        await eventually(async () => await chip.innerText() === "思考强度", "Model change must discard the previous model's effort choices");
        await chip.click();
        await f.page.getByText("当前工具或模型不支持选择思考强度", { exact: true }).waitFor();
        await f.page.keyboard.press("Escape");
        empty = true;
        await f.page.reload();
        await chip.click();
        await f.page.getByText("当前工具或模型不支持选择思考强度", { exact: true }).waitFor();
        await f.page.keyboard.press("Escape");
        await f.page.getByRole("button", { name: "模型", exact: true }).click();
        await f.page.getByText("这个 AI 工具没有暴露模型选择", { exact: true }).waitFor();
        await f.page.keyboard.press("Escape");

    },
    async "approval-mode-preference"(f) {
        // The harness exposes its approval behaviour as the "mode" selector;
        // the composer must offer it beside the model and reasoning chips,
        // save it as a preference, and say so when a tool has no such mode.
        let modes = true;
        const preferences = new Map();
        await f.page.route("**/console/context?*", (route) => {
            const conversation = new URL(route.request().url()).searchParams.get("conversation");
            return route.fulfill({ json: { enabled: true, context: { conversation, project: { ...project("scratch"), bound: true }, agents: [], agent: { id: "test-agent", node: "test-node", harness: "codex", model: "gpt-6-astra", ready: true, usable: true } } } });
        });
        await f.page.route("**/console/selectors?*", (route) => {
            const conversation = new URL(route.request().url()).searchParams.get("conversation");
            const preferred = preferences.get(conversation) || {};
            const options = [{ ID: "reasoning_effort", Name: "Reasoning effort", Category: "thought_level", Current: "high", Choices: [{ Value: "low", Label: "Low" }, { Value: "high", Label: "High" }] }];
            if (modes) options.unshift({ ID: "mode", Name: "Mode", Category: "mode", Current: preferred.mode || "read-only", Choices: [{ Value: "read-only", Label: "Ask for approval" }, { Value: "agent", Label: "Approve for me" }, { Value: "agent-full-access", Label: "Full access" }] });
            return route.fulfill({ json: { model: "gpt-6-astra", preferred, models: [{ Value: "gpt-6-astra", Label: "gpt-6-astra" }], options } });
        });
        await f.page.route("**/console/preferences", (route) => {
            const input = route.request().postDataJSON();
            f.calls.push({ method: "PUT", path: "/console/preferences", ...input });
            preferences.set(input.conversation, { ...preferences.get(input.conversation), ...input.patch });
            return route.fulfill({ json: { ok: true } });
        });
        await f.page.reload();
        const chip = f.page.getByRole("button", { name: "审批", exact: true });
        await chip.waitFor();
        assert.equal(await chip.innerText(), "审批", "Before discovery the chip names only the selector");
        await chip.click();
        await f.page.getByText("审批 · 当前 Ask for approval", { exact: true }).waitFor();
        await f.page.getByRole("menuitem", { name: "Full access", exact: true }).click();
        await eventually(async () => (await chip.innerText()).includes("Full access"), "Saved approval mode must appear on the chip");
        const write = f.calls.find((c) => c.path === "/console/preferences");
        assert.deepEqual(write.patch, { mode: "agent-full-access" });
        assert.equal(write.conversation, A); assert.equal(write.agent, "test-agent");
        const effort = f.page.getByRole("button", { name: "思考强度", exact: true });
        assert.ok((await effort.innerText()).includes("High"), "The reasoning chip must keep its own selector");
        await f.pick("B");
        assert.equal(await chip.innerText(), "审批", "A different conversation must not inherit the approval choice");
        modes = false;
        await chip.click();
        await f.page.getByText("当前工具不支持切换审批模式", { exact: true }).waitFor();
        await f.page.keyboard.press("Escape");
    },
    async "preferences-save-retry"(f) {
        let model = "model-one", reject = false;
        await f.page.route("**/console/context?*", (route) => route.fulfill({ json: { enabled: true, context: { conversation: A, project: { ...project("scratch"), bound: true }, agents: [], agent: { id: "test-agent", node: "test-node", harness: "test", model, ready: true, usable: true } } } }));
        await f.page.route("**/console/selectors?*", (route) => route.fulfill({ json: { model, preferred: { model }, models: [{ Value: "model-one", Label: "Model One" }, { Value: "model-two", Label: "Model Two" }], options: [] } }));
        await f.page.route("**/console/preferences", (route) => {
            const input = route.request().postDataJSON();
            f.calls.push({ method: "PUT", path: "/console/preferences", ...input });
            if (reject) return route.fulfill({ status: 503, body: "Preferences unavailable" });
            model = input.patch.model;
            return route.fulfill({ json: { ok: true, note: "下一轮以新会话开始" } });
        });
        await f.page.reload();
        const chip = f.page.getByRole("button", { name: "模型", exact: true });
        const choose = async (name) => { await chip.click(); await f.page.getByRole("menuitem", { name, exact: true }).click(); };
        await choose("Model Two");
        await eventually(async () => await chip.innerText() === "model-two", "Successful preference save must update the model chip");
        reject = true;
        await choose("Model One");
        await f.page.getByText("Preferences unavailable", { exact: true }).first().waitFor();
        assert.equal(await chip.innerText(), "model-two", "A rejected save must retain the last successful model");
        reject = false;
        await choose("Model One");
        await eventually(async () => await chip.innerText() === "model-one", "Reopening after an error must allow a successful retry");
        const writes = f.calls.filter((c) => c.path === "/console/preferences");
        assert.equal(writes.length, 3);
        assert.ok(writes.every((c) => c.conversation === A && c.agent === "test-agent"), "Preference changes must target the current conversation and agent");
    },
};

async function noHorizontalOverflow(page) {
    const size = await page.evaluate(() => ({ document: document.documentElement.scrollWidth, viewport: window.innerWidth }));
    assert.ok(size.document <= size.viewport + 1, `Document overflows horizontally: ${JSON.stringify(size)}`);
}

async function visibleControl(locator, label) {
    assert.ok(await locator.isVisible(), `${label} must be visible`);
    const rect = await locator.boundingBox();
    const viewport = locator.page().viewportSize();
    assert.ok(rect && rect.width > 20 && rect.height > 20 && rect.x >= -1 && rect.y >= -1 && rect.x + rect.width <= viewport.width + 1 && rect.y + rect.height <= viewport.height + 1, `${label} must fit in the viewport: ${JSON.stringify(rect)}`);
    assert.ok(await locator.evaluate((element) => {
        const rect = element.getBoundingClientRect();
        const top = document.elementFromPoint(rect.x + rect.width / 2, rect.y + rect.height / 2);
        return top === element || element.contains(top);
    }), `${label} must not be covered by navigation or inspector`);
    return rect;
}

const inspector = (page) => page.getByRole("complementary").filter({ has: page.getByRole("tab", { name: "会话", exact: true }) });
async function composerGeometry(f) {
    await noHorizontalOverflow(f.page);
    const message = await visibleControl(f.box, "Message");
    const send = await visibleControl(f.page.locator('button[aria-label="发送"], button[aria-label="排队"]'), "Send");
    const stop = f.page.getByRole("button", { name: "停止", exact: true });
    if (await stop.isVisible()) await visibleControl(stop, "Stop");
    assert.ok(send.x >= message.x - 12 && send.x + send.width <= message.x + message.width + 12, `Send must remain in the composer column: ${JSON.stringify({ message, send })}`);
    const detail = inspector(f.page);
    if (await detail.isVisible()) {
        const panel = await detail.boundingBox();
        assert.ok(message.x + message.width <= panel.x + 1 || message.x >= panel.x + panel.width - 1, "Message must not extend into a docked inspector");
        assert.ok(send.x + send.width <= panel.x + 1 || send.x >= panel.x + panel.width - 1, "Send must not extend into a docked inspector");
    }
}

for (const width of [1280, 1024, 390]) {
    checks[`design-layout-${width}`] = async (f) => {
        await f.page.setViewportSize({ width, height: 900 });
        await f.box.fill("A draft that keeps the composer actions visible");
        await composerGeometry(f);
        await f.startRunning();
        await f.page.getByRole("button", { name: "停止", exact: true }).waitFor();
        await composerGeometry(f);
        await f.page.screenshot({ path: path.join(output, `design-layout-${width}.png`) });
    };
}

checks["design-mobile-sessions"] = async (f) => {
    await f.page.setViewportSize({ width: 390, height: 844 });
    await f.box.fill("Mobile draft A");
    const open = f.page.getByRole("button", { name: /^(会话列表|打开会话列表)$/ });
    await visibleControl(open, "Open conversation navigation");
    await open.click();
    await visibleControl(f.page.getByRole("button", { name: /^Conversation B/ }), "Conversation B");
    await f.pick("B");
    await visibleControl(f.box, "Message after selecting B");
    assert.equal(await draftOf(f.box), "", "Mobile selection must switch to B's own draft");
    await open.click();
    await f.pick("A");
    assert.equal(await draftOf(f.box), "Mobile draft A");
    await open.click();
    await f.page.getByRole("button", { name: /^(关闭会话列表|关闭会话导航)$/ }).click();
    await visibleControl(f.box, "Message after dismissing conversation navigation");
    await noHorizontalOverflow(f.page);
};

checks["design-mobile-current-session"] = async (f) => {
    await f.page.setViewportSize({ width: 390, height: 844 });
    await f.box.fill("Current mobile conversation draft");
    await f.page.getByRole("button", { name: "会话列表", exact: true }).click();
    const sheet = f.page.getByRole("dialog", { name: "会话列表", exact: true });
    await sheet.getByRole("button", { name: /^Conversation A/ }).click();
    await eventually(async () => await sheet.count() === 0, "Selecting the current conversation must dismiss the mobile navigation sheet");
    await visibleControl(f.box, "Message after reselecting the current conversation");
    assert.equal(await draftOf(f.box), "Current mobile conversation draft");
};

checks["design-mobile-child"] = async (f) => {
    const tasks = [task("11", A, "scratch"), { ...task("33", A, "scratch"), parent: "11" }];
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState({ at, hub: { node: "test-node", started: at, version: "test" }, nodes: [], agents: [], tasks, plans: [], projects: [project("scratch"), project("home")], attempts: [], landings: [] }) }));
    await f.page.route("**/console/replies?*", (route) => route.fulfill({ json: { enabled: true, replies: [{ id: "parent-reply", kind: "reply", conversation: A, at, text: "Parent reply", process: { steps: [{ id: "#33", kind: "delegate", goal: "Delegated work", state: "done", answer: "Child answer" }] } }] } }));
    await f.page.reload();
    await f.box.waitFor();
    await f.page.setViewportSize({ width: 390, height: 844 });
    await f.box.fill("Draft before viewing a child");
    await f.page.getByRole("button", { name: "会话列表", exact: true }).click();
    const sheet = f.page.getByRole("dialog", { name: "会话列表", exact: true });
    await sheet.getByRole("button", { name: "展开 Conversation A 的任务", exact: true }).click();
    await sheet.getByRole("button", { name: /#33.*Task 33/ }).click();
    await eventually(async () => await sheet.count() === 0, "Opening a child task must dismiss mobile conversation navigation");
    await f.page.getByText("Child answer", { exact: true }).waitFor();
    await f.page.getByRole("button", { name: /回到对话/ }).click();
    await visibleControl(f.box, "Message after returning from a child task");
    assert.equal(await draftOf(f.box), "Draft before viewing a child");
};

checks["design-connection-compact"] = async (f) => {
    await f.page.setViewportSize({ width: 1024, height: 900 });
    const connected = f.page.getByRole("status", { name: /^(已连接|实时)$/ });
    await connected.waitFor();
    assert.ok(await connected.isVisible(), "Compact navigation must show the connected state");
    await f.page.evaluate(() => window.sources.forEach((source) => source.onerror?.()));
    const reconnecting = f.page.getByRole("status", { name: /^(正在连接|重连中)$/ });
    await reconnecting.waitFor();
    const rect = await reconnecting.boundingBox();
    assert.ok(rect && rect.width > 0 && rect.height > 0 && rect.x >= 0 && rect.x + rect.width <= 1024 && rect.y + rect.height <= 900, "Connection changes must remain visible inside compact navigation");
};

checks["compact-settings-alignment"] = async (f) => {
    await f.page.getByRole("button", { name: "收起菜单", exact: true }).click();
    for (const width of [1600, 1024]) {
        await f.page.setViewportSize({ width, height: 900 });
        const sidebar = f.page.locator(".app-sidebar");
        const settings = sidebar.getByRole("link", { name: "设置", exact: true });
        const rail = await sidebar.boundingBox(), icon = await settings.locator("svg").boundingBox();
        assert.ok(Math.abs(icon.x + icon.width / 2 - rail.x - rail.width / 2) <= 1, "Settings icon must share the compact navigation centerline");
        const hit = await settings.boundingBox();
        assert.ok(hit.width >= 36 && hit.height >= 36, "Compact settings must retain a usable hit target");
        await settings.focus();
        assert.equal(await settings.evaluate((el) => el === document.activeElement), true, "Settings remains keyboard reachable");
        const expand = sidebar.getByRole("button", { name: "展开菜单", exact: true });
        if (await expand.count()) {
            const button = await expand.boundingBox();
            assert.ok(button.y >= hit.y + hit.height, "Expand and settings must not compete for the same narrow row");
        }
    }
    const small = await f.page.evaluate(() => [...document.querySelectorAll("button, a[href], [role=button]")].map((el) => ({ label: (el.getAttribute("aria-label") || el.textContent || "").trim().slice(0, 40), ...el.getBoundingClientRect().toJSON() })).filter((box) => box.width > 0 && box.height > 0 && (box.width < 24 || box.height < 24)));
    assert.deepEqual(small, [], "Every visible control keeps at least a 24px target");
};

checks["relationship-execution-state"] = async (f) => {
    const tasks = [
        { ...task("11", A, "scratch"), execution: "idle" },
        task("12", A, "scratch"),
        { ...task("13", A, "scratch"), execution: "unknown" },
        { ...task("14", A, "scratch"), lifecycle: "done", execution: "idle" },
        { ...task("15", A, "scratch"), parent: "12", origin: "delegate", execution: "idle" },
    ];
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState({ at, hub: { node: "test-node", started: at, version: "test" }, nodes: [], agents: [], tasks, plans: [], projects: [project("scratch"), project("home")], attempts: [], landings: [] }) }));
    await f.page.reload(); await f.box.waitFor();
    await f.page.getByRole("button", { name: "显示详情", exact: true }).click();
    await f.page.getByRole("tab", { name: "关系", exact: true }).click();
    const inspector = f.page.getByRole("complementary", { name: "详情", exact: true });
    const row = (id) => inspector.getByRole("button").filter({ has: f.page.locator(`[title="Task ${id}"]`) });
    const expectState = async (id, label, spins) => {
        await row(id).getByText(label, { exact: true }).waitFor();
        assert.equal(await row(id).locator(".animate-spin").count(), spins, `Task ${id}: ${label} must ${spins ? "show" : "not show"} execution animation`);
    };
    await expectState("11", "空闲", 0);
    await expectState("12", "进行中", 1);
    await expectState("13", "状态未知", 0);
    await expectState("14", "已完成", 0);
    await expectState("15", "空闲", 0);
    tasks[1].execution = "idle";
    await f.emit({ kind: "task.updated", task_id: "12" }); await f.page.clock.runFor(350);
    await expectState("12", "空闲", 0);
    tasks[1].execution = "running";
    await f.emit({ kind: "task.updated", task_id: "12" }); await f.page.clock.runFor(350);
    await expectState("12", "进行中", 1);
};

checks["narrow-relationships-layout"] = async (f) => {
    const node = "node-0123456789abcdef0123456789abcdef";
    const tasks = ["11", "12", "13"].map((id) => ({ ...task(id, A, "scratch"), member: "reviewer", node, lifecycle: "done", execution: "done" }));
    tasks.push({ ...task("14", A, "scratch"), member: "helper", node, parent: "11", origin: "delegate", lifecycle: "done", execution: "done" });
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState({ at, hub: { node: "test-node", started: at, version: "test" }, nodes: [], agents: [], tasks, plans: [], projects: [project("scratch"), project("home")], attempts: [], landings: [] }) }));
    await f.page.reload();
    await f.box.waitFor();
    await f.page.getByRole("button", { name: "显示详情", exact: true }).click();
    await f.page.getByRole("tab", { name: "关系", exact: true }).click();
    await f.page.getByRole("separator", { name: "调整详情栏宽度" }).press("Home");
    const inspector = f.page.getByRole("complementary", { name: "详情", exact: true });
    for (const width of [1600, 1024]) {
        await f.page.setViewportSize({ width, height: 900 });
        // Changing breakpoints replaces the docked inspector with a sheet.
        // Read one settled layout rather than comparing detached row handles.
        await eventually(async () => {
            if (!await inspector.isVisible()) return false;
            return inspector.evaluate((el) => {
                const bounds = el.getBoundingClientRect();
                const rows = [...el.querySelectorAll('[role="button"]')];
                return rows.length === 4 && rows.every((row) => {
                    const box = row.getBoundingClientRect();
                    return box.height > 0 && box.height < 100 && box.left >= bounds.left && box.right <= bounds.right && row.scrollWidth <= row.clientWidth + 1;
                });
            });
        }, "All relationship rows must remain compact and contained after a narrow-window transition");
    }
};

checks["design-mobile-new-failure"] = async (f) => {
    await f.page.setViewportSize({ width: 390, height: 844 });
    await f.box.fill("Draft kept after mobile creation fails");
    f.failBinding = true;
    await f.page.getByRole("button", { name: "会话列表", exact: true }).click();
    await f.page.getByRole("dialog", { name: "会话列表", exact: true }).getByRole("button", { name: "新会话", exact: true }).click();
    await eventually(() => f.calls.some((call) => call.path.endsWith("/initialize") && call.project === "scratch"), "Mobile project binding must begin");
    await f.page.getByRole("status").filter({ hasText: "Project binding unavailable" }).waitFor();
    await visibleControl(f.box, "Message after mobile creation failure");
    assert.equal(await draftOf(f.box), "Draft kept after mobile creation fails");
    await f.page.locator("main header").getByText("Conversation A", { exact: true }).waitFor();
};

checks["design-active-workspace"] = async (f) => {
    await f.page.route("**/console/context?*", (route) => route.fulfill({ json: { enabled: true, context: { conversation: A, agents: [], project: { ...project("scratch"), bound: true }, agent: { id: "test-agent", node: "worker-west", harness: "test", model: "model-one", ready: true, usable: true, place: { node: "worker-west", kind: "copy", workspace: "scratch@worker-west" } } } } }));
    await f.page.reload();
    await f.box.waitFor();
    await f.page.locator("main header").getByText(/scratch.*worker-west/).waitFor();
    await f.page.getByRole("button", { name: "显示详情", exact: true }).click();
    const detail = inspector(f.page);
    await detail.getByText("当前工作区", { exact: true }).waitFor();
    await detail.getByText("副本 · worker-west", { exact: true }).waitFor();
    await detail.getByText("项目主机", { exact: true }).waitFor();
    assert.equal(await detail.getByText("test-node", { exact: true }).count(), 1, "Inspector must distinguish the project's host from the selected agent's workspace");
};

checks["design-inspector-toggle"] = async (f) => {
    const panel = inspector(f.page);
    const show = f.page.getByRole("button", { name: /^(显示详情|打开详情)$/ });
    const hide = f.page.getByRole("button", { name: /^(隐藏详情|关闭详情)$/ });
    if (await panel.isVisible()) await hide.first().click();
    await visibleControl(show, "Show inspector");
    await f.box.fill("Draft while inspecting");
    await show.click();
    await panel.waitFor();
    await composerGeometry(f);
    await hide.first().click();
    assert.equal(await panel.isVisible(), false, "Inspector must be explicitly dismissible");
    await f.page.setViewportSize({ width: 390, height: 844 });
    await f.page.locator(".app-mobile-bar").waitFor();
    const before = await visibleControl(f.box, "Mobile message before inspector");
    const underlyingMessage = await f.box.elementHandle();
    await show.click();
    await panel.waitFor();
    await noHorizontalOverflow(f.page);
    const during = await underlyingMessage.boundingBox();
    assert.ok(Math.abs(during.width - before.width) <= 1, "Narrow-screen inspector must overlay instead of shrinking the conversation");
    const close = f.page.getByRole("button", { name: /^(关闭详情|隐藏详情)$/ });
    await visibleControl(close.last(), "Close mobile inspector");
    await close.last().click();
    await visibleControl(f.box, "Mobile message after inspector closes");
    assert.equal(await draftOf(f.box), "Draft while inspecting");
};

// The rail's conversation tab answers what the agent is working with:
// the instructions read as markdown rather than printed as code, the
// pieces they were assembled from, the MCP servers with their tools and
// the commands the machine reported.
checks["session-setup-view"] = async (f) => {
    f.setup = {
        agent: "test-agent", node: "test-node", harness: "test", model: "model-one", applied: false,
        instructions: "# 身份\n\n**记住**：你是 Steve。\n\n- 第一条\n- 第二条",
        sections: [
            { kind: "identity", bytes: 320 },
            { kind: "language", name: "zh", bytes: 96 },
            { kind: "prompt", name: "test-agent", bytes: 1024 },
            { kind: "skill", name: "review", path: "/skills/review", bytes: 8192 },
            { kind: "memory", name: "home", bytes: 512 },
        ],
        mcp_servers: ["steve"],
    };
    await f.page.route("**/console/mcp", (route) => route.fulfill({ json: {
        platform: [{ name: "steve", description: "Session tools", tools: [{ name: "steve_context", description: "Read the current workspace." }] }],
        deployments: [], machines: [],
    } }));
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState({ at, hub: { node: "test-node", started: at, version: "test" },
        nodes: [{ name: "test-node", up: true, snapshot: { schema: "v1", node: "test-node", generation: 1, sequence: 1, generated_at: at, coverage: {}, offers: [{ kind: "tool", id: "ripgrep", availability: "available" }, { kind: "harness", id: "test", availability: "available" }, { kind: "skill", id: "skill-creator", scope: "test", availability: "available" }, { kind: "skill", id: "other-harness-skill", scope: "elsewhere", availability: "available" }] } }],
        agents: [], tasks: [], plans: [], projects: [project("scratch"), project("home")], attempts: [], landings: [] }) }));
    await f.page.route("**/console/context?*", (route) => route.fulfill({ json: { enabled: true, context: { conversation: A, agents: [], project: { ...project("scratch"), bound: true }, agent: { id: "test-agent", node: "test-node", harness: "test", model: "model-one", ready: true, usable: true } } } }));
    await f.page.reload();
    await f.box.waitFor();
    await f.page.getByRole("button", { name: "显示详情", exact: true }).click();
    const detail = inspector(f.page);
    await detail.getByText("运行配置", { exact: true }).waitFor();
    await detail.getByText("下一轮会重新发送", { exact: true }).waitFor();
    // The pieces are named and weighed, so a skill folded in behind the
    // agent's own prompt is visible rather than buried in one wall of text.
    await detail.getByText("指令构成", { exact: true }).waitFor();
    await detail.getByText("Agent 提示", { exact: true }).waitFor();
    await detail.getByText("review", { exact: true }).waitFor();
    // Markdown is rendered, not printed as source, and the source stays
    // one click away for anyone checking what was actually sent.
    await detail.getByText(/^指令 · /).click();
    await detail.locator("h1").getByText("身份", { exact: true }).waitFor();
    await detail.locator("strong").getByText("记住", { exact: true }).waitFor();
    await detail.getByRole("button", { name: "原文", exact: true }).click();
    await detail.getByText("# 身份").first().waitFor();
    await detail.getByRole("button", { name: "渲染", exact: true }).click();
    await detail.locator("h1").getByText("身份", { exact: true }).waitFor();
    // MCP tools and the machine's commands are listed here too: a trace
    // never says what the agent could have reached for.
    await detail.getByText("steve", { exact: true }).click();
    await detail.getByText("steve_context", { exact: true }).waitFor();
    // The rail is narrow while the viewport is wide, so a media query
    // cannot tell: the page's two-column tool row would squeeze the
    // description to nothing. It has to stay readable here.
    const summary = detail.getByText("Read the current workspace.", { exact: true }).first();
    await summary.waitFor();
    assert.ok((await summary.boundingBox()).width > 120, "An MCP tool's description must stay readable in the rail");
    await detail.getByText("机器上的命令", { exact: true }).waitFor();
    await detail.getByText("ripgrep", { exact: true }).waitFor();
    // Skills the harness loads itself never reach the instructions, so
    // they are listed separately — and only the ones it can actually load.
    await detail.getByText("机器上的技能", { exact: true }).waitFor();
    await detail.getByText("skill-creator", { exact: true }).waitFor();
    assert.equal(await detail.getByText("other-harness-skill", { exact: true }).count(), 0, "A skill scoped to another AI tool must not be listed as this agent's");
    // Sections are counted in UTF-8 bytes; the fold has to agree with them
    // rather than counting a Chinese character as one byte.
    await detail.getByText(/^指令 · 6[0-9] B$/).waitFor();
    await f.page.setViewportSize({ width: 390, height: 844 });
    await noHorizontalOverflow(f.page);
};

checks["mcp-tool-details"] = async (f) => {
    const description = "Read the current workspace. " + "A long explanation of the available context. ".repeat(20) + "Final detail remains readable.";
    const externalName = "external_workspace_".repeat(6);
    await f.page.route("**/console/mcp", (route) => route.fulfill({ json: {
        platform: [{ name: "steve", description: "Session tools", tools: [{ name: "steve_context", description }] }],
        deployments: [{ node: "test-node", name: "example-service", type: "http", url: "https://example.test/mcp", agents: [], probe: { at, ok: true, tools: [{ name: externalName, description, input_schema: { type: "object", properties: { workspace: { type: "string" } } } }] } }],
        machines: [],
    } }));
    await f.page.getByRole("link", { name: "MCP", exact: true }).click();
    const summary = f.page.locator("summary").filter({ hasText: "steve_context" });
    await summary.waitFor();
    assert.match(await summary.innerText(), /查看当前会话/);
    const fullDescription = f.page.getByText(description, { exact: true });
    assert.equal(await fullDescription.isVisible(), false, "Long tool instructions should not crowd the initial list");
    await summary.click();
    assert.equal(await fullDescription.isVisible(), true, "Expanding a tool must expose its complete description");
    await summary.press("Enter");
    assert.equal(await fullDescription.isVisible(), false, "Keyboard must collapse the tool details");
    await f.page.setViewportSize({ width: 390, height: 844 });
    await summary.click();
    await noHorizontalOverflow(f.page);
    await f.page.getByRole("row").filter({ hasText: "example-service" }).click();
    const drawer = f.page.getByRole("dialog", { name: "example-service", exact: true });
    await drawer.waitFor();
    assert.equal(await drawer.getByRole("heading", { name: "example-service", exact: true, level: 2 }).count(), 1, "Entity drawers expose their actual name as a heading");
    await drawer.locator("summary").filter({ hasText: externalName }).click();
    assert.equal(await drawer.getByRole("paragraph").filter({ hasText: description }).isVisible(), true, "Installed tools must also expose the full description beyond the preview");
    await drawer.getByText("输入参数", { exact: true }).waitFor();
    assert.match(await drawer.locator("pre").innerText(), /"workspace"/);
    assert.ok(await drawer.evaluate((el) => el.scrollWidth <= el.clientWidth + 1), "Long tool names must not overflow the mobile drawer");
    assert.equal(f.calls.length, 0, "Reading tool descriptions must not invoke, probe, or install services");
};

async function homeFixture(f) {
    const files = [
        { name: "SOUL.md", text: "# Identity\n\nA calm assistant.", budget: 128 },
        { name: "USER.md", text: "# Preferences\n\nConcise replies.", budget: 256 },
        { name: "MEMORY.md", text: "", budget: 256, missing: true },
    ];
    const projects = [{ id: "scratch", path: "/test/scratch/MEMORY.md", text: "# Project memory\n\nInitial context.", budget: 256, facts: 0 }];
    const state = { writes: [], hold: null, failWrite: false, failRead: false };
    await f.page.route(/\/console\/(home|memory)(\/|$)/, async (route) => {
        const req = route.request(), url = new URL(req.url());
        if (req.method() === "GET") {
            if (state.failRead) return route.fulfill({ status: 503, body: "Readback unavailable" });
            return route.fulfill({ json: { path: "/test/home", files, projects, total_budget: 640, owner_bytes: 80, guest_bytes: 30, warnings: [], audit: "/test/audit" } });
        }
        assert.equal(req.method(), "PUT");
        const input = req.postDataJSON();
        state.writes.push({ path: url.pathname, ...input });
        if (state.hold) await state.hold;
        if (state.failWrite) return route.fulfill({ status: 400, body: "Save rejected" });
        const name = decodeURIComponent(url.pathname.split("/").pop());
        const doc = url.pathname.includes("/memory/") ? projects.find((p) => p.id === name) : files.find((p) => p.name === name);
        doc.text = input.text.trimEnd() + "\n";
        doc.missing = false;
        return route.fulfill({ json: { ok: true } });
    });
    await f.page.getByRole("link", { name: "档案", exact: true }).click();
    await f.page.getByRole("heading", { name: "身份", exact: true }).waitFor();
    return state;
}

checks["home-document-drafts"] = async (f) => {
    const state = await homeFixture(f);
    assert.equal(await f.page.getByRole("textbox").count(), 0, "Profiles should open as a single readable document");
    await f.page.getByRole("button", { name: "编辑", exact: true }).click();
    const identity = f.page.getByRole("textbox", { name: "SOUL.md", exact: true });
    await identity.fill("Identity draft");
    await f.page.getByRole("link", { name: "用户档案", exact: true }).click();
    await f.page.getByRole("button", { name: "编辑", exact: true }).click();
    const user = f.page.getByRole("textbox", { name: "USER.md", exact: true });
    await user.fill("Preferences draft");
    const pending = gate(); state.hold = pending.promise; f.releases.push(pending.release);
    await f.page.getByRole("button", { name: "保存", exact: true }).dblclick();
    await eventually(() => state.writes.length === 1, "Save must start once");
    await user.fill("Preferences continued while saving");
    await f.page.getByRole("link", { name: "身份", exact: true }).click();
    assert.equal(await identity.inputValue(), "Identity draft", "Switching documents must keep each draft");
    pending.release();
    await f.page.getByRole("link", { name: "用户档案", exact: true }).click();
    await eventually(() => f.page.getByRole("button", { name: "保存", exact: true }).isEnabled(), "Save and readback must finish before checking the retained draft");
    assert.equal(await user.inputValue(), "Preferences continued while saving", "Server readback must preserve input typed after submission");
    assert.equal(state.writes.length, 1, "Repeated save must not duplicate the PUT");
    assert.match(await f.page.getByRole("status").last().innerText(), /未保存/);
    await f.page.getByRole("link", { name: "身份", exact: true }).click();
    assert.equal(await identity.inputValue(), "Identity draft", "Saving another document must not replace this draft");
    await f.page.getByRole("button", { name: "放弃修改", exact: true }).click();
    await f.page.getByRole("button", { name: "继续编辑", exact: true }).click();
    assert.equal(await identity.inputValue(), "Identity draft", "Canceling discard must keep the draft");
    await f.page.getByRole("button", { name: "放弃修改", exact: true }).click();
    await f.page.getByRole("button", { name: "确认放弃", exact: true }).click();
    assert.equal(await identity.count(), 0);
    await f.page.getByRole("link", { name: "工作台", exact: true }).click();
    await f.box.waitFor();
    await f.page.getByRole("link", { name: "档案", exact: true }).click();
    await f.page.getByRole("link", { name: "用户档案", exact: true }).click();
    assert.equal(await user.inputValue(), "Preferences continued while saving", "Returning to profiles must restore unsaved work");
};

checks["home-save-feedback"] = async (f) => {
    const state = await homeFixture(f);
    await f.page.getByRole("button", { name: "编辑", exact: true }).click();
    const editor = f.page.getByRole("textbox", { name: "SOUL.md", exact: true });
    await editor.fill("界".repeat(50));
    assert.equal(await f.page.getByRole("button", { name: "保存", exact: true }).isEnabled(), false, "Budget counts UTF-8 bytes");
    await editor.fill("Retryable draft");
    state.failWrite = true;
    await f.page.getByRole("button", { name: "保存", exact: true }).click();
    await f.page.getByRole("alert").filter({ hasText: "Save rejected" }).waitFor();
    assert.equal(await editor.inputValue(), "Retryable draft");
    state.failWrite = false; state.failRead = true;
    await f.page.getByRole("button", { name: "保存", exact: true }).click();
    await f.page.getByText(/已保存.*刷新失败/).waitFor();
    assert.equal(state.writes.length, 2);
    state.failRead = false;
    await f.page.getByRole("link", { name: "全局记忆", exact: true }).click();
    await f.page.getByRole("button", { name: "编辑", exact: true }).click();
    await f.page.getByRole("textbox", { name: "MEMORY.md", exact: true }).fill("New memory");
    await f.page.getByRole("button", { name: "保存", exact: true }).click();
    await f.page.getByText("已保存", { exact: true }).waitFor();
    await f.page.getByRole("link", { name: "scratch", exact: true }).click();
    await f.page.getByRole("button", { name: "编辑", exact: true }).click();
    await f.page.getByRole("textbox", { name: "memory scratch", exact: true }).fill("Project draft");
    await f.page.getByRole("button", { name: "保存", exact: true }).click();
    await eventually(() => state.writes.some((w) => w.path === "/console/memory/scratch"), "Project memory must save to the selected project's endpoint");
};

checks["inspector-resize"] = async (f) => {
    await f.page.getByRole("button", { name: "显示详情", exact: true }).click();
    const handle = f.page.getByRole("separator", { name: "调整详情栏宽度", exact: true });
    await handle.waitFor();
    const panel = f.page.locator(".workbench-inspector");
    const initial = (await panel.boundingBox()).width;
    const drag = async (dx) => {
        const box = await handle.boundingBox();
        const x = box.x + box.width / 2, y = box.y + box.height / 2;
        await f.page.mouse.move(x, y); await f.page.mouse.down();
        await f.page.mouse.move(x + dx, y, { steps: 10 }); await f.page.mouse.up();
    };
    await drag(-140);
    assert.ok(Math.abs((await panel.boundingBox()).width - initial - 140) <= 2, "Dragging left must widen the right inspector");
    await handle.press("ArrowRight");
    const preferred = (await panel.boundingBox()).width;
    assert.ok(preferred < initial + 140, "Arrow keys provide a non-drag resize alternative");
    await f.page.getByRole("button", { name: "关闭详情", exact: true }).click();
    await f.page.getByRole("button", { name: "显示详情", exact: true }).click();
    assert.ok(Math.abs((await panel.boundingBox()).width - preferred) <= 1, "Reopening must restore the chosen width");
    await f.page.reload(); await f.box.waitFor();
    await f.page.getByRole("button", { name: "显示详情", exact: true }).click();
    assert.ok(Math.abs((await panel.boundingBox()).width - preferred) <= 1, "Reload must preserve the width preference");
    await drag(-1000);
    assert.ok((await f.page.locator(".conversation-content").boundingBox()).width >= 480, "Resizing must preserve space to compose messages");
    await f.page.setViewportSize({ width: 1536, height: 1000 });
    await eventually(async () => (await f.page.locator(".conversation-content").boundingBox()).width >= 480, "Window resizing must enforce the conversation minimum");
    await drag(1000);
    assert.ok((await panel.boundingBox()).width >= 320, "Inspector cannot collapse below its readable minimum");
    await handle.dblclick();
    assert.ok(Math.abs((await panel.boundingBox()).width - initial) <= 1, "Double-click restores the default width");
    await f.page.setViewportSize({ width: 1280, height: 900 });
    await f.page.getByRole("dialog", { name: "详情", exact: true }).waitFor();
    const conversationBefore = (await f.page.locator(".conversation-content").boundingBox()).width;
    await drag(-80);
    assert.ok((await panel.boundingBox()).width > initial, "Desktop overlay inspector should also resize");
    assert.equal((await f.page.locator(".conversation-content").boundingBox()).width, conversationBefore, "Resizing the overlay must not squeeze the underlying conversation");
    const handleBox = await handle.boundingBox();
    await f.page.mouse.move(handleBox.x + 5, handleBox.y + 50); await f.page.mouse.down();
    await f.page.mouse.move(handleBox.x - 50, handleBox.y + 50);
    await handle.press("Escape"); await f.page.mouse.up();
    assert.ok(Math.abs((await panel.boundingBox()).width - initial - 80) <= 1, "Escape must cancel an active resize");
    assert.notEqual(await f.page.evaluate(() => document.body.style.cursor), "col-resize", "Cancel must release the drag cursor");
    await f.page.setViewportSize({ width: 390, height: 844 });
    await handle.waitFor({ state: "detached" });
    assert.equal(await handle.count(), 0, "The mobile sheet must not show a desktop resize handle");
    await noHorizontalOverflow(f.page);
};

async function reviewFixture(f, { open = true, files = {} } = {}) {
    const changes = [
        { path: "src/main.ts", status: "M", added: 1, deleted: 1 },
        { path: "removed.txt", status: "D", added: 0, deleted: 1 },
        { path: "new.txt", status: "A", added: 1, deleted: 0 },
        { path: "logo.png", status: "M", added: 0, deleted: 0, binary: true },
        { path: "script.sh", status: "M", added: 0, deleted: 0 },
    ];
    const state = { reads: [], hold: null, holdIndex: null, holdFile: null, failDiff: false, failIndex: false };
    await f.page.route(/\/console\/attempts\?/, (route) => route.fulfill({ json: nativeHistory([{ id: "review-attempt", kind: "turn", state: "done", agent: "test-agent", node: "test-node", base: "before", artifact: "after", started_at: at, files: 5 }, { id: "review-other", kind: "turn", state: "done", agent: "other-agent", node: "test-node", base: "older-before", artifact: "older-after", started_at: "2026-09-05T10:00:00Z", files: 1 }]) }));
    await f.page.route(/\/console\/attempts\/review-other\//, (route) => {
        const url = new URL(route.request().url());
        const body = url.pathname.endsWith("/changes") ? { attempt: "review-other", base: "older-before", artifact: "older-after", changes: [{ path: "other.txt", status: "A", added: 1, deleted: 0 }] }
            : url.pathname.endsWith("/tree") ? { attempt: "review-other", commit: "older-after", which: "result", dir: "", entries: [{ path: "other.txt", name: "other.txt", kind: "file" }] }
                : { path: "other.txt", diff: "@@ -0,0 +1 @@\n+Other execution content\n" };
        return route.fulfill({ json: body });
    });
    await f.page.route(/\/console\/attempts\/review-attempt\//, async (route) => {
        const url = new URL(route.request().url());
        assert.equal(route.request().method(), "GET");
        const file = url.searchParams.get("path");
        state.reads.push({ endpoint: url.pathname.split("/").pop(), file });
        if (url.pathname.endsWith("/changes")) {
            if (state.holdIndex) await state.holdIndex;
            if (state.failIndex) return route.fulfill({ status: 400, body: "Index unavailable" });
            return route.fulfill({ json: { attempt: "review-attempt", project: "scratch", base: "before", artifact: "after", changes } });
        }
        if (url.pathname.endsWith("/tree")) return route.fulfill({ json: { attempt: "review-attempt", commit: "after", which: "result", dir: file, entries: file === "src" ? [{ name: "main.ts", path: "src/main.ts", kind: "file", size: 48 }, { name: "helper.ts", path: "src/helper.ts", kind: "file", size: 22 }] : [{ name: "src", path: "src", kind: "dir" }, { name: "README.md", path: "README.md", kind: "file", size: 36 }, { name: "empty.txt", path: "empty.txt", kind: "file", size: 0 }, { name: "new.txt", path: "new.txt", kind: "file", size: 10 }, { name: "linked", path: "linked", kind: "link" }, { name: "vendor", path: "vendor", kind: "repo" }, { name: "large.txt", path: "large.txt", kind: "file", size: 40000 }, ...Object.entries(files).map(([path, text]) => ({ name: path, path, kind: "file", size: Buffer.byteLength(text) }))] } });
        if (url.pathname.endsWith("/file")) {
            if (state.holdFile && file === "README.md") await state.holdFile;
            const text = files[file] ?? (file === "README.md" ? "# Project\n\nUnchanged project guide.\n\nSee [the helper](./src/helper.ts), [outside](../outside.txt), [the site](https://example.com/docs) and [top](#project).\n" : file === "src/main.ts" ? "const shared = true;\nconst after = 2;\n" : file === "empty.txt" ? "" : file === "large.txt" ? Array.from({ length: 1501 }, (_, i) => `line ${i + 1}`).join("\n") : file === "linked" ? "README.md" : "export const helper = 1;");
            return route.fulfill({ json: { attempt: "review-attempt", path: file, commit: "after", text, size: text.length } });
        }
        if (state.hold && file === "src/main.ts") await state.hold;
        if (state.failDiff) return route.fulfill({ status: 400, body: "Diff unavailable" });
        assert.notEqual(file, "logo.png", "Binary files must not request text diffs");
        const diff = file === "removed.txt" ? "diff --git a/removed.txt b/removed.txt\n--- a/removed.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-Removed content\n"
            : file === "new.txt" ? "diff --git a/new.txt b/new.txt\n--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+Added content\n"
                : file === "script.sh" ? "diff --git a/script.sh b/script.sh\nold mode 100644\nnew mode 100755\n"
                    : "diff --git a/src/main.ts b/src/main.ts\n--- a/src/main.ts\n+++ b/src/main.ts\n@@ -1,2 +1,2 @@\n const shared = true;\n-const before = 1;\n+const after = 2;\n\\ No newline at end of file\n";
        return route.fulfill({ json: { path: file, diff, truncated: file === "new.txt" } });
    });
    await f.page.getByRole("button", { name: "显示详情", exact: true }).click();
    await f.page.getByRole("tab", { name: "产物", exact: true }).click();
    if (open) {
        const history = f.page.getByText("版本与变更", { exact: true });
        await history.click();
        await f.page.getByRole("button", { name: "查看变更", exact: true }).first().click();
        await f.page.getByRole("dialog", { name: "产物工作区", exact: true }).waitFor();
    }
    return state;
}

checks["code-all-conversation-tasks"] = async (f) => {
    const tasks = Array.from({ length: 9 }, (_, i) => ({ ...task(String(i + 1), A, "scratch"), updated_at: `2026-09-06T10:00:0${8 - i}Z` }));
    for (let i = 10; i < 16; i++) tasks.push({ ...task(String(i), "child-channel", "scratch"), parent: String(i - 1) });
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState({ at, hub: { node: "test-node", started: at, version: "test" }, nodes: [], agents: [], tasks, plans: [], projects: [project("scratch")], attempts: [], landings: [] }) }));
    const readTasks = [];
    await f.page.route(/\/console\/attempts\?/, (route) => {
        const id = new URL(route.request().url()).searchParams.get("conversation"); readTasks.push(id);
        return route.fulfill({ json: nativeHistory(id === A ? [{ task_id: "15", id: "old-output", kind: "turn", state: "bound", agent: "older-agent", base: "old-base", artifact: "old-result", started_at: at }] : []) });
    });
    await f.page.reload(); await f.box.waitFor();
    await f.page.getByRole("button", { name: "显示详情", exact: true }).click();
    await f.page.getByRole("tab", { name: "产物", exact: true }).click();
    await f.page.getByRole("button", { name: "浏览文件", exact: true }).waitFor();
    assert.ok(readTasks.length > 0 && readTasks.every((id) => id === A), "Unified files use one conversation query, including hidden deep descendants");
    await f.page.getByText(/older-agent/).first().waitFor();
};

checks["code-latest-result"] = async (f) => {
    const waiting = gate(); f.releases.push(waiting.release);
    let readOnlyResult = false;
    await f.page.route(/\/console\/attempts\?/, async (route) => {
        if (new URL(route.request().url()).searchParams.get("conversation") === B) { await waiting.promise; return route.fulfill({ json: nativeHistory([]) }); }
        return route.fulfill({ json: nativeHistory([
            ...(readOnlyResult ? [{ id: "latest-read-only", agent: "reader", kind: "turn", state: "bound", base: "latest", started_at: "2026-09-06T10:02:00Z", ended_at: "2026-09-06T10:02:30Z" }] : []),
            { id: "new-base", agent: "new-agent", kind: "turn", state: "running", base: "new-start", started_at: "2026-09-06T10:03:00Z", ended_at: "0001-01-01T00:00:00Z" },
            { id: "newer-start", agent: "early-finish", kind: "turn", state: "done", base: "before", artifact: "early", started_at: "2026-09-06T10:01:00Z", ended_at: "2026-09-06T10:01:30Z" },
            { id: "latest-result", agent: "late-finish", kind: "turn", state: "done", base: "before", artifact: "latest", started_at: at, ended_at: "2026-09-06T10:02:00Z" },
        ]) });
    });
    const reads = [];
    await f.page.route(/\/console\/attempts\/[^/]+\//, (route) => {
        const url = new URL(route.request().url()), attempt = url.pathname.split("/")[3]; reads.push(attempt);
        assert.equal(route.request().method(), "GET");
        return route.fulfill({ json: url.pathname.endsWith("/tree") ? { attempt, commit: "latest", which: "result", dir: "", entries: [] } : { attempt, base: "before", artifact: "latest", changes: [] } });
    });
    await f.page.getByRole("button", { name: "显示详情", exact: true }).click();
    await f.page.getByRole("tab", { name: "产物", exact: true }).click();
    await f.page.getByRole("button", { name: "浏览文件", exact: true }).click();
    const workspace = f.page.getByRole("dialog", { name: "产物工作区", exact: true });
    await workspace.getByText("空目录", { exact: true }).waitFor();
    assert.ok(reads.length > 0 && reads.every((id) => id === "latest-result"), "Default to the latest finished result, not a newer base or earlier completion");
    await workspace.getByRole("button", { name: "返回", exact: true }).click();
    readOnlyResult = true; reads.length = 0;
    await f.page.getByRole("tab", { name: "关系", exact: true }).click();
    await f.page.getByRole("tab", { name: "产物", exact: true }).click();
    await f.page.getByRole("button", { name: "浏览文件", exact: true }).click();
    await workspace.getByText("空目录", { exact: true }).waitFor();
    assert.ok(reads.length > 0 && reads.every((id) => id === "latest-read-only"), "A newer completed read-only snapshot must supersede an older artifact");
    await workspace.getByRole("button", { name: "返回", exact: true }).click();
    await f.pick("B");
    await f.page.getByText("读取执行记录…", { exact: true }).waitFor();
    assert.equal(await f.page.getByRole("button", { name: "浏览文件", exact: true }).count(), 0, "A loading conversation must not expose another conversation's files");
    waiting.release();
    await f.page.getByText("还没有产物快照", { exact: true }).waitFor();
};

checks["code-unified-entry"] = async (f) => {
    await f.box.fill("Draft while browsing outputs");
    const state = await reviewFixture(f, { open: false });
    const panel = f.page.getByRole("complementary", { name: "详情", exact: true });
    const browse = panel.getByRole("button", { name: "浏览文件", exact: true });
    await browse.waitFor();
    assert.equal(await browse.count(), 1, "Conversation files must have one entry rather than one per execution");
    assert.equal(await panel.getByRole("button", { name: "查看变更", exact: true }).count(), 0, "Version actions should be collapsed initially");
    const heading = panel.getByRole("heading", { name: "会话产物", exact: true });
    const gap = await heading.evaluate((el) => el.getBoundingClientRect().top - el.closest(".workbench-inspector").querySelector(".inspector-tabs").getBoundingClientRect().bottom);
    assert.ok(gap >= 16, "Code content needs separation from the tab bar");
    await browse.click();
    const workspace = f.page.getByRole("dialog", { name: "产物工作区", exact: true });
    await workspace.getByRole("navigation", { name: "项目文件" }).getByRole("button", { name: "README.md", exact: true }).click();
    await workspace.getByText("Unchanged project guide.", { exact: true }).waitFor();
    await workspace.getByRole("button", { name: /选择版本/ }).click();
    await f.page.getByRole("option", { name: /other-agent/ }).click();
    await workspace.getByRole("navigation", { name: "项目文件" }).getByRole("button", { name: "other.txt", exact: true }).waitFor();
    await workspace.getByRole("button", { name: "返回", exact: true }).click();
    const history = panel.locator("summary").filter({ hasText: "版本与变更" });
    await history.press("Enter");
    await panel.getByRole("button", { name: "查看变更", exact: true }).first().click();
    await workspace.getByRole("table").waitFor();
    await workspace.getByRole("button", { name: "返回", exact: true }).click();
    assert.equal(await browse.count(), 1);
    assert.equal(await draftOf(f.box), "Draft while browsing outputs");
    assert.equal(f.calls.length, 0, "Browsing output and history must remain read-only");
    assert.ok(state.reads.some((r) => r.endpoint === "file"));
};

checks["review-navigation"] = async (f) => {
    await f.box.fill("Draft retained while reviewing");
    const state = await reviewFixture(f);
    const review = f.page.getByRole("dialog", { name: "产物工作区", exact: true });
    await review.getByRole("table", { name: "修改前与修改后的代码", exact: true }).waitFor();
    await review.getByRole("button", { name: "统一", exact: true }).click();
    await review.getByRole("table", { name: "代码变更", exact: true }).waitFor();
    assert.equal(state.reads.filter((r) => r.endpoint === "diff" && r.file === "src/main.ts").length, 1, "Changing diff layout should reuse the loaded patch");
    await review.getByRole("button", { name: "removed.txt", exact: true }).click();
    await review.getByText("Removed content", { exact: true }).waitFor();
    await review.getByRole("button", { name: "标记已查看", exact: true }).click();
    await review.getByText("已查看 1 / 5", { exact: true }).waitFor();
    await review.getByRole("textbox", { name: "筛选变更文件", exact: true }).fill("missing");
    await review.getByText("没有匹配的文件", { exact: true }).waitFor();
    await review.getByRole("textbox", { name: "筛选变更文件", exact: true }).fill("");
    await review.getByRole("button", { name: "logo.png", exact: true }).click();
    await review.getByText("二进制文件不显示文本差异。", { exact: true }).waitFor();
    await review.getByRole("button", { name: "script.sh", exact: true }).click();
    await review.getByLabel("文件变更信息").filter({ hasText: "new mode 100755" }).waitFor();
    await review.getByRole("button", { name: "new.txt", exact: true }).click();
    await review.getByText(/此文件的差异已截断/).waitFor();
    await review.getByText("Added content", { exact: true }).waitFor();
    await f.page.setViewportSize({ width: 390, height: 844 });
    await review.getByRole("button", { name: "文件导航", exact: true }).waitFor();
    await noHorizontalOverflow(f.page);
    await review.getByRole("button", { name: "返回", exact: true }).click();
    await f.page.getByRole("button", { name: "关闭详情", exact: true }).click();
    assert.equal(await draftOf(f.box), "Draft retained while reviewing", "Review must preserve the conversation draft");
    assert.equal(f.calls.length, 0, "Review must never write or submit work");
};

checks["review-late-response"] = async (f) => {
    const state = await reviewFixture(f);
    const review = f.page.getByRole("dialog", { name: "产物工作区", exact: true });
    await review.getByRole("table").waitFor();
    state.failDiff = true;
    await review.getByRole("button", { name: "removed.txt", exact: true }).click();
    await review.getByRole("alert").filter({ hasText: "Diff unavailable" }).waitFor();
    state.failDiff = false;
    await review.getByRole("button", { name: "重试", exact: true }).click();
    await review.getByText("Removed content", { exact: true }).waitFor();
    await review.getByRole("button", { name: "返回", exact: true }).click();
    const pending = gate(); state.hold = pending.promise; f.releases.push(pending.release);
    await f.page.getByRole("button", { name: "查看变更", exact: true }).first().click();
    await review.getByRole("button", { name: "new.txt", exact: true }).click();
    await review.getByText("Added content", { exact: true }).waitFor();
    pending.release(); await delay(100);
    assert.equal(await review.getByText("const after = 2;", { exact: true }).count(), 0, "Late response must not replace the newly selected file");
    await review.press("Escape");
    await review.waitFor({ state: "hidden" });
    await f.page.getByRole("button", { name: "浏览文件", exact: true }).first().click();
    await review.getByRole("navigation", { name: "项目文件" }).getByRole("button", { name: "new.txt", exact: true }).click();
    await review.getByRole("button", { name: "Diff", exact: true }).click();
    await review.getByRole("heading", { name: "new.txt", exact: true }).waitFor();
    await review.getByText("Added content", { exact: true }).waitFor();
};

checks["review-slow-scope"] = async (f) => {
    const state = await reviewFixture(f);
    const review = f.page.getByRole("dialog", { name: "产物工作区", exact: true });
    await review.getByRole("table").waitFor();
    await review.getByRole("button", { name: "返回", exact: true }).click();
    const pending = gate(); state.holdIndex = pending.promise; f.releases.push(pending.release);
    await f.page.getByRole("button", { name: "浏览文件", exact: true }).first().click();
    await review.getByRole("navigation", { name: "项目文件" }).getByRole("button", { name: "README.md", exact: true }).click();
    await review.getByText("Unchanged project guide.", { exact: true }).waitFor();
    assert.ok(state.reads.some((r) => r.endpoint === "file"), "Source browsing must remain independent of a slow changes index");
    pending.release();
    await review.getByRole("button", { name: "返回", exact: true }).click();
    state.holdIndex = null; state.failIndex = true;
    await f.page.getByRole("button", { name: "查看变更", exact: true }).first().click();
    await review.getByRole("alert").filter({ hasText: "Index unavailable" }).waitFor();
    state.failIndex = false;
    await review.getByRole("button", { name: "重试", exact: true }).click();
    await review.getByRole("table").waitFor();
    await review.getByRole("button", { name: /选择版本/ }).click();
    await f.page.getByRole("option", { name: /other-agent/ }).click();
    await review.getByRole("heading", { name: "other.txt", exact: true }).waitFor();
    await review.getByText("Other execution content", { exact: true }).first().waitFor();
    assert.equal(await review.getByText("const after = 2;", { exact: true }).count(), 0, "Execution selection must show only that execution's changes");
};

checks["review-mobile-origin"] = async (f) => {
    await f.page.setViewportSize({ width: 1280, height: 900 });
    await reviewFixture(f);
    const review = f.page.getByRole("dialog", { name: "产物工作区", exact: true });
    await review.getByRole("table").waitFor();
    await f.page.setViewportSize({ width: 390, height: 844 });
    await review.getByRole("button", { name: "文件导航", exact: true }).waitFor();
    await review.getByRole("button", { name: "返回", exact: true }).click();
    await f.page.getByRole("dialog", { name: "详情", exact: true }).waitFor();
    await f.page.getByRole("button", { name: "关闭详情", exact: true }).click();
    await f.box.waitFor();
    await noHorizontalOverflow(f.page);
};

checks["code-workspace-files"] = async (f) => {
    const state = await reviewFixture(f);
    const workspace = f.page.getByRole("dialog", { name: "产物工作区" });
    await workspace.getByRole("button", { name: "全部文件", exact: true }).click();
    const tree = workspace.getByRole("navigation", { name: "项目文件" });
    await tree.getByRole("button", { name: "README.md", exact: true }).click();
    await workspace.getByText("Unchanged project guide.", { exact: true }).waitFor();
    await tree.getByRole("button", { name: "src/main.ts", exact: true }).click();
    await workspace.getByRole("region", { name: "源码 src/main.ts", exact: true }).waitFor();
    assert.ok(await workspace.locator(".hljs-keyword").count(), "Source uses syntax highlighting");
    await workspace.getByRole("button", { name: "Diff", exact: true }).click();
    await workspace.getByRole("table").waitFor();
    await workspace.getByRole("button", { name: "阅读 README.md", exact: true }).click();
    await workspace.getByText("Unchanged project guide.", { exact: true }).waitFor();
    assert.equal(state.reads.filter((read) => read.endpoint === "file" && read.file === "README.md").length, 1, "File tabs reuse the loaded source");
    await workspace.getByRole("button", { name: "关闭 README.md", exact: true }).click();
    await tree.getByRole("button", { name: "empty.txt", exact: true }).click();
    await workspace.getByText("空文件", { exact: true }).waitFor();
    await tree.getByRole("button", { name: "linked", exact: true }).click();
    await workspace.getByText("符号链接 · 仅显示目标，不跟随链接", { exact: true }).waitFor();
    assert.equal(await tree.getByRole("button", { name: "vendor", exact: true }).isEnabled(), false);
    await tree.getByRole("button", { name: "large.txt", exact: true }).click();
    await workspace.getByRole("button", { name: "继续加载 501 行", exact: true }).waitFor();
    assert.equal(await workspace.locator(".source-line").count(), 1000);
    await workspace.getByRole("button", { name: "继续加载 501 行", exact: true }).click();
    assert.equal(await workspace.locator(".source-line").count(), 1501);
    await workspace.getByRole("button", { name: "自动换行", exact: true }).click();
    assert.equal(await workspace.getByRole("button", { name: "自动换行", exact: true }).getAttribute("aria-pressed"), "true");
    await workspace.locator(".source-scroll").evaluate((element) => { element.scrollTop = 2200; });
    await workspace.getByRole("button", { name: "阅读 src/main.ts", exact: true }).click();
    await workspace.getByRole("button", { name: "阅读 large.txt", exact: true }).click();
    await workspace.getByRole("region", { name: "源码 large.txt", exact: true }).waitFor();
    assert.equal(await workspace.locator(".source-line").count(), 1501, "Tabs retain the expanded source range");
    assert.equal(await workspace.getByRole("button", { name: "自动换行", exact: true }).getAttribute("aria-pressed"), "true");
    assert.ok(await workspace.locator(".source-scroll").evaluate((element) => element.scrollTop > 2100), "Tabs retain the source reading position");
    await f.page.setViewportSize({ width: 390, height: 844 });
    await workspace.getByRole("button", { name: "文件导航", exact: true }).click();
    await tree.getByRole("button", { name: "README.md", exact: true }).click();
    await workspace.getByText("Unchanged project guide.", { exact: true }).waitFor();
    await noHorizontalOverflow(f.page);
    assert.equal(f.calls.length, 0, "Read-only workspace must not submit work");
};

checks["code-snapshot-states"] = async (f) => {
    const state = { commit: "snapshot", failIndex: false, fileReads: 0 };
    await f.page.route(/\/console\/attempts\?/, (route) => route.fulfill({ json: nativeHistory([
        { id: "unchanged", kind: "turn", state: "done", base: "snapshot", artifact: "snapshot", started_at: at },
        { id: "base-only", kind: "turn", state: "running", agent: "base-agent", base: "start", started_at: "2026-09-05T10:00:00Z" },
    ]) }));
    await f.page.route(/\/console\/attempts\/(unchanged|base-only)\//, (route) => {
        const url = new URL(route.request().url()), base = url.pathname.includes("base-only"), commit = base ? "start" : state.commit;
        assert.equal(route.request().method(), "GET");
        if (url.pathname.endsWith("/changes")) return state.failIndex ? route.fulfill({ status: 503, body: "Index offline" }) : route.fulfill({ json: { attempt: base ? "base-only" : "unchanged", base: commit, artifact: base ? undefined : commit, changes: [] } });
        if (url.pathname.endsWith("/tree")) return route.fulfill({ json: { attempt: base ? "base-only" : "unchanged", commit, which: base ? "base" : "result", dir: "", entries: [{ name: "plain.ts", path: "plain.ts", kind: "file" }] } });
        if (url.pathname.endsWith("/file")) { state.fileReads++; return route.fulfill({ json: { attempt: base ? "base-only" : "unchanged", commit, path: "plain.ts", text: `// ${commit}\nconst plain = true;\n`, size: 40 } }); }
        assert.fail("An unchanged file must not fetch a diff");
    });
    await f.page.getByRole("button", { name: "显示详情", exact: true }).click();
    await f.page.getByRole("tab", { name: "产物", exact: true }).click();
    await f.page.getByRole("button", { name: "浏览文件", exact: true }).first().click();
    const workspace = f.page.getByRole("dialog", { name: "产物工作区" });
    await workspace.getByRole("button", { name: "plain.ts", exact: true }).click();
    await workspace.getByText("// snapshot", { exact: true }).waitFor();
    await workspace.getByRole("button", { name: /选择版本/ }).click();
    await f.page.getByRole("option", { name: /base-agent/ }).click();
    await workspace.getByRole("button", { name: "plain.ts", exact: true }).click();
    await workspace.getByText("// start", { exact: true }).waitFor();
    assert.ok((await workspace.locator(".review-source").innerText()).includes("开始快照"));
    await workspace.getByRole("button", { name: "返回", exact: true }).click();
    state.failIndex = true;
    await f.page.getByRole("button", { name: "浏览文件", exact: true }).first().click();
    await workspace.getByRole("button", { name: "plain.ts", exact: true }).click();
    await workspace.getByText("// snapshot", { exact: true }).waitFor();
    await workspace.getByText("变更状态未统计", { exact: true }).waitFor();
    await workspace.getByRole("button", { name: "仅变更", exact: true }).click();
    await workspace.getByRole("alert").filter({ hasText: "Index offline" }).waitFor();
    state.failIndex = false;
    await workspace.getByRole("button", { name: "重试", exact: true }).click();
    await workspace.getByText("无变更", { exact: true }).waitFor();
    await workspace.getByRole("button", { name: "返回", exact: true }).click();
    await f.page.getByRole("button", { name: "浏览文件", exact: true }).first().click();
    await workspace.getByRole("button", { name: "plain.ts", exact: true }).waitFor();
    state.commit = "updated";
    await workspace.getByRole("button", { name: "plain.ts", exact: true }).click();
    await workspace.getByRole("button", { name: "重新载入快照", exact: true }).waitFor();
    assert.equal(await workspace.getByText("// updated", { exact: true }).count(), 0, "A new snapshot must not appear inside the old tree");
    await workspace.getByRole("button", { name: "重新载入快照", exact: true }).click();
    await workspace.getByRole("button", { name: "plain.ts", exact: true }).click();
    await workspace.getByText("// updated", { exact: true }).waitFor();
    assert.equal(f.calls.length, 0);
};

checks["code-late-source"] = async (f) => {
    const state = await reviewFixture(f);
    const workspace = f.page.getByRole("dialog", { name: "产物工作区" });
    const pending = gate(); state.holdFile = pending.promise; f.releases.push(pending.release);
    await workspace.getByRole("button", { name: "全部文件", exact: true }).click();
    const tree = workspace.getByRole("navigation", { name: "项目文件" });
    await tree.getByRole("button", { name: "README.md", exact: true }).click();
    await tree.getByRole("button", { name: "empty.txt", exact: true }).click();
    await workspace.getByText("空文件", { exact: true }).waitFor();
    pending.release(); await delay(100);
    assert.equal(await workspace.getByText("Unchanged project guide.", { exact: true }).count(), 0, "Late source must not replace the active file");
    assert.equal(f.calls.length, 0);
};

checks["preview-document-links"] = async (f) => {
    await reviewFixture(f);
    const workspace = f.page.getByRole("dialog", { name: "产物工作区" });
    const before = f.page.url();
    await workspace.getByRole("button", { name: "全部文件", exact: true }).click();
    const tree = workspace.getByRole("navigation", { name: "项目文件" });
    await tree.getByRole("button", { name: "README.md", exact: true }).click();
    await workspace.getByText("Unchanged project guide.", { exact: true }).waitFor();
    const site = workspace.getByRole("link", { name: "the site", exact: true });
    assert.equal(await site.getAttribute("target"), "_blank", "A link to the web opens beside the console, not over it");
    assert.equal(await workspace.getByRole("link", { name: "the helper", exact: true }).count(), 0, "A path inside the snapshot must not be a browser navigation");
    for (const [words, title] of [["outside", "链接指向 ../outside.txt，在这里无法打开"], ["top", "链接指向 #project，在这里无法打开"]]) {
        const inert = workspace.locator(".md-link-inert").filter({ hasText: words });
        assert.equal(await inert.getAttribute("title"), title, "A link that leads nowhere readable says so instead of navigating");
    }
    const linked = workspace.getByRole("button", { name: "the helper", exact: true });
    assert.equal(await linked.getAttribute("title"), "在工作区打开 src/helper.ts");
    await linked.click();
    await workspace.getByRole("region", { name: "源码 src/helper.ts", exact: true }).waitFor();
    await workspace.getByText("export const helper = 1;").waitFor();
    assert.equal(f.page.url(), before, "Following a document link keeps the console on its own page");
    await workspace.getByRole("button", { name: "阅读 README.md", exact: true }).click();
    await workspace.getByText("Unchanged project guide.", { exact: true }).waitFor();
    assert.equal(f.calls.length, 0);
};

// These documents exist only in mocked snapshot responses, never in a user's
// project. Exercise the real file tree and FilePreview, not a hand-built iframe.
const previewFiles = {
    "static.html": `<!doctype html><html><head><meta name="viewport" content="width=device-width">
<style>body { margin: 12px; } h1 { color: rgb(12, 34, 56); }</style></head>
<body><h1>Static preview report</h1><p>Snapshot HTML content</p></body></html>`,
    "interactive.html": `<!doctype html><html><head><meta name="viewport" content="width=device-width">
<style>
body { margin: 12px; background-color: rgb(1, 2, 3) !important; color: white; }
button { color: rgb(4, 5, 6) !important; }
.review-file-header { display: none !important; }
</style></head><body>
<h1>Interactive preview</h1><button id="increment">Increment</button><output id="count">0</output>
<p id="ready">Script pending</p><pre id="isolation"></pre>
<script>
const count = document.getElementById("count");
document.getElementById("increment").addEventListener("click", () => { count.textContent = String(Number(count.textContent) + 1); });
const results = {};
for (const [name, probe] of Object.entries({
    parentDOM: () => { parent.document.body.dataset.previewEscaped = "yes"; },
    localStorage: () => { localStorage.setItem("preview-sentinel", "escaped"); },
    sessionStorage: () => { sessionStorage.setItem("preview-sentinel", "escaped"); },
    parentStorage: () => { parent.localStorage.setItem("preview-sentinel", "escaped"); }
})) {
    try { probe(); results[name] = "allowed"; } catch (error) { results[name] = error.name; }
}
document.getElementById("isolation").textContent = JSON.stringify(results);
document.getElementById("ready").textContent = "Inline script ready";
</script></body></html>`,
    "diagram.svg": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 320 140" width="100%"
onload="document.getElementById('load-state').textContent = 'Load handler ran'">
<title>Script-free diagram</title><rect width="320" height="140" fill="#eef"/>
<text id="script-state" x="12" y="30">SVG script pending</text>
<text id="load-state" x="12" y="60">SVG load pending</text>
<text id="click-state" x="12" y="100" onclick="this.textContent = 'Click handler ran'">Click SVG probe</text>
<script>document.getElementById('script-state').textContent = 'SVG script ran';</script>
</svg>`,
    "report.html": `<!doctype html><html><head><meta name="viewport" content="width=device-width">
<style>body { margin: 0; } header { position: sticky; top: 0; height: 48px; background: rgb(20, 40, 80); color: white; } h1, h2 { margin: 0; padding: 12px; font-size: 20px; line-height: 24px; } section { height: 400px; border-bottom: 1px solid #ccc; }</style>
</head><body><header><h1>Report bar</h1></header>
${Array.from({ length: 12 }, (_, i) => `<section id="s${i + 1}"><h2>Section ${i + 1}</h2></section>`).join("\n")}
</body></html>`,
};

async function filePreviewFixture(f) {
    const state = await reviewFixture(f, { open: false, files: previewFiles });
    await f.page.getByRole("button", { name: "浏览文件", exact: true }).click();
    const workspace = f.page.getByRole("dialog", { name: "产物工作区", exact: true });
    const tree = workspace.getByRole("navigation", { name: "项目文件", exact: true });
    await tree.waitFor();
    const iframe = workspace.locator("iframe.file-preview-frame");
    const frame = workspace.frameLocator("iframe.file-preview-frame");
    const open = async (file) => {
        if (!await tree.isVisible()) await workspace.getByRole("button", { name: "文件导航", exact: true }).click();
        await tree.getByRole("button", { name: file, exact: true }).click();
        await workspace.getByRole("heading", { name: file, exact: true }).waitFor();
        await eventually(async () => await iframe.getAttribute("srcdoc") === previewFiles[file], `FilePreview must load ${file} from the mocked snapshot`);
    };
    return { state, workspace, tree, iframe, frame, open };
}

checks["preview-html-interaction-isolation"] = async (f) => {
    const { workspace, iframe, frame, open } = await filePreviewFixture(f);
    await open("static.html");
    await frame.getByRole("heading", { name: "Static preview report", exact: true }).waitFor();
    await frame.getByText("Snapshot HTML content", { exact: true }).waitFor();
    assert.equal(await frame.locator("h1").evaluate((element) => getComputedStyle(element).color), "rgb(12, 34, 56)", "Static HTML must render its own CSS");
    const parentStyle = () => f.page.evaluate(() => ({
        background: getComputedStyle(document.body).backgroundColor,
        headerDisplay: getComputedStyle(document.querySelector(".review-file-header")).display,
        buttonColor: getComputedStyle(document.querySelector(".review-file-actions button")).color,
    }));
    const beforeStyle = await parentStyle(), beforeURL = f.page.url();
    await f.page.evaluate(() => {
        localStorage.setItem("preview-sentinel", "parent-local");
        sessionStorage.setItem("preview-sentinel", "parent-session");
    });
    await open("interactive.html");
    await frame.getByText("Inline script ready", { exact: true }).waitFor();
    assert.deepEqual(JSON.parse(await frame.locator("#isolation").innerText()), {
        parentDOM: "SecurityError", localStorage: "SecurityError", sessionStorage: "SecurityError", parentStorage: "SecurityError",
    }, "Running inline JS must be unable to access the parent DOM or browser storage");
    assert.equal(await iframe.getAttribute("sandbox"), "allow-scripts", "HTML allows scripts but no same-origin or navigation privileges");
    assert.equal(await iframe.getAttribute("referrerpolicy"), "no-referrer");
    assert.equal(await frame.locator("body").evaluate((element) => getComputedStyle(element).backgroundColor), "rgb(1, 2, 3)", "The isolation probe's CSS must actually apply inside the preview");
    await frame.getByRole("button", { name: "Increment", exact: true }).click();
    await frame.getByRole("button", { name: "Increment", exact: true }).click();
    assert.equal(await frame.locator("#count").innerText(), "2", "Inline event listeners must work repeatedly");
    assert.deepEqual(await parentStyle(), beforeStyle, "Artifact CSS must not restyle or hide console controls");
    assert.deepEqual(await f.page.evaluate(() => ({
        escaped: document.body.dataset.previewEscaped ?? null,
        local: localStorage.getItem("preview-sentinel"), session: sessionStorage.getItem("preview-sentinel"),
    })), { escaped: null, local: "parent-local", session: "parent-session" }, "The parent document and its storage must remain untouched");
    assert.equal(f.page.url(), beforeURL);
    await workspace.getByRole("button", { name: "源码", exact: true }).click();
    await workspace.getByRole("region", { name: "源码 interactive.html", exact: true }).waitFor();
    assert.equal(f.calls.length, 0, "Preview interactions must not submit console work");
};

async function assertInertSVG(frame) {
    await frame.locator("svg").waitFor();
    await eventually(() => frame.locator("svg").evaluate((element) => element.ownerDocument.readyState === "complete"), "SVG must finish loading before testing blocked scripts");
    assert.equal(await frame.locator("#script-state").textContent(), "SVG script pending", "SVG script elements must not execute");
    assert.equal(await frame.locator("#load-state").textContent(), "SVG load pending", "SVG onload must not execute");
    await frame.locator("#click-state").click();
    assert.equal(await frame.locator("#click-state").textContent(), "Click SVG probe", "SVG click handlers must not execute");
}

checks["preview-svg-scripts-disabled"] = async (f) => {
    const { iframe, frame, open } = await filePreviewFixture(f);
    // Transition from a script-enabled HTML frame: SVG must not inherit it.
    await open("interactive.html");
    await frame.getByText("Inline script ready", { exact: true }).waitFor();
    await open("diagram.svg");
    await assertInertSVG(frame);
    assert.equal(await iframe.getAttribute("sandbox"), "", "SVG must retain an empty sandbox, not omit the attribute");
    assert.equal(await frame.locator("h1").count(), 0, "Switching to SVG must remove the previous HTML document");
    await open("interactive.html");
    await frame.getByText("Inline script ready", { exact: true }).waitFor();
    await frame.getByRole("button", { name: "Increment", exact: true }).click();
    assert.equal(await frame.locator("#count").innerText(), "1", "Returning to HTML must restore script-enabled interaction");
    assert.equal(f.calls.length, 0);
};

// A page is read in its own viewport: the frame takes the whole reading
// area and the document scrolls inside it, the way its author laid it out
// — a sticky bar stays put, a 100vh section fills the pane, a scroll-spy
// index follows the reader. A fixed-height box with the pane empty below
// it is neither a viewport nor a document.
checks["preview-html-viewport"] = async (f) => {
    const { workspace, iframe, frame, open } = await filePreviewFixture(f);
    await open("report.html");
    await frame.getByRole("heading", { name: "Section 12", exact: true }).waitFor();
    const pane = workspace.locator(".review-code-scroll");
    const layout = async () => {
        const [frameBox, paneBox] = await Promise.all([iframe.boundingBox(), pane.boundingBox()]);
        const overflow = await pane.evaluate((element) => element.scrollHeight - element.clientHeight);
        return { frameBox, paneBox, overflow };
    };
    const fits = async (when) => {
        const { frameBox, paneBox, overflow } = await layout();
        assert.ok(overflow <= 0, `${when}: the reading pane must not scroll around the frame (overflow ${overflow}px)`);
        const slack = paneBox.y + paneBox.height - (frameBox.y + frameBox.height);
        assert.ok(slack >= 0 && slack <= 40, `${when}: the frame must reach the bottom of the reading area, not stop ${slack}px above it`);
        return frameBox.height;
    };
    const tall = await fits("1000px window");
    assert.ok(tall > 700, `The frame must use the reading area it is given, not a fixed share of the window (${tall}px)`);
    await f.page.screenshot({ path: path.join(output, "preview-html-viewport.png") });
    // The document scrolls inside the frame and its sticky bar keeps its place.
    const bar = frame.locator("header");
    const frameTop = (await iframe.boundingBox()).y + 1;
    assert.ok(Math.abs((await bar.boundingBox()).y - frameTop) <= 1, "The page starts at the top of its frame");
    await frame.locator("body").evaluate((body) => body.ownerDocument.defaultView.scrollTo(0, 1600));
    await eventually(async () => (await frame.locator("#s5").boundingBox()).y < frameTop + 200, "The page must scroll inside its frame");
    assert.ok(Math.abs((await bar.boundingBox()).y - frameTop) <= 1, "A sticky bar stays at the top of the frame while the page scrolls");
    await f.page.setViewportSize({ width: 1600, height: 700 });
    const short = await fits("700px window");
    assert.ok(tall - short >= 250, `The frame must follow the window height: ${tall}px then ${short}px`);
    await open("diagram.svg");
    await assertInertSVG(frame);
    await fits("SVG in a 700px window");
    assert.equal(f.calls.length, 0);
};

checks["preview-html-source-switch-narrow"] = async (f) => {
    const { state, workspace, iframe, frame, open } = await filePreviewFixture(f);
    await f.page.setViewportSize({ width: 390, height: 844 });
    await open("interactive.html");
    await frame.getByText("Inline script ready", { exact: true }).waitFor();
    await frame.getByRole("button", { name: "Increment", exact: true }).press("Enter");
    assert.equal(await frame.locator("#count").innerText(), "1", "Preview interaction must remain keyboard-accessible in a narrow window");
    await workspace.getByRole("button", { name: "源码", exact: true }).click();
    const source = workspace.getByRole("region", { name: "源码 interactive.html", exact: true });
    await source.waitFor();
    assert.ok((await source.innerText()).includes('document.getElementById("increment").addEventListener'), "Source mode must show the literal inline script");
    assert.equal(await iframe.count(), 0, "Source mode must unmount the executable preview");
    await open("static.html");
    await frame.getByRole("heading", { name: "Static preview report", exact: true }).waitFor();
    await workspace.getByRole("button", { name: "阅读 interactive.html", exact: true }).click();
    await source.waitFor();
    assert.equal(await iframe.count(), 0, "Returning to a file must remember its source mode");
    await workspace.getByRole("button", { name: "预览", exact: true }).click();
    await frame.getByText("Inline script ready", { exact: true }).waitFor();
    assert.equal(await frame.locator("#count").innerText(), "0", "Reopening preview must render a fresh document rather than stale interactive state");
    await frame.getByRole("button", { name: "Increment", exact: true }).click();
    assert.equal(await frame.locator("#count").innerText(), "1");
    await noHorizontalOverflow(f.page);
    const bounds = await iframe.boundingBox();
    assert.ok(bounds && bounds.width > 100 && bounds.x >= 0 && bounds.x + bounds.width <= 391, `HTML preview must fit the narrow reading pane: ${JSON.stringify(bounds)}`);
    await visibleControl(workspace.getByRole("button", { name: "源码", exact: true }), "Source switch");
    await f.page.screenshot({ path: path.join(output, "preview-html-narrow.png") });
    await open("diagram.svg");
    await assertInertSVG(frame);
    await noHorizontalOverflow(f.page);
    await f.page.screenshot({ path: path.join(output, "preview-svg-narrow.png") });
    assert.equal(state.reads.filter((read) => read.endpoint === "file" && read.file === "interactive.html").length, 1, "File and mode switches must reuse the loaded snapshot source");
    assert.equal(f.calls.length, 0);
};

checks["preview-width"] = async (f) => {
    await f.page.setViewportSize({ width: 1280, height: 900 });
    await reviewFixture(f);
    const workspace = f.page.getByRole("dialog", { name: "产物工作区" });
    await workspace.getByRole("button", { name: "全部文件", exact: true }).click();
    await workspace.getByRole("navigation", { name: "项目文件" }).getByRole("button", { name: "README.md", exact: true }).click();
    const prose = workspace.locator(".file-preview-prose");
    await prose.waitFor();
    const measure = async () => (await prose.boundingBox()).width;
    const narrowWindow = await measure();
    await f.page.setViewportSize({ width: 1800, height: 900 });
    await f.page.clock.runFor(350);
    const wideWindow = await measure();
    assert.ok(wideWindow > narrowWindow + 40, `Reading width must follow the window: ${narrowWindow} then ${wideWindow}`);
    const widths = workspace.getByRole("group", { name: "预览宽度", exact: true });
    await widths.getByRole("button", { name: "窄", exact: true }).click();
    const narrow = await measure();
    assert.ok(narrow < wideWindow, "A tighter column is narrower than the comfortable measure");
    await widths.getByRole("button", { name: "全宽", exact: true }).click();
    const full = await measure();
    assert.ok(full > wideWindow, "Full width uses the whole reading pane");
    await noHorizontalOverflow(f.page);
    await widths.getByRole("button", { name: "适中", exact: true }).click();
    await f.page.screenshot({ path: path.join(output, "preview-width.png") });
    await widths.getByRole("button", { name: "全宽", exact: true }).click();
    await workspace.getByRole("button", { name: "返回", exact: true }).click();
    await f.page.getByRole("button", { name: "浏览文件", exact: true }).first().click();
    await workspace.getByRole("navigation", { name: "项目文件" }).getByRole("button", { name: "README.md", exact: true }).click();
    await prose.waitFor();
    assert.equal(await workspace.getByRole("button", { name: "全宽", exact: true }).getAttribute("aria-pressed"), "true", "The chosen width is remembered for the next file");
    assert.equal(f.calls.length, 0);
};

checks["code-source-feedback"] = async (f) => {
    await reviewFixture(f);
    const workspace = f.page.getByRole("dialog", { name: "产物工作区" });
    const sample = "<script>window.untrusted = true</script>\r\nconst value = '<img src=x>';\r\n";
    const value = { text: sample, size: sample.length, truncated: true, binary: false };
    await f.page.route(/\/console\/attempts\/review-attempt\/file\?/, (route) => route.fulfill({ json: { attempt: "review-attempt", commit: "after", path: new URL(route.request().url()).searchParams.get("path"), ...value } }));
    await f.page.evaluate(() => {
        window.copiedSource = ""; window.clipboardRejects = true;
        Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (text) => { if (window.clipboardRejects) throw new Error("denied"); window.copiedSource = text; } } });
    });
    await workspace.getByRole("button", { name: "全部文件", exact: true }).click();
    const tree = workspace.getByRole("navigation", { name: "项目文件" });
    await tree.getByRole("button", { name: "README.md", exact: true }).click();
    // Markdown opens rendered; this check is about reading it as source.
    await workspace.getByRole("button", { name: "源码", exact: true }).click();
    await workspace.getByText("文件较大，仅取回部分内容。当前浏览与复制均不包含未取回的部分。", { exact: true }).waitFor();
    assert.equal(await workspace.locator(".source-text script,.source-text img").count(), 0, "Untrusted source must never become executable markup");
    assert.equal(await f.page.evaluate(() => window.untrusted), undefined);
    await workspace.getByRole("button", { name: "复制", exact: true }).click();
    await workspace.getByText("复制失败，请允许剪贴板权限，或选中文本手动复制。", { exact: true }).waitFor();
    await f.page.evaluate(() => { window.clipboardRejects = false; });
    await workspace.getByRole("button", { name: "复制", exact: true }).click();
    await workspace.getByRole("button", { name: "已复制", exact: true }).waitFor();
    assert.equal(await f.page.evaluate(() => window.copiedSource), sample, "Copy retains source line endings and characters");
    value.binary = true; value.text = ""; value.truncated = false;
    await tree.getByRole("button", { name: "new.txt", exact: true }).click();
    await workspace.getByText("无法显示为源码。", { exact: true }).waitFor();
    assert.equal(await workspace.getByRole("button", { name: "复制", exact: true }).count(), 0);
    assert.equal(f.calls.length, 0);
};

checks["code-nested-repository"] = async (f) => {
    await reviewFixture(f);
    const workspace = f.page.getByRole("dialog", { name: "产物工作区" });
    await workspace.getByRole("button", { name: "返回", exact: true }).click();
    let sourceRequests = 0;
    await f.page.route(/\/console\/attempts\/review-attempt\//, (route) => {
        const url = new URL(route.request().url());
        if (url.pathname.endsWith("/changes")) return route.fulfill({ json: { attempt: "review-attempt", base: "before", artifact: "after", changes: [{ path: "vendor", status: "M", added: 1, deleted: 1 }] } });
        if (url.pathname.endsWith("/tree")) return route.fulfill({ json: { attempt: "review-attempt", commit: "after", which: "result", entries: [{ path: "vendor", name: "vendor", kind: "repo" }] } });
        if (url.pathname.endsWith("/file")) { sourceRequests++; return route.fulfill({ status: 400, body: "not a file" }); }
        return route.fulfill({ json: { path: "vendor", diff: "diff --git a/vendor b/vendor\nindex 111..222 160000\n--- a/vendor\n+++ b/vendor\n@@ -1 +1 @@\n-Subproject commit 111\n+Subproject commit 222\n" } });
    });
    await f.page.getByRole("button", { name: "查看变更", exact: true }).first().click();
    await workspace.getByText(/^嵌套仓库 · 仅显示提交变化/).waitFor();
    assert.equal(await workspace.getByRole("button", { name: "源码", exact: true }).isEnabled(), false);
    assert.equal(sourceRequests, 0, "A gitlink must not be read as a source file");
};

checks["skill-source-controls"] = async (f) => {
    const names = ["alpha", "long-skill-name-".repeat(8)];
    const skills = names.map((name) => ({ name, path: `/test/skills/${name}`, root: "/test/skills", source: "example-skills", description: "A useful skill with a concise description.", enabled: false, agents: [], projects: [] }));
    const source = { slug: "example-skills", url: "https://github.com/example/skills.git", root: "/test/skills", head: "0123456", skills: names };
    let hold = null, rejectInstall = false;
    await f.page.route(/\/console\/skills(?:\/|$)/, async (route) => {
        const req = route.request(), pathname = new URL(req.url()).pathname;
        if (req.method() !== "GET") {
            const input = req.postDataJSON(); f.calls.push({ path: pathname, ...input });
            if (hold) await hold;
            if (pathname.endsWith("/sources") && rejectInstall) return route.fulfill({ status: 400, body: "Invalid source" });
            if (req.method() === "PUT") skills.find((skill) => pathname.endsWith(encodeURIComponent(skill.name))).enabled = input.enabled;
            return route.fulfill({ json: { ok: true } });
        }
        if (pathname.endsWith("/machines")) return route.fulfill({ json: { machines: [] } });
        if (pathname === "/console/skills") return route.fulfill({ json: { skills, sources: [source], search_paths: [], nodes: [] } });
        return route.fulfill({ json: { name: "alpha", path: "/test/skills/alpha", content: "# Alpha\n\nSkill documentation." } });
    });
    await f.page.getByRole("link", { name: "技能", exact: true }).click();
    const input = f.page.getByRole("textbox", { name: "仓库地址", exact: true });
    const install = f.page.getByRole("button", { name: "安装", exact: true });
    await input.fill("example/new-skills");
    const pending = gate(); hold = pending.promise; f.releases.push(pending.release);
    await install.dblclick();
    await eventually(() => f.calls.length === 1, "Installation should submit once");
    assert.equal(await input.isEnabled(), false, "A pending install must not clear a newer repository entry");
    pending.release();
    await eventually(() => input.isEnabled(), "Installation form must recover");
    assert.equal(await input.inputValue(), "");
    assert.equal(f.calls.length, 1, "Repeated installation click must not repeat the request");
    hold = null; rejectInstall = true;
    await input.fill("bad/source"); await input.press("Enter");
    await f.page.getByRole("alert").filter({ hasText: "Invalid source" }).waitFor();
    assert.equal(await input.inputValue(), "bad/source", "Failed install must preserve its address for correction");
    const card = f.page.locator(".skill-source-card");
    await card.getByText("管理技能", { exact: true }).click();
    await card.locator("label").filter({ has: f.page.getByRole("switch", { name: "启用 alpha", exact: true }) }).click();
    await card.getByText(/1 \/ 2 个已启用/).waitFor();
    await card.getByRole("button", { name: "查看 alpha 文档", exact: true }).click();
    await f.page.getByRole("dialog").getByText("Skill documentation.", { exact: true }).waitFor();
    await f.page.getByRole("button", { name: "关闭", exact: true }).click();
    for (const width of [1600, 390]) {
        await f.page.setViewportSize({ width, height: 900 });
        await eventually(async () => {
            const a = await input.boundingBox(), b = await install.boundingBox();
            return Math.abs(a.y - b.y) <= 1 && Math.abs(a.y + a.height - b.y - b.height) <= 1;
        }, "Install button must align with the input after responsive layout settles");
        await noHorizontalOverflow(f.page);
    }
};

checks["skill-toggle-layout"] = async (f) => {
    const skills = Array.from({ length: 32 }, (_, i) => ({ name: `skill-${String(i).padStart(2, "0")}`, path: `/test/skills/skill-${i}`, root: "/test/skills", source: "sample-skills", description: "A skill for testing enablement in a long, scrollable directory.", enabled: i < 8, agents: [], projects: [] }));
    const source = { slug: "sample-skills", url: "https://github.com/example/skills.git", root: "/test/skills", skills: skills.map((skill) => skill.name) };
    await f.page.route(/\/console\/skills(?:\/|$)/, async (route) => {
        const request = route.request(), pathname = new URL(request.url()).pathname;
        if (request.method() === "PUT") {
            const input = request.postDataJSON(); f.calls.push({ path: pathname, ...input });
            skills.find((skill) => pathname.endsWith(skill.name)).enabled = input.enabled;
            return route.fulfill({ json: { ok: true } });
        }
        if (pathname.endsWith("/machines")) return route.fulfill({ json: { machines: [{ name: "test-node", up: true, hub: true, skills: Array.from({ length: 30 }, (_, i) => ({ name: `local-${i}`, path: `/test/local-${i}`, description: "Available on this machine.", loaded: false })) }] } });
        return route.fulfill({ json: { skills, sources: [source], search_paths: [], nodes: [] } });
    });
    await f.page.getByRole("link", { name: "技能", exact: true }).click();
    await f.page.getByText("管理技能", { exact: true }).click();
    const toggle = f.page.locator(".skill-source-card label").filter({ has: f.page.getByRole("switch", { name: "启用 skill-22", exact: true }) });
    await toggle.scrollIntoViewIfNeeded();
    const geometry = () => f.page.evaluate(() => ({ window: window.scrollY, root: document.querySelector(".workbench-shell").scrollTop, main: document.querySelector("main").getBoundingClientRect().toJSON(), sidebar: document.querySelector(".app-sidebar").getBoundingClientRect().toJSON(), height: window.innerHeight }));
    for (const keyboard of [false, true]) {
        const before = await geometry();
        if (keyboard) await toggle.getByRole("switch").press("Space");
        else await toggle.click();
        await f.page.locator(".skill-source-card").getByText(`${keyboard ? 8 : 9} / 32 个已启用`, { exact: true }).waitFor();
        await eventually(() => toggle.getByRole("switch").isEnabled(), "The skill switch must recover after saving");
        const after = await geometry();
        assert.equal(after.window, before.window, "Skill enablement must not scroll the outer document");
        assert.equal(after.root, before.root, "Skill enablement must not scroll the app shell");
        assert.equal(after.main.bottom, after.height, "Main content must still fill the viewport without a black gap");
        assert.equal(after.sidebar.bottom, after.height, "The sidebar must remain anchored to the viewport");
    }
    assert.equal(f.calls.length, 2, "Each mouse/keyboard toggle sends exactly one update");
};

checks["readmodel-unknown"] = async (f) => {
    const state = { ...usageState(), tasks: [{ ...task("11", A, "scratch"), execution: "unknown", lane: "unknown" }], agents: [{ id: "test-agent", harness: "mock", eligible: true, activity_known: false, busy: 0, activities: [] }], sources: [
        { name: "ledger", wired: true, error: "partial ledger read" },
        { name: "ledger-live", wired: true, error: "activity unavailable" },
        { name: "ledger-attention", wired: true, error: "attention unavailable" },
    ] };
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState(state) }));
    await f.page.goto(`${app.url}/#/console?view=board`); await f.page.reload();
    await f.page.getByText("Task 11", { exact: true }).waitFor();
    await f.page.getByText("状态未知", { exact: true }).first().waitFor();
    await f.page.getByRole("link", { name: "仪表盘", exact: true }).click();
    await f.page.getByRole("heading", { name: "用量概览", exact: true }).waitFor();
    await f.page.getByRole("region", { name: "区间用量汇总", exact: true }).waitFor();
    await f.page.getByRole("link", { name: "待处理", exact: true }).click();
    await f.page.getByText(/待处理信息尚未完整读取/).waitFor();
    assert.equal(await f.page.getByText("暂无待处理请求", { exact: true }).count(), 0, "Unavailable attention cannot be presented as no requests");
    await f.page.getByRole("link", { name: "资源", exact: true }).click();
    await f.page.getByRole("tab", { name: /^Agent/ }).click();
    await f.page.getByText("活动状态未知", { exact: true }).waitFor();
};

checks["profile-regenerate"] = async (f) => {
    // Generating a profile is still the reader's message: the confirmation
    // opens a personal thread with the instructions written out, and nothing
    // is sent until they send it.
    await f.page.route("**/console/home", (route) => route.fulfill({ json: { path: "/test/home/.steve", files: [{ name: "USER.md", text: "# 用户档案\n\n旧的内容", bytes: 32, budget: 8192 }], total_budget: 32768, owner_bytes: 32, guest_bytes: 0, warnings: [], projects: [] } }));
    await f.page.goto(`${app.url}/#/home?doc=${encodeURIComponent("file:USER.md")}`);
    await f.page.getByRole("button", { name: "自动生成", exact: true }).click();
    await f.page.getByRole("button", { name: "去新会话", exact: true }).click();
    await eventually(async () => (await draftOf(f.box)).includes("/test/home/.steve/USER.md"), "The prepared prompt must land in the new thread's box");
    assert.ok((await draftOf(f.box)).includes("USER.md"), "The prompt must name the file being rewritten");
    assert.equal(f.queued().length, 0, "Preparing a prompt must never send it");
    const bound = f.calls.filter((c) => c.path.endsWith("/initialize"));
    assert.equal(bound.length, 1, "Exactly one thread is created");
    assert.equal(bound[0].project, "home", "The thread belongs to the personal project");
};

checks["composer-keys"] = async (f) => {
    // Enter sends and Shift+Enter is a newline, the way every chat box
    // people already use behaves. Enter is also the key an input method
    // confirms a candidate with, and that keystroke must never send.
    await f.box.fill("first line");
    await f.box.press("Shift+Enter");
    assert.equal(f.queued().length, 0, "Shift+Enter must not send");
    assert.equal(await draftOf(f.box), "first line\n", "Shift+Enter inserts a newline");
    await f.box.type("**second** line");
    await f.box.evaluate((box) => box.dispatchEvent(new CompositionEvent("compositionstart", { bubbles: true })));
    const composingDraft = await draftOf(f.box);
    await f.box.press("Enter");
    assert.equal(f.queued().length, 0, "Enter must not send while an input method is composing");
    assert.equal(await draftOf(f.box), composingDraft, "Nor may it leave a newline behind while composing");
    await f.box.evaluate((box) => box.dispatchEvent(new CompositionEvent("compositionend", { bubbles: true })));
    const settledDraft = await draftOf(f.box);
    // Still the same keystroke: the key that confirmed the candidate has not
    // been let go of, so the Enter that arrives with the composition's end is
    // the method's, however long the machine took to deliver it.
    await f.page.keyboard.down("Enter");
    assert.equal(f.queued().length, 0, "The Enter that lands with compositionend must not send either");
    assert.equal(await draftOf(f.box), settledDraft, "That same Enter must not leave a stray newline behind either");
    await f.page.keyboard.up("Enter");
    await f.box.press("Enter");
    await eventually(() => f.queued().length === 1, "Enter sends once the confirming key has been released");
    assert.equal(f.queued()[0].input, "first line\n**second** line", "The whole multi-line draft is sent");
    f.replies[A].push({ id: "sent-md", kind: "sent", conversation: A, at, input: "first line\n**second** line", text: "" });
    await f.emit({ kind: "console.sent", exchange_id: "md-turn", reply_id: "sent-md", text: "first line\n**second** line" });
    await f.page.locator(".message-user-body strong", { hasText: "second" }).waitFor();
};

checks["composer-markdown"] = async (f) => {
    // The draft is markdown and the box draws it: a heading carries weight,
    // a bullet is a bullet, bold is bold, and the marks that said so step
    // aside. The line the caret is on is the exception — it shows its own
    // source, so a mark can still be typed or repaired.
    const box = f.box, lines = f.page.locator(".composer-box .cm-line");
    const draft = "## 发布 **v0.4**\n- 升级 `steve-node`\n- [x] 备份已确认\n> *注意*：2 * 3 * 4 is 24\n普通一行";
    await box.fill(draft);
    assert.equal(await draftOf(box), draft, "Drawing a draft may not rewrite it");
    assert.equal(await lines.nth(0).innerText(), "发布 v0.4", "A heading reads as a heading, without its hashes");
    assert.match(await lines.nth(1).innerText(), /^•\s*升级 steve-node$/, "A list item gets a bullet, and a code span drops its backticks");
    assert.match(await lines.nth(2).innerText(), /^☑\s*备份已确认$/, "A ticked task reads as a ticked box");
    assert.equal(await lines.nth(3).innerText(), "注意：2 * 3 * 4 is 24", "A quote drops its angle bracket, and arithmetic is not emphasis");
    const size = (n) => lines.nth(n).evaluate((el) => parseFloat(getComputedStyle(el).fontSize));
    assert.ok(await size(0) > await size(4), "A heading is drawn larger than body text");
    const face = (sel) => f.page.locator(sel).first().evaluate((el) => getComputedStyle(el).fontFamily);
    assert.notEqual(await face(".composer-box .cm-md-code"), await face(".composer-box .cm-line"), "A code span is set in a different face");
    assert.equal(await f.page.locator(".composer-box .cm-md-strong").count(), 1, "Bold is marked up once, marks and all excluded");

    await lines.nth(0).click();
    assert.equal(await lines.nth(0).innerText(), "## 发布 **v0.4**", "The line being edited shows its source");
    assert.match(await lines.nth(1).innerText(), /^•\s*升级 steve-node$/, "while the lines around it stay drawn");
    await lines.nth(1).click();
    assert.match(await lines.nth(1).innerText(), /^•\s*升级 `steve-node`$/, "A bullet is what an item looks like even while it is written");
    assert.equal(await draftOf(box), draft, "Moving the caret changes nothing that will be sent");

    // The bullet appears as `- ` is typed, which is the whole point.
    await box.fill("");
    await box.pressSequentially("- 新一项");
    assert.match(await lines.nth(0).innerText(), /^•\s*新一项$/, "Typing a list mark draws its bullet straight away");
    assert.equal(await draftOf(box), "- 新一项", "What is drawn is still what will be sent");

    // A newline inside a list carries the list; an item left empty ends it.
    await box.fill("- first");
    await box.press("Shift+Enter");
    assert.equal(await draftOf(box), "- first\n- ", "Shift+Enter continues the list");
    await box.pressSequentially("second");
    await box.press("Shift+Enter");
    await box.press("Shift+Enter");
    assert.equal(await draftOf(box), "- first\n- second\n", "An empty item takes its marker back");
    assert.equal(f.queued().length, 0, "None of that sends");
    await box.fill("1. one");
    await box.press("Shift+Enter");
    assert.equal(await draftOf(box), "1. one\n2. ", "A numbered list counts on");
};

checks["theme-palettes"] = async (f) => {
    // A palette is picked from the toolbar, survives a reload before the
    // app has even booted, and stays legible: every scheme shipped here is
    // measured rather than eyeballed, so adding one cannot quietly ship an
    // unreadable console.
    await f.page.setViewportSize({ width: 1280, height: 900 });
    const open = f.page.getByRole("button", { name: /^主题/ });
    await open.click();
    await f.page.getByRole("menuitemradio", { name: "Dracula", exact: true }).click();
    await eventually(async () => await f.page.evaluate(() => document.documentElement.dataset.theme) === "dracula", "Picking a palette applies it");
    assert.ok(await f.page.evaluate(() => document.documentElement.classList.contains("dark-mode")), "A dark palette reads as dark mode");
    await f.page.reload({ waitUntil: "commit" });
    assert.equal(await f.page.evaluate(() => document.documentElement.dataset.theme), "dracula", "The palette is painted before the app boots, so a reload never flashes");
    await open.click();
    await f.page.getByRole("menuitemradio", { name: "Atom One Light", exact: true }).click();
    await eventually(async () => !(await f.page.evaluate(() => document.documentElement.classList.contains("dark-mode"))), "A light palette drops dark mode");

    // Every palette the stylesheet defines must also be offered, and the
    // menu must not offer one the stylesheet never defined.
    const defined = await f.page.evaluate(() => {
        const ids = new Set();
        for (const sheet of [...document.styleSheets]) {
            let rules = []; try { rules = [...sheet.cssRules]; } catch { continue; }
            for (const rule of rules) for (const hit of (rule.selectorText || "").matchAll(/\[data-theme="([^"]+)"\]/g)) ids.add(hit[1]);
        }
        return [...ids];
    });
    assert.ok(defined.length >= 8, `The stylesheet must define the palettes: ${JSON.stringify(defined)}`);
    await open.click();
    const offered = await f.page.evaluate(() => [...document.querySelectorAll('[role="menuitemradio"]')].map((n) => n.getAttribute("data-key") || n.id));
    await f.page.keyboard.press("Escape");
    for (const id of defined) assert.ok(offered.includes(id), `The menu must offer ${id}, which the stylesheet defines`);

    // The browser resolves color-mix() into whatever space it likes, so the
    // colours are rasterised and read back as plain sRGB rather than parsed.
    const contrast = await f.page.evaluate((ids) => {
        const probe = document.createElement("div");
        document.body.append(probe);
        const canvas = document.createElement("canvas");
        canvas.width = canvas.height = 1;
        const paint = canvas.getContext("2d", { willReadFrequently: true });
        const luminance = (token) => {
            probe.style.backgroundColor = `var(${token})`;
            paint.clearRect(0, 0, 1, 1);
            paint.fillStyle = getComputedStyle(probe).backgroundColor;
            paint.fillRect(0, 0, 1, 1);
            const [r, g, b] = [...paint.getImageData(0, 0, 1, 1).data].slice(0, 3).map((v) => v / 255).map((c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
            return 0.2126 * r + 0.7152 * g + 0.0722 * b;
        };
        const ratio = (a, b) => { const [x, y] = [luminance(a), luminance(b)].sort((m, n) => n - m); return (x + 0.05) / (y + 0.05); };
        const previous = document.documentElement.dataset.theme;
        const report = {};
        for (const id of ids) {
            document.documentElement.dataset.theme = id;
            report[id] = {
                body: ratio("--color-text-primary", "--color-bg-primary"),
                muted: ratio("--color-text-tertiary", "--color-bg-primary"),
                faint: ratio("--color-text-quaternary", "--color-bg-primary"),
                accent: ratio("--color-bg-brand-solid", "--color-bg-primary"),
                line: ratio("--color-border-primary", "--color-bg-primary"),
                sidebar: ratio("--color-text-primary", "--sidebar-surface"),
            };
        }
        if (previous) document.documentElement.dataset.theme = previous; else delete document.documentElement.dataset.theme;
        probe.remove();
        return report;
    }, defined);
    const floor = { body: 7, muted: 4.5, faint: 3.2, accent: 3, line: 1.2, sidebar: 7 };
    for (const [id, measured] of Object.entries(contrast)) {
        for (const [role, value] of Object.entries(measured)) {
            assert.ok(value >= floor[role], `${id} is not legible: ${role} contrast is ${value.toFixed(2)}, below ${floor[role]}`);
        }
    }
};

checks["composer-columns"] = async (f) => {
    // The control row answers two questions. Where the message goes reads
    // from the left; how the turn will run reads from the right and ends at
    // the send button. Too narrow for two groups and the row closes ranks,
    // so no control is stranded in the middle of an empty row.
    await f.page.route("**/console/context?*", (route) => route.fulfill({ json: { enabled: true, context: { conversation: A, project: { ...project("scratch"), bound: true }, agents: [], agent: { id: "test-agent", node: "test-node", harness: "codex", model: "gpt-6-astra", ready: true, usable: true } } } }));
    await f.page.route("**/console/selectors?*", (route) => route.fulfill({ json: { model: "gpt-6-astra", preferred: {}, models: [{ Value: "gpt-6-astra", Label: "gpt-6-astra" }], options: [{ ID: "reasoning_effort", Name: "Reasoning effort", Category: "thought_level", Current: "high", Choices: [{ Value: "high", Label: "High" }] }] } }));
    await f.page.setViewportSize({ width: 1280, height: 900 });
    await f.page.reload();
    const project_ = f.page.getByRole("button", { name: "项目", exact: true });
    const agent = f.page.getByRole("button", { name: "Agent", exact: true });
    const model = f.page.getByRole("button", { name: "模型", exact: true });
    const queue = f.page.getByRole("button", { name: "排队", exact: true }).first();
    const send = f.page.locator('button[aria-label="发送"]');
    await model.waitFor();
    const box = async (locator) => await locator.boundingBox();
    const [left, right, tail, button] = [await box(agent), await box(model), await box(queue), await box(send)];
    assert.ok(right.x - (left.x + left.width) > 80, `The run controls must sit apart from the message controls: ${JSON.stringify({ left, right })}`);
    assert.ok(button.x - (tail.x + tail.width) < 24, `The last run control must meet the send button: ${JSON.stringify({ tail, button })}`);
    assert.ok((await box(project_)).x < right.x, "The project stays on the left of the row");
    await f.page.setViewportSize({ width: 520, height: 900 });
    await eventually(async () => {
        const [near, far] = [await box(agent), await box(model)];
        return far.y > near.y + 4 || far.x - (near.x + near.width) < 40;
    }, "A composer too narrow for two groups must close the gap instead of stranding controls");
    await noHorizontalOverflow(f.page);
};

checks["fleet-live-activity"] = async (f) => {
    // The activity column follows streamed progress, ahead of the 10 s
    // snapshot floor, and without re-reading /state per chunk.
    const state = { ...usageState(), tasks: [task("11", A, "scratch")], agents: [{ id: "test-agent", harness: "mock", eligible: true, busy: 1, activities: [] }] };
    let stateReads = 0;
    await f.page.route("**/state", (route) => { stateReads++; return route.fulfill({ json: f.snapshot = workState(state) }); });
    await f.page.goto(`${app.url}/#/fleet?tab=agents`); await f.page.reload();
    await f.page.getByText("test-agent", { exact: true }).first().waitFor();
    await f.page.getByText("空闲", { exact: true }).first().waitFor();
    const reads = stateReads;
    await f.emit({ kind: "console.progress", task_id: "11", progress: { agent: "test-agent", tools: [{ id: "t1", kind: "shell", name: "go test ./...", status: "running" }] } });
    await f.page.getByText(/#11 .*shell/).first().waitFor();
    await f.emit({ kind: "console.progress", task_id: "11", progress: { agent: "test-agent", tools: [{ id: "t1", kind: "shell", name: "go test ./...", status: "completed" }, { id: "t2", kind: "read", name: "Read README", status: "running" }] } });
    await f.page.getByText(/#11 .*read/).first().waitFor();
    assert.equal(stateReads, reads, "Streamed progress must update the activity column without re-reading /state");
};

checks["fleet-display-name"] = async (f) => {
    // A machine's identity is its immutable node ID; people see the
    // coordination display name first and rename it from the drawer
    // without touching the ID. Machines without a display name show the ID alone.
    const nodes = [
        { name: "node-4bbf207fa8525645ba6935bd07d227a7", display_name: "Steve's MacBook", role: "hub", up: true, version: "test", capabilities: ["gpu", "office"], harnesses: [] },
        { name: "node-77aa11bb22cc33dd44ee55ff66aa77bb", role: "node", up: true, version: "test", harnesses: [] },
    ];
    const state = { ...usageState(), hub: { node: nodes[0].name, version: "test", started: at }, nodes };
    const view = { enabled: true, cluster_id: "cluster-one", node_id: nodes[0].name, coordinator_id: nodes[0].name, epoch: 1, revision: 4, authoritative: true, observed_at: at, auto_failover: false, ready: true, nodes: [{ id: nodes[0].name, name: "Steve's MacBook", local: true, online: true, voter: true, auto_eligible: true, ready: true }], events: [] };
    const renames = [];
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState(state) }));
    await f.page.route("**/console/coordination", (route) => route.fulfill({ json: view }));
    await f.page.route("**/console/coordination/name", (route) => {
        const body = route.request().postDataJSON(); renames.push({ method: route.request().method(), body });
        if (renames.length === 1) { view.revision++; return route.fulfill({ status: 409, json: { error: "The coordination revision changed. Review current state before choosing again." } }); }
        view.revision++; view.nodes[0].name = body.name; nodes[0].display_name = body.name;
        return route.fulfill({ json: view });
    });
    await f.page.goto(`${app.url}/#/fleet?tab=machines`); await f.page.reload();
    const machines = f.page.locator("#fleet-machines");
    await machines.getByText("Steve's MacBook", { exact: true }).waitFor();
    await machines.getByText(nodes[0].name, { exact: true }).waitFor();
    await machines.getByText(nodes[1].name, { exact: true }).waitFor();
    await machines.getByRole("row", { name: /Steve's MacBook/ }).click();
    const drawer = f.page.getByRole("dialog");
    await drawer.getByText("gpu", { exact: true }).waitFor();
    await drawer.getByText("office", { exact: true }).waitFor();
    await f.page.screenshot({ path: path.join(output, "fleet-display-name.png"), fullPage: true });
    await drawer.getByRole("button", { name: "重命名", exact: true }).click();
    const input = drawer.getByLabel("显示名称");
    await input.fill("  Build box  ");
    await drawer.getByRole("button", { name: "保存名称", exact: true }).click();
    await drawer.getByRole("alert").waitFor();
    assert.equal(renames.length, 1);
    assert.equal(renames[0].method, "PUT");
    assert.equal(renames[0].body.node_id, nodes[0].name);
    assert.equal(renames[0].body.expected_revision, 4);
    assert.equal(renames[0].body.name, "Build box", "The name is trimmed before it is sent");
    await drawer.getByRole("button", { name: "保存名称", exact: true }).click();
    await eventually(() => renames.length === 2, "A retry after a conflict re-reads the revision and sends again");
    assert.equal(renames[1].body.expected_revision, 5, "The retry re-reads the revision instead of reusing the stale one");
    await f.emit({ kind: "node.changed" }); await f.page.clock.runFor(350);
    await machines.getByText("Build box", { exact: true }).waitFor();
    await machines.getByText(nodes[0].name, { exact: true }).waitFor();
    assert.equal(await drawer.getByLabel("显示名称").count(), 0, "The form closes once the rename lands");
};

checks["audit-space-by-machine"] = async (f) => {
    // Steve's own footprint is part of the audit: a machine says how much
    // its workspace holds, one that has not finished its first walk says
    // so, and one that reported nothing is not passed off as zero.
    const nodes = [
        { name: "node-4bbf207fa8525645ba6935bd07d227a7", display_name: "Steve's MacBook", role: "hub", up: true, version: "test", harnesses: [], health: { disk_free: 42 * (1 << 30), disk_total: 500 * (1 << 30), load1: 1.2, worktrees: 2, at, root: "/Users/steve/steve", state_root: "/Users/steve/Library/Application Support/Steve", workspace_bytes: 3.5 * (1 << 30), state_bytes: 180 * (1 << 20), space_at: at } },
        { name: "node-77aa11bb22cc33dd44ee55ff66aa77bb", role: "node", up: true, version: "test", harnesses: [], health: { disk_free: 9 * (1 << 30), disk_total: 100 * (1 << 30), load1: 0.4, worktrees: 0, at, root: "/srv/steve", workspace_bytes: 512 * (1 << 20), space_at: at, space_partial: true } },
        { name: "node-99cc88dd77ee66ff55aa44bb33cc22dd", role: "node", up: true, version: "test", harnesses: [], health: { disk_free: 5 * (1 << 30), disk_total: 50 * (1 << 30), load1: 0, worktrees: 0, at, root: "/opt/steve" } },
        { name: "node-11223344556677889900aabbccddeeff", role: "node", up: false, version: "test", harnesses: [] },
    ];
    await f.page.route("**/state", (route) => route.fulfill({ json: { ...usageState(), hub: { node: nodes[0].name, version: "test", started: at }, nodes } }));
    await f.page.goto(`${app.url}/#/dashboard?tab=audit`);
    await f.page.reload();
    const table = f.page.getByRole("grid", { name: "各节点占用", exact: true });
    await table.waitFor();
    const row = (name) => table.getByRole("row").filter({ hasText: name });
    await row("Steve's MacBook").getByText("3.5 GB", { exact: true }).waitFor();
    await row("Steve's MacBook").getByText("180 MB", { exact: true }).waitFor();
    await row("Steve's MacBook").getByText("42 GB / 共 500 GB", { exact: true }).waitFor();
    await row("Steve's MacBook").getByText("/Users/steve/steve", { exact: true }).waitFor();
    // The installation number is large enough to surprise, so the row says
    // which directory it was counted under.
    assert.equal(await row("Steve's MacBook").getByTitle("/Users/steve/Library/Application Support/Steve").count(), 1, "The state figure names the directory it came from");
    assert.equal(await row(nodes[1].name.slice(0, 12)).getByTitle("目录很大，测量到预算上限即停，实际占用不低于该值").count(), 1, "A walk that stopped at its budget is marked as a floor");
    await row(nodes[2].name.slice(0, 12)).getByText("首次测量中…", { exact: true }).waitFor();
    await row(nodes[3].name.slice(0, 12)).getByText("该机器未上报占用", { exact: true }).waitFor();
    await f.page.setViewportSize({ width: 390, height: 844 });
    await noHorizontalOverflow(f.page);
    assert.equal(f.calls.length, 0);
};

// Forty replicas is a wall of rows the reader has to scroll past to
// reach anything under it. The audit lists are read a page at a time,
// the page size is the reader's and is remembered, and a list short
// enough to read whole is left alone.
checks["audit-table-paging"] = async (f) => {
    const replicas = Array.from({ length: 40 }, (_, i) => ({ artifact: `artifact-${String(i + 1).padStart(2, "0")}`, node: "node-4bbf207fa8525645ba6935bd07d227a7", generation: 1, state: "verified", at }));
    const nodes = [{ name: "node-4bbf207fa8525645ba6935bd07d227a7", display_name: "Steve's MacBook", role: "hub", up: true, version: "test", harnesses: [] }];
    const facts = { reservations: [], attestations: [], replicas, disclosures: [], effects: [], grants: [] };
    await f.page.route("**/state", (route) => route.fulfill({ json: { ...usageState(), hub: { node: nodes[0].name, version: "test", started: at }, nodes, facts } }));
    await f.page.goto(`${app.url}/#/dashboard?tab=audit`);
    await f.page.reload();
    const table = f.page.getByRole("grid", { name: "副本", exact: true });
    await table.waitFor();
    const rows = () => table.getByRole("row").filter({ hasText: "artifact-" }).count();
    const card = f.page.locator(".workbench-table").filter({ has: table });
    const pager = card.locator(".table-pager");
    await pager.getByText("第 1–20 条，共 40 条", { exact: true }).waitFor();
    assert.equal(await rows(), 20, "A long list opens twenty rows deep");
    await pager.getByText("第 1 / 2 页", { exact: true }).waitFor();
    assert.equal(await table.getByText("artifact-40", { exact: true }).count(), 0);

    const next = pager.getByRole("button", { name: "下一页", exact: true });
    const previous = pager.getByRole("button", { name: "上一页", exact: true });
    assert.equal(await previous.isDisabled(), true, "There is nothing before the first page");
    await next.click();
    await pager.getByText("第 21–40 条，共 40 条", { exact: true }).waitFor();
    await table.getByText("artifact-40", { exact: true }).waitFor();
    assert.equal(await next.isDisabled(), true, "There is nothing after the last page");
    await previous.click();
    await pager.getByText("第 1–20 条，共 40 条", { exact: true }).waitFor();

    // The page size is a reading preference, not a property of one table.
    await pager.getByRole("button", { name: /每页/ }).click();
    await f.page.getByRole("option", { name: "每页 50 条", exact: true }).click();
    await pager.getByText("第 1–40 条，共 40 条", { exact: true }).waitFor();
    assert.equal(await rows(), 40, "Choosing fifty rows shows the whole list");
    assert.equal(await f.page.evaluate(() => localStorage.getItem("steve.table.pageSize")), "50");
    await f.page.reload();
    await f.page.locator(".table-pager").first().getByText("第 1–40 条，共 40 条", { exact: true }).waitFor();

    // One machine is not a wall, so its table keeps its rows and no footer.
    const machines = f.page.locator(".workbench-table").filter({ has: f.page.getByRole("grid", { name: "各节点占用", exact: true }) });
    await machines.waitFor();
    assert.equal(await machines.locator(".table-pager").count(), 0, "A list short enough to read whole needs no pager");
    assert.equal(f.calls.length, 0, "Paging a table must not submit work");
};

checks["usage-dashboard-ranges"] = async (f) => {
    const usage = usageFixture();
    await f.page.route("**/state", (route) => route.fulfill({ json: usageState() }));
    await f.page.route("**/usage", (route) => route.fulfill({ json: usageResponse(usage) }));
    await f.page.goto(`${app.url}/#/dashboard?tab=overview`);
    await f.page.reload();
    await f.page.getByRole("heading", { name: "用量概览", exact: true }).waitFor();
    const cards = f.page.getByRole("region", { name: "区间用量汇总", exact: true });
    for (const range of ["7d", "1d", "30d"]) {
        await f.page.getByRole("button", { name: range, exact: true }).click();
        await eventually(async () => await f.page.getByRole("button", { name: range, exact: true }).getAttribute("aria-pressed") === "true", `Range ${range} should become active`);
        await f.page.getByRole("grid", { name: "按 Agent", exact: true }).getByText(`agent-${range}`, { exact: true }).waitFor();
        assert.ok((await cards.innerText()).includes(String(usage.periods[range].total.attempts)), "Range metrics must use that period's totals");
        assert.equal(await f.page.locator('a[href="#/dashboard"]').getAttribute("aria-current"), "page");
        await f.page.locator(".recharts-line-curve").waitFor();
    }
    await f.page.getByRole("button", { name: "执行次数", exact: true }).click();
    await f.page.getByRole("group", { name: "执行次数趋势图", exact: true }).waitFor();
    await f.page.getByRole("button", { name: "耗时", exact: true }).click();
    await f.page.getByRole("group", { name: "耗时趋势图", exact: true }).waitFor();
    await f.page.locator("summary").filter({ hasText: "查看分时数据" }).click();
    assert.equal(await f.page.getByRole("table", { name: "分时用量数据", exact: true }).locator("tbody tr").count(), 30);
    await f.page.reload();
    await f.page.getByRole("button", { name: "30d", exact: true }).waitFor();
    assert.equal(await f.page.getByRole("button", { name: "30d", exact: true }).getAttribute("aria-pressed"), "true", "Selected range survives refresh");
    await f.page.setViewportSize({ width: 390, height: 844 });
    await noHorizontalOverflow(f.page);
    assert.equal(f.calls.length, 0, "Changing usage range must never submit work");
};

checks["usage-dashboard-unreported"] = async (f) => {
    const usage = usageFixture();
    const period = usage.periods["1d"];
    period.series = [{ key: period.from, tokens: { context: 65000 }, seconds: 120, attempts: 2, unreported: 2 }];
    period.total = { ...period.series[0], key: "total" }; period.by_agent = []; period.by_model = [];
    await f.page.route("**/state", (route) => route.fulfill({ json: usageState() }));
    await f.page.route("**/usage", (route) => route.fulfill({ json: usageResponse(usage) }));
    await f.page.goto(`${app.url}/#/dashboard?tab=overview&range=1d`); await f.page.reload();
    await f.page.getByText("未上报 Token 按 0 绘制。", { exact: false }).waitFor();
    assert.equal(await f.page.getByRole("region", { name: "区间用量汇总", exact: true }).getByText("0", { exact: true }).count(), 1, "Unreported token usage is rendered as zero");
    await f.page.getByRole("button", { name: "执行次数", exact: true }).click();
    assert.equal(await f.page.getByText("此时段未上报 token，可切换查看执行次数或耗时", { exact: true }).count(), 0, "Execution data remains available without token reports");
    await f.page.locator(".recharts-line-dot").first().waitFor();
};

checks["usage-dashboard-gaps"] = async (f) => {
    const usage = usageFixture();
    const period = usage.periods["1d"];
    period.series = [
        { key: "2026-11-01T01:00:00-04:00", tokens: {}, seconds: 120, attempts: 1, unreported: 1 },
        { key: "2026-11-01T01:00:00-05:00", tokens: { input: 100, output: 20, total: 120 }, seconds: 120, attempts: 1 },
        { key: "2026-11-01T02:00:00-05:00", tokens: {}, seconds: 120, attempts: 1, unreported: 1 },
    ];
    period.total = { key: "total", tokens: { input: 100, output: 20, total: 120 }, seconds: 360, attempts: 3, unreported: 2 };
    period.by_agent = []; period.by_model = [];
    await f.page.route("**/state", (route) => route.fulfill({ json: usageState() }));
    await f.page.route("**/usage", (route) => route.fulfill({ json: usageResponse(usage) }));
    await f.page.goto(`${app.url}/#/dashboard?tab=overview&range=1d`); await f.page.reload();
    await f.page.locator(".recharts-line-dot").first().waitFor();
    assert.equal(await f.page.locator(".recharts-line-dot").count(), 3, "Unreported buckets must be zero-valued points on the continuous line");
    await f.page.locator("summary").filter({ hasText: "查看分时数据" }).click();
    const detail = f.page.getByRole("table", { name: "分时用量数据", exact: true });
    assert.equal(await detail.locator("tbody tr").count(), 3);
    assert.notEqual(await detail.locator("tbody tr").nth(0).locator("th").innerText(), await detail.locator("tbody tr").nth(1).locator("th").innerText(), "Distinct instants remain distinguishable across DST");
    await f.page.locator(".recharts-surface").press("ArrowRight");
    assert.notEqual(await f.page.locator(".recharts-surface").evaluate((el) => getComputedStyle(el).outlineStyle), "none", "Keyboard chart navigation needs visible focus");
};

checks["usage-dashboard-dimensions"] = async (f) => {
    const usage = usageFixture();
    await f.page.route("**/state", (route) => route.fulfill({ json: usageState() }));
    await f.page.route("**/usage", (route) => route.fulfill({ json: usageResponse(usage) }));
    await f.page.goto(`${app.url}/#/dashboard?tab=overview&range=7d`); await f.page.reload();
    await f.page.getByRole("heading", { name: "任务平均耗时", exact: true }).waitFor();
    for (const [name, row] of [["按模型", "Demo Model"], ["按 Harness", "codex-acp"], ["按触发来源", "定时任务"], ["按项目", "scratch"]]) {
        await f.page.getByRole("button", { name, exact: true }).click();
        await f.page.getByRole("grid", { name, exact: true }).getByText(row, { exact: true }).waitFor();
    }
    await f.page.getByRole("button", { name: "按 Harness", exact: true }).click();
    await f.page.getByLabel("排序依据").selectOption("cache");
    const table = f.page.getByRole("grid", { name: "按 Harness", exact: true });
    await table.getByText("缓存读取", { exact: true }).waitFor();
    await table.getByText("最短 / 最长", { exact: true }).waitFor();
    await f.page.getByRole("group", { name: "趋势指标", exact: true }).getByRole("button", { name: "TPM", exact: true }).click();
    await f.page.getByRole("group", { name: "TPM趋势图", exact: true }).waitFor();
    await f.page.getByRole("group", { name: "趋势指标", exact: true }).getByRole("button", { name: "Tokens", exact: true }).click();
    await f.page.getByRole("group", { name: "Token 用量", exact: true }).getByRole("button", { name: "缓存读取", exact: true }).click();
    assert.equal(await f.page.locator('.recharts-line-curve').count(), 2, "I/O/cache series can be compared without replacing the metric");
    await f.page.locator("summary").filter({ hasText: "查看任务明细" }).click();
    await f.page.getByRole("table", { name: "任务消耗明细", exact: true }).getByText(/Synthetic scheduled task/).waitFor();
    assert.equal(f.calls.length, 0, "Analysis controls never submit or mutate work");
};

checks["usage-dashboard-duration-coverage"] = async (f) => {
    let usage = usageDurationFixture("missing");
    await f.page.route("**/state", (route) => route.fulfill({ json: usageState() }));
    await f.page.route("**/usage", (route) => route.fulfill({ json: usageResponse(usage) }));
    await f.page.goto(`${app.url}/#/dashboard?tab=overview&range=1d`); await f.page.reload();
    const cardValue = (label) => f.page.locator(".usage-metric").filter({ has: f.page.getByRole("heading", { name: label, exact: true }) }).locator("strong");
    await f.page.getByRole("heading", { name: "任务平均耗时", exact: true }).waitFor();
    assert.equal(await cardValue("任务平均耗时").innerText(), "—", "Missing duration must not appear as a measured zero");
    assert.equal(await cardValue("TPM").innerText(), "不可估算", "Tokens without a positive duration cannot produce TPM");
    await f.page.getByText("TPM 已覆盖 0 Token；100 Token 缺少有效时长。", { exact: true }).waitFor();
    await f.page.locator("summary").filter({ hasText: "查看任务明细" }).click();
    const taskTable = f.page.getByRole("table", { name: "任务消耗明细", exact: true });
    assert.equal(await taskTable.locator("tbody tr").first().locator("td").last().innerText(), "—");
    await f.page.getByRole("button", { name: "耗时", exact: true }).click();
    assert.equal(await f.page.locator(".recharts-line-dot").count(), 0, "Unknown durations are gaps rather than zero-valued points");
    await f.page.getByRole("group", { name: "趋势指标", exact: true }).getByRole("button", { name: "TPM", exact: true }).click();
    assert.equal(await f.page.locator(".recharts-line-dot").count(), 0, "Unmeasurable throughput must not produce a flat zero line");
    await f.page.getByRole("heading", { name: "用量概览", exact: true }).scrollIntoViewIfNeeded();
    await f.page.screenshot({ path: path.join(output, "usage-duration-missing.png"), fullPage: true });

    usage = usageDurationFixture("zero"); await f.page.reload();
    await f.page.getByText("有效耗时覆盖 1/1 个任务", { exact: true }).waitFor();
    assert.equal(await cardValue("任务平均耗时").innerText(), "0 秒", "A measured zero-duration task remains zero");
    assert.equal(await cardValue("TPM").innerText(), "不可估算");
    await f.page.locator("summary").filter({ hasText: "查看任务明细" }).click();
    assert.equal(await taskTable.locator("tbody tr").first().locator("td").last().innerText(), "0 秒");

    usage = usageDurationFixture("partial"); await f.page.reload();
    await f.page.getByText("有效耗时覆盖 1/2 个任务", { exact: true }).waitFor();
    await f.page.getByText("TPM 已覆盖 200 Token；100 Token 缺少有效时长。", { exact: true }).waitFor();
    assert.equal(await cardValue("任务平均耗时").innerText(), "1 分 0 秒", "Missing durations must not dilute the measured average");
    assert.equal(await cardValue("TPM").innerText(), "200");
    await f.page.locator("summary").filter({ hasText: "查看任务明细" }).click();
    const unknown = taskTable.locator("tbody tr").filter({ hasText: "#unknown" });
    assert.equal(await unknown.locator("td").last().innerText(), "—");
    await f.page.getByRole("heading", { name: "用量概览", exact: true }).scrollIntoViewIfNeeded();
    await f.page.screenshot({ path: path.join(output, "usage-duration-partial.png"), fullPage: true });
    await f.page.evaluate(() => {
        localStorage.setItem("steve.ui.locale", "en");
        window.dispatchEvent(new StorageEvent("storage", { key: "steve.ui.locale", newValue: "en", storageArea: localStorage }));
    });
    await f.page.getByText("TPM covers 200 tokens; 100 tokens have no valid duration.", { exact: true }).waitFor();
    await f.page.getByText("Duration measured for 1/2 tasks", { exact: true }).waitFor();

    usage = usageDurationFixture("unreported"); await f.page.reload();
    await f.page.getByText("有效耗时覆盖 1/1 个任务", { exact: true }).waitFor();
    assert.equal(await cardValue("TPM").innerText(), "0", "A valid duration with unreported tokens follows the explicit zero-token policy");
    assert.equal(await cardValue("任务平均耗时").innerText(), "1 分 0 秒");
    usage = usageDurationFixture("missing"); await f.page.reload();
    await f.page.evaluate(() => {
        localStorage.setItem("steve.ui.locale", "en");
        window.dispatchEvent(new StorageEvent("storage", { key: "steve.ui.locale", newValue: "en", storageArea: localStorage }));
    });
    await f.page.getByText("TPM covers 0 tokens; 100 tokens have no valid duration.", { exact: true }).waitFor();
    assert.equal(await cardValue("TPM").innerText(), "Unavailable");
    await f.page.setViewportSize({ width: 390, height: 844 });
    await noHorizontalOverflow(f.page);
    await f.page.screenshot({ path: path.join(output, "usage-duration-missing-narrow-en.png"), fullPage: true });
    assert.equal(f.calls.length, 0, "Coverage inspection must never submit work");
};

checks["usage-dashboard-task-details-lazy"] = async (f) => {
    const usage = usageFixture();
    const period = usage.periods["7d"];
    const sample = period.by_task[0];
    period.by_task = Array.from({ length: 1000 }, (_, i) => ({ ...sample, key: String(i), task_id: String(i), title: `Task detail ${i}` }));
    period.tasks.count = period.by_task.length;
    await f.page.route("**/state", (route) => route.fulfill({ json: usageState() }));
    await f.page.route("**/usage", (route) => route.fulfill({ json: usageResponse(usage) }));
    await f.page.goto(`${app.url}/#/dashboard?tab=overview&range=7d`); await f.page.reload();
    await f.page.getByRole("heading", { name: "用量概览", exact: true }).waitFor();
    const taskRows = f.page.locator('table[aria-label="任务消耗明细"] tbody tr');
    assert.equal(await taskRows.count(), 0, "Collapsed task details must not build every task row");
    const toggle = f.page.locator("summary").filter({ hasText: "查看任务明细" });
    await toggle.click();
    await eventually(async () => await taskRows.count() === 1000, "Opening the details must show the complete task list");
    await toggle.click();
    await eventually(async () => await taskRows.count() === 0, "Closing the details must release task row DOM");
    assert.equal(f.calls.length, 0);
};

checks["empty-process"] = async (f) => {
    const processes = [
        { timeline: [{ kind: "text", text: "Direct answer", at }] },
        { timeline: [{ kind: "thought", text: " \n ", at }, { kind: "tool", tool: "missing", at }, { kind: "text", text: "Direct answer", at }] },
        { timeline: [{ kind: "text", text: "Direct answer", at }, { kind: "text", text: " \n", at }], steps: [{ id: "empty-step", timeline: [{ kind: "thought", text: " ", at }] }] },
        { reasoning: " \n " },
    ];
    f.replies[A] = processes.map((process, i) => ({ id: `empty-${i}`, kind: "reply", conversation: A, at, text: "Direct answer", process }));
    await f.page.reload();
    await f.page.getByText("Direct answer", { exact: true }).first().waitFor();
    assert.equal(await f.page.locator(".message-assistant summary").count(), 0, "A final answer, blank trace, or missing tool must not create an empty disclosure");
    assert.equal(await f.page.getByText("Direct answer", { exact: true }).count(), processes.length, "Each final answer remains visible once");
    assert.equal(f.calls.length, 0);
};

// A delegated child is a line of the thread, not a header above it: it
// stays readable without opening the trace, it sits where it was handed
// over, and the row says who took it — the target keeps its room even
// when the goal is long, because the goal can be read in the open card.
checks["design-delegation-stream"] = async (f) => {
    const minute = (m, sec = 0) => `2026-09-06T09:${String(m).padStart(2, "0")}:${String(sec).padStart(2, "0")}Z`;
    const goal = "在 BOE 机器上只读盘点该用户的开发工作痕迹，记录仓库名、路径与 remote，并按证据分类给出待确认项。";
    const child = { id: "#41", kind: "delegate", goal, state: "done", since: minute(51), elapsed: "2m10s", agent: "builder", node: "node-7f3c9a", answer: "盘点完成。" };
    f.replies[A] = [
        { id: "sent-1", kind: "sent", conversation: A, at: minute(50), input: "先委派一次盘点。" },
        { id: "reply-1", kind: "reply", conversation: A, at: minute(53), text: "第一轮结果已汇总。", process: { timeline: [{ kind: "thought", text: "先决定交给谁。", at: minute(50, 30) }, { kind: "text", text: "第一轮结果已汇总。", at: minute(53) }], steps: [child] } },
        { id: "sent-2", kind: "sent", conversation: A, at: minute(54), input: "继续下一轮。" },
        { id: "reply-2", kind: "reply", conversation: A, at: minute(58), text: "第二轮结果已汇总。" },
    ];
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState({ at, hub: { node: "test-node", started: at, version: "test" }, nodes: [{ name: "node-7f3c9a", display_name: "工作本", role: "hub", up: true, version: "test" }], agents: [], tasks: [], plans: [], projects: [project("scratch"), project("home")], attempts: [], landings: [] }) }));
    await f.page.reload();
    const recorded = f.page.locator('[data-task-id="#41"]');
    await recorded.waitFor();
    assert.equal(await f.page.locator('details.group\\/process [data-task-id="#41"]').count(), 0, "A child must not be buried in the trace fold");
    assert.equal(await recorded.locator(`span[title="${child.since}"]`).count(), 1, "The row says when the child was handed over");
    const who = recorded.locator('span[title="builder @ 工作本"]');
    assert.ok(await who.evaluate((el) => el.scrollWidth <= el.clientWidth + 1), "A long goal must not clip the target");
    // A child no reply has recorded yet belongs to the turn that started
    // it, between the line that asked and the line that answered.
    await f.emit({ kind: "delegate.progress", task_id: "40", step_id: "#40", step: { kind: "delegate", state: "done", goal: "较早的一次委派。", since: minute(55) }, progress: { agent: "builder", node: "node-7f3c9a" } });
    await f.page.locator('[data-task-id="#40"]').waitFor();
    const placed = await f.page.evaluate(() => {
        const kids = [...document.querySelector(".transcript-messages").children];
        const find = (text) => kids.findIndex((el) => el.textContent.includes(text));
        return { earlier: kids.findIndex((el) => el.dataset.taskId === "#40"), asked: find("继续下一轮。"), answered: find("第二轮结果已汇总。") };
    });
    assert.ok(placed.earlier > placed.asked && placed.earlier < placed.answered, `An unrecorded child belongs where it started, not at the top (${JSON.stringify(placed)})`);
    await f.startRunning();
    await f.page.getByRole("status").first().waitFor();
    await f.emit({ kind: "delegate.progress", task_id: "42", step_id: "#42", step: { kind: "delegate", state: "running", goal: "跑一遍验证。", since: "2026-09-06T10:00:05Z" }, progress: { agent: "checker", node: "node-7f3c9a" } });
    await f.page.locator('[data-task-id="#42"]').waitFor();
    const running = await f.page.evaluate(() => {
        const block = [...document.querySelector(".transcript-messages").children].find((el) => el.querySelector('[role="status"]'));
        const card = block?.querySelector('[data-task-id="#42"]');
        const status = block?.querySelector('[role="status"]');
        return { inside: !!card, below: !!(card && status && (status.compareDocumentPosition(card) & Node.DOCUMENT_POSITION_FOLLOWING)) };
    });
    assert.ok(running.inside && running.below, "A child this turn started stays inside the running line, under it");
    await f.page.screenshot({ path: path.join(output, "design-delegation-stream.png"), fullPage: true });
};

checks["workbench-split"] = async (f) => {
    const minute = (m) => `2026-09-06T09:${String(m).padStart(2, "0")}:00Z`;
    const child = { id: "#41", kind: "delegate", goal: "在 BOE 机器上只读盘点开发痕迹。", state: "done", since: minute(51), elapsed: "2m10s", agent: "builder", node: "node-7f3c9a", answer: "盘点完成，仓库共 3 个。" };
    f.replies[A] = [
        { id: "sent-1", kind: "sent", conversation: A, at: minute(50), input: "先委派一次盘点。" },
        { id: "reply-1", kind: "reply", conversation: A, at: minute(53), text: "第一轮结果已汇总。", process: { timeline: [{ kind: "text", text: "第一轮结果已汇总。", at: minute(53) }], steps: [child] } },
    ];
    await f.page.reload();
    await f.page.locator('[data-task-id="#41"]').waitFor();
    await f.page.getByRole("button", { name: "在分栏中查看委派 #41", exact: true }).click();
    const pane = f.page.locator(".split-pane");
    await pane.waitFor();
    await pane.getByText("盘点完成，仓库共 3 个。").waitFor();
    assert.equal(await pane.getByRole("tab", { name: "委派 #41", exact: true }).getAttribute("aria-selected"), "true", "The delegation opens as the pane's own tab");
    // The strip reads like an editor's page tabs: the tab in front fills
    // the strip's height, carries the panel's own background, and shows
    // what kind of thing it holds.
    const tabLook = await f.page.evaluate(() => {
        const bar = document.querySelector(".split-pane-bar");
        const tab = document.querySelector(".split-tab[data-active]");
        const style = getComputedStyle(tab);
        return {
            gap: bar.getBoundingClientRect().height - tab.getBoundingClientRect().height,
            radius: style.borderTopLeftRadius,
            background: style.backgroundColor,
            panel: getComputedStyle(document.querySelector(".split-pane")).backgroundColor,
            icon: !!tab.querySelector(".split-tab-icon"),
            divider: getComputedStyle(tab).borderRightWidth,
        };
    });
    assert.ok(tabLook.gap <= 2, `The tab fills the strip instead of floating in it (${tabLook.gap}px left over)`);
    assert.equal(tabLook.radius, "0px", "A page tab has square shoulders, not a chip's rounding");
    assert.equal(tabLook.background, tabLook.panel, "The tab in front shares the panel's background");
    assert.ok(tabLook.icon, "A tab says what kind of thing it holds");
    assert.notEqual(tabLook.divider, "0px", "Tabs are told apart by a hairline between them");
    await noHorizontalOverflow(f.page);
    // The pane takes its room from the conversation, which keeps enough
    // of its own to read and compose in.
    const room = async () => ({ talk: (await f.page.locator(".conversation-content").boundingBox()).width, pane: (await pane.boundingBox()).width });
    const before = await room();
    assert.ok(before.talk >= 480 && before.pane >= 320, `Both columns start readable (${JSON.stringify(before)})`);
    const handle = pane.getByRole("separator", { name: "拖动调整分栏宽度", exact: true });
    const drag = async (dx) => {
        const box = await handle.boundingBox();
        const x = box.x + box.width / 2, y = box.y + box.height / 2;
        await f.page.mouse.move(x, y); await f.page.mouse.down();
        await f.page.mouse.move(x + dx, y, { steps: 10 }); await f.page.mouse.up();
    };
    await drag(-120);
    const widened = await room();
    assert.ok(Math.abs(widened.pane - before.pane - 120) <= 2, "Dragging the divider left widens the pane");
    assert.ok(Math.abs(widened.talk - before.talk + 120) <= 2, "What the pane gains the conversation gives");
    await drag(-2000);
    assert.ok((await room()).talk >= 480, "The divider stops where the conversation would stop being usable");
    const summary = f.page.locator('[data-task-id="#41"] > summary').first();
    assert.ok(await summary.evaluate((el) => el.scrollWidth <= el.clientWidth + 1), "A delegation row must stay whole in the column the pane leaves behind");
    await drag(2000);
    assert.ok((await pane.boundingBox()).width >= 320, "The pane cannot be squeezed below a readable width");
    await handle.dblclick();
    // The details column is independent now: both may be open at once.
    await f.page.getByRole("button", { name: "显示详情", exact: true }).click();
    await inspector(f.page).waitFor();
    await pane.waitFor();
    await noHorizontalOverflow(f.page);
    assert.ok((await f.page.locator(".conversation-content").boundingBox()).width >= 480, "Three columns still leave the conversation its minimum");
    await f.page.screenshot({ path: path.join(output, "workbench-split.png"), fullPage: true });
    // Hiding keeps the tab; closing it gives the room back for good.
    await f.page.getByRole("button", { name: "收起分栏", exact: true }).first().click();
    await pane.waitFor({ state: "detached" });
    await f.page.getByRole("button", { name: "展开分栏（1）", exact: true }).click();
    await pane.getByText("盘点完成，仓库共 3 个。").waitFor();
    await pane.getByRole("button", { name: "关闭委派 #41", exact: true }).click();
    await pane.waitFor({ state: "detached" });
    assert.equal(await f.page.locator(".console-toolbar").getByRole("button", { name: /分栏/ }).count(), 0, "With nothing left to show, the pane leaves no controls behind");
};

checks["process-content"] = async (f) => {
    const process = { agent: "test-agent", node: "node-7f3c9a", model: "GPT-5.6-Sol", tools: [{ id: "read", title: "Read file", status: "completed", output: "file content" }], timeline: [{ kind: "thought", text: "Checking the file", at }, { kind: "text", text: "Opening the file", at }, { kind: "tool", tool: "read", at }, { kind: "text", text: "Checked answer", at }], steps: [{ id: "empty-step", timeline: [{ kind: "thought", text: " ", at }] }] };
    f.replies[A] = [{ id: "trace", kind: "reply", conversation: A, at, text: "Checked answer", process }];
    // People know a machine by the name they gave it. Every attribution
    // shows that name; the node ID is only the tooltip.
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState({ at, hub: { node: "test-node", started: at, version: "test" }, nodes: [{ name: "node-7f3c9a", display_name: "工作本", role: "hub", up: true, version: "test" }], agents: [], tasks: [], plans: [], projects: [project("scratch"), project("home")], attempts: [], landings: [] }) }));
    await f.page.reload();
    const message = f.page.locator(".message-assistant");
    await message.getByText("Checked answer", { exact: true }).waitFor();
    const toggle = message.locator("summary").filter({ hasText: /^过程/ });
    await toggle.press("Enter");
    await message.getByText("Checking the file", { exact: true }).waitFor();
    await message.getByText("Opening the file", { exact: true }).waitFor();
    assert.equal(await message.locator('[data-span-kind="tool"]').count(), 1, "Actual tool activity remains inspectable");
    // The activity line already says what the group did; the rows under it
    // must not repeat the same sentence in a second fold.
    assert.equal(await message.getByText("调用了 1 次工具", { exact: true }).count(), 1, "A tool activity states what it did once, not once per nested fold");
    assert.equal(await message.getByText("Checked answer", { exact: true }).count(), 1, "The final answer is not repeated inside the trace");
    assert.equal(await message.getByText("empty-step", { exact: true }).count(), 0, "Empty plan steps must not leave a heading");
    const signature = message.getByText("test-agent @ 工作本 · GPT-5.6-Sol", { exact: true });
    await signature.waitFor();
    assert.equal(await signature.getAttribute("title"), "node-7f3c9a", "The machine's ID stays available as the tooltip");
    await toggle.press("Enter");
    await f.startRunning();
    await f.page.getByText("正在准备执行…", { exact: true }).waitFor();
    await f.emit({ kind: "console.progress", exchange_id: "design-running", progress: { agent: "test-agent", timeline: [{ kind: "thought", text: " ", at }], answer: "Streaming answer" } });
    await f.page.getByText("test-agent", { exact: true }).waitFor();
    await f.page.getByText("Streaming answer", { exact: true }).waitFor();
    assert.equal(await f.page.locator('summary').filter({ hasText: /^过程/ }).count(), 1, "Empty live progress retains its status without a disclosure");
    assert.equal(f.calls.length, 0);
};

checks["board-overview"] = async (f) => {
    const root = (id, lifecycle, lane) => ({ ...task(id, A, "scratch"), state: lifecycle, lifecycle, lane, execution: "idle" });
    const rows = [
        { ...root("11", "running", "needs_you"), pending_results: 2, uncertain_results: 1 },
        root("12", "done", "ended"), root("13", "cancelled", "ended"), root("14", "paused", "set_aside"),
        { ...root("15", "done", "ended"), archived_at: at },
        { ...root("21", "done", "ended"), parent: "11", origin: "delegate:11", result_delivery: { state: "pending", attempts: 2, error: "Temporary delivery failure", next_attempt_at: "2026-09-06T10:00:10Z", at } },
        { ...root("22", "done", "needs_you"), parent: "11", origin: "delegate:11", result_delivery: { state: "uncertain", attempts: 1, error: "Receipt lost: " + "long-unbroken-detail".repeat(20), at } },
        { ...root("16", "paused", "set_aside"), parent: "15" },
    ];
    const state = { ...usageState(), tasks: rows, projects: [project("scratch")] };
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState(state) }));
    await f.page.goto(`${app.url}/#/console?view=board`); await f.page.reload();
    const summary = f.page.getByRole("region", { name: "主任务统计" });
    await summary.getByText("主任务", { exact: true }).waitFor();
    assert.match(await summary.innerText(), /主任务\s+5/);
    // Owner summary counts all roots, including archived roots. A paused
    // child of an archived root is not a new root when that root is hidden.
    assert.match(await summary.innerText(), /已完成\s+2/);
    assert.match(await summary.innerText(), /已取消\s+1/);
    assert.match(await summary.innerText(), /已暂停\s+1/);
    await f.page.getByText("2 个结果待交接 · 1 个交接待确认", { exact: true }).waitFor();
    await f.page.getByRole("switch", { name: "显示已归档" }).press("Space");
    assert.match(await summary.innerText(), /主任务\s+5/);
    assert.match(await summary.innerText(), /已完成\s+2/);
    assert.match(await summary.innerText(), /已暂停\s+1/);
    await f.page.screenshot({ path: path.join(output, "board-overview-wide.png"), fullPage: true });
    const open = f.page.getByRole("button", { name: "打开任务 #11 Task 11", exact: true });
    await open.focus(); await open.press("Enter");
    const dialog = f.page.getByRole("dialog");
    await dialog.getByRole("heading", { name: "结果交接", exact: true }).waitFor();
    await dialog.getByText("Temporary delivery failure", { exact: true }).waitFor();
    await dialog.getByText(/下次自动重试：/).waitFor();
    await dialog.getByText(/先核对父任务会话/).waitFor();
    for (const width of [1600, 390]) {
        await f.page.setViewportSize({ width, height: 1000 });
        assert.ok(await dialog.evaluate((el) => el.scrollWidth <= el.clientWidth + 1), "Long handoff details must not overflow");
        await f.page.screenshot({ path: path.join(output, `board-handoffs-${width}.png`), fullPage: true });
    }
    rows[2].state = rows[2].lifecycle = "done";
    rows[0].pending_results = 1; rows[0].uncertain_results = 0; rows[0].lane = "pending";
    rows[6].result_delivery = { state: "delivered", at };
    await f.emit({ kind: "task.changed" }); await f.page.clock.runFor(350);
    await eventually(async () => (await dialog.getByText("已送达", { exact: true }).count()) === 1, "A receipt should update the open drawer");
    await f.page.keyboard.press("Escape");
    await open.waitFor();
    await f.page.screenshot({ path: path.join(output, "board-overview-narrow.png"), fullPage: true });
    await f.page.evaluate(() => {
        localStorage.setItem("steve.ui.locale", "en");
        window.dispatchEvent(new StorageEvent("storage", { key: "steve.ui.locale", newValue: "en", storageArea: localStorage }));
    });
    await f.page.getByRole("region", { name: "Root task summary" }).waitFor();
    assert.equal(f.calls.length, 0, "Reading handoffs must not dispatch work");
};

checks["child-handoff"] = async (f) => {
    const child = { ...task("22", A, "scratch"), state: "done", lifecycle: "done", execution: "idle", lane: "needs_you", parent: "11", result_delivery: { state: "uncertain", error: "Child receipt was lost", attempts: 2, at } };
    const state = { ...usageState(), tasks: [task("11", A, "scratch"), child] };
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState(state) }));
    await f.page.goto(`${app.url}/#/console?view=board&tab=all`); await f.page.reload();
    await f.page.getByRole("row").filter({ hasText: "Task 22" }).click();
    const dialog = f.page.getByRole("dialog");
    await dialog.getByRole("heading", { name: "结果交接", exact: true }).waitFor();
    await dialog.getByText("#22 → #11", { exact: true }).waitFor();
    await dialog.getByText("Child receipt was lost", { exact: true }).waitFor();
    await f.page.screenshot({ path: path.join(output, "child-handoff.png"), fullPage: true });
};

checks["fleet-version-drift"] = async (f) => {
    const nodes = [
        { name: "hub", role: "hub", up: true, version: "abc1234", harnesses: [] },
        { name: "worker-old", role: "node", up: true, version: "def5678", harnesses: [] },
        { name: "worker-unknown", role: "node", up: true, harnesses: [] },
        { name: "worker-offline", role: "node", up: false, version: "old", harnesses: [] },
    ];
    const state = { ...usageState(), hub: { node: "hub", version: "abc1234", started: at }, nodes };
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState(state) }));
    await f.page.goto(`${app.url}/#/fleet?tab=machines`); await f.page.reload();
    await f.page.getByText(/1 台机器与协调节点版本不同/).waitFor();
    assert.equal(await f.page.getByText("版本不同", { exact: true }).count(), 1);
    await f.page.screenshot({ path: path.join(output, "fleet-version-drift.png"), fullPage: true });
    nodes[1].version = "abc1234";
    await f.emit({ kind: "node.changed" }); await f.page.clock.runFor(350);
    await eventually(async () => (await f.page.getByText("版本不同", { exact: true }).count()) === 0, "Drift should clear after a running process updates");
};

// A machine refused for an older node protocol is told apart from one that
// is only offline, and only it is offered the upgrade that brings it back.
// A machine refused for a newer protocol says the coordinator is behind.
checks["fleet-protocol-mismatch"] = async (f) => {
    const nodes = [
        { name: "hub", role: "hub", up: true, version: "abc1234", harnesses: [] },
        { name: "worker-v1", role: "node", addr: "10.0.0.7:7701", up: false, protocol_mismatch: { node: 1, hub_min: 2, hub_max: 2 }, harnesses: [],
            last_error: "nodewire: protocol version mismatch: hub speaks v2–v2, node speaks v1–v1; upgrade steve on that machine to this build (a machine enrolled over SSH can be upgraded from the console)" },
        { name: "worker-v3", role: "node", addr: "10.0.0.8:7701", up: false, protocol_mismatch: { node: 3, hub_min: 2, hub_max: 2 }, harnesses: [],
            last_error: "nodewire: protocol version mismatch: hub speaks v2–v2, node speaks v3–v3; upgrade this hub to a build that speaks v3" },
        { name: "worker-offline", role: "node", addr: "10.0.0.9:7701", up: false, version: "abc1234", last_error: "dial tcp 10.0.0.9:7701: connection refused", harnesses: [] },
    ];
    const state = { ...usageState(), hub: { node: "hub", version: "abc1234", started: at }, nodes };
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState(state) }));
    await f.page.goto(`${app.url}/#/fleet?tab=machines`); await f.page.reload();
    const calls = f.calls.length;
    const banner = f.page.getByRole("status").filter({ hasText: "1 台机器的节点协议比协调节点旧" });
    await banner.waitFor();
    const machines = f.page.locator("#fleet-machines");
    assert.equal(await machines.getByText("协议不兼容", { exact: true }).count(), 2, "Both refused machines are marked, the offline one is not");
    await f.page.screenshot({ path: path.join(output, "fleet-protocol-mismatch.png"), fullPage: true });
    const drawer = async (name) => {
        await machines.getByRole("row", { name: new RegExp(name) }).click();
        const opened = f.page.getByRole("dialog", { name, exact: true }); await opened.waitFor();
        return opened;
    };
    const close = async (opened) => { await opened.getByRole("button", { name: "关闭", exact: true }).click(); await opened.waitFor({ state: "detached" }); };
    let opened = await drawer("worker-v1");
    await opened.getByText(/节点协议是 v1，协调节点接受 v2/).waitFor();
    await opened.getByRole("button", { name: "升级到 abc1234", exact: true }).waitFor();
    await f.page.screenshot({ path: path.join(output, "fleet-protocol-mismatch-drawer.png") });
    await close(opened);
    opened = await drawer("worker-v3");
    await opened.getByText(/把协调节点升级到支持 v3 的版本/).waitFor();
    assert.equal(await opened.getByRole("button", { name: /^升级到/ }).count(), 0, "A machine newer than the coordinator is not offered an upgrade");
    await close(opened);
    opened = await drawer("worker-offline");
    await opened.getByText("dial tcp 10.0.0.9:7701: connection refused", { exact: true }).waitFor();
    assert.equal(await opened.getByRole("button", { name: /^升级到/ }).count(), 0, "An offline machine is not offered an upgrade");
    assert.equal(await opened.getByText(/节点协议/).count(), 0, "An offline machine is not said to be refused for its protocol");
    await close(opened);
    await banner.getByRole("button", { name: "全部升级", exact: true }).click();
    const upgrade = f.page.getByRole("dialog", { name: "升级机器", exact: true });
    await upgrade.getByText("worker-v1", { exact: true }).waitFor();
    for (const other of ["worker-v3", "worker-offline"]) assert.equal(await upgrade.getByText(other, { exact: true }).count(), 0, `${other} is not upgraded from the banner`);
    await upgrade.getByRole("button", { name: "取消", exact: true }).click();
    assert.equal(f.calls.length, calls, "Reading the fleet and opening the upgrade must not start one");
};

async function checkNativeHistoryImport(f, autoProject = false) {
    const node = { name: "test-node", role: "node", up: true, version: "test", features: ["native_history.v1"], harnesses: [] };
    const imported = "console:import:fixture";
    const home = "/original/codex";
    const entries = [
        { native_id: "retained-native-id", harness: "codex", source_home: home, workdir: "/test/scratch", title: "Retained context with a long title and original workspace", updated_at: at, revision: "revision-one" },
        { native_id: "unmapped-native-id", harness: "codex", source_home: home, workdir: "/very/long/original/workspace/without/a/matching/project", title: "Unmapped session", updated_at: at, revision: "revision-two" },
    ];
    const state = { at, hub: { node: "test-node", started: at, version: "test" }, nodes: [node], agents: [{ id: "test-agent", node: "test-node", harness: "codex", eligible: true }], tasks: [], plans: [], projects: [{ ...project("scratch"), workspaces: [{ id: "scratch-work", node: "test-node", path: "/test//scratch/./", kind: "canonical", agents: ["test-agent"] }] }], attempts: [], landings: [] };
    const pendingState = gate();
    await f.page.route("**/state", async (route) => { await pendingState.promise; return route.fulfill({ json: f.snapshot = workState(state) }); });
    const posts = []; let failRead = true, loseReceipt = true;
    await f.page.route("**/console/nodes/test-node/native-history**", async (route) => {
        if (route.request().method() === "GET") {
            if (failRead) { failRead = false; return route.fulfill({ status: 503, json: { error: "Source machine is temporarily unavailable" } }); }
            return route.fulfill({ json: { entries } });
        }
        posts.push(route.request().postDataJSON());
        if (loseReceipt) {
            loseReceipt = false;
            if (autoProject) {
                state.projects.push({ ...project("imported-project"), workspaces: [{ id: "imported-work", node: "test-node", path: entries[1].workdir, kind: "canonical", agents: ["test-agent"] }] });
            }
            return route.abort("connectionreset");
        }
        f.conversations.push({ id: imported, title: "Imported conversation", project: autoProject ? "imported-project" : "scratch", agent: "test-agent" });
        f.replies[imported] = [{ id: "import-notice", conversation: imported, kind: "notice", at, text: "History imported; send your next message to continue." }];
        return route.fulfill({ json: { conversation: imported } });
    });
    await f.page.reload();
    await f.page.getByRole("button", { name: "导入历史会话", exact: true }).click();
    const dialog = f.page.getByRole("dialog", { name: "导入历史会话", exact: true });
    await dialog.getByText("先接入支持会话迁移的机器并登记 Agent。", { exact: true }).waitFor();
    pendingState.release();
    await dialog.getByRole("button", { name: "查找会话", exact: true }).click();
    await dialog.getByRole("alert").getByText("Source machine is temporarily unavailable").waitFor();
    await dialog.getByRole("button", { name: "查找会话", exact: true }).click();
    await dialog.getByRole("textbox", { name: "筛选标题、目录或会话 ID" }).fill("no matching session");
    await dialog.getByRole("textbox", { name: "历史目录（可选）" }).fill(home);
    await dialog.getByRole("button", { name: "查找会话", exact: true }).click();
    assert.equal(await dialog.getByRole("textbox", { name: "筛选标题、目录或会话 ID" }).inputValue(), "");
    await dialog.getByText("Unmapped session", { exact: true }).click();
    await dialog.getByText("导入时会自动登记原工作目录并关联项目。", { exact: true }).waitFor();
    assert.equal(await dialog.getByRole("button", { name: "导入并打开会话", exact: true }).isEnabled(), true, "An unregistered directory must be importable without leaving the dialog");
    if (!autoProject) {
        await dialog.getByText("Retained context with a long title and original workspace", { exact: true }).click();
        await dialog.getByText("将自动关联项目 scratch。", { exact: true }).waitFor();
    }
    await f.page.screenshot({ path: path.join(output, "native-import-wide.png"), fullPage: true });
    await f.page.setViewportSize({ width: 390, height: 844 });
    await f.page.screenshot({ path: path.join(output, "native-import-narrow.png"), fullPage: true });
    assert.ok(await dialog.evaluate((e) => e.scrollWidth <= e.clientWidth + 1), "Narrow import dialog must not overflow");
    await f.page.keyboard.press("Tab");
    assert.ok(await dialog.evaluate((e) => e.contains(document.activeElement)), "Keyboard focus must remain in import dialog");
    await dialog.getByRole("button", { name: "导入并打开会话", exact: true }).click();
    await dialog.getByRole("alert").waitFor();
    assert.equal(posts.length, 1);
    if (autoProject) {
        await f.emit({ kind: "project.changed" }); await f.page.clock.runFor(350);
        await dialog.getByText("将自动关联项目 imported-project。", { exact: true }).waitFor();
    }
    await dialog.getByRole("button", { name: "导入并打开会话", exact: true }).click();
    await f.page.getByText("History imported; send your next message to continue.", { exact: true }).waitFor();
    assert.deepEqual(posts[0], posts[1], "Lost import receipt must retry the same command and selected history");
    assert.equal(posts[1].source.home, home); assert.equal(posts[1].native_id, autoProject ? "unmapped-native-id" : "retained-native-id");
    assert.equal(posts[1].project, "", "The server resolves the destination from the selected history, including retries after registration");
    assert.equal(f.calls.filter((c) => c.path === "/console/send" || (c.path === "/console/queue" && c.method === "POST")).length, 0, "Import must not submit a task prompt");
};

checks["native-history-import"] = (f) => checkNativeHistoryImport(f);
checks["native-history-auto-project"] = (f) => checkNativeHistoryImport(f, true);

// The sidebar reads the same threads along more than one axis: the
// project tree it has always had, and — for "what moved last" or "what
// is that machine busy with" — grouping by machine or Agent, ordering by
// time, attention or name. The arrangement is the reader's and it is
// remembered; arranging never submits anything.
checks["shared-sidebar-icon-actions"] = async (f) => {
    const sidebar = f.page.locator(".conversation-sidebar");
    await sidebar.getByRole("button", { name: "收起会话栏", exact: true }).click();
    await f.page.locator(".conversation-sidebar.is-collapsed").waitFor();
    const expand = sidebar.getByRole("button", { name: "展开会话栏", exact: true });
    const newConversation = sidebar.getByRole("button", { name: "新会话", exact: true });
    for (const button of [expand, newConversation]) {
        const box = await button.boundingBox(), rail = await sidebar.boundingBox();
        assert.equal(box.width, 28, "Dense sidebar actions use the explicit shared sm size");
        assert.equal(box.height, 28);
        assert.ok(box.x >= rail.x && box.x + box.width <= rail.x + rail.width, "Icon actions fit the unchanged narrow rail");
    }
    await expand.focus(); await f.page.keyboard.press("Enter");
    await sidebar.getByRole("button", { name: "收起会话栏", exact: true }).waitFor();
    const disclosure = sidebar.locator(".conversation-project button[aria-expanded]").first();
    const before = await disclosure.getAttribute("aria-expanded");
    await disclosure.focus(); await f.page.keyboard.press("Space");
    assert.equal(await disclosure.getAttribute("aria-expanded"), String(before !== "true"), "Keyboard toggles the group exactly once");
    const arrange = sidebar.getByRole("button", { name: /^排列/ });
    await arrange.focus(); await f.page.keyboard.press("Enter");
    await f.page.getByRole("menuitemradio", { name: "按机器", exact: true }).waitFor();
    await f.page.keyboard.press("Escape");
    await f.page.waitForFunction((element) => element === document.activeElement, await arrange.elementHandle());
    assert.equal(f.calls.length, 0, "Presentation and grouping do not submit work");
};

checks["sessions-arrangement"] = async (f) => {
    const threads = [
        { id: A, title: "Conversation A", project: "scratch", agent: "builder", place: { workspace: "w-a", kind: "canonical", node: "node-one" }, last_at: "2026-09-06T09:00:00Z", count: 1, running: false, questions: 2 },
        { id: B, title: "Conversation B", project: "home", agent: "scout", place: { workspace: "w-b", kind: "copy", node: "node-two" }, last_at: "2026-09-06T11:00:00Z", count: 1, running: false },
        { id: "console:interaction-c", title: "Conversation C", project: "scratch", agent: "builder", place: { workspace: "w-c", kind: "copy", node: "node-one" }, last_at: "2026-09-06T10:00:00Z", count: 1, running: false },
    ];
    const nodes = [{ name: "node-one", display_name: "树莓派" }, { name: "node-two", display_name: "工作站" }];
    await f.page.route("**/console/conversations", (route) => route.fulfill({ json: { enabled: true, conversations: threads } }));
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState({ at, hub: { node: "node-one" }, nodes, agents: [], tasks: [], plans: [], projects: [project("scratch"), project("home")], attempts: [], landings: [] }) }));
    await f.page.reload();
    const sidebar = f.page.locator(".conversation-sidebar");
    const titles = () => sidebar.locator(".conversation-row .u-title").allInnerTexts();
    const heading = () => sidebar.locator(".conversation-section-label").first().innerText();
    const arrange = sidebar.getByRole("button", { name: /^排列/ });
    await arrange.waitFor();
    assert.match(await heading(), /项目/, "The project tree stays the arrangement a fresh reader gets");
    assert.equal(await sidebar.getByRole("button", { name: "在 scratch 下新会话", exact: true }).count(), 1);

    // Grouping and order are two choices in one visit, so the menu holds open.
    await arrange.click();
    await f.page.getByRole("menuitemradio", { name: "按机器", exact: true }).click();
    assert.equal(await f.page.getByRole("menuitemradio", { name: "名称", exact: true }).count(), 1, "The menu holds open so both choices can be made at once");
    await f.page.keyboard.press("Escape");
    assert.match(await heading(), /按机器/);
    await sidebar.getByText("树莓派", { exact: true }).waitFor();
    await sidebar.getByText("工作站", { exact: true }).waitFor();
    assert.deepEqual(await titles(), ["Conversation B", "Conversation C", "Conversation A"], "The machine with the freshest thread leads, and inside it the freshest thread");
    await sidebar.getByText("scratch", { exact: true }).first().waitFor();
    await sidebar.getByText("scratch · 主目录", { exact: true }).waitFor();
    assert.equal(await sidebar.getByText(/主目录 · 树莓派/).count(), 0, "Under a machine's own heading the machine is not said twice");

    // Flat and by time is the plain answer to "what moved last".
    await arrange.click();
    await f.page.getByRole("menuitemradio", { name: "不分组", exact: true }).click();
    await f.page.keyboard.press("Escape");
    assert.deepEqual(await titles(), ["Conversation B", "Conversation C", "Conversation A"]);
    assert.equal(await sidebar.getByRole("button", { name: "在 scratch 下新会话", exact: true }).count(), 0);

    // What owes the owner an answer outranks what merely spoke last.
    await arrange.click();
    await f.page.getByRole("menuitemradio", { name: "待处理优先", exact: true }).click();
    await f.page.keyboard.press("Escape");
    assert.deepEqual(await titles(), ["Conversation A", "Conversation B", "Conversation C"]);
    await arrange.click();
    await f.page.getByRole("menuitemradio", { name: "名称", exact: true }).click();
    await f.page.keyboard.press("Escape");
    assert.deepEqual(await titles(), ["Conversation A", "Conversation B", "Conversation C"]);

    // A list that forgets how it was arranged is arranged again every morning.
    await f.page.reload();
    await sidebar.getByRole("button", { name: /^排列/ }).waitFor();
    assert.match(await heading(), /会话/);
    assert.deepEqual(await titles(), ["Conversation A", "Conversation B", "Conversation C"]);
    assert.equal(f.calls.length, 0, "Arranging the list must not submit work");
};

checks["conversation-work-disclosure"] = async (f) => {
    const tasks = [
        { ...task("11", A, "scratch"), execution: "idle" },
        { ...task("12", A, "scratch"), parent: "11", execution: "idle" },
        { ...task("22", B, "home"), execution: "idle", plan_id: "plan" },
    ];
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState({ at, hub: { node: "test-node" }, nodes: [], agents: [], tasks, plans: [], projects: [project("scratch"), project("home")], attempts: [], landings: [] }) }));
    await f.page.addInitScript((id) => localStorage.setItem("steve.work.open." + id, "1"), A);
    await f.page.reload();
    const sidebar = f.page.locator(".conversation-sidebar");
    const toggle = sidebar.getByRole("button", { name: "展开 Conversation A 的任务", exact: true });
    await toggle.waitFor();
    assert.equal(await toggle.getAttribute("aria-expanded"), "false");
    assert.equal(await sidebar.getByText("1 个任务 · 1 次委派", { exact: true }).count(), 0, "Task statistics must not occupy a row by default, even with an old saved expansion");
    await f.box.fill("Retain this draft while expanding work");
    await toggle.focus(); await f.page.keyboard.press("Enter");
    const collapse = sidebar.getByRole("button", { name: "收起 Conversation A 的任务", exact: true });
    assert.equal(await collapse.getAttribute("aria-expanded"), "true");
    await sidebar.getByText("1 个任务 · 1 次委派", { exact: true }).waitFor();
    await sidebar.getByRole("button", { name: /#12.*Task 12/ }).waitFor();
    assert.equal(await sidebar.getByRole("button", { name: "展开 Conversation B 的任务", exact: true }).getAttribute("aria-expanded"), "false", "Expanding a thread must not expand its neighbours");
    await collapse.press("Space");
    assert.equal(await sidebar.getByText("1 个任务 · 1 次委派", { exact: true }).count(), 0);
    await f.pick("B"); await f.pick("A");
    assert.equal(await toggle.getAttribute("aria-expanded"), "false", "Picking a conversation must not expand its work");
    assert.equal(await draftOf(f.box), "Retain this draft while expanding work");
    await toggle.click();
    await f.page.reload();
    await toggle.waitFor();
    assert.equal(await toggle.getAttribute("aria-expanded"), "false", "A fresh page starts compact");
    assert.equal(f.calls.length, 0, "Disclosure and selection must not submit work");
};

checks["thread-work-rows-align"] = async (f) => {
    // A thread's work is read down the left edge: the state mark, the
    // number, then who has it. Marks differ in shape and numbers in
    // width, so the columns are fixed — otherwise every other row sits a
    // few pixels off and the list looks broken.
    const other = "node-other";
    const tasks = [
        { ...task("7", A, "scratch"), execution: "idle", lifecycle: "idle" },
        { ...task("41", A, "scratch"), parent: "7", origin: "delegate", member: "gpu-codex", lifecycle: "done", execution: "done" },
        { ...task("9", A, "scratch"), parent: "7", origin: "delegate", member: "gpu-claude", lifecycle: "failed", execution: "idle" },
        { ...task("12", A, "scratch"), parent: "7", origin: "delegate", member: "reviewer", node: other, execution: "running" },
    ];
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState({ at, hub: { node: "test-node" }, nodes: [{ name: other, display_name: "另一台", online: true }], agents: [], tasks, plans: [], projects: [project("scratch"), project("home")], attempts: [], landings: [] }) }));
    await f.page.reload();
    const sidebar = f.page.locator(".conversation-sidebar");
    await sidebar.getByRole("button", { name: "展开 Conversation A 的任务", exact: true }).click();
    const rows = sidebar.locator("button").filter({ hasText: /^#/ });
    await rows.first().waitFor();
    const read = await rows.evaluateAll((list) => list.map((row) => {
        const cells = [...row.children].map((cell) => ({ text: cell.textContent.trim(), x: cell.getBoundingClientRect().x, right: cell.getBoundingClientRect().right }));
        return { indent: cells[0].x, cells };
    }));
    assert.equal(read.length, 4, "Every task and delegation keeps its own row");
    assert.deepEqual(read.map((row) => row.cells[1].text), ["#7", "#41", "#12", "#9"], "Work reads newest first, parents before what they handed on");
    const kids = read.slice(1);
    for (const row of kids) {
        assert.ok(row.indent > read[0].indent, "A delegation is set in from the task that made it");
        assert.equal(row.cells[1].right.toFixed(1), kids[0].cells[1].right.toFixed(1), "Numbers of any width end on one column");
        assert.equal(row.cells[2].x.toFixed(1), kids[0].cells[2].x.toFixed(1), "Names start on one column whatever the state mark is");
        assert.ok(!row.cells[2].text.includes("→"), "The indent already says it was handed on");
    }
    assert.equal(kids[0].cells[2].text, "gpu-codex", "A delegation on its parent's machine does not repeat the machine");
    assert.equal(kids[1].cells[2].text, "reviewer@另一台", "A delegation that moved machines says so by name");
    assert.equal(f.calls.length, 0, "Reading a thread's work must not submit work");
};

checks["sent-line-actions"] = async (f) => {
    // Stopping a turn is an act on the turn: the control and its receipt are
    // kept by the server and drawn by no one. What the person said keeps its
    // time, a copy of it, and a way to say it again.
    f.replies[A] = [
        { id: "asked", kind: "sent", conversation: A, at, input: "问题1：" },
        { id: "stop-control", kind: "sent", conversation: A, at, input: "/cancel", silent: true },
        { id: "stop-receipt", kind: "reply", conversation: A, at, text: "已请求取消 dev 当前任务", silent: true },
        { id: "relayed", kind: "sent", conversation: A, at, input: "⤵ 子任务 #12 完成", relayed: true },
    ];
    await f.context.grantPermissions(["clipboard-read", "clipboard-write"]);
    await f.page.reload();
    const sent = f.page.locator(".message-user").filter({ hasText: "问题1：" });
    await sent.waitFor();
    assert.equal(await f.page.getByText("/cancel", { exact: true }).count(), 0, "A stop must not leave a control line in the transcript");
    assert.equal(await f.page.getByText("已请求取消 dev 当前任务", { exact: true }).count(), 0, "A stop's receipt belongs to the status line, not the transcript");
    assert.match(await sent.locator(".message-user-meta > span").first().innerText(), /\d{1,2}:\d{2}/, "A sent line says when it was sent");
    await sent.getByRole("button", { name: "复制", exact: true }).click();
    assert.equal(await f.page.evaluate(() => navigator.clipboard.readText()), "问题1：", "Copy puts the line on the clipboard");
    const edit = sent.getByRole("button", { name: "改写这条消息，从这里重来", exact: true });
    await edit.click();
    await eventually(async () => await draftOf(f.box) === "问题1：", "Editing a sent line puts it back in the box");
    await f.box.fill("A draft that must not vanish");
    await edit.click();
    await f.page.getByText("替换正在输入的内容？", { exact: true }).waitFor();
    assert.equal(await draftOf(f.box), "A draft that must not vanish", "Nothing is replaced before the answer");
    await f.page.getByRole("button", { name: "改写", exact: true }).click();
    await eventually(async () => await draftOf(f.box) === "问题1：", "Confirming replaces the draft with the edited line");
    assert.equal(f.calls.length, 0, "Editing prepares a message; it does not send one");
    // A line Steve relayed records something that happened; it can be
    // copied, but there is nothing of the owner's in it to rewrite.
    const relayed = f.page.locator(".message-user").filter({ hasText: "⤵ 子任务 #12 完成" });
    await relayed.waitFor();
    assert.equal(await relayed.getByRole("button", { name: "复制", exact: true }).count(), 1, "A relayed line can still be copied");
    assert.equal(await relayed.getByRole("button", { name: "改写这条消息，从这里重来", exact: true }).count(), 0, "A relayed line offers no rewrite");
};

checks["preferences-during-a-turn"] = async (f) => {
    // A turn in flight used to refuse every selector, so the chips could not
    // even be opened while the agent worked — the one moment the owner wants
    // to line up a different model. The choice is taken now, and the page
    // says when it lands.
    await f.page.route("**/console/context?*", (route) => {
        const conversation = new URL(route.request().url()).searchParams.get("conversation");
        return route.fulfill({ json: { enabled: true, context: { conversation, project: { ...project("scratch"), bound: true }, agents: [], agent: { id: "test-agent", node: "test-node", harness: "codex", model: "gpt-6-astra", ready: true, usable: true } } } });
    });
    await f.page.route("**/console/selectors?*", (route) => route.fulfill({
        json: { model: "gpt-6-astra", models: [{ Value: "gpt-6-astra", Label: "Astra" }], options: [{ ID: "reasoning_effort", Name: "Reasoning effort", Category: "thought_level", Current: "medium", Choices: [{ Value: "medium", Label: "Medium" }, { Value: "high", Label: "High" }] }] },
    }));
    await f.page.route("**/console/preferences", (route) => {
        f.calls.push({ path: "/console/preferences", ...route.request().postDataJSON() });
        return route.fulfill({ json: { ok: true } });
    });
    await f.page.reload();
    const chip = f.page.getByRole("button", { name: "思考强度", exact: true });
    await chip.click();
    await f.page.getByRole("menuitem", { name: "High", exact: true }).click();
    await f.page.getByText("偏好已保存，这一轮结束后生效", { exact: true }).waitFor();
    assert.deepEqual(f.calls.find((c) => c.path === "/console/preferences").patch, { reasoning_effort: "high" }, "The choice made during a turn must reach the server");
};

// The order of the projects is the reader's own: the one opened every
// morning belongs at the top. A drag is the whole gesture — there is no
// save button — and the order survives a reload. The keyboard can do the
// same move, and the default order can always be had back.
checks["project-drag-reorder"] = async (f) => {
    const projects = [project("alpha"), project("beta"), project("gamma"), project("home")];
    await f.page.route("**/state", (route) => route.fulfill({ json: f.snapshot = workState({ at, hub: { node: "test-node" }, nodes: [], agents: [], tasks: [], plans: [], projects, attempts: [], landings: [] }) }));
    await f.page.reload();
    const sidebar = f.page.locator(".conversation-sidebar");
    const rows = sidebar.locator('.conversation-project[draggable="true"]');
    const order = () => rows.locator(".u-title").allInnerTexts();
    const rowOf = (id) => rows.filter({ has: f.page.getByRole("button", { name: id, exact: true }) });
    const stored = () => f.page.evaluate(() => localStorage.getItem("steve.projects.order"));
    await rowOf("alpha").waitFor();
    assert.deepEqual(await order(), ["alpha", "beta", "gamma"], "A reader who has never moved anything gets the order by name");
    assert.equal(await rows.count(), 3, "Only the work projects can be dragged; the private chat keeps its place");

    // Dropping on the upper half of a row puts the project above it, and
    // the line that says so appears while the pointer is still down.
    const drag = async (from, onto, edge) => {
        await rowOf(from).hover();
        await f.page.mouse.down();
        const box = await rowOf(onto).boundingBox();
        const y = edge === "above" ? box.y + 3 : box.y + box.height - 3;
        await f.page.mouse.move(box.x + 40, y, { steps: 8 });
        await f.page.mouse.move(box.x + 40, y);
        assert.equal(await rowOf(onto).getAttribute("data-drop"), edge === "above" ? "before" : "after", "The insertion line must say where the project will land");
        await f.page.mouse.up();
    };
    await drag("gamma", "alpha", "above");
    assert.deepEqual(await order(), ["gamma", "alpha", "beta"]);
    assert.equal(await rows.locator("[data-drop]").count(), 0, "The insertion line goes away once the project has landed");
    assert.deepEqual(JSON.parse(await stored()), ["gamma", "alpha", "beta"], "The drag is the save");

    // A list that forgets how it was ordered is ordered again every morning.
    await f.page.reload();
    await rowOf("alpha").waitFor();
    assert.deepEqual(await order(), ["gamma", "alpha", "beta"]);

    // The same move without a mouse.
    const title = rowOf("gamma").getByRole("button", { name: "gamma", exact: true });
    assert.equal(await title.getAttribute("aria-keyshortcuts"), "Alt+ArrowUp Alt+ArrowDown");
    assert.match(await title.getAttribute("title"), /Alt/, "The row says how it can be moved");
    await title.focus();
    await f.page.keyboard.press("Alt+ArrowDown");
    assert.deepEqual(await order(), ["alpha", "gamma", "beta"]);
    await f.page.keyboard.press("Alt+ArrowUp");
    assert.deepEqual(await order(), ["gamma", "alpha", "beta"], "Alt + up is the undo of Alt + down");

    // Dropping below the last row puts a project at the end.
    await drag("gamma", "beta", "below");
    assert.deepEqual(await order(), ["alpha", "beta", "gamma"]);
    assert.deepEqual(JSON.parse(await stored()), ["alpha", "beta", "gamma"], "An order that happens to match the names is still the reader's order");

    // And the default order can be had back without dragging anything.
    const arrange = sidebar.getByRole("button", { name: /^排列/ });
    await arrange.click();
    await f.page.getByRole("menuitem", { name: "恢复默认项目顺序", exact: true }).click();
    assert.deepEqual(await order(), ["alpha", "beta", "gamma"]);
    assert.equal(await stored(), null, "Restoring the default order forgets the saved one");
    await arrange.click();
    assert.equal(await f.page.getByRole("menuitem", { name: "恢复默认项目顺序", exact: true }).count(), 0, "There is nothing to restore until something is moved");
    await f.page.keyboard.press("Escape");
    assert.equal(f.calls.length, 0, "Ordering the sidebar must not submit work");
};

const selected = process.env.CHECK ? process.env.CHECK.split(",") : Object.keys(checks);
let failed = 0;
try {
    for (const name of selected) {
        assert.ok(checks[name], `Unknown CHECK ${name}; choices: ${Object.keys(checks).join(",")}`);
        const f = await fixture({ history: name.startsWith("scroll-"), running: name.startsWith("stop-") || name === "scroll-stream" || name === "preferences-during-a-turn", sandboxed: name.startsWith("preview-html-") || name === "preview-svg-scripts-disabled" });
        try {
            await checks[name](f);
            assert.deepEqual(f.errors, [], "Browser and mocked API errors");
            console.log(`PASS ${name}`);
        } catch (error) {
            failed++;
            await f.page.screenshot({ path: path.join(output, `${name}.png`) });
            console.error(`FAIL ${name}: ${error.message}`);
        } finally {
            for (const release of f.releases) release();
            await f.context.close();
        }
    }
} finally {
    await browser.close();
    app.close();
}
console.log(`${selected.length - failed}/${selected.length} passed; screenshots: ${output}`);
if (failed) process.exitCode = 1;
