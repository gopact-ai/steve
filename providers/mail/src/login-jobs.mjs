import { randomUUID } from 'node:crypto';
import { googleClient } from './google.mjs';
import { microsoftClient } from './microsoft.mjs';
import { ICloudInbox } from './icloud.mjs';
import { errorCode } from './runner.mjs';
import { SetupError } from './errors.mjs';

async function login(account, store, hooks) {
  if (account.provider === 'gmail') return googleClient(account, store, true, { openBrowser: hooks.google, signal: hooks.signal });
  if (['outlook', 'hotmail'].includes(account.provider)) return microsoftClient(account, store, true, (_, response) => hooks.microsoft(response), { signal: hooks.signal });
  if (!account.app_password && !account.password_env) throw new SetupError('Add an iCloud app-specific password before signing in');
  const client = new ICloudInbox(account);
  await client.poll(null, new Date().toISOString());
  return client;
}

export class LoginJobs {
  constructor(manager, factory = login) { this.manager = manager; this.factory = factory; this.jobs = new Map(); }
  active(account) { return [...this.jobs.values()].some(job => job.account === account && ['starting', 'waiting'].includes(job.state)); }
  forAccount(account) { return [...this.jobs.values()].findLast(job => job.account === account)?.id; }
  status(id, publicOnly = false) {
    const job = this.jobs.get(id);
    if (!job) throw new SetupError('Login session expired; start login again');
    const { promise, controller, url, user_code, ...result } = job;
    return publicOnly ? result : { ...result, url, user_code };
  }
  start(id) {
    const account = this.manager.get(id);
    if (this.active(id)) return this.status(this.forAccount(id), true);
    if (this.manager.runner?.inflight?.has(id)) throw new SetupError('Wait for mailbox synchronization to finish');
    const apps = this.manager.store.secret('managed:oauth') || {};
    if (account.provider === 'gmail' && !account.google_keys && !account.credentials_file) account.google_keys = apps.google_keys;
    if (['outlook', 'hotmail'].includes(account.provider) && !account.client_id) account.client_id = apps.client_id;
    if (account.provider === 'gmail' && !account.google_keys && !account.credentials_file) throw new SetupError('Configure a Google desktop OAuth application first');
    if (['outlook', 'hotmail'].includes(account.provider) && !account.client_id) throw new SetupError('Configure a Microsoft public-client application first');
    this.manager.store.account(account, false); this.manager.save();
    const controller = new AbortController();
    const job = { id: randomUUID(), account: id, state: 'starting', at: new Date().toISOString(), controller };
    // One latest job per account keeps administration and polling bounded.
    for (const [key, old] of this.jobs) if (old.account === id) this.jobs.delete(key);
    this.jobs.set(job.id, job);
    const automatic = account.provider === 'gmail' && account.credential_ref && this.manager.config.onepassword?.auto_login === true;
    if (automatic) { job.method = 'onepassword'; job.state = 'waiting'; job.stage = 'queued'; job.needs_owner = false; }
    const perform = () => {
      if (controller.signal.aborted) throw new SetupError('Login cancelled');
      return this.factory(account, this.manager.store, { signal: controller.signal,
        google: async url => {
          job.url = url; job.state = 'waiting';
          if (automatic) {
            await this.manager.loginBrowser.login(account, url, { signal: controller.signal, progress: (stage, hint) => {
              job.stage = stage; job.needs_owner = stage === 'owner_action_required' || stage === 'onepassword_approval';
              if (hint) job.hint = hint; else delete job.hint;
            } });
          } else { job.method = 'manual'; job.needs_owner = true; }
        },
        microsoft: response => { job.url = response.verificationUri; job.user_code = response.userCode; job.state = 'waiting'; job.method = 'manual'; job.needs_owner = true; },
      });
    };
    job.promise = (automatic ? this.manager.loginBrowser.enqueue(perform) : perform()).then(client => {
      this.manager.store.set(`authorized:${id}`, '1');
      this.manager.runner?.adapters.set(id, client);
      job.state = 'authorized'; job.needs_owner = false; delete job.url; delete job.user_code; delete job.hint;
    }).catch(error => { job.state = 'error'; job.code = errorCode(error); job.hint = error instanceof SetupError ? error.message : 'Authorization failed; start login again';
      delete job.url; delete job.user_code; });
    return this.status(job.id, true);
  }
  async close() { for (const job of this.jobs.values()) job.controller.abort(); await this.manager.loginBrowser?.close(); await Promise.all([...this.jobs.values()].map(job => job.promise)); }
}
