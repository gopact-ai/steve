// Deleting is reachable and asks once: a project row and a conversation
// row each offer a delete that states what goes with it, and only the
// confirmed dialog issues the request.
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const server = await createServer({ configFile: path.join(web, "vite.config.ts"), root: web, server: { host: "127.0.0.1", port: 0 } }); await server.listen();
const url = server.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
const page = await context.newPage(); page.setDefaultTimeout(7000);
const at = "2026-09-16T00:00:00Z";
const nodes = [{ name: "node-mac", display_name: "My Mac", role: "hub", up: true, version: "test", projects_root: "/Users/me/Steve/projects" }];
const projects = [
    { id: "workspace", node: "node-mac", path: "/Users/me/Steve", default: true, level: "internal", repo: "inplace", agents: ["steve"], workspaces: [{ id: "w1", node: "node-mac", path: "/Users/me/Steve", kind: "canonical", agents: ["steve"] }] },
    { id: "redis-platform", node: "node-mac", path: "/Users/me/Steve/projects/redis-platform", level: "internal", repo: "inplace", agents: ["steve"], workspaces: [{ id: "w2", node: "node-mac", path: "/Users/me/Steve/projects/redis-platform", kind: "canonical", agents: ["steve"] }] },
];
const tasks = [{ id: "7", goal: "ship it", state: "running", member: "steve", channel: "console:one", project_id: "redis-platform", lane: "active" }];
const conversations = [
    { id: "console:one", title: "the doomed thread", project: "redis-platform", agent: "steve", last_at: at, count: 4, running: false },
    { id: "console:two", title: "another thread", project: "workspace", agent: "steve", last_at: at, count: 2, running: false },
];
const f = { deletedProjects: [], deletedThreads: [], errors: [] };
page.on("pageerror", (error) => f.errors.push(String(error)));
await page.addInitScript(() => {
    localStorage.setItem("steve.ui.locale", "en");
    window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
});
await page.route("**/*", async (route) => {
    const req = route.request(), u = new URL(req.url()), p = decodeURIComponent(u.pathname);
    if (u.origin !== new URL(url).origin) { f.errors.push("external " + u.origin); return route.abort(); }
    if (!p.startsWith("/console/") && !["/state", "/events"].includes(p)) return route.continue();
    if (p === "/state") return route.fulfill({ json: { at, hub: { node: "node-mac", version: "test" }, nodes, agents: [], projects, tasks, plans: [], attempts: [], landings: [], facts: { grants: [] } } });
    if (p === "/console/conversations") return route.fulfill({ json: { enabled: true, conversations } });
    if (p === "/console/queue") return route.fulfill({ json: { queue: [], submission_keys: true } });
    if (p === "/console/replies") return route.fulfill({ json: { enabled: true, replies: [], conversations: conversations.map((c) => c.id) } });
    if (p === "/console/context") return route.fulfill({ json: { enabled: true, context: { conversation: "console:one", project: { id: "redis-platform", bound: true }, agent: { id: "steve" } } } });
    if (p === "/console/verbs") return route.fulfill({ json: { verbs: [] } });
    if (p.startsWith("/console/projects/") && req.method() === "DELETE") { f.deletedProjects.push(p.slice("/console/projects/".length)); return route.fulfill({ json: { ok: true } }); }
    if (p.startsWith("/console/conversations/") && req.method() === "DELETE") { f.deletedThreads.push(p.slice("/console/conversations/".length)); return route.fulfill({ json: { ok: true } }); }
    return route.continue();
});
try {
    await page.goto(url + "#/projects");
    const row = page.getByRole("row", { name: /redis-platform/ });

    // A project opens, closes and opens again: reading it once must not
    // cost the owner every action it holds.
    await row.getByText("redis-platform", { exact: true }).click();
    await assert.doesNotReject(page.getByRole("dialog", { name: "redis-platform", exact: true }).waitFor());
    await page.getByRole("button", { name: "Close" }).first().click();
    await row.getByText("redis-platform", { exact: true }).click();
    const drawer = page.getByRole("dialog", { name: "redis-platform", exact: true });
    await assert.doesNotReject(drawer.waitFor());

    // Asking from inside the open drawer leaves only the question on
    // screen, and backing out of it keeps the project.
    await drawer.getByRole("button", { name: "Delete project", exact: true }).click();
    const asked = page.getByRole("dialog", { name: /Delete project redis-platform/ });
    await assert.doesNotReject(drawer.waitFor({ state: "detached" }));
    await asked.getByRole("button", { name: "Cancel", exact: true }).click();
    await assert.doesNotReject(asked.waitFor({ state: "detached" }));
    assert.deepEqual(f.deletedProjects, [], "cancelling keeps the project");

    // Delete is on the row itself, and says what it takes with it.
    await row.getByRole("button", { name: /More actions for redis-platform/ }).click();
    await page.getByRole("menuitem", { name: "Delete project", exact: true }).click();
    const confirm = page.getByRole("dialog", { name: /Delete project redis-platform/ });
    await assert.doesNotReject(confirm.getByText(/1 conversations and 1 tasks/).waitFor());
    assert.deepEqual(f.deletedProjects, [], "asking is not deleting");
    await confirm.getByRole("button", { name: "Delete project", exact: true }).click();
    await assert.doesNotReject(page.getByRole("dialog", { name: /Delete project/ }).waitFor({ state: "detached" }));
    assert.deepEqual(f.deletedProjects, ["redis-platform"]);

    // The default project cannot be deleted by mistake.
    const first = page.getByRole("row", { name: /workspace/ });
    await first.getByRole("button", { name: /More actions for workspace/ }).click();
    assert.equal(await page.getByRole("menuitem", { name: "Delete project", exact: true }).isDisabled(), true, "the default project offers no delete");
    await page.keyboard.press("Escape");

    // A conversation is deleted from its own row, after the same question.
    await page.goto(url + "#/console");
    const thread = page.getByRole("listitem").filter({ hasText: "the doomed thread" }).last();
    const more = thread.getByRole("button", { name: "More" });
    // An action nobody can see is an action nobody has: the row's menu
    // does not wait for a pointer to appear.
    assert.ok(await more.evaluate((el) => Number(getComputedStyle(el).opacity) > 0.2), "a conversation's actions are legible before it is hovered");
    await more.click();
    await page.getByRole("menuitem", { name: "Delete", exact: true }).click();
    const threadConfirm = page.getByRole("dialog", { name: /Delete this conversation/ });
    await assert.doesNotReject(threadConfirm.getByText(/the doomed thread/).waitFor());
    assert.deepEqual(f.deletedThreads, [], "asking is not deleting");
    await threadConfirm.getByRole("button", { name: "Delete", exact: true }).click();
    await assert.doesNotReject(page.getByRole("dialog", { name: /Delete this conversation/ }).waitFor({ state: "detached" }));
    assert.deepEqual(f.deletedThreads, ["console:one"]);

    assert.deepEqual(f.errors, []);
    console.log("PASS projects and conversations are deleted from their rows, once confirmed");
} finally {
    await context.close(); await browser.close(); await server.close();
}
