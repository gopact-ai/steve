import { workState } from "./work-fixture.mjs";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { channelInputs, channelPatch, changedInputs } from "../../web/console/src/lib/settings-channels.ts";
import { settingsInputs, settingsPatch } from "../../web/console/src/lib/settings-values.ts";

const values = () => ({ gateway: { locale: "", owner_id: "owner-fixture", default_approval: "", task_max_turns: 0, task_max_elapsed: "0s", prompt_timeout: "10m" }, policies: { execution: { step_timeout: "15m", verify_timeout: "10m" }, planning: { timeout: "3m", attempts: 2 }, snapshot: { max_files: 20000, max_bytes: 1000, max_file_bytes: 100 }, review: { max_changes: 500, max_diff_bytes: 204800, max_file_bytes: 204800, max_entries: 2000, timeout: "30s" }, landing: { conflicts: "agent" } } });
function fixtureView(mode = "restart") {
    const desired = values(), fields = [];
    function walk(object, prefix = "") {
        for (const [key, value] of Object.entries(object)) {
            const name = prefix ? `${prefix}.${key}` : key;
            if (typeof value === "object") walk(value, name);
            else fields.push({ path: name, type: typeof value === "number" ? "integer" : name.endsWith("locale") || name.endsWith("owner_id") ? "string" : "duration", minimum: name === "gateway.task_max_turns" || name === "gateway.task_max_elapsed" ? 0 : 1, ...(typeof value === "number" ? { maximum: Number.MAX_SAFE_INTEGER, unit: name.endsWith("bytes") ? "bytes" : "count" } : {}), ...(name.endsWith("locale") ? { enum: ["", "zh", "en"] } : {}), ...(name.endsWith("default_approval") ? { type: "string", enum: ["", "ask", "auto", "full"] } : {}), ...(name === "policies.landing.conflicts" ? { type: "string", enum: ["agent", "manual"] } : {}), default: value, apply_mode: mode === "restart" ? "restart" : name === "gateway.owner_id" ? "deployment" : ["gateway.task_max_turns", "gateway.task_max_elapsed"].includes(name) ? "next_task" : "next_operation" });
        }
    }
    walk(desired);
    const effective = structuredClone(desired);
    effective.gateway.locale = "zh";
    return { revision: "revision-a", desired, effective, pending_restart: false, apply_mode: mode, fields };
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
assert.throws(() => settingsPatch(view, { ...input, "gateway.default_approval": "automode" }));
assert.deepEqual(settingsPatch(view, { ...input, "gateway.default_approval": "full" }), { gateway: { default_approval: "full" } });
assert.equal(input["policies.landing.conflicts"], "agent", "Landing conflict policy must be editable when offered by the server");
assert.deepEqual(settingsPatch(view, { ...input, "policies.landing.conflicts": "manual" }), { policies: { landing: { conflicts: "manual" } } });
assert.throws(() => settingsPatch(view, { ...input, "policies.landing.conflicts": "merge" }));
const olderView = { ...view, fields: view.fields.filter((field) => field.path !== "policies.landing.conflicts") };
assert.equal(settingsInputs(olderView)["policies.landing.conflicts"], undefined);
assert.deepEqual(settingsPatch(olderView, { ...settingsInputs(olderView), "policies.landing.conflicts": "manual" }), {}, "Do not submit fields absent from an older server schema");
const liveView = fixtureView("live");
assert.equal(settingsInputs(liveView)["gateway.owner_id"], undefined, "Deployment identity stays read-only");
assert.deepEqual(settingsPatch(liveView, { ...settingsInputs(liveView), "gateway.task_max_turns": "0", "gateway.task_max_elapsed": "0", "gateway.owner_id": "attacker" }), { gateway: { task_max_elapsed: "0" } }, "Zero budgets remain valid and identity is never submitted");
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
        const writes = [], errors = [], external = [], operations = new Map(), restartPosts = [], queries = [], approvalSyncs = [];
        let settingsReads = 0, versionsReads = 0;
        await page.addInitScript(() => {
            localStorage.setItem("steve.ui.locale", "zh");
            window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
        });
        page.on("pageerror", (error) => errors.push(String(error)));
        await page.route("**/*", async (route) => {
            const request = route.request(), url = new URL(request.url());
            if (url.origin !== origin) { external.push(url.href); return route.abort(); }
            if (url.pathname === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false } });
            if (url.pathname === "/console/desktop") return route.fulfill({ json: { enabled: false, setup_required: false, agent_count: 0 } });
            if (url.pathname === "/state") return route.fulfill({ json: workState({ at: "2026-09-07T00:00:00Z", hub: { node: "hub-fixture", version: "v1", started: "" }, nodes: [], agents: [], tasks: [], plans: [], projects: [], attempts: [], landings: [] }) });
            if (url.pathname === "/console/queue" && request.method() === "GET") return route.fulfill({ json: { queue: [], submission_keys: true, material_refs: true, interactive_requests: true } });
            if (url.pathname === "/console/settings") {
                if (request.method() === "GET") { settingsReads++; return route.fulfill({ json: { ...state, revision: configRevision } }); }
                const body = request.postDataJSON(); writes.push({ group: "hub", ...body });
                if (conflict || body.base_revision !== configRevision) return route.fulfill({ status: 409, json: { code: "config_conflict", error: "settings revision conflict" } });
                assert.equal(body.settings.gateway?.owner_id, undefined);
                configRevision = `revision-${writes.length}`;
                state = { ...state, revision: configRevision, desired: { ...state.desired, gateway: { ...state.desired.gateway, ...body.settings.gateway } }, pending_restart: true };
                for (const [group, patch] of Object.entries(body.settings.policies || {})) state.desired.policies[group] = { ...state.desired.policies[group], ...patch };
                if (state.apply_mode === "live") {
                    state.effective = structuredClone(state.desired);
                    state.effective.gateway.locale ||= "zh";
                    state.pending_restart = false;
                }
                return route.fulfill({ json: state });
            }
            if (url.pathname === "/console/channels") {
                if (request.method() === "GET") return route.fulfill({ json: { ...channels, revision: configRevision } });
                const body = request.postDataJSON(); writes.push({ group: "channels", ...body });
                if (body.base_revision !== configRevision) return route.fulfill({ status: 409, json: { error: "channel revision conflict" } });
                configRevision = `revision-${writes.length}`;
                const { app_secret, ...patch } = body.channels.feishu || {};
                channels = { ...channels, revision: configRevision, desired: { ...channels.desired, ...body.channels, feishu: { ...channels.desired.feishu, ...patch, app_secret_configured: app_secret ? app_secret.action === "replace" : channels.desired.feishu.app_secret_configured } }, pending_restart: true };
                if (channels.apply_mode === "mixed") {
                    for (const path of channels.live_fields || []) {
                        const key = path.replace(/^feishu\./, "");
                        channels.effective.feishu[key] = structuredClone(channels.desired.feishu[key]);
                    }
                    channels.pending_restart = !!app_secret || JSON.stringify(channels.desired) !== JSON.stringify(channels.effective);
                }
                return route.fulfill({ json: channels });
            }
            if (url.pathname === "/console/services") return route.fulfill({ json: { services: [{ name: "hub", kind: "hub", label: "Coordinator", online: true, version: "v1", supported: true }, { name: "node-a", kind: "node", label: "Node A", online: true, version: "v1", supported: true }] } });
            const service = /^\/console\/services\/(.+)\/restart$/.exec(url.pathname);
            if (service) {
                const name = decodeURIComponent(service[1]);
                if (request.method() === "POST") {
                    const body = request.postDataJSON(); restartPosts.push({ name, id: body.command_id, mode: body.mode, cancel: body.cancel });
                    if (name === "node-a") return route.fulfill({ status: 409, json: { code: "service_busy", error: "service_busy: active execution" } });
                    if (body.cancel) {
                        operations.set(body.command_id, { command_id: body.command_id, state: "cancelled", incarnation: 100 });
                        return route.fulfill({ json: operations.get(body.command_id) });
                    }
                    if (body.mode === "when-idle") {
                        operations.set(body.command_id, { command_id: body.command_id, state: "draining", incarnation: 100, mode: "when-idle", waiting_on: "question", waiting_conversations: ["console:one"] });
                        return route.fulfill({ json: operations.get(body.command_id) });
                    }
                    operations.set(body.command_id, { command_id: body.command_id, state: "accepted", incarnation: 100 });
                    if (loseRestart) { loseRestart = false; return route.abort("connectionreset"); }
                    return route.fulfill({ json: operations.get(body.command_id) });
                }
                const id = url.searchParams.get("command_id"); queries.push({ name, id });
                if (restartUnknown || !operations.has(id)) return route.fulfill({ status: 503, json: { error: "temporarily disconnected" } });
                return route.fulfill({ json: operations.get(id) });
            }
            if (url.pathname === "/console/agents/approval") { approvalSyncs.push(request.method()); configRevision = `revision-approval-${approvalSyncs.length}`; return route.fulfill({ json: { intent: "full", cleared: [{ agent: "dev", was: "read-only" }], following: ["planner"], unmapped: ["dev-claude"] } }); }
            if (url.pathname === "/console/conversations") return route.fulfill({ json: { enabled: true, conversations: [{ id: "console:one", title: "发布流程", last_at: "", count: 1, running: false }] } });
            if (url.pathname === "/console/versions") { versionsReads++; return route.fulfill({ json: { hub: "v1", hub_id: "hub-fixture", protocol_min: 1, protocol_max: 2, automatic: false, discovery_configured: false, nodes: [], projects: [], peers: [] } }); }
            if (url.pathname.startsWith("/console/")) { errors.push(`Unexpected API ${url.pathname}`); return route.fulfill({ status: 501, json: { error: "Unmocked API" } }); }
            return route.continue();
        });
        await page.goto(`${origin}/#/projects`);
        await page.getByRole("link", { name: "设置", exact: true }).click();
        const nav = page.getByRole("navigation", { name: "设置分类", exact: true });
        await nav.waitFor();
        await page.locator('[data-setting="gateway.locale"]').waitFor();
        assert.equal(await page.locator('[data-setting="gateway.locale"] .settings-restart-link').count(), 0, "Resolved automatic language is not pending restart");
        assert.doesNotMatch(await page.locator(".settings-savebar").innerText(), /要重启|才生效/, "Idle save bar must not claim every server setting requires restart");
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
        const cancelledAgain = page.waitForEvent("dialog");
        await page.evaluate(() => window.history.back());
        await (await cancelledAgain).dismiss();
        await page.waitForTimeout(150);
        assert.match(page.url(), /settings/);
        assert.equal(await turns.inputValue(), "7", "Repeated cancelled navigation must keep the same form mounted");
        const confirmedLeave = page.waitForEvent("dialog");
        await page.evaluate(() => window.history.back());
        await (await confirmedLeave).accept();
        await page.waitForURL("**/#/projects");
        await page.getByRole("link", { name: "设置", exact: true }).click();
        await nav.getByRole("link", { name: "执行与资源", exact: true }).click();
        await turns.waitFor();
        assert.equal(await turns.inputValue(), "0", "Confirmed navigation releases the old draft and guard");
        await turns.fill("7");
        await nav.getByRole("link", { name: "接入渠道", exact: true }).click();
        await page.getByRole("switch", { name: "启用 Feishu / Lark", exact: true }).focus();
        await page.getByRole("switch", { name: "启用 Feishu / Lark", exact: true }).press("Space");
        await page.getByRole("textbox", { name: "App ID", exact: true }).fill("app-fixture");
        await page.getByRole("button", { name: "保存渠道设置", exact: true }).click();
        await page.getByRole("alert").filter({ hasText: "有效的 App Secret" }).waitFor(); assert.equal(writes.length, 0);
        const beforeChannelSaveReads = settingsReads;
        await page.getByRole("textbox", { name: "App Secret", exact: true }).fill("secret-fixture");
        await page.getByRole("button", { name: "保存渠道设置", exact: true }).click();
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
        await page.getByRole("button", { name: "保存系统设置", exact: true }).click();
        await page.locator('[data-setting="gateway.task_max_turns"] [data-desired]').filter({ hasText: "7" }).waitFor();
        assert.equal(writes[1].base_revision, "revision-1");
        assert.equal(await page.locator('[data-setting="gateway.task_max_turns"] [data-effective]').innerText(), "0");
        // A saved value that is not running yet has to say where it is made
        // to run. The badge and the row both lead to Nodes & services, which
        // is the restart the reader needs; closing the app is not one.
        const pendingBadge = page.getByRole("link", { name: "去「节点与服务」重启协调节点", exact: true });
        await pendingBadge.waitFor();
        await nav.getByRole("link", { name: "通用", exact: true }).click();
        await page.locator('[data-setting="gateway.locale"]').waitFor();
        assert.equal(await page.locator('[data-setting="gateway.locale"] .settings-restart-link').count(), 0, "Another field pending restart must not turn inherited language resolution into a pending change");
        await nav.getByRole("link", { name: "执行与资源", exact: true }).click();
        await page.locator('[data-setting="gateway.task_max_turns"]').getByRole("link", { name: "去重启服务", exact: true }).waitFor();
        await pendingBadge.click();
        await page.getByRole("heading", { name: "节点与服务", exact: true }).waitFor();
        assert.match(page.url(), /section=services/, "The pending badge opens the page that restarts the service");
        await nav.getByRole("link", { name: "执行与资源", exact: true }).click();
        await turns.waitFor();
        await turns.fill("8"); conflict = true;
        await page.getByRole("button", { name: "保存系统设置", exact: true }).click();
        await page.getByRole("alert").filter({ hasText: "配置版本已变化" }).waitFor();
        assert.equal(await turns.inputValue(), "8");
        page.once("dialog", (dialog) => dialog.dismiss());
        await page.locator('a[href="#/projects"]').click();
        assert.match(page.url(), /settings/); assert.equal(await turns.inputValue(), "8");
        conflict = false;
        await page.getByRole("button", { name: "放弃草稿并重新读取", exact: true }).click();
        await page.getByRole("button", { name: "放弃草稿并读取", exact: true }).click();
        await page.waitForFunction(() => document.querySelector('[data-setting="gateway.task_max_turns"] input')?.value === "7");
        // The approval stance is set once for the fleet: the agents that pinned
        // a mode of their own are let go of it on request, and only from what
        // the hub has already saved.
        await nav.getByRole("link", { name: "审批与权限", exact: true }).click();
        await page.getByText("会话单独设置 → Agent 默认 → 全局默认 → AI 工具默认", { exact: true }).waitFor();
        const approvalSelect = page.locator('[data-setting="gateway.default_approval"] button').first();
        const syncButton = page.getByRole("button", { name: "恢复 Agent 跟随", exact: true });
        assert.equal(await syncButton.isDisabled(), true, "Syncing an unset stance must be refused");
        await approvalSelect.click();
        await page.getByRole("option", { name: "完全放行", exact: true }).click();
        assert.equal(await syncButton.isDisabled(), true, "An unsaved stance must be saved before it is handed to the fleet");
        await page.getByRole("button", { name: "保存系统设置", exact: true }).click();
        await page.locator('[data-setting="gateway.default_approval"] [data-desired]').filter({ hasText: "完全放行" }).waitFor();
        assert.equal(writes.at(-1).settings.gateway.default_approval, "full");
        await nav.getByRole("link", { name: "接入渠道", exact: true }).click();
        await page.getByRole("textbox", { name: "App ID", exact: true }).fill("channel-draft-kept");
        await nav.getByRole("link", { name: "审批与权限", exact: true }).click();
        assert.deepEqual(approvalSyncs, [], "Saving a global default must never clear Agent overrides");
        await syncButton.click();
        const resetDialog = page.getByRole("dialog", { name: "恢复所有 Agent 跟随全局默认？", exact: true });
        await resetDialog.waitFor();
        assert.deepEqual(approvalSyncs, [], "Opening confirmation must not write");
        await resetDialog.getByRole("button", { name: "取消", exact: true }).click();
        assert.deepEqual(approvalSyncs, [], "Cancelling must preserve overrides");
        await syncButton.click();
        await resetDialog.getByRole("button", { name: "确认恢复", exact: true }).click();
        await page.getByRole("status").filter({ hasText: "1 个 Agent 已改为跟随默认：dev" }).waitFor();
        await page.getByRole("status").filter({ hasText: "dev-claude" }).waitFor();
        assert.deepEqual(approvalSyncs, ["POST"], "Syncing must be one deliberate write");
        // Agent resets change the same config revision as system settings.
        // The next save must use the refreshed revision, not force a conflict.
        await approvalSelect.click();
        await page.getByRole("option", { name: "自动执行", exact: true }).click();
        await page.getByRole("button", { name: "保存系统设置", exact: true }).click();
        await page.locator('[data-setting="gateway.default_approval"] [data-desired]').filter({ hasText: "自动执行" }).waitFor();
        assert.equal(writes.at(-1).base_revision, "revision-approval-1");
        assert.deepEqual(approvalSyncs, ["POST"], "Later default edits remain independent of reset");
        await nav.getByRole("link", { name: "接入渠道", exact: true }).click();
        assert.equal(await page.getByRole("textbox", { name: "App ID", exact: true }).inputValue(), "channel-draft-kept", "Refreshing after reset must preserve unrelated drafts");
        await page.locator(".settings-conflict").waitFor();
        assert.equal(await page.getByRole("button", { name: "保存渠道设置", exact: true }).isDisabled(), true, "Unrelated drafts must be reviewed against the new revision");
        await page.getByRole("button", { name: "重新读取", exact: true }).click();
        await page.getByRole("button", { name: "放弃草稿并读取", exact: true }).click();
        await page.waitForFunction(() => !document.querySelector(".settings-dirty-dot"));
        await nav.getByRole("link", { name: "审批与权限", exact: true }).click();
        if (screenshots) await page.screenshot({ path: path.join(screenshots, "approval-desktop.png") });
        await page.setViewportSize({ width: 390, height: 620 });
        await page.evaluate(() => document.documentElement.style.setProperty("--ui-font-size", "18px"));
        assert.ok(await page.locator(".settings-content").evaluate(el => el.scrollWidth <= el.clientWidth + 1), "Approval defaults and reset controls wrap at UI18");
        if (screenshots) await page.screenshot({ path: path.join(screenshots, "approval-mobile.png"), fullPage: true });
        await page.evaluate(() => document.documentElement.style.removeProperty("--ui-font-size"));
        await page.setViewportSize({ width: 1280, height: 960 });
        await nav.getByRole("link", { name: "节点与服务", exact: true }).click();
        const hubRow = page.locator('.settings-service-list > li').filter({ hasText: "Coordinator" });
        await hubRow.getByRole("button", { name: "重启服务", exact: true }).click();
        await page.getByRole("dialog", { name: "重启「Coordinator」？", exact: true }).waitFor();
        // A restart that waits never ends a turn: it reports what it is
        // waiting for, names the conversation the way the reader does, and
        // can be taken back.
        await page.getByRole("button", { name: "空闲后重启", exact: true }).click();
        await hubRow.getByText("已排队，空闲后自动重启", { exact: true }).waitFor();
        await hubRow.getByText("有会话在等你答复，答复后会自动继续重启", { exact: true }).waitFor();
        await hubRow.getByText("发布流程", { exact: true }).waitFor();
        assert.equal(restartPosts[0].mode, "when-idle");
        await hubRow.getByRole("button", { name: "撤销", exact: true }).click();
        await hubRow.getByText("已撤销", { exact: true }).waitFor();
        assert.equal(restartPosts[1].cancel, true);
        await hubRow.getByRole("button", { name: "重启服务", exact: true }).click();
        await page.getByRole("dialog", { name: "重启「Coordinator」？", exact: true }).waitFor();
        await page.getByRole("button", { name: "立即重启", exact: true }).click();
        await hubRow.getByText("重启结果尚未确认", { exact: true }).waitFor();
        const id = restartPosts[2].id;
        await nav.getByRole("link", { name: "通用", exact: true }).click();
        await nav.getByRole("link", { name: "节点与服务", exact: true }).click();
        await hubRow.getByText("重启结果尚未确认", { exact: true }).waitFor();
        await page.waitForTimeout(2000); assert.equal(restartPosts.length, 3, "Polling must never resend a restart");
        assert.ok(queries.some((query) => query.id === id));
        await hubRow.getByRole("button", { name: "重试原请求", exact: true }).click();
        await hubRow.getByText("已接受，等待重启确认", { exact: true }).waitFor();
        assert.equal(restartPosts[3].id, id);
        operations.set(id, { command_id: id, state: "restarted", incarnation: 101, previous_incarnation: 100 }); restartUnknown = false;
        channels.runtime_error = "Application credentials rejected";
        await hubRow.getByRole("button", { name: "重新核对", exact: true }).click();
        await hubRow.getByText("已确认重启", { exact: true }).waitFor();
        const nodeRow = page.locator('.settings-service-list > li').filter({ hasText: "Node A" });
        await nodeRow.getByRole("button", { name: "重启服务", exact: true }).click(); await page.getByRole("button", { name: "立即重启", exact: true }).click();
        await nodeRow.getByText("service_busy: active execution", { exact: true }).waitFor();
        assert.equal(restartPosts.length, 5); await nodeRow.getByText("重启失败", { exact: true }).waitFor();
        assert.equal(versionsReads, 0, "Advanced identity/protocol reads are deferred");
        if (screenshots) await page.screenshot({ path: path.join(screenshots, "services-desktop.png") });
        await nav.getByRole("link", { name: "通用", exact: true }).click();
        await page.getByRole("heading", { name: "通用", exact: true }).waitFor();
        await page.getByRole("button", { name: "简体中文 语言", exact: true }).click();
        await page.getByRole("option", { name: "English", exact: true }).click();
        await page.getByRole("heading", { name: "General", exact: true }).waitFor();
        await page.setViewportSize({ width: 780, height: 540 });
        const compactNav = page.getByRole("navigation", { name: "Settings categories", exact: true });
        for (const link of await compactNav.getByRole("link").all()) {
            assert.ok(await link.evaluate((el) => {
                const bounds = el.getBoundingClientRect();
                const label = el.querySelector("span").getBoundingClientRect();
                const nav = el.parentElement.getBoundingClientRect();
                return label.right <= bounds.right - 8 && bounds.right <= nav.right && bounds.height >= 36 && el.scrollWidth <= el.clientWidth;
            }), "English settings labels fit their navigation column and keep the primary navigation row height");
        }
        await page.getByText("Interface preferences on this device. Changes apply immediately.", { exact: true }).waitFor();
        await page.setViewportSize({ width: 1280, height: 960 });
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

        // The runtime-enabled server publishes policy at operation/task entry.
        // Keep this fixture local; the older restart-only path above stays covered.
        state = fixtureView("live"); channels = channelView(); configRevision = "revision-live";
        await page.goto(`${origin}/#/settings`);
        await page.reload();
        await page.setViewportSize({ width: 1280, height: 960 });
        const row = (setting) => page.locator(`[data-setting="${setting}"]`);
        await row("gateway.locale").waitFor();
        assert.match(await row("gateway.locale").innerText(), /之后开始的操作.*正在执行的操作保留原配置/);
        assert.equal(await page.locator(".settings-pending-link, .settings-restart-link").count(), 0);
        const writesBeforeLocal = writes.length;
        await page.getByRole("button", { name: "简体中文 语言", exact: true }).click();
        await page.getByRole("option", { name: "English", exact: true }).click();
        await page.getByRole("heading", { name: "General", exact: true }).waitFor();
        assert.equal(writes.length, writesBeforeLocal, "Local preferences never save server settings");
        assert.equal(await page.getByRole("button", { name: "Save system settings", exact: true }).isDisabled(), true);
        assert.match(await row("gateway.locale").innerText(), /next operation/i);
        await row("gateway.locale").getByRole("button").first().click();
        await page.getByRole("listbox", { name: "Default system reply language", exact: true }).getByRole("option", { name: "English", exact: true }).click();
        await page.getByRole("button", { name: "Save system settings", exact: true }).click();
        await page.getByRole("status").filter({ hasText: "Settings saved." }).waitFor();
        assert.equal(writes.at(-1).settings.gateway.locale, "en");
        assert.equal(await page.locator(".settings-pending-link, .settings-restart-link").count(), 0, "Saving a next-operation field must not suggest a restart");
        assert.doesNotMatch(await page.locator(".settings-content").innerText(), /restart|restarting/i);
        await page.getByRole("navigation", { name: "Settings categories", exact: true }).getByRole("link", { name: "Execution & resources", exact: true }).click();
        assert.match(await row("gateway.task_max_turns").innerText(), /new tasks|newly created tasks/i);
        assert.match(await row("gateway.task_max_turns").innerText(), /0 means unlimited/i);
        assert.match(await row("gateway.task_max_elapsed").innerText(), /0 means unlimited/i);
        await row("gateway.task_max_elapsed").getByText("Defaults & limits", { exact: true }).click();
        assert.match(await row("gateway.task_max_elapsed").innerText(), /0 means unlimited/i);
        await row("gateway.task_max_turns").getByRole("textbox").fill("12");
        await page.getByRole("button", { name: "Save system settings", exact: true }).click();
        await page.getByRole("button", { name: "Save system settings", exact: true }).waitFor({ state: "visible" });
        await page.waitForFunction(() => document.querySelector('[data-setting="gateway.task_max_turns"] input')?.value === "12" && !document.querySelector(".settings-dirty-dot"));
        assert.equal(state.effective.gateway.task_max_turns, 12);
        assert.equal(await page.locator(".settings-pending-link, .settings-restart-link").count(), 0);
        await row("gateway.task_max_turns").getByRole("textbox").fill("0");
        await row("gateway.task_max_elapsed").getByRole("textbox").fill("0");
        await page.getByRole("button", { name: "Save system settings", exact: true }).click();
        await page.waitForFunction(() => !document.querySelector(".settings-dirty-dot"));
        assert.deepEqual(writes.at(-1).settings, { gateway: { task_max_turns: 0, task_max_elapsed: "0" } }, "A positive budget can be reset to unlimited for new tasks");
        assert.equal(await page.locator(".settings-pending-link, .settings-restart-link").count(), 0);
        await page.getByText("Result landing", { exact: true }).click();
        await row("policies.landing.conflicts").getByRole("button").first().click();
        await page.getByRole("option", { name: "Manual (/resolve)", exact: true }).click();
        await page.getByRole("button", { name: "Save system settings", exact: true }).click();
        await page.waitForFunction(() => !document.querySelector(".settings-dirty-dot"));
        assert.deepEqual(writes.at(-1).settings, { policies: { landing: { conflicts: "manual" } } });
        assert.equal(state.effective.policies.landing.conflicts, "manual");
        assert.equal(await row("gateway.owner_id").count(), 0);
        if (screenshots) await page.screenshot({ path: path.join(screenshots, "settings-live-desktop.png"), fullPage: true });
        await page.setViewportSize({ width: 390, height: 844 });
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "Apply-mode help and landing options fit on mobile");
        if (screenshots) await page.screenshot({ path: path.join(screenshots, "settings-live-mobile.png"), fullPage: true });

        // A group-level pending flag must not turn next-operation/task fields
        // into restart fields, even when their saved and effective values differ.
        state.desired.gateway.prompt_timeout = "20m";
        state.desired.gateway.task_max_elapsed = "1h";
        state.desired.gateway.task_max_turns = 15;
        state.desired.gateway.default_approval = "full";
        state.fields.find((field) => field.path === "gateway.default_approval").apply_mode = "live";
        state.fields.find((field) => field.path === "gateway.task_max_turns").apply_mode = "restart";
        state.pending_restart = true;
        await page.getByRole("button", { name: "Reload", exact: true }).click();
        await row("gateway.task_max_turns").locator("[data-desired]").filter({ hasText: "15" }).waitFor();
        assert.equal(await row("gateway.task_max_turns").locator(".settings-restart-link").count(), 1);
        assert.equal(await row("gateway.prompt_timeout").locator(".settings-restart-link").count(), 0);
        assert.equal(await row("gateway.task_max_elapsed").locator(".settings-restart-link").count(), 0);
        await page.locator(".settings-nav").getByRole("link", { name: "Approval & permissions", exact: true }).click();
        await row("gateway.default_approval").waitFor();
        assert.equal(await row("gateway.default_approval").locator(".settings-restart-link").count(), 0);
        await page.locator(".settings-nav").getByRole("link", { name: "Execution & resources", exact: true }).click();
        state.pending_restart = false;
        await page.getByRole("button", { name: "Reload", exact: true }).click();
        await page.waitForFunction(() => !document.querySelector(".settings-pending-link"));
        assert.equal(await row("gateway.task_max_turns").locator(".settings-restart-link").count(), 0, "A value difference alone never means pending restart");
        state.fields = state.fields.filter((field) => field.path !== "policies.landing.conflicts");
        await page.getByRole("button", { name: "Reload", exact: true }).click();
        await row("policies.landing.conflicts").waitFor({ state: "detached" });
        assert.equal(await page.getByText("Result landing", { exact: true }).count(), 0, "Do not show an empty landing section on older servers");

        // Only capabilities actually returned by this channel runtime may
        // promise immediate application; mixed is not a blanket live mode.
        channels = channelView();
        channels.desired.feishu = { ...channels.desired.feishu, enabled: true, app_id: "app-fixture", app_secret_configured: true };
        channels.effective = structuredClone(channels.desired);
        channels.apply_mode = "mixed";
        channels.live_fields = ["feishu.group_policy", "feishu.allow_unmentioned", "feishu.allowed_senders", "feishu.blocked_senders"];
        await englishNav.getByRole("link", { name: "Channels", exact: true }).click();
        await page.getByRole("button", { name: "Reload", exact: true }).click();
        const channelHint = (path) => page.locator(`[data-channel-apply="${path}"]`);
        await channelHint("feishu.group_policy").waitFor({ state: "attached" });
        await page.getByText("Access rules", { exact: true }).click();
        for (const path of channels.live_fields) assert.match(await channelHint(path).innerText(), /subsequent incoming messages.*no restart/i);
        for (const path of ["feishu.enabled", "feishu.app_id", "feishu.app_secret", "feishu.domain", "feishu.owner_open_id", "default_channel"]) assert.match(await channelHint(path).innerText(), /restart/i);
        await page.getByRole("switch", { name: "Receive messages without a mention", exact: true }).press("Space");
        await page.getByRole("textbox", { name: "Allowed senders", exact: true }).fill("allowed-fixture");
        await page.getByRole("textbox", { name: "Blocked senders", exact: true }).fill("blocked-fixture");
        await page.getByRole("button", { name: "Open Group message policy", exact: true }).click();
        await page.getByRole("option", { name: "Allowlist", exact: true }).click();
        await page.getByRole("button", { name: "Save channel settings", exact: true }).click();
        await page.getByRole("status").filter({ hasText: "Channel settings saved and applied." }).waitFor();
        assert.deepEqual(channels.desired, channels.effective);
        assert.equal(await page.locator(".settings-pending-link, .settings-restart-link").count(), 0, "A live-only channel save must not offer restart");
        assert.deepEqual(writes.at(-1).channels.feishu, { group_policy: "allowlist", allow_unmentioned: true, allowed_senders: ["allowed-fixture"], blocked_senders: ["blocked-fixture"] });
        if (screenshots) await page.screenshot({ path: path.join(screenshots, "channels-mixed-mobile.png"), fullPage: true });
        await page.getByRole("textbox", { name: "App ID", exact: true }).fill("replacement-app");
        await page.getByRole("textbox", { name: "Blocked senders", exact: true }).fill("new-blocked-fixture");
        await page.getByRole("button", { name: "Save channel settings", exact: true }).click();
        await page.locator(".settings-pending-link").waitFor();
        await page.getByRole("status").filter({ hasText: "Some saved changes still require" }).waitFor();
        assert.equal(channels.effective.feishu.app_id, "app-fixture");
        assert.deepEqual(channels.effective.feishu.blocked_senders, ["new-blocked-fixture"]);
        assert.equal(await page.locator('.settings-content [role="status"] .settings-restart-link').count(), 1);
        channels.live_fields = ["feishu.group_policy"];
        await page.getByRole("button", { name: "Reload", exact: true }).click();
        await channelHint("feishu.allow_unmentioned").filter({ hasText: "Restart" }).waitFor();
        assert.match(await channelHint("feishu.group_policy").innerText(), /no restart/i);
        // Startup failure / older servers do not advertise live fields.
        channels.apply_mode = "restart"; delete channels.live_fields;
        channels.runtime_error = "Application credentials rejected";
        await page.getByRole("button", { name: "Reload", exact: true }).click();
        await channelHint("feishu.group_policy").filter({ hasText: "Restart" }).waitFor();
        await page.getByRole("alert").filter({ hasText: "Channel startup failed" }).waitFor();
        assert.equal(await page.locator('[data-channel-apply]').filter({ hasText: "no restart" }).count(), 0);
        assert.deepEqual(errors, []); assert.deepEqual(external, []);
        console.log("PASS settings center browser: drafts/CAS, next-operation/task modes, zero budgets, landing schema, mixed/restart channel capabilities, restart receipts, English and narrow layout");
        await context.close();
    } finally { await browser.close(); await server.close(); }
}
