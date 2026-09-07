// Exercises role management through isolated state and command fixtures only.
import assert from "node:assert/strict";
import path from "node:path";
import { mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const server = await createServer({ configFile: path.join(web, "vite.config.ts"), root: web, server: { host: "127.0.0.1", port: 0 } }); await server.listen();
const url = server.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
const page = await context.newPage(); page.setDefaultTimeout(6000);
const at = "2026-09-07T01:00:00Z";
const node = (id, name, eligible = false) => ({ id, name, online: true, voter: true, auto_eligible: eligible, ready: true, local: id === "a" });
const f = { view: { enabled: true, cluster_id: "cluster-one", node_id: "a", coordinator_id: "b", epoch: 1, revision: 1, authoritative: true, observed_at: at, auto_failover: false, ready: true, reason: "", nodes: [node("a", "laptop", true), node("b", "dev-box", true), node("c", "build-node")], events: [] }, calls: [], errors: [], reset: false, resetBeforeCommit: false, hideReceipts: false, readError: false, hold: false, release: null, reject: false };
page.on("pageerror", (error) => f.errors.push(String(error)));
await page.addInitScript(() => { localStorage.setItem("steve.ui.locale", "en"); window.sources = []; window.EventSource = class { constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); } close() { window.sources = window.sources.filter((item) => item !== this); } }; });
const response = () => ({ ...f.view, events: f.hideReceipts ? [] : f.view.events });
await page.route("**/*", async (route) => {
    const req = route.request(), u = new URL(req.url()), p = u.pathname;
    if (u.origin !== new URL(url).origin) { f.errors.push("external " + u.origin); return route.abort(); }
    if (!p.startsWith("/console/") && !["/state", "/events"].includes(p)) return route.continue();
    if (p === "/state") return route.fulfill({ json: { at, hub: { node: "laptop", version: "test" }, nodes: [], agents: [], tasks: [], plans: [], projects: [], attempts: [], landings: [] } });
    if (p === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false, agent_count: 0 } });
    if (p === "/console/queue" && req.method() === "GET") return route.fulfill({ json: { queue: [], submission_keys: true } });
    if (p === "/console/coordination") return f.readError ? route.fulfill({ status: 503, json: { error: "Current coordination state cannot be read" } }) : route.fulfill({ json: response() });
    if (p.startsWith("/console/coordination/")) {
        const kind = p.split("/").at(-1), body = req.postDataJSON(); f.calls.push({ kind, body });
        if (f.hold) await new Promise((resolve) => { f.release = resolve; });
        if (f.resetBeforeCommit) { f.resetBeforeCommit = false; return route.abort("connectionreset"); }
        if (f.reject) { f.reject = false; return route.fulfill({ status: 409, json: { error: "The coordination revision changed. Review current state before choosing again." } }); }
        if (!f.view.events.some((event) => event.id === body.command_id)) {
            let event = { id: body.command_id, at, kind: "", actor: "owner" };
            if (kind === "transfer") { assert.equal(body.expected_epoch, f.view.epoch); event = { ...event, kind: "coordinator_transferred", from: f.view.coordinator_id, to: body.target_node_id, reason: "User requested handover" }; f.view.coordinator_id = body.target_node_id; f.view.epoch++; }
            if (kind === "policy") { assert.equal(body.expected_revision, f.view.revision); f.view.auto_failover = body.enabled; event.kind = body.enabled ? "automatic_failover_enabled" : "automatic_failover_disabled"; }
            if (kind === "eligibility") { assert.equal(body.expected_revision, f.view.revision); const target = f.view.nodes.find((item) => item.id === body.node_id); target.auto_eligible = body.eligible; event.to = body.node_id; event.kind = body.eligible ? "automatic_eligibility_granted" : "automatic_eligibility_removed"; }
            f.view.revision++; f.view.events = [event, ...f.view.events];
        }
        if (f.reset) { f.reset = false; return route.abort("connectionreset"); }
        return route.fulfill({ json: response() });
    }
    f.errors.push(req.method() + " " + p); return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
});
async function waitFor(check, message) { for (let i = 0; i < 120; i++) { if (await check()) return; await new Promise((resolve) => setTimeout(resolve, 25)); } assert.fail(message); }
async function screenshot(name) { if (!process.env.COORDINATION_SCREENSHOTS) return; await mkdir(process.env.COORDINATION_SCREENSHOTS, { recursive: true }); await page.screenshot({ path: path.join(process.env.COORDINATION_SCREENSHOTS, name + ".png"), animations: "disabled" }); }
try {
    await page.goto(url + "#/fleet");
    const panel = page.getByRole("region", { name: "Coordination and failover", exact: true });
    await panel.waitFor();
    await page.getByRole("link", { name: "Coordinated by dev-box", exact: true }).waitFor();
    assert.equal(await page.getByRole("link", { name: "Coordinated by laptop", exact: true }).count(), 0);
    const policy = panel.getByRole("switch", { name: "Automatic failover", exact: true });
    assert.equal(await policy.isChecked(), false);
    assert.equal(await panel.getByRole("switch", { name: "Allow build-node to take over automatically", exact: true }).isChecked(), false);
    f.hold = true;
    await policy.focus(); await page.keyboard.press("Space"); await waitFor(() => f.calls.length === 1, "one policy command");
    assert.equal(await policy.isChecked(), false);
    await page.keyboard.press("Space"); assert.equal(f.calls.length, 1);
    f.hold = false; f.release();
    await waitFor(() => policy.isChecked(), "policy changes only after acknowledgement");
    assert.equal(f.calls[0].body.expected_revision, 1);
    console.log("PASS shared coordinator identity and policy actions use the authoritative view");

    f.readError = true;
    await panel.getByRole("button", { name: "Refresh status", exact: true }).click();
    await panel.getByText("Current coordination state cannot be read", { exact: true }).waitFor();
    assert.equal(await policy.isDisabled(), true);
    await page.getByRole("link", { name: "Last coordinated by dev-box", exact: true }).waitFor();
    f.readError = false;
    f.view.authoritative = false; f.view.ready = false; f.view.reason = "Only one voting node is reachable; a majority is unavailable.";
    await panel.getByRole("button", { name: "Refresh status", exact: true }).click();
    await panel.getByText(f.view.reason, { exact: true }).waitFor();
    assert.equal(await panel.getByRole("button", { name: "Hand over", exact: true }).isDisabled(), true);
    assert.equal(await policy.isDisabled(), true);
    assert.equal(await policy.isChecked(), true);
    await panel.getByText("Automatic takeover is currently unavailable", { exact: true }).waitFor();
    assert.equal(f.calls.length, 1);
    f.view.authoritative = true; f.view.ready = true; f.view.reason = ""; f.view.auto_failover = false;
    const third = f.view.nodes.pop();
    await panel.getByRole("button", { name: "Refresh status", exact: true }).click();
    await waitFor(async () => !(await panel.getByRole("button", { name: "Hand over", exact: true }).isDisabled()), "two nodes allow manual handover");
    assert.equal(await policy.isDisabled(), true);
    f.view.nodes.push(third);
    await panel.getByRole("button", { name: "Refresh status", exact: true }).click();
    console.log("PASS two-node and minority constraints distinguish configured policy from current readiness");

    await panel.getByRole("button", { name: "Hand over", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Hand over coordination", exact: true });
    assert.equal(await dialog.getByRole("radio", { name: "build-node", exact: true }).isDisabled(), false, "manual handover does not require automatic eligibility");
    await dialog.getByText("laptop", { exact: true }).click();
    f.reset = true; f.hideReceipts = true;
    await dialog.getByRole("button", { name: "Hand over", exact: true }).click();
    await panel.getByRole("button", { name: "Retry the same operation", exact: true }).waitFor();
    const transfer = f.calls.at(-1); assert.equal(transfer.body.expected_epoch, 1); assert.equal(transfer.body.target_node_id, "a");
    assert.equal(f.view.nodes.every((item) => item.online), true);
    await page.reload();
    await panel.getByRole("button", { name: "Retry the same operation", exact: true }).waitFor();
    f.hideReceipts = false;
    await panel.getByRole("button", { name: "Retry the same operation", exact: true }).click();
    await panel.getByText("Operation recorded", { exact: true }).waitFor();
    assert.equal(f.calls.length, 2, "an existing receipt must not resend the transfer");
    await page.getByRole("link", { name: "Coordinated by laptop", exact: true }).waitFor();
    await panel.getByText("User requested handover", { exact: true }).waitFor();
    console.log("PASS role handover retains online nodes and reconciles a lost response without a second command");

    f.resetBeforeCommit = true;
    await panel.getByRole("switch", { name: "Allow build-node to take over automatically", exact: true }).focus(); await page.keyboard.press("Space");
    await panel.getByRole("button", { name: "Retry the same operation", exact: true }).waitFor();
    const eligibility = f.calls.at(-1);
    f.view.cluster_id = "cluster-other";
    await panel.getByRole("button", { name: "Refresh status", exact: true }).click();
    await panel.getByText("This pending operation belongs to another cluster and cannot be retried here. Its record is retained.", { exact: true }).waitFor();
    assert.equal(await panel.getByRole("button", { name: "Retry the same operation", exact: true }).isDisabled(), true);
    f.view.cluster_id = "cluster-one";
    await page.reload();
    await panel.getByRole("button", { name: "Retry the same operation", exact: true }).click();
    await waitFor(() => panel.getByRole("switch", { name: "Allow build-node to take over automatically", exact: true }).isChecked(), "eligibility retry applied");
    assert.deepEqual(f.calls.at(-1), eligibility);
    console.log("PASS retry without a receipt retains the exact eligibility payload and command ID");

    f.reject = true;
    await policy.focus(); await page.keyboard.press("Space");
    await panel.getByText("This operation was not accepted", { exact: true }).waitFor();
    assert.equal(await policy.isChecked(), false);
    await panel.getByRole("button", { name: "Choose an operation again", exact: true }).click();
    await panel.getByText("Unaccepted operations retained on this device", { exact: true }).waitFor();
    assert.equal(f.calls.length, 5);
    console.log("PASS revision conflicts retain the rejected command before permitting a new choice");

    await page.setViewportSize({ width: 390, height: 844 });
    await panel.getByRole("button", { name: "Hand over", exact: true }).focus();
    assert.ok(await panel.evaluate((el) => el.scrollWidth <= el.clientWidth));
    await screenshot("coordination-narrow-light");
    await page.evaluate(() => document.documentElement.classList.add("dark-mode")); await page.waitForTimeout(180);
    await screenshot("coordination-narrow-dark");
    f.view.enabled = false;
    await panel.getByRole("button", { name: "Refresh status", exact: true }).click();
    await panel.waitFor({ state: "hidden" });
    assert.equal(await page.getByRole("button", { name: "Hand over", exact: true }).count(), 0);
    assert.deepEqual(f.errors, []);
    console.log("PASS responsive controls and disabled deployments expose no unsupported management actions");
} catch (error) { console.log("DEBUG", JSON.stringify(f.errors), await page.locator("body").innerText()); throw error; }
finally { await context.close(); await browser.close(); await server.close(); }
