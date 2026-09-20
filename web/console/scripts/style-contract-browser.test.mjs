// Shared presentation must be determined by its variant, not by its ancestors.
// Compile the real components and CSS; no app data, config or Hub is used.
import assert from "node:assert/strict";
import { mkdtemp, writeFile, readFile, rm, mkdir } from "node:fs/promises";
import http from "node:http";
import path from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { build } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";

const web = fileURLToPath(new URL("..", import.meta.url));
const scratch = await mkdtemp(path.join(tmpdir(), "steve-style-contract-"));
let server, browser;
try {
    await writeFile(path.join(scratch, "index.html"), '<div id="root"></div><script type="module" src="/entry.tsx"></script>');
    // The fixture lives outside the source tree; explicitly scan the real
    // sources so the test cannot pass with missing Tailwind utilities.
    await writeFile(path.join(scratch, "style.css"), `@import "${web}/src/styles/globals.css";\n@source "${web}/src";\n@source "${scratch}/entry.tsx";`);
    await writeFile(path.join(scratch, "entry.tsx"), `
        import { createRoot } from "react-dom/client";
        import { useState } from "react";
        import { Panel } from "@/components/steve/page";
        import { Drawer } from "@/components/steve/drawer";
        import { Button } from "@/components/base/buttons/button";
        import { LocaleProvider } from "@/providers/locale-provider";
        import { CloseStackProvider } from "@/providers/close-stack";
        import "./style.css";
        const contexts = ["ordinary", "inspector", "skills"];
        function Detail() {
            const [open, setOpen] = useState(false);
            return <><Button onClick={() => setOpen(true)}>Open detail</Button>{open &&
                <Drawer title="Named detail" badges={<span>Ready</span>} subtitle="Context and description" onClose={() => setOpen(false)}>
                    <Button color="secondary">Inside detail</Button>
                </Drawer>}</>;
        }
        createRoot(document.getElementById("root")).render(<LocaleProvider><CloseStackProvider><main className="grid min-w-0 gap-6 p-4">
            {["card", "section"].map(variant => <div key={variant} className="grid min-w-0 gap-4 sm:grid-cols-3">
                {contexts.map(context => <div key={context} id={variant + "-" + context} className={context === "inspector" ? "inspector-body" : ""}>
                    <Panel variant={variant} className={context === "skills" ? "skill-settings-panel" : undefined}
                        title="一致的区域 / Same section" description="Same variant, same presentation."
                        aside={<Button size="sm" color="secondary" onClick={() => window.actions = (window.actions || 0) + 1}>Action</Button>}>
                        <p>Content retained in every context.</p>
                    </Panel>
                </div>)}
            </div>)}
            <div id="flush"><Panel title="List" padding="flush" toolbar={<span>Filter controls</span>} footer={<Button color="secondary">Next page</Button>}>
                <ul><li className="px-4 py-3">First item</li><li className="px-4 py-3">Second item</li></ul>
            </Panel></div>
            <Panel title={"很长的中文标题 / long-title-without-spaces".repeat(5)}
                aside={<Button color="secondary">A long action label</Button>}>
                <p>Long titles must not push actions outside the viewport.</p>
            </Panel>
            <Detail />
        </main></CloseStackProvider></LocaleProvider>);
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
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    const origin = `http://127.0.0.1:${server.address().port}`;
    browser = await chromium.launch({ headless: true });
    const page = await browser.newPage({ viewport: { width: 1440, height: 1000 }, reducedMotion: "reduce" });
    const errors = [];
    page.on("pageerror", (error) => errors.push(String(error)));
    await page.route("**/*", (route) => {
        if (new URL(route.request().url()).origin === origin) return route.continue();
        errors.push("Unexpected external request"); return route.abort();
    });
    await page.goto(origin);
    await page.locator("#card-ordinary section").waitFor();
    const properties = ["borderRadius", "borderWidth", "borderColor", "backgroundColor", "boxShadow", "padding", "gap", "fontSize", "fontWeight", "color"];
    const computed = (locator) => locator.evaluate((element, properties) => {
        const style = getComputedStyle(element);
        return Object.fromEntries(properties.map((key) => [key, style[key]]));
    }, properties);
    for (const theme of ["light", "dark", "nord"]) {
        await page.evaluate((theme) => {
            document.documentElement.classList.toggle("dark-mode", theme !== "light");
            if (theme === "nord") document.documentElement.dataset.theme = "nord";
            else delete document.documentElement.dataset.theme;
        }, theme);
        for (const variant of ["card", "section"]) {
            for (const part of ["section", "header", "h2", "section > div"]) {
                const expected = await computed(page.locator(`#${variant}-ordinary ${part}`));
                for (const context of ["inspector", "skills"]) {
                    assert.deepEqual(await computed(page.locator(`#${variant}-${context} ${part}`)), expected, `${theme}: ${variant} ${part} must not be restyled by ${context}`);
                }
            }
        }
        const card = await computed(page.locator("#card-ordinary section"));
        assert.equal(card.borderRadius, "8px", "real Tailwind utilities must be generated");
        assert.equal(card.borderWidth, "1px", "card boundaries must not disappear through a shadow override");
        const section = await computed(page.locator("#section-ordinary section"));
        assert.equal(section.borderRadius, "0px");
        assert.equal(section.borderWidth, "0px 0px 1px", "the explicit section variant keeps its divider outside the rail too");
        assert.equal((await computed(page.locator("#flush .workbench-panel-body"))).padding, "0px");
        assert.equal(await page.locator("#flush li").count(), 2);
        await page.getByText("Filter controls", { exact: true }).waitFor();
        assert.equal(await page.locator("#flush footer").getByRole("button", { name: "Next page" }).count(), 1);
        for (const width of [1440, 390]) {
            await page.setViewportSize({ width, height: 1000 });
            assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${theme}/${width}: titles and actions stay within the viewport`);
            const action = page.locator("#card-ordinary").getByRole("button", { name: "Action", exact: true });
            await action.focus(); await action.press("Enter");
            assert.equal(await action.evaluate((element) => element === document.activeElement), true);
            if (process.env.STYLE_SCREENSHOT_DIR) {
                await mkdir(process.env.STYLE_SCREENSHOT_DIR, { recursive: true });
                await page.screenshot({ path: path.join(process.env.STYLE_SCREENSHOT_DIR, `panels-${theme}-${width}.png`), fullPage: true });
            }
        }
    }
    assert.equal(await page.evaluate(() => window.actions), 6);
    const trigger = page.getByRole("button", { name: "Open detail", exact: true });
    await trigger.click();
    const dialog = page.getByRole("dialog", { name: "Named detail", exact: true });
    await dialog.waitFor();
    assert.equal(await dialog.getByRole("heading", { name: "Named detail", exact: true, level: 2 }).count(), 1, "Drawer owns its heading, including simple loading titles");
    assert.equal(await dialog.getByText("Ready", { exact: true }).count(), 1, "badges are separate from the heading");
    await dialog.getByRole("button", { name: "Inside detail", exact: true }).focus();
    await page.keyboard.press("Tab");
    assert.equal(await dialog.evaluate((element) => element.contains(document.activeElement)), true, "focus remains in the drawer");
    await page.keyboard.press("Escape");
    await dialog.waitFor({ state: "hidden" });
    await page.waitForFunction(() => document.activeElement?.textContent === "Open detail");
    assert.deepEqual(errors, []);
    console.log("PASS explicit panel variants, ancestor isolation, real borders, flush lists, toolbar/footer, themes, named drawer headings and narrow keyboard layout");
} finally {
    await browser?.close();
    if (server) await new Promise((resolve) => server.close(resolve));
    await rm(scratch, { recursive: true, force: true });
}
