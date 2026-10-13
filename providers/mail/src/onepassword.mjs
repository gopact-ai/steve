import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { SetupError } from './errors.mjs';

const exec = promisify(execFile);
const id = value => typeof value === 'string' && /^[a-z0-9]{26}$/i.test(value);
export function googleLoginItem(item) {
  const urls = Array.isArray(item?.urls) ? item.urls : [];
  const primary = urls.filter(entry => entry.primary);
  return item?.category === 'LOGIN' && !item.archived && (primary.length ? primary : urls).some(entry => {
    try { const url = new URL(entry.href); return url.protocol === 'https:' && !url.username && !url.password && ['accounts.google.com', 'mail.google.com'].includes(url.hostname); }
    catch { return false; }
  });
}
function fields(value) {
  const rows = Array.isArray(value) ? value : [value];
  return Object.fromEntries(rows.filter(row => row && typeof row.value === 'string').map(row => [row.id || row.label, row.value]));
}

// Only this local broker sees CLI output. No item-get or arbitrary-command tool
// is exposed to the agent; password/OTP values are passed directly to the browser.
export class OnePassword {
  constructor(config, executor = exec) { this.config = config; this.executor = executor; }
  configured() { return !!this.config?.vaults?.length; }
  async run(args, signal, json = true) {
    if (!this.configured()) throw new SetupError('Configure the local 1Password CLI and allowed vaults first');
    try {
      const result = await this.executor(this.config.command || 'op', [...args, ...(this.config.account ? [`--account=${this.config.account}`] : [])], {
        shell: false, encoding: 'utf8', maxBuffer: 4 * 1024 * 1024, timeout: 120000, signal,
        env: { ...process.env, OP_INCLUDE_ARCHIVE: 'false' }, windowsHide: true,
      });
      return json ? JSON.parse(result.stdout) : result.stdout.trim();
    } catch {
      // CLI errors can include item contents, command arguments and stderr.
      throw new SetupError('1Password is unavailable or access was denied; check the official CLI, app unlock and vault approval');
    }
  }
  async list(vault, signal) {
    if (!this.config?.vaults?.includes(vault)) throw new SetupError('Use an owner-configured 1Password vault');
    const items = await this.run(['item', 'list', `--vault=${vault}`, '--categories=Login', '--format=json'], signal);
    if (!Array.isArray(items) || items.length > 1000) throw new SetupError('The selected 1Password vault exceeds 1000 items');
    return items;
  }
  async preview(vault, signal) {
    const items = await this.list(vault, signal); const accounts = []; const seen = new Set(); let skipped = 0;
    for (const item of items) {
      if (!googleLoginItem(item) || !id(item.id) || !id(item.vault?.id)) { skipped++; continue; }
      const value = fields(await this.run(['item', 'get', item.id, `--vault=${vault}`, '--fields=label=username', '--format=json'], signal));
      const address = (value.username || '').trim().toLowerCase();
      if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(address) || address.length > 254 || seen.has(address)) { skipped++; continue; }
      seen.add(address);
      accounts.push({ provider: 'gmail', address, title: String(item.title || address).slice(0, 256), group: vault.slice(0, 80),
        tags: (item.tags || []).filter(tag => typeof tag === 'string').slice(0, 20).map(tag => tag.slice(0, 40)),
        credential_ref: { vault, vault_id: item.vault.id, item_id: item.id } });
    }
    return { accounts, skipped };
  }
  async verify(account, signal) {
    const ref = account.credential_ref;
    if (!ref || !id(ref.item_id) || !id(ref.vault_id)) throw new SetupError('Import a Gmail login from an approved 1Password vault first');
    const items = await this.list(ref.vault, signal);
    const item = items.find(item => item.id === ref.item_id && item.vault?.id === ref.vault_id);
    if (!googleLoginItem(item)) throw new SetupError('The selected 1Password item is no longer an active Google login in the approved vault');
    return ref;
  }
  async credentials(account, signal) {
    const ref = await this.verify(account, signal);
    const value = fields(await this.run(['item', 'get', ref.item_id, `--vault=${ref.vault_id}`, '--fields=label=username,label=password', '--reveal', '--format=json'], signal));
    if (value.username?.trim().toLowerCase() !== account.address.toLowerCase()) throw new SetupError('The 1Password login address changed; import the item again');
    if (!value.password || value.password.length > 4096) throw new SetupError('This 1Password login needs a usable password; passkey-only logins require the owner');
    return { username: account.address, password: value.password };
  }
  async otp(account, signal) {
    const ref = await this.verify(account, signal);
    const value = await this.run(['item', 'get', ref.item_id, `--vault=${ref.vault_id}`, '--otp'], signal, false);
    if (!/^\d{6,8}$/.test(value)) throw new SetupError('This login has no usable 1Password verification code');
    return value;
  }
}
