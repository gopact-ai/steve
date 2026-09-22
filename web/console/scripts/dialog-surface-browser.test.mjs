// Real consumers, synthetic data, a private Vite cache, and no live backend.
import assert from "node:assert/strict";
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";
import { workState } from "../../../e2e/console/work-fixture.mjs";

const root = fileURLToPath(new URL("..", import.meta.url));
const scratch = await mkdtemp(path.join(tmpdir(), "steve-dialog-browser-"));
const fixtureID = path.join(root, "__dialog_fixture.tsx");
const fixture = `
import React, { useState } from "react";
import { createRoot } from "react-dom/client";
import { HashRouter } from "react-router";
import { LocaleProvider } from "@/providers/locale-provider";
import { ThemeProvider } from "@/providers/theme-provider";
import { CloseStackProvider } from "@/providers/close-stack";
import { FleetProvider, IntentProvider } from "@/lib/fleet";
import { CoordinationProvider } from "@/lib/coordination";
import { ProjectsPage } from "@/pages/projects";
import { FleetPage } from "@/pages/fleet";
import { HomePage } from "@/pages/home";
import { SettingsPage } from "@/pages/settings";
import { SettingsServices } from "@/components/steve/settings-services";
import { SSHConnect } from "@/components/steve/ssh-connect";
import { DesktopSetupDialog } from "@/components/steve/desktop-setup-dialog";
import { NodeAgentEnrollment } from "@/components/steve/node-agent-enrollment";
import { MachineUpgrade } from "@/components/steve/machine-upgrade";
import { ConfirmDialog } from "@/components/steve/confirm";
import "@/styles/globals.css";
import "@/styles/settings.css";
function Fixture() {
  const [open, setOpen] = useState(false);
  const mode = new URLSearchParams(location.search).get("mode");
  const close = () => setOpen(false);
  const scenario = new URLSearchParams(location.search).get("scenario");
  const status = { enabled: true, node_id: "fixture", setup_required: true, agent_count: 0, setup: { step: scenario === "nested" ? "machines" : "identity", done: false } };
  if (mode === "home") return <HomePage />;
  if (mode === "settings") return <SettingsPage />;
  if (mode === "services") return <SettingsServices onRestarted={() => {}} />;
  if (["ssh", "desktop", "enrollment", "upgrade"].includes(mode)) return <>
    <button onClick={() => setOpen(true)}>Open wizard</button>{open && (
      mode === "ssh" ? <SSHConnect onClose={close} onChanged={() => {}} /> :
      mode === "desktop" ? <DesktopSetupDialog status={status} entry={null} onClose={close} onStatus={() => {}} /> :
      mode === "enrollment" ? <NodeAgentEnrollment node="fixture" name="Fixture machine" onClose={close} onRegistered={() => {}} /> :
      <MachineUpgrade nodes={[{ name: "fixture", version: "old-fixture" }]} version="fixture" onClose={close} onChanged={() => {}} />)}
    </>;
  return mode === "projects" ? <ProjectsPage /> : mode === "fleet" ? <FleetPage /> :
    <><button onClick={() => setOpen(true)}>Open confirmation</button>{open && <ConfirmDialog
      title="Fixture confirmation" body="A synthetic destructive action." confirmLabel="Confirm fixture"
      onClose={() => setOpen(false)} onConfirm={() => new Promise((resolve, reject) => {
        window.confirmCalls = (window.confirmCalls || 0) + 1;
        window.finishConfirmation = (fail) => fail ? reject(new Error("Fixture rejection")) : resolve();
      })} />}</>;
}
createRoot(document.getElementById("root")).render(<LocaleProvider><ThemeProvider><HashRouter>
  <CloseStackProvider><FleetProvider><IntentProvider onNavigate={() => {}}><CoordinationProvider>
    <Fixture />
  </CoordinationProvider></IntentProvider></FleetProvider></CloseStackProvider>
</HashRouter></ThemeProvider></LocaleProvider>);
`;
const errors = [], failures = [], samples = [];
let server, browser;
try {
    server = await createServer({
        root, envDir: false, cacheDir: path.join(scratch, "cache"), logLevel: "error",
        server: { host: "127.0.0.1", port: 0 },
        plugins: [{
            name: "dialog-fixture",
            resolveId(id) { if (id === "/__dialog_fixture.tsx" || id === fixtureID) return fixtureID; },
            load(id) { if (id === fixtureID) return fixture; },
            configureServer(vite) {
                vite.middlewares.use("/__dialog", async (_req, res) => {
                    res.setHeader("Content-Type", "text/html");
                    res.end(await vite.transformIndexHtml("/__dialog", '<html><body><div id="root"></div><script type="module" src="/__dialog_fixture.tsx"></script></body></html>'));
                });
            },
        }],
    });
    await server.listen();
    const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
    browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
    async function open(mode, viewport, theme = "light", scenario = "") {
        const context = await browser.newContext({ viewport, serviceWorkers: "block", reducedMotion: "reduce" });
        const page = await context.newPage();
        const operation = Promise.withResolvers(), writes = [];
        page.setDefaultTimeout(15000);
        page.on("pageerror", (error) => errors.push(`${mode}: ${error}`));
        await page.addInitScript(({ theme }) => {
            localStorage.setItem("steve.ui.locale", "en");
            localStorage.setItem("ui-theme", theme);
            window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
        }, { theme });
        await context.route("**/*", async (route) => {
            const req = route.request(), url = new URL(req.url()), p = url.pathname;
            if (url.origin !== origin) { errors.push(`External request: ${url.origin}`); return route.abort(); }
            if (req.method() === "PUT" && p === "/console/desktop/setup") {
                writes.push(p);
                return route.fulfill({ json: { enabled: true, node_id: "fixture", setup_required: true, agent_count: 0, setup: req.postDataJSON() } });
            }
            if (req.method() === "POST" && ["/console/ssh/check", "/console/nodes/fixture/agents", "/console/ssh/upgrades/fixture"].includes(p)) {
                writes.push(p);
                await operation.promise;
                if (p === "/console/ssh/check") return route.fulfill({ json: { candidate: { alias: "fixture", host_name: "fixture.invalid", port: 22 }, reachable: false, tools: [], existing_installation: false, steps: [], checked_at: "2026-09-20T00:00:00Z" } });
                return route.fulfill({ status: 409, json: { error: "Synthetic operation rejected" } });
            }
            if (req.method() !== "GET") { errors.push(`Unexpected mutation: ${p}`); return route.abort(); }
            if (p === "/state") return route.fulfill({ json: workState({
                at: "2026-09-20T00:00:00Z", hub: { node: "fixture", version: "fixture" },
                nodes: scenario.startsWith("entities") ? [{ name: "fixture", display_name: "Fixture machine", role: "hub", up: false, capabilities: [], version: "fixture" }] : [],
                projects: scenario.startsWith("entities") ? [{ id: "fixture-project", node: "fixture", path: "/fixture/project", level: "internal", repo: "inplace", agents: [], workspaces: [], repos: [] }] : mode === "home" ? [{ id: "personal", home: true, path: "/fixture/home", node: "fixture" }] : [],
                agents: scenario.startsWith("entities") ? [{ id: "fixture-agent", node: "fixture", harness: "fixture", level: "internal", eligible: true, requires: [], activities: [], models: [], mcp_servers: [] }] : [], tasks: [], plans: [], attempts: [], landings: [],
            }) });
            if (p === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [] } });
            const settings = { revision: "fixture", desired: { gateway: { locale: "en" } }, effective: { gateway: { locale: "en" } }, pending_restart: false, apply_mode: "live", fields: [{ path: "gateway.locale", type: "string", apply_mode: "live" }] };
            const channels = { default_channel: "console", console: { enabled: true, owner_id: "fixture" }, feishu: { enabled: false, app_id: "", app_secret_configured: false, domain: "feishu", owner_open_id: "", group_policy: "disabled", allow_unmentioned: false, allowed_senders: [], blocked_senders: [] } };
            const fixtures = {
                "/console/settings": settings,
                "/console/channels": { revision: "fixture", desired: channels, effective: channels, pending_restart: false },
                "/console/services": { services: [{ name: "hub", kind: "hub", label: "Fixture hub", online: true, supported: true, version: "fixture" }] },
                "/console/home": { path: "/fixture/home", files: [{ name: "SOUL.md", text: "Synthetic profile", bytes: 17, budget: 4000 }], projects: [], warnings: [], total_budget: 4000, owner_bytes: 17, guest_bytes: 0 },
                "/console/ssh/candidates": { candidates: [{ alias: "fixture", host_name: "fixture.invalid", port: 22, source: "/fixture/ssh", line: 1 }], warnings: [] },
                "/console/nodes/fixture/agents": { revision: "fixture", agents: [{ id: "fixture", name: "Fixture CLI", harness: "fixture", installed: true, registered: false, models: [], selectors: [] }] },
                "/console/nodes/fixture/settings": { settings: { revision: "fixture", capabilities: [], harnesses: {}, tools: [], mcp_servers: {}, declares: [] } },
            };
            if (Object.hasOwn(fixtures, p)) return route.fulfill({ json: fixtures[p] });
            if (p.startsWith("/console/") || ["/history", "/events"].includes(p)) {
                errors.push(`Unhandled API: ${p}`); return route.abort();
            }
            return route.continue();
        });
        await page.goto(`${origin}/__dialog?mode=${mode}&scenario=${scenario}${scenario.startsWith("entities") ? `#/fleet?tab=${scenario === "entities-agents" ? "agents" : "machines"}` : ""}`, { waitUntil: "networkidle" });
        if (mode === "settings") await page.locator("#setting-gateway-locale").fill("zh");
        const label = mode === "home" ? "Generate" : mode === "settings" ? "Reload" : mode === "services" ? "Restart service" : ["ssh", "desktop", "enrollment", "upgrade"].includes(mode) ? "Open wizard" : mode === "projects" ? "Add project" : mode === "fleet" ? "Add machine / agent" : "Open confirmation";
        const trigger = page.getByRole("button", { name: label, exact: true });
        await trigger.focus();
        await trigger.press("Enter");
        const dialog = page.getByRole("dialog");
        await dialog.waitFor();
        await page.evaluate(() => Promise.all(document.getAnimations().filter((animation) => animation.effect?.getComputedTiming().iterations !== Infinity).map((animation) => animation.finished.catch(() => {}))));
        return { context, page, dialog, trigger, writes, release: operation.resolve };
    }
    async function sample(f, mode, width, theme) {
        const migrated = f.dialog.locator("[data-dialog-surface]");
        const surface = await migrated.count() ? migrated.first() : f.dialog.locator(":scope > div").first();
        const actual = await surface.evaluate((el) => {
            const visual = getComputedStyle(el).backgroundColor === "rgba(0, 0, 0, 0)" ? el.parentElement : el;
            const s = getComputedStyle(visual);
            const body = el.querySelector("[data-dialog-body]") || el;
            return { padding: getComputedStyle(body).padding, radius: s.borderRadius, background: s.backgroundColor, shadow: s.boxShadow, width: el.getBoundingClientRect().width };
        });
        samples.push({ mode, theme, ...actual, surfaceWidth: actual.width, width });
        if (["projects", "fleet", "home", "settings", "services"].includes(mode)) {
            const heading = f.dialog.getByRole("heading", { level: 2 });
            if (await heading.count() !== 1) failures.push(`${mode}: ordinary dialog needs one semantic h2 title`);
            else {
                const type = await heading.evaluate((el) => { const s = getComputedStyle(el); return { size: s.fontSize, line: s.lineHeight, weight: s.fontWeight }; });
                if (type.size !== "16px" || type.line !== "24px" || type.weight !== "600") failures.push(`${mode}: ordinary heading ${JSON.stringify(type)}, expected 16/24/600`);
            }
        }
        const expected = mode === "confirm" || width < 640 ? "20px" : "24px";
        if (actual.padding !== expected) failures.push(`${mode}/${width}: padding ${actual.padding}, expected ${expected}`);
        if (actual.radius !== "16px") failures.push(`${mode}/${width}: radius ${actual.radius}, expected 16px`);
        assert.ok(actual.width <= width, `${mode}: surface stays within the viewport`);
        assert.equal(await f.dialog.evaluate((el) => el.scrollWidth <= el.clientWidth), true, `${mode}: no dialog horizontal overflow`);
    }
    for (const theme of (process.env.DIALOG_BEHAVIOR_ONLY ? [] : process.env.DIALOG_HEADERS_ONLY ? ["light"] : ["light", "dark"])) {
        for (const viewport of (process.env.DIALOG_HEADERS_ONLY ? [{ width: 1440, height: 1000 }] : [{ width: 1440, height: 1000 }, { width: 390, height: 844 }, { width: 844, height: 390 }])) {
            for (const mode of (process.env.DIALOG_HEADERS_ONLY ? ["projects", "fleet", "home", "settings", "services"] : process.env.DIALOG_CORE_ONLY ? ["projects", "fleet", "confirm"] : ["projects", "fleet", "confirm", "home", "settings", "services", "ssh", "desktop", "enrollment", "upgrade"])) {
                const f = await open(mode, viewport, theme);
                await sample(f, mode, viewport.width, theme);
                if (process.env.DIALOG_SCREENSHOTS && theme === "light" && viewport.width === 390 && ["projects", "confirm", "desktop", "services"].includes(mode)) {
                    await mkdir(process.env.DIALOG_SCREENSHOTS, { recursive: true });
                    await f.page.screenshot({ animations: "disabled", path: path.join(process.env.DIALOG_SCREENSHOTS, `${mode}-390.png`) });
                }
                for (let i = 0; i < 5; i++) {
                    await f.page.keyboard.press("Tab");
                    assert.equal(await f.dialog.evaluate((el) => el.contains(document.activeElement)), true, `${mode}: keyboard focus stays in dialog`);
                }
                await f.page.keyboard.press("Escape");
                await f.dialog.waitFor({ state: "hidden" });
                await f.page.waitForFunction((el) => el === document.activeElement, await f.trigger.elementHandle());
                await f.context.close();
            }
            const matching = samples.filter((s) => s.width === viewport.width && s.theme === theme);
            for (const s of matching.slice(1)) {
                if (s.background !== matching[0].background) failures.push(`${s.mode}/${s.width}: surface color differs`);
                if (s.shadow !== matching[0].shadow) failures.push(`${s.mode}/${s.width}: surface elevation differs`);
            }
        }
    }
    const f = await open("confirm", { width: 390, height: 844 });
    await f.dialog.getByRole("button", { name: "Confirm fixture", exact: true }).click();
    await f.page.keyboard.press("Escape");
    await f.page.evaluate(() => window.steveCloseLayer());
    assert.equal(await f.dialog.isVisible(), true, "pending confirmation remains open");
    assert.equal(await f.page.evaluate(() => window.confirmCalls), 1);
    await f.page.evaluate(() => window.finishConfirmation(true));
    await f.dialog.getByRole("alert").filter({ hasText: "Fixture rejection" }).waitFor();
    await f.dialog.getByRole("button", { name: "Confirm fixture", exact: true }).click();
    await f.page.evaluate(() => window.finishConfirmation(false));
    await f.dialog.waitFor({ state: "hidden" });
    assert.equal(await f.page.evaluate(() => window.confirmCalls), 2);
    await f.context.close();
    if (!process.env.DIALOG_CORE_ONLY && !process.env.DIALOG_HEADERS_ONLY) {
        for (const [mode, scenario, entity, title] of [["projects", "entities", "fixture-project", "fixture-project"], ["fleet", "entities-agents", "fixture-agent", "fixture-agent"], ["fleet", "entities", "Fixture machine", "Fixture machine"]]) {
            const d = await open(mode, { width: 1440, height: 1000 }, "light", scenario);
            await d.page.keyboard.press("Escape"); await d.dialog.waitFor({ state: "hidden" });
            const row = d.page.getByRole("row").filter({ hasText: entity }).first();
            if (mode === "projects") {
                const menu = row.getByRole("button").last();
                await menu.click();
                await d.page.getByRole("menu").waitFor();
                await d.page.keyboard.press("Escape");
                await d.page.getByRole("menu").waitFor({ state: "hidden" });
                await d.page.waitForFunction((el) => el.contains(document.activeElement), await row.elementHandle());
                assert.equal(await menu.getAttribute("aria-expanded"), "false", "IconButton retains Dropdown trigger context");
            }
            await row.getByRole("rowheader").click();
            const drawer = d.page.getByRole("dialog", { name: title, exact: true });
            await drawer.waitFor();
            assert.equal(await drawer.getByRole("heading", { level: 2, name: title, exact: true }).count(), 1);
            await d.page.keyboard.press("Escape");
            await drawer.waitFor({ state: "hidden" });
            assert.equal(d.writes.length, 0);
            await d.context.close();
        }
        // Existing heading refs must still focus the next stage; no native picker runs.
        const desktop = await open("desktop", { width: 390, height: 844 });
        await desktop.dialog.getByRole("button", { name: "Next", exact: true }).click();
        const heading = desktop.dialog.getByRole("heading", { name: "Working directory", exact: true });
        await heading.waitFor();
        await desktop.page.waitForFunction((el) => el === document.activeElement, await heading.elementHandle());
        assert.deepEqual(desktop.writes, ["/console/desktop/setup"]);
        await desktop.context.close();

        // Real nested Desktop -> SSH overlays, not an imitation of their lifecycle.
        const nested = await open("desktop", { width: 390, height: 844 }, "light", "nested");
        const connect = nested.dialog.getByRole("button", { name: "Connect a machine over SSH", exact: true });
        await connect.focus(); await connect.press("Enter");
        const all = nested.page.getByRole("dialog", { includeHidden: true });
        await nested.page.waitForFunction(() => document.querySelectorAll('[role="dialog"]').length === 2);
        await nested.page.keyboard.press("Tab");
        assert.equal(await all.last().evaluate((el) => el.contains(document.activeElement)), true, "nested focus stays at top");
        await nested.page.keyboard.press("Escape");
        await nested.page.waitForFunction(() => document.querySelectorAll('[role="dialog"]').length === 1);
        await nested.page.waitForFunction((el) => el === document.activeElement, await connect.elementHandle());
        assert.equal(await nested.dialog.isVisible(), true, "closing SSH retains desktop guide");
        await nested.context.close();

        for (const mode of ["ssh", "enrollment", "upgrade"]) {
            const w = await open(mode, { width: 390, height: 844 });
            if (mode === "ssh") await w.dialog.getByRole("radio").first().press("Space");
            if (mode === "enrollment") {
                await w.dialog.getByRole("checkbox", { name: "Fixture CLI", exact: true }).press("Space");
                const field = w.dialog.getByRole("textbox", { name: "Agent name", exact: true });
                await field.fill("INVALID NAME");
                await w.dialog.getByRole("button", { name: "Register selected agents", exact: true }).click();
                assert.equal(await field.evaluate((el) => el === document.activeElement), true, "forwarded enrollment form ref focuses invalid field");
                assert.equal(w.writes.length, 0);
                await field.fill("fixture-agent");
            }
            const action = mode === "ssh" ? "Check connection" : mode === "enrollment" ? "Register selected agents" : "Upgrade this machine";
            await w.dialog.getByRole("button", { name: action, exact: true }).click();
            await w.page.waitForTimeout(100);
            assert.equal(w.writes.length, 1, `${mode}: one synthetic operation`);
            await w.page.keyboard.press("Escape");
            await w.page.evaluate(() => window.steveCloseLayer());
            await w.page.mouse.click(2, 2);
            assert.equal(await w.dialog.isVisible(), true, `${mode}: pending guards retained`);
            w.release();
            if (mode === "ssh") {
                const next = w.dialog.getByRole("heading", { name: "SSH connectivity has not been confirmed.", exact: true });
                await next.waitFor();
                await w.page.waitForFunction((el) => el === document.activeElement, await next.elementHandle());
                assert.equal(await w.dialog.locator("[data-dialog-body]").evaluate((el) => el.scrollTop), 0, "SSH stage ref resets original scroll body");
            } else await w.dialog.getByRole("alert").filter({ hasText: "Synthetic operation rejected" }).waitFor();
            await w.context.close();
        }

        // Home uses DialogTrigger: cancellation must not discard its local draft.
        const home = await open("home", { width: 390, height: 844 });
        await home.page.keyboard.press("Escape"); await home.dialog.waitFor({ state: "hidden" });
        await home.page.getByRole("button", { name: "Edit", exact: true }).click();
        await home.page.getByRole("textbox", { name: "SOUL.md", exact: true }).fill("Synthetic edited draft");
        await home.page.getByRole("button", { name: "Discard changes", exact: true }).click();
        await home.dialog.getByRole("button", { name: "Keep editing", exact: true }).click();
        assert.equal(await home.page.getByRole("textbox", { name: "SOUL.md", exact: true }).inputValue(), "Synthetic edited draft");
        assert.equal(home.writes.length, 0);
        await home.context.close();
    }
    console.log(JSON.stringify({ samples, failures, errors }, null, 2));
    assert.deepEqual(errors, []);
    assert.deepEqual(failures, [], "standard dialog surfaces share chrome across consumers and viewports");
    console.log("Dialog surface computed styles, focus restoration, and confirmation lifecycle passed.");
} finally {
    await browser?.close();
    await server?.close();
    await rm(scratch, { recursive: true, force: true });
}
