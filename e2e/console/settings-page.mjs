import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { channelInputs, channelPatch, changedInputs } from "../../web/console/src/lib/settings-channels.ts";
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

const channelValues = () => ({ default_channel: "console", console: { enabled: true, owner_id: "owner-fixture" }, feishu: { enabled: false, app_id: "", app_secret_configured: false, domain: "feishu", owner_open_id: "", group_policy: "open", allow_unmentioned: false, allowed_senders: [], blocked_senders: [] } });
const channelView = () => ({ revision: "revision-a", desired: channelValues(), effective: channelValues(), pending_restart: false, apply_mode: "restart" });
const channel = channelView(), channelRaw = channelInputs(channel);
assert.deepEqual(channelPatch(channel, channelRaw), {});
assert.throws(() => channelPatch(channel, { ...channelRaw, default_channel: "feishu" }));
assert.throws(() => channelPatch(channel, { ...channelRaw, enabled: true, app_id: "app" }));
assert.deepEqual(channelPatch(channel, { ...channelRaw, enabled: true, app_id: "app", secret: " value " }).feishu.app_secret, { action: "replace", value: " value " });
const configured = channelView(); configured.desired.feishu.app_secret_configured = true;
assert.equal(channelPatch(configured, { ...channelInputs(configured), app_id: "changed" }).feishu.app_secret, undefined, "Blank secret preserves credentials");
assert.deepEqual(channelPatch(configured, { ...channelInputs(configured), clearSecret: true }).feishu.app_secret, { action: "clear" });
assert.deepEqual(changedInputs({ one: "old", two: "stay" }, { one: "typed", two: "stay" }), { one: "typed" });
console.log("PASS channel raw drafts, credential keep/replace/clear and invalid default channel");

if (process.env.PURE_ONLY !== "1") {
    const { chromium } = await import("../../web/console/node_modules/playwright/index.mjs");
    const { createServer } = await import("../../web/console/node_modules/vite/dist/node/index.js");
    const root = fileURLToPath(new URL("../../web/console/", import.meta.url));
    const server = await createServer({ root, configFile: path.join(root, "vite.config.ts"), server: { host: "127.0.0.1", port: 0, strictPort: false, hmr: false }, logLevel: "error" });
    await server.listen();
    const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
    const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
    const screenshots = process.env.SETTINGS_SCREENSHOTS;
    const { mkdir } = await import("node:fs/promises");
    if (screenshots) await mkdir(screenshots, { recursive: true });
    try {
        const context = await browser.newContext({ locale: "zh-CN", reducedMotion: "reduce", viewport: { width: 1280, height: 960 } });
        const page = await context.newPage(); page.setDefaultTimeout(7000);
        let state = fixtureView(), channels = channelView(), configRevision = "revision-a", conflict = false, loseRestart = true, restartUnknown = true;
        const writes = [], errors = [], external = [], operations = new Map(), restartPosts = [], queries = [];
        let settingsReads = 0, versionsReads = 0;
        await page.addInitScript(() => {
            localStorage.setItem("steve.ui.locale", "zh");
            window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
        });
        page.on("pageerror", (error) => errors.push(String(error)));
        await page.route("**/*", async (route) => {
            const request = route.request(), url = new URL(request.url());
            if (url.origin !== origin) { external.push(url.href); return route.abort(); }
            if (url.pathname === "/state") return route.fulfill({ json: { at: "2026-09-07T00:00:00Z", hub: { node: "hub-fixture", version: "v1", started: "" }, nodes: [], agents: [], tasks: [], plans: [], projects: [], attempts: [], landings: [] } });
            if (url.pathname === "/console/settings") {
                if (request.method() === "GET") { settingsReads++; return route.fulfill({ json: { ...state, revision: configRevision } }); }
                const body = request.postDataJSON(); writes.push({ group: "hub", ...body });
                if (conflict || body.base_revision !== configRevision) return route.fulfill({ status: 409, json: { code: "config_conflict", error: "settings revision conflict" } });
                assert.equal(body.settings.gateway?.owner_id, undefined);
                configRevision = `revision-${writes.length}`;
                state = { ...state, revision: configRevision, desired: { ...state.desired, gateway: { ...state.desired.gateway, ...body.settings.gateway } }, pending_restart: true };
                return route.fulfill({ json: state });
            }
            if (url.pathname === "/console/channels") {
                if (request.method() === "GET") return route.fulfill({ json: { ...channels, revision: configRevision } });
                const body = request.postDataJSON(); writes.push({ group: "channels", ...body });
                if (body.base_revision !== configRevision) return route.fulfill({ status: 409, json: { error: "channel revision conflict" } });
                configRevision = `revision-${writes.length}`;
                const { app_secret, ...patch } = body.channels.feishu || {};
                channels = { ...channels, revision: configRevision, desired: { ...channels.desired, ...body.channels, feishu: { ...channels.desired.feishu, ...patch, app_secret_configured: app_secret ? app_secret.action === "replace" : channels.desired.feishu.app_secret_configured } }, pending_restart: true };
                return route.fulfill({ json: channels });
            }
            if (url.pathname === "/console/services") return route.fulfill({ json: { services: [{ name: "hub", kind: "hub", label: "Main Hub", online: true, version: "v1", supported: true }, { name: "node-a", kind: "node", label: "Node A", online: true, version: "v1", supported: true }] } });
            const service = /^\/console\/services\/(.+)\/restart$/.exec(url.pathname);
            if (service) {
                const name = decodeURIComponent(service[1]);
                if (request.method() === "POST") {
                    const body = request.postDataJSON(); restartPosts.push({ name, id: body.command_id });
                    if (name === "node-a") return route.fulfill({ status: 409, json: { code: "service_busy", error: "service_busy: active execution" } });
                    operations.set(body.command_id, { command_id: body.command_id, state: "accepted", incarnation: 100 });
                    if (loseRestart) { loseRestart = false; return route.abort("connectionreset"); }
                    return route.fulfill({ json: operations.get(body.command_id) });
                }
                const id = url.searchParams.get("command_id"); queries.push({ name, id });
                if (restartUnknown || !operations.has(id)) return route.fulfill({ status: 503, json: { error: "temporarily disconnected" } });
                return route.fulfill({ json: operations.get(id) });
            }
            if (url.pathname === "/console/versions") { versionsReads++; return route.fulfill({ json: { hub: "v1", hub_id: "hub-fixture", protocol_min: 1, protocol_max: 2, automatic: false, discovery_configured: false, nodes: [], projects: [], peers: [] } }); }
            if (url.pathname.startsWith("/console/")) { errors.push(`Unexpected API ${url.pathname}`); return route.fulfill({ status: 501, json: { error: "Unmocked API" } }); }
            return route.continue();
        });
        await page.goto(`${origin}/#/projects`);
        await page.getByRole("link", { name: "设置", exact: true }).click();
        const nav = page.getByRole("navigation", { name: "设置分类", exact: true });
        await nav.waitFor();
        assert.equal(await page.getByRole("textbox", { name: "任务回合上限", exact: true }).count(), 0, "General must not flatten all policies");
        assert.equal(await page.getByRole("link", { name: "设置", exact: true }).count(), 1, "Only the footer gear opens settings");
        if (screenshots) await page.screenshot({ path: path.join(screenshots, "general-desktop.png") });
        await nav.getByRole("link", { name: "执行与资源", exact: true }).click();
        const turns = page.getByRole("textbox", { name: "任务回合上限", exact: true }); await turns.waitFor();
        await turns.fill("7");
        page.once("dialog", (dialog) => dialog.dismiss());
        await page.evaluate(() => window.history.go(-2));
        await page.waitForTimeout(600);
        assert.match(page.url(), /settings/); assert.equal(await turns.inputValue(), "7", "Cancelling browser back preserves the draft");
        await nav.getByRole("link", { name: "Channel", exact: true }).click();
        await page.getByRole("switch", { name: "启用 Feishu / Lark", exact: true }).focus();
        await page.getByRole("switch", { name: "启用 Feishu / Lark", exact: true }).press("Space");
        await page.getByRole("textbox", { name: "App ID", exact: true }).fill("app-fixture");
        await page.getByRole("button", { name: "保存 Channel 设置", exact: true }).click();
        await page.getByRole("alert").filter({ hasText: "有效的 App Secret" }).waitFor(); assert.equal(writes.length, 0);
        const beforeChannelSaveReads = settingsReads;
        await page.getByRole("textbox", { name: "App Secret", exact: true }).fill("secret-fixture");
        await page.getByRole("button", { name: "保存 Channel 设置", exact: true }).click();
        await page.getByText("已配置 App Secret", { exact: true }).waitFor();
        assert.equal(await page.getByRole("textbox", { name: "App Secret", exact: true }).inputValue(), "");
        assert.equal(writes[0].channels.feishu.app_secret.action, "replace");
        assert.equal(settingsReads, beforeChannelSaveReads, "A dirty Hub draft must not be refreshed after channel save");
        await nav.getByRole("link", { name: "执行与资源", exact: true }).click();
        assert.equal(await turns.inputValue(), "7");
        await page.getByRole("button", { name: "读取最新值并保留草稿", exact: true }).click();
        await page.getByRole("button", { name: "保留草稿并读取", exact: true }).click();
        await page.getByRole("status").filter({ hasText: "已读取最新配置" }).waitFor();
        assert.equal(await turns.inputValue(), "7");
        await page.getByRole("button", { name: "保存 Hub 设置", exact: true }).click();
        await page.locator('[data-setting="gateway.task_max_turns"] [data-desired]').filter({ hasText: "7" }).waitFor();
        assert.equal(writes[1].base_revision, "revision-1");
        assert.equal(await page.locator('[data-setting="gateway.task_max_turns"] [data-effective]').innerText(), "0");
        await turns.fill("8"); conflict = true;
        await page.getByRole("button", { name: "保存 Hub 设置", exact: true }).click();
        await page.getByRole("alert").filter({ hasText: "配置版本已变化" }).waitFor();
        assert.equal(await turns.inputValue(), "8");
        page.once("dialog", (dialog) => dialog.dismiss());
        await page.locator('a[href="#/projects"]').click();
        assert.match(page.url(), /settings/); assert.equal(await turns.inputValue(), "8");
        conflict = false;
        await page.getByRole("button", { name: "放弃草稿并重新读取", exact: true }).click();
        await page.getByRole("button", { name: "放弃草稿并读取", exact: true }).click();
        await page.waitForFunction(() => document.querySelector('[data-setting="gateway.task_max_turns"] input')?.value === "7");
        if (screenshots) await page.screenshot({ path: path.join(screenshots, "policies-desktop.png") });
        await nav.getByRole("link", { name: "节点与服务", exact: true }).click();
        const hubRow = page.locator('.settings-service-list > li').filter({ hasText: "Main Hub" });
        await hubRow.getByRole("button", { name: "重启服务", exact: true }).click();
        await page.getByRole("dialog", { name: "重启「Main Hub」？", exact: true }).waitFor();
        await page.getByRole("button", { name: "确认重启", exact: true }).click();
        await hubRow.getByText("重启结果尚未确认", { exact: true }).waitFor();
        const id = restartPosts[0].id;
        await nav.getByRole("link", { name: "通用", exact: true }).click();
        await nav.getByRole("link", { name: "节点与服务", exact: true }).click();
        await hubRow.getByText("重启结果尚未确认", { exact: true }).waitFor();
        await page.waitForTimeout(2000); assert.equal(restartPosts.length, 1, "Polling must never resend a restart");
        assert.ok(queries.some((query) => query.id === id));
        await hubRow.getByRole("button", { name: "重试原请求", exact: true }).click();
        await hubRow.getByText("已接受，等待重启确认", { exact: true }).waitFor();
        assert.equal(restartPosts[1].id, id);
        operations.set(id, { command_id: id, state: "restarted", incarnation: 101, previous_incarnation: 100 }); restartUnknown = false;
        channels.runtime_error = "Application credentials rejected";
        await hubRow.getByRole("button", { name: "重新核对", exact: true }).click();
        await hubRow.getByText("已确认重启", { exact: true }).waitFor();
        const nodeRow = page.locator('.settings-service-list > li').filter({ hasText: "Node A" });
        await nodeRow.getByRole("button", { name: "重启服务", exact: true }).click(); await page.getByRole("button", { name: "确认重启", exact: true }).click();
        await nodeRow.getByText("service_busy: active execution", { exact: true }).waitFor();
        assert.equal(restartPosts.length, 3); await nodeRow.getByText("重启失败", { exact: true }).waitFor();
        assert.equal(versionsReads, 0, "Advanced identity/protocol reads are deferred");
        if (screenshots) await page.screenshot({ path: path.join(screenshots, "services-desktop.png") });
        await nav.getByRole("link", { name: "通用", exact: true }).click();
        await page.getByRole("heading", { name: "通用", exact: true }).waitFor();
        await page.getByRole("button", { name: "简体中文 语言", exact: true }).click();
        await page.getByRole("option", { name: "English", exact: true }).click();
        await page.getByRole("heading", { name: "General", exact: true }).waitFor();
        await page.getByRole("navigation", { name: "Settings categories", exact: true }).getByRole("link", { name: "Channels", exact: true }).click();
        await page.getByRole("alert").filter({ hasText: "Channel startup failed" }).waitFor();
        await page.getByText("Error details", { exact: true }).click();
        await page.getByText("Application credentials rejected", { exact: true }).waitFor();
        await page.setViewportSize({ width: 390, height: 844 });
        await page.waitForTimeout(150);
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "Narrow settings must not overflow the viewport");
        const englishNav = page.getByRole("navigation", { name: "Settings categories", exact: true });
        await englishNav.getByRole("link", { name: "Channels", exact: true }).click();
        await page.getByRole("textbox", { name: "App ID", exact: true }).fill("edited-id");
        await englishNav.getByRole("link", { name: "General", exact: true }).click();
        await englishNav.getByRole("link", { name: "Channels", exact: true }).click();
        assert.equal(await page.getByRole("textbox", { name: "App ID", exact: true }).inputValue(), "edited-id");
        if (screenshots) await page.screenshot({ path: path.join(screenshots, "channels-mobile.png"), fullPage: true });
        await page.getByRole("button", { name: "Save channel settings", exact: true }).click();
        await page.getByText("App Secret is configured", { exact: true }).waitFor();
        assert.equal(writes.at(-1).channels.feishu.app_secret, undefined, "Blank secret must not clear or resend credentials");
        await page.getByRole("switch", { name: "Enable Feishu / Lark", exact: true }).focus();
        await page.getByRole("switch", { name: "Enable Feishu / Lark", exact: true }).press("Space");
        await page.getByRole("switch", { name: "Clear the existing secret", exact: true }).focus();
        await page.getByRole("switch", { name: "Clear the existing secret", exact: true }).press("Space");
        await page.getByRole("button", { name: "Save channel settings", exact: true }).click();
        await page.getByText("App Secret is not configured", { exact: true }).waitFor();
        assert.deepEqual(writes.at(-1).channels.feishu.app_secret, { action: "clear" });
        assert.deepEqual(errors, []); assert.deepEqual(external, []);
        console.log("PASS settings center browser: focused groups, preserved drafts, channel secrets/CAS, guarded navigation, restart identity/polling/busy, English and narrow layout");
        await context.close();
    } finally { await browser.close(); await server.close(); }
}
