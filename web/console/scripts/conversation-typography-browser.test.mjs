// Real shared message components and production CSS, isolated synthetic content.
import assert from "node:assert/strict";
import { mkdtemp, writeFile, readFile, rm, mkdir } from "node:fs/promises";
import http from "node:http";
import path from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { build } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";
import react from "../node_modules/@vitejs/plugin-react/dist/index.js";
import tailwindcss from "../node_modules/@tailwindcss/vite/dist/index.mjs";

const web = fileURLToPath(new URL("..", import.meta.url));
const scratch = await mkdtemp(path.join(tmpdir(), "steve-conversation-type-"));
const text = [
    "检查已完成，**没有修改业务代码**。当前的 `warning` 来自检查规则与实际输入格式不一致，不代表服务运行失败。",
    "这条规则把时间中的冒号识别成了 `host:port`。输入还会经过 `_validate_dimension()` 的白名单检查，已有测试覆盖了正常值和边界条件。保留正确行为比为单条误报改变实现更重要。",
    "处理后状态为 `resolved`，后续仍需人工 review。这里区分自动检查的结果与合并权限，避免把检查通过误认为已经获准发布。",
    "## 验证结果",
    "| 检查项 | 结果 | 说明 |\n| --- | --- | --- |\n| 单元测试 | 通过 | 原有行为保持不变 |\n| 静态检查 | 通过 | 无新增告警 |\n| 人工审核 | 待处理 | 由维护者确认 |",
    "```text\nUnit tests       succeeded\nStatic analysis  succeeded\nHuman review     pending\n```",
    "## 后续操作",
    "- 请维护者确认变更范围。\n- 保留当前实现，不引入额外兼容分支。\n  - 复核通过后再安排发布。\n  - 若结论变化，重新检查边界条件。",
    "> 自动检查通过不等于已经发布。\n>\n> 发布前仍需确认审核状态。",
    "参考 [项目说明](https://example.test/guide)，或者查看 `request_status_and_validation_result_with_a_very_long_identifier_without_spaces`。更多说明用于验证中文与 English 混排时的阅读节奏。",
];
const markdown = text.join("\n\n");
let server, browser;
try {
    await writeFile(path.join(scratch, "index.html"), '<meta charset="utf-8"><div id="root"></div><script type="module" src="/entry.tsx"></script>');
    await writeFile(path.join(scratch, "style.css"), `@import "${web}/src/styles/globals.css";\n@source "${web}/src";\n@source "${scratch}/entry.tsx";`);
    await writeFile(path.join(scratch, "entry.tsx"), `
        import { createRoot } from "react-dom/client";
        import { useMemo, useRef, useState } from "react";
        import { PageHeader, Panel } from "@/components/steve/page";
        import { DialogHeader } from "@/components/steve/dialog-surface";
        import { SourceView } from "@/components/steve/source-view";
        import { DiffView } from "@/components/steve/diff-view";
        import { CodeBlock } from "@/components/steve/code-block";
        import { MarkdownInput } from "@/components/steve/markdown-input";
        import { AssistantMessage, UserMessage } from "@/components/steve/message";
        import { Md } from "@/components/steve/markdown";
        import { LocaleProvider } from "@/providers/locale-provider";
        import { SelectionProvider } from "@/providers/selection-provider";
        import "./style.css";
        const text = ${JSON.stringify(markdown)};
        const reply = { id:"fixture", conversation:"console:fixture", kind:"reply", text, at:"2026-09-21T07:16:29Z" };
        function RoleFixtures() {
            const [draft, setDraft] = useState(${JSON.stringify("Composer reading text\n\n## Composer heading\n\n```text\ncomposer code\n```")});
            const [sourceLines, setSourceLines] = useState(1);
            window.setSourceLines = setSourceLines;
            const sourceText = useMemo(() => Array(sourceLines).fill("source text").join("\\n"), [sourceLines]);
            const handle = useRef(null);
            return <section id="roles">
                <PageHeader title="Page heading" /><Panel title="Section heading">UI body</Panel>
                <div data-dialog-surface><DialogHeader title="Dialog heading" /></div>
                <div className="profile-reading"><Md text={${JSON.stringify("# Profile heading\n\nProfile reading text")}} /></div>
                <textarea className="profile-editor font-mono text-sm" defaultValue="Profile draft" />
                <MarkdownInput value={draft} onChange={setDraft} label="Draft" placeholder="Write" handle={handle} />
                <SelectionProvider><div id="source-fixture" style={{height:300}}>
                    <SourceView file={{ path:"fixture.txt", text:sourceText, size:sourceText.length }}
                        capture={{project:"fixture",title:"Fixture",source:{kind:"snapshot-file",attempt:"fixture",commit:"fixture",path:"fixture.txt"}}} />
                </div></SelectionProvider>
                <div className="console-toolbar" style={{width:600,maxWidth:"100%"}}>
                    <span className="console-status" id="status-fixture" style={{width:200}}>Stop is not confirmed.<br/>Retry the same request.</span>
                </div>
                <DiffView layout="unified" diff={${JSON.stringify("@@ -1 +1 @@\n-old\n+new\n")}} />
                <div id="reader"><Md text={${JSON.stringify("## Reader heading\n\nReader text with `inline code`.\n\n##### Small heading")}} /></div>
                <div id="tool-output"><CodeBlock code="tool output" lang="shell" /></div>
                <div className="prose md md-conversation"><div className="not-prose" id="embedded-ui">
                    <span className="u-meta">Embedded metadata</span>
                    <div className="prose md md-conversation"><p>Nested excluded prose</p><h2>Nested excluded heading</h2><code>excluded code</code></div>
                </div></div>
                <div className="app-nav-item">Navigation</div><span className="text-xs" id="ui-token">UI token</span>
            </section>;
        }
        createRoot(document.getElementById("root")).render(<LocaleProvider>
            <main className="conversation-content" style={{ minHeight:"100vh" }}>
                <header className="console-toolbar"><div className="console-heading"><h1>检查结果与后续处理</h1><div className="console-location">示例项目 · 工作区</div></div></header>
                <div className="transcript-scroll"><div className="transcript-messages">
                    <UserMessage r={{...reply, kind:"sent", input:"请检查告警原因，并说明验证结果。"}} readOnly />
                    <section id="answer"><AssistantMessage r={reply} readOnly /></section>
                    <section id="plain"><AssistantMessage r={{...reply,id:"plain",text:"Plain text stays plain.\\n第二行保留原始换行。",format:"text"}} readOnly /></section>
                    <section id="compact"><Md size="xs" className="md-quiet" text={"过程摘要：\\n\\n保持紧凑，不采用最终回答的段落间距。"} /></section>
                </div></div>
                <RoleFixtures />
            </main>
        </LocaleProvider>);
    `);
    await build({
        root: scratch, configFile: false, envDir: false,
        plugins: [react(), tailwindcss(), {
            // Exercise the real selection UI without fleet, storage or action side effects.
            name: "selection-fixture-context", enforce: "pre",
            load(id) {
                if (id.endsWith("/providers/material-provider.tsx"))
                    return 'export const useMaterial = () => ({target:{project:"fixture",conversation:"fixture",title:"Fixture"}});';
                if (id.endsWith("/providers/side-chat-provider.tsx"))
                    return "export const useSideChat = () => ({});";
            },
        }],
        cacheDir: path.join(scratch, "cache"), logLevel: "error",
        resolve: { alias: { "@": path.join(web, "src"), react: path.join(web, "node_modules/react"), "react-dom": path.join(web, "node_modules/react-dom") } },
        build: { outDir: path.join(scratch, "dist"), emptyOutDir: true },
    });
    server = http.createServer(async (req, res) => {
        try {
            assert.equal(req.method, "GET");
            const file = path.resolve(scratch, "dist", "." + (req.url === "/" ? "/index.html" : req.url));
            assert.ok(file.startsWith(path.join(scratch, "dist") + path.sep));
            res.setHeader("Content-Type", file.endsWith(".js") ? "text/javascript" : file.endsWith(".css") ? "text/css" : "text/html");
            res.end(await readFile(file));
        } catch { res.writeHead(404); res.end(); }
    });
    await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
    const origin = `http://127.0.0.1:${server.address().port}`;
    browser = await chromium.launch({ headless: true });
    const page = await browser.newPage({ viewport: { width: 1100, height: 1500 }, reducedMotion: "reduce" });
    const errors = [];
    page.on("pageerror", error => errors.push(String(error)));
    await page.route("**/*", route => {
        const request = route.request(), url = new URL(request.url());
        if (url.origin === origin && request.method() === "GET") {
            if (url.pathname === "/console/queue" && url.searchParams.get("capabilities") === "1")
                return route.fulfill({ json: { queue: [], submission_keys: true, material_refs: true } });
            if (!url.pathname.startsWith("/console/") && url.pathname !== "/state") return route.continue();
        }
        errors.push(`Unexpected request: ${request.method()} ${url.pathname}`); return route.abort();
    });
    await page.addInitScript(() => {
        localStorage.setItem("steve.ui.locale", "en");
        Object.defineProperty(navigator, "clipboard", { value: { writeText: async value => { window.copiedText = value; } } });
    });
    await page.goto(origin);
    await page.locator("#answer .md").waitFor();
    for (const dark of [true, false]) {
        await page.evaluate(dark => document.documentElement.classList.toggle("dark-mode", dark), dark);
        for (const width of [1100, 390]) {
            await page.setViewportSize({ width, height: 1500 });
            if (process.env.TYPOGRAPHY_SCREENSHOTS) {
                await mkdir(process.env.TYPOGRAPHY_SCREENSHOTS, { recursive: true });
                await page.screenshot({ path: path.join(process.env.TYPOGRAPHY_SCREENSHOTS, `${dark ? "dark" : "light"}-${width}.png`), fullPage: true });
            }
            const metrics = await page.evaluate(() => {
                const root = document.querySelector("#answer .md"), p = root.querySelector("p"), code = p.querySelector("code");
                const css = element => getComputedStyle(element);
                return { font: parseFloat(css(root).fontSize), line: parseFloat(css(root).lineHeight),
                    paragraph: parseFloat(css(p).marginBottom), codeBorder: css(code).borderWidth, codeShadow: css(code).boxShadow,
                    codeFont: parseFloat(css(code).fontSize), column: document.querySelector(".transcript-messages").getBoundingClientRect().width,
                    heading: parseFloat(css(root.querySelector("h2")).marginTop),
                    overflow: document.documentElement.scrollWidth > innerWidth,
                    compactGap: parseFloat(css(document.querySelector("#compact p")).marginBottom) };
            });
            if (process.env.TYPOGRAPHY_BASELINE) { console.log({ dark, width, metrics }); continue; }
            assert.ok(metrics.line / metrics.font >= 1.8, "conversation prose needs a relaxed line height");
            assert.ok(metrics.paragraph >= 14, "paragraph separation must be greater than the old half-em gap");
            assert.equal(metrics.codeBorder, "0px", "inline code must not look like outlined controls");
            assert.equal(metrics.codeShadow, "none", "remove the inherited second outline");
            assert.ok(metrics.codeFont <= metrics.font, "inline code cannot dominate body text");
            assert.ok(metrics.column <= 768, "bound wide reading lines");
            assert.ok(metrics.heading >= 24, "separate sections from preceding prose");
            assert.equal(metrics.overflow, false, "long code and tables must not overflow the viewport");
            assert.ok(metrics.compactGap < metrics.paragraph, "working notes retain compact typography");
        }
    }
    if (!process.env.TYPOGRAPHY_BASELINE) {
        const codeCopy = page.locator("#answer .not-prose button").first();
        await codeCopy.focus();
        await page.keyboard.press("Enter");
        assert.equal(await page.evaluate(() => window.copiedText), "Unit tests       succeeded\nStatic analysis  succeeded\nHuman review     pending");
        await page.locator("#answer .message-meta button").first().click();
        assert.equal(await page.evaluate(() => window.copiedText), markdown, "copy keeps the original markdown");
        assert.equal(await page.locator("#plain [data-selection-surface]").innerText(), "Plain text stays plain.\n第二行保留原始换行。");
        assert.ok(await page.locator("#answer [data-md-start]").count() > 0, "source mapping survives presentation changes");
        const sampleRoles = () => page.evaluate(() => {
            const style = selector => {
                const element = document.querySelector(selector), css = getComputedStyle(element);
                return { size: parseFloat(css.fontSize), family: css.fontFamily, line: parseFloat(css.lineHeight),
                    margin: parseFloat(css.marginBottom), padding: css.padding };
            };
            return Object.fromEntries(Object.entries({
                answer: "#answer .md", plain: "#plain .conversation-text", user: ".message-user-body",
                compact: "#compact .md", compactParagraph: "#compact p", inline: "#answer p code",
                heading: "#answer h2", page: "#roles .workbench-page-header h1", section: "#roles h2.text-sm",
                dialog: "#roles [data-dialog-surface] h2", profile: ".profile-reading .md", profileHeading: ".profile-reading h1",
                profileEditor: ".profile-editor", composer: "#roles .cm-scroller", composerHeading: "#roles .cm-md-h2", composerCode: "#roles .cm-md-code-line", code: "#answer pre",
                codeMeta: "#answer .not-prose .u-meta", source: "#roles .source-code", diff: "#roles .review-diff-code",
                sourceMeta: "#roles .source-file-meta", diffMeta: "#roles .review-diff-table thead", meta: "#answer .message-meta",
                reader: "#reader .md", readerHeading: "#reader h2", readerInline: "#reader p code", smallHeading: "#reader h5", tool: "#tool-output pre", nav: "#roles .app-nav-item",
                token: "#ui-token", embedded: "#embedded-ui .u-meta", excluded: "#embedded-ui p",
                excludedHeading: "#embedded-ui h2", excludedCode: "#embedded-ui code", paragraph: "#answer p",
            }).map(([role, selector]) => [role, style(selector)]));
        });
        const near = (actual, expected, message) => assert.ok(Math.abs(actual - expected) < 0.03, `${message}: ${actual} vs ${expected}`);
        const defaults = await sampleRoles();
        near(defaults.answer.size, 16, "default reading size");
        near(defaults.answer.line, 29.6, "default reading rhythm");
        near(defaults.paragraph.margin, 16, "default paragraph spacing");
        near(defaults.compact.size, 14, "compact notes keep their default size");
        near(defaults.compactParagraph.margin, 7, "compact notes keep their default spacing");
        for (const [ui, reading, code, heading] of [[14,16,13,1], [18,16,13,1], [14,20,13,1], [14,16,17,1], [14,16,13,0.9], [14,16,13,1.15], [18,20,17,1.15], [12,12,11,0.9], [18,24,22,1.15], [12,12,22,1.15]]) {
            await page.evaluate(({ ui, reading, code, heading }) => {
                for (const [name, value] of Object.entries({ "--ui-font-size": `${ui}px`, "--reading-font-size": `${reading}px`,
                    "--code-font-size": `${code}px`, "--heading-scale": String(heading), "--font-body": "Arial",
                    "--font-reading": "Georgia", "--font-display": '"Times New Roman"', "--font-mono": '"Courier New"' })) {
                    document.documentElement.style.setProperty(name, value);
                }
            }, { ui, reading, code, heading });
            for (const width of [1100, 390]) {
                await page.setViewportSize({ width, height: 1500 });
                const roles = await sampleRoles();
                for (const role of ["answer", "plain", "user", "profile", "profileEditor", "composer", "reader"]) {
                    near(roles[role].size, reading, `${role} follows reading size`);
                    assert.ok(roles[role].family.includes("Georgia"), `${role} follows reading font`);
                }
                for (const role of ["code", "source", "diff", "tool", "composerCode"]) {
                    near(roles[role].size, code, `${role} follows code size`);
                    assert.ok(roles[role].family.includes("Courier New"), `${role} follows mono font`);
                }
                for (const [role, base] of [["meta",11], ["codeMeta",12], ["sourceMeta",12], ["diffMeta",11], ["nav",13], ["token",12], ["embedded",12]]) {
                    near(roles[role].size, ui * base / 14, `${role} follows UI size independently`);
                    assert.ok(roles[role].family.includes("Arial"), `${role} follows UI font`);
                }
                for (const [role, size] of [["page",ui * 20/14], ["section",ui], ["dialog",ui * 16/14], ["heading",reading * 1.25], ["profileHeading",reading * 1.5], ["readerHeading",reading * 1.5], ["smallHeading",reading], ["composerHeading",reading * 17/16]]) {
                    near(roles[role].size, size * heading, `${role} follows heading scale once`);
                    assert.ok(roles[role].family.includes("Times New Roman"), `${role} follows display font`);
                }
                near(roles.composerCode.line, code * 22/13, "composer code line-height is independent of reading size");
                near(roles.compact.size, reading * 14/16, "compact reading size");
                near(roles.inline.size, reading * 0.9, "inline code stays relative to reading text");
                near(roles.readerInline.size, reading * 0.9, "reader inline code stays relative");
                for (const role of ["excluded", "excludedHeading", "excludedCode"]) {
                    near(roles[role].size, ui, `${role} excludes nested prose typography`);
                }
                near(roles.excluded.margin, 0, "nested not-prose paragraphs exclude prose margins");
                near(roles.answer.line / reading, 1.85, "reading line-height remains unchanged");
                assert.equal(roles.nav.padding, defaults.nav.padding, "UI font changes do not scale layout padding");
                assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, "appearance settings do not overflow the viewport");
            }
        }
        // Geometry, not just computed font sizes: enlarged text must stay readable.
        const layoutFailures = [];
        const checkLayout = async (name, run) => {
            try { await run(); console.log(`PASS ${name}`); }
            catch (error) { layoutFailures.push(`${name}: ${error.message}`); }
        };
        const layoutScreenshot = async (name, locator) => {
            if (process.env.TYPOGRAPHY_SCREENSHOTS)
                await locator.screenshot({ path: path.join(process.env.TYPOGRAPHY_SCREENSHOTS, `${name}.png`) });
        };
        await page.evaluate(() => {
            for (const [key, value] of Object.entries({
                "--ui-font-size": "18px", "--code-font-size": "22px",
                "--font-body": "Arial", "--font-mono": '"Courier New"',
            })) document.documentElement.style.setProperty(key, value);
            window.setSourceLines(1000);
        });
        await page.locator('#source-fixture [data-line="1000"]').waitFor({ state: "attached" });
        await page.setViewportSize({ width: 390, height: 1500 });
        const sourceGeometry = () => page.locator('#source-fixture [data-line="1000"]').evaluate(number => {
            const range = document.createRange();
            range.selectNodeContents(number);
            const text = range.getBoundingClientRect(), box = number.getBoundingClientRect();
            const code = number.nextElementSibling.getBoundingClientRect();
            return { width: box.width, left: box.left, right: text.right, textLeft: text.left,
                codeLeft: code.left, gap: code.left - text.right, padding: parseFloat(getComputedStyle(number).paddingRight) };
        });
        await checkLayout("1000th source line fits at code 22 / narrow", async () => {
            for (const wrap of [false, true]) {
                if (wrap) await page.locator("#source-fixture").getByRole("button", { name: "Wrap lines", exact: true }).click();
                const m = await sourceGeometry();
                assert.ok(m.textLeft >= m.left - 0.1 && m.gap >= m.padding - 0.1,
                    `line number must preserve its code gap (wrap=${wrap}): ${JSON.stringify(m)}`);
            }
            await page.locator('#source-fixture [data-line="1000"]').scrollIntoViewIfNeeded();
            await layoutScreenshot("source-code22-390", page.locator("#source-fixture"));
            await page.locator("#source-fixture").getByRole("button", { name: "Wrap lines", exact: true }).click();
        });
        await checkLayout("source gutter reserves total digits before pagination", async () => {
            await page.evaluate(() => window.setSourceLines(100000));
            await page.waitForFunction(() => document.querySelector(".source-file-meta").textContent.includes("100000"));
            const before = await sourceGeometry();
            const digitWidth = await page.locator(".source-code").evaluate(element => {
                const canvas = document.createElement("canvas"), context = canvas.getContext("2d"), css = getComputedStyle(element);
                context.font = `${css.fontSize} ${css.fontFamily}`;
                return context.measureText("100000").width;
            });
            assert.ok(before.width >= digitWidth + before.padding - 0.1,
                `gutter must reserve all six digits, not just the rendered page: ${before.width}`);
            await page.locator(".source-more button").click();
            await page.locator('#source-fixture [data-line="2000"]').waitFor({ state: "attached" });
            near((await sourceGeometry()).width, before.width, "pagination must not move the code column");
        });
        await checkLayout("default source gutter widths stay unchanged", async () => {
            await page.evaluate(() => {
                window.setSourceLines(1000);
                document.documentElement.style.setProperty("--code-font-size", "13px");
            });
            for (const [width, gutter] of [[1100, 64], [390, 44]]) {
                await page.setViewportSize({ width, height: 1500 });
                near((await sourceGeometry()).width, gutter, "default gutter");
            }
        });
        await checkLayout("console status preserves two complete lines at UI 18", async () => {
            await page.setViewportSize({ width: 1100, height: 1500 });
            for (const ui of [14, 18]) {
                await page.evaluate(ui => document.documentElement.style.setProperty("--ui-font-size", `${ui}px`), ui);
                const m = await page.locator("#status-fixture").evaluate(element => {
                    const css = getComputedStyle(element), box = element.getBoundingClientRect(), range = document.createRange();
                    range.selectNodeContents(element);
                    return { height: box.height, line: parseFloat(css.lineHeight), bottom: box.bottom,
                        textBottom: Math.max(...Array.from(range.getClientRects(), rect => rect.bottom)) };
                });
                near(m.height, 2 * m.line, `UI ${ui}: status must allow two line boxes`);
                assert.ok(m.textBottom <= m.bottom + 0.1, `UI ${ui}: status glyphs must not be clipped`);
            }
            await layoutScreenshot("console-status-ui18", page.locator("#status-fixture"));
        });
        await checkLayout("actual English selection actions fit at UI 18 and narrow widths", async () => {
            // Hold the source pane in view so document scrolling cannot dismiss
            // the actual selection toolbar while we inspect its geometry.
            await page.locator("#source-fixture").evaluate(element => {
                Object.assign(element.style, { position: "fixed", inset: "160px 16px auto", zIndex: "100" });
                element.querySelector(".source-scroll").scrollTop = 0;
            });
            for (const [width, ui] of [[1100, 14], [390, 18], [320, 18]]) {
                await page.setViewportSize({ width, height: 1500 });
                await page.evaluate(ui => document.documentElement.style.setProperty("--ui-font-size", `${ui}px`), ui);
                await page.locator('#source-fixture [data-line="1"]').click();
                const toolbar = page.getByRole("toolbar", { name: "Selection actions" });
                await toolbar.waitFor();
                const m = await toolbar.evaluate(element => {
                    const box = element.getBoundingClientRect();
                    return { left: box.left, right: box.right, bottom: box.bottom, client: element.clientWidth, scroll: element.scrollWidth,
                        viewport: innerWidth, buttons: Array.from(element.querySelectorAll("button"), button => {
                            const r = button.getBoundingClientRect();
                            return { label: button.textContent, left: r.left, right: r.right, top: r.top, bottom: r.bottom };
                        }) };
                });
                assert.equal(m.buttons.length, 4, "all actual selection actions are present");
                assert.ok(m.scroll <= m.client + 1, `toolbar content must fit: ${JSON.stringify(m)}`);
                for (const b of m.buttons)
                    assert.ok(b.left >= m.left && b.right <= m.right && b.right <= m.viewport && b.bottom <= m.bottom,
                        `${b.label} must stay inside toolbar and viewport: ${JSON.stringify(m)}`);
                if (ui === 14) assert.equal(new Set(m.buttons.map(b => b.top)).size, 1, "default toolbar stays one row");
                await layoutScreenshot(`selection-ui${ui}-${width}`, toolbar);
                // The last action must remain keyboard-reachable after wrapping.
                await toolbar.getByRole("button", { name: "Add to chat", exact: true }).focus();
                await page.keyboard.press("End");
                assert.equal(await toolbar.getByRole("button", { name: "Dismiss selection actions" }).evaluate(element => element === document.activeElement), true);
                await page.keyboard.press("Escape");
                await toolbar.waitFor({ state: "detached" });
            }
        });
        assert.deepEqual(errors, []);
        assert.deepEqual(layoutFailures, [], "Typography layout regressions");
        console.log("Conversation typography: dark/light, wide/narrow, hierarchy, quiet code, compact isolation, independent appearance axes and copy PASS");
    }
} finally {
    await browser?.close();
    if (server) await new Promise(resolve => server.close(resolve));
    await rm(scratch, { recursive: true, force: true });
}
