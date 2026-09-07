// Source preview; all SSH, discovery and installation calls stay inside fixtures.
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { mkdir } from "node:fs/promises";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const server = await createServer({ configFile: path.join(web, "vite.config.ts"), root: web, server: { host: "127.0.0.1", port: 0 } }); await server.listen();
const url = server.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
const page = await context.newPage(); page.setDefaultTimeout(6500);
const at = "2026-09-07T01:00:00Z";
const candidate = { alias: "dev-box", host_name: "10.0.0.9", user: "developer", port: 22, proxy_jump: "bastion", has_proxy_command: false, has_identity_file: true, conditional: true, source: "/test/ssh/config", line: 3 };
const check = { candidate, reachable: true, address: "10.0.0.9", os: "linux", arch: "arm64", tools: [{ name: "bash", available: true }, { name: "nohup", available: true }], existing_installation: false, existing_paths: [], installation_mode: "peer", steps: [{ id: "ssh", status: "ready", message: "SSH connection and authentication verified" }], checked_at: at };
const f = { candidates: [candidate], checks: [], plans: [], installs: [], nodeAgentReads: [], errors: [], ready: false, checkError: false, discoveryError: false, reset: false, hold: false, release: null, connected: false, coordinator: "my-desktop", changedNetwork: false };
page.on("pageerror", (error) => f.errors.push(String(error)));
await page.addInitScript(() => { localStorage.setItem("steve.ui.locale", "en"); window.sources = []; window.EventSource = class { constructor() { window.sources.push(this); setTimeout(() => this.onopen?.(), 0); } close() {} }; });
const routeRequest = async (route) => {
    const req = route.request(), u = new URL(req.url()), p = u.pathname;
    if (u.origin !== new URL(url).origin) { f.errors.push("external " + u.origin); return route.abort(); }
    if (!p.startsWith("/console/") && !["/state", "/events"].includes(p)) return route.continue();
    if (p === "/state") return route.fulfill({ json: { at, hub: { node: f.coordinator, version: "test" }, nodes: [], agents: [], tasks: [], plans: [], projects: [], attempts: [], landings: [] } });
    if (p === "/console/queue" && req.method() === "GET") return route.fulfill({ json: { queue: [], submission_keys: true } });
    if (p === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false } });
    if (p === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false, agent_count: 0 } });
    if (p === "/console/ssh/candidates") return f.discoveryError ? route.fulfill({ status: 503, json: { error: "SSH config is temporarily unavailable" } }) : route.fulfill({ json: { candidates: f.candidates, warnings: [{ source: "/test/ssh/config", line: 1, code: "dynamic_include", message: "Some conditional destinations are resolved when connecting." }], revision: "r1" } });
    if (p === "/console/ssh/check") {
        f.checks.push(req.postDataJSON());
        return f.checkError ? route.fulfill({ status: 400, json: { error: "SSH host identity needs verification. Use a terminal to confirm the host key first." } }) : route.fulfill({ json: check });
    }
    if (p === "/console/ssh/plans") {
        const request = req.postDataJSON(); f.plans.push(request);
        return route.fulfill({ json: { id: "plan-" + f.plans.length, request: f.changedNetwork ? { ...request, source_host: "203.0.113.23" } : request, check, script: "mkdir -p ~/steve-bin\n# install a verified node binary", effects: ["Create node configuration on dev-box", "Start the node service on port 7701"], steps: [...check.steps, ...(f.ready ? [] : [{ id: "binary", status: "blocked", message: "No matching node package", suggestion: "Provide a Linux arm64 node package and review a new plan." }])], ready: f.ready, expires_at: "2030-01-01T00:00:00Z", binary: { os: "linux", arch: "arm64", sha256: "a".repeat(64), size: 2048 } } });
    }
    if (p === "/console/nodes/node-stable-9/agents" && req.method() === "GET") { f.nodeAgentReads.push(p); return route.fulfill({ json: { revision: "r1", agents: [] } }); }
    if (/^\/console\/ssh\/plans\/[^/]+\/install$/.test(p)) {
        const plan_id = p.split("/")[4]; f.installs.push(plan_id);
        if (f.hold) await new Promise((resolve) => { f.release = resolve; });
        if (f.reset) { f.reset = false; return route.abort("connectionreset"); }
        return route.fulfill({ json: { plan_id, name: "worker-west", node_id: "node-stable-9", registered: true, connected: f.connected, status: f.connected ? "connected" : "needs_attention", steps: [{ id: "registration", status: "ready", message: "Node registration is retained" }, ...(f.connected ? [{ id: "connectivity", status: "ready", message: "Node protocol handshake completed" }] : [{ id: "connectivity", status: "blocked", message: "The node service is not reachable yet", suggestion: "Check the node address and network route in Resources." }])] } });
    }
    f.errors.push(req.method() + " " + p); return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
};
await page.route("**/*", routeRequest);
async function waitFor(check, message) { for (let i = 0; i < 120; i++) { if (await check()) return; await new Promise((resolve) => setTimeout(resolve, 25)); } assert.fail(message); }
async function screenshot(name, target = page) { if (!process.env.SSH_SCREENSHOTS) return; await mkdir(process.env.SSH_SCREENSHOTS, { recursive: true }); await target.screenshot({ path: path.join(process.env.SSH_SCREENSHOTS, name + ".png"), animations: "disabled" }); }
try {
    await page.goto(url + "#/fleet");
    await page.getByRole("link", { name: "Register local agents", exact: true }).waitFor();
    assert.equal(await page.getByRole("link", { name: "Register local agents", exact: true }).getAttribute("href"), "#/console?setup=agents");
    await page.getByRole("button", { name: "Connect with SSH", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Connect a machine with SSH", exact: true });
    await dialog.getByText("dev-box", { exact: true }).waitFor();
    await dialog.getByText("Some conditional destinations are resolved when connecting.", { exact: true }).waitFor();
    assert.deepEqual(f.checks, []); assert.deepEqual(f.plans, []); assert.deepEqual(f.installs, []);
    await dialog.getByText("dev-box", { exact: true }).click();
    f.checkError = true;
    await dialog.getByRole("button", { name: "Check connection", exact: true }).click();
    await dialog.getByText("SSH host identity needs verification. Use a terminal to confirm the host key first.", { exact: true }).waitFor();
    assert.deepEqual(f.plans, []); assert.deepEqual(f.installs, []);
    const failedChecks = f.checks.length;
    f.candidates = [];
    await dialog.getByRole("button", { name: "Refresh machines", exact: true }).click();
    await dialog.getByRole("heading", { name: "No SSH machines found", exact: true }).waitFor();
    assert.equal(await dialog.getByRole("alert").count(), 0, "successful discovery clears obsolete connection errors");
    assert.equal(await dialog.getByRole("button", { name: "Check connection", exact: true }).isDisabled(), true);
    f.candidates = [candidate];
    await dialog.getByRole("button", { name: "Refresh machines", exact: true }).click();
    await dialog.getByRole("radio").waitFor();
    assert.equal(await dialog.getByRole("radio").isChecked(), false, "a removed alias does not silently become selected again");
    await dialog.getByRole("button", { name: "Check connection", exact: true }).click();
    await dialog.getByRole("alert").getByText("Choose a machine first.", { exact: true }).waitFor();
    assert.equal(f.checks.length, failedChecks, "an obsolete selection cannot issue another connection check");
    assert.equal(await dialog.getByRole("radio").evaluate((el) => el === document.activeElement || el.contains(document.activeElement)), true);
    await dialog.getByText("dev-box", { exact: true }).click();
    f.checkError = false;
    check.existing_installation = true; check.existing_paths = ["~/steve-bin/node.json", "~/.steve-node"]; check.existing_node = { name: "recorded-worker", owner: "recorded-workspace" };
    await page.setViewportSize({ width: 780, height: 540 });
    await dialog.getByRole("button", { name: "Check connection", exact: true }).click();
    await dialog.getByRole("heading", { name: "Existing Steve configuration found", exact: true }).waitFor();
    await dialog.getByText("dev-box", { exact: true }).waitFor();
    await dialog.getByText("10.0.0.9", { exact: true }).waitFor();
    await dialog.getByText("~/steve-bin/node.json", { exact: true }).waitFor();
    await dialog.getByText("Node name in configuration", { exact: true }).waitFor();
    await dialog.getByText("recorded-worker", { exact: true }).waitFor();
    await dialog.getByText("Workspace recorded in configuration", { exact: true }).waitFor();
    await dialog.getByText("recorded-workspace", { exact: true }).waitFor();
    assert.equal(await dialog.getByRole("textbox", { name: "Node name", exact: true }).count(), 0);
    assert.equal(await dialog.getByRole("button", { name: "Review installation", exact: true }).count(), 0);
    assert.equal(await dialog.getByRole("button", { name: "Connect as an execution node only", exact: true }).count(), 0);
    assert.ok(await dialog.evaluate((el) => el.scrollWidth <= el.clientWidth));
    assert.deepEqual(f.plans, []); assert.deepEqual(f.installs, []);
    await dialog.getByRole("button", { name: "View machines in this workspace", exact: true }).click();
    await dialog.waitFor({ state: "hidden" });
    assert.equal(await page.locator("#fleet-machines").evaluate((el) => el === document.activeElement), true);
    await page.getByRole("button", { name: "Connect with SSH", exact: true }).click();
    await dialog.getByRole("heading", { name: "Existing Steve configuration found", exact: true }).waitFor();
    await dialog.getByText("dev-box", { exact: true }).waitFor();
    await dialog.getByText("10.0.0.9", { exact: true }).waitFor();
    check.existing_installation = false; check.existing_paths = [];
    await dialog.getByRole("button", { name: "Check this machine again", exact: true }).click();
    await dialog.getByRole("heading", { name: "Participate in collaboration and recovery", exact: true }).waitFor();
    assert.equal(await dialog.getByRole("button", { name: "Data level", exact: true }).count(), 0);
    console.log("PASS existing installations show evidence and usable navigation without a fresh-install dead end");
    await dialog.getByRole("button", { name: "Choose another machine", exact: true }).click();
    await dialog.getByRole("button", { name: "Check connection", exact: true }).click();
    await dialog.getByText("SSH is reachable. Review how to connect this machine.", { exact: true }).waitFor();
    assert.equal(await dialog.getByRole("heading", { name: "SSH is reachable. Review how to connect this machine.", exact: true }).evaluate((el) => el === document.activeElement), true);
    assert.equal(await dialog.getByRole("textbox", { name: "Node address", exact: true }).inputValue(), "10.0.0.9:7701");
    await page.setViewportSize({ width: 780, height: 540 });
    await dialog.getByRole("textbox", { name: "Node name", exact: true }).fill("worker-west");
    const scrollArea = dialog.locator("div.overflow-y-auto").first();
    const review = dialog.getByRole("button", { name: "Review installation", exact: true });
    await review.scrollIntoViewIfNeeded();
    assert.ok(await scrollArea.evaluate((el) => el.scrollTop > 0), "form is scrolled before advancing");
    await dialog.getByRole("button", { name: "Review installation", exact: true }).click();
    await dialog.getByText("No matching node package", { exact: true }).waitFor();
    assert.equal(await scrollArea.evaluate((el) => el.scrollTop), 0, "new plan starts at its confirmation context");
    assert.equal(await dialog.getByRole("heading", { name: "Review connection changes", exact: true }).evaluate((el) => el === document.activeElement), true);
    await page.setViewportSize({ width: 1440, height: 1000 });
    assert.equal(await dialog.getByRole("button", { name: "Confirm installation", exact: true }).isDisabled(), true);
    assert.equal(f.plans.length, 1); assert.deepEqual(f.installs, []);
    console.log("PASS SSH discovery stays read-only, checks surface actionable errors and blocked plans cannot install");

    await dialog.getByRole("button", { name: "Edit connection details", exact: true }).click();
    await page.setViewportSize({ width: 780, height: 540 });
    await dialog.getByText("Advanced network settings", { exact: true }).click();
    await dialog.getByRole("textbox", { name: "Election address", exact: true }).fill(" 10.0.0.9:8802 ");
    const sourceHost = dialog.getByRole("textbox", { name: "This machine’s network address", exact: true });
    await sourceHost.scrollIntoViewIfNeeded();
    await sourceHost.focus();
    const formScroll = await scrollArea.evaluate((el) => el.scrollTop);
    assert.ok(formScroll > 0);
    await sourceHost.fill(" 10.0.0.4 ");
    assert.equal(await scrollArea.evaluate((el) => el.scrollTop), formScroll, "editing keeps the current field in place");
    assert.equal(await sourceHost.evaluate((el) => el === document.activeElement), true);
    await page.setViewportSize({ width: 390, height: 844 });
    assert.ok(await dialog.evaluate((el) => el.scrollWidth <= el.clientWidth));
    await dialog.getByRole("textbox", { name: "This machine’s network address", exact: true }).scrollIntoViewIfNeeded();
    await screenshot("ssh-network-narrow");
    await page.setViewportSize({ width: 1440, height: 1000 });
    f.ready = true;
    await dialog.getByRole("button", { name: "Review installation", exact: true }).click();
    await dialog.getByText("Create node configuration on dev-box", { exact: true }).waitFor();
    await dialog.getByText("Start the node service on port 7701", { exact: true }).waitFor();
    await screenshot("ssh-plan-desktop");
    assert.deepEqual(f.plans.at(-1), { alias: "dev-box", name: "worker-west", addr: "10.0.0.9:7701", raft_addr: "10.0.0.9:8802", source_host: "10.0.0.4", level: "restricted" });
    await dialog.getByText("10.0.0.9:8802", { exact: true }).waitFor();
    await dialog.getByText("10.0.0.4", { exact: true }).waitFor();
    f.hold = true; f.reset = true;
    const install = dialog.getByRole("button", { name: "Confirm installation", exact: true });
    await install.focus(); await page.keyboard.press("Enter");
    await waitFor(() => f.installs.length === 1, "one explicit installation");
    await page.keyboard.press("Enter"); assert.equal(f.installs.length, 1);
    assert.equal(await dialog.getByText("Machine connected", { exact: true }).count(), 0);
    f.hold = false; f.release();
    await dialog.getByRole("button", { name: "Check this installation", exact: true }).waitFor();
    const original = f.installs[0];
    f.coordinator = "new-coordinator";
    await page.reload();
    await page.getByRole("button", { name: "Connect with SSH", exact: true }).click();
    await dialog.getByRole("button", { name: "Check this installation", exact: true }).click();
    await dialog.getByText("The node service is not reachable yet", { exact: true }).waitFor();
    await dialog.getByText("Registered, awaiting connection", { exact: true }).waitFor();
    assert.equal(await scrollArea.evaluate((el) => el.scrollTop), 0, "installation result returns to outcome context");
    assert.equal(await dialog.getByRole("heading", { name: "Connection needs your attention", exact: true }).evaluate((el) => el === document.activeElement), true);
    assert.equal(await dialog.getByText("Machine connected", { exact: true }).count(), 0);
    assert.deepEqual(f.installs, [original, original]); assert.equal(f.plans.length, 2);
    console.log("PASS installation awaits its result; reconnecting after coordinator change keeps the same plan and partial registration");

    await page.setViewportSize({ width: 780, height: 540 });
    const planCountBeforeResume = f.plans.length;
    f.hold = true;
    await dialog.getByRole("button", { name: "Continue checking this connection", exact: true }).click();
    await waitFor(() => f.installs.length === 3, "resume posts the original plan");
    assert.equal(await dialog.getByRole("button", { name: "Connect another machine", exact: true }).isDisabled(), true);
    f.hold = false; f.release();
    await dialog.getByText("Registered, awaiting connection", { exact: true }).waitFor();
    assert.equal(f.plans.length, planCountBeforeResume);
    assert.deepEqual(f.installs, [original, original, original]);
    assert.equal(await dialog.getByRole("button", { name: "Check this installation", exact: true }).count(), 0);
    await dialog.getByRole("button", { name: "Back to resources", exact: true }).focus();
    assert.ok(await dialog.evaluate((el) => el.scrollWidth <= el.clientWidth));
    await screenshot("ssh-needs-attention-narrow");
    await page.evaluate(() => document.documentElement.classList.add("dark-mode")); await page.waitForTimeout(180);
    await screenshot("ssh-needs-attention-dark");
    await dialog.getByRole("button", { name: "Connect another machine", exact: true }).click();
    await dialog.getByText("1 connection records on this device", { exact: true }).click();
    await dialog.getByText("worker-west", { exact: true }).waitFor();
    assert.equal(f.plans.length, 2);
    check.existing_installation = true; check.existing_paths = ["~/steve-bin/node.json"];
    await dialog.getByText("dev-box", { exact: true }).click();
    await dialog.getByRole("button", { name: "Check connection", exact: true }).click();
    await dialog.getByText("This device has connection records using the same SSH alias. These are saved plans, not proof of the same machine or installation.", { exact: true }).waitFor();
    await dialog.getByRole("button", { name: "View record", exact: true }).click();
    await dialog.getByText("Registered, awaiting connection", { exact: true }).waitFor();
    check.existing_installation = false; check.existing_paths = [];
    await dialog.getByRole("button", { name: "Back to resources", exact: true }).click();
    await dialog.waitFor({ state: "hidden" });
    assert.equal(f.plans.length, 2); assert.deepEqual(f.installs, [original, original, original]);
    await page.getByRole("button", { name: "Add machine / agent", exact: true }).click();
    await page.getByRole("dialog", { name: "Add machine", exact: true }).waitFor();
    console.log("PASS known partial outcomes preserve registration and manual enrollment remains available");

    // A separate isolated workspace starts a new, explicitly approved installation.
    const successContext = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: "block" });
    try {
        const successPage = await successContext.newPage(); successPage.setDefaultTimeout(6500);
        await successPage.addInitScript(() => { localStorage.setItem("steve.ui.locale", "en"); window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} }; });
        await successPage.route("**/*", routeRequest);
        await successPage.goto(url + "#/fleet");
        await successPage.getByRole("button", { name: "Connect with SSH", exact: true }).click();
        const success = successPage.getByRole("dialog", { name: "Connect a machine with SSH", exact: true });
        await success.getByText("dev-box", { exact: true }).click();
        await success.getByRole("button", { name: "Check connection", exact: true }).click();
        await success.getByRole("textbox", { name: "Node name", exact: true }).fill("worker-west");
        await success.getByRole("button", { name: "Review installation", exact: true }).click();
        f.connected = true;
        await success.getByRole("button", { name: "Confirm installation", exact: true }).click();
        await success.getByText("Machine connected", { exact: true }).waitFor();
        await success.getByRole("button", { name: "Register agents on this machine", exact: true }).click();
        const enrollment = successPage.getByRole("dialog", { name: "Register an agent on worker-west", exact: true });
        await enrollment.getByText("No usable tools found on this machine", { exact: true }).waitFor();
        assert.ok(f.nodeAgentReads.length > 0);
        assert.deepEqual([...new Set(f.nodeAgentReads)], ["/console/nodes/node-stable-9/agents"]);
        await enrollment.getByRole("button", { name: "Close", exact: true }).click();
        await success.getByRole("button", { name: "Done", exact: true }).click();
        await success.waitFor({ state: "hidden" });
        assert.equal(f.plans.length, 3); assert.equal(f.installs.at(-1), "plan-3");
        console.log("PASS connected requires an acknowledged node protocol handshake");
        await successPage.getByRole("button", { name: "Connect with SSH", exact: true }).click();
        await success.getByText("dev-box", { exact: true }).click();
        await success.getByRole("button", { name: "Check connection", exact: true }).click();
        await success.getByRole("textbox", { name: "Node name", exact: true }).fill("worker-second");
        const beforeInstall = f.installs.length;
        f.changedNetwork = true;
        await success.getByRole("button", { name: "Review installation", exact: true }).click();
        await success.getByRole("alert").getByText("The service response was incomplete. Retry the same operation.", { exact: true }).waitFor();
        assert.equal(await success.getByRole("button", { name: "Confirm installation", exact: true }).count(), 0);
        assert.equal(f.installs.length, beforeInstall);
        f.changedNetwork = false;
        await success.getByRole("button", { name: "Review installation", exact: true }).click();
        await success.getByRole("button", { name: "Confirm installation", exact: true }).waitFor();
        assert.equal(f.installs.length, beforeInstall);
        console.log("PASS network overrides persist with the plan and a changed response cannot become approved installation details");
    } finally { await successContext.close(); }
    // Saved low-access drafts require an explicit storage decision, in both languages.
    for (const locale of ["zh", "en"]) {
        const scopedContext = await browser.newContext({ viewport: { width: 780, height: 540 }, serviceWorkers: "block" });
        try {
            const scopedPage = await scopedContext.newPage(); scopedPage.setDefaultTimeout(6500);
            await scopedPage.addInitScript(({ locale, check }) => {
                localStorage.setItem("steve.ui.locale", locale);
                localStorage.setItem(`steve.ssh.connect:${new URL(".", window.location.href).href}`, JSON.stringify({ request: { alias: "dev-box", name: "saved-worker", addr: "10.0.0.9:7701", level: "internal" }, check }));
                window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
            }, { locale, check });
            await scopedPage.route("**/*", routeRequest);
            await scopedPage.goto(url + "#/fleet");
            const zh = locale === "zh";
            await scopedPage.getByRole("button", { name: zh ? "通过 SSH 接入" : "Connect with SSH", exact: true }).click();
            const scoped = scopedPage.getByRole("dialog", { name: zh ? "通过 SSH 接入机器" : "Connect a machine with SSH", exact: true });
            await scoped.getByRole("heading", { name: zh ? "参与协作与恢复" : "Participate in collaboration and recovery", exact: true }).waitFor();
            assert.equal(await scoped.getByRole("button", { name: zh ? "查看安装计划" : "Review installation", exact: true }).count(), 0);
            const before = f.plans.length;
            await scoped.getByRole("button", { name: zh ? "允许保存上述数据并查看计划" : "Allow this data and review installation", exact: true }).click();
            await scoped.getByRole("heading", { name: zh ? "确认接入改动" : "Review connection changes", exact: true }).waitFor();
            assert.equal(f.plans.length, before + 1); assert.equal(f.plans.at(-1).level, "restricted");
            assert.ok(await scoped.evaluate((el) => el.scrollWidth <= el.clientWidth));
            await screenshot("ssh-peer-consent-" + locale, scopedPage);
            await scoped.getByRole("button", { name: zh ? "修改接入信息" : "Edit connection details", exact: true }).click();
            await scoped.getByRole("button", { name: zh ? "仅作为执行节点接入" : "Connect as an execution node only", exact: true }).click();
            const manual = scopedPage.getByRole("dialog", { name: zh ? "添加机器" : "Add machine", exact: true });
            await manual.waitFor();
            assert.equal(await manual.getByRole("textbox").nth(0).inputValue(), "saved-worker");
            assert.equal(await manual.getByRole("textbox").nth(1).inputValue(), "10.0.0.9:7701");
            await manual.getByText(zh ? "先登记执行节点，再将生成的启动命令放到这台机器上运行。这个入口不会通过 SSH 自动安装，也不会迁移协作数据。" : "Register the execution node, then run the generated startup command on that machine. This action does not install through SSH or transfer collaboration data.", { exact: true }).waitFor();
            assert.ok(await manual.evaluate((el) => el.scrollWidth <= el.clientWidth));
        } finally { await scopedContext.close(); }
    }
    console.log("PASS Chinese and English low-access drafts require explicit consent; execution-only action prefills a real manual enrollment");
    // Older unchecked drafts must not guess which kind of node the server installs.
    const oldContext = await browser.newContext({ viewport: { width: 780, height: 540 }, serviceWorkers: "block" });
    try {
        const oldPage = await oldContext.newPage(); oldPage.setDefaultTimeout(6500);
        const { installation_mode, ...oldCheck } = check;
        await oldPage.addInitScript(({ check }) => {
            localStorage.setItem("steve.ui.locale", "en");
            localStorage.setItem(`steve.ssh.connect:${new URL(".", window.location.href).href}`, JSON.stringify({ request: { alias: "dev-box", name: "old-worker", addr: "10.0.0.9:7701", level: "internal" }, check }));
            window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
        }, { check: oldCheck });
        await oldPage.route("**/*", routeRequest);
        await oldPage.goto(url + "#/fleet");
        const before = f.checks.length;
        await oldPage.getByRole("button", { name: "Connect with SSH", exact: true }).click();
        const oldDialog = oldPage.getByRole("dialog", { name: "Connect a machine with SSH", exact: true });
        await oldDialog.getByRole("button", { name: "Check connection", exact: true }).waitFor();
        assert.equal(f.checks.length, before, "discarding an old check does not start SSH automatically");
        check.installation_mode = "executor";
        await oldDialog.getByRole("button", { name: "Check connection", exact: true }).click();
        assert.equal(await oldDialog.getByRole("textbox", { name: "Node name", exact: true }).inputValue(), "old-worker");
        const dataLevel = oldDialog.getByRole("button", { name: /Internal · e.g. internal code and work documents/ });
        await dataLevel.click();
        await oldPage.getByRole("option", { name: "Public · e.g. open-source code and public documents", exact: true }).click();
        assert.equal(await oldDialog.getByRole("heading", { name: "Participate in collaboration and recovery", exact: true }).count(), 0);
        const beforePlans = f.plans.length;
        await oldDialog.getByRole("button", { name: "Review installation", exact: true }).click();
        await oldDialog.getByRole("heading", { name: "Review connection changes", exact: true }).waitFor();
        assert.equal(f.plans.length, beforePlans + 1); assert.equal(f.plans.at(-1).level, "public");
        f.connected = false;
        await oldDialog.getByRole("button", { name: "Confirm installation", exact: true }).click();
        await oldDialog.getByText("Registered, awaiting connection", { exact: true }).waitFor();
        assert.equal(await oldDialog.getByRole("button", { name: "Continue checking this connection", exact: true }).count(), 0, "execution-node cached outcomes do not offer unsupported registration recovery");
    } finally { check.installation_mode = "peer"; await oldContext.close(); }
    console.log("PASS old checks require explicit refresh and executor data options explain their project scope");

    const reviewedContext = await browser.newContext({ viewport: { width: 780, height: 540 }, serviceWorkers: "block" });
    try {
        const reviewedPage = await reviewedContext.newPage(); reviewedPage.setDefaultTimeout(6500);
        const { installation_mode, ...oldCheck } = check;
        await reviewedPage.addInitScript(({ check }) => {
            const request = { alias: "dev-box", name: "reviewed-worker", addr: "10.0.0.9:7701", level: "internal" };
            localStorage.setItem("steve.ui.locale", "en");
            localStorage.setItem(`steve.ssh.connect:${new URL(".", window.location.href).href}`, JSON.stringify({ request, check, plan: { id: "saved-reviewed-plan", request, check, steps: [], effects: ["Original reviewed effect"], ready: false, expires_at: "2030-01-01T00:00:00Z" } }));
            window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
        }, { check: oldCheck });
        await reviewedPage.route("**/*", routeRequest);
        await reviewedPage.goto(url + "#/fleet");
        await reviewedPage.getByRole("button", { name: "Connect with SSH", exact: true }).click();
        const reviewed = reviewedPage.getByRole("dialog", { name: "Connect a machine with SSH", exact: true });
        await reviewed.getByText("Original reviewed effect", { exact: true }).waitFor();
        const beforeChecks = f.checks.length;
        await reviewed.getByRole("button", { name: "Edit connection details", exact: true }).click();
        await reviewed.getByRole("button", { name: "Check connection", exact: true }).waitFor();
        assert.equal(f.checks.length, beforeChecks);
        assert.equal(await reviewed.getByRole("button", { name: "Review installation", exact: true }).count(), 0);
        check.installation_mode = undefined;
        await reviewed.getByRole("button", { name: "Check connection", exact: true }).click();
        await reviewed.getByRole("alert").getByText("The service response was incomplete. Retry the same operation.", { exact: true }).waitFor();
        assert.equal(await reviewed.getByRole("textbox", { name: "Node name", exact: true }).count(), 0);
        check.installation_mode = "peer";
        await reviewed.getByRole("button", { name: "Check connection", exact: true }).click();
        await reviewed.getByRole("heading", { name: "Participate in collaboration and recovery", exact: true }).waitFor();
        assert.equal(await reviewed.getByRole("textbox", { name: "Node name", exact: true }).inputValue(), "reviewed-worker");
        await reviewed.getByRole("button", { name: "Allow this data and review installation", exact: true }).waitFor();
    } finally { check.installation_mode = "peer"; await reviewedContext.close(); }
    console.log("PASS editing a reviewed plan with unknown mode requires an explicit fresh check, preserving its request");
    assert.deepEqual(f.errors, []);
} catch (error) { console.log("DEBUG", JSON.stringify(f.errors), await page.locator("body").innerText()); throw error; }
finally { await context.close(); await browser.close(); await server.close(); }
