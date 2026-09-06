import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { settingsInputs, settingsPatch } from "../../web/console/src/lib/settings-values.ts";

const values = () => ({ gateway: { locale: "", owner_id: "owner-fixture", task_max_turns: 0, task_max_elapsed: "0s", prompt_timeout: "10m" }, policies: { execution: { step_timeout: "15m", verify_timeout: "10m" }, planning: { timeout: "3m", attempts: 2 }, snapshot: { max_files: 20000, max_bytes: 1000, max_file_bytes: 100 }, review: { max_changes: 500, max_diff_bytes: 204800, max_file_bytes: 204800, max_entries: 2000, timeout: "30s" } } });
function fixtureView() {
    const desired = values(), fields = [];
    function walk(object, prefix = "") {
        for (const [key, value] of Object.entries(object)) {
            const name = prefix ? `${prefix}.${key}` : key;
            if (typeof value === "object") walk(value, name);
            else fields.push({ path: name, type: typeof value === "number" ? "integer" : name.endsWith("locale") || name.endsWith("owner_id") ? "string" : "duration", minimum: name === "gateway.task_max_turns" || name === "gateway.task_max_elapsed" ? 0 : 1, ...(typeof value === "number" ? { maximum: Number.MAX_SAFE_INTEGER, unit: name.endsWith("bytes") ? "bytes" : "count" } : {}), ...(name.endsWith("locale") ? { enum: ["", "zh", "en"] } : {}), default: value, apply_mode: "restart" });
        }
    }
    walk(desired);
    return { revision: "revision-a", desired, effective: structuredClone(desired), pending_restart: false, apply_mode: "restart", fields };
}

const view = fixtureView();
const input = settingsInputs(view);
assert.equal(input["gateway.owner_id"], undefined, "Owner identity must not be editable");
assert.deepEqual(settingsPatch(view, input), {});
const raw = { ...input, "gateway.task_max_turns": "7", "gateway.owner_id": "attacker", "unrecognized.field": "bad" };
assert.deepEqual(settingsPatch(view, raw), { gateway: { task_max_turns: 7 } }, "Only explicit changed schema fields may be submitted");
assert.equal(view.desired.gateway.task_max_turns, 0, "Parsing cannot mutate server values");
for (const invalid of ["", "1.5", "1e3", "-1", "9007199254740992"]) {
    const draft = { ...input, "gateway.task_max_turns": invalid };
    assert.throws(() => settingsPatch(view, draft));
    assert.equal(draft["gateway.task_max_turns"], invalid, "Invalid partial input must remain intact");
}
for (const duration of ["1h30m", "0.5s", "1ns", "15ms"]) assert.equal(settingsPatch(view, { ...input, "gateway.prompt_timeout": duration }).gateway.prompt_timeout, duration);
for (const duration of ["1h3", "0s", "-1m", "NaN", "1d", "0.1ns"]) assert.throws(() => settingsPatch(view, { ...input, "gateway.prompt_timeout": duration }));
assert.deepEqual(settingsPatch(view, { ...input, "gateway.task_max_elapsed": "0" }), { gateway: { task_max_elapsed: "0" } });
assert.throws(() => settingsPatch(view, { ...input, "gateway.locale": "fr" }));
assert.throws(() => settingsPatch(view, { ...input, "policies.snapshot.max_file_bytes": "1001" }), /快照/);
console.log("PASS settings field ranges, durations, partial input, identity isolation and cross-field limits");

if (process.env.PURE_ONLY !== "1") {
    const { chromium } = await import("../../web/console/node_modules/playwright/index.mjs");
    const { createServer } = await import("../../web/console/node_modules/vite/dist/node/index.js");
    const root = fileURLToPath(new URL("../../web/console/", import.meta.url));
    const server = await createServer({ root, configFile: path.join(root, "vite.config.ts"), server: { host: "127.0.0.1", port: 0, strictPort: false }, logLevel: "error" });
    await server.listen();
    const address = server.httpServer.address();
    const origin = `http://127.0.0.1:${address.port}`;
    const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
    try {
        const context = await browser.newContext({ locale: "zh-CN", viewport: { width: 1280, height: 1000 } });
        const page = await context.newPage(); page.setDefaultTimeout(6000);
        let state = fixtureView(), conflict = false, hold;
        const writes = [], errors = [], external = [];
        page.on("pageerror", (error) => errors.push(String(error)));
        await page.addInitScript(() => {
            localStorage.setItem("steve.ui.locale", "zh");
            window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
        });
        await page.route("**/*", async (route) => {
            const request = route.request(), url = new URL(request.url());
            if (url.origin !== origin) { external.push(url.href); return route.abort(); }
            if (url.pathname === "/state") return route.fulfill({ json: { at: "2026-09-07T00:00:00Z", hub: { node: "hub-fixture", version: "v1", started: "" }, nodes: [], agents: [], tasks: [], plans: [], projects: [], attempts: [], landings: [] } });
            if (url.pathname === "/console/settings") {
                if (request.method() === "GET") return route.fulfill({ json: state });
                const body = request.postDataJSON(); writes.push(body);
                if (conflict || body.base_revision !== state.revision) return route.fulfill({ status: 409, json: { error: "settings revision conflict" } });
                if (hold) await hold;
                assert.equal(body.settings.gateway?.owner_id, undefined);
                state = { ...state, revision: "revision-b", desired: { ...state.desired, gateway: { ...state.desired.gateway, ...body.settings.gateway } }, pending_restart: true };
                return route.fulfill({ json: state });
            }
            if (url.pathname === "/console/versions") return route.fulfill({ json: {
                hub: "v1", hub_id: "hub-fixture", protocol_min: 1, protocol_max: 2, automatic: false, discovery_configured: false,
                nodes: [{ name: "node-a", version: "v0", online: true, matches_hub: false, protocol: 1, features: [], os: "linux", arch: "amd64" }],
                projects: [{ project: "alpha", hub_id: "hub-fixture", epoch: 3, state: "active" }, { project: "beta", hub_id: "hub-fixture", epoch: 4, state: "released", transfer_id: "transfer-fixture", target_hub: "hub-peer" }],
                peers: [{ id: "hub-peer", name: "Peer fixture", url: "https://must-not-be-requested.invalid/" }],
            } });
            if (url.pathname.startsWith("/console/")) { errors.push(`Unexpected API ${url.pathname}`); return route.fulfill({ status: 501, json: { error: "Unmocked API" } }); }
            return route.continue();
        });
        await page.goto(`${origin}/#/settings`);
        const turn = page.getByRole("textbox", { name: "任务回合上限", exact: true });
        await turn.waitFor();
        await page.getByText("与 Hub 版本不同", { exact: true }).waitFor();
        await page.getByText("项目管理归属", { exact: true }).click();
        const ownership = page.getByRole("table", { name: "项目管理归属", exact: true });
        await ownership.getByRole("row", { name: /alpha/ }).getByText("管理中", { exact: true }).waitFor();
        const released = ownership.getByRole("row", { name: /beta/ });
        await released.getByText("已释放", { exact: true }).waitFor();
        assert.match(await released.innerText(), /4.*hub-peer/s);
        await page.getByText("已配置的其他 Hub", { exact: true }).click();
        await page.getByText("https://must-not-be-requested.invalid/", { exact: true }).waitFor();
        assert.equal(writes.length, 0, "Inspecting ownership and peers must not submit operations");
        assert.equal(await page.getByRole("textbox", { name: "所有者身份", exact: true }).count(), 0);
        const timeout = page.getByRole("textbox", { name: "静默超时", exact: true });
        await timeout.fill("1h3");
        await page.getByRole("button", { name: "保存配置", exact: true }).click();
        await page.getByRole("alert").filter({ hasText: "有效时长" }).waitFor();
        assert.equal(await timeout.inputValue(), "1h3"); assert.equal(writes.length, 0);
        await page.getByRole("button", { name: "偏好设置", exact: true }).click();
        await page.getByRole("menuitem", { name: "English", exact: true }).click();
        await page.getByRole("textbox", { name: "Idle timeout", exact: true }).waitFor();
        await page.getByRole("table", { name: "Project ownership", exact: true }).getByRole("row", { name: /beta/ }).getByText("Released", { exact: true }).waitFor();
        assert.equal(await page.getByRole("textbox", { name: "Idle timeout", exact: true }).inputValue(), "1h3");
        await page.getByRole("alert").filter({ hasText: "valid duration" }).waitFor();
        await page.getByRole("textbox", { name: "Idle timeout", exact: true }).fill("10m");
        await page.getByRole("textbox", { name: "Task turn limit", exact: true }).fill("7");
        let release; hold = new Promise((resolve) => { release = resolve; });
        const save = page.getByRole("button", { name: "Save settings", exact: true });
        const saveElement = await save.elementHandle();
        await saveElement.dispatchEvent("click"); await saveElement.dispatchEvent("click");
        await page.waitForFunction(() => document.querySelector('[data-setting="gateway.task_max_turns"] input')?.disabled);
        release(); hold = null;
        await page.getByText("Saved · Restart required", { exact: true }).waitFor();
        assert.equal(writes.length, 1); assert.equal(writes[0].base_revision, "revision-a");
        assert.equal(await page.locator('[data-setting="gateway.task_max_turns"] [data-effective]').innerText(), "0");
        assert.equal(await page.locator('[data-setting="gateway.task_max_turns"] [data-desired]').innerText(), "7");
        conflict = true;
        await page.getByRole("textbox", { name: "Task turn limit", exact: true }).fill("8");
        await save.click(); await page.getByRole("alert").filter({ hasText: "Another operation" }).waitFor();
        assert.equal(await page.getByRole("textbox", { name: "Task turn limit", exact: true }).inputValue(), "8");
        await page.getByRole("button", { name: "Reload", exact: true }).first().click();
        await page.getByRole("dialog", { name: "Discard draft and reload?", exact: true }).waitFor();
        await page.getByRole("button", { name: "Cancel", exact: true }).click();
        assert.equal(await page.getByRole("textbox", { name: "Task turn limit", exact: true }).inputValue(), "8");
        conflict = false; state.revision = "revision-c"; state.desired.gateway.task_max_turns = 9;
        await page.getByRole("button", { name: "Reload", exact: true }).first().click();
        await page.getByRole("button", { name: "Discard draft and reload", exact: true }).click();
        await page.waitForFunction(() => document.querySelector('[data-setting="gateway.task_max_turns"] input')?.value === "9");
        assert.equal(writes.length, 2); assert.deepEqual(external, []); assert.deepEqual(errors, []);
        console.log("PASS isolated settings UI: raw input, live locale, one save, restart state, revision conflict, confirmed reload and read-only Hub ownership/peers");
        await context.close();
    } finally { await browser.close(); await server.close(); }
}
