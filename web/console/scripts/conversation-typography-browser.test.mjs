// Real shared message components and production CSS, isolated synthetic content.
import assert from "node:assert/strict";
import { mkdtemp, writeFile, readFile, rm, mkdir } from "node:fs/promises";
import http from "node:http";
import path from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { build } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";

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
        import { useRef, useState } from "react";
        import { PageHeader, Panel } from "@/components/steve/page";
        import { DialogHeader } from "@/components/steve/dialog-surface";
        import { SourceView } from "@/components/steve/source-view";
        import { DiffView } from "@/components/steve/diff-view";
        import { CodeBlock } from "@/components/steve/code-block";
        import { MarkdownInput } from "@/components/steve/markdown-input";
        import { AssistantMessage, UserMessage } from "@/components/steve/message";
        import { Md } from "@/components/steve/markdown";
        import { LocaleProvider } from "@/providers/locale-provider";
        import "./style.css";
        const text = ${JSON.stringify(markdown)};
        const reply = { id:"fixture", conversation:"console:fixture", kind:"reply", text, at:"2026-09-21T07:16:29Z" };
        function RoleFixtures() {
            const [draft, setDraft] = useState(${JSON.stringify("Composer reading text\n\n## Composer heading\n\n```text\ncomposer code\n```")});
            const handle = useRef(null);
            return <section id="roles">
                <PageHeader title="Page heading" /><Panel title="Section heading">UI body</Panel>
                <div data-dialog-surface><DialogHeader title="Dialog heading" /></div>
                <div className="profile-reading"><Md text={${JSON.stringify("# Profile heading\n\nProfile reading text")}} /></div>
                <textarea className="profile-editor font-mono text-sm" defaultValue="Profile draft" />
                <MarkdownInput value={draft} onChange={setDraft} label="Draft" placeholder="Write" handle={handle} />
                <SourceView file={{ path:"fixture.txt", text:"source text", size:11 }} />
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
        root: scratch, configFile: path.join(web, "vite.config.ts"), envDir: false,
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
        if (new URL(route.request().url()).origin === origin) return route.continue();
        errors.push("Unexpected external request"); return route.abort();
    });
    await page.addInitScript(() => {
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
        assert.deepEqual(errors, []);
        console.log("Conversation typography: dark/light, wide/narrow, hierarchy, quiet code, compact isolation, independent appearance axes and copy PASS");
    }
} finally {
    await browser?.close();
    if (server) await new Promise(resolve => server.close(resolve));
    await rm(scratch, { recursive: true, force: true });
}
