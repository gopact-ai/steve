import { createHash, randomUUID } from 'node:crypto';
import { readImport, writeExport } from './formats.mjs';
import { SetupError } from './errors.mjs';
import { LoginJobs } from './login-jobs.mjs';
import { OnePassword } from './onepassword.mjs';
import { LoginBrowser } from './login-browser.mjs';

export class MailManager {
  constructor(config, store, runner, loginFactory) {
    Object.assign(this, { config, store, runner });
    this.accounts = store.secret('managed:accounts') || config.accounts.map(a => ({ ...a, enabled: true }));
    this.onepassword = new OnePassword(config.onepassword);
    this.loginBrowser = new LoginBrowser(this.onepassword, config.onepassword);
    this.jobs = new LoginJobs(this, loginFactory);
    this.syncTasks = new Set();
    this.save();
  }
  save() { this.store.secret('managed:accounts', this.accounts); }
  get(id) {
    const account = this.accounts.find(a => a.id === id && !a.archived);
    if (!account) throw new SetupError('Mailbox not found');
    return account;
  }
  publicAccount(a, states) {
    const health = JSON.parse(this.store.get(`health:${a.id}`) || 'null');
    const state = states ? states.get(a.id) : this.store.status().find(row => row.id === a.id);
    return { id: a.id, provider: a.provider, address: a.address, title: a.title || a.address, group: a.group || '', tags: a.tags || [], enabled: a.enabled !== false,
      authorized: this.authorized(a),
      has_password: !!(a.password || a.app_password), needs_app_password: a.provider === 'icloud' && !a.app_password && !a.password_env,
      login_method: a.credential_ref && this.config.onepassword?.auto_login === true ? 'onepassword' : 'manual',
      pending: state?.pending || 0, health, login: this.jobs.forAccount(a.id) };
  }
  authorized(a) { return !!this.store.get(`authorized:${a.id}`) || !!this.store.secret(`google:${a.id}`) || !!this.store.get(`microsoft-owner:${a.id}`); }
  startSync(id) {
    const account = this.get(id);
    if (account.enabled === false || !this.authorized(account) || this.jobs.active(id)) throw new SetupError('Authorize and enable this mailbox before synchronization');
    if (this.runner.inflight.has(id)) return { state: 'syncing', duplicate: true };
    const task = this.runner.sync(account).finally(() => this.syncTasks.delete(task));
    this.syncTasks.add(task);
    return { state: 'syncing' };
  }
  snapshot() {
    const apps = this.store.secret('managed:oauth') || {};
    const client = this.store.secret('managed:client');
    const states = new Map(this.store.status().map(row => [row.id, row]));
    return { items: this.accounts.filter(a => !a.archived).map(a => this.publicAccount(a, states)), next: '',
      apps: { google: !!apps.google_keys, microsoft: !!apps.client_id },
      ai: { enabled: !!client, client: client?.id, conversation: client?.conversation } };
  }
  onePasswordStatus() {
    return { configured: this.onepassword.configured(), vaults: this.config.onepassword?.vaults || [],
      auto_login: this.config.onepassword?.auto_login === true, provider: 'gmail' };
  }
  async previewOnePassword(vault) {
    const parsed = await this.onepassword.preview(vault);
    const id = randomUUID();
    this.store.secret(`draft:${id}`, { rows: parsed.accounts.map((row, index) => ({ ...row, id: String(index) })), skipped: parsed.skipped, expires: Date.now() + 24 * 3600000 });
    return this.draft(id);
  }
  configureApps(input) {
    const before = this.store.secret('managed:oauth') || {};
    if (input.google_credentials) {
      let keys; try { keys = JSON.parse(input.google_credentials).installed; } catch { /* Own error contains no credentials. */ }
      if (!keys?.client_id || !keys.client_secret || typeof keys.client_id !== 'string') throw new SetupError('Use the Google desktop application credentials JSON');
      before.google_keys = { client_id: keys.client_id, client_secret: keys.client_secret };
    }
    if (input.client_id) {
      if (!/^[0-9a-f-]{36}$/i.test(input.client_id)) throw new SetupError('Enter the Microsoft public-client application ID');
      before.client_id = input.client_id;
    }
    this.store.secret('managed:oauth', before);
    return this.snapshot();
  }
  add(input) {
    const { accounts } = readImport('accounts.json', Buffer.from(JSON.stringify([input])));
    if (accounts.length !== 1) throw new SetupError('Choose a supported provider and a valid mailbox address');
    const row = accounts[0]; const address = row.address.toLowerCase();
    const existing = this.accounts.find(a => a.provider === row.provider && a.address.toLowerCase() === address);
    if (existing && !existing.archived) return { ...this.publicAccount(existing), duplicate: true };
    if (existing) {
      Object.assign(existing, { archived: false, enabled: true, title: row.title, group: row.group, tags: row.tags, password: row.password,
        app_password: row.provider === 'icloud' && row.credential_kind === 'app_password' ? row.password : '' });
      this.save(); return { ...this.publicAccount(existing), restored: true };
    }
    if (this.accounts.length >= 1000) throw new SetupError('Manage at most 1000 mailboxes');
    const apps = this.store.secret('managed:oauth') || {};
    const id = `mail_${createHash('sha256').update(`${row.provider}:${address}`).digest('hex').slice(0, 24)}`;
    const account = { ...row, id, enabled: true, ...(row.provider === 'gmail' && apps.google_keys ? { google_keys: apps.google_keys } : {}),
      ...(['outlook', 'hotmail'].includes(row.provider) && apps.client_id ? { client_id: apps.client_id } : {}) };
    if (row.provider === 'icloud' && row.credential_kind === 'app_password') account.app_password = row.password;
    // Bind SDK identity when login begins; OAuth setup can still be completed later.
    this.accounts.push(account); this.save();
    return this.publicAccount(account);
  }
  update(id, input) {
    const account = this.get(id);
    if (this.runner?.inflight?.has(id) || this.jobs.active(id)) throw new SetupError('Wait for the mailbox operation to finish');
    if (input.group !== undefined) {
      if (typeof input.group !== 'string' || input.group.length > 80) throw new SetupError('Use a category of at most 80 characters');
    }
    if (input.tags !== undefined) {
      if (!Array.isArray(input.tags) || input.tags.length > 20 || !input.tags.every(tag => typeof tag === 'string' && tag.length <= 40)) throw new SetupError('Use at most 20 short mailbox tags');
    }
    if (input.app_password !== undefined) {
      if (account.provider !== 'icloud' || typeof input.app_password !== 'string' || !input.app_password.trim() || input.app_password.length > 4096) throw new SetupError('Enter an iCloud app-specific password');
    }
    if (typeof input.enabled === 'boolean') account.enabled = input.enabled;
    if (typeof input.title === 'string') account.title = input.title.slice(0, 256);
    if (input.group !== undefined) account.group = input.group.trim();
    if (input.tags !== undefined) account.tags = [...new Set(input.tags.map(tag => tag.trim()).filter(Boolean))];
    if (input.app_password !== undefined) {
      account.app_password = input.app_password; account.password = input.app_password; account.credential_kind = 'app_password';
      this.store.set(`authorized:${id}`, '');
    }
    this.runner?.adapters.delete(id); this.save();
    return this.publicAccount(account);
  }
  batch(input) {
    if (!Array.isArray(input.ids) || !input.ids.length || input.ids.length > 1000 || !input.ids.every(id => typeof id === 'string')) throw new SetupError('Select at most 1000 mailboxes');
    if (input.group !== undefined && (typeof input.group !== 'string' || input.group.length > 80)) throw new SetupError('Use a category of at most 80 characters');
    if (input.enabled !== undefined && typeof input.enabled !== 'boolean') throw new SetupError('Use a valid mailbox enabled state');
    const accounts = [...new Set(input.ids)].map(id => this.get(id));
    if (accounts.some(a => this.runner?.inflight?.has(a.id) || this.jobs.active(a.id))) throw new SetupError('Wait for the selected mailbox operations to finish');
    const before = structuredClone(this.accounts);
    try {
      for (const account of accounts) {
        if (input.group !== undefined) account.group = input.group.trim();
        if (input.enabled !== undefined) account.enabled = input.enabled;
      }
      this.save();
    } catch (error) { this.accounts = before; throw error; }
    return { updated: accounts.map(a => a.id) };
  }
  archive(id) {
    const account = this.get(id);
    if (this.runner?.inflight?.has(id) || this.jobs.active(id)) throw new SetupError('Wait for the mailbox operation to finish');
    Object.assign(account, { archived: true, enabled: false, password: '', app_password: '' }); delete account.credential_ref;
    for (const key of [`google:${id}`, `msal:${id}`]) this.store.deleteSecret(key);
    this.store.set(`microsoft-owner:${id}`, ''); this.store.set(`authorized:${id}`, '');
    this.runner?.adapters.delete(id); this.save();
    return { archived: true };
  }
  preview(input) {
    if (typeof input.name !== 'string' || typeof input.data !== 'string' || input.data.length > 6 * 1024 * 1024) throw new SetupError('Choose an import file of at most 4 MB');
    const parsed = readImport(input.name, Buffer.from(input.data, 'base64'));
    const id = randomUUID();
    const rows = parsed.accounts.map((row, index) => ({ ...row, id: `${index}` }));
    this.store.secret(`draft:${id}`, { rows, skipped: parsed.skipped, expires: Date.now() + 24 * 3600000 });
    return this.draft(id);
  }
  draft(id) {
    const draft = this.store.secret(`draft:${id}`);
    if (!draft || draft.expires < Date.now()) throw new SetupError('Import preview expired; choose the file again');
    return { id, skipped: draft.skipped, committed: !!draft.committed, imported: draft.imported || [], items: draft.rows.map(({ password, credential_ref, ...row }) => ({
      ...row, has_password: row.has_password ?? !!password,
      ...(credential_ref ? { login_method: 'onepassword' } : {}),
      duplicate: this.accounts.some(a => !a.archived && a.provider === row.provider && a.address.toLowerCase() === row.address.toLowerCase()) })) };
  }
  commit(input) {
    const preview = this.draft(input.draft_id);
    if (preview.committed) return { ...preview, imported: preview.imported || [], duplicate: true };
    if (!Array.isArray(input.rows) || !input.rows.every(id => typeof id === 'string' && preview.items.some(row => row.id === id))) throw new SetupError('Select rows from the current import preview');
    const draft = this.store.secret(`draft:${input.draft_id}`);
    const selected = new Set(input.rows); const imported = [];
    // Account and receipt writes share one transaction, making a lost response safe to retry.
    this.store.db.exec('BEGIN IMMEDIATE');
    const before = structuredClone(this.accounts);
    try {
      for (const row of draft.rows.filter(row => selected.has(row.id))) {
        const account = this.add({ ...row, credential_kind: input.icloud_app_passwords && row.provider === 'icloud' ? 'app_password' : row.credential_kind });
        if (row.credential_ref) {
          const target = this.get(account.id);
          if (this.runner?.inflight?.has(target.id) || this.jobs.active(target.id)) throw new SetupError('Wait for the mailbox operation to finish before linking 1Password');
          target.credential_ref = row.credential_ref; this.save();
        }
        if (!account.duplicate || row.credential_ref) imported.push(account.id);
      }
      // A committed preview only needs metadata and its receipt, never a second credential copy.
      for (const row of draft.rows) { row.has_password = !!row.password; if (row.credential_ref) row.login_method = 'onepassword'; delete row.password; delete row.credential_ref; }
      draft.committed = true; draft.imported = imported;
      this.store.secret(`draft:${input.draft_id}`, draft); this.store.db.exec('COMMIT');
    } catch (error) { this.store.db.exec('ROLLBACK'); this.accounts = before; throw error; }
    return { ...this.draft(input.draft_id), imported };
  }
  export(input) { return writeExport(input.format, this.accounts.filter(a => !a.archived && (!Array.isArray(input.ids) || input.ids.includes(a.id))), input.include_secrets === true); }
}
