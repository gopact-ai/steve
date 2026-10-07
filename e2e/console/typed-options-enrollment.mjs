// Production enrollment selector controls with isolated typed observations.
import assert from "node:assert/strict";
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
import { workState } from "./work-fixture.mjs";
const web=fileURLToPath(new URL("../../web/console",import.meta.url)),temp=await mkdtemp(path.join(tmpdir(),"topt-enroll-"));
let server,browser;
const selectors=()=>[
 {id:"toggle",name:"Fast lane",type:"boolean",category:"vendor/private",current:"false"},
 {id:"unset",name:"Unset toggle",type:"boolean"},
 {id:"__proto__",name:"Prototype option",type:"boolean",current:"false"},
 {id:"opaque",name:"Opaque choice",type:"select",category:"vendor/unknown",current:"false",values:["false","__default"],choices:["Literal false ID","Opaque default-looking ID"]},
 {id:"unknown",name:"Unknown option",current:"false",values:["true"],choices:["Do not guess"]},
 {id:"future",name:"Future option",type:"future",current:"false"},
];
try {
 server=await createServer({root:web,configFile:path.join(web,"vite.config.ts"),cacheDir:path.join(temp,"cache"),server:{host:"127.0.0.1",port:0,hmr:false},logLevel:"error"});await server.listen();
 const origin=`http://127.0.0.1:${server.httpServer.address().port}`;browser=await chromium.launch({headless:true});
 for(const surface of ["node","desktop"].filter(value=>!process.env.TYPED_ENROLL_SURFACE||value===process.env.TYPED_ENROLL_SURFACE)){
  const context=await browser.newContext({viewport:{width:1280,height:980},serviceWorkers:"block",reducedMotion:"reduce"});const page=await context.newPage();page.setDefaultTimeout(6500);
  const observed=selectors(),posts=[],unexpected=[],errors=[];
  const candidate={id:"fixture",name:"Fixture tool",harness:"fixture",installed:true,requires:[],configured:true,registered:false,selectors:observed,model:"fixture-model",models:["fixture-model","alternate-model"]};
  const status={enabled:surface==="desktop",node_id:"fixture-hub",agent_count:0,local_agent_count:0,setup_required:surface==="desktop",setup:{step:"agents",done:false}};
  await page.addInitScript(()=>{localStorage.setItem("steve.ui.locale","en");window.EventSource=class{addEventListener(){}constructor(){setTimeout(()=>this.onopen?.(),0)}close(){}}});
  page.on("pageerror",error=>errors.push(String(error)));
  await page.route("**/*",async route=>{
   const req=route.request(),url=new URL(req.url()),p=url.pathname;assert.equal(url.origin,origin,"external traffic prohibited");
   if(p==="/state")return route.fulfill({json:workState({at:"",hub:{node:"fixture-hub",started:""},nodes:[{name:"fixture-worker",role:"worker",up:true}],agents:[],tasks:[],plans:[],projects:[],attempts:[],landings:[]})});
   if(p==="/console/desktop")return route.fulfill({json:status});
   if(p==="/console/desktop/agents"||p==="/console/nodes/fixture-worker/agents"){
    if(req.method()==="GET")return route.fulfill({json:surface==="node"?{revision:"fixture-r1",agents:[candidate]}:{agents:[candidate]}});
    assert.equal(req.method(),"POST");posts.push(req.postDataJSON());return route.fulfill({status:403,json:{error:"Fixture registration rejected"}});
   }
   if(p==="/console/coordination")return route.fulfill({json:{enabled:false,nodes:[],events:[]}});
   if(p==="/console/queue")return route.fulfill({json:{queue:[],submission_keys:true}});
   if(p.startsWith("/console/")){
    if(req.method()!=="GET"){unexpected.push(req.method()+" "+p);return route.fulfill({status:500,json:{error:"unexpected mutation"}})}
    if(p==="/console/context")return route.fulfill({json:{enabled:true,context:{conversation:"console:fixture",agents:[]}}});
    if(p==="/console/conversations")return route.fulfill({json:{enabled:true,conversations:[]}});
    if(p==="/console/replies")return route.fulfill({json:{enabled:true,replies:[]}});
    return route.fulfill({json:{}});
   }
   return route.continue();
  });
  await page.goto(origin+(surface==="node"?"/#/fleet?tab=machines":"/#/console"));
  if(surface==="node"){
   await page.getByRole("row",{name:/fixture-worker/}).click();await page.getByRole("button",{name:"Register agents on this machine",exact:true}).click();
  }
  const dialog=page.getByRole("dialog",{name:surface==="node"?"Register agents on fixture-worker":"First-time setup",exact:true});await dialog.waitFor();
  await dialog.getByText("Fixture tool",{exact:true}).first().click();
  const tool=dialog.getByRole("group",{name:"Fixture tool",exact:true});
  const option=name=>tool.getByRole("group",{name,exact:true});
  await option("Fast lane").getByText("Agent reported: false",{exact:true}).waitFor();
  assert.match(await option("Fast lane").getByRole("button",{name:/Requested preference/}).innerText(),/Unfixed/);
  await option("Unset toggle").getByText("Agent reported: Not reported",{exact:true}).waitFor();
  for(const name of ["Unknown option","Future option"])assert.equal(await option(name).getByRole("button").count(),0);
  const choose=async(name,value)=>{await option(name).getByRole("button",{name:/Requested preference/}).click();const listbox=page.getByRole("listbox").last();await listbox.getByRole("option",{name:value,exact:true}).click();await listbox.waitFor({state:"hidden"})};
  await choose("Fast lane","true");await option("Fast lane").getByText("Agent reported: false",{exact:true}).waitFor();
  await choose("Fast lane","Unfixed (Agent default)");
  await choose("Fast lane","false");await choose("Prototype option","false");await choose("Opaque choice","Opaque default-looking ID");
  await tool.getByRole("button",{name:/Model$/}).click();{const box=page.getByRole("listbox").last();await box.getByRole("option",{name:"alternate-model",exact:true}).click();await box.waitFor({state:"hidden"});}
  assert.equal(posts.length,0,"changing preference controls cannot register/install/bind");
  assert.deepEqual(observed,selectors(),"controls cannot mutate reported observations");
  await page.reload();
  if(surface==="node"){await page.getByRole("row",{name:/fixture-worker/}).click();await page.getByRole("button",{name:"Register agents on this machine",exact:true}).click()}
  await option("Fast lane").getByRole("button",{name:/Requested preference/}).waitFor();
  assert.match(await option("Fast lane").getByRole("button",{name:/Requested preference/}).innerText(),/false/,"an actual false pin must survive reload on both surfaces");
  await option("Fast lane").getByText("Agent reported: false",{exact:true}).waitFor();
  assert.match(await option("Prototype option").getByRole("button",{name:/Requested preference/}).innerText(),/false/);assert.match(await tool.getByRole("button",{name:/Model$/}).innerText(),/alternate-model/);
  assert.match(await option("Unset toggle").getByRole("button",{name:/Requested preference/}).innerText(),/Unfixed/);
  await dialog.getByRole("button",{name:"Register selected agents",exact:true}).click();
  await dialog.getByRole("alert").filter({hasText:"Fixture registration rejected"}).waitFor();
  assert.equal(posts.length,1);assert.equal(posts[0].agents[0].options.toggle,"false");assert.equal(posts[0].agents[0].options.opaque,"__default");
  assert.equal(posts[0].agents[0].options.unset,undefined);assert.equal(posts[0].agents[0].options.unknown,undefined);assert.equal(posts[0].agents[0].model,"alternate-model");assert.equal(Object.hasOwn(posts[0].agents[0].options,"__proto__"),true);assert.equal(posts[0].agents[0].options["__proto__"],"false");
  if(surface==="desktop"){await page.reload();await option("Fast lane").getByRole("button",{name:/Requested preference/}).waitFor();assert.equal(posts.length,1,"pending receipt reload cannot blindly repeat registration");assert.equal(await option("Fast lane").getByRole("button",{name:/Requested preference/}).isDisabled(),true);}
  await option("Fast lane").getByText("Agent reported: false",{exact:true}).waitFor();
  await page.setViewportSize({width:390,height:844});assert.ok(await dialog.evaluate(el=>el.scrollWidth<=el.clientWidth));
  if(process.env.TYPED_OPTIONS_SCREENSHOTS){await option("Fast lane").scrollIntoViewIfNeeded();await mkdir(process.env.TYPED_OPTIONS_SCREENSHOTS,{recursive:true});await page.screenshot({path:path.join(process.env.TYPED_OPTIONS_SCREENSHOTS,`enrollment-${surface}-narrow.png`)});}
  if(surface==="desktop"){
   await page.setViewportSize({width:1280,height:980});
   const key="steve.desktop.enrollment:fixture-hub",otherKey="fixture.other-user-setting",otherValue="unchanged";
   const draft={selected:["fixture"],names:{fixture:"draft-name"},about:{fixture:"draft description"},primary:"fixture"};
   const stored={...draft,models:{fixture:"",["__proto__"]:"alternate-model"},options:{fixture:{toggle:"",unset:"",opaque:"false",["__proto__"]:"false"},["__proto__"]:{toggle:"true"}}};
   const seed=async value=>{await page.evaluate(({key,otherKey,otherValue,value})=>{localStorage.setItem(key,value);localStorage.setItem(otherKey,otherValue)}, {key,otherKey,otherValue,value});await page.reload();await dialog.getByRole("checkbox",{name:"Fixture tool",exact:true}).waitFor()};
   const unchanged=async value=>{assert.equal(await page.evaluate(key=>localStorage.getItem(key),key),value,"loading a draft cannot rewrite storage");assert.equal(await page.evaluate(key=>localStorage.getItem(key),otherKey),otherValue);assert.equal(posts.length,1,"restoring storage cannot resend a receipt or install/register");assert.equal(await page.evaluate(()=>Object.hasOwn(Object.prototype,"toggle")),false,"prototype-looking IDs cannot pollute prototypes")};
   const checkOtherFields=async()=>{assert.equal(await tool.getByRole("textbox",{name:"Name",exact:true}).inputValue(),"draft-name");assert.equal(await tool.getByRole("textbox",{name:"Good for",exact:true}).inputValue(),"draft description");await dialog.getByText("draft-name will be the default after registration.",{exact:true}).waitFor()};
   let raw=JSON.stringify(stored);await seed(raw);await checkOtherFields();
   assert.match(await option("Fast lane").getByRole("button",{name:/Requested preference/}).innerText(),/Unfixed/);
   assert.match(await option("Unset toggle").getByRole("button",{name:/Requested preference/}).innerText(),/Unfixed/);
   assert.match(await tool.getByRole("button",{name:/Model$/}).innerText(),/Follow the tool default/);
   assert.match(await option("Opaque choice").getByRole("button",{name:/Requested preference/}).innerText(),/Literal false ID/);
   assert.match(await option("Prototype option").getByRole("button",{name:/Requested preference/}).innerText(),/false/);
   await option("Fast lane").getByText("Agent reported: false",{exact:true}).waitFor();await unchanged(raw);
   for(const invalid of [{models:{fixture:7},options:{fixture:{toggle:false}}},{models:["alternate-model"],options:{fixture:["false"]}},{models:null,options:{fixture:null}}]){
    raw=JSON.stringify({...draft,...invalid});await seed(raw);await checkOtherFields();
    assert.match(await tool.getByRole("button",{name:/Model$/}).innerText(),/Follow the tool default/);
    assert.match(await option("Fast lane").getByRole("button",{name:/Requested preference/}).innerText(),/Unfixed/);
    assert.match(await option("Prototype option").getByRole("button",{name:/Requested preference/}).innerText(),/Unfixed/);
    await unchanged(raw);
   }
   raw=JSON.stringify({...stored,pending:[{candidate_id:"old-unknown-tool",agent_id:"previous-agent",model:"previous-model",options:{toggle:"false"}}]});await seed(raw);await checkOtherFields();
   assert.equal(await option("Fast lane").getByRole("button",{name:/Requested preference/}).isDisabled(),true);await unchanged(raw);
   raw="{invalid-json";await seed(raw);assert.equal(await dialog.getByRole("checkbox",{name:"Fixture tool",exact:true}).isChecked(),false);assert.equal(await tool.count(),0);await unchanged(raw);
   await page.evaluate(key=>{const original=Storage.prototype.setItem;Storage.prototype.setItem=function(k,v){if(k===key)throw new DOMException("fixture storage blocked","QuotaExceededError");return original.call(this,k,v)}},key);
   await dialog.getByText("Fixture tool",{exact:true}).first().click();await dialog.getByRole("alert").filter({hasText:"Your local selection could not be saved"}).waitFor();assert.equal(posts.length,1);assert.equal(await page.evaluate(key=>localStorage.getItem(key),key),raw);assert.equal(await page.evaluate(key=>localStorage.getItem(key),otherKey),otherValue);
   console.log("PASS Desktop string-only storage validation, own prototype IDs, empty/unfixed model/options, other settings retained, unknown pending untouched/no resend, original storage error; fixture only");
  }
  assert.deepEqual(unexpected,[]);assert.deepEqual(errors,[]);console.log(`PASS ${surface} typed selector controls: false/unfixed, unknown read-only, persisted draft, opaque strings, reject keeps Reported; fixture only`);
  await context.close();
 }
}finally{await browser?.close();await server?.close();await rm(temp,{recursive:true,force:true});}
