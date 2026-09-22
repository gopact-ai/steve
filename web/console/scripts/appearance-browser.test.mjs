// Isolated real components + production CSS/inline bootstrap. No live hub.
import assert from "node:assert/strict";
import { mkdtemp, writeFile, readFile, rm, mkdir } from "node:fs/promises";
import http from "node:http";
import path from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { build } from "../node_modules/vite/dist/node/index.js";
import { chromium } from "../node_modules/playwright/index.mjs";
import {
  defaultAppearance,
  APPEARANCE_KEY,
  exportPalette,
  importPalette,
} from "../src/lib/appearance.ts";
import { PALETTES } from "../src/lib/themes.ts";
const web = fileURLToPath(new URL("..", import.meta.url));
const scratch = await mkdtemp(path.join(tmpdir(), "steve-appearance-"));
let server, browser;
try {
  await writeFile(
    path.join(scratch, "index.html"),
    '<html><head><meta charset="utf-8"><meta name="theme-color" content="#fff"><!-- appearance-boot --></head><body><div id="root"></div><script type="module" src="/entry.tsx"></script></body></html>',
  );
  await writeFile(
    path.join(scratch, "style.css"),
    `@import "${web}/src/styles/globals.css";\n@import "${web}/src/styles/settings.css";\n@source "${web}/src";`,
  );
  await writeFile(
    path.join(scratch, "entry.tsx"),
    `
 import {createRoot} from 'react-dom/client';
 import {HashRouter, useLocation, useNavigate} from 'react-router';
 import {installBackNavigationGuard, registerBackNavigationGuard} from '@/lib/navigation-guard';
 installBackNavigationGuard();
 window.registerBackNavigationGuard=registerBackNavigationGuard;
 import {LocaleProvider} from '@/providers/locale-provider';
 import {ThemeProvider,useTheme} from '@/providers/theme-provider';
 import {SettingsPage} from '@/pages/settings';
 import {ThemeMenu} from '@/components/steve/theme-menu';
 import {Md} from '@/components/steve/markdown';
 import './style.css';
 function Fixture(){const location=useLocation(),navigate=useNavigate();const value=useTheme();window.appearance=value.appearance;window.changeAppearance=value.updateAppearance;
 return <><a href="#main-content" onClick={e=>{e.preventDefault();document.querySelector('#role-fixture').focus();}}>跳到内容</a><a href="#/other">原生路由链接</a><button onClick={()=>navigate('/settings?section=appearance')}>前往外观</button><header style={{padding:16,display:'flex',justifyContent:'flex-end'}}><ThemeMenu/></header>{location.pathname==='/settings'?<SettingsPage/>:<main id="main-content" tabIndex={-1}>其他页面</main>}<section id="role-fixture" style={{padding:24}}><div className="u-meta">辅助信息</div><Md variant="conversation" text={${JSON.stringify("## 标题\n\n正文内容 `inline_code`\n\n```go\nvar answer = 42\n```")}}/></section></>;}
 createRoot(document.getElementById('root')).render(<LocaleProvider><ThemeProvider><HashRouter><Fixture/></HashRouter></ThemeProvider></LocaleProvider>);
 `,
  );
  await build({
    root: scratch,
    configFile: path.join(web, "vite.config.ts"),
    envDir: false,
    cacheDir: path.join(scratch, "cache"),
    logLevel: "error",
    resolve: {
      alias: {
        "@": path.join(web, "src"),
        react: path.join(web, "node_modules/react"),
        "react-dom": path.join(web, "node_modules/react-dom"),
        "react-router": path.join(web, "node_modules/react-router"),
      },
    },
    build: { outDir: path.join(scratch, "dist"), emptyOutDir: true },
  });
  server = http.createServer(async (req, res) => {
    try {
      assert.equal(req.method, "GET");
      const file = path.resolve(
        scratch,
        "dist",
        "." + (req.url === "/" ? "/index.html" : req.url),
      );
      assert.ok(file.startsWith(path.join(scratch, "dist") + path.sep));
      res.setHeader(
        "Content-Type",
        file.endsWith(".js")
          ? "text/javascript"
          : file.endsWith(".css")
            ? "text/css"
            : "text/html",
      );
      res.end(await readFile(file));
    } catch {
      res.writeHead(404);
      res.end();
    }
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  const origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({
    viewport: { width: 1200, height: 1000 },
    colorScheme: "light",
    reducedMotion: "reduce",
  });
  const errors = [],
    writes = [],
    reads = [];
  await context.route("**/*", (route) => {
    const req = route.request(),
      url = new URL(req.url());
    if (url.origin !== origin) {
      errors.push("external request");
      return route.abort();
    }
    if (url.pathname.startsWith("/console/") || url.pathname === "/state") {
      if (req.method() !== "GET")
        writes.push(req.method() + " " + url.pathname);
      else reads.push(url.pathname);
      return route.fulfill({
        status: 503,
        json: { error: "Isolated offline fixture" },
      });
    }
    return route.continue();
  });
  await context.addInitScript(() => {
    localStorage.setItem("steve.ui.locale", "zh");
    window.addEventListener(
      "storage",
      (event) => {
        if (window.pauseAppearanceStorage) event.stopImmediatePropagation();
      },
      true,
    );
  });
  const page = await context.newPage();
  page.on("pageerror", (e) => errors.push(String(e)));
  await page.goto(origin + "/#/settings?section=appearance");
  await page.getByRole("heading", { name: "外观", exact: true }).waitFor();
  // Context updates exercise bounded model rendering and independent modes.
  await page.evaluate(() =>
    window.changeAppearance((a) => ({
      ...a,
      light: "one-light",
      dark: "nord",
      mode: "system",
    })),
  );
  await page.waitForFunction(
    () => document.documentElement.dataset.theme === "one-light",
  );
  await page.emulateMedia({ colorScheme: "dark" });
  await page.waitForFunction(
    () => document.documentElement.dataset.theme === "nord",
  );
  await page.emulateMedia({ colorScheme: "light" });
  await page.waitForFunction(
    () => document.documentElement.dataset.theme === "one-light",
  );
  await page.getByRole("button", { name: /^主题/ }).click();
  await page
    .getByRole("menuitemradio", { name: "Dracula", exact: true })
    .click();
  await page.waitForFunction(
    () => document.documentElement.dataset.theme === "dracula",
  );
  assert.equal(await page.evaluate(() => window.appearance.light), "one-light");
  assert.equal(await page.evaluate(() => window.appearance.mode), "dark");
  await page.reload();
  await page.getByRole("heading", { name: "外观", exact: true }).waitFor();
  assert.equal(
    await page.evaluate(() => document.documentElement.dataset.theme),
    "dracula",
  );
  // A second window opened from the first shares its renderer, so both read
  // one localStorage cache. Pages created separately live in separate
  // processes whose caches converge asynchronously; a lock cannot order
  // that, so a truly simultaneous edit there may still read a stale value.
  const [second] = await Promise.all([
    context.waitForEvent("page"),
    page.evaluate((url) => {
      window.open(url);
    }, origin + "/#/settings?section=appearance"),
  ]);
  await second.getByRole("heading", { name: "外观", exact: true }).waitFor();
  await page.evaluate(() =>
    window.changeAppearance((a) => ({
      ...a,
      fonts: {
        ...a.fonts,
        reading: { family: "serif", size: 22 },
        heading: { family: "serif", scale: 1.15 },
        code: { family: "mono", size: 18 },
        ui: { family: "system", size: 16 },
      },
    })),
  );
  await second.waitForFunction(
    () => window.appearance?.fonts.reading.size === 22,
  );
  const metrics = await page.locator("#role-fixture").evaluate((el) => {
    const css = (e) => getComputedStyle(e),
      md = el.querySelector(".md");
    return {
      font: css(md).fontSize,
      family: css(md).fontFamily,
      heading: css(md.querySelector("h2")).fontSize,
      code: css(md.querySelector("pre")).fontSize,
      meta: css(el.querySelector(".u-meta")).fontSize,
    };
  });
  assert.equal(metrics.font, "22px");
  assert.equal(metrics.code, "18px");
  assert.ok(parseFloat(metrics.heading) > 22);
  assert.ok(parseFloat(metrics.meta) < 16);
  assert.match(metrics.family, /serif/);
  // New page blocks the React module: persisted preferences must paint before boot.
  const boot = await context.newPage();
  await boot.route("**/*.js", (r) => r.abort());
  await boot.goto(origin, { waitUntil: "domcontentloaded" });
  assert.equal(
    await boot.evaluate(() => document.documentElement.dataset.theme),
    "dracula",
  );
  assert.equal(
    await boot.evaluate(() =>
      document.documentElement.style.getPropertyValue("--reading-font-size"),
    ),
    "22px",
  );
  await boot.close();
  // A suspended window may not receive storage events before its next edit.
  await second.evaluate(() => {
    window.pauseAppearanceStorage = true;
  });
  await page.evaluate(() =>
    window.changeAppearance((a) => ({
      ...a,
      fonts: { ...a.fonts, code: { ...a.fonts.code, size: 21 } },
    })),
  );
  assert.notEqual(
    await second.evaluate(() => window.appearance.fonts.code.size),
    21,
  );
  await second.evaluate(() =>
    window.changeAppearance((a) => ({ ...a, mode: "dark" })),
  );
  assert.equal(
    await second.evaluate(
      () =>
        JSON.parse(localStorage.getItem("steve.ui.appearance")).fonts.code.size,
    ),
    21,
    "mode edit must rebase onto another window persisted code size",
  );
  await second.evaluate(() => {
    window.pauseAppearanceStorage = false;
  });
  await Promise.all([
    page.evaluate(() =>
      window.changeAppearance((a) => ({
        ...a,
        fonts: { ...a.fonts, ui: { ...a.fonts.ui, size: 17 } },
      })),
    ),
    second.evaluate(() =>
      window.changeAppearance((a) => ({
        ...a,
        fonts: { ...a.fonts, reading: { ...a.fonts.reading, size: 23 } },
      })),
    ),
  ]);
  const parallelFonts = await page.evaluate(
    () => JSON.parse(localStorage.getItem("steve.ui.appearance")).fonts,
  );
  assert.equal(parallelFonts.ui.size, 17);
  assert.equal(
    parallelFonts.reading.size,
    23,
    "simultaneous independent edits are serialized, not last-snapshot wins",
  );
  // Local font input applies only validated names, and reset clears the editor.
  await page.getByRole("button", { name: /阅读正文字体/ }).click();
  await page
    .getByRole("option", { name: "本地字体名称…", exact: true })
    .click();
  const localFont = page.getByRole("textbox", {
    name: "阅读正文本地字体名称",
    exact: true,
  });
  await localFont.fill("bad; color:red");
  assert.equal(
    await page
      .getByRole("button", { name: "应用名称", exact: true })
      .isDisabled(),
    true,
  );
  await localFont.fill("Hiragino Sans GB");
  await localFont.press("Enter");
  await second.waitForFunction(
    () => window.appearance.fonts.reading.family === "Hiragino Sans GB",
  );
  assert.match(
    await page
      .locator("#role-fixture .md")
      .evaluate((el) => getComputedStyle(el).fontFamily),
    /Hiragino Sans GB/,
  );
  await page.getByRole("button", { name: "恢复默认字体", exact: true }).click();
  assert.equal(
    await page
      .getByRole("textbox", { name: "阅读正文本地字体名称", exact: true })
      .count(),
    0,
  );
  // Edit with actual controls, keyboard selection, scoped preview and JSON roundtrip.
  await page.setViewportSize({ width: 1200, height: 1000 });
  await page.getByRole("button", { name: /阅读正文字号/ }).click();
  await page.getByRole("option", { name: "20 px", exact: true }).click();
  assert.equal(
    await page.evaluate(() => window.appearance.fonts.reading.size),
    20,
  );
  await page
    .getByRole("button", { name: "复制并编辑", exact: true })
    .nth(1)
    .click();
  await page
    .getByRole("textbox", { name: "配色名称", exact: true })
    .fill("自定义验收");
  const oldBg = await page.evaluate(() =>
    getComputedStyle(document.documentElement).getPropertyValue(
      "--color-bg-primary",
    ),
  );
  await page
    .getByRole("textbox", { name: /主背景/ })
    .and(page.locator("input:not([type=color])"))
    .fill("#123456");
  assert.equal(
    await page.evaluate(() =>
      getComputedStyle(document.documentElement).getPropertyValue(
        "--color-bg-primary",
      ),
    ),
    oldBg,
    "draft cannot recolour the real workbench",
  );
  assert.equal(
    await page
      .locator("[data-appearance-preview]")
      .evaluate((el) =>
        getComputedStyle(el).getPropertyValue("--seed-bg").trim(),
      ),
    "#123456",
  );
  await page
    .getByRole("textbox", { name: /正文/ })
    .and(page.locator("input:not([type=color])"))
    .fill("#123456");
  await page.getByText(/低对比度提醒/).waitFor();
  await page
    .getByRole("textbox", { name: /主背景/ })
    .and(page.locator("input:not([type=color])"))
    .fill("url(https://bad)");
  assert.equal(
    await page
      .getByRole("button", { name: "保存并应用配色", exact: true })
      .isDisabled(),
    true,
  );
  page.once("dialog", (d) => d.accept());
  await page.getByRole("button", { name: "恢复原配色", exact: true }).click();
  await page
    .getByRole("textbox", { name: "配色名称", exact: true })
    .fill("自定义验收");
  await page
    .getByRole("textbox", { name: /主背景/ })
    .and(page.locator("input:not([type=color])"))
    .fill("#123456");
  await page
    .getByRole("button", { name: "保存并应用配色", exact: true })
    .click();
  await page.waitForFunction(() =>
    window.appearance.custom.some((p) => p.name === "自定义验收"),
  );
  await page.waitForFunction(() =>
    document.documentElement.dataset.theme.startsWith("custom-"),
  );
  const saved = await page.evaluate(() =>
    window.appearance.custom.find((p) => p.name === "自定义验收"),
  );
  assert.equal(saved.colors.bg, "#123456");
  const downloadPromise = page.waitForEvent("download");
  await page.getByRole("button", { name: "导出 JSON", exact: true }).click();
  const download = await downloadPromise;
  const exported = await readFile(await download.path(), "utf8");
  assert.equal(importPalette(exported).colors.bg, "#123456");
  await page.getByRole("button", { name: "编辑", exact: true }).click();
  await page
    .getByRole("textbox", { name: "配色名称", exact: true })
    .fill("不保存的名称");
  page.once("dialog", (d) => d.accept());
  await page.getByRole("button", { name: "取消", exact: true }).click();
  assert.equal(
    await page.evaluate(() => window.appearance.custom[0].name),
    "自定义验收",
  );
  // Import validates before opening an editable draft and never auto-replaces a palette.
  const input = page.locator("input[type=file]");
  await input.setInputFiles({
    name: "invalid.json",
    mimeType: "application/json",
    buffer: Buffer.from("{bad"),
  });
  await page
    .getByRole("alert")
    .filter({ hasText: /无法导入/ })
    .waitFor();
  assert.equal(await page.evaluate(() => window.appearance.custom.length), 1);
  await input.setInputFiles({
    name: "palette.json",
    mimeType: "application/json",
    buffer: Buffer.from(exportPalette({ ...PALETTES[2], name: "导入验收" })),
  });
  await page.getByRole("textbox", { name: "配色名称", exact: true }).waitFor();
  assert.equal(await page.evaluate(() => window.appearance.custom.length), 1);
  // Another window's font change survives saving this palette draft.
  await second.evaluate(() =>
    window.changeAppearance((a) => ({
      ...a,
      fonts: { ...a.fonts, code: { ...a.fonts.code, size: 19 } },
    })),
  );
  await page.waitForFunction(() => window.appearance.fonts.code.size === 19);
  await page
    .getByRole("button", { name: "保存并应用配色", exact: true })
    .click();
  await page.waitForFunction(() => window.appearance.custom.length === 2);
  assert.equal(
    await page.evaluate(() => window.appearance.fonts.code.size),
    19,
  );
  assert.equal(
    await page.evaluate(() => window.appearance.mode),
    "dark",
    "saving a light palette must not change display mode",
  );
  await page.getByRole("button", { name: "删除", exact: true }).click();
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "删除", exact: true })
    .click();
  await page.waitForFunction(() => window.appearance.custom.length === 1);
  assert.equal(await page.evaluate(() => window.appearance.light), "light");
  await page.getByRole("button", { name: "恢复默认字体", exact: true }).click();
  assert.equal(
    await page.evaluate(() => window.appearance.fonts.reading.size),
    16,
  );
  // Keyboard navigation reaches the same appearance page via toolbar shortcut.
  await page.getByRole("button", { name: /^主题/ }).click();
  await page.getByRole("menuitem", { name: "外观", exact: true }).focus();
  await page.keyboard.press("Enter");
  await page.getByRole("heading", { name: "外观", exact: true }).waitFor();

  const custom = {
    ...PALETTES[0],
    id: "custom-browser",
    name: "示例自定义",
    colors: { ...PALETTES[0].colors, bg: "#17252b" },
  };
  await page.evaluate(
    (custom) =>
      window.changeAppearance((a) => ({
        ...a,
        mode: "dark",
        dark: custom.id,
        custom: [custom],
      })),
    custom,
  );
  await page.waitForFunction(
    () =>
      getComputedStyle(document.documentElement)
        .getPropertyValue("--seed-bg")
        .trim() === "#17252b",
  );
  await page.reload();
  await page.getByRole("heading", { name: "外观", exact: true }).waitFor();
  assert.equal(
    await page.evaluate(() => document.documentElement.dataset.theme),
    "custom-browser",
  );
  for (const scheme of ["light", "dark"]) {
    await page.evaluate(
      (s) => window.changeAppearance((a) => ({ ...a, mode: s })),
      scheme,
    );
    await page.waitForFunction(
      (s) =>
        document.documentElement.classList.contains("dark-mode") ===
        (s === "dark"),
      scheme,
    );
    for (const width of [1200, 390]) {
      await page.setViewportSize({ width, height: 1000 });
      assert.equal(
        await page.evaluate(
          () => document.documentElement.scrollWidth > innerWidth,
        ),
        false,
        "appearance settings must fit narrow windows with larger fonts",
      );
      if (process.env.APPEARANCE_SCREENSHOTS) {
        await mkdir(process.env.APPEARANCE_SCREENSHOTS, { recursive: true });
        await page.screenshot({
          path: path.join(
            process.env.APPEARANCE_SCREENSHOTS,
            `${scheme}-${width}.png`,
          ),
          fullPage: true,
          animations: "disabled",
        });
      }
    }
  }
  assert.deepEqual(
    writes,
    [],
    "appearance preferences must not write to a hub",
  );
  assert.deepEqual(
    reads,
    [],
    "appearance page does not need server configuration",
  );
  assert.deepEqual(errors, []);
  // The bootstrap guard must run before HashRouter unmounts the editor.
  const nav = await context.newPage();
  nav.on("pageerror", (e) => errors.push(String(e)));
  await nav.goto(origin + "/#/other");
  await nav.getByText("其他页面", { exact: true }).waitFor();
  assert.deepEqual(
    await nav.evaluate(() => {
      const calls = [];
      const parent = window.registerBackNavigationGuard((event) => {
        calls.push("parent");
        event.stopImmediatePropagation();
      });
      const child = window.registerBackNavigationGuard((event) => {
        calls.push("child");
        event.stopImmediatePropagation();
      }, 1);
      window.dispatchEvent(new PopStateEvent("popstate"));
      child();
      window.dispatchEvent(new PopStateEvent("popstate"));
      parent();
      return calls;
    }),
    ["child", "parent"],
    "child rejection runs first, and releasing it preserves the parent guard",
  );
  await nav.getByRole("button", { name: "前往外观", exact: true }).click();
  await nav
    .getByRole("button", { name: "复制并编辑", exact: true })
    .first()
    .click();
  await nav
    .getByRole("textbox", { name: "配色名称", exact: true })
    .fill("保留返回草稿");
  let confirmations = 0;
  const reject = (dialog) => {
    confirmations++;
    return dialog.dismiss();
  };
  nav.on("dialog", reject);
  await Promise.all([nav.waitForEvent("dialog"), nav.goBack()]);
  assert.equal(
    confirmations,
    1,
    "Back must ask before unmounting an appearance draft",
  );
  assert.equal(
    await nav
      .getByRole("textbox", { name: "配色名称", exact: true })
      .inputValue(),
    "保留返回草稿",
  );
  await nav.getByRole("link", { name: "跳到内容", exact: true }).click();
  assert.equal(
    confirmations,
    1,
    "same-document focus links are not navigation",
  );
  assert.equal(
    await nav.evaluate(() => {
      const event = new Event("beforeunload", { cancelable: true });
      window.dispatchEvent(event);
      return event.defaultPrevented;
    }),
    true,
    "focus link must not disable unload protection",
  );
  nav.off("dialog", reject);
  nav.once("dialog", (dialog) => dialog.accept());
  await nav.goBack();
  await nav.getByText("其他页面", { exact: true }).waitFor();
  await nav.getByRole("button", { name: "前往外观", exact: true }).click();
  await nav
    .getByRole("button", { name: "复制并编辑", exact: true })
    .first()
    .click();
  let nativeConfirmations = 0;
  nav.on("dialog", (dialog) => {
    nativeConfirmations++;
    return dialog.accept();
  });
  await nav.getByRole("link", { name: "原生路由链接", exact: true }).click();
  await nav.getByText("其他页面", { exact: true }).waitFor();
  assert.equal(
    nativeConfirmations,
    1,
    "native hash click and popstate share exactly one confirmation",
  );
  await nav.close();
  assert.deepEqual(errors, []);
  // A queued save must not close an editor that changed while the lock was held.
  await page
    .getByRole("button", { name: "复制并编辑", exact: true })
    .first()
    .click();
  await page
    .getByRole("textbox", { name: "配色名称", exact: true })
    .fill("SAVE SNAPSHOT");
  await page.evaluate((key) => {
    window.saveLockHeld = false;
    window.heldSaveLock = navigator.locks.request(
      key,
      () =>
        new Promise((resolve) => {
          window.releaseSaveLock = resolve;
          window.saveLockHeld = true;
        }),
    );
  }, APPEARANCE_KEY);
  await page.waitForFunction(() => window.saveLockHeld);
  await page
    .getByRole("button", { name: "保存并应用配色", exact: true })
    .click();
  await page
    .getByRole("textbox", { name: "配色名称", exact: true })
    .fill("LATER EDIT");
  await page.evaluate(() => window.releaseSaveLock());
  await page.waitForFunction(() =>
    window.appearance.custom.some((p) => p.name === "SAVE SNAPSHOT"),
  );
  await page.waitForTimeout(100);
  assert.equal(
    await page
      .getByRole("textbox", { name: "配色名称", exact: true })
      .inputValue(),
    "LATER EDIT",
    "an earlier save must not discard later typing",
  );
  await page
    .getByRole("button", { name: "保存并应用配色", exact: true })
    .click();
  await page
    .getByText("配色已保存并选用于对应模式。", { exact: true })
    .waitFor();
  // Malformed storage and storage failure do not make the app inaccessible.
  const broken = await browser.newContext();
  await broken.route("**/*", (r) =>
    new URL(r.request().url()).origin === origin ? r.continue() : r.abort(),
  );
  await broken.addInitScript((key) => {
    localStorage.setItem(key, "{broken");
    localStorage.setItem("steve.ui.locale", "zh");
  }, APPEARANCE_KEY);
  const bp = await broken.newPage();
  await bp.goto(origin + "/#/settings?section=appearance");
  await bp.getByRole("heading", { name: "外观", exact: true }).waitFor();
  assert.deepEqual(
    await bp.evaluate(() => window.appearance),
    defaultAppearance(),
  );
  await bp.evaluate(
    (palette) =>
      window.changeAppearance((a) => ({
        ...a,
        custom: Array.from({ length: 31 }, (_, index) => ({
          ...palette,
          id: `custom-capacity-${index}`,
          name: `Saved ${index}`,
        })),
      })),
    custom,
  );
  await bp.evaluate(() => {
    window.originalSetItem = Storage.prototype.setItem;
    window.failedAppearanceWrites = 0;
    Storage.prototype.setItem = () => {
      window.failedAppearanceWrites++;
      throw new DOMException("quota", "QuotaExceededError");
    };
    window.changeAppearance((a) => ({ ...a, mode: "dark" }));
  });
  await bp.waitForFunction(() =>
    document.documentElement.classList.contains("dark-mode"),
  );
  await bp
    .getByRole("alert")
    .filter({ hasText: /无法持久保存/ })
    .waitFor();
  await bp
    .getByRole("button", { name: "复制并编辑", exact: true })
    .first()
    .click();
  await bp
    .getByRole("textbox", { name: "配色名称", exact: true })
    .fill("未保存配色");
  await bp.getByRole("button", { name: "保存并应用配色", exact: true }).click();
  await bp.waitForFunction(() => window.failedAppearanceWrites >= 2);
  assert.equal(
    await bp.evaluate(() => window.appearance.custom.length),
    31,
    "failed palette save must not create a phantom saved palette",
  );
  assert.equal(
    await bp
      .getByRole("textbox", { name: "配色名称", exact: true })
      .inputValue(),
    "未保存配色",
    "failed persistence must keep the palette draft open",
  );
  assert.equal(
    await bp.getByText("配色已保存并选用于对应模式。", { exact: true }).count(),
    0,
  );
  await bp.evaluate(() => {
    Storage.prototype.setItem = window.originalSetItem;
  });
  assert.equal(
    await bp
      .getByRole("button", { name: "保存并应用配色", exact: true })
      .isEnabled(),
    true,
    "the 32nd unsaved palette must remain retryable",
  );
  await bp.getByRole("button", { name: "保存并应用配色", exact: true }).click();
  await bp.getByText("配色已保存并选用于对应模式。", { exact: true }).waitFor();
  assert.equal(
    await bp.evaluate((key) => {
      const saved = JSON.parse(localStorage.getItem(key));
      return (
        saved.mode === "dark" &&
        saved.custom.filter((p) => p.name === "未保存配色").length === 1
      );
    }, APPEARANCE_KEY),
    true,
    "retry replays unsaved changes without duplicating the palette",
  );
  // Reset after a failed edit must not persist the failed name on the next save.
  await bp.getByRole("button", { name: "编辑", exact: true }).click();
  await bp
    .getByRole("textbox", { name: "配色名称", exact: true })
    .fill("FAILED NAME");
  await bp.evaluate(() => {
    window.failedAppearanceWrites = 0;
    Storage.prototype.setItem = () => {
      window.failedAppearanceWrites++;
      throw new DOMException("quota", "QuotaExceededError");
    };
  });
  await bp.getByRole("button", { name: "保存并应用配色", exact: true }).click();
  await bp.waitForFunction(() => window.failedAppearanceWrites === 1);
  bp.once("dialog", (dialog) => dialog.accept());
  await bp.getByRole("button", { name: "恢复原配色", exact: true }).click();
  assert.equal(
    await bp
      .getByRole("textbox", { name: "配色名称", exact: true })
      .inputValue(),
    "未保存配色",
  );
  await bp.evaluate(() => {
    Storage.prototype.setItem = window.originalSetItem;
  });
  await bp.getByRole("button", { name: "保存并应用配色", exact: true }).click();
  await bp.getByText("配色已保存并选用于对应模式。", { exact: true }).waitFor();
  assert.equal(
    await bp.evaluate(
      (key) =>
        JSON.parse(localStorage.getItem(key)).custom.some(
          (p) => p.name === "FAILED NAME",
        ),
      APPEARANCE_KEY,
    ),
    false,
    "failed palette edits must not survive Reset",
  );
  await bp.getByRole("button", { name: "编辑", exact: true }).click();
  await bp
    .getByRole("textbox", { name: "配色名称", exact: true })
    .fill("CANCELLED NAME");
  await bp.evaluate(() => {
    window.failedAppearanceWrites = 0;
    Storage.prototype.setItem = () => {
      window.failedAppearanceWrites++;
      throw new DOMException("quota", "QuotaExceededError");
    };
  });
  await bp.getByRole("button", { name: "保存并应用配色", exact: true }).click();
  await bp.waitForFunction(() => window.failedAppearanceWrites === 1);
  bp.once("dialog", (dialog) => dialog.accept());
  await bp.getByRole("button", { name: "取消", exact: true }).click();
  await bp.evaluate(async () => {
    Storage.prototype.setItem = window.originalSetItem;
    await window.changeAppearance((a) => ({
      ...a,
      fonts: { ...a.fonts, ui: { ...a.fonts.ui, size: 17 } },
    }));
  });
  assert.equal(
    await bp.evaluate(
      (key) =>
        JSON.parse(localStorage.getItem(key)).custom.some(
          (p) => p.name === "CANCELLED NAME",
        ),
      APPEARANCE_KEY,
    ),
    false,
    "a later font save must not replay cancelled palette edits",
  );
  await bp.evaluate(() =>
    Object.defineProperty(navigator, "locks", {
      value: undefined,
      configurable: true,
    }),
  );
  assert.equal(
    await bp.evaluate(() =>
      window.changeAppearance((a) => ({ ...a, mode: "light" })),
    ),
    false,
  );
  await bp
    .getByRole("alert")
    .filter({ hasText: /无法持久保存/ })
    .waitFor();
  assert.equal(
    await bp.evaluate(
      (key) => JSON.parse(localStorage.getItem(key)).mode,
      APPEARANCE_KEY,
    ),
    "dark",
    "without coordination changes stay local rather than overwriting other windows",
  );
  await broken.close();
  await context.close();
  console.log(
    "Appearance browser: independent schemes, toolbar, persistence/prepaint, fonts, multi-window, custom palette, narrow/dark/light, offline and no hub writes PASS",
  );
} finally {
  await browser?.close();
  if (server) await new Promise((r) => server.close(r));
  await rm(scratch, { recursive: true, force: true });
}
