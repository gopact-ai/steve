import { workState, nativeHistory } from "./work-fixture.mjs";
// Source preview only; every API is intercepted, no live hub and no dist build.
import assert from "node:assert/strict";
import path from "node:path";
import { mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createServer } from "../../web/console/node_modules/vite/dist/node/index.js";
import { chromium } from "../../web/console/node_modules/playwright/index.mjs";
import { draftOf } from "./composer.mjs";
const web = fileURLToPath(new URL("../../web/console", import.meta.url));
const server = await createServer({ configFile: path.join(web, "vite.config.ts"), root: web, server: { host: "127.0.0.1", port: 0 } });
await server.listen();
const url = server.resolvedUrls.local[0];
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
const A="console:material-ui", at="2026-09-07T01:00:00Z", base="a".repeat(40), after="b".repeat(40);
const png=Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO7+gxkAAAAASUVORK5CYII=","base64");
const context=await browser.newContext({viewport:{width:1600,height:1000},serviceWorkers:"block"});const page=await context.newPage();page.setDefaultTimeout(7000);
const f={version:"test",supportsMaterials:true,captures:[],posts:[],queue:[],materials:new Map(),notes:[],answers:[],questions:[],hideQueue:false,reset:false,errors:[],uploads:[],uploadFailure:null,contextMissing:false,uploadHold:null,capabilities:[],capabilityFailure:null,queueReads:0};
page.on("pageerror",e=>f.errors.push(String(e)));
function material(id,title,source,kind="text",mime="text/plain",data=Buffer.from("first\nsecond\nthird\n")) {const value={id,project:"p",title,source,kind,mime,size:data.length,digest:"d".repeat(64),created_at:at,...(kind==="image"?{width:1,height:1}:{})};f.materials.set(id,{value,data});return value;}
const note=material("m-seed-1","Design note",{kind:'upload'});material("m-seed-2","Trace log",{kind:'upload'});
// What a line carried has to be drawn on the line: a picture as the
// picture, a file as a named file, a cut of text as the text.
const shot=material("m-shot","screenshot.png",{kind:'upload'},"image","image/png",png);
const report=material("m-report","report.pdf",{kind:'upload'},"binary","application/pdf",Buffer.from("%PDF-1.4 report"));
const frozen=(m,selector,text)=>({ref:{id:m.id,...(selector?{selector}:{})},material:m,...(text?{text}:{})});
const carried={refs:[{id:shot.id},{id:report.id},{id:note.id,selector:{kind:"lines",start:1,end:2}},{id:"m-gone"}],materials:[frozen(shot),frozen(report),frozen(note,{kind:"lines",start:1,end:2},"first\nsecond")]};
const initialMaterials=new Map(f.materials);
await page.route("**/*",async route=>{
 const req=route.request(),u=new URL(req.url()),p=u.pathname;
 if(u.origin!==new URL(url).origin){f.errors.push("external "+u.origin);return route.abort();}
 if(!p.startsWith("/console/")&&!['/state','/events'].includes(p))return route.continue();
 const input=req.method()==="GET"?null:(req.headers()['content-type']||'').includes('application/json')?req.postDataJSON():null;
 if (p === "/console/coordination") return route.fulfill({ json: { enabled: false, nodes: [], events: [], epoch: 0, revision: 0, authoritative: false, observed_at: "", auto_failover: false, ready: false } });
 if(p==="/console/desktop")return route.fulfill({json:{enabled:false,setup_required:false,agent_count:0}});
 if(p==="/state")return route.fulfill({json:workState({at,hub:{node:"test-hub",version:f.version},nodes:[],agents:[],projects:[{id:"p",node:"test-hub",path:"/work/p",repo:"inplace",level:"public",agents:[],workspaces:[]}],tasks:[{id:"11",channel:A,project_id:"p",goal:"Code task",state:"running",lifecycle:"running",execution:"idle",lane:"pending",attention:0,turns:1,max_turns:10,updated_at:at}],plans:[],attempts:[],landings:[]})});
 if(p==="/console/context")return route.fulfill({json:{enabled:true,context:f.contextMissing?null:{conversation:A,project:{id:"p",node:"test-hub",path:"/work/p",repo:"inplace",level:"public",bound:true},agents:[]}}});
 if(p==="/console/replies")return route.fulfill({json:{enabled:true,replies:[{id:"r0",conversation:A,kind:"sent",at,input:"Look at these",project_id:"p",...carried},{id:"r1",conversation:A,kind:"reply",at,text:"Reference reply",project_id:"p",revision:"reply-version-1"}]}});
 if(p==="/console/conversations")return route.fulfill({json:{conversations:[{id:A,title:"Material conversation",project:"p",count:1,last_at:at,running:false}]}});
 if(p==="/console/verbs")return route.fulfill({json:{verbs:[]}});
 if(p==="/console/suggest")return route.fulfill({json:{suggestions:[]}});
 if(p==="/console/queue"){
  if(req.method()==="GET")f.queueReads++;
  if(req.method()==="GET"&&f.capabilityFailure?.queue){const failure=f.capabilityFailure;return route.fulfill({status:failure.status,contentType:failure.contentType||'application/json',body:failure.body});}
  if(req.method()==="GET"&&u.searchParams.get('capabilities')==='1'){
   f.capabilities.push(req.url());
   if(f.capabilityFailure){const failure=f.capabilityFailure;if(failure.abort)return route.abort('failed');return route.fulfill({status:failure.status,contentType:failure.contentType||'application/json',body:failure.body});}
  }
  if(req.method()==="POST"){f.posts.push(input);let item=f.queue.find(e=>e.key==="client:"+input.command_id);if(!item){item={id:"e"+(f.queue.length+1),conversation:input.conversation,key:"client:"+input.command_id,input:input.input,refs:input.refs,locale:input.locale,quotes:input.quotes,state:"queued",enqueued_at:at};f.queue.push(item);}if(f.reset){f.reset=false;return route.abort('connectionreset');}return route.fulfill({json:item});}
  return route.fulfill({json:{queue:f.hideQueue?[]:f.queue,submission_keys:true,material_refs:f.supportsMaterials,interactive_requests:true}});
 }
 if(p==="/console/materials")return route.fulfill({json:{materials:[...f.materials.values()].map(m=>m.value)}});
 if(p==="/console/materials/capture"){f.captures.push(input);assert.equal(input.project,"p");if(input.source.kind==="reply")assert.equal(input.source.revision,"reply-version-1");return route.fulfill({json:material("m-"+f.captures.length,input.title||input.source.path||"Reference reply",input.source)});}
 if(p==="/console/materials/upload"){
  f.uploads.push({name:u.searchParams.get('name'),mime:req.headers()['content-type'],locale:req.headers()['accept-language'],data:req.postDataBuffer()});
  if(f.uploadFailure && f.uploadFailure.name===u.searchParams.get('name')) {
   const failure=f.uploadFailure;
   if(failure.abort)return route.abort('failed');
   return route.fulfill({status:failure.status,contentType:failure.contentType||'application/json',body:failure.body});
  }
  if(f.uploadHold && f.uploadHold.name===u.searchParams.get('name'))await f.uploadHold.promise;
  const kind=req.headers()['content-type'].startsWith('image/')?'image':'binary';return route.fulfill({json:material("m-upload-"+f.materials.size,u.searchParams.get('name'),{kind:'upload'},kind,req.headers()['content-type'],req.postDataBuffer())});}
 if(p.match(/^\/console\/materials\/[^/]+\/content$/)){const m=f.materials.get(p.split('/')[3]);return route.fulfill({contentType:m.value.mime,body:m.data});}
 if(p.match(/^\/console\/materials\/[^/]+$/)){const m=f.materials.get(p.split('/')[3]);return route.fulfill({json:m.value});}
 if(p==="/console/annotations")return route.fulfill({json:{annotations:f.notes.filter(n=>!n.deleted)}});
 if(p.startsWith('/console/annotations/')){const id=p.split('/')[3],old=f.notes.find(n=>n.id===id);if((old?.revision||0)!==input.expected_revision)return route.fulfill({status:409,json:{error:'Annotation changed'}});const note={...input,id,author:'owner',revision:(old?.revision||0)+1,created_at:at,updated_at:at};f.notes=f.notes.filter(n=>n.id!==id);f.notes.push(note);return route.fulfill({json:note});}
 if(p==="/console/questions")return route.fulfill({json:{questions:f.questions}});
 if(p.match(/^\/console\/questions\/[^/]+\/answer$/)){f.answers.push(input);if(input.decision!=='accept'&&input.choice)return route.fulfill({status:400,json:{error:'Non-accept decision must not include choice'}});await new Promise(r=>setTimeout(r,150));const q=f.questions.find(q=>q.id===p.split('/')[3]);q.state=input.decision==='accept'?'answered':input.decision==='decline'?'declined':'cancelled';q.answer=input;return route.fulfill({json:{question:q}});}
 if(p==="/console/attempts")return route.fulfill({json:nativeHistory([{id:'attempt1',kind:'task',state:'done',base,artifact:after,started_at:at,files:1}])});
 if(p==="/console/attempts/attempt1/changes")return route.fulfill({json:{attempt:'attempt1',project:'p',base,artifact:after,changes:[{path:'app.ts',status:'M',added:2,deleted:1}]}});
 if(p==="/console/attempts/attempt1/tree")return route.fulfill({json:{attempt:'attempt1',commit:after,which:'result',dir:'',entries:[{name:'app.ts',path:'app.ts',kind:'file',size:20}]}});
 if(p==="/console/attempts/attempt1/file")return route.fulfill({json:{attempt:'attempt1',commit:after,path:'app.ts',text:'first\nsecond\nthird\n',size:20}});
 if(p==="/console/attempts/attempt1/diff")return route.fulfill({json:{path:'app.ts',diff:'--- a/app.ts\n+++ b/app.ts\n@@ -1,2 +1,3 @@\n-old\n+first\n+second\n third\n'}});
 f.errors.push(req.method()+" "+p);return route.fulfill({status:500,json:{error:'Unmocked API'}});
});
await page.addInitScript((conversation)=>{sessionStorage.setItem('steve.conversation',conversation);if(!localStorage.getItem('steve.ui.locale'))localStorage.setItem('steve.ui.locale','en');window.sources=[];window.EventSource=class{addEventListener(){}constructor(){window.sources.push(this);setTimeout(()=>this.onopen?.(),0)}close(){window.sources=window.sources.filter(s=>s!==this)}};window.emit=(e)=>window.sources.forEach(s=>s.onmessage?.({data:JSON.stringify(e)}));},A);
async function waitFor(test,label){for(let i=0;i<100;i++){if(await test())return;await new Promise(r=>setTimeout(r,30));}assert.fail(label);}
async function draftAttachmentPresentation() {
 await page.evaluate(A=>{for(const kind of ['submission','materials','text'])localStorage.removeItem('steve.console.draft.'+kind+':'+A);localStorage.setItem('steve.ui.locale','en');},A);
 await page.reload();const message=page.getByRole('textbox',{name:'Message',exact:true});await message.waitFor();await message.fill('Body stays a draft');
 const uploads=f.uploads.length,posts=f.posts.length;
 await page.getByLabel('Attach files',{exact:true}).setInputFiles([{name:'draft-preview.png',mimeType:'image/png',buffer:png},{name:'draft-file.pdf',mimeType:'application/pdf',buffer:Buffer.from('%PDF test')}]);
 const list=page.getByRole('list',{name:'Attached materials',exact:true});await list.getByText('draft-file.pdf',{exact:true}).waitFor();
 const image=list.getByRole('img',{name:'draft-preview.png',exact:true});await image.waitFor();await image.evaluate(image=>image.decode());
 assert.equal(await image.evaluate(image=>image.complete&&image.naturalWidth===1),true,'draft renders the decoded image rather than just a filename');
 const file=list.getByRole('button',{name:'Open draft-file.pdf',exact:true});await file.waitFor();assert.match(await file.innerText(),/Attachment/);assert.match(await file.innerText(),/9 B/);
 assert.equal(await draftOf(message),'Body stays a draft');assert.equal(f.posts.length,posts);assert.equal(f.uploads.length,uploads+2);
 await file.press('Enter');const dialog=page.getByRole('dialog',{name:'Preview',exact:true});await dialog.getByRole('heading',{name:'draft-file.pdf',exact:true}).waitFor();await dialog.getByRole('button',{name:'Close',exact:true}).click();await dialog.waitFor({state:'hidden'});
 await page.setViewportSize({width:390,height:844});await image.waitFor();assert.ok(await list.evaluate(list=>list.getBoundingClientRect().right<=window.innerWidth),'draft cards fit the narrow composer');
 await list.getByRole('button',{name:'Remove draft-preview.png',exact:true}).click();await image.waitFor({state:'hidden'});assert.equal(await draftOf(message),'Body stays a draft');assert.equal(f.posts.length,posts);
 await list.getByRole('button',{name:'Remove draft-file.pdf',exact:true}).click();await list.waitFor({state:'hidden'});
 await page.setViewportSize({width:1600,height:1000});f.materials=new Map(initialMaterials);f.uploads=[];
 console.log('PASS uploaded draft shows decoded image and typed/size file card, keyboard preview, narrow layout and independent removal without submission');
}

async function fileDropRegressions() {
 // DOM DragEvents cover the browser input contract. Native Finder gestures
 // and real backend byte receipts are separate acceptance evidence.
 f.uploadFailure=null;f.contextMissing=false;f.supportsMaterials=true;f.queue=[];f.hideQueue=true;
 await page.evaluate(A=>{for(const kind of ['submission','materials','text'])localStorage.removeItem('steve.console.draft.'+kind+':'+A);},A);
 await page.setViewportSize({width:1600,height:1000});await page.reload();
 const message=page.getByRole('textbox',{name:'Message',exact:true});await message.waitFor();
 await message.fill('Keep this body unchanged');
 await page.getByLabel('Attach files',{exact:true}).setInputFiles({name:'drop-existing.txt',mimeType:'text/plain',buffer:Buffer.from('existing attachment')});
 await page.getByRole('list',{name:'Attached materials'}).getByText('drop-existing.txt',{exact:true}).waitFor();
 const saved=()=>page.evaluate(A=>({text:localStorage.getItem('steve.console.draft.text:'+A),refs:JSON.parse(localStorage.getItem('steve.console.draft.materials:'+A)||'[]')}),A);
 const baseline=await saved(),posts=f.posts.length;
 const drop=async (selector,files,text='FILE_FALLBACK_MUST_NOT_ENTER_BODY')=>page.evaluate(({selector,files,text})=>{
  const target=document.querySelector(selector),data=new DataTransfer();
  for(const file of files)data.items.add(new File([new Uint8Array(file.bytes)],file.name,{type:file.mime}));
  data.setData('text/plain',text);data.setData('text/uri-list','file:///fixture/drop.txt');
  const r=target.getBoundingClientRect(),init={dataTransfer:data,bubbles:true,cancelable:true,clientX:r.x+Math.min(12,r.width/2),clientY:r.y+Math.min(12,r.height/2)};
  const over=new DragEvent('dragover',init);target.dispatchEvent(over);
  const event=new DragEvent('drop',init);target.dispatchEvent(event);
  return {over:over.defaultPrevented,drop:event.defaultPrevented};
 },{selector,files,text});
 const file=(name,text,mime='text/plain')=>({name,mime,bytes:[...Buffer.from(text)]});
 const before=f.uploads.length;
 const handled=await drop('[aria-label="Message"]',[file('dropped.txt','ACTUAL_DROPPED_FILE_BYTES 中文')]);
 await waitFor(()=>f.uploads.length===before+1,'file drop must upload instead of becoming draft text');
 await page.getByRole('list',{name:'Attached materials'}).getByText('dropped.txt',{exact:true}).waitFor();
 assert.deepEqual(handled,{over:true,drop:true});assert.equal(await draftOf(message),baseline.text);
 assert.equal(f.uploads.at(-1).data.toString(),'ACTUAL_DROPPED_FILE_BYTES 中文');
 assert.equal(f.uploads.length,before+1);assert.equal(f.uploads.at(-1).name,'dropped.txt');assert.deepEqual((await saved()).refs.slice(0,baseline.refs.length),baseline.refs);assert.equal(f.posts.length,posts);
 // The controls and padding accept files too; drop retains real names.
 await drop('.composer-controls',[{name:'image.png',mime:'image/png',bytes:[...png]}]);
 await page.getByRole('list',{name:'Attached materials'}).getByText('image.png',{exact:true}).waitFor();
 assert.equal(f.uploads.at(-1).name,'image.png');assert.equal(await draftOf(message),baseline.text);
 const batchBefore=f.uploads.length;
 await drop('[aria-label="Message"]',[file('drop-batch-a.txt','batch a'),file('drop-batch-b.txt','batch b')]);
 await page.getByRole('list',{name:'Attached materials'}).getByText('drop-batch-b.txt',{exact:true}).waitFor();
 assert.equal(f.uploads.length,batchBefore+2);assert.deepEqual(f.uploads.slice(batchBefore).map(x=>x.name),['drop-batch-a.txt','drop-batch-b.txt']);assert.equal(await draftOf(message),baseline.text);
 const repeatBefore=f.uploads.length;
 await page.evaluate(()=>{
  const box=document.querySelector('[aria-label="Message"]');
  for(let i=0;i<2;i++){const data=new DataTransfer();data.items.add(new File(['repeat bytes'],'drop-repeat.txt',{type:'text/plain'}));box.dispatchEvent(new DragEvent('drop',{dataTransfer:data,bubbles:true,cancelable:true}));}
 });
 await page.getByRole('list',{name:'Attached materials'}).getByText('drop-repeat.txt',{exact:true}).waitFor();
 await page.waitForTimeout(100);assert.equal(f.uploads.length,repeatBefore+1,'same-tick repeated drops cannot start overlapping uploads');assert.equal(await draftOf(message),baseline.text);
 let releaseUpload;f.uploadHold={name:'drop-held.txt',promise:new Promise(resolve=>{releaseUpload=resolve;})};
 const heldBefore=f.uploads.length;
 await drop('[aria-label="Message"]',[file('drop-held.txt','held first bytes')]);
 await waitFor(()=>f.uploads.length===heldBefore+1,'first slow drop is in flight');
 await drop('.composer-controls',[file('drop-second.txt','second distinct bytes')]);
 await page.getByText('An upload is already in progress. The new files were not accepted; select or drop them again after it finishes.',{exact:true}).waitFor();
 assert.equal(f.uploads.length,heldBefore+1);assert.equal(await draftOf(message),baseline.text);
 releaseUpload();await page.getByRole('list',{name:'Attached materials'}).getByText('drop-held.txt',{exact:true}).waitFor();f.uploadHold=null;
 await drop('.composer-controls',[file('drop-second.txt','second distinct bytes')]);
 await page.getByRole('list',{name:'Attached materials'}).getByText('drop-second.txt',{exact:true}).waitFor();
 assert.deepEqual(f.uploads.slice(heldBefore).map(x=>x.name),['drop-held.txt','drop-second.txt']);assert.equal(f.posts.length,posts);
 const preserved=await saved();f.uploadFailure={name:'drop-refused.pdf',status:403,body:JSON.stringify({error:'fixture denied'})};
 await drop('[aria-label="Message"]',[file('drop-refused.pdf','refused bytes','application/pdf')]);
 await page.locator('.composer-dock [role="alert"]').filter({hasText:'drop-refused.pdf'}).waitFor();
 assert.deepEqual(await saved(),preserved);assert.equal(f.posts.length,posts);
 f.uploadFailure=null;f.supportsMaterials=false;
 await page.evaluate(async()=>{await (await import('/src/lib/api/console.ts')).checkSubmissionSupport();});
 const unsupportedUploads=f.uploads.length;
 await drop('[aria-label="Message"]',[file('unsupported-drop.txt','MUST_NOT_INSERT_UNSUPPORTED')]);
 await page.getByText('This Hub does not support material references; dropped files were not attached.',{exact:true}).waitFor();
 assert.equal(f.uploads.length,unsupportedUploads);assert.deepEqual(await saved(),preserved);
 f.supportsMaterials=true;f.contextMissing=true;await page.reload();await message.waitFor();
 await waitFor(async()=>await message.getAttribute('aria-disabled')==='true','no context disables composer');
 const disabledUploads=f.uploads.length;
 const disabledEditor=await drop('[aria-label="Message"]',[file('disabled-drop.txt','MUST_NOT_INSERT_DISABLED')]);
 const disabledRoot=await drop('.composer-controls',[file('disabled-root.txt','MUST_NOT_INSERT_ROOT')]);
 assert.deepEqual(disabledEditor,{over:true,drop:true});assert.deepEqual(disabledRoot,{over:true,drop:true});
 await page.waitForTimeout(100);assert.equal(f.uploads.length,disabledUploads);assert.deepEqual(await saved(),preserved);assert.equal(f.posts.length,posts);
 f.contextMissing=false;await page.reload();await message.waitFor();
 await waitFor(async()=>await message.getAttribute("aria-disabled")!=="true","restored context enables composer before the unavailable-file case");
 // A file-bearing transfer can lose file access while still carrying a text
 // fallback. It must neither insert that fallback nor invent an attachment.
 await page.evaluate(()=>{
  const box=document.querySelector('[aria-label="Message"]'),data=new DataTransfer();
  data.items.add(new File(['unavailable'],'unavailable-drop.txt',{type:'text/plain'}));
  data.setData('text/plain','INACCESSIBLE_FILE_TEXT_MUST_NOT_ENTER_BODY');
  const empty=new DataTransfer();Object.defineProperty(data,'files',{get:()=>empty.files});
  box.dispatchEvent(new DragEvent('drop',{dataTransfer:data,bubbles:true,cancelable:true}));
 });
 await page.getByText('The dropped files could not be read. Use Attach files to select them. Your text and existing attachments are unchanged.',{exact:true}).waitFor();
 assert.equal(f.uploads.length,disabledUploads);assert.deepEqual(await saved(),preserved);assert.equal(f.posts.length,posts);
 // Non-file text keeps CodeMirror's existing drop behavior.
 await drop('[aria-label="Message"]',[],'PLAIN_TEXT_DROP');
 await waitFor(async()=> (await draftOf(message)).includes('PLAIN_TEXT_DROP'),'plain text drag must still insert text');
 assert.equal(f.uploads.length,disabledUploads);assert.equal(f.posts.length,posts);
 console.log('PASS file drop uploads exact bytes once without changing body/old refs; dock target, errors, unsupported/disabled and text fallback');
 f.materials=new Map(initialMaterials);f.uploads=[];f.uploadFailure=null;f.contextMissing=false;
}

async function uploadErrorRegressions() {
 // These are observed backend/browser failures, driven through the actual Console
 // and material API. All writes are intercepted, including capability checks.
 f.uploadFailure=null;f.questions=[];f.queue=[];f.hideQueue=true;
 await page.evaluate(A=>{for(const kind of ['submission','materials','text'])localStorage.removeItem('steve.console.draft.'+kind+':'+A);localStorage.setItem('steve.ui.locale','en');},A);
 await page.setViewportSize({width:1600,height:1000});await page.reload();await page.getByLabel('Attach files',{exact:true}).waitFor();
 const faults=[];
 const check=async (label,work)=>{try{await work();console.log('PASS '+label);}catch(error){faults.push(new Error(label,{cause:error}));console.error('FAIL '+label+' '+error.stack);}};
 const secret='fixture-private-token',privatePath='/Users/fixture-private/documents/secret.png';
 const message='Keep my body\n**unchanged**';
 const seed={id:note.id,title:note.title,project:'p',kind:note.kind,mime:note.mime,size:note.size};
 const pending=await page.evaluate(async ({A,seed})=>{
  const store=await import('/src/lib/drafts.ts');
  await store.addDraftMaterial(A,seed);
  await store.updateDraft(A,'Pending original message');
  const pending=await store.beginSubmission(A,'Pending original message',[],true,'en');
  await store.failSubmission(A,pending.id,'Fixture unconfirmed submission','unknown');
  await store.addDraftMaterial(A,{...seed,id:'m-seed-2',title:'Trace log'});
  await store.updateDraft(A,'Keep my body\n**unchanged**');
  return localStorage.getItem('steve.console.draft.submission:'+A);
 },{A,seed});
 const saved=()=>page.evaluate(A=>({text:localStorage.getItem('steve.console.draft.text:'+A),refs:JSON.parse(localStorage.getItem('steve.console.draft.materials:'+A)||'[]'),pending:localStorage.getItem('steve.console.draft.submission:'+A)}),A);
 const initial=await saved(),postCount=f.posts.length;
 const cases=[
  {name:'broken.png',mime:'image/png',status:400,body:JSON.stringify({error:'invalid material: unsupported or mismatched image'}),reason:/invalid or unsupported/i},
  {name:'oversized.pdf',status:413,body:JSON.stringify({error:'material too large'}),reason:/smaller file/i},
  {name:'sign-in.pdf',status:401,body:'Unauthorized',contentType:'text/plain',reason:/sign-in.*permissions/i},
  {name:'forbidden.pdf',status:403,body:JSON.stringify({error:privatePath+' '+secret}),reason:/sign-in.*permissions/i},
  {name:'service.pdf',status:503,body:'<html>'+privatePath+' '+secret+'</html>',contentType:'text/html',reason:/try again later/i},
  {name:'offline.pdf',abort:true,reason:/read.*file.*connection/i},
  {name:'unreadable.pdf',local:true,reason:/read.*file.*connection/i},
  {name:'invalid-ack.pdf',status:200,body:'not JSON '+privatePath+' '+secret,contentType:'text/plain',reason:/receipt.*invalid.*cannot be confirmed/i},
  ...[
   ['null-ack.pdf',null],['empty-ack.pdf',{}],['missing-id.pdf',{project:'p',title:'fixture',kind:'binary',mime:'application/pdf',size:12}],
   ['wrong-project.pdf',{...note,id:'foreign',project:'other'}],['bad-kind.pdf',{...note,kind:'surprise'}],
   ['bad-size.pdf',{...note,size:-1}],['bad-title.pdf',{...note,title:privatePath,mime:null}],
   ['wrong-name.pdf',{...note,title:privatePath}],['bad-digest.pdf',{...note,digest:'invalid'}],['bad-date.pdf',{...note,created_at:'invalid'}],
   ['bad-source.pdf',{...note,source:{kind:'reply'}}],['bad-image.pdf',{...note,kind:'image',mime:'image/png',width:0,height:1}],
  ].map(([name,body])=>({name,status:200,body:JSON.stringify(body),reason:/receipt.*invalid.*cannot be confirmed/i})),
 ];
 await page.evaluate(({privatePath,secret})=>{
  const fetch=window.fetch;
  window.uploadReadFailures=0;
  window.fetch=function(url,init){
   if(String(url).includes('/materials/upload')&&init?.body?.name==='unreadable.pdf'){
    window.uploadReadFailures++;
    return Promise.reject(new TypeError('Load failed '+privatePath+' Bearer '+secret));
   }
   return fetch.call(this,url,init);
  };
 },{privatePath,secret});
 const input=page.getByLabel('Attach files',{exact:true});
 for(const failure of cases) {
  f.uploadFailure=failure;
  const before=f.uploads.length,localBefore=await page.evaluate(()=>window.uploadReadFailures);
  await input.setInputFiles({name:failure.name,mimeType:failure.mime||'application/pdf',buffer:Buffer.from('not a PNG')});
  await waitFor(async()=>f.uploads.length>before||await page.evaluate(()=>window.uploadReadFailures)>localBefore,'upload attempted');
  await waitFor(()=>input.isEnabled(),'upload busy flag released');
  await check('upload UI '+failure.name,async()=>{
   const alert=page.locator('.composer-dock [role="alert"]').filter({hasText:failure.name});
   assert.equal(await alert.count(),1,'file-specific failure is visible beside the composer, not raw toolbar text');
   const text=await alert.innerText();
   if(failure.status!==undefined)assert.match(text,new RegExp('HTTP '+failure.status));
   assert.match(text,failure.reason);assert.match(text,/select.*again|try again|check.*materials/i);assert.match(text,/message.*attached materials.*retained/i);
   assert.doesNotMatch(text,/\{"error"|invalid material|Load failed|fixture-private|secret.png|Bearer|Hub.*(offline|unreachable|disconnect)/i);
   if(failure.status===200)assert.doesNotMatch(text,/file was uploaded|read the local file|connection works/i);
   assert.deepEqual(await saved(),initial,'every upload failure keeps text, previous draft refs and pending submission');
   assert.equal(f.posts.length,postCount,'no submission/replay from upload failure');
   assert.equal(f.uploads.length,before+(failure.local?0:1),'no transparent upload retry');
   assert.equal(await input.inputValue(),'','same file can be selected again');
   if(failure.name==='broken.png'&&process.env.MATERIAL_UPLOAD_ARTIFACTS){await mkdir(process.env.MATERIAL_UPLOAD_ARTIFACTS,{recursive:true});await page.screenshot({path:path.join(process.env.MATERIAL_UPLOAD_ARTIFACTS,'upload-error-en-desktop.png')});}
  });
 }
 // Initial support is true. Only the upload's mandatory capability recheck
 // fails: the File POST must never happen, and no other visible status surface
 // may echo the private response or confuse preflight with reading a file.
 for(const failure of [
  {status:401,body:JSON.stringify({error:privatePath+' Bearer '+secret}),reason:/sign-in.*permissions/i},
  {status:403,body:privatePath+' Bearer '+secret,contentType:'text/plain',reason:/sign-in.*permissions/i},
  {status:500,body:'<html>'+privatePath+' Bearer '+secret+'</html>',contentType:'text/html',reason:/attachment support.*not.*confirmed/i},
  {abort:true,reason:/attachment support.*not.*confirmed/i},
 ]) {
  f.uploadFailure=null;f.capabilityFailure=null;
  await page.evaluate(async()=>{await (await import('/src/lib/api/console.ts')).checkSubmissionSupport();});
  const file='preflight-'+(failure.status||'no-response')+'.pdf',before=f.capabilities.length,uploads=f.uploads.length;
  f.capabilityFailure=failure;
  await input.setInputFiles({name:file,mimeType:'application/pdf',buffer:Buffer.from('do not upload')});
  await waitFor(()=>f.capabilities.length>before,'upload capability recheck attempted');await waitFor(()=>input.isEnabled(),'preflight failure settles');
  await check('preflight UI preserves stage/status and all visible privacy '+(failure.status||'no-response'),async()=>{
   const alert=page.locator('.composer-dock [role="alert"]').filter({hasText:file});const text=await alert.innerText();
   const visible=await page.locator('.composer-dock').innerText()+' '+await page.locator('.console-status').innerText()+' '+await page.locator('.console-status').getAttribute('title');
   assert.doesNotMatch(visible,/fixture-private|secret.png|Bearer|\{"error"|<html>/i);
   assert.match(text,failure.reason);assert.doesNotMatch(text,/read the local file|connection works|file was uploaded/i);
   if(failure.status)assert.match(text,new RegExp('HTTP '+failure.status));
   assert.deepEqual(await saved(),initial);assert.equal(f.uploads.length,uploads);assert.equal(f.posts.length,postCount);
   assert.equal(f.capabilities.length,before+1,'no transparent preflight retry');
  });
  const result=await page.evaluate(async()=>{
   const {uploadMaterial}=await import('/src/lib/api/material.ts'),{HTTPError}=await import('/src/lib/http.ts'),api=await import('/src/lib/api/console.ts');
   try{await uploadMaterial('p',new File(['no upload'],'preflight-api.pdf'),'en');return {ok:true};}
   catch(error){const support=api.getSubmissionSupport();return {http:error instanceof HTTPError,status:error.status,phase:error.phase,supportError:support.error,supportHTTP:support.failure instanceof HTTPError,supportStatus:support.failure?.status,supportMessage:support.failure?.message};}
  });
  await check('preflight API keeps HTTPError and capability phase '+(failure.status||'no-response'),async()=>{
   assert.doesNotMatch(result.supportError+' '+result.supportMessage,/fixture-private|secret.png|Bearer|\{"error"|<html>/i);
   assert.equal(result.phase,'capability');assert.equal(result.http,!!failure.status);assert.equal(result.status,failure.status);assert.equal(result.supportHTTP,!!failure.status);assert.equal(result.supportStatus,failure.status);
   assert.doesNotMatch(result.supportError,/fixture-private|secret.png|Bearer|\{"error"|<html>/i);assert.equal(f.uploads.length,uploads);
  });
 }
 // The shared queue read error can also reach the toolbar. Probe that real
 // event-driven path, not only the new attachment warning surface.
 f.capabilityFailure={queue:true,status:403,body:JSON.stringify({error:privatePath+' Bearer '+secret})};
 const queueReads=f.queueReads;
 await page.evaluate(e=>window.emit(e),{kind:'console.queue',conversation:A,at});
 await waitFor(()=>f.queueReads>queueReads,'queue error read after capability failure');
 await check('shared queue/support status never renders raw failure or title',async()=>{
  await page.locator('.console-status').filter({hasText:/HTTP 403/}).waitFor();
  const visible=await page.locator('.composer-dock').innerText()+' '+await page.locator('.console-status').innerText()+' '+await page.locator('.console-status').getAttribute('title');
  assert.doesNotMatch(visible,/fixture-private|secret.png|Bearer|\{"error"|<html>/i);assert.deepEqual(await saved(),initial);
 });
 f.capabilityFailure=null;await page.evaluate(e=>window.emit(e),{kind:'console.queue',conversation:A,at});
 await page.evaluate(async()=>{await (await import('/src/lib/api/console.ts')).checkSubmissionSupport();});
 for(const body of ['not JSON '+privatePath+' Bearer '+secret,'null','{}',JSON.stringify({...note,id:'foreign',project:'other'}),JSON.stringify({...note,kind:'wrong'})]) {
  f.uploadFailure={name:'receipt-api.pdf',status:200,body,contentType:'application/json'};
  const result=await page.evaluate(async()=>{
   const {uploadMaterial}=await import('/src/lib/api/material.ts'),{HTTPError}=await import('/src/lib/http.ts');
   try{const material=await uploadMaterial('p',new File(['uploaded bytes'],'receipt-api.pdf',{type:'application/pdf'}),'en');return {ok:true,material};}
   catch(error){return {http:error instanceof HTTPError,status:error.status,phase:error.phase,message:error.message};}
  });
  await check('receipt API rejects invalid acknowledgement '+JSON.stringify(body),async()=>{
   assert.equal(result.http,true);assert.equal(result.status,200);assert.equal(result.phase,'receipt');
   assert.doesNotMatch(result.message,/fixture-private|secret.png|Bearer|<html>|not JSON/i);
  });
 }
 // File.name is normally a basename, but injected/clipboard File values
 // must not expose directory components or control characters either.
 f.uploadFailure={name:'C:\\fixture-private\\documents\\bad-path.png',status:400,body:JSON.stringify({error:privatePath+' Bearer '+secret})};
 const beforePath=f.uploads.length;
 await page.evaluate(name=>{const file=new File(['bad PNG'],name,{type:'image/png'}),data=new DataTransfer();data.items.add(file);const input=document.querySelector('input[type="file"]');input.files=data.files;input.dispatchEvent(new Event('change',{bubbles:true}));},f.uploadFailure.name);
 await waitFor(()=>f.uploads.length>beforePath,'path-named File attempted');await waitFor(()=>input.isEnabled(),'path-named File settles');
 await check('failure shows only the file basename, never paths or credentials',async()=>{
  const alert=page.locator('.composer-dock [role="alert"]').filter({hasText:'bad-path.png'});assert.equal(await alert.count(),1);assert.doesNotMatch(await alert.innerText(),/fixture-private|documents|Bearer|secret.png/);assert.deepEqual(await saved(),initial);
 });
 // One earlier file succeeds, the next is rejected, and the later one is not
 // attempted. Both the pre-existing refs and this success must remain attached.
 f.uploadFailure=cases[0];
 const beforeBatch=f.uploads.length;
 await input.setInputFiles([{name:'batch-kept.pdf',mimeType:'application/pdf',buffer:Buffer.from('pdf bytes')},{name:'broken.png',mimeType:'image/png',buffer:Buffer.from('not a PNG')},{name:'not-attempted.pdf',mimeType:'application/pdf',buffer:Buffer.from('later')}]);
 await waitFor(()=>f.uploads.length>=beforeBatch+2,'batch reaches second file');await waitFor(()=>input.isEnabled(),'batch settles');
 await check('partial upload retains successful refs and names the failed file',async()=>{
  const now=await saved();assert.equal(now.text,message);assert.equal(now.pending,pending);
  assert.deepEqual(now.refs.slice(0,initial.refs.length),initial.refs);assert.equal(now.refs.at(-1).title,'batch-kept.pdf');
  assert.deepEqual(f.uploads.slice(beforeBatch).map(u=>u.name),['batch-kept.pdf','broken.png']);
  const alert=page.locator('.composer-dock [role="alert"]').filter({hasText:'broken.png'});
  assert.match(await alert.innerText(),/Files left unuploaded: 1/i);
 });
 // A successful upload followed by a failed local draft write is not a
 // network failure. Its error must not claim the new ref was attached.
 f.uploadFailure=null;
 const beforeAttach=await saved(),beforeAttachUploads=f.uploads.length;
 await page.evaluate(A=>{const set=Storage.prototype.setItem;window.restoreDraftStorage=()=>{Storage.prototype.setItem=set;};Storage.prototype.setItem=function(key,value){if(key==='steve.console.draft.materials:'+A)throw new Error('/Users/fixture-private/local-storage Bearer fixture-private-token');return set.call(this,key,value);};},A);
 await input.setInputFiles({name:'draft-write.pdf',mimeType:'application/pdf',buffer:Buffer.from('uploaded bytes')});
 await waitFor(()=>f.uploads.length>beforeAttachUploads,'file uploaded before draft failure');await waitFor(()=>input.isEnabled(),'draft failure settles');
 await page.evaluate(()=>window.restoreDraftStorage());
 await check('uploaded file draft failure retains all existing text/refs',async()=>{
  const alert=page.locator('.composer-dock [role="alert"]').filter({hasText:'draft-write.pdf'});assert.match(await alert.innerText(),/uploaded.*could not be added.*draft/i);
  assert.deepEqual(await saved(),beforeAttach);assert.equal(f.uploads.length,beforeAttachUploads+1);
 });
 // Retry is explicit reselection. The same name succeeds without sending a
 // message, duplicating a pending turn, or deleting earlier materials.
 await input.setInputFiles({name:'broken.png',mimeType:'image/png',buffer:png});
 await page.getByRole('list',{name:'Attached materials'}).getByText('broken.png',{exact:true}).waitFor();
 await check('explicit reselection succeeds without replay or draft loss',async()=>{
  const now=await saved();assert.equal(now.text,message);assert.equal(now.pending,pending);assert.deepEqual(now.refs.slice(0,beforeAttach.refs.length),beforeAttach.refs);
  assert.equal(await page.locator('.composer-dock [role="alert"]').filter({hasText:'Could not attach'}).count(),0);
  assert.equal(f.posts.length,postCount);
 });
 // Exercise the real API with a File body: no JSON conversion/copy, correct
 // locale and shared HTTPError identity/status/error-body contract.
 for(const response of [
  {status:400,body:'{"error":"invalid material: unsupported or mismatched image"}',message:'invalid material: unsupported or mismatched image'},
  {status:403,body:'Permission denied',message:'Permission denied',contentType:'text/plain'},
  {status:503,body:'',message:'503 Service Unavailable'},
  {status:400,body:'null',message:'null'},
  {status:400,body:'{"error":17}',message:'{"error":17}'},
 ]) {
  f.uploadFailure={name:'api-boundary.png',...response};
  const before=f.uploads.length;
  const result=await page.evaluate(async()=>{
   const {uploadMaterial}=await import('/src/lib/api/material.ts');const {HTTPError}=await import('/src/lib/http.ts');
   try{await uploadMaterial('p',new File(['raw file bytes'],'api-boundary.png',{type:'image/png'}),'zh');return {success:true};}
   catch(error){return {http:error instanceof HTTPError,status:error.status,message:error.message};}
  });
  await check('upload API response '+response.status+' '+JSON.stringify(response.body),async()=>{
   assert.deepEqual(result,{http:true,status:response.status,message:response.message});assert.equal(f.uploads.length,before+1);
   const sent=f.uploads.at(-1);assert.equal(sent.mime,'image/png');assert.equal(sent.locale,'zh');assert.equal(sent.data.toString(),'raw file bytes');
  });
 }
 await check('HTTP status survives an unreadable error response body',async()=>{
  const result=await page.evaluate(async()=>{
   const {uploadMaterial}=await import('/src/lib/api/material.ts'),{HTTPError}=await import('/src/lib/http.ts');
   const fetch=window.fetch;window.fetch=(url,init)=>String(url).includes('/materials/upload')?Promise.resolve({ok:false,status:413,statusText:'Payload Too Large',text:async()=>{throw new TypeError('Load failed /Users/fixture-private');}}):fetch.call(window,url,init);
   try{await uploadMaterial('p',new File(['file bytes'],'response-read.pdf'),'en');return {success:true};}
   catch(error){return {http:error instanceof HTTPError,status:error.status,message:error.message};}
   finally{window.fetch=fetch;}
  });
  assert.deepEqual(result,{http:true,status:413,message:'413 Payload Too Large'});
 });
 f.uploadFailure={name:'中文坏图.png',status:400,body:'{"error":"invalid material: unsupported or mismatched image"}'};
 await page.evaluate(()=>localStorage.setItem('steve.ui.locale','zh'));await page.reload();
 const zhInput=page.getByLabel('添加附件',{exact:true});await zhInput.waitFor();
 await page.setViewportSize({width:390,height:844});
 const beforeZh=f.uploads.length;
 await zhInput.setInputFiles({name:'中文坏图.png',mimeType:'image/png',buffer:Buffer.from('bad image')});await waitFor(()=>f.uploads.length>beforeZh,'Chinese upload');await waitFor(()=>zhInput.isEnabled(),'Chinese upload settles');
 await check('Chinese narrow-screen failure is localized and readable',async()=>{
  const alert=page.locator('.composer-dock [role="alert"]').filter({hasText:'中文坏图.png'});const text=await alert.innerText();assert.match(text,/无效|不支持/);assert.match(text,/重新选择/);assert.match(text,/正文.*附件.*保留/);assert.doesNotMatch(text,/invalid material|\{"error"/);
  assert.ok(await alert.evaluate(el=>el.scrollWidth<=el.clientWidth+1),'failure wraps rather than overflows');assert.equal((await saved()).text,message);assert.equal((await saved()).pending,pending);
 });
 if(process.env.MATERIAL_UPLOAD_ARTIFACTS){await mkdir(process.env.MATERIAL_UPLOAD_ARTIFACTS,{recursive:true});await page.screenshot({path:path.join(process.env.MATERIAL_UPLOAD_ARTIFACTS,'upload-error-zh-narrow.png')});}
 await page.evaluate(()=>{localStorage.setItem('steve.ui.locale','en');window.dispatchEvent(new StorageEvent('storage',{key:'steve.ui.locale',newValue:'en',storageArea:localStorage}));});await page.getByLabel('Attach files',{exact:true}).waitFor();
 await check('existing upload failure translates without rereading or retrying',async()=>{
  const alert=page.locator('.composer-dock [role="alert"]').filter({hasText:'中文坏图.png'});assert.match(await alert.innerText(),/invalid or unsupported/i);assert.equal(f.uploads.length,beforeZh+1);assert.equal((await saved()).pending,pending);
 });

 await check('keyboard focus remains available after a failed upload',async()=>{
  const input=page.getByLabel('Attach files',{exact:true}),box=page.getByRole('textbox',{name:'Message',exact:true});
  await input.focus();assert.ok(await input.evaluate(el=>document.activeElement===el));
  await box.focus();await box.press('ArrowRight');assert.ok(await box.evaluate(el=>document.activeElement===el));
  assert.equal(await draftOf(box),message);assert.equal(f.posts.length,postCount);
 });
 assert.equal(f.posts.length,postCount);
 // Choosing to send the retained draft is a separate explicit action. The
 // old upload warning must not outlive that draft or carry into another chat.
 await page.evaluate(async A=>{const store=await import('/src/lib/drafts.ts');await store.finishSubmission(A);},A);
 f.hideQueue=false;f.uploadFailure=null;
 const sending=await saved();
 await page.getByRole('textbox',{name:'Message',exact:true}).press('Enter');
 await waitFor(()=>f.posts.length===postCount+1,'explicit retained draft submission');
 await check('explicit submission clears obsolete attachment feedback',async()=>{
  assert.equal(f.posts.at(-1).input,message);assert.deepEqual(f.posts.at(-1).refs,sending.refs.map(({id,selector})=>({id,...(selector?{selector}:{})})));
  assert.equal(await page.locator('.composer-dock [role="alert"]').filter({hasText:'中文坏图.png'}).count(),0);
 });
 if(faults.length)throw new AggregateError(faults,'Material upload regressions');
}
try{
 await page.goto(url+'#/console');await page.getByRole('heading',{name:'Material conversation',exact:true}).waitFor();await page.getByRole('button',{name:'Actions',exact:true}).waitFor();
 await draftAttachmentPresentation();
 await fileDropRegressions();
 await uploadErrorRegressions();
 if(!process.env.MATERIAL_UPLOAD_ERRORS_ONLY){
  f.materials=new Map(initialMaterials);f.uploadFailure=null;f.capabilityFailure=null;f.capabilities=[];f.uploads=[];f.posts=[];f.queue=[];f.questions=[];f.hideQueue=false;
  await page.evaluate(()=>{localStorage.clear();localStorage.setItem('steve.ui.locale','en');});await page.setViewportSize({width:1600,height:1000});await page.reload();await page.getByRole('heading',{name:'Material conversation',exact:true}).waitFor();await page.getByRole('button',{name:'Actions',exact:true}).waitFor();
 // The sent line shows what it carried, each kind in its own shape, and
 // says so rather than going quiet when a reference cannot be resolved.
 const sent=page.locator('.message-user').first();
 await sent.getByRole('img',{name:'screenshot.png',exact:true}).waitFor();
 await sent.getByRole('button',{name:'Open report.pdf',exact:true}).waitFor();
 await sent.getByText('Design note · L1–L2',{exact:true}).waitFor();
 await sent.getByText('Material unavailable',{exact:true}).waitFor();
 assert.ok((await sent.innerText()).includes('second'),'A cut of a file brings its lines into the transcript');
 await sent.getByRole('button',{name:'Open screenshot.png',exact:true}).click();const carriedDialog=page.getByRole('dialog',{name:'Preview',exact:true});await carriedDialog.getByRole('img').waitFor();await carriedDialog.getByRole('button',{name:'Close',exact:true}).click();await carriedDialog.waitFor({state:'hidden'});
 console.log('PASS a sent line draws its image, names its file, quotes its cut and admits a lost reference');
 await page.getByRole('button',{name:'Actions',exact:true}).click();await page.getByRole('menuitem',{name:'Add to Material conversation',exact:true}).click();await page.getByRole('list',{name:'Attached materials'}).getByText('Reference reply').waitFor();assert.equal(f.posts.length,0);
 await page.getByRole('button',{name:'Actions',exact:true}).click();await page.getByRole('menuitem',{name:'Pin in materials',exact:true}).click();const shelfTab=page.getByRole('tab',{name:/^Materials/});await shelfTab.waitFor();assert.equal(f.posts.length,0);
 // The shelf is the project's materials, not the local pin list: the two
 // nobody pinned are listed, and the tab carries the count.
 await shelfTab.click();const shelf=page.getByRole('complementary',{name:'Details'});await shelf.getByRole('button',{name:'Design note',exact:true}).waitFor();await shelf.getByRole('button',{name:'Trace log',exact:true}).waitFor();await shelf.getByRole('button',{name:'Reference reply',exact:true}).first().waitFor();await shelfTab.getByText('6',{exact:true}).waitFor();console.log('PASS materials shelf lists the project, not just local pins');
 await page.getByRole('button',{name:'Actions',exact:true}).click();await page.getByRole('menuitem',{name:'Annotate',exact:true}).click();const dialog=page.getByRole('dialog',{name:'Annotate material'});await dialog.getByRole('textbox',{name:'Annotation',exact:true}).fill('Verify the result');await dialog.getByRole('button',{name:'Save',exact:true}).click();await waitFor(()=>f.notes.length===1,'annotation persisted');await dialog.waitFor({state:'hidden'});await page.getByText('Verify the result',{exact:true}).waitFor();assert.equal(f.notes.length,1);assert.equal(f.posts.length,0);console.log('PASS reply capture, explicit draft target, local pin and durable annotation');
 // Source and deleted-side line ranges use different immutable commits.
 await page.getByRole('tab',{name:'Artifacts',exact:true}).click();await page.getByRole('button',{name:/^(浏览文件|Browse files)$/}).click();await page.getByRole('button',{name:'app.ts',exact:true}).first().click();
 const source=page.getByRole('region',{name:'app.ts file content'}),sourceActions=page.getByRole('region',{name:'Source app.ts',exact:true}).getByRole('button',{name:'Actions',exact:true}),selectionBar=page.getByRole('toolbar',{name:'Selection actions',exact:true});
 for(const viewport of [{width:1600,height:1000},{width:780,height:540},{width:390,height:844},{width:1600,height:1000}]){
  await page.setViewportSize(viewport);
  await source.getByRole('button',{name:'Select line 2',exact:true}).click();await source.getByRole('button',{name:'Select line 3',exact:true}).click({modifiers:['Shift']});
  await selectionBar.getByText('Selected L2–L3',{exact:true}).waitFor();
  const hit=await sourceActions.evaluate(el=>{const rect=el.getBoundingClientRect(),point={x:rect.x+rect.width/2,y:rect.y+rect.height/2},target=document.elementFromPoint(point.x,point.y);return {point,button:rect.toJSON(),target:target?.outerHTML,receivesPointer:el.contains(target)};});
  console.log('Material Actions center hit-test',JSON.stringify({viewport,...hit}));
  if(process.env.MATERIAL_UPLOAD_ARTIFACTS){await mkdir(process.env.MATERIAL_UPLOAD_ARTIFACTS,{recursive:true});await page.screenshot({path:path.join(process.env.MATERIAL_UPLOAD_ARTIFACTS,'material-actions-hit-'+viewport.width+'.png')});}
  assert.equal(hit.receivesPointer,true,'L2–L3 selection toolbar must not intercept the real source Actions center');
  assert.ok(await selectionBar.evaluate(el=>{const r=el.getBoundingClientRect();return r.x>=0&&r.y>=0&&r.right<=innerWidth&&r.bottom<=innerHeight;}),'selection toolbar stays within the viewport');
  assert.ok(await selectionBar.getByRole('button').evaluateAll(buttons=>buttons.every(el=>{const r=el.getBoundingClientRect();return el.contains(document.elementFromPoint(r.x+r.width/2,r.y+r.height/2));})),'selection toolbar buttons still receive real pointer hits');
 }
 // Locale changes must preserve the live selection. A supported UI-font
 // preference supplies real size growth independent of installed font metrics.
 const dynamicBar=page.locator('.selection-toolbar'),dynamicActions=page.locator('.source-view [data-material-actions] button[aria-haspopup="true"]');
 const setLocale=async(locale)=>{await page.evaluate(locale=>{localStorage.setItem('steve.ui.locale',locale);window.dispatchEvent(new StorageEvent('storage',{key:'steve.ui.locale',newValue:locale,storageArea:localStorage}));},locale);await page.waitForFunction(locale=>document.documentElement.lang===(locale==='zh'?'zh-CN':'en'),locale);};
 const afterPaint=()=>page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
 const originalAppearance=await page.evaluate(()=>localStorage.getItem('steve.ui.appearance'));
 const setUIFont=async(size)=>{
  await page.evaluate(size=>{const key='steve.ui.appearance';const a=JSON.parse(localStorage.getItem(key)||'null')||{version:1,mode:'system',light:'light',dark:'dark',fonts:{ui:{family:'system',size:14},heading:{family:'system',scale:1},reading:{family:'system',size:16},code:{family:'mono',size:13}},custom:[]};a.fonts.ui.size=size;localStorage.setItem(key,JSON.stringify(a));window.dispatchEvent(new StorageEvent('storage',{key,newValue:JSON.stringify(a),storageArea:localStorage}));},size);
  await page.waitForFunction(size=>getComputedStyle(document.documentElement).getPropertyValue('--ui-font-size').trim()===`${size}px`,size);await afterPaint();
 };
 await setUIFont(12);

 await setLocale('zh');
 const chineseSource=page.getByRole('region',{name:'app.ts 文件内容',exact:true});
 // Continue the viewport case's live range instead of relying on a
 // preference change to dismiss it before selecting the same lines again.
 assert.match(await dynamicBar.textContent(), /L2–L3/);
 assert.deepEqual(await chineseSource.locator('.source-number[aria-pressed="true"]').evaluateAll(lines=>lines.map(el=>el.dataset.line)), ['2','3']);
 await afterPaint();
 const beforeReflow=await dynamicBar.evaluate(el=>{window.materialReflowToolbar=el;return el.getBoundingClientRect().toJSON();});
 assert.ok(await dynamicActions.evaluate(el=>{const r=el.getBoundingClientRect();return el.contains(document.elementFromPoint(r.x+r.width/2,r.y+r.height/2));}),'small localized toolbar begins with reachable Actions');
 await setLocale('en');await afterPaint();
 assert.equal(await dynamicBar.evaluate(el=>el===window.materialReflowToolbar),true,'locale change retains the same toolbar');
 assert.ok(await dynamicActions.evaluate(el=>{const r=el.getBoundingClientRect();return el.contains(document.elementFromPoint(r.x+r.width/2,r.y+r.height/2));}),'localized selection keeps Actions reachable');
 await setUIFont(18);
 const afterReflow=await dynamicBar.evaluate(el=>({rect:el.getBoundingClientRect().toJSON(),same:el===window.materialReflowToolbar,label:el.querySelector('.selection-label')?.textContent}));
 const reflowHit=await dynamicActions.evaluate(el=>{const r=el.getBoundingClientRect(),point={x:r.x+r.width/2,y:r.y+r.height/2},target=document.elementFromPoint(point.x,point.y);return {point,button:r.toJSON(),target:target?.outerHTML,receivesPointer:el.contains(target)};});
 console.log('Material toolbar reflow hit-test',JSON.stringify({before:beforeReflow,after:afterReflow,...reflowHit}));
 if(process.env.MATERIAL_UPLOAD_ARTIFACTS){await mkdir(process.env.MATERIAL_UPLOAD_ARTIFACTS,{recursive:true});await page.screenshot({path:path.join(process.env.MATERIAL_UPLOAD_ARTIFACTS,'material-toolbar-reflow-locale.png')});}
 assert.equal(afterReflow.same,true,'locale change must preserve the existing toolbar');assert.match(afterReflow.label,/L2–L3/);
 assert.ok(afterReflow.rect.height>beforeReflow.height,'supported UI font setting grows the actual toolbar without reselection');
 assert.equal(reflowHit.receivesPointer,true,'existing selection reflow must remeasure material Actions hit avoidance');
 assert.ok(await dynamicBar.getByRole('button').evaluateAll(buttons=>buttons.every(el=>{const r=el.getBoundingClientRect();return el.contains(document.elementFromPoint(r.x+r.width/2,r.y+r.height/2));})),'resized floating buttons remain clickable');
 console.log('PASS active material toolbar preserves locale and remeasures real font-size changes');
 await page.evaluate(value=>{const key='steve.ui.appearance';if(value===null)localStorage.removeItem(key);else localStorage.setItem(key,value);window.dispatchEvent(new StorageEvent('storage',{key,newValue:value,storageArea:localStorage}));},originalAppearance);await afterPaint();
 await page.keyboard.press('Shift+F10');assert.equal(await selectionBar.getByRole('button',{name:'Add to chat',exact:true}).evaluate(el=>document.activeElement===el),true);
 await page.keyboard.press('ArrowRight');assert.equal(await selectionBar.getByRole('button',{name:'More details',exact:true}).evaluate(el=>document.activeElement===el),true);
 await page.keyboard.press('Home');assert.equal(await selectionBar.getByRole('button',{name:'Add to chat',exact:true}).evaluate(el=>document.activeElement===el),true);
 // Keep the original real mouse action and resulting immutable source ref.
 assert.ok(await selectionBar.isVisible(),'keyboard navigation must not hide the toolbar');
 assert.ok(await sourceActions.evaluate(el=>{const r=el.getBoundingClientRect();return el.contains(document.elementFromPoint(r.x+r.width/2,r.y+r.height/2));}),'Actions center stays clickable while the keyboard-focused toolbar is still present');
 await sourceActions.click();await page.getByRole('menuitem',{name:'Add to Material conversation',exact:true}).click();
 await waitFor(()=>f.captures.at(-1)?.source.commit===after,'source snapshot capture');assert.equal(f.captures.at(-1).source.commit,after);
 // The floating toolbar is still an operable control, not a pointer-events
 // workaround. Its normal preview action works with keyboard and mouse.
 for(const keyboard of [true,false]){
  await source.getByRole('button',{name:'Select line 3',exact:true}).click();await selectionBar.getByText('Selected L3–L3',{exact:true}).waitFor();
  if(keyboard){await page.keyboard.press('Shift+F10');await page.keyboard.press('ArrowRight');await page.keyboard.press('Enter');}
  else await selectionBar.getByRole('button',{name:'More details',exact:true}).click();
  const preview=page.getByRole('dialog',{name:'Preview',exact:true});await preview.getByText('third',{exact:true}).waitFor();await preview.getByRole('button',{name:'Close',exact:true}).click();await preview.waitFor({state:'hidden'});
 }
 console.log('PASS selected material Actions hit areas, keyboard navigation and live floating actions without bypass');
 await page.getByRole('button',{name:'Diff',exact:true}).click();await page.getByRole('button',{name:'Before · Select line 1',exact:true}).click();
 const diffActions=page.locator('.review-diff').getByRole('button',{name:'Actions',exact:true});
 assert.ok(await diffActions.evaluate(el=>{const r=el.getBoundingClientRect();return el.contains(document.elementFromPoint(r.x+r.width/2,r.y+r.height/2));}),'deleted-side Actions center is not blocked by its selection toolbar');
 await diffActions.click();await page.getByRole('menuitem',{name:'Add to Material conversation',exact:true}).click();await waitFor(()=>f.captures.at(-1)?.source.commit===base,'base snapshot capture');assert.equal(f.captures.at(-1).source.commit,base);
 await page.getByRole('button',{name:'Back',exact:true}).click();const refs=await page.evaluate(A=>JSON.parse(localStorage.getItem('steve.console.draft.materials:'+A)),A);assert.ok(refs.some(r=>r.selector?.start===2&&r.selector?.end===3));assert.ok(refs.some(r=>r.selector?.start===1&&r.selector?.end===1));console.log('PASS source range and deleted diff range anchor exact snapshots');
 await page.getByRole('button',{name:'Hide details',exact:true}).click();
 await page.getByLabel('Attach files',{exact:true}).setInputFiles([{name:'pixel.png',mimeType:'image/png',buffer:png},{name:'notes.pdf',mimeType:'application/pdf',buffer:Buffer.from('%PDF-1.4 fixture')}]);await page.getByRole('list',{name:'Attached materials'}).getByText('notes.pdf',{exact:true}).waitFor();
 const imageChip=page.getByRole('list',{name:'Attached materials'}).getByText('pixel.png',{exact:true});await imageChip.click();const imageDialog=page.getByRole('dialog',{name:'Preview',exact:true});await imageDialog.getByRole('img').waitFor();await imageDialog.getByText('Image region',{exact:true}).click();const x=imageDialog.getByRole('textbox',{name:'Left ratio',exact:true});await x.fill('');await x.pressSequentially('0.25');assert.equal(await x.inputValue(),'0.25');await imageDialog.getByRole('textbox',{name:'Width ratio',exact:true}).fill('0.5');await imageDialog.getByRole('textbox',{name:'Height ratio',exact:true}).fill('0.5');await imageDialog.getByRole('button',{name:'Use region',exact:true}).click();await imageDialog.getByText('Selected original image region: x=0.25, y=0, width=0.5, height=0.5',{exact:true}).waitFor();
 await x.fill('NaN');await imageDialog.getByRole('button',{name:'Use region',exact:true}).click();await imageDialog.getByRole('alert').waitFor();assert.equal(await x.inputValue(),'NaN');await x.fill('0.9');await imageDialog.getByRole('button',{name:'Use region',exact:true}).click();await imageDialog.getByRole('alert').waitFor();await x.fill('0.25');await page.setViewportSize({width:390,height:844});await imageDialog.getByRole('button',{name:'Use region',exact:true}).click();await imageDialog.getByRole('button',{name:'Actions',exact:true}).click();await page.getByRole('menuitem',{name:'Add to Material conversation',exact:true}).click();await imageDialog.getByRole('button',{name:'Close',exact:true}).click();const crop=await page.evaluate(()=>JSON.parse(localStorage.getItem('steve.console.draft.materials:console:material-ui')).find(r=>r.selector?.kind==='rect'));assert.deepEqual(crop.selector.rect,{x:0.25,y:0,width:0.5,height:0.5});await page.setViewportSize({width:1600,height:1000});console.log('PASS image region raw decimals, NaN/bounds rejection and scale-independent normalized coordinates');
 // A screenshot on the clipboard is an attachment: the composer uploads it under a stamped name so one paste stays apart from the next.
 await page.evaluate((bytes)=>{const box=document.querySelector('[aria-label="Message"]');const data=new DataTransfer();data.items.add(new File([new Uint8Array(bytes)],'image.png',{type:'image/png'}));box.dispatchEvent(new ClipboardEvent('paste',{clipboardData:data,bubbles:true,cancelable:true}));},[...png]);
 await page.getByRole('list',{name:'Attached materials'}).getByText(/^pasted-\d{8}-\d{6}\.png$/).waitFor();console.log('PASS a pasted screenshot attaches itself under a stamped name');
 f.hideQueue=true;f.reset=true;await page.getByRole('textbox',{name:'Message',exact:true}).fill('Use these exact references');await page.getByRole('textbox',{name:'Message',exact:true}).press('Enter');await page.getByRole('button',{name:'Retry this submission',exact:true}).waitFor();const first=f.posts[0];assert.equal(first.locale,'en');assert.equal(first.refs.length,7);await page.getByRole('button',{name:'Retry this submission',exact:true}).click();await waitFor(()=>f.posts.length===2,'retry');assert.equal(f.queue.length,1);assert.equal(f.posts[1].command_id,first.command_id);assert.deepEqual(f.posts[1].refs,first.refs);console.log('PASS attachments persist with immutable refs and retry identity');
 // Previously attached materials must not be silently dropped by an older Hub.
 await page.getByLabel('Attach files',{exact:true}).setInputFiles({name:'old-hub.pdf',mimeType:'application/pdf',buffer:Buffer.from('%PDF-1.4 older hub')});await page.getByRole('list',{name:'Attached materials'}).getByText('old-hub.pdf',{exact:true}).waitFor();
 f.supportsMaterials=false;await page.getByRole('textbox',{name:'Message',exact:true}).fill('Do not send without attachment');await page.getByRole('textbox',{name:'Message',exact:true}).press('Enter');await page.getByText('This Hub does not support material references; the instruction was not sent.',{exact:true}).waitFor();await waitFor(async()=>await draftOf(page.getByRole('textbox',{name:'Message',exact:true}))==='Do not send without attachment','material capability rejection keeps draft');assert.equal(f.posts.length,2);f.supportsMaterials=true;
 console.log('PASS missing material capability prevents POST rather than stripping refs');

 f.hideQueue=false;f.queue.push({id:'live-turn',conversation:A,input:'Continue the original task',state:'running',enqueued_at:at,started_at:at});await page.evaluate(e=>window.emit(e),{kind:'console.queue',conversation:A,at});
 f.questions=[{id:'q1',conversation:A,exchange_id:'e1',kind:'question',title:'Choose a target',message:'Where should the change apply?',options:[{id:'yes',label:'Only this project'},{id:'no',label:'Do not proceed'}],required:true,created_at:at,deadline:'2030-01-01T00:00:00Z',updated_at:at,state:'pending'}];await page.evaluate(e=>window.emit(e),{kind:'console.question',conversation:A,text:'q1',at});await page.getByRole('button',{name:'Only this project',exact:true}).click();await page.getByText('Resolved requests (1)',{exact:true}).waitFor();await page.getByText('Resolved requests (1)',{exact:true}).click();await page.getByText('Answered',{exact:true}).waitFor();assert.equal(f.answers.length,1);assert.equal(f.answers[0].choice,'yes');assert.equal(f.posts.length,2);console.log('PASS agent question uses original options without submitting another prompt');
 for(const [decision,label] of [['decline','Decline'],['cancel','Cancel request']]) {const id='q-'+decision;f.questions=[{id,conversation:A,exchange_id:'e1',kind:'permission',title:id,message:'Choose before declining or cancelling',options:[{id:'allow_once',label:'Allow only once'}],required:true,created_at:at,deadline:'2030-01-01T00:00:00Z',updated_at:at,state:'pending'}];await page.evaluate(e=>window.emit(e),{kind:'console.question',conversation:A,text:id,at});await page.getByText('Allow only once',{exact:true}).click();const before=f.answers.length;await page.getByRole('button',{name:label,exact:true}).click();await waitFor(()=>f.questions[0].state!=='pending','decision accepted');assert.equal(f.answers.length,before+1);assert.equal(f.answers.at(-1).decision,decision);assert.equal(Object.hasOwn(f.answers.at(-1),'choice'),false);}
 console.log('PASS selected permission can be declined or cancelled with one valid decision payload');
 // A Hub upgrade must not discard edits outside the composer.
 await page.getByRole('textbox',{name:'Message',exact:true}).fill('');let navigations=0;const navigated=frame=>{if(frame===page.mainFrame())navigations++;};page.on('framenavigated',navigated);
 await page.getByRole('button',{name:'Actions',exact:true}).click();await page.getByRole('menuitem',{name:'Annotate',exact:true}).click();const editing=page.getByRole('dialog',{name:'Annotate material',exact:true});await editing.getByRole('textbox',{name:'Annotation',exact:true}).fill('Unsaved annotation survives an upgrade');f.version='test-upgraded';await page.evaluate(e=>window.emit(e),{kind:'node.updated',at});await waitFor(async()=> (await page.locator('.console-main').textContent()).includes('Coordinator updated.'),'upgrade banner');assert.equal(navigations,0);assert.equal(await editing.getByRole('textbox',{name:'Annotation',exact:true}).inputValue(),'Unsaved annotation survives an upgrade');assert.equal(await page.evaluate(()=>{const event=new Event('beforeunload',{cancelable:true});window.dispatchEvent(event);return event.defaultPrevented;}),true);
 page.once('dialog',dialog=>dialog.dismiss());await editing.getByRole('button',{name:'Cancel',exact:true}).click();assert.equal(await editing.isVisible(),true);page.once('dialog',dialog=>dialog.accept());await editing.getByRole('button',{name:'Cancel',exact:true}).click();await editing.waitFor({state:'hidden'});page.off('framenavigated',navigated);console.log('PASS Hub upgrades preserve annotation drafts and annotation dismissal has a local edit guard');
 const makeQuestion=(id,state,time)=>({id,conversation:A,exchange_id:'e1',kind:'question',title:id,message:'A retained question with enough context to wrap in a small window.',options:[],allow_free_text:true,required:true,created_at:time,deadline:'2030-01-01T00:00:00Z',updated_at:time,state});
 f.questions=[...Array.from({length:24},(_,i)=>makeQuestion('history-'+i,'answered',new Date(Date.parse(at)+i*1000).toISOString())),makeQuestion('pending-later','pending','2026-09-07T03:00:00Z'),makeQuestion('pending-first','pending','2026-09-07T02:00:00Z')];
 await page.evaluate(e=>window.emit(e),{kind:'console.question',conversation:A,text:'pending-first',at});await page.getByRole('heading',{name:'pending-first',exact:true}).waitFor();
 const panel=page.getByRole('region',{name:'Your response is needed'});assert.equal(await panel.locator(':scope > article h3').first().innerText(),'pending-first');assert.equal(await panel.locator('details article').count(),5);await page.setViewportSize({width:390,height:844});
 const message=page.getByRole('textbox',{name:'Message',exact:true});await waitFor(async()=>{const box=await message.boundingBox();return box&&box.y>=0&&box.y+box.height<=844;},'Question history must not push the composer out of the viewport after responsive layout settles');
 await panel.getByRole('textbox',{name:'Answer',exact:true}).first().fill('Narrow-screen response');assert.equal(await panel.getByRole('textbox',{name:'Answer',exact:true}).first().inputValue(),'Narrow-screen response');assert.ok(await panel.evaluate(el=>el.clientHeight<=window.innerHeight*.4+1));console.log('PASS many questions keep pending order, bound history and preserve a reachable mobile composer');

 }
 assert.deepEqual(f.errors,[]);
}catch(error){console.log("DEBUG",JSON.stringify(f.errors),JSON.stringify(f.captures),await page.locator("body").innerText());throw error;}finally{await context.close();await browser.close();await server.close();}
