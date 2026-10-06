import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import ts from "typescript";
import { sessionOption, reportedOption } from "../src/lib/session-options.ts";

const web=fileURLToPath(new URL("..",import.meta.url)),repo=await realpath(path.resolve(web,"../.."));
const temp=await mkdtemp(path.join(tmpdir(),"topt-wire-"));
try {
 const destination=path.join(temp,"wire.json"),overlay=path.join(temp,"overlay.json");
 await writeFile(overlay,JSON.stringify({Replace:{[path.join(repo,"internal/httpapi/typed_options_frontend_wire_test.go")]:path.join(web,"scripts/typed-options-wire.go.txt")}}));
 const go=spawnSync("go",["test","-p","1","-parallel","1","-overlay",overlay,"./internal/httpapi","-run","^TestTypedOptionsFrontendWireFixture$","-count=1"],{cwd:repo,encoding:"utf8",env:{...process.env,TYPED_OPTIONS_WIRE:destination}});
 assert.equal(go.status,0,go.stdout+go.stderr);
 const fixture=JSON.parse(await readFile(destination,"utf8"));
 const shape=path.join(temp,"wire.ts");
 await writeFile(shape,`import type {Selectors,Snapshot} from ${JSON.stringify(path.join(web,"src/lib/types.ts"))};\n(${JSON.stringify(fixture.selectors)} satisfies Selectors);\n(${JSON.stringify(fixture.state.agents.map(({id,harness,node,selectors,options})=>({id,harness,node,selectors,options})))} satisfies Pick<Snapshot["agents"][number],"id"|"harness"|"node"|"selectors"|"options">[]);`);
 const program=ts.createProgram([shape],{strict:true,noEmit:true,skipLibCheck:true,allowImportingTsExtensions:true,target:ts.ScriptTarget.ESNext,module:ts.ModuleKind.ESNext,moduleResolution:ts.ModuleResolutionKind.Bundler});
 const diagnostics=ts.getPreEmitDiagnostics(program);assert.equal(diagnostics.length,0,ts.formatDiagnostics(diagnostics,{getCurrentDirectory:()=>temp,getCanonicalFileName:s=>s,getNewLine:()=>"\n"}));
 for(const [name,source] of [["http","src/lib/http.ts"],["console","src/lib/api/console.ts"],["fleet","src/lib/api/fleet.ts"],["i18n","src/lib/i18n.ts"]]) {
  if(name==="i18n") continue;
  let code=ts.transpileModule(await readFile(path.join(web,source),"utf8"),{compilerOptions:{target:ts.ScriptTarget.ESNext,module:ts.ModuleKind.ESNext}}).outputText;
  code=code.replaceAll('from "../http"','from "./http.mjs"').replace('from "../i18n"','from "./i18n.mjs"');
  await writeFile(path.join(temp,`${name}.mjs`),code);
 }
 await writeFile(path.join(temp,"i18n.mjs"),'export class LocalizedError extends Error {}; export const translate=()=>"fixture";');
 globalThis.window={location:{search:""}};globalThis.sessionStorage={getItem:()=>""};
 globalThis.fetch=async url=>new Response(JSON.stringify(String(url).includes("/selectors")?fixture.selectors:fixture.state),{status:200,headers:{"Content-Type":"application/json"}});
 const api=await import(pathToFileURL(path.join(temp,"console.mjs"))),fleet=await import(pathToFileURL(path.join(temp,"fleet.mjs")));
 const selectors=await api.fetchSelectors("console:fixture","fixture-agent"),state=await fleet.fetchState();
 const fromSession=selectors.options.map(sessionOption),fromObservation=state.agents[0].selectors.map(sessionOption);
 const comparable=options=>options.map(option=>({...option,current:option.current ?? ""}));
 assert.deepEqual(comparable(fromSession),comparable(fromObservation));
 assert.equal(fromSession[0].type,"boolean");assert.equal(reportedOption(fromSession[0]),"false");assert.equal(selectors.preferred["vendor/toggle"],"true");
 assert.equal(reportedOption(fromSession[1]),undefined);assert.equal(fromSession[2].type,"select");assert.equal(fromSession[2].current,"false");assert.equal(fromSession[2].choices[0].value,"false");
 console.log("PASS real Go observation→HTTP handlers→TS API→normalization: explicit Type/false/unset/opaque IDs, requested != Actual; no Agent execution");
} finally {await rm(temp,{recursive:true,force:true});}
