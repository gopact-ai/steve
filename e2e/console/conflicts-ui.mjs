// Whether two machines are stuck on the same files is a question about the
// whole workspace, so it is answered in one place, and both ways out of a
// conflict are offered there: hand it to an agent, or write the
// resolution yourself.
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
const nodes = [{ name: "node-linux", display_name: "Build box", role: "hub", up: true, version: "test" }, { name: "node-mac", display_name: "My Mac", role: "worker", up: true, version: "test" }];
const marked = "keep\n<<<<<<< ours\nthe main line\n=======\nthis result\n>>>>>>> theirs\ntail\n";
const conflicts = [
    { project: "atlas", artifact: "art-editable-1", landing: "land-1", node: "node-mac", files: ["src/a.txt", "src/b.txt"], resolvable: true, editable: true, at },
    { project: "sealed-thing", artifact: "art-sealed-2", landing: "land-2", node: "node-linux", files: ["secret.txt"], resolvable: true, editable: false, at },
    { project: "atlas", artifact: "art-notree-3", landing: "land-3", node: "node-mac", files: ["gone.txt"], resolvable: false, editable: false, at },
];
const f = { agent: [], all: 0, reads: [], manual: [], errors: [] };
page.on("pageerror", (error) => f.errors.push(String(error)));
await page.addInitScript(() => {
    localStorage.setItem("steve.ui.locale", "en");
    window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
});
await page.route("**/*", async (route) => {
    const req = route.request(), u = new URL(req.url()), p = u.pathname;
    if (u.origin !== new URL(url).origin) { f.errors.push("external " + u.origin); return route.abort(); }
    if (!p.startsWith("/console/") && !["/state", "/events"].includes(p)) return route.continue();
    if (p === "/state") return route.fulfill({ json: { at, hub: { node: "node-linux", version: "test" }, nodes, agents: [], projects: [], tasks: [], plans: [], attempts: [], landings: [], conflicts, inbox: [] } });
    if (p === "/console/queue") return route.fulfill({ json: { queue: [], submission_keys: true } });
    if (p === "/console/conflicts" && req.method() === "POST") { f.all++; return route.fulfill({ json: { started: 2 } }); }
    if (p.endsWith("/agent") && req.method() === "POST") { f.agent.push(p); return route.fulfill({ json: { started: 1 } }); }
    if (p.endsWith("/file")) { const want = u.searchParams.get("path"); f.reads.push(want); return route.fulfill({ json: { path: want, text: marked, size: marked.length } }); }
    if (p.endsWith("/manual") && req.method() === "POST") { f.manual.push({ path: p, body: req.postDataJSON() }); return route.fulfill({ json: { ok: true } }); }
    return route.continue();
});
try {
    await page.goto(url + "#/inbox");
    const panel = page.getByRole("region", { name: "Merge conflicts", exact: true });
    await panel.waitFor();

    // The list names the project and the machine by the name a person gave
    // it, and says which files disagreed: a node id is not an answer.
    await panel.getByText("atlas", { exact: true }).first().waitFor();
    await panel.getByText("Build box", { exact: true }).waitFor();
    await panel.getByText("My Mac", { exact: true }).first().waitFor();
    assert.equal((await panel.innerText()).includes("node-mac"), false, "machines are named, not identified");
    await panel.getByText("src/a.txt · src/b.txt", { exact: false }).waitFor();

    // What cannot be done is said, rather than offered and then refused.
    const sealed = panel.getByRole("listitem").filter({ hasText: "sealed-thing" });
    await sealed.getByText("stays on its home machine", { exact: false }).waitFor();
    assert.equal(await sealed.getByRole("button", { name: "Resolve by hand", exact: true }).count(), 0, "a sealed project cannot be edited here");
    assert.equal(await sealed.getByRole("button", { name: "Hand to an agent", exact: true }).count(), 1, "a sealed project can still be handed to an agent");
    const noTree = panel.getByRole("listitem").filter({ hasText: "art-notree-3" });
    assert.equal(await noTree.getByRole("button", { name: "Hand to an agent", exact: true }).count(), 0, "nothing to work from means no agent button");
    await noTree.getByText("no snapshot to edit", { exact: false }).waitFor();

    // The count is carried to the navigation, so a conflict is visible from
    // anywhere in the console and not only on the page that lists it.
    await page.getByRole("link", { name: /Inbox/ }).getByText("3", { exact: true }).waitFor();

    const first = panel.getByRole("listitem").filter({ hasText: "art-editable" });
    await first.getByRole("button", { name: "Hand to an agent", exact: true }).click();
    await first.getByText("Handed to an agent", { exact: false }).waitFor();
    assert.deepEqual(f.agent, ["/console/conflicts/art-editable-1/agent"]);
    console.log("PASS every project's conflicts are listed in one place, named by machine, with only the actions that apply");

    // Resolving by hand: the half-merged text is what is edited, taking a
    // side is one click, and nothing can be submitted while markers remain.
    await first.getByRole("button", { name: "Resolve by hand", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Resolve the conflict in art-editable", exact: false });
    await dialog.waitFor();
    // Deduped: the dev server mounts effects twice, which production does not.
    assert.deepEqual([...new Set(f.reads)].sort(), ["src/a.txt", "src/b.txt"]);
    const editor = dialog.getByRole("textbox", { name: "src/a.txt", exact: true });
    await editor.waitFor();
    assert.ok((await editor.inputValue()).includes("<<<<<<< ours"), "the editor holds what git left");
    const submit = dialog.getByRole("button", { name: "Submit and merge", exact: true });
    assert.equal(await submit.isDisabled(), true, "markers left in means nothing to submit");
    await dialog.getByText("Conflict markers are still in it", { exact: true }).waitFor();

    await dialog.getByRole("button", { name: "Keep this result", exact: true }).click();
    assert.equal(await editor.inputValue(), "keep\nthis result\ntail\n", "taking a side keeps the untouched lines around it");
    await dialog.getByText("Ready to submit", { exact: true }).waitFor();
    // The other file is still marked, so the dialog as a whole is not ready.
    assert.equal(await submit.isDisabled(), true, "one file resolved is not the conflict resolved");

    await dialog.getByRole("button", { name: "src/b.txt", exact: false }).click();
    await dialog.getByRole("button", { name: "Keep the main line", exact: true }).click();
    assert.equal(await dialog.getByRole("textbox", { name: "src/b.txt", exact: true }).inputValue(), "keep\nthe main line\ntail\n");
    assert.equal(await submit.isDisabled(), false);
    await submit.click();
    await dialog.waitFor({ state: "hidden" });
    assert.equal(f.manual.length, 1);
    assert.equal(f.manual[0].path, "/console/conflicts/art-editable-1/manual");
    assert.deepEqual(f.manual[0].body.files, [
        { path: "src/a.txt", text: "keep\nthis result\ntail\n" },
        { path: "src/b.txt", text: "keep\nthe main line\ntail\n" },
    ]);
    console.log("PASS a conflict can be resolved by hand: both sides offered, markers blocked, every file sent once");

    await panel.getByRole("button", { name: "Hand them all to an agent", exact: true }).click();
    await panel.getByText("2 conflict(s) being resolved", { exact: false }).waitFor();
    assert.equal(f.all, 1);
    console.log("PASS the whole workspace's conflicts can be handed over at once");

    assert.deepEqual(f.errors, []);
} finally {
    await browser.close(); await server.close();
}
