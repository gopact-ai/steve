import { googleClient } from './google.mjs';
import { microsoftClient } from './microsoft.mjs';
import { ICloudInbox } from './icloud.mjs';
import { setTimeout as delay } from 'node:timers/promises';
import { SetupError } from './errors.mjs';

export async function adapter(account, store, login = false) {
  if (account.provider === 'gmail') return googleClient(account, store, login);
  if (['outlook', 'hotmail'].includes(account.provider)) return microsoftClient(account, store, login);
  return new ICloudInbox(account);
}

export function errorCode(error) {
  const code = error.statusCode || error.response?.status || error.errorCode || error.code || error.name || 'error';
  return /^[a-zA-Z0-9_-]{1,80}$/.test(String(code)) ? String(code) : 'error';
}

export class Runner {
  constructor(config, store, steve, factory = adapter, report = row => console.log(JSON.stringify(row))) {
    Object.assign(this, { config, store, steve, factory, report });
    this.adapters = new Map();
  }
  async cycle() {
    // SDK calls can wait on network/OAuth for one mailbox. Other accounts still
    // synchronize; SQLite commits stay synchronous and process ownership is locked.
    const results = await Promise.all(this.config.accounts.map(account => this.sync(account)));
    return results.every(Boolean);
  }
  async run(signal) {
    await Promise.all(this.config.accounts.map(async account => {
      while (!signal.aborted) {
        await this.sync(account);
        await delay(this.config.poll_seconds * 1000, undefined, { signal }).catch(error => {
          if (error.name !== 'AbortError') throw error;
        });
      }
    }));
  }
  async sync(account) {
    try {
      let { cutoff, cursor } = this.store.account(account);
      let published = await this.steve.drain(account.id);
      if (!this.adapters.has(account.id)) this.adapters.set(account.id, await this.factory(account, this.store));
      for (let page = 0; page < 10; page++) {
        const batch = await this.adapters.get(account.id).poll(cursor, cutoff);
        const unseen = batch.messages.filter(mail => !this.store.seen(mail.key));
        this.store.stage(account, batch.cursor, unseen);
        cursor = batch.cursor;
        published += await this.steve.drain(account.id);
        if (!batch.more) break;
      }
      const row = { account: account.id, state: 'ready', published, at: new Date().toISOString() };
      this.store.set(`health:${account.id}`, JSON.stringify(row));
      this.report(row);
      return true;
    } catch (error) {
      const row = { account: account.id, state: 'error', code: errorCode(error), at: new Date().toISOString(),
        ...(error instanceof SetupError ? { hint: error.message } : {}) };
      this.store.set(`health:${account.id}`, JSON.stringify(row));
      this.report(row);
      return false;
    }
  }
}
