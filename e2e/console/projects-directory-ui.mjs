// A project's directory is named under the workspace of the machine it
// belongs to, never picked as a path of its own; every API call stays
// inside the fixture.
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
// The coordinator runs on the Linux machine while the console runs on the
// Mac: which machine is "this machine" cannot be read from who coordinates.
const nodes = [{ name: "node-linux", display_name: "Build box", role: "hub", up: true, version: "test", projects_root: "/home/dev/steve-workspace/projects" }, { name: "node-mac", display_name: "My Mac", role: "worker", up: true, version: "test", projects_root: "/Users/me/Steve/projects" }];
const f = { browses: [], picks: [], projects: [], errors: [] };
page.on("pageerror", (error) => f.errors.push(String(error)));
await page.addInitScript(() => {
    localStorage.setItem("steve.ui.locale", "en");
    window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
    window.pickedWith = [];
    window.steveDesktop = { pickDirectory: (directory) => { window.pickedWith.push(directory ?? null); return Promise.resolve("/Users/me/work/mac-service"); } };
});
await page.route("**/*", async (route) => {
    const req = route.request(), u = new URL(req.url()), p = u.pathname;
    if (u.origin !== new URL(url).origin) { f.errors.push("external " + u.origin); return route.abort(); }
    if (!p.startsWith("/console/") && !["/state", "/events"].includes(p)) return route.continue();
    if (p === "/state") return route.fulfill({ json: { at, hub: { node: "node-linux", version: "test" }, nodes, agents: [], projects: [], tasks: [], plans: [], attempts: [], landings: [] } });
    if (p === "/console/queue") return route.fulfill({ json: { queue: [], submission_keys: true } });
    if (p === "/console/desktop") return route.fulfill({ json: { enabled: true, node_id: "node-mac", setup_required: false, agent_count: 1 } });
    if (p === "/console/coordination") return route.fulfill({ json: { enabled: true, cluster_id: "cluster-test", node_id: "node-mac", coordinator_id: "node-linux", epoch: 1, revision: 1, authoritative: true, observed_at: at, auto_failover: false, ready: true, nodes: [], events: [] } });
    if (p === "/console/ssh/browse") {
        const body = req.postDataJSON(); f.browses.push(body);
        const home = "/home/dev", entries = body.path === "/home/dev/work" ? ["steve"] : ["work"];
        const at_ = body.path === "/home/dev/work" ? { path: home + "/work", display: "~/work", parent: home } : { path: home, display: "~", parent: "/home" };
        return route.fulfill({ json: { ...at_, home, writable: true, entries: entries.map((name) => ({ name, path: at_.path + "/" + name })) } });
    }
    if (p === "/console/projects" && req.method() === "POST") { f.projects.push(req.postDataJSON()); return route.fulfill({ json: { ok: true } }); }
    return route.continue();
});
try {
    await page.goto(url + "#/projects");
    await page.getByRole("button", { name: "Add project", exact: true }).first().click();
    const dialog = page.getByRole("dialog", { name: "Add project", exact: true });
    await dialog.getByRole("textbox", { name: "Name", exact: true }).fill("my-service");

    // The directory a project gets is the same relative directory on every
    // machine, so there is nothing to browse for: the machine's workspace
    // is shown, the project's own name fills the rest.
    assert.equal(await dialog.getByRole("button", { name: "Browse…", exact: true }).count(), 0, "a project directory is not browsed");
    assert.equal(await dialog.getByRole("button", { name: "Choose folder…", exact: true }).count(), 0, "a project directory is not chosen from the system");
    await assert.doesNotReject(dialog.getByText("/home/dev/steve-workspace/projects/my-service", { exact: true }).waitFor());

    // Another machine resolves the same project under its own workspace.
    await dialog.getByRole("button", { name: /Machine/ }).click();
    await page.getByRole("option", { name: /My Mac/ }).click();
    await assert.doesNotReject(dialog.getByText("/Users/me/Steve/projects/my-service", { exact: true }).waitFor());

    await dialog.getByRole("textbox", { name: "Directory", exact: true }).fill("team/my-service");
    await assert.doesNotReject(dialog.getByText("/Users/me/Steve/projects/team/my-service", { exact: true }).waitFor());

    await dialog.getByRole("button", { name: "Add project", exact: true }).click();
    await assert.doesNotReject(dialog.getByText(/my-service/).first().waitFor());
    assert.deepEqual(f.projects, [{ id: "my-service", node: "node-mac", path: "team/my-service", repo: "inplace", level: "internal" }]);
    assert.deepEqual(f.browses, [], "the project dialog never browses a machine");
    assert.deepEqual(f.errors, []);
    console.log("PASS project directories are named under the machine's workspace");
} finally {
    await context.close(); await browser.close(); await server.close();
}
