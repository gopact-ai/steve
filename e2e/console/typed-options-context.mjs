// Actual ConsolePage caller/context, no helper-built scope or Agent execution.
import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
import { workState } from "./work-fixture.mjs";
const web=fileURLToPath(new URL("../../web/console",import.meta.url)),temp=await mkdtemp(path.join(tmpdir(),"topt-context-"));
let server,browser;
const waitFor=async check=>{for(let i=0;i<150;i++){if(check())return;await new Promise(r=>setTimeout(r,20))}assert.fail("fixture condition not reached")};
try{
 server=await createServer({root:web,configFile:path.join(web,"vite.config.ts"),cacheDir:path.join(temp,"cache"),server:{host:"127.0.0.1",port:0,hmr:false},logLevel:"error"});await server.listen();
 const origin=`http://127.0.0.1:${server.httpServer.address().port}`;browser=await chromium.launch({headless:true});
 for(const scenario of ["F1","F2","F3"].filter(x=>!process.env.TYPED_CONTEXT_CASE||x===process.env.TYPED_CONTEXT_CASE)){
  const context=await browser.newContext({viewport:{width:1360,height:1000},serviceWorkers:"block",reducedMotion:"reduce"});const page=await context.newPage();page.setDefaultTimeout(6500);
  const f={node:"node-one",harness:"harness-one",project:"project-one",path:"/fixture/one",version:1,workspace:"workspace-one",preferred:{toggle:"false"},reads:[],posts:[],errors:[],failReport:false,reject:false,holdCap:false,capRelease:null,holdPut:false,putRelease:null,holdReport:false,reportRelease:null};
  const agent=()=>({id:"fixture-agent",node:f.node,harness:f.harness,ready:true,usable:true,current:true,place:{workspace:f.workspace,kind:"copy",node:f.node}});
  const project=()=>({id:f.project,node:f.node,path:f.path,repo:"fixture",level:"internal",version:f.version,bound:true});
  const snapshot=()=>workState({at:"",hub:{node:"fixture-hub",started:""},nodes:[{name:f.node,role:"worker",up:true}],agents:[{...agent(),eligible:true}],projects:[{...project(),agents:["fixture-agent"],workspaces:[{id:f.workspace,node:f.node,path:f.path,kind:"copy",state:"ready",agents:["fixture-agent"]}]}],tasks:[],plans:[],attempts:[],landings:[]});
  const options=()=>[{ID:"toggle",Name:f.node==="node-one"?"Old toggle":"New toggle",Type:"boolean",Category:"vendor/private",Current:"false"},...(scenario==="F3"?[{ID:"__proto__",Name:"Thinking preset",Type:"select",Category:"vendor/private",Current:"false",Choices:[{Value:"false",Label:"Literal false ID"}]}]:[])];
  const report=()=>({model:"Reported model",models:[],options:options(),preferred:f.preferred});
  await page.addInitScript(()=>{localStorage.setItem("steve.ui.locale","en");sessionStorage.setItem("steve.conversation","console:fixture");window.sources=[];window.EventSource=class{addEventListener(){}constructor(){window.sources.push(this);setTimeout(()=>this.onopen?.(),0)}close(){window.sources=window.sources.filter(s=>s!==this)}}});
  page.on("pageerror",e=>f.errors.push(String(e)));
  await page.route("**/*",async route=>{
   const req=route.request(),url=new URL(req.url()),p=url.pathname;assert.equal(url.origin,origin,"external traffic prohibited");
   if(p==="/state")return route.fulfill({json:snapshot()});
   if(p==="/console/context")return route.fulfill({json:{enabled:true,context:{conversation:"console:fixture",project:project(),agent:agent(),agents:[agent()]}}});
   if(p==="/console/selectors"){const body=structuredClone(report());f.reads.push({node:f.node,harness:f.harness,project:f.project,workspace:f.workspace});if(f.failReport){f.failReport=false;return route.fulfill({status:503,json:{error:"Fixture report unavailable"}})}if(f.holdReport)await new Promise(resolve=>{f.reportRelease=resolve});return route.fulfill({json:body});}
   if(p==="/console/preferences"){
    assert.equal(req.method(),"PUT");const body=req.postDataJSON();f.posts.push({body,node:f.node});if(f.holdPut)await new Promise(resolve=>{f.putRelease=resolve});
    if(f.reject){f.reject=false;return route.fulfill({status:400,json:{error:"Fixture preference rejected"}})}
    f.preferred={...f.preferred,...body.patch};if(body.patch.toggle==="")f.preferred.toggle="false";return route.fulfill({json:{ok:true,live:false}});
   }
   if(p==="/console/queue"){if(url.searchParams.get("capabilities")==="1"&&f.holdCap)await new Promise(resolve=>{f.capRelease=resolve});return route.fulfill({json:{queue:[],submission_keys:true}})}
   if(p==="/console/conversations")return route.fulfill({json:{enabled:true,conversations:[{id:"console:fixture",title:"Fixture",count:0,running:false}]}});
   if(p==="/console/replies")return route.fulfill({json:{enabled:true,replies:[]}});
   if(p==="/console/desktop")return route.fulfill({json:{enabled:false}});
   if(p==="/console/coordination")return route.fulfill({json:{enabled:false,nodes:[],events:[]}});
   if(p.startsWith("/console/")){assert.equal(req.method(),"GET",`unexpected mutation ${p}`);return route.fulfill({json:{}})}
   return route.continue();
  });
  await page.goto(origin+"/#/console");const trigger=page.getByRole("button",{name:"Session options",exact:true});await trigger.waitFor();
  const open=async()=>{await trigger.click();const dialog=page.getByRole("dialog",{name:"Session options",exact:true});await dialog.waitFor();return dialog};
  let dialog=await open();const group=name=>dialog.getByRole("group",{name,exact:true});
  const choose=async(name,value)=>{await group(name).getByRole("button",{name:/Requested preference/}).click();const box=page.getByRole("listbox").last();await box.getByRole("option",{name:value,exact:true}).click();await box.waitFor({state:"hidden"})};
  const settledScope=async workspace=>{await page.waitForFunction(value=>{const raw=document.querySelector('[aria-label="Session options"]')?.getAttribute("data-preference-scope");return raw&&JSON.parse(raw)[13]===value},workspace)};
  const signal=async()=>{await page.evaluate(()=>{for(const s of window.sources)s.onmessage?.({data:JSON.stringify({kind:"console.meta",conversation:"console:fixture"}),lastEventId:"fixture."+Date.now()})});};
  if(scenario==="F1"){
   await group("Old toggle").getByText("Agent reported: false",{exact:true}).waitFor();f.failReport=true;await choose("Old toggle","true");
   await dialog.getByRole("alert").filter({hasText:"Fixture report unavailable"}).waitFor();assert.equal(f.preferred.toggle,"true");
   assert.match(await group("Old toggle").getByRole("button",{name:/Requested preference/}).innerText(),/true/);await group("Old toggle").getByText("Agent reported: false",{exact:true}).waitFor();
   await group("Old toggle").getByText("Preference readback pending",{exact:false}).waitFor();
   f.reject=true;await choose("Old toggle","false");await dialog.getByRole("alert").filter({hasText:"Fixture preference rejected"}).waitFor();assert.match(await group("Old toggle").getByRole("button",{name:/Requested preference/}).innerText(),/true/);
   f.failReport=true;await choose("Old toggle","Unfixed (Agent default)");await dialog.getByRole("alert").filter({hasText:"Fixture report unavailable"}).waitFor();
   assert.match(await group("Old toggle").getByRole("button",{name:/Requested preference/}).innerText(),/Reset accepted/);await group("Old toggle").getByText("Agent reported: false",{exact:true}).waitFor();
   await dialog.getByRole("button",{name:"Reload Agent report",exact:true}).click();await group("Old toggle").getByRole("button",{name:/Requested preference/}).waitFor();
   await waitFor(()=>f.reads.length>=4);await group("Old toggle").getByText("Preference readback pending",{exact:false}).waitFor({state:"hidden"});assert.match(await group("Old toggle").getByRole("button",{name:/Requested preference/}).innerText(),/false/);
  }else if(scenario==="F2"){
   await group("Old toggle").waitFor();await page.keyboard.press("Escape");await dialog.waitFor({state:"hidden"});const before=f.reads.length;
   f.node="node-two";f.harness="harness-two";f.path="/fixture/two";f.workspace="workspace-two";f.version=2;await signal();
   await page.locator(".console-location").filter({hasText:"node-two"}).waitFor();await settledScope(f.workspace);dialog=await open();await group("New toggle").waitFor();assert.ok(f.reads.length>before,"same ID new context must refetch without manual Reload");
   f.holdCap=true;await choose("New toggle","true");await waitFor(()=>!!f.capRelease);const posts=f.posts.length;
   f.node="node-three";f.harness="harness-three";f.project="project-three";f.path="/fixture/three";f.workspace="workspace-three";f.version=3;await signal();
   await page.locator(".console-location").filter({hasText:"node-three"}).waitFor();await settledScope(f.workspace);f.holdCap=false;f.capRelease();await page.waitForTimeout(150);assert.equal(f.posts.length,posts,"stale preflight must not dispatch PUT in the new scope");
   dialog=await open();await group("New toggle").waitFor();f.holdPut=true;await choose("New toggle","true");await waitFor(()=>!!f.putRelease);
   f.node="node-four";f.harness="harness-four";f.project="project-four";f.path="/fixture/four";f.workspace="workspace-four";f.version=4;await signal();
   await page.locator(".console-location").filter({hasText:"node-four"}).waitFor();await settledScope(f.workspace);const beforeLate=f.reads.length;f.holdPut=false;f.putRelease();await page.waitForTimeout(150);assert.equal(f.reads.length,beforeLate,"old-scope accepted write must not trigger a new-scope report read");
   f.node="node-five";f.harness="harness-five";f.project="project-five";f.path="/fixture/five";f.workspace="workspace-five";f.version=5;await signal();await settledScope(f.workspace);
   f.holdReport=true;dialog=await open();await waitFor(()=>!!f.reportRelease);
   f.node="node-six";f.harness="harness-six";f.project="project-six";f.path="/fixture/six";f.workspace="workspace-six";f.version=6;await signal();await settledScope(f.workspace);
   f.holdReport=false;f.reportRelease();await page.waitForTimeout(100);assert.equal(await page.getByRole("dialog",{name:"Session options",exact:true}).count(),0,"old GET must not reopen or publish into a remounted scope");
   dialog=await open();await group("New toggle").waitFor();assert.equal(f.reads.at(-1).node,"node-six");
  }else{
   await group("Thinking preset").waitFor();assert.match(await group("Thinking preset").getByRole("button",{name:/Requested preference/}).innerText(),/Unfixed/);assert.ok(!(await page.locator("body").innerText()).includes("[object Object]"));
   await choose("Thinking preset","Literal false ID");await waitFor(()=>f.posts.length===1);assert.equal(Object.hasOwn(f.posts[0].body.patch,"__proto__"),true);assert.equal(f.posts[0].body.patch["__proto__"],"false");
  }
  assert.deepEqual(f.errors,[]);console.log(`PASS ${scenario} actual ConsolePage caller: accepted/readback separation, scope-bound async operations, explicit role/own keys; fixture only`);await context.close();
 }
}finally{await browser?.close();await server?.close();await rm(temp,{recursive:true,force:true});}
