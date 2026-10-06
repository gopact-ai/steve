// Isolated production AgentDrawer; configuration preferences never fabricate Actual.
import assert from "node:assert/strict";
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
import { workState } from "./work-fixture.mjs";
const web=fileURLToPath(new URL("../../web/console",import.meta.url)),temp=await mkdtemp(path.join(tmpdir(),"topt-fleet-"));
let server,browser;
const agent={id:"fixture-agent",harness:"fixture",node:"fixture-node",eligible:true,level:"internal",options:{},selectors:[
 {id:"toggle",name:"Fast lane",type:"boolean",category:"vendor/private",current:"false"},
 {id:"unset",name:"Unset toggle",type:"boolean"},
 {id:"__proto__",name:"Prototype-looking ID",type:"boolean",current:"false"},
 {id:"opaque",name:"Opaque choice",type:"select",category:"vendor/unknown",current:"false",values:["false","__none"],choices:["Literal false ID","Opaque reserved-looking ID"]},
 {id:"unknown",name:"Unknown option",current:"false",values:["false"],choices:["Do not guess"]},
]};
const writes=[],errors=[];let reject=true;
try {
 server=await createServer({root:web,configFile:path.join(web,"vite.config.ts"),cacheDir:path.join(temp,"cache"),server:{host:"127.0.0.1",port:0,hmr:false},logLevel:"error"});await server.listen();
 const origin=`http://127.0.0.1:${server.httpServer.address().port}`;
 browser=await chromium.launch({headless:true});const page=await browser.newPage({viewport:{width:1440,height:1000},reducedMotion:"reduce",serviceWorkers:"block"});page.setDefaultTimeout(6500);
 await page.addInitScript(()=>{localStorage.setItem("steve.ui.locale","en");window.EventSource=class{addEventListener(){}constructor(){setTimeout(()=>this.onopen?.(),0)}close(){}}});
 page.on("pageerror",e=>errors.push(String(e)));
 await page.route("**/*",async route=>{
  const req=route.request(),url=new URL(req.url());assert.equal(url.origin,origin,"external traffic prohibited");
  if(url.pathname==="/state")return route.fulfill({json:workState({at:"",hub:{node:"fixture-node",started:""},nodes:[],agents:[agent],tasks:[],plans:[],projects:[],attempts:[],landings:[]})});
  if(url.pathname==="/console/agents/fixture-agent"&&req.method()==="PUT"){
   const body=req.postDataJSON();writes.push(body);
   if(reject){reject=false;return route.fulfill({status:400,json:{error:"Fixture save rejected"}})}
   agent.options=body.options;return route.fulfill({json:{ok:true}});
  }
  if(url.pathname.startsWith("/console/")){
   assert.equal(req.method(),"GET",`unexpected mutation ${url.pathname}`);
   if(url.pathname==="/console/coordination")return route.fulfill({json:{enabled:false,nodes:[],events:[]}});
   if(url.pathname==="/console/desktop")return route.fulfill({json:{enabled:false}});
   if(url.pathname==="/console/queue")return route.fulfill({json:{queue:[],submission_keys:true}});
   return route.fulfill({json:{}});
  }
  return route.continue();
 });
 await page.goto(origin+"/#/fleet?tab=agents");await page.getByRole("row",{name:"fixture-agent",exact:true}).click();
 const drawer=page.getByRole("dialog",{name:"fixture-agent",exact:true});
 const group=name=>drawer.getByRole("group",{name,exact:true});
 await group("Fast lane").getByText("Agent reported: false",{exact:true}).waitFor();
 await group("Unset toggle").getByText("Agent reported: Not reported",{exact:true}).waitFor();
 await group("Unknown option").getByText("Unknown, missing or invalid option type; shown read-only.",{exact:true}).waitFor();
 await drawer.getByRole("button",{name:"Edit",exact:true}).click();
 const choose=async(name,value)=>{await group(name).getByRole("button",{name:/Requested preference/}).click();const listbox=page.getByRole("listbox").last();await listbox.getByRole("option",{name:value,exact:true}).click();await listbox.waitFor({state:"hidden"})};
 assert.match(await group("Fast lane").getByRole("button",{name:/Requested preference/}).innerText(),/Unfixed/);
 await choose("Fast lane","true");await group("Fast lane").getByText("Agent reported: false",{exact:true}).waitFor();
 await drawer.getByRole("button",{name:"Save",exact:true}).click();await drawer.getByRole("alert").filter({hasText:"Fixture save rejected"}).waitFor();
 assert.equal(writes.at(-1).options.toggle,"true");assert.equal(agent.options.toggle,undefined);await group("Fast lane").getByText("Agent reported: false",{exact:true}).waitFor();
 await choose("Fast lane","false");await choose("Prototype-looking ID","false");await choose("Opaque choice","Literal false ID");await drawer.getByRole("button",{name:"Save",exact:true}).click();
 await drawer.getByRole("button",{name:"Edit",exact:true}).waitFor();assert.equal(agent.options.toggle,"false");assert.equal(agent.options.opaque,"false");assert.equal(Object.hasOwn(agent.options,"__proto__"),true);assert.equal(agent.options["__proto__"],"false");
 await group("Fast lane").getByText("Agent reported: false",{exact:true}).waitFor();
 await drawer.getByRole("button",{name:"Edit",exact:true}).click();await choose("Fast lane","Unfixed (Agent default)");await choose("Prototype-looking ID","Unfixed (Agent default)");await choose("Opaque choice","Opaque reserved-looking ID");
 await drawer.getByRole("button",{name:"Save",exact:true}).click();await drawer.getByRole("button",{name:"Edit",exact:true}).waitFor();
 assert.deepEqual(agent.options,{opaque:"__none"});await group("Fast lane").getByText("Agent reported: false",{exact:true}).waitFor();
 await page.setViewportSize({width:390,height:844});assert.ok(await drawer.evaluate(el=>el.scrollWidth<=el.clientWidth));
 if(process.env.TYPED_OPTIONS_SCREENSHOTS){await mkdir(process.env.TYPED_OPTIONS_SCREENSHOTS,{recursive:true});await page.screenshot({path:path.join(process.env.TYPED_OPTIONS_SCREENSHOTS,"agent-options-narrow.png")});}
 assert.deepEqual(errors,[]);console.log("PASS real AgentDrawer typed options: explicit false/unfixed, failed-save drafts, reported immutability, opaque IDs, narrow; fixture only");
} finally {await browser?.close();await server?.close();await rm(temp,{recursive:true,force:true});}
