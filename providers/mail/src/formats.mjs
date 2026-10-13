import { parse } from 'csv-parse/sync';
import { stringify } from 'csv-stringify/sync';
import { unzipSync, zipSync, strToU8 } from 'fflate';
import { randomUUID } from 'node:crypto';
import { SetupError } from './errors.mjs';

export const FILE_LIMIT = 4 * 1024 * 1024;
const DATA_LIMIT = 8 * 1024 * 1024;
const MAX_ITEMS = 1000;
const websites = { gmail: 'https://mail.google.com', outlook: 'https://outlook.live.com', hotmail: 'https://outlook.live.com', icloud: 'https://www.icloud.com/mail' };

export function providerFor(address, website = '') {
  const domain = address.toLowerCase().split('@')[1];
  if (['gmail.com', 'googlemail.com'].includes(domain)) return 'gmail';
  if (['hotmail.com', 'hotmail.co.uk', 'live.com', 'msn.com'].includes(domain)) return 'hotmail';
  if (['outlook.com', 'outlook.co.uk'].includes(domain)) return 'outlook';
  if (['icloud.com', 'me.com', 'mac.com'].includes(domain)) return 'icloud';
  // Recognize custom domains only when the item identifies a supported service.
  let host; try { host = new URL(website).hostname; } catch { return null; }
  if (['mail.google.com', 'accounts.google.com'].includes(host)) return 'gmail';
  if (['outlook.live.com', 'login.live.com'].includes(host)) return 'outlook';
  if (['www.icloud.com', 'icloud.com'].includes(host)) return 'icloud';
  return null;
}

function candidate(row) {
  const address = String(row.username || row.address || '').trim();
  const provider = row.provider || providerFor(address, row.website);
  if (address.length > 254 || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(address) || !Object.hasOwn(websites, provider)) return null;
  const password = String(row.password || '').slice(0, 4096);
  let metadata = {};
  try { metadata = JSON.parse(String(row.notes || '').split('Steve Mail metadata: ')[1] || '{}'); } catch { /* Optional classification metadata. */ }
  const group = String(metadata.group ?? row.group ?? '').trim().slice(0, 80);
  const tags = (Array.isArray(metadata.tags) ? metadata.tags : Array.isArray(row.tags) ? row.tags : String(row.tags || '').split(/[,;]/))
    .filter(tag => typeof tag === 'string').map(tag => tag.trim().slice(0, 40)).filter(Boolean).slice(0, 20);
  return { address, provider, title: String(row.title || address).slice(0, 256), password,
    group, tags: [...new Set(tags)],
    credential_kind: row.credential_kind === 'app_password' || metadata.credential_kind === 'app_password' || String(row.notes || '').startsWith('App-specific password') ? 'app_password' : 'login_password' };
}

function parse1pux(buffer) {
  let inflated = 0;
  const files = unzipSync(buffer, { filter(file) {
    if (!['export.attributes', 'export.data'].includes(file.name)) return false;
    inflated += file.originalSize;
    if (inflated > DATA_LIMIT || !Number.isFinite(inflated)) throw new SetupError('1PUX data exceeds the import limit');
    return true;
  } });
  if (!files['export.data'] || !files['export.attributes']) throw new SetupError('Missing 1PUX export.data or export.attributes');
  const attrs = JSON.parse(Buffer.from(files['export.attributes']).toString());
  if (attrs.version !== 3) throw new SetupError('Supported 1PUX version is 3');
  const data = JSON.parse(Buffer.from(files['export.data']).toString());
  const rows = [];
  for (const account of data.accounts || []) for (const vault of account.vaults || []) for (const item of vault.items || []) {
    if (item.state !== 'active' || item.categoryUuid !== '001') continue;
    const fields = item.details?.loginFields || [];
    const sections = (item.details?.sections || []).flatMap(section => section.fields || []);
    const app = sections.find(field => /app.*password|应用.*密码|专用密码/i.test(field.title || ''));
    rows.push({ title: item.overview?.title, group: vault.attrs?.name, tags: item.overview?.tags, website: item.overview?.url || item.overview?.urls?.[0]?.url,
      username: fields.find(field => field.designation === 'username')?.value || sections.find(field => field.id === 'username')?.value?.string,
      password: app?.value?.concealed || fields.find(field => field.designation === 'password')?.value,
      notes: item.details?.notesPlain, credential_kind: app?.value?.concealed ? 'app_password' : 'login_password' });
    if (rows.length > 50000) throw new SetupError('Import at most 50000 login items at a time');
  }
  return rows;
}

export function readImport(name, buffer) {
  if (buffer.length > FILE_LIMIT) throw new SetupError('Import files must be at most 4 MB');
  let rows;
  try {
    if (/\.1pux$/i.test(name)) rows = parse1pux(buffer);
    else if (/\.json$/i.test(name)) {
      const data = JSON.parse(buffer.toString('utf8')); rows = Array.isArray(data) ? data : data.accounts;
    } else if (/\.csv$/i.test(name)) {
      rows = parse(buffer, { bom: true, columns: headers => headers.map(h => h.trim().toLowerCase().replace(/ /g, '_')),
        skip_empty_lines: true, max_record_size: 64 * 1024 });
      rows = rows.filter(row => !['true', '1'].includes(String(row.archived || '').toLowerCase()));
    } else {
      // Paste-only account lists: passwords are never inferred from arbitrary text.
      rows = (buffer.toString('utf8').match(/[^\s<>;,]+@[^\s<>;,]+\.[a-z]{2,}/gi) || []).map(address => ({ address }));
    }
    if (!Array.isArray(rows) || rows.length > 50000) throw new SetupError('Import at most 50000 login items at a time');
    const seen = new Set(); const accounts = []; let skipped = 0;
    for (const row of rows) {
      const item = candidate(row); const key = item && `${item.provider}:${item.address.toLowerCase()}`;
      if (!item || seen.has(key)) { skipped++; continue; }
      seen.add(key); accounts.push(item);
      if (accounts.length > MAX_ITEMS) throw new SetupError('Import at most 1000 mailboxes at a time');
    }
    return { accounts, skipped };
  } catch (error) {
    if (error instanceof SetupError) throw error;
    throw new SetupError('Cannot read the import file; use a valid 1Password CSV, 1PUX or mailbox JSON export');
  }
}

export function writeExport(format, accounts, includeSecrets = false) {
  const rows = accounts.map(account => ({ title: account.title || account.address, website: websites[account.provider],
    username: account.address, password: includeSecrets ? account.app_password || account.password || '' : '',
    one_time_password: '', favorite: false, archived: false, tags: (account.tags || []).join(','),
    notes: `${account.app_password && includeSecrets ? 'App-specific password\n' : ''}Steve Mail metadata: ${JSON.stringify({ group: account.group || '', tags: account.tags || [], credential_kind: account.app_password ? 'app_password' : 'login_password' })}` }));
  let data;
  if (format === 'csv') data = Buffer.from(stringify(rows, { header: true, columns: ['title', 'website', 'username', 'password', 'one_time_password', 'favorite', 'archived', 'tags', 'notes'] }));
  else if (format === 'json') data = Buffer.from(JSON.stringify({ version: 1, accounts: accounts.map((a, index) => ({
    title: rows[index].title, provider: a.provider, address: a.address, group: a.group || '', tags: a.tags || [], credential_kind: a.app_password ? 'app_password' : 'login_password', password: rows[index].password })) }, null, 2));
  else if (format === '1pux') {
    const now = Math.floor(Date.now() / 1000);
    const items = rows.map((row, index) => ({ uuid: randomUUID(), favIndex: 0, createdAt: now, updatedAt: now, state: 'active', categoryUuid: '001',
      overview: { title: row.title, url: row.website, urls: [{ label: '', url: row.website }], tags: accounts[index].tags || [] },
      details: { loginFields: [{ designation: 'username', name: 'username', fieldType: 'E', value: row.username },
        { designation: 'password', name: 'password', fieldType: 'P', value: row.password }], notesPlain: row.notes, sections: [], passwordHistory: [] } }));
    const grouped = new Map();
    for (const [index, item] of items.entries()) { const group = accounts[index].group || ''; const entries = grouped.get(group) || []; entries.push(item); grouped.set(group, entries); }
    const vaults = [...grouped].map(([group, entries]) => ({ attrs: { uuid: randomUUID(), name: group || 'Mailboxes', desc: '', type: 'P' }, items: entries }));
    data = Buffer.from(zipSync({ 'export.attributes': strToU8(JSON.stringify({ version: 3, description: '1Password Unencrypted Export', createdAt: now })),
      'export.data': strToU8(JSON.stringify({ accounts: [{ attrs: { uuid: randomUUID(), accountName: 'Steve Mail', name: 'Steve Mail', email: '', domain: '' },
        vaults }] })) }));
  } else throw new SetupError('Choose CSV, 1PUX or JSON export');
  return { name: `steve-mail.${format}`, type: format === '1pux' ? 'application/zip' : format === 'csv' ? 'text/csv' : 'application/json', data: data.toString('base64') };
}
