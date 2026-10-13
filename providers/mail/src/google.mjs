import { SetupError } from './errors.mjs';
import { readFileSync } from 'node:fs';
import { gmail } from '@googleapis/gmail';
import { OAuth2Client } from 'google-auth-library';
import { authorizeGoogle } from './google-login.mjs';
import { envelope, plainText } from './message.mjs';

export const GOOGLE_SCOPES = ['https://www.googleapis.com/auth/gmail.readonly'];

export async function googleClient(account, store, login = false) {
  const keys = JSON.parse(readFileSync(account.credentials_file, 'utf8')).installed;
  if (!keys?.client_id) throw new SetupError('Use Google desktop OAuth application credentials');
  let auth;
  if (login) {
    auth = await authorizeGoogle(keys, GOOGLE_SCOPES);
  } else {
    const saved = store.secret(`google:${account.id}`);
    if (!saved?.refresh_token) throw new SetupError('Run login for this Gmail account');
    auth = new OAuth2Client(keys.client_id, keys.client_secret);
    auth.setCredentials(saved);
  }
  const api = gmail({ version: 'v1', auth });
  const profile = (await api.users.getProfile({ userId: 'me' }, { timeout: 30000 })).data;
  if (profile.emailAddress?.toLowerCase() !== account.address.toLowerCase()) throw new SetupError('Google signed in to a different mailbox; correct the configured login address');
  if (login) {
    if (!auth.credentials.refresh_token) throw new SetupError('Google did not return offline access; reauthorize this application');
    store.secret(`google:${account.id}`, auth.credentials);
  }
  auth.on('tokens', tokens => store.secret(`google:${account.id}`, { ...store.secret(`google:${account.id}`), ...tokens }));
  return new GmailInbox(account, api);
}

function decodePart(part) {
  const leaves = [];
  const attachments = [];
  function visit(p) {
    if (p.filename) attachments.push(p.filename);
    else if (p.body?.data && ['text/plain', 'text/html'].includes(p.mimeType)) {
      leaves.push({ mime: p.mimeType, value: Buffer.from(p.body.data, 'base64url').toString('utf8') });
    }
    for (const child of p.parts || []) visit(child);
  }
  visit(part || {});
  const text = leaves.filter(p => p.mime === 'text/plain');
  return { body: text.length ? text.map(p => p.value).join('\n') : leaves.map(p => plainText(p.value, true)).join('\n'), hasAttachments: !!attachments.length };
}

export class GmailInbox {
  constructor(account, api) { this.account = account; this.api = api; }
  async read(id, cutoff) {
    let message;
    try { message = (await this.api.users.messages.get({ userId: 'me', id, format: 'full' }, { timeout: 30000 })).data; }
    catch (error) { if (Number(error.code) === 404) return null; throw error; }
    if (!message.labelIds?.includes('INBOX') || Number(message.internalDate) < Date.parse(cutoff)) return null;
    const headers = Object.fromEntries((message.payload?.headers || []).map(h => [h.name.toLowerCase(), h.value]));
    return envelope(this.account, { id: message.id, internetId: headers['message-id'], threadId: message.threadId,
      from: headers.from, to: headers.to, subject: headers.subject,
      receivedAt: new Date(Number(message.internalDate)).toISOString(), ...decodePart(message.payload) });
  }
  async poll(cursor, cutoff) {
    if (!cursor) {
      const profile = (await this.api.users.getProfile({ userId: 'me' }, { timeout: 30000 })).data;
      return { cursor: { history: profile.historyId }, messages: [] };
    }
    if (cursor.scan) return this.scan(cursor, cutoff);
    let response;
    try {
      response = (await this.api.users.history.list({ userId: 'me', startHistoryId: cursor.history,
        pageToken: cursor.page, historyTypes: ['messageAdded'], labelId: 'INBOX', maxResults: 100 }, { timeout: 30000 })).data;
    } catch (error) {
      if (Number(error.code) !== 404) throw error;
      const profile = (await this.api.users.getProfile({ userId: 'me' }, { timeout: 30000 })).data;
      return this.scan({ scan: true, snapshot: profile.historyId }, cutoff);
    }
    const ids = new Set((response.history || []).flatMap(h => (h.messagesAdded || []).map(m => m.message.id)));
    const messages = [];
    for (const group of chunks([...ids], 5)) messages.push(...(await Promise.all(group.map(id => this.read(id, cutoff)))).filter(Boolean));
    return { messages, cursor: response.nextPageToken ? { history: cursor.history, page: response.nextPageToken } : { history: response.historyId },
      more: !!response.nextPageToken };
  }
  async scan(cursor, cutoff) {
    const response = (await this.api.users.messages.list({ userId: 'me', labelIds: ['INBOX'],
      q: `after:${Math.floor(Date.parse(cutoff) / 1000)}`, pageToken: cursor.page, maxResults: 100 }, { timeout: 30000 })).data;
    const messages = [];
    for (const group of chunks(response.messages || [], 5)) messages.push(...(await Promise.all(group.map(item => this.read(item.id, cutoff)))).filter(Boolean));
    return { messages, cursor: response.nextPageToken ? { ...cursor, page: response.nextPageToken } : { history: cursor.snapshot },
      more: !!response.nextPageToken };
  }
}

function* chunks(values, count) {
  for (let i = 0; i < values.length; i += count) yield values.slice(i, i + count);
}
