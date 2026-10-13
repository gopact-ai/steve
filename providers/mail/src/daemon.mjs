import { setTimeout as delay } from 'node:timers/promises';
import { requiredEnv } from './config.mjs';
import { Runner } from './runner.mjs';
import { MailManager } from './manager.mjs';
import { MailClientWorker } from './client-worker.mjs';
import { adminServer } from './admin-server.mjs';

export async function serveMail(config, store, steve, signal) {
  const runner = new Runner(config, store, steve);
  const manager = new MailManager(config, store, runner);
  const worker = new MailClientWorker(manager);
  const server = await adminServer(manager, requiredEnv(config.admin_token_env), config.admin_port, worker);
  console.log(JSON.stringify({ service: 'mail-admin', url: server.url }));
  const tasks = new Set(); const due = new Map();
  const clientTask = worker.run(signal);
  try {
    while (!signal.aborted) {
      for (const account of manager.accounts) {
        if (account.archived || account.enabled === false || manager.jobs.active(account.id) || !manager.authorized(account)) continue;
        if (runner.inflight.has(account.id) || Date.now() < (due.get(account.id) || 0)) continue;
        due.set(account.id, Date.now() + config.poll_seconds * 1000);
        const task = runner.sync(account).finally(() => tasks.delete(task)); tasks.add(task);
      }
      await delay(1000, undefined, { signal }).catch(() => {});
    }
  } finally {
    await server.close(); await manager.jobs.close(); await Promise.all([...tasks, ...manager.syncTasks]); await clientTask;
  }
}
