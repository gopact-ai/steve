// Source preview only. All APIs are intercepted; no live Hub or embedded dist.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";

const root = fileURLToPath(new URL("../../web/console", import.meta.url));
const server = await createServer({ root, configFile: path.join(root, "vite.config.ts"), server: { host: "127.0.0.1", port: 0, hmr: false }, logLevel: "error" });
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
const output = process.env.OUTPUT_DIR || path.join(os.tmpdir(), "steve-changes-card");
await mkdir(output, { recursive: true });
const browser = await chromium.launch({ headless: process.env.HEADED !== "1", channel: process.env.BROWSER_CHANNEL });
const conversation = "console:changes-card", at = "2026-09-07T01:00:00Z", base = "a".repeat(40), artifact = "b".repeat(40);
const files = [
    { path: "src/app.tsx", status: "M", added: 6, deleted: 2 },
    { path: "src/components/feature-with-a-very-long-name/long-component-file-name.tsx", status: "A", added: 4, deleted: 0 },
    { path: "assets/screenshot.png", status: "A", added: 0, deleted: 0, binary: true },
    { path: "src/obsolete.ts", status: "D", added: 0, deleted: 3 },
    { path: "tests/feature.test.ts", status: "A", added: 2, deleted: 0 },
    { path: "bin/tool", status: "T", added: 0, deleted: 1 },
];
const summary = (attempt = "history-attempt") => ({ attempt, base, artifact, project: "p", files: files.length, added: 12, deleted: 6, binary_files: 1 });
const reply = (changes = summary(), text = "Completed this historical change.") => ({ id: `reply-${changes.attempt}`, conversation, kind: "reply", at, text, changes });

async function fixture(replies, options = {}) {
    const context = await browser.newContext({ viewport: { width: 1480, height: 960 }, serviceWorkers: "block", reducedMotion: "reduce" });
    const page = await context.newPage(); page.setDefaultTimeout(7000);
    const reads = [], writes = [], errors = [];
    page.on("pageerror", (error) => errors.push(String(error)));
    await page.addInitScript((conversation) => {
        sessionStorage.setItem("steve.conversation", conversation);
        localStorage.setItem("steve.ui.locale", "en");
        window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
    }, conversation);
    await page.route("**/*", async (route) => {
        const request = route.request(), url = new URL(request.url()), name = url.pathname;
        if (url.origin !== origin) { errors.push(`Unexpected external request ${url.origin}`); return route.abort(); }
        if (options.failReview && name.endsWith("/review-workspace.tsx")) return route.abort();
        if (!name.startsWith("/console/") && !["/state", "/events"].includes(name)) return route.continue();
        if (request.method() !== "GET") { writes.push(name); return route.fulfill({ status: 500, json: { error: "Unexpected mutation" } }); }
        if (name === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false } });
        if (name === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false, agent_count: 0 } });
        if (name === "/state") return route.fulfill({ json: { at, hub: { node: "fixture-hub", version: "test" }, nodes: [], agents: [], projects: [{ id: "p", node: "fixture-hub", path: "/work/p", level: "public", repo: "inplace", agents: [], workspaces: [] }], tasks: [{ id: "active-task", channel: conversation, project_id: "p", goal: "Unrelated current task", state: "running", lifecycle: "running", execution: "idle", lane: "pending", attention: 0, turns: 1, updated_at: at }], plans: [], attempts: [], landings: [] } });
        if (name === "/console/context") return route.fulfill({ json: { enabled: true, context: { conversation, project: { id: "p", node: "fixture-hub", path: "/work/p", level: "public", repo: "inplace", bound: true }, agents: [] } } });
        if (name === "/console/replies") return route.fulfill({ json: { enabled: true, replies } });
        if (name === "/console/conversations") return route.fulfill({ json: { enabled: true, conversations: [{ id: conversation, title: "Changes conversation", project: "p", count: replies.length, last_at: at, running: false }] } });
        if (name === "/console/queue") return route.fulfill({ json: { queue: [], submission_keys: true } });
        if (name === "/console/questions") return route.fulfill({ json: { questions: [] } });
        if (name === "/console/verbs") return route.fulfill({ json: { verbs: [] } });
        if (name === "/console/suggest") return route.fulfill({ json: { suggestions: [] } });
        if (name === "/console/tasks/active-task/attempts") return route.fulfill({ json: [{ id: "active-current", kind: "task", state: "running", base, artifact, started_at: at }] });
        const match = name.match(/^\/console\/attempts\/([^/]+)\/(changes|diff|tree|file)$/);
        if (match) {
            const [, attempt, type] = match;
            reads.push({ attempt, type, path: url.searchParams.get("path") });
            if (type === "changes") {
                if (options.failFirst && reads.filter((r) => r.type === "changes").length === 1) return route.fulfill({ status: 503, json: { error: "Index temporarily unavailable" } });
                await new Promise((resolve) => setTimeout(resolve, options.delay || 30));
                return route.fulfill({ json: { attempt, project: "p", base, artifact: options.changedSnapshot ? "c".repeat(40) : artifact, changes: options.empty ? null : options.truncated ? files.slice(0, 2) : files, truncated: !!options.truncated } });
            }
            if (type === "diff") return route.fulfill({ json: { path: url.searchParams.get("path"), diff: "--- a/app.tsx\n+++ b/app.tsx\n@@ -1 +1 @@\n-before\n+historical result\n" } });
            if (type === "tree") return route.fulfill({ json: { attempt, commit: artifact, which: "result", dir: "", entries: files.map((file) => ({ name: file.path, path: file.path, kind: "file", size: 10 })) } });
            return route.fulfill({ json: { attempt, commit: artifact, path: url.searchParams.get("path"), text: "historical source", size: 17 } });
        }
        errors.push(`Unhandled API ${name}`);
        return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
    });
    await page.goto(`${origin}/#/console`);
    await page.getByRole("heading", { name: "Changes conversation", exact: true }).waitFor();
    return { page, context, reads, writes, errors, card: page.getByRole("region", { name: "File changes", exact: true }), async close() { assert.deepEqual(writes, []); assert.deepEqual(errors, []); await context.close(); } };
}

try {
    {
        const f = await fixture([reply()]); const { page, card } = f;
        await card.locator(".changes-card-file").nth(2).waitFor();
        assert.equal(await card.locator(".changes-card-file").count(), 3);
        await card.getByLabel("12 lines added, 6 lines deleted", { exact: true }).waitFor();
        const message = page.locator(".message-assistant").filter({ hasText: "Completed this historical change." });
        assert.ok(await message.evaluate((el) => {
            const text = [...el.querySelectorAll("p")].find((p) => p.textContent === "Completed this historical change.");
            return !!text && !!(text.compareDocumentPosition(el.querySelector(".changes-card")) & Node.DOCUMENT_POSITION_FOLLOWING);
        }), "Changes must follow their reply body");
        await card.getByRole("button", { name: "Show 3 more files", exact: true }).click();
        assert.equal(await card.locator(".changes-card-file").count(), 6);
        await card.getByRole("button", { name: "Show fewer files", exact: true }).click();
        assert.equal(await card.locator(".changes-card-file").count(), 3);
        await card.getByRole("button", { name: "Files changed: 6", exact: true }).focus();
        await page.keyboard.press("Enter");
        assert.equal(await card.locator(".changes-card-body").isVisible(), false);
        await page.keyboard.press("Space");
        assert.equal(await card.locator(".changes-card-body").isVisible(), true);
        assert.equal(f.reads.filter((r) => r.type === "changes").length, 1, "Folding never refetches the index");
        await card.getByRole("button", { name: `Review ${files[1].path}`, exact: true }).click();
        const review = page.getByRole("dialog", { name: "Code workspace", exact: true });
        await review.getByText("historical result", { exact: true }).waitFor();
        assert.ok(f.reads.some((r) => r.attempt === "history-attempt" && r.type === "diff" && r.path === files[1].path));
        assert.ok(f.reads.every((r) => r.attempt !== "active-current"), "Review cannot substitute the currently active task's attempt");
        await review.getByRole("button", { name: "Back", exact: true }).click();
        await card.getByRole("button", { name: "Review changes", exact: true }).click();
        await review.getByRole("table").waitFor();
        await review.getByRole("button", { name: "Back", exact: true }).click();
        await page.screenshot({ path: path.join(output, "changes-card-desktop.png") });
        await page.evaluate(() => document.documentElement.classList.add("dark-mode"));
        await page.screenshot({ path: path.join(output, "changes-card-dark.png") });
        await page.setViewportSize({ width: 390, height: 844 });
        await card.scrollIntoViewIfNeeded();
        assert.ok(await card.evaluate((el) => el.scrollWidth <= el.clientWidth + 1), "The card must fit a narrow conversation");
        assert.ok((await card.getByRole("button", { name: "Review changes", exact: true }).boundingBox()).width > 0);
        await page.screenshot({ path: path.join(output, "changes-card-mobile.png") });
        await f.close(); console.log("PASS reply position, first-three expansion, keyboard folding, historical Review and narrow layouts");
    }
    {
        const f = await fixture([reply({ ...summary(), files: 2, added: 10, deleted: 2, binary_files: 0, truncated: true })], { truncated: true });
        await f.card.getByText("Showing the first 2 entries.", { exact: true }).waitFor();
        await f.card.getByLabel("Listed files: 10 lines added, 2 lines deleted", { exact: true }).waitFor();
        await f.card.getByRole("button", { name: "Files changed: 2+", exact: true }).waitFor();
        await f.close(); console.log("PASS partial counts never claim a complete total");
    }
    {
        const f = await fixture([reply()], { failFirst: true });
        await f.card.getByRole("alert").waitFor();
        await f.card.getByRole("button", { name: "Retry", exact: true }).click();
        await f.card.locator(".changes-card-file").nth(2).waitFor();
        assert.equal(f.reads.filter((r) => r.type === "changes").length, 2);
        await f.close(); console.log("PASS failed index can retry");
    }
    {
        const f = await fixture([reply()], { changedSnapshot: true });
        await f.card.getByRole("alert").filter({ hasText: "snapshot changed" }).waitFor();
        assert.equal(await f.card.locator(".changes-card-file").count(), 0);
        await f.close(); console.log("PASS a changed snapshot cannot populate a historical card");
    }
    {
        const f = await fixture([reply({ attempt: "no-capture", files: 0, note: "Snapshot capture unavailable" }), reply({ attempt: "no-changes", files: 0 })]);
        assert.equal(await f.card.count(), 1);
        await f.card.getByText("Snapshot capture unavailable", { exact: true }).waitFor();
        assert.equal(await f.card.getByRole("button", { name: "Review changes", exact: true }).count(), 0);
        assert.equal(f.reads.filter((r) => r.type === "changes").length, 0);
        await f.close(); console.log("PASS absent capture and unchanged replies do not pretend a diff exists");
    }
    {
        const f = await fixture([reply()], { empty: true });
        await f.card.getByText("No file changes", { exact: true }).waitFor();
        await f.close(); console.log("PASS empty/null index is rendered without crashing");
    }
    {
        const history = Array.from({ length: 45 }, (_, i) => reply(summary(`history-${i}`), `Completed change ${i}.\n\n${"A retained explanation. ".repeat(24)}`));
        const f = await fixture(history);
        await f.page.locator('[data-attempt="history-44"] .changes-card-file').first().waitFor();
        await f.page.waitForTimeout(250);
        const fetched = new Set(f.reads.filter((r) => r.type === "changes").map((r) => r.attempt));
        assert.ok(fetched.size < 10, `Offscreen history must not issue a full set of index requests: ${fetched.size}`);
        assert.ok(!fetched.has("history-20"));
        await f.page.locator('[data-attempt="history-20"]').scrollIntoViewIfNeeded();
        await f.page.locator('[data-attempt="history-20"] .changes-card-file').first().waitFor();
        assert.ok(f.reads.some((r) => r.type === "changes" && r.attempt === "history-20"));
        await f.close(); console.log("PASS historical cards load only when they enter the visible conversation");
    }
    {
        const f = await fixture([reply()], {failReview:true});
        await f.page.getByRole("textbox",{name:"Message",exact:true}).fill("Keep this draft");
        await f.card.getByRole("button",{name:"Review changes",exact:true}).click();
        await f.page.getByRole("alert").getByText(/code workspace could not load/).waitFor();
        await f.page.getByRole("heading",{name:"Changes conversation",exact:true}).waitFor();
        assert.equal(await f.page.getByRole("textbox",{name:"Message",exact:true}).inputValue(),"Keep this draft");
        await f.page.getByRole("alert").getByRole("button",{name:"Back",exact:true}).click();
        await f.close();console.log("PASS Review module failure preserves the conversation and draft");
    }
} finally {
    await browser.close(); await server.close();
}
