// Pure request lifecycle checks; browser scenarios below use only mocked APIs.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "../../web/console/node_modules/typescript/lib/typescript.js";
import { createResourceRead } from "../../web/console/src/lib/resource-read.ts";

const gate = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; };
const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

async function resourceChecks() {
    const requests = [], seen = [], errors = [];
    const resource = createResourceRead((signal) => { const request = gate(); requests.push({ ...request, signal }); return request.promise; }, (value) => seen.push(value), (error) => errors.push(error));
    const pending = resource.refresh();
    for (let i = 0; i < 20; i++) resource.refresh();
    assert.equal(requests.length, 1, "Repeated refreshes must not overlap a resource read");
    requests[0].resolve("first"); await tick();
    assert.deepEqual(seen, ["first"], "Continuous refresh traffic must not starve a completed read");
    assert.equal(requests.length, 2, "Refreshes during a read collapse into one follow-up");
    requests[1].resolve("second"); await pending; await tick();
    assert.deepEqual(seen, ["first", "second"]);
    const next = resource.refresh();
    resource.dispose();
    assert.equal(requests[2].signal.aborted, true, "Changing resource identity cancels its old generation");
    requests[2].resolve("old generation"); await next;
    assert.deepEqual(seen, ["first", "second"], "Disposed results must not reach the new resource");
    assert.deepEqual(errors, []);
    const failure = gate(), failures = [];
    const failed = createResourceRead(() => failure.promise, () => assert.fail("unexpected success"), (error) => failures.push(error));
    const done = failed.refresh(); failure.reject(new Error("Unavailable")); await done;
    assert.equal(failures[0].message, "Unavailable", "Read errors must remain observable");
    console.log("PASS resource coalescing, generation, progress and failure");
}
await resourceChecks();

async function submissionSupportChecks() {
    // Compile the real API module with only its HTTP boundary replaced. Every
    // request is held locally; this check cannot contact a running service.
    const requests = [], posts = [];
    globalThis.__supportRequest = (path, options = {}) => {
        if (options.method === "POST") { posts.push(path); return Promise.resolve({ id: "exchange", conversation: "console:test", key: "client:test" }); }
        const request = gate(); requests.push({ ...request, path }); return request.promise;
    };
    try {
        const source = readFileSync(new URL("../../web/console/src/lib/api/console.ts", import.meta.url), "utf8").replace('import { request, UnsentRequestError } from "../http";', 'const request = globalThis.__supportRequest; class UnsentRequestError extends Error {}');
        const code = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
        const api = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);
        for (const outcome of ["unsupported", "failed"]) {
            const offset = requests.length;
            const old = api.fetchQueue("console:old");
            const sending = api.enqueue("console:test", "Run once", [], "test");
            const rejected = assert.rejects(sending, /Hub|指令尚未发送/, "A stale supported poll must not replace this write's failed preflight");
            if (outcome === "unsupported") requests[offset + 1].resolve({ queue: [], submission_keys: false });
            else requests[offset + 1].reject(new Error("Preflight unavailable"));
            // Resolve both in the same microtask batch, newest response first.
            requests[offset].resolve({ queue: [], submission_keys: true });
            await Promise.all([old, rejected]);
            assert.equal(posts.length, 0, "Rejected or failed capability checks must emit zero POSTs");
            assert.equal(api.getSubmissionSupport().state, outcome === "unsupported" ? "unsupported" : "unknown", "Older polls must not overwrite the newest completed capability result");
        }
        const offset = requests.length;
        const sending = api.enqueue("console:test", "Run once", [], "test");
        const rejected = assert.rejects(sending, /Hub/, "Even a newer supported poll cannot authorize a rejected preflight");
        const newer = api.fetchQueue("console:newer");
        requests[offset].resolve({ queue: [], submission_keys: false });
        requests[offset + 1].resolve({ queue: [], submission_keys: true });
        await Promise.all([newer, rejected]);
        assert.equal(posts.length, 0, "Each write must use its own capability response");
        assert.equal(api.getSubmissionSupport().state, "supported", "Display state may follow the newer poll independently");
        console.log("PASS submission capability preflight identity and stale-response ordering");
    } finally { delete globalThis.__supportRequest; }
}
await submissionSupportChecks();

const { settingsDraft, parseSettings } = await import("../../web/console/src/lib/settings-draft.ts");
const original = { harnesses: { sample: { command: "sample", args: ["hello world", "", "--flag", "--flag"] } }, mcp_servers: { sample: { type: "stdio", env: { FOO: "bar", SPACED: " value " }, headers: { Accept: "text/plain", Spaced: " value " } } }, tools: [], declares: [], capabilities: [] };
const draft = settingsDraft(original);
assert.deepEqual(parseSettings(draft), original, "Arguments including whitespace, empty values and duplicates must round-trip");
draft.mcp_servers[0].value.envText = "F";
assert.throws(() => parseSettings(draft), /第 1 行/, "Incomplete lines fail at save without changing the raw draft");
assert.equal(draft.mcp_servers[0].value.envText, "F");
draft.mcp_servers[0].value.envText = "FOO=bar\nFOO=another";
assert.throws(() => parseSettings(draft), /名称重复/);
draft.harnesses.push({ key: 2, id: " sample ", value: { command: "other" } });
assert.throws(() => parseSettings(draft), /名称重复/);
console.log("PASS configuration drafts round-trip and reject invalid/duplicate keys");

const { short } = await import("../../web/console/src/lib/format.ts");
const { applyLive } = await import("../../web/console/src/lib/live.ts");
assert.equal(short("abcdef", 3), "abc", "Display formatting must import without browser globals");
const running = applyLive(null, { kind: "console.sent", at: "2026-09-06T00:00:00Z", exchange_id: "current" });
assert.equal(applyLive(running, { kind: "console.reply", at: "", exchange_id: "previous" }), running, "A previous exchange reply must not finish the current turn");
assert.equal(applyLive(running, { kind: "console.reply", at: "", exchange_id: "current" }), null);
console.log("PASS browser-independent formatting and live projection");

const stored = new Map();
globalThis.sessionStorage = { getItem: (key) => stored.get(key) ?? null, setItem: (key, value) => stored.set(key, value) };
const submissions = await import("../../web/console/src/lib/drafts.ts");
submissions.updateDraft("test:a", "Original");
const first = submissions.beginSubmission("test:a", "Original", []);
assert.ok(first.id);
assert.equal(JSON.parse(stored.get("steve.console.drafts")).drafts["test:a"], undefined, "Persisting pending and consuming the draft must be atomic");
submissions.updateDraft("test:a", "Newer draft");
submissions.failSubmission("test:a", first.id, "Connection reset", "unknown");
submissions.retrySubmission("test:a");
submissions.failSubmission("test:a", first.id, "Preflight failed before retry", "rejected");
assert.equal(submissions.restoreSubmission("test:a"), false, "Uncertain keyed work must not become a fresh draft");
const retry = submissions.retrySubmission("test:a");
assert.equal(retry.id, first.id);
assert.equal(retry.input, "Original");
assert.equal(submissions.reconcileSubmission("test:a", [{ conversation: "test:a", input: "Original", key: `client:${first.id}` }]), true);
const second = submissions.beginSubmission("test:a", "Newer draft", []);
assert.equal(submissions.failSubmission("test:a", first.id, "Late reset", "unknown"), false, "An old acknowledgement cannot change a newer operation");
submissions.updateDraft("test:a", "More typing");
submissions.failSubmission("test:a", second.id, "Bad input", "rejected");
assert.equal(JSON.parse(stored.get("steve.console.drafts")).drafts["test:a"], "Newer draft\nMore typing");
const conflict = submissions.beginSubmission("test:a", "Conflict", []);
submissions.failSubmission("test:a", conflict.id, "Conflict", "conflict");
assert.equal(submissions.retrySubmission("test:a"), null);
assert.equal(submissions.restoreSubmission("test:a"), false);
submissions.finishSubmission("test:a", conflict.id);
const stop = submissions.beginStop("test:a");
assert.equal(submissions.beginStop("test:a"), null, "Stop pending is shared independently of route instances");
submissions.finishStop("test:a", stop, { uncertain: true });
assert.equal(submissions.beginStop("test:a"), stop, "Explicit stop retry reuses the same command identity");
submissions.finishStop("test:a", stop, { error: "Retry preflight failed", uncertain: false });
assert.equal(submissions.isStopPending("test:a"), true, "A failed stop retry cannot settle the original unknown operation");
assert.equal(submissions.beginStop("test:a"), stop);
submissions.finishStop("test:a", stop, { message: "Stopped" });
assert.equal(submissions.isStopPending("test:a"), false);
delete globalThis.sessionStorage;
console.log("PASS durable submission identity, reconciliation, rejection and shared stop lifecycle");

if (process.env.PURE_ONLY !== "1") {
    const { preview } = await import("../childcard/preview.mjs");
    const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || new URL("../../web/console/node_modules/playwright/index.mjs", import.meta.url).href);
    const app = await preview();
    const browser = await chromium.launch({ headless: process.env.HEADED !== "1", channel: process.env.BROWSER_CHANNEL });
    const at = "2026-09-06T10:00:00Z", A = "console:architecture-a", B = "console:architecture-b";
    const token = "architecture-fixture-token";
    const project = { id: "scratch", node: "test-node", path: "/test/scratch", repo: "inplace", level: "public", agents: [], workspaces: [] };
    async function eventually(predicate, label) {
        for (let i = 0; i < 100; i++) { if (await predicate()) return; await new Promise((resolve) => setTimeout(resolve, 20)); }
        assert.fail(label);
    }
    async function fixture() {
        const context = await browser.newContext({ viewport: { width: 1600, height: 1000 }, serviceWorkers: "block" });
        const page = await context.newPage(); page.setDefaultTimeout(3500);
        const f = { page, context, queue: [], posts: [], stopCalls: [], submissionKeys: true, queueReadError: 0, queueGate: null, inbox: [], stopReset: false, resetAfterAccept: false, hideQueue: false, reject: 0, stopGate: null, releases: [], errors: [], historyReads: 0, contextReads: [], held: null, agent: "first-agent", writes: [], settings: { ...original, mcp_servers: { sample: { type: "stdio", command: "sample", env: {}, headers: {} } } } };
        page.on("pageerror", (error) => f.errors.push(String(error)));
        await page.clock.install();
        await page.route("**/*", async (route) => {
            const req = route.request(), url = new URL(req.url());
            if (url.origin !== app.url) { f.errors.push(`Unexpected external request ${url.origin}`); return route.abort(); }
            if (!["/state", "/events", "/history"].includes(url.pathname) && !url.pathname.startsWith("/console/")) return route.continue();
            assert.equal(url.searchParams.has("token"), false, "Normal API requests must not expose authentication in the URL");
            assert.equal(req.headers().authorization, `Bearer ${token}`, "Every normal API call must use Authorization");
            const channel = url.searchParams.get("conversation") || A;
            if (url.pathname === "/state") return route.fulfill({ json: { at, hub: { node: "test-node", version: "test", started: at }, nodes: [{ name: "test-node", role: "hub", up: true }], agents: [], tasks: [], plans: [], projects: [project], inbox: f.inbox, attempts: [], landings: [] } });
            if (url.pathname === "/console/context") {
                const agent = channel === B ? "agent-b" : f.agent;
                f.contextReads.push(channel);
                if (f.held && channel === A) { const hold = f.held; f.held = null; await hold.promise; }
                return route.fulfill({ json: { enabled: true, context: { conversation: channel, project: { ...project, bound: true }, agent: { id: agent, model: "test", ready: true }, agents: [] } } }).catch(() => {});
            }
            if (url.pathname === "/console/conversations") return route.fulfill({ json: { enabled: true, conversations: [A, B].map((id) => ({ id, title: id === A ? "Architecture A" : "Architecture B", project: "scratch", count: 1, last_at: at, running: false })) } });
            if (url.pathname === "/console/replies") return route.fulfill({ json: { enabled: true, replies: [] } });
            if (url.pathname === "/console/queue") {
                if (req.method() === "POST") {
                    const input = req.postDataJSON(); f.posts.push(input);
                    if (f.reject) return route.fulfill({ status: f.reject, json: { error: f.reject === 409 ? "Command conflict" : "Submission rejected" } });
                    let entry = f.queue.find((entry) => entry.conversation === input.conversation && entry.key === `client:${input.command_id}`);
                    if (!entry) { entry = { id: `exchange-${f.queue.length + 1}`, conversation: input.conversation, input: input.input, key: `client:${input.command_id}`, state: "queued", enqueued_at: at }; f.queue.push(entry); }
                    if (f.resetAfterAccept) { f.resetAfterAccept = false; return route.abort("connectionreset"); }
                    return route.fulfill({ json: entry });
                }
                if (f.queueGate) await f.queueGate.promise;
                if (f.queueReadError) return route.fulfill({ status: f.queueReadError, json: { error: "Capability unavailable" } });
                return route.fulfill({ json: { queue: f.hideQueue ? [] : f.queue.filter((entry) => entry.conversation === channel), ...(f.submissionKeys ? { submission_keys: true } : {}) } });
            }
            if (url.pathname === "/console/send") {
                const input = req.postDataJSON(); f.stopCalls.push(input);
                if (f.stopGate) await f.stopGate.promise;
                for (const entry of f.queue) if (entry.conversation === input.conversation) entry.state = "cancelled";
                if (f.stopReset) { f.stopReset = false; return route.abort("connectionreset"); }
                return route.fulfill({ json: { reply: { conversation: input.conversation, kind: "reply", at, text: "Stopped" } } });
            }
            if (url.pathname === "/console/verbs") return route.fulfill({ json: { verbs: [] } });
            if (url.pathname === "/console/suggest") return route.fulfill({ json: { suggestions: [] } });
            if (url.pathname === "/console/nodes/test-node/settings") {
                if (req.method() === "PUT") { f.settings = req.postDataJSON(); f.writes.push(f.settings); }
                return route.fulfill({ json: { settings: f.settings } });
            }
            if (url.pathname === "/history") { f.historyReads++; return route.fulfill({ json: { entries: [], next: 0 } }); }
            f.errors.push(`Unmocked API ${url.pathname}`);
            return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
        });
        await page.addInitScript(({ conversation }) => {
            sessionStorage.setItem("steve.conversation", conversation);
            window.sources = [];
            window.EventSource = class {
                constructor(url) { this.url = url; window.sources.push(this); setTimeout(() => this.onopen?.(), 0); }
                close() { window.sources = window.sources.filter((source) => source !== this); }
            };
            window.emit = (event) => window.sources.forEach((source) => source.onmessage?.({ data: JSON.stringify(event) }));
        }, { conversation: A });
        await page.goto(`${app.url}/?token=${token}#/console`);
        await page.getByRole("button", { name: "Agent", exact: true }).getByText("first-agent").waitFor();
        await new Promise((resolve) => setTimeout(resolve, 100));
        assert.match(await page.evaluate(() => window.sources[0].url), /token=architecture-fixture-token/, "EventSource retains query authentication");
        f.emit = (event) => page.evaluate((event) => window.emit(event), { at, conversation: A, ...event });
        return f;
    }
    const cases = {
        async "quarantined-writer-inbox"(f) {
            f.inbox = [{ id: "writer:attempt-isolated", type: "writer", source: "attempt-isolated", attempt_id: "attempt-isolated", node: "worker-quarantined", workspace: "/work/isolated-project", project_id: "scratch", task_id: "2", summary: "原执行进程是否退出尚未确认，目录与执行资源继续保留占用", created_at: at, resolvable: false, choices: [] }];
            await f.page.locator('a[href="#/inbox"]').click(); await f.page.reload();
            await f.page.getByText("隔离执行待核实", { exact: true }).waitFor();
            await f.page.getByText("worker-quarantined", { exact: true }).waitFor();
            await f.page.getByText("/work/isolated-project", { exact: true }).waitFor();
            await f.page.getByText("attempt-isolated", { exact: true }).waitFor();
            const writer = f.page.getByRole("listitem").filter({ hasText: "attempt-isolated" });
            assert.match(await writer.innerText(), /需运维.*核实原进程已退出/);
            assert.equal(await writer.getByRole("button").count(), 0, "Writer quarantine has no one-click settlement or effects action");
            assert.equal(await f.page.getByText("暂无待处理请求", { exact: true }).count(), 0, "Non-resolvable writers remain visible in the inbox");
            assert.equal(f.posts.length + f.stopCalls.length + f.writes.length, 0, "Reading quarantine must not perform a write");
        },
        async "uncertain-retry-rejection"(f) {
            const box = f.page.getByRole("textbox", { name: "Message", exact: true });
            f.hideQueue = true; f.resetAfterAccept = true;
            await box.fill("One durable operation"); await box.press("Enter");
            const retry = f.page.getByRole("button", { name: "重试这次发送", exact: true });
            await retry.waitFor(); await box.fill("Newer unsent work");
            const id = f.posts[0].command_id;
            f.queueReadError = 503;
            await retry.click();
            await f.page.getByText("无法确认 Hub 是否支持安全提交，当前仅供查看。", { exact: true }).waitFor();
            await retry.waitFor();
            const retained = await f.page.evaluate(() => JSON.parse(sessionStorage.getItem("steve.console.drafts")));
            assert.equal(retained.submissions[A].id, id, "A failed retry preflight must retain the original possibly accepted key");
            assert.equal(retained.drafts[A], "Newer unsent work");
            assert.equal(f.posts.length, 1, "A failed preflight sends no retry POST");
            f.queueReadError = 0;
            await f.page.getByRole("button", { name: "重新检查", exact: true }).click();
            await eventually(() => box.isEnabled(), "Capability recovers");
            f.reject = 401; await retry.click();
            await eventually(() => f.posts.length === 2, "The server can reject authentication on a retry");
            await retry.waitFor();
            const rejectedRetry = await f.page.evaluate(() => JSON.parse(sessionStorage.getItem("steve.console.drafts")));
            assert.equal(rejectedRetry.submissions[A].id, id, "A retry rejection cannot prove the original operation was never accepted");
            assert.equal(await box.inputValue(), "Newer unsent work");
            f.reject = 0; await retry.click();
            await eventually(() => f.posts.length === 3, "A later retry still uses the original operation");
            assert.ok(f.posts.every((post) => post.command_id === id));
            assert.equal(f.queue.length, 1, "Lost receipt plus rejected retries still produce only one logical work item");
        },
        async "uncertain-stop-preflight"(f) {
            f.queue.push({ id: "running", conversation: A, state: "running", input: "Long work", started_at: at, enqueued_at: at });
            await f.emit({ kind: "console.queue" });
            f.stopReset = true;
            await f.page.getByRole("button", { name: "停止", exact: true }).click();
            const retry = f.page.getByRole("button", { name: "重试停止", exact: true });
            await retry.waitFor();
            const id = f.stopCalls[0].command_id;
            f.queueReadError = 503; await retry.click();
            await f.page.getByText("无法确认 Hub 是否支持安全提交，当前仅供查看。", { exact: true }).waitFor();
            await retry.waitFor();
            assert.equal(f.stopCalls.length, 1, "Retry preflight did not send another stop");
            f.queueReadError = 0; await retry.click();
            await eventually(() => f.stopCalls.length === 2, "Stop can be retried after capability recovers");
            assert.ok(f.stopCalls.every((call) => call.command_id === id), "Unknown stop keeps its identity through a preflight failure");
        },
        async "intent-capability-pending"(f) {
            f.inbox = [{ id: "approval", type: "effect", summary: "Fixture approval", created_at: at, resolvable: true, choices: [{ command: "/audit confirm", label: "确认测试" }] }];
            await f.page.locator('a[href="#/inbox"]').click();
            await f.page.reload();
            const held = gate(); f.queueGate = held; f.releases.push(held.resolve);
            await f.page.getByRole("button", { name: "确认测试", exact: true }).click();
            await f.page.getByRole("button", { name: "Agent", exact: true }).getByText("first-agent").waitFor();
            assert.equal(f.posts.length, 0, "An intent waits until capability is confirmed");
            held.resolve();
            await eventually(() => f.posts.length === 1, "The retained intent executes after capability becomes available");
            assert.equal(f.posts[0].input, "/audit confirm");
        },
        async "old-hub-read-only"(f) {
            f.submissionKeys = false;
            await f.page.reload();
            const box = f.page.getByRole("textbox", { name: "Message", exact: true });
            await box.waitFor();
            await f.page.getByText("Hub 需更新后才能发送指令。当前仍可查看会话和执行记录。", { exact: true }).waitFor();
            assert.equal(await box.isDisabled(), true, "An old hub must remain read-only");
            await f.page.getByRole("button", { name: "新会话", exact: true }).click();
            await eventually(() => f.page.getByRole("button", { name: "新会话", exact: true }).isEnabled(), "Programmatic creation must finish without writing");
            f.queue.push({ id: "running", conversation: A, state: "running", input: "Old running", started_at: at, enqueued_at: at }, { id: "queued", conversation: A, state: "queued", input: "Old queued", enqueued_at: at });
            await f.emit({ kind: "console.queue" });
            await f.page.getByRole("button", { name: "停止", exact: true }).click();
            await eventually(() => f.page.getByRole("button", { name: "停止", exact: true }).isEnabled(), "Stop preflight rejects an old hub");
            await f.page.getByRole("listitem").filter({ hasText: "Old queued" }).getByRole("button", { name: "更多", exact: true }).click();
            await f.page.getByRole("menuitem", { name: "在新线程里问", exact: true }).click();
            await tick();
            assert.equal(f.posts.length, 0, "No enqueue POST reaches an old hub");
            assert.equal(f.stopCalls.length, 0, "No send POST, including newSession and stop, reaches an old hub");
            assert.equal(f.errors.length, 0, "Side chat must stop before deleting its old queued item");
        },
        async "capability-failure-retry"(f) {
            f.queueReadError = 503;
            await f.page.reload();
            const box = f.page.getByRole("textbox", { name: "Message", exact: true });
            await box.waitFor();
            await f.page.getByText("无法确认 Hub 是否支持安全提交，当前仅供查看。", { exact: true }).waitFor();
            assert.equal(await box.isDisabled(), true);
            f.queueReadError = 0;
            await f.page.getByRole("button", { name: "重新检查", exact: true }).click();
            await eventually(() => box.isEnabled(), "A successful capability recheck enables the composer");
            assert.equal(f.posts.length + f.stopCalls.length, 0, "Capability retry is a read");
        },
        async "uncertain-submission"(f) {
            const box = f.page.getByRole("textbox", { name: "Message", exact: true });
            f.resetAfterAccept = true; f.hideQueue = true;
            await box.fill("Perform once"); await box.press("Enter");
            await f.page.getByRole("button", { name: "重试这次发送", exact: true }).waitFor();
            await box.fill("Newer draft"); await box.press("Enter");
            assert.equal(f.posts.length, 1, "Uncertain receipt must block a fresh submission");
            const persisted = await f.page.evaluate(() => JSON.parse(sessionStorage.getItem("steve.console.drafts")));
            assert.equal(persisted.submissions[A].id, f.posts[0].command_id, "The operation identity is durable before sending");
            await f.page.getByRole("button", { name: "重试这次发送", exact: true }).click();
            await eventually(() => f.posts.length === 2, "Explicit retry sends the original operation");
            assert.equal(f.posts[1].command_id, f.posts[0].command_id);
            assert.equal(f.queue.length, 1, "The server receives one logical operation despite a lost receipt");
            assert.equal(await box.inputValue(), "Newer draft");
            await eventually(async () => await f.page.getByRole("button", { name: "重试这次发送", exact: true }).count() === 0, "Acknowledgement clears uncertainty");
        },
        async "submission-reload-reconciliation"(f) {
            f.resetAfterAccept = true; f.hideQueue = true;
            const box = f.page.getByRole("textbox", { name: "Message", exact: true });
            await box.fill("Accepted before reload"); await box.press("Enter");
            await f.page.getByRole("button", { name: "重试这次发送", exact: true }).waitFor();
            await box.fill("Draft survives reconciliation");
            f.hideQueue = false; await f.page.reload();
            await box.waitFor();
            await eventually(async () => await f.page.getByRole("button", { name: "重试这次发送", exact: true }).count() === 0, "Reload reconciles the original operation against durable queue keys");
            assert.equal(f.posts.length, 1, "Reconciliation must not send again");
            assert.equal(await box.inputValue(), "Draft survives reconciliation");
        },
        async "submission-rejection-conflict"(f) {
            const box = f.page.getByRole("textbox", { name: "Message", exact: true });
            f.reject = 400;
            await box.fill("Rejected work"); await box.press("Enter");
            await eventually(async () => await box.inputValue() === "Rejected work", "Explicit rejection restores the draft");
            f.reject = 409; await box.press("Enter");
            await f.page.getByText("这次发送的标识与服务器记录冲突，请核对会话记录，不要直接重新发送。", { exact: true }).waitFor();
            assert.equal(await f.page.getByRole("button", { name: "重试这次发送", exact: true }).count(), 0);
            await box.fill("Newer draft after conflict"); await box.press("Enter");
            assert.equal(f.posts.length, 2, "A conflict cannot silently generate a replacement key");
        },
        async "stop-remount"(f) {
            f.queue.push({ id: "running", conversation: A, state: "running", input: "Long task", started_at: at, enqueued_at: at });
            await f.emit({ kind: "console.queue" });
            const held = gate(); f.stopGate = held; f.releases.push(held.resolve);
            await f.page.getByRole("button", { name: "停止", exact: true }).click();
            await eventually(() => f.stopCalls.length === 1, "First stop request starts");
            await f.page.locator('a[href="#/projects"]').click();
            await f.page.getByRole("button", { name: "添加项目", exact: true }).waitFor();
            await f.page.locator('a[href="#/console"]').click();
            const stopping = f.page.getByRole("button", { name: "正在停止", exact: true });
            await stopping.waitFor(); assert.equal(await stopping.isDisabled(), true, "Shared stop guard survives route remount");
            assert.equal(f.stopCalls.length, 1);
            held.resolve();
            await f.page.getByRole("status").getByText("Stopped", { exact: true }).waitFor();
            await f.emit({ kind: "console.queue" });
            await eventually(async () => await f.page.getByRole("button", { name: "正在停止", exact: true }).count() === 0, "The remounted UI observes stop completion");
        },
        async "context-generation"(f) {
            const held = gate(); f.held = held;
            const before = f.contextReads.length;
            await f.emit({ kind: "console.reply", reply_id: "request-one", text: "One" });
            await eventually(() => f.contextReads.length > before, "First context read starts");
            f.agent = "new-agent";
            await f.emit({ kind: "console.reply", reply_id: "request-two", text: "Two" });
            await tick();
            assert.equal(f.contextReads.length, before + 1, "Two refresh triggers must share the in-flight context read");
            held.resolve();
            await f.page.getByRole("button", { name: "Agent", exact: true }).getByText("new-agent").waitFor();
            const stale = gate(); f.held = stale;
            await f.emit({ kind: "console.reply", reply_id: "request-three", text: "Three" });
            await eventually(() => f.held === null, "A read is outstanding before changing conversation");
            await f.page.getByRole("button", { name: /^Architecture B/ }).click();
            await f.page.getByRole("button", { name: "Agent", exact: true }).getByText("agent-b").waitFor();
            stale.resolve(); await tick();
            assert.match(await f.page.getByRole("button", { name: "Agent", exact: true }).innerText(), /agent-b/, "Old resource responses cannot affect the selected conversation");
        },
        async "configuration-input"(f) {
            await f.page.locator('a[href="#/fleet"]').click();
            await f.page.locator('[aria-label="Nodes"]').getByRole("row").filter({ hasText: "test-node" }).click();
            await f.page.getByRole("button", { name: "编辑配置", exact: true }).click();
            const env = f.page.getByRole("textbox", { name: "环境变量", exact: true });
            await env.pressSequentially("FOO=bar");
            assert.equal(await env.inputValue(), "FOO=bar", "Typing an incomplete key must not erase the draft");
            await env.fill("FOO=bar\nFOO=other");
            await f.page.getByRole("button", { name: "保存到这台机器", exact: true }).click();
            await f.page.getByRole("alert").getByText(/名称重复/).waitFor();
            assert.equal(f.writes.length, 0, "Invalid configuration must not reach the API");
            assert.equal(await env.inputValue(), "FOO=bar\nFOO=other");
            await env.fill("FOO=bar");
            await f.page.getByRole("button", { name: "保存到这台机器", exact: true }).click();
            await eventually(() => f.writes.length === 1, "A valid configuration saves once");
            assert.deepEqual(f.writes[0].harnesses.sample.args, original.harnesses.sample.args, "Existing argv values must survive unrelated edits");
            assert.deepEqual(f.writes[0].mcp_servers.sample.env, { FOO: "bar" });
        },
        async "history-buffer"(f) {
            await f.page.locator('a[href="#/history"]').click();
            await eventually(() => f.historyReads > 0, "Initial history read");
            await f.page.evaluate(() => { for (let i = 0; i < 300; i++) window.emit({ kind: "node.connected", at: "2026-09-06T10:00:00Z", detail: String(i) }); });
            await tick(); await tick();
            const before = f.historyReads;
            await f.emit({ kind: "node.connected", detail: "301" });
            await eventually(() => f.historyReads > before, "The 301st event must still refresh history");
        },
    };
    try {
        for (const [name, check] of Object.entries(cases)) {
            const f = await fixture();
            try { await check(f); assert.deepEqual(f.errors, []); console.log(`PASS ${name}`); }
            finally { f.held?.resolve(); for (const release of f.releases) release(); await f.context.close(); }
        }
    } finally { await browser.close(); app.close(); }
}
