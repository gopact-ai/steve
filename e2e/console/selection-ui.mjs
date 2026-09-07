// Source preview only; every API is intercepted, no live hub and no dist build.
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const server = await createServer({ configFile: path.join(web, "vite.config.ts"), root: web, server: { host: "127.0.0.1", port: 0 } });
await server.listen();
const url = server.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const A="console:selection-ui", at="2026-09-07T01:00:00Z", base="a".repeat(40), after="b".repeat(40);
const replyText="A **bold passage** with [a link](https://example.test) and `code`.\n\nSecond paragraph.";
const png=Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO7+gxkAAAAASUVORK5CYII=","base64");
const context=await browser.newContext({viewport:{width:1600,height:1000},serviceWorkers:"block"});const page=await context.newPage();page.setDefaultTimeout(7000);
const f={version:"test",project:"p",captureGate:null,releaseCapture:null,supportsMaterials:true,captures:[],posts:[],queue:[],materials:new Map(),notes:[],answers:[],questions:[],hideQueue:false,reset:false,errors:[]};
page.on("pageerror",e=>f.errors.push(String(e)));
function material(id,title,source,kind="text",mime="text/plain",data=Buffer.from(replyText)) {const value={id,project:"p",title,source,kind,mime,size:data.length,digest:"d".repeat(64),created_at:at,...(kind==="image"?{width:1,height:1}:{})};f.materials.set(id,{value,data});return value;}
await page.route("**/*",async route=>{
 const req=route.request(),u=new URL(req.url()),p=u.pathname;
 if(u.origin!==new URL(url).origin){f.errors.push("external "+u.origin);return route.abort();}
 if(!p.startsWith("/console/")&&!['/state','/events'].includes(p))return route.continue();
 const input=req.method()==="GET"?null:(req.headers()['content-type']||'').includes('application/json')?req.postDataJSON():null;
 if (p === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false } });
 if(p==="/console/desktop")return route.fulfill({json:{enabled:false,setup_required:false,agent_count:0}});
 if(p==="/state")return route.fulfill({json:{at,hub:{node:"test-hub",version:f.version},nodes:[],agents:[],projects:[{id:"p",node:"test-hub",path:"/work/p",repo:"inplace",level:"public",agents:[],workspaces:[]}],tasks:[{id:"11",channel:A,project_id:"p",goal:"Code task",state:"running",lifecycle:"running",execution:"idle",lane:"pending",attention:0,turns:1,max_turns:10,updated_at:at}],plans:[],attempts:[],landings:[]}});
 if(p==="/console/send"){f.posts.push(input);return route.fulfill({json:{reply:{id:"binding",conversation:input.conversation,text:"bound",at,kind:"reply"}}});}
 if(p==="/console/context")return route.fulfill({json:{enabled:true,context:{conversation:u.searchParams.get("conversation"),project:{id:f.project,node:"test-hub",path:"/work/p",repo:"inplace",level:"public",bound:true},agents:[]}}});
 if(p==="/console/replies" && u.searchParams.get("conversation")!==A)return route.fulfill({json:{enabled:true,replies:[]}});
 if(p==="/console/replies")return route.fulfill({json:{enabled:true,replies:[{id:"r1",conversation:A,kind:"reply",at,text:replyText,project_id:"p",revision:"reply-version-1",changes:{attempt:"attempt1",project:"p",base,artifact:after,files:1,added:2,deleted:1}}]}});
 if(p==="/console/conversations")return route.fulfill({json:{conversations:[{id:A,title:"Material conversation",project:"p",count:1,last_at:at,running:false}]}});
 if(p==="/console/verbs")return route.fulfill({json:{verbs:[]}});
 if(p==="/console/suggest")return route.fulfill({json:{suggestions:[]}});
 if(p==="/console/queue"){
  if(req.method()==="POST"){f.posts.push(input);let item=f.queue.find(e=>e.key==="client:"+input.command_id);if(!item){item={id:"e"+(f.queue.length+1),conversation:input.conversation,key:"client:"+input.command_id,input:input.input,refs:input.refs,locale:input.locale,quotes:input.quotes,state:"queued",enqueued_at:at};f.queue.push(item);}if(f.reset){f.reset=false;return route.abort('connectionreset');}return route.fulfill({json:item});}
  return route.fulfill({json:{queue:f.hideQueue?[]:f.queue.filter(e=>e.conversation===u.searchParams.get("conversation")),submission_keys:true,material_refs:f.supportsMaterials,interactive_requests:true}});
 }
 if(p==="/console/materials/capture"){f.captures.push(input);if(f.captureGate)await f.captureGate;assert.equal(input.project,"p");if(input.source.kind==="reply")assert.equal(input.source.revision,"reply-version-1");return route.fulfill({json:material("m-"+f.captures.length,input.title||input.source.path||"Reference reply",input.source)});}
 if(p==="/console/materials/upload"){const kind=req.headers()['content-type'].startsWith('image/')?'image':'binary';return route.fulfill({json:material("m-upload-"+f.materials.size,u.searchParams.get('name'),{kind:'upload'},kind,req.headers()['content-type'],req.postDataBuffer())});}
 if(p.match(/^\/console\/materials\/[^/]+\/content$/)){const m=f.materials.get(p.split('/')[3]);return route.fulfill({contentType:m.value.mime,body:m.data});}
 if(p.match(/^\/console\/materials\/[^/]+$/)){const m=f.materials.get(p.split('/')[3]);return route.fulfill({json:m.value});}
 if(p==="/console/annotations")return route.fulfill({json:{annotations:f.notes.filter(n=>!n.deleted)}});
 if(p.startsWith('/console/annotations/')){const id=p.split('/')[3],old=f.notes.find(n=>n.id===id);if((old?.revision||0)!==input.expected_revision)return route.fulfill({status:409,json:{error:'Annotation changed'}});const note={...input,id,author:'owner',revision:(old?.revision||0)+1,created_at:at,updated_at:at};f.notes=f.notes.filter(n=>n.id!==id);f.notes.push(note);return route.fulfill({json:note});}
 if(p==="/console/questions")return route.fulfill({json:{questions:f.questions}});
 if(p.match(/^\/console\/questions\/[^/]+\/answer$/)){f.answers.push(input);if(input.decision!=='accept'&&input.choice)return route.fulfill({status:400,json:{error:'Non-accept decision must not include choice'}});await new Promise(r=>setTimeout(r,150));const q=f.questions.find(q=>q.id===p.split('/')[3]);q.state=input.decision==='accept'?'answered':input.decision==='decline'?'declined':'cancelled';q.answer=input;return route.fulfill({json:{question:q}});}
 if(p==="/console/tasks/11/attempts")return route.fulfill({json:[{id:'attempt1',kind:'task',state:'done',base,artifact:after,started_at:at,files:1}]});
 if(p==="/console/attempts/attempt1/changes")return route.fulfill({json:{attempt:'attempt1',project:'p',base,artifact:after,changes:[{path:'app.ts',status:'M',added:2,deleted:1}]}});
 if(p==="/console/attempts/attempt1/tree")return route.fulfill({json:{attempt:'attempt1',commit:after,which:'result',dir:'',entries:[{name:'app.ts',path:'app.ts',kind:'file',size:20}]}});
 if(p==="/console/attempts/attempt1/file")return route.fulfill({json:{attempt:'attempt1',commit:after,path:'app.ts',text:'first\nsecond\nthird\n',size:20}});
 if(p==="/console/attempts/attempt1/diff")return route.fulfill({json:{path:'app.ts',diff:'--- a/app.ts\n+++ b/app.ts\n@@ -1,2 +1,3 @@\n-old\n+first\n+second\n third\n'}});
 f.errors.push(req.method()+" "+p);return route.fulfill({status:500,json:{error:'Unmocked API'}});
});
await page.addInitScript((conversation)=>{sessionStorage.setItem('steve.conversation',conversation);localStorage.setItem('steve.ui.locale','en');window.sources=[];window.EventSource=class{constructor(){window.sources.push(this);setTimeout(()=>this.onopen?.(),0)}close(){window.sources=window.sources.filter(s=>s!==this)}};window.emit=(e)=>window.sources.forEach(s=>s.onmessage?.({data:JSON.stringify(e)}));},A);
async function waitFor(test,label){for(let i=0;i<100;i++){if(await test())return;await new Promise(r=>setTimeout(r,30));}assert.fail(label);}

async function select(locator, needle) {
 await locator.evaluate((element, text) => {
  const walker=document.createTreeWalker(element,NodeFilter.SHOW_TEXT);const nodes=[];let node;while(node=walker.nextNode()) {if(node.parentElement.closest('button,[data-selection-ignore],.selection-toolbar'))continue;nodes.push(node)}
  const joined=nodes.map(n=>n.textContent).join('');const start=joined.indexOf(text);if(start<0)throw new Error('Missing text '+text+' in '+joined);
  let offset=0,first,last;for(const n of nodes){const end=offset+n.textContent.length;if(first===undefined&&start<end)first=[n,start-offset];if(start+text.length<=end&&first){last=[n,start+text.length-offset];break}offset=end}
  const range=document.createRange();range.setStart(...first);range.setEnd(...last);const selection=window.getSelection();selection.removeAllRanges();selection.addRange(range);document.dispatchEvent(new Event('selectionchange'));
 },needle);
 await page.getByRole('toolbar',{name:'Selection actions'}).waitFor();
}
try {
 await page.goto(url+'#/console');await page.getByRole('heading',{name:'Material conversation',exact:true}).waitFor();
 const message=page.locator('.message-assistant [data-selection-surface]').first();
 await select(message,'bold passage with a link');
 const bar=page.getByRole('toolbar',{name:'Selection actions'});await bar.getByRole('button',{name:'Add to chat',exact:true}).click();
 await waitFor(async()=>!!(await page.evaluate(A=>JSON.parse(sessionStorage.getItem('steve.console.drafts')||'{}').materials?.[A]?.length,A)),'selection enters draft');
 const refs=await page.evaluate(A=>JSON.parse(sessionStorage.getItem('steve.console.drafts')).materials[A],A);
 assert.equal(refs[0].selector.kind,'quote');assert.ok(replyText.includes(refs[0].selector.quote));assert.equal(refs[0].selector.quote,'bold passage** with [a link');assert.equal(f.posts.length,0);
 console.log('PASS floating selection maps formatted reply to immutable raw source without sending');
 await select(message,'Second paragraph.');await page.keyboard.press('Tab');assert.equal(await bar.getByRole('button',{name:'Add to chat',exact:true}).evaluate(e=>e===document.activeElement),true);await page.keyboard.press('Escape');await bar.waitFor({state:'hidden'});
 await select(message,'Second paragraph.');await bar.getByRole('button',{name:'More details',exact:true}).click();await page.getByRole('dialog',{name:'Preview',exact:true}).getByText('Second paragraph.',{exact:true}).waitFor();await page.getByRole('dialog',{name:'Preview',exact:true}).getByRole('button',{name:'Close',exact:true}).click();
 console.log('PASS selection keyboard actions and source details');
 await select(message,'bold passage');await bar.getByRole('button',{name:'Ask in side chat',exact:true}).click();await page.locator('[data-side-chat]').waitFor();assert.equal(f.posts.length,0);assert.equal(await page.evaluate(()=>sessionStorage.getItem('steve.conversation')),A);
 const side=page.locator('[data-side-chat]');await side.getByRole('textbox').last().fill('Explain this wording');await side.getByRole('button',{name:'Send',exact:true}).click();
 await waitFor(()=>f.posts.some(p=>p.input==='Explain this wording'),'side sends explicit question');const sent=f.posts.find(p=>p.input==='Explain this wording');assert.notEqual(sent.conversation,A);assert.equal(sent.refs.length,1);assert.equal(sent.refs[0].selector.quote,'bold passage');assert.equal(await page.evaluate(()=>sessionStorage.getItem('steve.conversation')),A);
 await side.getByRole('button',{name:'Close side chat',exact:true}).click();
 console.log('PASS side question uses separate same-project conversation and preserves main context');
 await page.getByRole('button',{name:/Review changes/}).first().click();
 await page.getByRole('dialog',{name:'Code workspace',exact:true}).waitFor();
 const summaryBox=await page.locator('.review-summary').boundingBox(),workspaceBox=await page.locator('.review-workspace').boundingBox();assert.ok(summaryBox.height<80&&summaryBox.width>1500&&workspaceBox.x<5,'Summary must sit above the full-width review workspace');
 const deleted=page.locator('code[data-selection-before="1"]').first();await select(deleted,'old');await bar.getByRole('button',{name:'Add to chat',exact:true}).click();await waitFor(()=>f.captures.at(-1)?.source.commit===base,'deleted selection captures base');
 await page.getByRole('button',{name:'Source',exact:true}).click();await page.locator('.source-text').first().waitFor();await select(page.locator('.source-code'),'second');await bar.getByRole('button',{name:'Add to chat',exact:true}).click();await waitFor(()=>f.captures.at(-1)?.source.commit===after,'source selection captures result');
 await select(page.locator('.source-code'),'third');await bar.getByRole('button',{name:'Ask in side chat',exact:true}).click();await page.getByRole('dialog',{name:'Code workspace',exact:true}).locator('[data-side-chat]').waitFor();
 await page.setViewportSize({width:390,height:844});assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth),true);await side.getByRole('button',{name:'Close side chat',exact:true}).click();await select(page.locator('.source-code'),'second');const box=await bar.boundingBox();assert.ok(box.x>=0&&box.x+box.width<=390&&box.y>=0&&box.y+box.height<=844);
 await page.screenshot({path:'/tmp/steve-selection-review-mobile.png'});console.log('PASS source/deleted-side references, Review side chat and narrow toolbar placement');
 await page.keyboard.press('Escape');await page.getByRole('dialog',{name:'Code workspace',exact:true}).getByRole('button',{name:'Back',exact:true}).click();await page.setViewportSize({width:1600,height:1000});
 const draft=page.getByRole('textbox',{name:'Message',exact:true});await draft.fill('Keep this draft while the project changes');
 const saved=await page.evaluate(A=>JSON.parse(sessionStorage.getItem('steve.console.drafts')).materials[A],A),writes=f.posts.length,captures=f.captures.length;
 f.captureGate=new Promise(resolve=>f.releaseCapture=resolve);
 await select(message,'Second paragraph.');await bar.getByRole('button',{name:'Add to chat',exact:true}).click();await waitFor(()=>f.captures.length===captures+1,'delayed capture begins');
 f.project='q';await page.evaluate(event=>window.emit(event),{kind:'console.reply',conversation:A,reply_id:'binding-change',text:'Project changed',at});await page.getByText('q · test-hub',{exact:true}).waitFor();
 f.releaseCapture();f.captureGate=null;await bar.getByRole('alert').waitFor();
 assert.deepEqual(await page.evaluate(A=>JSON.parse(sessionStorage.getItem('steve.console.drafts')).materials[A],A),saved,'A delayed capture cannot add old-project material after the target is rebound');
 assert.equal(await draft.inputValue(),'Keep this draft while the project changes');assert.equal(f.posts.length,writes,'Rejecting a stale capture cannot submit a question');
 console.log('PASS delayed capture rechecks the target project without changing drafts or sending');
 assert.deepEqual(f.errors,[]);
} catch(error) {console.log("DEBUG",f.errors,await page.locator("body").innerText());await page.screenshot({path:"/tmp/steve-selection-error.png"});throw error;} finally { f.releaseCapture?.();await context.close();await browser.close();await server.close(); }
