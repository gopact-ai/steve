import assert from "node:assert/strict";
const saved=new Map();globalThis.localStorage={getItem:k=>saved.get(k)??null,setItem:(k,v)=>saved.set(k,String(v)),removeItem:k=>saved.delete(k),key:i=>[...saved.keys()][i]??null,get length(){return saved.size}};
const refs=id=>JSON.parse(saved.get('steve.console.draft.materials:'+id)||'null');
globalThis.navigator.locks ||= { request: async (_key, work) => work() };
const store=await import('../../web/console/src/lib/drafts.ts');
const ref={id:'m-one',title:'File lines',project:'p',kind:'text',mime:'text/plain',size:50,selector:{kind:'lines',start:2,end:4}};
assert.equal(await store.addDraftMaterial('console:a',ref),true);await store.updateDraft('console:a','Discuss this range');
const first=await store.beginSubmission('console:a','Discuss this range',[],true,'en');
assert.deepEqual(first.refs,[ref]);assert.equal(first.locale,'en');assert.equal(refs('console:a'),null);
await store.addDraftMaterial('console:a',{...ref,id:'m-next'});await store.updateDraft('console:a','New draft');
await store.failSubmission('console:a',first.id,'reset','unknown');const retry=await store.retrySubmission('console:a');assert.deepEqual(retry.refs,[ref]);assert.equal(retry.locale,'en');assert.equal(retry.id,first.id);
assert.equal(await store.reconcileSubmission('console:a',[{conversation:'console:a',key:'client:'+first.id,input:first.input,refs:[{id:'m-wrong'}],locale:'en'}]),false);
assert.equal(await store.reconcileSubmission('console:a',[{conversation:'console:a',key:'client:'+first.id,input:first.input,refs:[{id:ref.id,selector:ref.selector}],locale:'en'}]),true);
const second=await store.beginSubmission('console:a','New draft',[],true,'zh');await store.failSubmission('console:a',second.id,'rejected','rejected');
assert.equal(saved.get('steve.console.draft.text:console:a'),'New draft');assert.equal(refs('console:a')[0].id,'m-next');
const control=await store.beginSubmission('console:a','/project use q',[],false,'en');assert.deepEqual(control.refs,[]);assert.equal(refs('console:a')[0].id,'m-next');
console.log('PASS material refs and locale are atomic with draft submission, retry and recovery');
const quote=(reply)=>({conversation:'console:q',reply_id:reply});
const quoteKey=(q)=>'steve.console.draft.quote:'+JSON.stringify([q.conversation,q.reply_id]);
const storedQuotes=()=>[...saved].filter(([key])=>key.startsWith('steve.console.draft.quote:')).map(([,value])=>JSON.parse(value)).sort((a,b)=>a.order<b.order?-1:a.order>b.order?1:0).map((entry)=>entry.quote.reply_id);
saved.set(quoteKey(quote('r2')),JSON.stringify({quote:quote('r2'),order:'2'}));saved.set(quoteKey(quote('r1')),JSON.stringify({quote:quote('r1'),order:'1'}));
await store.updateDraft('console:q','With quotes');
const carried=await store.beginSubmission('console:q','With quotes',[],true,'en',true);
assert.deepEqual(carried.quotes.map((q)=>q.reply_id),['r1','r2']);assert.deepEqual(storedQuotes(),[]);
saved.set(quoteKey(quote('r3')),JSON.stringify({quote:quote('r3'),order:'1'}));
assert.equal(await store.failSubmission('console:q',carried.id,'rejected','rejected'),true);
assert.deepEqual(storedQuotes(),['r1','r2','r3'],'restored quotes return ahead of one added meanwhile, which keeps its own entry');
console.log('PASS each quote is stored on its own, in order, and a rejected send returns its quotes first');
// A quote placed between two others takes a position strictly between theirs
// and never renumbers them, however often the same gap is split again.
for (const [where, place] of [['before the last', (list, next) => [...list.slice(0, -1), next, list.at(-1)]], ['after the first', (list, next) => [list[0], next, ...list.slice(1)]], ['at the front', (list, next) => [next, ...list]]]) {
  let list = [quote('first'), quote('last')], known = store.orderQuotes(list, {});
  for (let round = 0; round < 500; round++) {
    list = place(list, quote('inserted-' + round));
    const order = store.orderQuotes(list, known), keys = list.map((q) => JSON.stringify([q.conversation, q.reply_id]));
    for (const key of Object.keys(known)) assert.equal(order[key], known[key], `${where}, round ${round}: an existing quote keeps its position`);
    for (let index = 1; index < keys.length; index++) assert.ok(order[keys[index - 1]] < order[keys[index]], `${where}, round ${round}: positions ${index - 1} and ${index} stay in order`);
    known = order;
  }
}
console.log('PASS repeatedly inserting quotes at one place keeps every position distinct and in order');
