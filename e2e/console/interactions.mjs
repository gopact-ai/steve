// Build web/console first. Uses an existing Playwright installation;
// PLAYWRIGHT_MODULE accepts its absolute module path. CHECK selects comma-
// separated scenarios. Every API is mocked; no request reaches a real hub.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { preview } from "../childcard/preview.mjs";
import { usageDurationFixture, usageFixture, usageState } from "./usage-fixture.mjs";

const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || new URL("../../web/console/node_modules/playwright/index.mjs", import.meta.url).href);
const output = process.env.OUTPUT_DIR || path.join(os.tmpdir(), "steve-console-interactions");
await mkdir(output, { recursive: true });
const app = await preview();
const browser = await chromium.launch({ headless: process.env.HEADED !== "1", channel: process.env.BROWSER_CHANNEL });
const at = "2026-09-06T10:00:00Z";
const A = "console:interaction-a", B = "console:interaction-b";
const project = (id) => ({ id, node: "test-node", path: `/test/${id}`, repo: "inplace", level: "public", home: id === "home", agents: [], workspaces: [] });
const task = (id, channel, projectID) => ({ id, channel, project_id: projectID, goal: `Task ${id}`, state: "running", lifecycle: "running", execution: "running", lane: "running", attention: 0, turns: 1, max_turns: 10, member: "test-agent", node: "test-node", updated_at: at });
const gate = () => { let release; const promise = new Promise((resolve) => { release = resolve; }); return { promise, release }; };
async function eventually(predicate, message) {
    for (let i = 0; i < 80; i++) { if (await predicate()) return; await delay(25); }
    assert.fail(message);
}

async function fixture({ history = false, running = false } = {}) {
    const context = await browser.newContext({ viewport: { width: 1600, height: 1000 }, serviceWorkers: "block" });
    const page = await context.newPage();
    await page.addInitScript(() => localStorage.setItem("steve.ui.locale", "zh"));
    page.setDefaultTimeout(2500);
    await page.clock.install();
    const f = { page, context, calls: [], errors: [], releases: [], binding: null, enqueue: null, cancel: null, failBinding: false, failEnqueue: false, replyReads: 0 };
    page.on("pageerror", (e) => f.errors.push(String(e)));
    const conversations = [{ id: A, title: "Conversation A", project: "scratch" }, { id: B, title: "Conversation B", project: "home" }];
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
        if (!["/state", "/events", "/history"].includes(pathname) && !pathname.startsWith("/console/")) return route.continue();
        const input = req.postDataJSON();
        const call = { method: req.method(), path: pathname, ...input };
        if (req.method() !== "GET") f.calls.push(call);
        const conversation = input?.conversation || url.searchParams.get("conversation") || A;
        let current = conversations.find((c) => c.id === conversation);
        if (pathname === "/state") return route.fulfill({ json: { at, hub: { node: "test-node", started: at, version: "test" }, nodes: [], agents: [], tasks: [task("11", A, "scratch"), task("22", B, "home")], plans: [], projects, attempts: [], landings: [] } });
        if (pathname === "/console/send" && input?.input.startsWith("/project use ")) {
            if (f.binding) await f.binding;
            if (f.failBinding) return route.fulfill({ status: 503, json: { error: "Project binding unavailable" } });
            if (!current) { current = { id: conversation, title: "New project conversation", project: "home" }; conversations.push(current); }
            current.project = input.input.slice("/project use ".length);
            return route.fulfill({ json: { reply: { kind: "reply", conversation, at, text: `Bound ${current.project}` } } });
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
        if (pathname === "/console/verbs" || pathname === "/console/suggest") return route.fulfill({ json: { verbs: [], suggestions: [] } });
        f.errors.push(`Unhandled API: ${req.method()} ${pathname}`);
        return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
    });
    await page.addInitScript((conversation) => {
        if (!sessionStorage.getItem("steve.conversation")) sessionStorage.setItem("steve.conversation", conversation);
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
    f.box = page.getByRole("textbox", { name: "消息", exact: true });
    f.queued = () => f.calls.filter((c) => c.path === "/console/queue" && c.method === "POST");
    f.pick = async (name) => { await page.getByRole("button", { name: new RegExp(`^Conversation ${name}`) }).click(); await page.locator("main header").getByText(`Conversation ${name}`, { exact: true }).waitFor(); };
    return f;
}

const checks = {
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
        await eventually(() => f.calls.some((c) => c.input === "/project use scratch"), "Generic new conversation must bind the current project");
        await f.box.fill("First work");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "First work must be accepted after binding");
        assert.equal(f.queued()[0].projectAtEnqueue, "scratch");
    },
    async "binding-pending"(f) {
        const release = f.hold("binding");
        await f.page.getByRole("button", { name: "在 scratch 下新会话", exact: true }).click();
        await eventually(() => f.calls.some((c) => c.input === "/project use scratch"), "Project binding must begin");
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
        await eventually(() => f.calls.some((c) => c.input === "/project use scratch"), "Project binding must begin");
        await delay(150);
        assert.equal(await f.box.inputValue(), "Keep this existing draft", "Binding failure must preserve the original draft");
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
        assert.equal(await f.box.inputValue(), "", "A draft must not leak into B");
        await f.box.fill("Draft for B");
        await f.pick("A");
        assert.equal(await f.box.inputValue(), "Draft for A");
        await f.pick("B");
        assert.equal(await f.box.inputValue(), "Draft for B");
    },
    async "draft-persistence"(f) {
        await f.box.fill("Unsent work survives navigation");
        await f.page.locator('a[href="#/projects"]').click();
        await f.page.getByRole("button", { name: "添加项目", exact: true }).waitFor();
        await f.page.locator('a[href="#/console"]').click();
        assert.equal(await f.box.inputValue(), "Unsent work survives navigation", "Leaving the console must preserve its draft");
        await f.page.reload();
        await f.box.waitFor();
        assert.equal(await f.box.inputValue(), "Unsent work survives navigation", "Reload must preserve the unsent draft");
    },
    async "send-continuation"(f) {
        const release = f.hold("enqueue");
        await f.box.fill("First instruction");
        await f.box.press("Enter");
        await eventually(() => f.queued().length === 1, "Enqueue request must start");
        await f.box.pressSequentially("Next draft");
        assert.equal(await f.box.inputValue(), "Next draft", "Typing during a slow enqueue must begin a fresh draft");
        release();
        await delay(150);
        assert.equal(await f.box.inputValue(), "Next draft", "The delayed receipt must not clear the new draft");
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
        assert.match(await f.box.inputValue(), /^Unsent first instruction\n+New draft typed during submission$/, "Failed submission must restore the first instruction without losing newer typing");
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
        assert.equal(await f.box.inputValue(), "B unsent draft", "A's receipt must not alter B's draft");
        await f.pick("A");
        assert.equal(await f.box.inputValue(), "", "A's submitted instruction must not reappear as a draft");
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
        assert.match(await f.box.inputValue(), /New draft after remount/, "An old failure must not overwrite the new mounted draft");
        await f.page.reload();
        await f.box.waitFor();
        assert.match(await f.box.inputValue(), /New draft after remount/, "Newer typing must remain persisted after an unmounted request fails");
        assert.match(await f.box.inputValue(), /Original instruction/, "The failed instruction must remain recoverable");
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
        assert.equal(await f.box.inputValue(), "Newer unsent draft", "Reload must keep new typing separate from an uncertain submission");
        assert.equal(f.queued().length, 1, "Reload must not resend an instruction that may already have been accepted");
        await f.box.press("Enter");
        await delay(150);
        assert.equal(f.queued().length, 1, "Enter must not bypass unresolved submission recovery");
        assert.equal(await f.box.inputValue(), "Newer unsent draft");
        assert.equal(await f.page.getByRole("button", { name: "恢复为草稿", exact: true }).count(), 0, "An unresolved keyed submission must retain idempotency protection");
        await f.page.getByRole("button", { name: "重试这次发送", exact: true }).click();
        await eventually(() => f.queued().length === 2, "Explicit retry must be submitted");
        assert.equal(f.queued()[0].command_id, f.queued()[1].command_id, "Retry must preserve the original identity");
        release();
        await eventually(async () => await f.page.getByRole("button", { name: "重试这次发送", exact: true }).count() === 0, "Receipt must settle the pending submission");
        assert.equal(await f.box.inputValue(), "Newer unsent draft", "Retry must preserve newer typing");
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
        assert.equal(await f.box.inputValue(), "Newer persisted draft", "Failed recovery must preserve the previously saved draft");
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
        assert.equal(await f.box.inputValue(), "Question about this quote");
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
        assert.equal(await f.box.inputValue(), "New instruction during cancellation", "A blocked Enter must preserve the draft");
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
        await eventually(async () => (await f.box.inputValue()).startsWith("/plan"), "New plan must fill the composer");
        await f.box.fill("Plan carefully drafted after fill");
        await f.page.locator('a[href="#/projects"]').click();
        await f.page.getByRole("button", { name: "添加项目", exact: true }).waitFor();
        await f.page.locator('a[href="#/console"]').click();
        assert.equal(await f.box.inputValue(), "Plan carefully drafted after fill", "An already-consumed fill intent must not overwrite the persisted draft");
    },
    async "conversation-rename-once"(f) {
        const edit = async (title) => {
            await f.page.getByRole("button", { name: new RegExp(`^${title}`) }).locator("..").getByRole("button", { name: "更多", exact: true }).click();
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
    assert.equal(await f.box.inputValue(), "", "Mobile selection must switch to B's own draft");
    await open.click();
    await f.pick("A");
    assert.equal(await f.box.inputValue(), "Mobile draft A");
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
    assert.equal(await f.box.inputValue(), "Current mobile conversation draft");
};

checks["design-mobile-child"] = async (f) => {
    const tasks = [task("11", A, "scratch"), { ...task("33", A, "scratch"), parent: "11" }];
    await f.page.route("**/state", (route) => route.fulfill({ json: { at, hub: { node: "test-node", started: at, version: "test" }, nodes: [], agents: [], tasks, plans: [], projects: [project("scratch"), project("home")], attempts: [], landings: [] } }));
    await f.page.route("**/console/replies?*", (route) => route.fulfill({ json: { enabled: true, replies: [{ id: "parent-reply", kind: "reply", conversation: A, at, text: "Parent reply", process: { steps: [{ id: "#33", kind: "delegate", goal: "Delegated work", state: "done", answer: "Child answer" }] } }] } }));
    await f.page.reload();
    await f.box.waitFor();
    await f.page.setViewportSize({ width: 390, height: 844 });
    await f.box.fill("Draft before viewing a child");
    await f.page.getByRole("button", { name: "会话列表", exact: true }).click();
    const sheet = f.page.getByRole("dialog", { name: "会话列表", exact: true });
    await sheet.getByRole("button", { name: /^2 在跑$/ }).click();
    await sheet.getByRole("button", { name: /#33.*Task 33/ }).click();
    await eventually(async () => await sheet.count() === 0, "Opening a child task must dismiss mobile conversation navigation");
    await f.page.getByText("Child answer", { exact: true }).waitFor();
    await f.page.getByRole("button", { name: /回到对话/ }).click();
    await visibleControl(f.box, "Message after returning from a child task");
    assert.equal(await f.box.inputValue(), "Draft before viewing a child");
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

checks["design-mobile-new-failure"] = async (f) => {
    await f.page.setViewportSize({ width: 390, height: 844 });
    await f.box.fill("Draft kept after mobile creation fails");
    f.failBinding = true;
    await f.page.getByRole("button", { name: "会话列表", exact: true }).click();
    await f.page.getByRole("dialog", { name: "会话列表", exact: true }).getByRole("button", { name: "新会话", exact: true }).click();
    await eventually(() => f.calls.some((call) => call.input === "/project use scratch"), "Mobile project binding must begin");
    await f.page.getByRole("status").filter({ hasText: "Project binding unavailable" }).waitFor();
    await visibleControl(f.box, "Message after mobile creation failure");
    assert.equal(await f.box.inputValue(), "Draft kept after mobile creation fails");
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
    assert.equal(await f.box.inputValue(), "Draft while inspecting");
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
    const drawer = f.page.getByRole("dialog", { name: "详细信息", exact: true });
    await drawer.waitFor();
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

async function reviewFixture(f) {
    const changes = [
        { path: "src/main.ts", status: "M", added: 1, deleted: 1 },
        { path: "removed.txt", status: "D", added: 0, deleted: 1 },
        { path: "new.txt", status: "A", added: 1, deleted: 0 },
        { path: "logo.png", status: "M", added: 0, deleted: 0, binary: true },
        { path: "script.sh", status: "M", added: 0, deleted: 0 },
    ];
    const state = { reads: [], hold: null, holdIndex: null, holdFile: null, failDiff: false, failIndex: false };
    await f.page.route(/\/console\/tasks\/[^/]+\/attempts$/, (route) => route.fulfill({ json: [{ id: "review-attempt", kind: "turn", state: "done", agent: "test-agent", node: "test-node", base: "before", artifact: "after", started_at: at, files: 5 }, { id: "review-other", kind: "turn", state: "done", agent: "other-agent", node: "test-node", base: "older-before", artifact: "older-after", started_at: "2026-09-05T10:00:00Z", files: 1 }] }));
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
        if (url.pathname.endsWith("/tree")) return route.fulfill({ json: { attempt: "review-attempt", commit: "after", which: "result", dir: file, entries: file === "src" ? [{ name: "main.ts", path: "src/main.ts", kind: "file", size: 48 }, { name: "helper.ts", path: "src/helper.ts", kind: "file", size: 22 }] : [{ name: "src", path: "src", kind: "dir" }, { name: "README.md", path: "README.md", kind: "file", size: 36 }, { name: "empty.txt", path: "empty.txt", kind: "file", size: 0 }, { name: "new.txt", path: "new.txt", kind: "file", size: 10 }, { name: "linked", path: "linked", kind: "link" }, { name: "vendor", path: "vendor", kind: "repo" }, { name: "large.txt", path: "large.txt", kind: "file", size: 40000 }] } });
        if (url.pathname.endsWith("/file")) {
            if (state.holdFile && file === "README.md") await state.holdFile;
            const text = file === "README.md" ? "# Project\n\nUnchanged project guide." : file === "src/main.ts" ? "const shared = true;\nconst after = 2;\n" : file === "empty.txt" ? "" : file === "large.txt" ? Array.from({ length: 1501 }, (_, i) => `line ${i + 1}`).join("\n") : file === "linked" ? "README.md" : "export const helper = 1;";
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
    await f.page.getByRole("tab", { name: "代码", exact: true }).click();
    await f.page.getByRole("button", { name: "查看变更", exact: true }).first().click();
    await f.page.getByRole("dialog", { name: "代码工作区", exact: true }).waitFor();
    return state;
}

checks["review-navigation"] = async (f) => {
    await f.box.fill("Draft retained while reviewing");
    const state = await reviewFixture(f);
    const review = f.page.getByRole("dialog", { name: "代码工作区", exact: true });
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
    assert.equal(await f.box.inputValue(), "Draft retained while reviewing", "Review must preserve the conversation draft");
    assert.equal(f.calls.length, 0, "Review must never write or submit work");
};

checks["review-late-response"] = async (f) => {
    const state = await reviewFixture(f);
    const review = f.page.getByRole("dialog", { name: "代码工作区", exact: true });
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
    const review = f.page.getByRole("dialog", { name: "代码工作区", exact: true });
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
    await review.getByRole("button", { name: /选择执行/ }).click();
    await f.page.getByRole("option", { name: /other-agent/ }).click();
    await review.getByRole("heading", { name: "other.txt", exact: true }).waitFor();
    await review.getByText("Other execution content", { exact: true }).first().waitFor();
    assert.equal(await review.getByText("const after = 2;", { exact: true }).count(), 0, "Execution selection must show only that execution's changes");
};

checks["review-mobile-origin"] = async (f) => {
    await f.page.setViewportSize({ width: 1280, height: 900 });
    await reviewFixture(f);
    const review = f.page.getByRole("dialog", { name: "代码工作区", exact: true });
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
    const workspace = f.page.getByRole("dialog", { name: "代码工作区" });
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
    await f.page.route(/\/console\/tasks\/[^/]+\/attempts$/, (route) => route.fulfill({ json: [
        { id: "unchanged", kind: "turn", state: "done", base: "snapshot", artifact: "snapshot", started_at: at },
        { id: "base-only", kind: "turn", state: "running", agent: "base-agent", base: "start", started_at: "2026-09-05T10:00:00Z" },
    ] }));
    await f.page.route(/\/console\/attempts\/(unchanged|base-only)\//, (route) => {
        const url = new URL(route.request().url()), base = url.pathname.includes("base-only"), commit = base ? "start" : state.commit;
        assert.equal(route.request().method(), "GET");
        if (url.pathname.endsWith("/changes")) return state.failIndex ? route.fulfill({ status: 503, body: "Index offline" }) : route.fulfill({ json: { attempt: base ? "base-only" : "unchanged", base: commit, artifact: base ? undefined : commit, changes: [] } });
        if (url.pathname.endsWith("/tree")) return route.fulfill({ json: { attempt: base ? "base-only" : "unchanged", commit, which: base ? "base" : "result", dir: "", entries: [{ name: "plain.ts", path: "plain.ts", kind: "file" }] } });
        if (url.pathname.endsWith("/file")) { state.fileReads++; return route.fulfill({ json: { attempt: base ? "base-only" : "unchanged", commit, path: "plain.ts", text: `// ${commit}\nconst plain = true;\n`, size: 40 } }); }
        assert.fail("An unchanged file must not fetch a diff");
    });
    await f.page.getByRole("button", { name: "显示详情", exact: true }).click();
    await f.page.getByRole("tab", { name: "代码", exact: true }).click();
    await f.page.getByRole("button", { name: "浏览文件", exact: true }).first().click();
    const workspace = f.page.getByRole("dialog", { name: "代码工作区" });
    await workspace.getByRole("button", { name: "plain.ts", exact: true }).click();
    await workspace.getByText("// snapshot", { exact: true }).waitFor();
    await workspace.getByRole("button", { name: /选择执行/ }).click();
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
    const workspace = f.page.getByRole("dialog", { name: "代码工作区" });
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

checks["code-source-feedback"] = async (f) => {
    await reviewFixture(f);
    const workspace = f.page.getByRole("dialog", { name: "代码工作区" });
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
    const workspace = f.page.getByRole("dialog", { name: "代码工作区" });
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
    const usage = usageFixture();
    const state = { ...usageState(usage), tasks: [{ ...task("11", A, "scratch"), execution: "unknown", lane: "unknown" }], agents: [{ id: "test-agent", harness: "mock", eligible: true, activity_known: false, busy: 0, activities: [] }], sources: [
        { name: "ledger", wired: true, error: "partial ledger read" },
        { name: "ledger-live", wired: true, error: "activity unavailable" },
        { name: "ledger-attention", wired: true, error: "attention unavailable" },
        { name: "ledger-usage", wired: true },
    ] };
    await f.page.route("**/state", (route) => route.fulfill({ json: state }));
    await f.page.goto(`${app.url}/#/console?view=board`); await f.page.reload();
    await f.page.getByText("Task 11", { exact: true }).waitFor();
    await f.page.getByText("状态未知", { exact: true }).first().waitFor();
    await f.page.getByRole("tab", { name: "用量", exact: true }).click();
    await f.page.getByRole("heading", { name: "用量概览", exact: true }).waitFor();
    await f.page.getByRole("region", { name: "区间用量汇总", exact: true }).waitFor();
    await f.page.getByRole("link", { name: "待处理", exact: true }).click();
    await f.page.getByText(/待处理信息尚未完整读取/).waitFor();
    assert.equal(await f.page.getByText("暂无待处理请求", { exact: true }).count(), 0, "Unavailable attention cannot be presented as no requests");
    await f.page.getByRole("link", { name: "资源", exact: true }).click();
    await f.page.getByText("活动状态未知", { exact: true }).waitFor();
};

checks["usage-dashboard-ranges"] = async (f) => {
    const usage = usageFixture();
    await f.page.route("**/state", (route) => route.fulfill({ json: usageState(usage) }));
    await f.page.goto(`${app.url}/#/console?view=board&tab=usage`);
    await f.page.reload();
    await f.page.getByRole("heading", { name: "用量概览", exact: true }).waitFor();
    const cards = f.page.getByRole("region", { name: "区间用量汇总", exact: true });
    for (const range of ["7d", "1d", "30d"]) {
        await f.page.getByRole("button", { name: range, exact: true }).click();
        await eventually(async () => await f.page.getByRole("button", { name: range, exact: true }).getAttribute("aria-pressed") === "true", `Range ${range} should become active`);
        await f.page.getByRole("grid", { name: "按 Agent", exact: true }).getByText(`agent-${range}`, { exact: true }).waitFor();
        assert.ok((await cards.innerText()).includes(String(usage.periods[range].total.attempts)), "Range metrics must use that period's totals");
        assert.equal(await f.page.locator('a[href="#/console?view=board"]').getAttribute("aria-current"), "page");
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
    await f.page.route("**/state", (route) => route.fulfill({ json: usageState(usage) }));
    await f.page.goto(`${app.url}/#/console?view=board&tab=usage&range=1d`); await f.page.reload();
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
    await f.page.route("**/state", (route) => route.fulfill({ json: usageState(usage) }));
    await f.page.goto(`${app.url}/#/console?view=board&tab=usage&range=1d`); await f.page.reload();
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
    await f.page.route("**/state", (route) => route.fulfill({ json: usageState(usage) }));
    await f.page.goto(`${app.url}/#/console?view=board&tab=usage&range=7d`); await f.page.reload();
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
    await f.page.route("**/state", (route) => route.fulfill({ json: usageState(usage) }));
    await f.page.goto(`${app.url}/#/console?view=board&tab=usage&range=1d`); await f.page.reload();
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
    await f.page.getByRole("button", { name: "偏好设置", exact: true }).click();
    await f.page.getByRole("menuitem", { name: "English", exact: true }).click();
    await f.page.getByText("TPM covers 200 tokens; 100 tokens have no valid duration.", { exact: true }).waitFor();
    await f.page.getByText("Duration measured for 1/2 tasks", { exact: true }).waitFor();

    usage = usageDurationFixture("unreported"); await f.page.reload();
    await f.page.getByText("有效耗时覆盖 1/1 个任务", { exact: true }).waitFor();
    assert.equal(await cardValue("TPM").innerText(), "0", "A valid duration with unreported tokens follows the explicit zero-token policy");
    assert.equal(await cardValue("任务平均耗时").innerText(), "1 分 0 秒");
    usage = usageDurationFixture("missing"); await f.page.reload();
    await f.page.getByRole("button", { name: "偏好设置", exact: true }).click();
    await f.page.getByRole("menuitem", { name: "English", exact: true }).click();
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
    await f.page.route("**/state", (route) => route.fulfill({ json: usageState(usage) }));
    await f.page.goto(`${app.url}/#/console?view=board&tab=usage&range=7d`); await f.page.reload();
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

checks["process-content"] = async (f) => {
    const process = { tools: [{ id: "read", title: "Read file", status: "completed", output: "file content" }], timeline: [{ kind: "thought", text: "Checking the file", at }, { kind: "text", text: "Opening the file", at }, { kind: "tool", tool: "read", at }, { kind: "text", text: "Checked answer", at }], steps: [{ id: "empty-step", timeline: [{ kind: "thought", text: " ", at }] }] };
    f.replies[A] = [{ id: "trace", kind: "reply", conversation: A, at, text: "Checked answer", process }];
    await f.page.reload();
    const message = f.page.locator(".message-assistant");
    await message.getByText("Checked answer", { exact: true }).waitFor();
    const toggle = message.locator("summary").filter({ hasText: /^过程/ });
    await toggle.press("Enter");
    await message.getByText("Checking the file", { exact: true }).waitFor();
    await message.getByText("Opening the file", { exact: true }).waitFor();
    assert.equal(await message.locator('[data-span-kind="tool"]').count(), 1, "Actual tool activity remains inspectable");
    assert.equal(await message.getByText("Checked answer", { exact: true }).count(), 1, "The final answer is not repeated inside the trace");
    assert.equal(await message.getByText("empty-step", { exact: true }).count(), 0, "Empty plan steps must not leave a heading");
    await toggle.press("Enter");
    await f.startRunning();
    await f.page.getByText("正在放置…", { exact: true }).waitFor();
    await f.emit({ kind: "console.progress", exchange_id: "design-running", progress: { agent: "test-agent", timeline: [{ kind: "thought", text: " ", at }], answer: "Streaming answer" } });
    await f.page.getByText("test-agent", { exact: true }).waitFor();
    await f.page.getByText("Streaming answer", { exact: true }).waitFor();
    assert.equal(await f.page.locator('summary').filter({ hasText: /^过程/ }).count(), 1, "Empty live progress retains its status without a disclosure");
    assert.equal(f.calls.length, 0);
};

const selected = process.env.CHECK ? process.env.CHECK.split(",") : Object.keys(checks);
let failed = 0;
try {
    for (const name of selected) {
        assert.ok(checks[name], `Unknown CHECK ${name}; choices: ${Object.keys(checks).join(",")}`);
        const f = await fixture({ history: name.startsWith("scroll-"), running: name.startsWith("stop-") || name === "scroll-stream" });
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
