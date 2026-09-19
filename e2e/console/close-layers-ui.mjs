// Closing takes one layer at a time. A reader with a file open inside a
// snapshot, a chat beside it and a details rail on the far right means
// "put this away" long before they mean "close the window", so the
// shortcut works inward-out and right-to-left and only reports that
// nothing was left once the screen is clear.
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const server = await createServer({ configFile: path.join(web, "vite.config.ts"), root: web, server: { host: "127.0.0.1", port: 0 } });
await server.listen();
const url = server.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const A = "console:close-layers", at = "2026-09-18T01:00:00Z", base = "a".repeat(40), after = "b".repeat(40);
const replyText = "A passage worth reading twice.";
const context = await browser.newContext({ viewport: { width: 1600, height: 1000 }, serviceWorkers: "block" });
const page = await context.newPage(); page.setDefaultTimeout(7000);
const agents = [{ id: "local-agent", node: "test-hub", harness: "mock", ready: true, usable: true }];
const sideContexts = new Map();
const f = { errors: [], posts: [] };
page.on("pageerror", (error) => f.errors.push(String(error)));
await page.route("**/*", async (route) => {
    const req = route.request(), u = new URL(req.url()), p = u.pathname;
    if (u.origin !== new URL(url).origin) { f.errors.push("external " + u.origin); return route.abort(); }
    if (!p.startsWith("/console/") && !["/state", "/events"].includes(p)) return route.continue();
    const input = req.method() === "GET" ? null : (req.headers()["content-type"] || "").includes("application/json") ? req.postDataJSON() : null;
    if (p === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false } });
    if (p === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false, agent_count: 0 } });
    if (p === "/state") return route.fulfill({ json: { at, hub: { node: "test-hub", version: "test" }, nodes: [], agents: [], projects: [{ id: "p", node: "test-hub", path: "/work/p", repo: "inplace", level: "public", agents: [], workspaces: [] }], tasks: [{ id: "11", channel: A, project_id: "p", goal: "Code task", state: "running", lifecycle: "running", execution: "idle", lane: "pending", attention: 0, turns: 1, max_turns: 10, updated_at: at }], plans: [], attempts: [], landings: [], conflicts: [], inbox: [] } });
    const initialization = p.match(/^\/console\/conversations\/([^/]+)\/initialize$/);
    if (initialization && req.method() === "PUT") { const conversation = decodeURIComponent(initialization[1]); sideContexts.set(conversation, { ...sideContexts.get(conversation), project: input.project }); return route.fulfill({ json: { ok: true } }); }
    if (p === "/console/send") { const current = sideContexts.get(input.conversation) || {}; if (input.input.startsWith("/use ")) sideContexts.set(input.conversation, { ...current, agent: "local-agent" }); return route.fulfill({ json: { reply: { id: "binding", conversation: input.conversation, text: "bound", at, kind: "reply" } } }); }
    if (p === "/console/context") { const conversation = u.searchParams.get("conversation"), sideContext = sideContexts.get(conversation), project = conversation === A ? "p" : sideContext?.project; return route.fulfill({ json: { enabled: true, context: { conversation, ...(project ? { project: { id: project, node: "test-hub", path: "/work/p", repo: "inplace", level: "public", bound: true } } : {}), agent: agents[0], agents } } }); }
    if (p === "/console/replies" && u.searchParams.get("conversation") !== A) return route.fulfill({ json: { enabled: true, replies: [] } });
    if (p === "/console/replies") return route.fulfill({ json: { enabled: true, replies: [{ id: "r1", conversation: A, kind: "reply", at, text: replyText, project_id: "p", revision: "reply-version-1", changes: { attempt: "attempt1", project: "p", base, artifact: after, files: 2, added: 2, deleted: 1 } }] } });
    if (p === "/console/conversations") return route.fulfill({ json: { conversations: [{ id: A, title: "Layered conversation", project: "p", count: 1, last_at: at, running: false }] } });
    if (p === "/console/verbs") return route.fulfill({ json: { verbs: [] } });
    if (p === "/console/suggest") return route.fulfill({ json: { suggestions: [] } });
    if (p === "/console/queue") { if (req.method() === "POST") { f.posts.push(input); return route.fulfill({ json: { id: "e1", conversation: input.conversation, key: "client:" + input.command_id, input: input.input, state: "queued", enqueued_at: at } }); } return route.fulfill({ json: { queue: [], submission_keys: true, material_refs: true, interactive_requests: true } }); }
    if (p === "/console/setup") return route.fulfill({ json: { enabled: true, setup: { agent: "local-agent", node: "test-hub", harness: "mock", applied: true, instructions: "", sections: [], mcp_servers: [] } } });
    if (p === "/console/materials/capture") return route.fulfill({ json: { id: "m1", project: "p", title: "Reference reply", source: input.source, kind: "text", mime: "text/plain", size: replyText.length, digest: "d".repeat(64), created_at: at } });
    if (p.match(/^\/console\/materials\/[^/]+\/content$/)) return route.fulfill({ contentType: "text/plain", body: replyText });
    if (p.match(/^\/console\/materials\/[^/]+$/)) return route.fulfill({ json: { id: "m1", project: "p", title: "Reference reply", source: { kind: "reply" }, kind: "text", mime: "text/plain", size: replyText.length, digest: "d".repeat(64), created_at: at } });
    if (p === "/console/materials") return route.fulfill({ json: { materials: [] } });
    if (p === "/console/annotations") return route.fulfill({ json: { annotations: [] } });
    if (p === "/console/questions") return route.fulfill({ json: { questions: [] } });
    if (p === "/console/tasks/11/attempts") return route.fulfill({ json: [{ id: "attempt1", kind: "task", state: "done", base, artifact: after, started_at: at, files: 2 }] });
    if (p === "/console/attempts/attempt1/changes") return route.fulfill({ json: { attempt: "attempt1", project: "p", base, artifact: after, changes: [{ path: "app.ts", status: "M", added: 2, deleted: 1 }, { path: "docs/notes.md", status: "M", added: 1, deleted: 0 }] } });
    if (p === "/console/attempts/attempt1/tree") return route.fulfill({ json: { attempt: "attempt1", commit: after, which: "result", dir: "", entries: [{ name: "app.ts", path: "app.ts", kind: "file", size: 20 }] } });
    if (p === "/console/attempts/attempt1/file") return route.fulfill({ json: { attempt: "attempt1", commit: after, path: u.searchParams.get("path") || "app.ts", text: "first\nsecond\nthird\n", size: 20 } });
    if (p === "/console/attempts/attempt1/diff") return route.fulfill({ json: { path: u.searchParams.get("path") || "app.ts", diff: "--- a/x\n+++ b/x\n@@ -1,2 +1,3 @@\n-old\n+first\n+second\n third\n" } });
    f.errors.push(req.method() + " " + p); return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
});
await page.addInitScript((conversation) => { sessionStorage.setItem("steve.conversation", conversation); localStorage.setItem("steve.ui.locale", "en"); window.sources = []; window.EventSource = class { constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); } close() { window.sources = window.sources.filter((s) => s !== this); } }; }, A);

async function select(locator, needle) {
    await locator.evaluate((element, text) => {
        const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT); const nodes = []; let node;
        while (node = walker.nextNode()) { if (node.parentElement.closest("button,[data-selection-ignore],.selection-toolbar")) continue; nodes.push(node); }
        const joined = nodes.map((n) => n.textContent).join(""); const start = joined.indexOf(text); if (start < 0) throw new Error("Missing text " + text + " in " + joined);
        let offset = 0, first, last;
        for (const n of nodes) { const end = offset + n.textContent.length; if (first === undefined && start < end) first = [n, start - offset]; if (start + text.length <= end && first) { last = [n, start + text.length - offset]; break; } offset = end; }
        const range = document.createRange(); range.setStart(...first); range.setEnd(...last);
        const selection = window.getSelection(); selection.removeAllRanges(); selection.addRange(range); document.dispatchEvent(new Event("selectionchange"));
    }, needle);
    await page.getByRole("toolbar", { name: "Selection actions" }).waitFor();
}

/** What the macOS shell asks the page on Cmd+W: did anything close? */
const closeOne = () => page.evaluate(() => Boolean(window.steveCloseLayer?.()));
const dialog = page.getByRole("dialog", { name: "Artifact workspace", exact: true });

try {
    await page.goto(url + "#/console");
    await page.getByRole("heading", { name: "Layered conversation", exact: true }).waitFor();

    // Nothing open yet, so the shortcut hands the window back to the shell.
    assert.equal(await closeOne(), false, "an empty workbench must let the window close");

    // A chat opens in the pane beside the conversation, and the details
    // rail stands further right still, so the rail goes first.
    await select(page.locator(".message-assistant [data-selection-surface]").first(), "passage worth reading");
    await page.getByRole("toolbar", { name: "Selection actions" }).getByRole("button", { name: "Ask in side chat", exact: true }).click();
    const split = page.locator(".split-pane");
    await split.waitFor();
    await page.getByRole("button", { name: "Show details", exact: true }).click();
    await page.locator(".console-content.with-inspector").waitFor();

    assert.equal(await closeOne(), true, "the details rail closes first");
    await page.locator(".console-content.with-inspector").waitFor({ state: "detached" });
    await split.waitFor();
    assert.equal(await closeOne(), true, "the pane beside the conversation closes next");
    await split.waitFor({ state: "detached" });
    assert.equal(await closeOne(), false, "a cleared workbench lets the window close again");
    console.log("PASS console panels close right to left before the window");

    await page.getByRole("button", { name: /Review changes/ }).first().click();
    await dialog.waitFor();
    // Two files open; the reader is on the second.
    await dialog.locator('.review-file[aria-label="app.ts"]').click();
    await page.locator(".code-open-tab").first().waitFor();
    await dialog.locator('.review-file[aria-label="docs/notes.md"]').click();
    await page.waitForFunction(() => document.querySelectorAll(".code-open-tab").length === 2);

    assert.equal(await closeOne(), true, "the open file closes before the workspace");
    await page.waitForFunction(() => document.querySelectorAll(".code-open-tab").length === 1);
    await dialog.waitFor();
    assert.equal(await closeOne(), true, "the last file closes before the workspace");
    await page.waitForFunction(() => document.querySelectorAll(".code-open-tab").length === 0);
    await dialog.waitFor();
    assert.equal(await closeOne(), true, "the workspace closes once its files are away");
    await dialog.waitFor({ state: "detached" });
    assert.equal(await closeOne(), false, "closing the workspace returns the window to the shell");
    console.log("PASS the artifact workspace closes its files before itself");

    // Ctrl+W and Cmd+W are the same request, and a dialog on top answers
    // first no matter what is underneath it.
    await page.getByRole("button", { name: /Review changes/ }).first().click();
    await dialog.waitFor();
    await page.keyboard.press("Control+w");
    await page.waitForFunction(() => document.querySelectorAll(".code-open-tab").length === 0);
    await page.keyboard.press("Meta+w");
    await dialog.waitFor({ state: "detached" });
    console.log("PASS the keyboard reaches the same layers as the window menu");

    assert.deepEqual(f.errors, [], "no page errors or unmocked calls");
} finally {
    await browser.close();
    await server.close();
}
