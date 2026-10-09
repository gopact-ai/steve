// Production Composer and API client, isolated HTTP observations/preferences.
import assert from "node:assert/strict";
import { mkdir, mkdtemp, realpath, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import react from "../../web/console/node_modules/@vitejs/plugin-react/dist/index.js";
import tailwindcss from "../../web/console/node_modules/@tailwindcss/vite/dist/index.mjs";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
const web = process.env.TYPED_OPTIONS_WEB_ROOT || fileURLToPath(new URL("../../web/console", import.meta.url));
const scratch = await realpath(await mkdtemp(path.join(tmpdir(), "topt-")));
const selectors = { model: "Reported model", models: [], preferred: {}, options: [
    { ID: "vendor/toggle", Name: "Fast lane", Type: "boolean", Category: "vendor/private", Current: "false" },
    { ID: "unset-toggle", Name: "Unset toggle", Type: "boolean", Current: "" },
    { ID: "opaque", Name: "Opaque choice", Type: "select", Category: "vendor/unknown", Current: "false", Choices: [{ Value: "false", Label: "Literal false ID" }, { Value: "__none", Label: "Opaque reserved-looking ID" }] },
    { ID: "untyped", Name: "Untyped option", Current: "false", Choices: [{ Value: "true", Label: "Do not guess" }] },
    { ID: "future", Name: "Future option", Type: "future", Current: "false" },
] };
const errors = [], writes = [];
let server, browser, hold = false, release, reject = false, rejectReset = false, failReload = false;
try {
    await symlink(path.join(web, "node_modules"), path.join(scratch, "node_modules"), "dir");
    await writeFile(path.join(scratch, "index.html"), '<html><body><div id="root"></div><script type="module" src="/fixture.tsx"></script></body></html>');
    await writeFile(path.join(scratch, "fixture.css"), `@import "${web}/src/styles/globals.css";\n@source "${web}/src";\n@source "./fixture.tsx";`);
    await writeFile(path.join(scratch, "fixture.tsx"), `
import React, { useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { LocaleProvider } from "@/providers/locale-provider";
import { Composer } from "@/components/steve/composer";
import { fetchSelectors, setPreferences } from "@/lib/api/console";
import "./fixture.css";
const agent = { id: "fixture-agent", harness: "fixture", node: "fixture-node", usable: true, ready: true };
function Fixture() { const box = useRef(null), [text,setText] = useState(""); return <div className="workbench-shell"><main className="app-main"><div className="console-workbench"><div className="console-content"><div className="conversation-content"><div className="transcript-scroll">Fixture transcript</div><div className="composer-dock"><Composer value={text} onChange={setText} onKey={() => {}} onSubmit={() => {}} onStop={() => {}} boxRef={box} busy={false} suggestions={[]} pick={0} onApply={() => {}} verbs={[]} onVerb={() => {}} projects={[]} agents={[agent]} agent={agent} onProject={() => {}} onAgent={() => {}} onSelectors={() => fetchSelectors("console:fixture", agent.id)} onPrefer={async patch => { await setPreferences("console:fixture", agent.id, patch); }} /></div></div></div></div></main></div>; }
createRoot(document.getElementById("root")).render(<LocaleProvider><Fixture /></LocaleProvider>);
`);
    server = await createServer({ root: scratch, configFile: false, envDir: false, cacheDir: path.join(scratch, "cache"), plugins: [react(), tailwindcss()], resolve: { alias: { "@": path.join(web, "src") }, dedupe: ["react", "react-dom"] }, server: { host: "127.0.0.1", port: 0, fs: { allow: [web, scratch] } }, logLevel: "error" });
    await server.listen(); const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
    browser = await chromium.launch({ headless: true });
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, serviceWorkers: "block", reducedMotion: "reduce" });
    await context.addInitScript(() => { localStorage.setItem("steve.ui.locale", "en"); });
    const page = await context.newPage(); page.setDefaultTimeout(6000); page.on("pageerror", e => errors.push(String(e)));
    const modules = { count: 0, mode: "normal", release: null };
    async function observe(page) { await page.route("**/*", async route => {
        const req = route.request(), url = new URL(req.url());
        assert.equal(url.origin, origin, "external traffic prohibited");
        if (url.pathname.endsWith("/session-option.tsx")) {
            modules.count++;
            if (modules.mode === "hold") await new Promise(resolve => { modules.release = resolve; });
            if (modules.mode === "failed") return route.fulfill({ status:503, body:"Controlled optional-module failure" });
        }
        if (url.pathname === "/console/queue" && url.searchParams.get("capabilities") === "1") return route.fulfill({ json: { queue: [], submission_keys: true } });
        if (url.pathname === "/console/selectors") { if (failReload) { failReload=false; return route.fulfill({ status:503, json:{error:"Fixture report unavailable"} }); } return route.fulfill({ json: selectors }); }
        if (url.pathname === "/console/preferences") {
            assert.equal(req.method(), "PUT"); const body = req.postDataJSON(); writes.push(body);
            if (hold) await new Promise(resolve => { release = resolve; });
            if (rejectReset && Object.values(body.patch).includes("")) { rejectReset=false; return route.fulfill({ status:400, json:{error:"Fixture no declared default"} }); }
            if (reject) { reject = false; return route.fulfill({ status: 400, json: { error: "Fixture preference rejected" } }); }
            selectors.preferred={...selectors.preferred,...body.patch}; return route.fulfill({ json: { ok: true, live: false } });
        }
        assert.ok(!url.pathname.startsWith("/console/"), `unexpected API ${req.method()} ${url.pathname}`);
        return route.continue();
    }); }
    await observe(page);
    await page.goto(origin);
    await page.getByRole("button", { name: "Session options", exact: true }).waitFor();
    assert.equal(modules.count, 0, "optional rich controls are not requested before the options panel opens");
    await page.getByRole("button", { name: "Session options", exact: true }).click();
    const panel = page.getByRole("dialog", { name: "Session options", exact: true }); await panel.waitFor();
    await panel.getByText("Fast lane", { exact: true }).waitFor();
    const toggle = panel.getByRole("group", { name: "Fast lane", exact: true });
    await toggle.getByText("Agent reported: false", { exact: true }).waitFor();
    const waitFor = async check => { for (let i=0;i<100;i++) { if(check()) return; await new Promise(resolve=>setTimeout(resolve,25)); } assert.fail("preference request was not observed"); };
    const choose = async (group, option) => { const before=writes.length; await group.getByRole("button", { name: /Requested preference/ }).click(); const listbox=page.getByRole("listbox").last(); await listbox.getByRole("option", { name: option, exact: true }).click(); await listbox.waitFor({state:"hidden"}); await waitFor(()=>writes.length>before); };
    assert.match(await toggle.getByRole("button", { name: /Requested preference/ }).innerText(), /Unfixed/);
    await panel.getByRole("group", { name: "Unset toggle", exact: true }).getByText("Agent reported: Not reported", { exact: true }).waitFor();
    for (const name of ["Untyped option", "Future option"]) assert.equal(await panel.getByRole("group", { name, exact: true }).getByRole("button").count(), 0, "unknown/missing type is read-only");
    hold = true; await choose(toggle, "true");
    await page.waitForFunction(() => !!document.querySelector('[role="dialog"] [aria-busy="true"]') || !!document.querySelector('[role="dialog"] button:disabled'));
    await toggle.getByText("Agent reported: false", { exact: true }).waitFor();
    assert.deepEqual(writes.at(-1).patch, { "vendor/toggle": "true" });
    hold = false; release();
    await toggle.getByRole("button", { name: /Requested preference/ }).waitFor();
    await page.waitForFunction(() => !document.querySelector('[role="dialog"] button:disabled'));
    await toggle.getByText("Agent reported: false", { exact: true }).waitFor();
    assert.match(await toggle.getByRole("button", { name: /Requested preference/ }).innerText(), /true/);
    reject = true; await choose(toggle, "false");
    await panel.getByRole("alert").filter({ hasText: "Fixture preference rejected" }).waitFor();
    await toggle.getByText("Agent reported: false", { exact: true }).waitFor();
    assert.equal(selectors.preferred["vendor/toggle"], "true");
    const opaque = panel.getByRole("group", { name: "Opaque choice", exact: true });
    await choose(opaque, "Literal false ID"); assert.deepEqual(writes.at(-1).patch, { opaque: "false" });
    await choose(opaque, "Opaque reserved-looking ID"); assert.deepEqual(writes.at(-1).patch, { opaque: "__none" });
    rejectReset=true; await choose(toggle, "Unfixed (Agent default)");
    await panel.getByRole("alert").filter({hasText:"Fixture no declared default"}).waitFor();
    await toggle.getByText("Agent reported: false",{exact:true}).waitFor(); assert.equal(selectors.preferred["vendor/toggle"],"true");
    await choose(toggle, "Unfixed (Agent default)"); assert.deepEqual(writes.at(-1).patch, { "vendor/toggle": "" });
    await toggle.getByText("Agent reported: false", { exact: true }).waitFor();
    failReload=true; await choose(toggle,"true");
    await panel.getByRole("alert").filter({hasText:"Fixture report unavailable"}).waitFor();
    await toggle.getByText("Agent reported: false",{exact:true}).waitFor();
    await choose(toggle,"false"); await toggle.getByText("Agent reported: false",{exact:true}).waitFor();
    selectors.options[0].Current="true";
    const beforeReload=writes.length; await panel.getByRole("button",{name:"Reload Agent report",exact:true}).click();
    await toggle.getByText("Agent reported: true",{exact:true}).waitFor(); assert.equal(writes.length,beforeReload);
    await page.setViewportSize({ width: 390, height: 844 });
    assert.ok(await panel.evaluate(el => el.scrollWidth <= el.clientWidth), "no narrow horizontal overflow");
    await panel.getByRole("button", { name: /Requested preference/ }).first().focus(); await page.keyboard.press("Tab");
    assert.equal(await panel.evaluate(el => el.contains(document.activeElement)), true, "focus remains in popover dialog");
    if (process.env.TYPED_OPTIONS_SCREENSHOTS) { await mkdir(process.env.TYPED_OPTIONS_SCREENSHOTS, { recursive: true }); await page.screenshot({ path: path.join(process.env.TYPED_OPTIONS_SCREENSHOTS, "conversation-options-light.png") }); await page.evaluate(() => document.documentElement.classList.add("dark-mode")); await page.screenshot({ path: path.join(process.env.TYPED_OPTIONS_SCREENSHOTS, "conversation-options-dark.png") }); }
    assert.deepEqual(errors, []); console.log("PASS real Composer/API typed booleans: false vs unset, requested vs Actual, errors, opaque IDs, narrow/focus; fixture only");
    await page.close();
    for (const mode of ["hold", "failed"]) {
        const optional = await context.newPage(); optional.setDefaultTimeout(6000); optional.on("pageerror", e => errors.push(String(e)));
        await observe(optional); modules.count=0; modules.mode=mode; modules.release=null;
        await optional.goto(origin); const box=optional.getByRole("textbox",{name:"Message",exact:true}); await box.waitFor(); await box.fill("Keep my options draft");
        assert.equal(modules.count,0); const beforeWrites=writes.length;
        await optional.getByRole("button",{name:"Session options",exact:true}).click();
        const local=optional.getByRole("dialog",{name:"Session options",exact:true});await local.waitFor();
        const notice=local.getByRole(mode==="failed"?"alert":"status");await notice.getByRole("button",{name:"Close",exact:true}).waitFor();
        assert.equal(modules.count,1);await notice.getByRole("button",{name:"Close",exact:true}).click();await local.waitFor({state:"hidden"});
        assert.equal(await box.innerText(),"Keep my options draft");assert.equal(writes.length,beforeWrites,"closing optional load cannot write a preference");
        if(mode==="hold") {
            modules.mode="normal";modules.release();
            await optional.getByRole("button",{name:"Session options",exact:true}).click();await local.getByText("Fast lane",{exact:true}).waitFor();
            assert.equal(modules.count,1,"reopening waits for the original import rather than issuing another fetch");
            await optional.keyboard.press("Escape");await local.waitFor({state:"hidden"});assert.equal(await box.innerText(),"Keep my options draft");
        }
        await optional.close();
    }
    assert.deepEqual(errors,[]);console.log("PASS optional controls load only on opening; delayed/failed imports close locally and keep the draft without preference writes");
    await context.close();
} finally { await browser?.close(); await server?.close(); await rm(scratch, { recursive: true, force: true }); }
