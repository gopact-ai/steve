import { workState } from "./work-fixture.mjs";
// Source preview of restarting a machine's node process from the fleet
// drawer, with the SSH restart API as a fixture: which machines offer it,
// why one is not restarted from here, the confirmation an online one asks
// for, the progress of a restart, how automatic start stands, and the
// machine's last restart wherever it shows.
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
const page = await context.newPage(); page.setDefaultTimeout(6000);
const at = new Date(Date.now() - 3 * 60_000).toISOString();
const phases = ["preflight", "restart", "connectivity"];
const log = [{ at, stream: "steve", text: "Stopping peer process 4242." }];
const running = (automatic = false, plan = "restart-1") => ({ restart: { plan_id: plan, name: "", node_id: "", registered: true, connected: false, status: "installing", phase: "restart", phases, steps: [], log }, automatic });
const restarted = { plan_id: "restart-1", name: "", node_id: "node-build", registered: true, connected: true, status: "connected", phase: "connectivity", phases, log,
    steps: [{ id: "restart", status: "ready", message: "节点进程已重新启动" }, { id: "connectivity", status: "ready", message: "节点进程已重启，机器已回到集群" }] };
const far = "本节点没有这台机器的 SSH 连接；它若经 SSH 加入，请到把它加入集群的节点的控制台重启、查看自动拉起，否则需登录这台机器手动重启节点进程";
const nodes = [
    { name: "my-desktop", role: "hub", up: true, version: "test", last_restart: { at, by: "node-build", trigger: "manual", outcome: "restarted" } },
    { name: "node-build", display_name: "build-node", role: "worker", up: true, version: "test", os: "linux", arch: "arm64", addr: "10.0.0.9:7701",
        last_restart: { at, by: "my-desktop", trigger: "automatic", outcome: "started" } },
    { name: "node-gpu", display_name: "gpu-node", role: "worker", up: false, version: "test", addr: "10.0.0.10:7701", last_error: "dial tcp 10.0.0.10:7701: connection refused" },
    { name: "node-old", display_name: "old-node", role: "worker", up: true, version: "test", addr: "10.0.0.11:7701",
        last_restart: { at, by: "my-desktop", trigger: "manual", outcome: "failed", reason: "SSH 连不上这台机器" } },
    { name: "node-far", display_name: "far-node", role: "worker", up: true, version: "test", addr: "10.0.0.12:7701" },
    { name: "node-lag", display_name: "lag-node", role: "worker", up: true, version: "test", addr: "10.0.0.13:7701" },
];
const f = {
    errors: [], reads: {}, posts: [], states: 0, hold: false, release: null, refuse: null,
    status: {
        "node-build": { restartable: true, auto_start: { state: "watching", attempts: 0, limit: 5 } },
        "node-gpu": { restartable: true, auto_start: { state: "stopped", attempts: 5, limit: 5, last_error: "连续 5 次自动拉起后，这台机器都没能保持在线 10 分钟，已停止自动拉起" } },
        "node-far": { restartable: false, reason: far },
        "node-lag": { auto_start: { state: "watching", attempts: 0, limit: 5 } },
    },
    result: { "node-build": restarted, "node-gpu": { ...restarted, node_id: "node-gpu", steps: [{ id: "restart", status: "ready", message: "节点进程原本没有运行，已启动" }, { id: "connectivity", status: "ready", message: "节点进程已启动，机器已回到集群" }] } },
};
page.on("pageerror", (error) => f.errors.push(String(error)));
await page.addInitScript(() => { localStorage.setItem("steve.ui.locale", "zh"); window.EventSource = class { addEventListener() {} constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} }; });
await page.route("**/*", async (route) => {
    const req = route.request(), u = new URL(req.url()), p = u.pathname;
    if (u.origin !== new URL(url).origin) { f.errors.push("external " + u.origin); return route.abort(); }
    if (!p.startsWith("/console/") && !["/state", "/events"].includes(p)) return route.continue();
    if (p === "/state") { f.states++; return route.fulfill({ json: workState({ at, hub: { node: "my-desktop", version: "test" }, nodes, agents: [], tasks: [], plans: [], projects: [], attempts: [], landings: [] }) }); }
    if (p === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0 } });
    if (p === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false, agent_count: 0 } });
    if (p === "/console/queue") return route.fulfill({ json: { queue: [], submission_keys: true } });
    const restart = p.match(/^\/console\/ssh\/restarts\/([^/]+)$/);
    if (restart) {
        const id = decodeURIComponent(restart[1]);
        if (req.method() === "GET") f.reads[id] = (f.reads[id] || 0) + 1;
        if (id === "node-old") return route.fulfill({ status: 400, json: { error: "这个节点不能从这里重启", step: { id: "restart", status: "blocked", message: "这个节点不能从这里重启", code: "restart_unsupported" } } });
        if (req.method() === "GET") return route.fulfill({ json: f.status[id] });
        f.posts.push(id);
        if (f.hold) await new Promise((resolve) => { f.release = resolve; });
        if (f.refuse) { const message = f.refuse; f.refuse = null; return route.fulfill({ status: 400, json: { error: message, step: { id: "restart", status: "blocked", message, code: "in_progress" } } }); }
        f.status[id] = { ...f.status[id], restart: f.result[id], automatic: false };
        return route.fulfill({ json: f.result[id] });
    }
    f.errors.push(req.method() + " " + p); return route.fulfill({ status: 500, json: { error: "Unmocked API" } });
});
async function waitFor(check, message) { for (let i = 0; i < 160; i++) { if (await check()) return; await new Promise((resolve) => setTimeout(resolve, 25)); } assert.fail(message); }
const machines = page.locator("#fleet-machines");
async function open(name) {
    await machines.getByRole("row", { name: new RegExp(name) }).click();
    const drawer = page.getByRole("dialog", { name, exact: true }); await drawer.waitFor();
    return drawer;
}
async function close(drawer) { await drawer.getByRole("button", { name: "关闭", exact: true }).click(); await drawer.waitFor({ state: "detached" }); }
try {
    await page.goto(url + "#/fleet?tab=machines");

    let drawer = await open("my-desktop");
    await drawer.getByText("节点 ID", { exact: true }).waitFor();
    await drawer.getByText("my-desktop 的节点进程已重启", { exact: true }).waitFor();
    await drawer.getByText(/手动 · 由 build-node 执行/).waitFor();
    await page.waitForTimeout(300);
    assert.equal(await drawer.getByRole("button", { name: "重启节点" }).count(), 0, "the coordinator is not restarted over SSH");
    assert.equal(f.reads["my-desktop"], undefined, "the coordinator's restart status is never asked for");
    await close(drawer);
    drawer = await open("old-node");
    await waitFor(() => f.reads["node-old"] >= 1, "the restart status of a worker is asked for");
    await drawer.getByText("old-node 的节点重启没有成功", { exact: true }).waitFor();
    await drawer.getByText("SSH 连不上这台机器", { exact: true }).waitFor();
    await page.waitForTimeout(300);
    assert.equal(await drawer.getByRole("button", { name: "重启节点" }).count(), 0, "a machine this node does not restart offers no restart");
    await close(drawer);
    console.log("PASS the restart is offered only where the node serving the console restarts the machine, and a last restart shows wherever");

    drawer = await open("far-node");
    await drawer.getByText(far, { exact: true }).waitFor();
    assert.equal(await drawer.getByRole("button", { name: "重启节点", exact: true }).isDisabled(), true, "a machine this node has no link to is not restarted from here");
    await drawer.getByRole("button", { name: "重启节点", exact: true }).click({ force: true });
    await page.waitForTimeout(200);
    assert.equal(await drawer.getByText(/重启会中断它上面正在运行的执行/).count(), 0);
    assert.deepEqual(f.posts, [], "a restart this node cannot run is never asked for");
    await close(drawer);
    drawer = await open("lag-node");
    await drawer.getByText("提供控制台的节点没有说明能否从这里重启这台机器，可能它运行的版本与本页面不同；刷新页面后再看", { exact: true }).waitFor();
    assert.equal(await drawer.getByRole("button", { name: "重启节点", exact: true }).isDisabled(), true, "a machine whose status does not say it can be restarted is not restarted from here");
    assert.deepEqual(f.posts, []);
    await close(drawer);
    console.log("PASS a machine this node cannot restart, or does not say it can, shows the restart disabled and says why");

    drawer = await open("build-node");
    const restart = drawer.getByRole("button", { name: "重启节点", exact: true });
    await restart.waitFor();
    await drawer.getByText("机器离线且节点进程不在时，会经 SSH 自动启动", { exact: true }).waitFor();
    await drawer.getByText("build-node 的节点进程已启动", { exact: true }).waitFor();
    await drawer.getByText(/自动拉起 · 由 my-desktop 执行/).waitFor();
    await restart.click();
    await drawer.getByText("这台机器在线。重启会中断它上面正在运行的执行，这些执行会按正常的停止流程确认结果。", { exact: true }).waitFor();
    assert.deepEqual(f.posts, [], "an online machine is not restarted before the restart is confirmed");
    await drawer.getByRole("button", { name: "取消", exact: true }).click();
    assert.equal(await drawer.getByText(/重启会中断它上面正在运行的执行/).count(), 0);
    assert.deepEqual(f.posts, []);
    console.log("PASS an online machine asks for confirmation and says what a restart interrupts");

    const states = f.states;
    await restart.click();
    f.hold = true; f.status["node-build"] = { ...f.status["node-build"], ...running() };
    await drawer.getByRole("button", { name: "确认重启", exact: true }).click();
    await waitFor(() => f.posts.length === 1, "one restart");
    await drawer.getByText("重启进度", { exact: true }).waitFor();
    await drawer.getByText("第 2/3 步 · 重启节点进程", { exact: true }).waitFor();
    await drawer.getByText("Stopping peer process 4242.", { exact: true }).waitFor();
    assert.equal(await drawer.getByText("重启日志 · 1 行", { exact: true }).count(), 1, "a restart's log is named a restart log");
    assert.equal(await drawer.getByLabel("重启日志", { exact: true }).count(), 1, "a restart's log is labelled a restart log");
    assert.equal(await drawer.getByText(/安装日志/).count(), 0, "a restart's log is not named an installation log");
    assert.equal(await drawer.getByRole("button", { name: "重启节点", exact: true }).isDisabled(), true, "a machine is restarted one restart at a time");
    f.hold = false; f.release();
    await drawer.getByText("全部 3 步已完成", { exact: true }).waitFor();
    await drawer.getByText("节点进程已重启，机器已回到集群", { exact: true }).waitFor();
    await waitFor(() => f.states > states, "the fleet is read again after a restart");
    assert.equal(await drawer.getByRole("button", { name: "重启节点", exact: true }).isDisabled(), false);
    assert.deepEqual(f.posts, ["node-build"]);
    console.log("PASS a confirmed restart shows its progress while it runs and its result once it settles");

    f.refuse = "这台机器正在升级";
    await drawer.getByRole("button", { name: "重启节点", exact: true }).click();
    await drawer.getByRole("button", { name: "确认重启", exact: true }).click();
    await drawer.getByRole("alert").filter({ hasText: "这台机器正在升级" }).waitFor();
    await close(drawer);
    console.log("PASS a refused restart says why");

    drawer = await open("gpu-node");
    await drawer.getByText("已停止自动拉起", { exact: true }).waitFor();
    await drawer.getByText("连续 5 次自动拉起后，这台机器都没能保持在线 10 分钟，已停止自动拉起", { exact: true }).waitFor();
    await drawer.getByText("机器稳定在线一段时间或手动重启成功后会恢复", { exact: true }).waitFor();
    await drawer.getByRole("button", { name: "重启节点", exact: true }).click();
    await waitFor(() => f.posts.length === 3, "an offline machine is restarted at once");
    assert.equal(await drawer.getByText(/重启会中断它上面正在运行的执行/).count(), 0, "an offline machine asks no confirmation");
    await drawer.getByText("节点进程已启动，机器已回到集群", { exact: true }).waitFor();
    console.log("PASS an offline machine is restarted at once, and a stopped automatic start says why and how it resumes");

    const autoStatus = drawer.locator('[aria-live="polite"]').filter({ hasText: "已停止自动拉起" });
    assert.equal(await autoStatus.count(), 1, "how automatic start stands is in a polite live region, so its changes are announced");
    const announced = await autoStatus.elementHandle();
    f.status["node-gpu"] = { restartable: true, ...running(true, "restart-2"), auto_start: { state: "attempting", attempts: 1, limit: 5, last_at: at } };
    await drawer.getByText("正在自动启动（第 2/5 次）", { exact: true }).waitFor({ timeout: 8000 });
    assert.match(await announced.innerText(), /正在自动启动（第 2\/5 次）/, "the live region already there announces the next state");
    await drawer.getByText("自动拉起进度", { exact: true }).waitFor();
    await drawer.getByText("第 2/3 步 · 重启节点进程", { exact: true }).waitFor();
    assert.equal(await drawer.getByRole("button", { name: "重启节点", exact: true }).isDisabled(), true, "a machine being started automatically is not restarted by hand meanwhile");
    f.status["node-gpu"] = { restartable: true, restart: { ...f.result["node-gpu"], plan_id: "restart-2" }, automatic: true, auto_start: { state: "watching", attempts: 1, limit: 5 } };
    await drawer.getByText("已自动启动，等待机器稳定在线（第 1/5 次）", { exact: true }).waitFor();
    assert.match(await announced.innerText(), /已自动启动，等待机器稳定在线（第 1\/5 次）/, "the same live region announces the machine started");
    assert.equal(await drawer.getByRole("button", { name: "重启节点", exact: true }).isDisabled(), false);
    assert.equal(await drawer.getByText("节点进程已启动，机器已回到集群", { exact: true }).count(), 0, "a restart asked here is not shown once a later one ran");
    await close(drawer);
    assert.deepEqual(f.errors, []);
    console.log("PASS an automatic start shows as it runs and once the machine is back");
} catch (error) { console.log("DEBUG", JSON.stringify(f.errors), await page.locator("body").innerText()); throw error; }
finally { await context.close(); await browser.close(); await server.close(); }
