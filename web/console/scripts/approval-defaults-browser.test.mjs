import assert from 'node:assert/strict';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { createServer } from '../node_modules/vite/dist/node/index.js';
import { chromium } from '../node_modules/playwright/index.mjs';
import { workState } from '../../../e2e/console/work-fixture.mjs';

const root = fileURLToPath(new URL('..', import.meta.url));
const server = await createServer({ root, configFile: path.join(root, 'vite.config.ts'), server: { host: '127.0.0.1', port: 0, hmr: false }, logLevel: 'error' });
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL });
try {
    const page = await browser.newPage({ viewport: { width: 1280, height: 900 }, reducedMotion: 'reduce' });
    page.setDefaultTimeout(7000);
    const errors = [], writes = [];
    let rejectSave = true, holdReads = false;
    const delayedReads = [];
    const base = { harness: 'fixture', node: 'hub', eligible: true, level: 'internal', approval: 'ask' };
    const agents = [
        { ...base, id: 'known', options: { effort: 'high', permission: 'workspace' }, selectors: [{ id: 'permission', category: 'mode', name: 'Permissions', values: ['read-only', 'workspace', 'full-access'], choices: ['Read only', 'Workspace', 'Full access'] }, { id: 'effort', choices: ['low', 'high'] }] },
        { ...base, id: 'unknown', harness: 'other-fixture', options: {}, selectors: [] },
        { ...base, id: 'legacy', options: { mode: 'custom-policy' }, selectors: [] },
        { ...base, id: 'label-pin', options: { mode: 'Full access' }, selectors: [{ id: 'mode', category: 'mode', values: ['read-only', 'full-access'], choices: ['Read only', 'Full access'] }] },
    ];
    await page.addInitScript(() => {
        localStorage.setItem('steve.ui.locale', 'zh');
        window.EventSource = class { constructor() { setTimeout(() => this.onopen?.(), 0); } close() {} };
    });
    page.on('pageerror', error => errors.push(String(error)));
    await page.route('**/*', route => {
        const req = route.request(), url = new URL(req.url());
        if (url.origin !== origin) { errors.push('external request'); return route.abort(); }
        if (url.pathname === '/state' && holdReads) { delayedReads.push({ route, agents: structuredClone(agents) }); return; }
        if (url.pathname === '/state') return route.fulfill({ json: workState({ at: '', hub: { node: 'hub', started: '' }, agents, nodes: [], tasks: [], plans: [], projects: [], attempts: [], landings: [] }) });
        if (url.pathname === '/console/coordination') return route.fulfill({ json: { enabled: false, nodes: [], events: [] } });
        if (url.pathname === '/console/desktop') return route.fulfill({ json: { enabled: false } });
        if (url.pathname === '/console/conversations') return route.fulfill({ json: { enabled: true, conversations: [] } });
        if (url.pathname === '/console/queue') return route.fulfill({ json: { queue: [] } });
        if (url.pathname === '/console/agents/known' && req.method() === 'PUT') {
            const spec = req.postDataJSON();
            if (rejectSave) { rejectSave = false; return route.fulfill({ status: 400, json: { error: 'fixture: save rejected' } }); }
            writes.push(spec); Object.assign(agents[0], spec);
            return route.fulfill({ json: { ok: true } });
        }
        if (url.pathname.startsWith('/console/')) { errors.push(`Unexpected ${req.method()} ${url.pathname}`); return route.fulfill({ status: 501, json: { error: 'Unmocked' } }); }
        return route.continue();
    });
    await page.goto(`${origin}/#/fleet?tab=agents`);
    const open = async id => { await page.getByRole('row', { name: id, exact: true }).click(); return page.getByRole('dialog', { name: id, exact: true }); };
    let drawer = await open('known');
    await drawer.getByRole('heading', { name: '默认审批模式', exact: true }).waitFor();
    await drawer.getByText('Agent 单独设置 · workspace', { exact: true }).waitFor();
    assert.equal(await drawer.getByRole('link', { name: '全局审批设置', exact: true }).getAttribute('href'), '#/settings?section=approval');
    await drawer.getByRole('button', { name: '设置审批模式', exact: true }).click();
    const select = drawer.getByRole('button', { name: /默认审批模式/ });
    assert.equal(await select.innerText(), 'Workspace', 'A saved native value resolves to its display label');
    await select.focus(); await page.keyboard.press('Enter');
    await page.keyboard.press('End'); await page.keyboard.press('Enter');
    assert.match(await select.innerText(), /Full access/, 'Keyboard selection changes the approval draft');
    await drawer.getByRole('button', { name: '保存', exact: true }).click();
    await drawer.getByRole('alert').filter({ hasText: 'fixture: save rejected' }).waitFor();
    assert.match(await select.innerText(), /Full access/, 'Failed saves preserve the approval draft');
    assert.equal(agents[0].options.permission, 'workspace');
    holdReads = true;
    const laggingRead = page.waitForRequest(request => new URL(request.url()).pathname === '/state');
    await drawer.getByRole('button', { name: '保存', exact: true }).click();
    await drawer.getByRole('button', { name: '设置审批模式', exact: true }).click();
    assert.match(await select.innerText(), /Full access/, 'Re-edit before snapshot refresh retains the acknowledged approval');
    await drawer.getByRole('button', { name: '取消', exact: true }).click();
    await drawer.getByRole('button', { name: '设置审批模式', exact: true }).click();
    assert.match(await select.innerText(), /Full access/, 'Cancel while snapshot lags retains the acknowledged baseline');
    await laggingRead;
    await drawer.getByRole('textbox', { name: '用途', exact: true }).fill('Saved while refreshing');
    await drawer.getByRole('button', { name: '保存', exact: true }).click();
    await drawer.getByRole('button', { name: '设置审批模式', exact: true }).waitFor();
    assert.equal(writes.at(-1).options.permission, 'full-access', 'Saving another field must not restore the older mode');
    holdReads = false;
    for (const held of delayedReads.splice(0)) await held.route.fulfill({ json: workState({ at: '', hub: { node: 'hub', started: '' }, agents: held.agents, nodes: [], tasks: [], plans: [], projects: [], attempts: [], landings: [] }) });
    await drawer.getByRole('button', { name: '设置审批模式', exact: true }).click();
    assert.equal(await drawer.getByRole('textbox', { name: '用途', exact: true }).inputValue(), 'Saved while refreshing', 'An intermediate snapshot must not replace the latest acknowledged settings');
    await drawer.getByRole('button', { name: '取消', exact: true }).click();
    await drawer.getByText('Agent 单独设置 · full-access', { exact: true }).waitFor();
    assert.equal(writes.at(-1).options.permission, 'full-access');
    assert.equal(writes.at(-1).options.effort, 'high', 'Approval edits preserve unrelated options');
    await drawer.getByRole('button', { name: '设置审批模式', exact: true }).click();
    await select.click(); await page.getByRole('option', { name: '跟随全局默认（每步询问）', exact: true }).click();
    await drawer.getByRole('button', { name: '取消', exact: true }).click();
    await drawer.getByRole('button', { name: '设置审批模式', exact: true }).click();
    assert.match(await select.innerText(), /Full access/, 'Cancelled approval drafts must not survive a new edit');
    await select.click(); await page.getByRole('option', { name: '跟随全局默认（每步询问）', exact: true }).click();
    await drawer.getByRole('button', { name: '保存', exact: true }).click();
    await drawer.getByText('跟随全局默认 · 每步询问', { exact: true }).waitFor();
    assert.deepEqual(writes.at(-1).options, { effort: 'high' });
    await drawer.getByRole('button', { name: '设置审批模式', exact: true }).click();
    await drawer.getByRole('button', { name: /AI 工具$/ }).click();
    await page.getByRole('option', { name: 'other-fixture', exact: true }).click();
    assert.equal(await select.isDisabled(), true, 'A changed tool cannot use the previous tool’s options');
    holdReads = true;
    await drawer.getByRole('button', { name: '保存', exact: true }).click();
    await drawer.getByRole('button', { name: '设置审批模式', exact: true }).click();
    assert.equal(await select.isDisabled(), true, 'Confirmed tool edits must not re-enable selectors from an older snapshot');
    holdReads = false;
    for (const held of delayedReads.splice(0)) await held.route.fulfill({ json: workState({ at: '', hub: { node: 'hub', started: '' }, agents, nodes: [], tasks: [], plans: [], projects: [], attempts: [], landings: [] }) });
    await drawer.getByRole('button', { name: '关闭', exact: true }).click();
    drawer = await open('unknown');
    await drawer.getByRole('heading', { name: '默认审批模式', exact: true }).waitFor();
    await drawer.getByText(/尚未取得此 Agent 的审批选项/).waitFor();
    await drawer.getByRole('button', { name: '设置审批模式', exact: true }).click();
    assert.equal(await drawer.getByRole('button', { name: /默认审批模式/ }).isDisabled(), true);
    assert.equal(writes.length, 4, 'Viewing unavailable capabilities must never guess or save a mode');
    await page.setViewportSize({ width: 390, height: 620 });
    await page.evaluate(() => document.documentElement.style.setProperty('--ui-font-size', '18px'));
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    const bodyFits = await drawer.locator('.workbench-drawer-body').evaluate(el => el.scrollWidth <= el.clientWidth + 1);
    assert.ok(bodyFits, 'Approval settings and capability hint must wrap in a narrow drawer');
    if (process.env.APPROVAL_SCREENSHOT) await page.screenshot({ path: process.env.APPROVAL_SCREENSHOT });
    await drawer.getByRole('button', { name: '关闭', exact: true }).click();
    drawer = await open('legacy');
    await drawer.getByText('Agent 单独设置 · custom-policy', { exact: true }).waitFor();
    await drawer.getByRole('button', { name: '关闭', exact: true }).click();
    drawer = await open('label-pin');
    await drawer.getByRole('button', { name: '设置审批模式', exact: true }).click();
    const labelPin = drawer.getByRole('button', { name: /默认审批模式/ });
    assert.equal(await labelPin.innerText(), 'Full access', 'An existing label-form pin is recognized without rewriting it');
    await labelPin.click();
    assert.equal(await page.getByRole('option').count(), 3, 'A label-form pin must not add a duplicate unavailable choice');
    await page.keyboard.press('Escape');
    assert.equal(agents[3].options.mode, 'Full access');
    assert.deepEqual(errors, []);
    console.log('PASS Agent approval keyboard/save rejection and entry, native override, inherit reset, cancel, unknown capabilities and narrow layout');
} finally { await browser.close(); await server.close(); }
