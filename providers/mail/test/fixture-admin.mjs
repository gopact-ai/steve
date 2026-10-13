// Disposable UI/HTTP fixture. SDK protocol tests cover real clients separately;
// this runtime never contacts cloud mailboxes or a model.
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { Store } from '../src/store.mjs';
import { Runner } from '../src/runner.mjs';
import { MailManager } from '../src/manager.mjs';
import { MailClientWorker } from '../src/client-worker.mjs';
import { adminServer } from '../src/admin-server.mjs';

export async function createMailFixture() {
  const dir = mkdtempSync(join(tmpdir(), 'steve-mail-ui-')); const store = new Store(dir, Buffer.alloc(32, 4));
  const config = { accounts: [], poll_seconds: 60, steve_url: 'http://127.0.0.1:1' };
  const adapter = { async poll() { return { cursor: { fixture: true }, messages: [] }; } };
  const runner = new Runner(config, store, { async drain() { return 0; } }, async () => adapter, () => {});
  const manager = new MailManager(config, store, runner, async (account, _, hooks) => {
    if (account.provider === 'gmail') await hooks.google('https://accounts.google.com/o/oauth2/v2/auth?state=fixture');
    else if (['outlook', 'hotmail'].includes(account.provider)) hooks.microsoft({ verificationUri: 'https://www.microsoft.com/devicelogin', userCode: 'FIXTURE-CODE' });
    else if (!account.app_password) throw new Error('Missing fixture app password');
    await delay(2000, undefined, { signal: hooks.signal }); return adapter;
  });
  const worker = new MailClientWorker(manager);
  const token = 'mail-test-administration-token-of-at-least-32-characters';
  const server = await adminServer(manager, token, 0, worker);
  let timer; let pending = false;
  return { manager, worker, config, token, url: server.url,
    connect(url) {
      config.steve_url = url;
      timer = setInterval(async () => { if (pending) return; pending = true; try { await worker.tick(); } catch { /* Negative authorization scenarios are exercised by tests. */ } finally { pending = false; } }, 100);
    },
    async close() { clearInterval(timer); await server.close(); await manager.jobs.close(); await Promise.all(manager.syncTasks); while (pending) await delay(10); store.close(); rmSync(dir, { recursive: true, force: true }); },
  };
}
