import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createInterface } from "node:readline";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";

const root = fileURLToPath(new URL("../../", import.meta.url));
const web = path.join(root, "web/console");
const temp = await mkdtemp(path.join(tmpdir(), "steve-assistant-ui-"));
const binary = path.join(temp, "fixture");
async function build() { await new Promise((resolve, reject) => { const p = spawn("go", ["build", "-o", binary, "./e2e/assistantfixture"], { cwd: root, stdio: ["ignore", "ignore", "pipe"] }); let error="";p.stderr.on("data", (d)=>error+=d);p.on("exit",(code)=>code===0?resolve():reject(new Error(error))); }); }
let fixture, server, browser;
try {
    await build(); fixture = spawn(binary, [], { cwd: root, stdio: ["ignore", "pipe", "pipe"] });
    let stderr="";fixture.stderr.on("data",(d)=>stderr+=d);
    const connection = await new Promise((resolve,reject)=>{const lines=createInterface({input:fixture.stdout});const timeout=setTimeout(()=>reject(new Error("Fixture startup timed out: "+stderr)),30000);lines.once("line",(line)=>{clearTimeout(timeout);lines.close();resolve(JSON.parse(line));});fixture.once("exit",(code)=>{if(code)reject(new Error(stderr));});});
    const proxy={target:connection.url,changeOrigin:true,configure(p){p.on("proxyReq",(req)=>req.setHeader("Origin",connection.url));}};
    server=await createServer({configFile:path.join(web,"vite.config.ts"),root:web,server:{host:"127.0.0.1",port:0,proxy:{"/console":proxy,"/state":proxy,"/events":proxy}}});await server.listen();
    const url=server.resolvedUrls.local[0]; browser=await chromium.launch({headless:true,channel:process.env.BROWSER_CHANNEL});
    const page=await browser.newPage({viewport:{width:1280,height:900}});page.setDefaultTimeout(10000);const errors=[];page.on("pageerror",(e)=>errors.push(String(e)));
    await page.addInitScript(()=>localStorage.setItem("steve.ui.locale","en"));
    await page.goto(url+"?token=assistant-fixture-owner#/connections?section=subscriptions");
    await page.getByRole("button",{name:"Add Subscription",exact:true}).click();
    const dialog=page.getByRole("dialog",{name:"Add Subscription",exact:true});
    await dialog.getByRole("textbox",{name:/^Name(?:\s*\*)?$/}).fill("Review received mail");
    await dialog.getByRole("textbox",{name:/^Event Type(?:\s*\*)?$/}).fill("mail.received");
    await dialog.getByRole("button",{name:"Save",exact:true}).click();await dialog.waitFor({state:"hidden"});
    await page.getByRole("heading",{name:"Review received mail",exact:true}).waitFor();
    await page.reload();await page.getByRole("heading",{name:"Review received mail",exact:true}).waitFor();
    console.log("PASS subscription changes persist across reload against the real HTTP service and ledger");
    await page.goto(url+"?token=assistant-fixture-owner#/inbox");
    await page.locator("li").filter({hasText:connection.event}).getByRole("button",{name:"Inspect",exact:true}).click();
    await page.getByRole("dialog",{name:"Event Details",exact:true}).waitFor();
    await page.getByText("A long message for review",{exact:false}).waitFor();
    await page.getByRole("button",{name:"Process with Current Subscriptions",exact:true}).click();
    await page.getByRole("button",{name:"Process with Current Subscriptions",exact:true}).waitFor({state:"visible"});
    await page.waitForFunction(()=>!document.querySelector('[role="dialog"] button[data-loading]'));
    await page.keyboard.press("Escape");
    await page.getByRole("dialog",{name:"Event Details",exact:true}).waitFor({state:"hidden"});
    await page.getByRole("button",{name:"Approve Invocation",exact:true}).click();
    await page.getByRole("button",{name:"Approve Invocation",exact:true}).waitFor({state:"hidden"});
    await page.reload();await page.getByText("No Client Requests",{exact:true}).waitFor();
    console.log("PASS original events remain inspectable and client approvals survive browser reload");
    await page.setViewportSize({width:390,height:844});
    await page.getByRole("button",{name:"Inspect",exact:true}).first().focus();await page.keyboard.press("Enter");
    await page.getByRole("dialog",{name:"Event Details",exact:true}).waitFor();
    assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true,"narrow Inbox overflowed viewport");
    await page.keyboard.press("Escape");
    await page.getByRole("dialog",{name:"Event Details",exact:true}).waitFor({state:"hidden"});
    let fail=true;await page.route("**/console/inbox?*",(route)=>fail?route.fulfill({status:503,json:{error:"Read failed for acceptance"}}):route.continue());
    await page.reload();await page.getByText("Read failed for acceptance",{exact:false}).waitFor();
    assert.equal(await page.getByRole("button",{name:"Mark Resolved",exact:true}).count(),0,"failed read falsely offered stale actions");
    fail=false;await page.getByRole("button",{name:"Refresh",exact:true}).first().click();
    await page.getByRole("button",{name:"Mark Resolved",exact:true}).first().waitFor();
    assert.deepEqual(errors,[]);console.log("PASS keyboard, narrow layout and read-failure recovery keep decisions grounded in committed facts");
} finally {
    await browser?.close();await server?.close();
    if(fixture?.exitCode===null){fixture.kill("SIGTERM");await new Promise((resolve)=>fixture.once("exit",resolve));}
    await rm(temp,{recursive:true,force:true});
}
