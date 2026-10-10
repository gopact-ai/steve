import assert from "node:assert/strict";
import test from "node:test";
import { ownPreference, sessionOptionRole, sessionPreferenceScope } from "../src/lib/session-options.ts";

test("special roles never infer from vendor category, name, or inherited keys",()=>{
 for(const option of [{ID:"__proto__",Name:"Thinking preset",Type:"select",Category:"vendor/private"},{ID:"effort",Name:"Thinking level",Type:"select",Category:"vendor/private"},{ID:"mode",Name:"Mode",Type:"select",Category:"vendor/private"}])assert.equal(sessionOptionRole(option),undefined);
 assert.equal(sessionOptionRole({ID:"x",Name:"Any",Type:"select",Category:"thought_level"}),"reasoning");
 assert.equal(sessionOptionRole({ID:"thought_level",Name:"Any",Type:"select"}),"reasoning");
 assert.equal(sessionOptionRole({ID:"x",Name:"Any",Type:"select",Category:"mode"}),"approval");
 assert.equal(ownPreference({},"__proto__"),undefined);assert.equal(ownPreference({},"constructor"),undefined);
 const own=JSON.parse('{"__proto__":"false"}');assert.equal(ownPreference(own,"__proto__"),"false");
});
test("scope includes only supplied context and workspace facts, without invented revisions",()=>{
 const context={conversation:"c",agent:{id:"a",node:"n",harness:"h",place:{workspace:"w",node:"n",kind:"copy"}},agents:[],project:{id:"p",node:"n",path:"/fixture",repo:"r",version:1,bound:true}};
 const scope=sessionPreferenceScope("c",context);
 for(const change of [{agent:{...context.agent,node:"n2"}},{agent:{...context.agent,harness:"h2"}},{project:{...context.project,path:"/other"}},{project:{...context.project,version:2}},{agent:{...context.agent,place:{...context.agent.place,workspace:"w2"}}}])assert.notEqual(sessionPreferenceScope("c",{...context,...change}),scope);
 assert.equal(sessionPreferenceScope("c",structuredClone(context)),scope);
 assert.notEqual(sessionPreferenceScope("c",context,{id:"w",node:"n",path:"/one",kind:"copy"}),sessionPreferenceScope("c",context,{id:"w",node:"n",path:"/two",kind:"copy"}));
});
