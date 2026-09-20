// Isolated real-component fixtures: no app entry, runtime config, hub or shared Vite cache.
import assert from "node:assert/strict";
import { isDeepStrictEqual } from "node:util";
import { mkdtemp, writeFile, symlink, rm, readFile, realpath } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "../node_modules/vite/dist/node/index.js";
import react from "../node_modules/@vitejs/plugin-react/dist/index.js";
import tailwindcss from "../node_modules/@tailwindcss/vite/dist/index.mjs";
import { chromium } from "../node_modules/playwright/index.mjs";
import ts from "../node_modules/typescript/lib/typescript.js";

const root = fileURLToPath(new URL("..", import.meta.url));
const selected = process.env.CHECK?.split(",") ?? ["types", "density", "settings", "icon"];
const scratch = await realpath(await mkdtemp(path.join(tmpdir(), "steve-style-controls-")));
let server, browser;
const failures = [];
const record = async (name, run) => { try { await run(); console.log(`PASS ${name}`); } catch (error) { failures.push(`${name}: ${error.stack}`); console.error(`FAIL ${name}: ${error.message}`); } };
const fields = `<><Input size="sm" label="Input label" hint="Input hint" defaultValue="Value" /><Select size="sm" label="Select label" selectedKey="a" items={[{id:"a",label:"Selected value"}]}>{item => <Select.Item {...item}/>}</Select><Toggle label="Toggle label" hint="Toggle hint"/><TextArea label="Area label" hint="Area hint" defaultValue="Text"/></>`;
const settingsFixture = `
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { Toggle } from "@/components/base/toggle/toggle";
import { TextArea } from "@/components/base/textarea/textarea";
const Fields = () => ${fields};
const Heading = () => <div><h3 className="text-lg font-normal text-secondary">Shared heading</h3></div>;
function Fixture() { return <>
<div id="plain"><Fields/><Heading/></div>
<div className="settings-content">
<section><h3 id="owned-heading">Settings heading</h3></section>
<div className="settings-field"><div><label className="settings-field-label" id="owned-label">Setting caption</label><p className="settings-field-description" id="owned-help">Setting explanation</p></div><div id="wrapped"><Fields/></div></div>
<div className="settings-field"><label className="settings-field-label">Direct caption</label><Input size="sm" label="Direct input"/></div>
<div className="settings-field"><label className="settings-field-label">Direct toggle caption</label><Toggle label="Direct toggle"/></div>
<div id="first-fields">{React.Children.toArray(Fields().props.children).map((field,i)=><div key={i} className="settings-field">{field}</div>)}</div>
<div id="nested-heading"><Heading/></div>
<details className="settings-advanced" open><summary>Advanced</summary><p id="owned-advanced">Advanced explanation</p><div id="advanced"><Fields/></div><ul className="settings-service-list"><li><p id="owned-service">Service explanation</p></li></ul></details>
</div></> }
`;
const iconFixture = `
import { useRef, useEffect, useState } from "react";
import { X, Trash01, Plus } from "@untitledui/icons";
import { ButtonUtility } from "@/components/base/buttons/button-utility";
import { IconButton } from "@/components/steve/icon-button";
import { Dropdown } from "@/components/base/dropdown/dropdown";
window.events = {clicks:0, keys:[], submits:0, actions:[], ref:false};
function Fixture() {
const ref = useRef(null);
const [pressed,setPressed]=useState(false);
window.setPressed=setPressed;
const [legacyDisabled,setLegacyDisabled]=useState(false);
window.setLegacyDisabled=setLegacyDisabled;
// The two skills removals and four settings-editor actions retain their base API.
const legacyVariants=[
{id:"source",size:"sm",color:"tertiary",icon:Trash01},
{id:"directory",size:"sm",color:"tertiary",icon:Trash01},
{id:"list-remove",size:"xs",color:"tertiary",icon:Trash01},
{id:"list-add",size:"xs",color:"secondary",icon:Plus},
{id:"map-remove",size:"xs",color:"tertiary",icon:Trash01},
{id:"argument-remove",size:"xs",color:"tertiary",icon:Trash01}];
useEffect(() => { window.events.ref = ref.current instanceof HTMLButtonElement; window.focusIcon = () => ref.current.focus(); }, []);
return <>
<form onSubmit={e=>{e.preventDefault();window.events.submits++;}}>
<IconButton id="default" ref={ref} icon={X} label="Close panel" tooltip="Dismiss this panel" data-fixture="native" aria-controls="target" aria-keyshortcuts="Alt+C" aria-busy="true" onClick={()=>window.events.clicks++} onKeyDown={e=>window.events.keys.push(e.key)}/>
<IconButton id="submit" icon={X} label="Submit" type="submit"/>
<IconButton id="disabled" icon={X} label="Disabled" disabled tooltip="Unavailable" onClick={()=>window.events.clicks++}/>
<IconButton id="aria-disabled" icon={X} label="Aria disabled" isDisabled onClick={()=>window.events.clicks++}/>
</form>
<ButtonUtility id="base-default" icon={X} color="tertiary" size="sm" aria-label="Base"/>
{["xs","sm","lg"].flatMap(size=>["secondary","tertiary"].map(color=><div key={size+color}>
<IconButton id={size+color} icon={X} label={size+color} size={size} color={color}/>
<ButtonUtility id={"base-"+size+color} icon={X} aria-label={"Base "+size+color} size={size} color={color}/>
</div>))}
<IconButton id="element" title="Native title" label="Element icon" icon={<X data-element-icon className="rotate-180" ref={node=>{window.events.iconRef = node instanceof SVGElement;}}/>}/>
<Dropdown.Root><IconButton id="menu-trigger" label="More actions" tooltip="Actions for this row" icon={X}/><Dropdown.Popover><Dropdown.Menu aria-label="Actions" onAction={key=>window.events.actions.push(key)}><Dropdown.Item id="first" label="First action"/><Dropdown.Item id="second" label="Second action"/></Dropdown.Menu></Dropdown.Popover></Dropdown.Root>
{["secondary","tertiary"].map(color=><div key={color}>
<IconButton id={"pressed-"+color} label={"Pressed "+color} icon={X} color={color} aria-pressed={pressed}/>
<ButtonUtility id={"base-pressed-"+color} aria-label={"Base pressed "+color} icon={X} color={color} aria-pressed={pressed}/>
<ButtonUtility id={"base-disabled-"+color} aria-label={"Base disabled "+color} icon={X} color={color} isDisabled/>
</div>)}
{legacyVariants.map(({id,...props})=><ButtonUtility key={id} {...props} id={"legacy-"+id} tooltip={id} isDisabled={legacyDisabled} onClick={()=>window.events.clicks++}/>)}
<div className="app-sidebar is-compact"><IconButton id="compact" className="app-collapse" size="lg" label="Compact" icon={X}/></div>
<div id="target"/></> }
`;
async function fixture(name, source) {
    await writeFile(path.join(scratch, `${name}.tsx`), `import React from "react"; import {createRoot} from "react-dom/client"; import "./fixture.css";\n${source}\ncreateRoot(document.getElementById("root")).render(<Fixture/>);`);
    await writeFile(path.join(scratch, `${name}.html`), `<html><body><div id="root"></div><script type="module" src="/${name}.tsx"></script></body></html>`);
    const context = await browser.newContext({ viewport: {width: 1280, height: 1600}, serviceWorkers: "block" });
    const page = await context.newPage(); page.setDefaultTimeout(10000);
    const errors = [];
    page.on("pageerror", e=>errors.push(String(e)));
    await context.route("**/*", route=>{
        const url = new URL(route.request().url());
        if(url.origin !== origin || route.request().method() !== "GET" || ["fetch","xhr","eventsource"].includes(route.request().resourceType())) { errors.push(`Unexpected request: ${url.pathname}`); return route.abort(); }
        return route.continue();
    });
    await page.goto(`${origin}/${name}.html`);
    return {page, context, errors};
}
let origin;
const appearance = locator => locator.evaluate(el=>{
    const s=getComputedStyle(el), svg=el.querySelector("svg"), icon=svg&&getComputedStyle(svg);
    return Object.fromEntries(["width","height","padding","borderRadius","color","backgroundColor","boxShadow","opacity","outlineStyle","outlineWidth","outlineColor"].map(k=>[k,s[k]]).concat([["iconWidth",icon?.width],["iconHeight",icon?.height]]));
});
const textStyle = locator => locator.evaluate(el=>{const s=getComputedStyle(el);return Object.fromEntries(["paddingTop","marginTop","color","fontSize","lineHeight","fontWeight","display"].map(k=>[k,s[k]]));});
try {
    await symlink(path.join(root,"node_modules"),path.join(scratch,"node_modules"),"dir");
    if(selected.includes("types")) await record("IconButton public types",async()=>{
        const file=path.join(scratch,"types.tsx");
        await writeFile(file, `import {createRef} from "react";
import {IconButton, type IconButtonProps} from "@/components/steve/icon-button";
import {X} from "@untitledui/icons";
const ref=createRef<HTMLButtonElement>();
const props: IconButtonProps={label:"Name",icon:X,ref,title:"Hint",size:"lg",color:"secondary",onClick:e=>e.currentTarget.focus(),onKeyDown:e=>e.currentTarget.focus(),"aria-pressed":true};
<IconButton {...props} data-example="preserved"/>;
<IconButton label="Element" icon={<X className="rotate-180"/>}/>;
// @ts-expect-error an accessible name is mandatory, independent of tooltip
<IconButton icon={X} tooltip="Not a name"/>;
// @ts-expect-error no speculative medium size
<IconButton label="Name" size="md"/>;
// @ts-expect-error no third visual palette
<IconButton label="Name" color="primary"/>;
`);
        const program=ts.createProgram([file],{noEmit:true,strict:true,skipLibCheck:true,target:ts.ScriptTarget.ESNext,module:ts.ModuleKind.ESNext,moduleResolution:ts.ModuleResolutionKind.Bundler,jsx:ts.JsxEmit.ReactJSX,baseUrl:root,paths:{"@/*":["src/*"]}});
        const diagnostics=ts.getPreEmitDiagnostics(program);
        assert.equal(diagnostics.length,0,ts.formatDiagnosticsWithColorAndContext(diagnostics,{getCurrentDirectory:()=>root,getCanonicalFileName:f=>f,getNewLine:()=>"\n"}));
    });
    if(selected.includes("density")) await record("plugin field density", async()=>{
        for(const file of ["import","configure","preset"]) {
            const name=path.join(root,`src/components/steve/plugins/${file}.tsx`), source=await readFile(name,"utf8");
            const ast=ts.createSourceFile(name,source,ts.ScriptTarget.Latest,true,ts.ScriptKind.TSX);
            let count=0;
            function visit(node) {
                if((ts.isJsxOpeningElement(node)||ts.isJsxSelfClosingElement(node)) && ["Input","Select"].includes(node.tagName.getText(ast))) {
                    count++; const size=node.attributes.properties.find(a=>ts.isJsxAttribute(a)&&a.name.getText(ast)==="size");
                    assert.equal(size?.initializer?.getText(ast),'"sm"',`${file}:${ast.getLineAndCharacterOfPosition(node.pos).line+1} field density`);
                } ts.forEachChild(node,visit);
            } visit(ast); assert.ok(count>0);
        }
    });
    if(selected.some(name=>["settings","icon"].includes(name))) {
        await writeFile(path.join(scratch,"fixture.css"), `@import "${root}/src/styles/globals.css";\n@import "${root}/src/styles/settings.css";\n@source "${root}/src";\n`);
        server=await createServer({root:scratch,configFile:false,envDir:false,cacheDir:path.join(scratch,"cache"),plugins:[react(),tailwindcss()],resolve:{alias:{"@":path.join(root,"src")},dedupe:["react","react-dom"]},server:{host:"127.0.0.1",port:0,fs:{allow:[scratch,root]}},logLevel:"error"});
        await server.listen(); origin=`http://127.0.0.1:${server.httpServer.address().port}`;
        browser=await chromium.launch({headless:true,channel:process.env.BROWSER_CHANNEL});
    }
    if(selected.includes("settings")) await record("settings shared label/text isolation",async()=>{
        const {page,context,errors}=await fixture("settings",settingsFixture);
        try {
            await page.locator("#plain input").first().waitFor();
            const drift=[]; const compare=(actual,expected,name)=>{if(!isDeepStrictEqual(actual,expected))drift.push({name,actual,expected});};
            for(const width of [1280,390]) {
                await page.setViewportSize({width,height:1600});
                for(const scope of ["wrapped","advanced","first-fields"]) {
                    for(const selector of ["label[data-label]", "label:has([role=switch])", "label:has([role=switch]) p", "button section p"]) {
                        const baseline=page.locator(`#plain ${selector}`), nested=page.locator(`#${scope} ${selector}`);
                        assert.ok(await baseline.count()>0,selector);
                        assert.equal(await nested.count(),await baseline.count());
                        for(let i=0;i<await baseline.count();i++) compare(await textStyle(nested.nth(i)),await textStyle(baseline.nth(i)),`${width} ${scope} ${selector} ${i}`);
                    }
                }
                compare(await textStyle(page.locator(".settings-field > [data-input-wrapper] > label").filter({hasText:"Direct input"})),await textStyle(page.locator("#plain label[data-label]").first()),"direct Input label retains base style");
                compare(await textStyle(page.locator(".settings-field > label:has([role=switch])").filter({hasText:"Direct toggle"})),await textStyle(page.locator("#plain label:has([role=switch])")),"direct Toggle label retains base style");
                compare(await textStyle(page.locator("#nested-heading h3")),await textStyle(page.locator("#plain h3")),"settings h3 must not reach shared headings");
                assert.equal((await textStyle(page.locator("#owned-label"))).paddingTop,width===1280?"6px":"0px","owned caption spacing retained");
                assert.equal((await textStyle(page.locator("#owned-help"))).fontSize,"12px");
                assert.equal((await textStyle(page.locator("#owned-heading"))).fontSize,"14px");
                for(const id of ["owned-advanced","owned-service"]) assert.equal((await textStyle(page.locator(`#${id}`))).fontSize,"12px");
            }
            assert.deepEqual(drift,[], "settings descendants must retain shared text styles");
            assert.deepEqual(errors,[]);
        } finally {await context.close();}
    });
    if(selected.includes("icon")) await record("IconButton base parity and native contracts",async()=>{
        await readFile(path.join(root,"src/components/steve/icon-button.tsx"),"utf8");
        const {page,context,errors}=await fixture("icon",iconFixture);
        try {
            const button=page.getByRole("button",{name:"Close panel",exact:true}); await button.waitFor();
            assert.deepEqual(await appearance(button),await appearance(page.locator("#base-default")),"default sm tertiary base parity");
            for(const size of ["xs","sm","lg"]) for(const color of ["secondary","tertiary"]) assert.deepEqual(await appearance(page.locator(`#${size}${color}`)),await appearance(page.locator(`#base-${size}${color}`)));
            assert.equal((await appearance(page.locator("#lgtertiary"))).width,"40px"); assert.equal((await appearance(page.locator("#compact"))).height,"40px");
            assert.equal(await button.getAttribute("aria-keyshortcuts"),"Alt+C"); assert.equal(await button.getAttribute("aria-busy"),"true");
            assert.equal(await button.getAttribute("data-fixture"),"native"); assert.equal(await button.getAttribute("aria-controls"),"target");
            assert.equal(await page.evaluate(()=>window.events.ref),true);
            await page.evaluate(()=>window.focusIcon()); assert.equal(await button.evaluate(el=>el===document.activeElement),true);
            await button.press("Enter"); await button.press("Space");
            assert.equal(await page.evaluate(()=>window.events.clicks),2); assert.equal(await page.evaluate(()=>window.events.submits),0,"default button never submits");
            assert.ok((await page.evaluate(()=>window.events.keys)).includes("Enter"));
            await page.locator("#submit").click(); assert.equal(await page.evaluate(()=>window.events.submits),1,"explicit native submit retained");
            for(const id of ["disabled","aria-disabled"]) {assert.equal(await page.locator(`#${id}`).isDisabled(),true);await page.locator(`#${id}`).evaluate(el=>el.click());}
            assert.equal(await page.evaluate(()=>window.events.clicks),2);
            await button.hover(); await page.getByRole("tooltip").filter({hasText:"Dismiss this panel"}).waitFor(); assert.equal(await button.getAttribute("aria-label"),"Close panel","tooltip never replaces name");
            await page.mouse.move(1000,1000); await button.focus(); await page.keyboard.press("Tab"); await page.keyboard.press("Shift+Tab");
            assert.equal(await button.evaluate(el=>el.matches(":focus-visible")),true);
            assert.notEqual((await appearance(button)).outlineStyle,"none");
            const element=page.locator("#element svg");
            assert.equal(await element.getAttribute("aria-hidden"),"true");
            assert.notEqual(await element.getAttribute("data-icon"),null);
            assert.ok((await element.getAttribute("class")).includes("rotate-180"));
            assert.equal(await page.evaluate(()=>window.events.iconRef),true,"element icon ref preserved");
            const fcSize=await appearance(page.locator("#base-default")), elementSize=await appearance(page.locator("#element"));
            assert.equal(elementSize.iconWidth,fcSize.iconWidth); assert.equal(elementSize.iconHeight,fcSize.iconHeight);
            const stateDrift=[];
            const surface=async locator=>{const style=await appearance(locator); return {color:style.color,background:style.backgroundColor,shadow:style.boxShadow};};
            const idle=async()=>{await page.mouse.move(1000,1500); await page.waitForTimeout(150);};
            for(const color of ["secondary","tertiary"]) {
                const control=page.locator(`#pressed-${color}`), base=page.locator(`#base-pressed-${color}`), disabled=page.locator(`#base-disabled-${color}`);
                await idle(); const normal=await surface(control), disabledNormal=await surface(disabled);
                await control.hover(); await page.waitForTimeout(150); const hovered=await surface(control);
                await idle(); await page.evaluate(()=>window.setPressed(true)); await page.waitForTimeout(150);
                const active=await surface(control);
                if(isDeepStrictEqual(active,normal)||!isDeepStrictEqual(active,hovered)) stateDrift.push({color,normal,hovered,active});
                assert.deepEqual(active,await surface(base),"adapter and original base consumers share pressed owner");
                await page.evaluate(()=>window.setPressed(false)); await page.waitForTimeout(150); assert.deepEqual(await surface(control),normal,"pressed=false restores idle");
                await disabled.hover(); await page.waitForTimeout(150);
                if(!isDeepStrictEqual(await surface(disabled),disabledNormal)) stateDrift.push({color,disabledNormal,disabledHover:await surface(disabled)});
                await idle();
            }
            assert.equal(await page.locator("#element").getAttribute("title"),"Native title");
            const legacy=page.locator("[id^=legacy-]");
            for(let i=0;i<await legacy.count();i++) {
                const control=legacy.nth(i), id=await control.getAttribute("id");
                assert.equal(await control.getAttribute("aria-label"),id.slice(7));
                const expected=id==="legacy-source"||id==="legacy-directory"?"28px":"24px";
                assert.equal((await appearance(control)).width,expected,"existing utility consumer size unchanged");
            }
            await page.evaluate(()=>window.setLegacyDisabled(true));
            for(let i=0;i<await legacy.count();i++) {
                const control=legacy.nth(i); assert.equal(await control.isDisabled(),true);
                await idle(); const normal=await surface(control); await control.hover(); await page.waitForTimeout(150);
                assert.deepEqual(await surface(control),normal,"existing disabled utility does not hover");
                await control.evaluate(el=>el.click());
            }
            assert.equal(await page.evaluate(()=>window.events.clicks),2,"disabled legacy actions do not fire");
            const trigger=page.getByRole("button",{name:"More actions",exact:true});
            await trigger.focus(); await trigger.press("Enter"); await page.getByRole("menu",{name:"Actions"}).waitFor();
            await page.waitForFunction(()=>document.activeElement?.getAttribute("role")==="menuitem");
            await page.keyboard.press("ArrowDown"); await page.keyboard.press("Enter");
            assert.deepEqual(await page.evaluate(()=>window.events.actions),["second"]);
            await page.getByRole("menu").waitFor({state:"hidden"});
            await trigger.press("Enter"); await page.getByRole("menu").waitFor();
            await page.waitForFunction(()=>document.activeElement?.getAttribute("role")==="menuitem");
            await page.keyboard.press("Escape");
            await page.getByRole("menu").waitFor({state:"hidden"});
            await page.waitForFunction(()=>document.activeElement?.id==="menu-trigger");
            assert.equal(await trigger.evaluate(el=>el===document.activeElement),true,"menu returns focus to native button ref");
            assert.deepEqual(stateDrift,[],"pressed remains visible and disabled hover is inert");
            assert.deepEqual(errors,[]);
        } finally {await context.close();}
    });
} finally {await browser?.close();await server?.close();await rm(scratch,{recursive:true,force:true});}
if(failures.length) {console.error(failures.join("\n"));process.exitCode=1;}
