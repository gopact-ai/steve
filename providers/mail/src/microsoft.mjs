import { SetupError } from './errors.mjs';
import { PublicClientApplication, LogLevel } from '@azure/msal-node';
import { Client } from '@microsoft/microsoft-graph-client';
import { envelope, plainText } from './message.mjs';

export const MICROSOFT_SCOPES = ['https://graph.microsoft.com/Mail.Read', 'https://graph.microsoft.com/User.Read'];

export async function microsoftClient(account, store, login = false, prompt = console.log, { signal } = {}) {
  const msal = new PublicClientApplication({
    auth: { clientId: account.client_id, authority: 'https://login.microsoftonline.com/consumers' },
    system: { loggerOptions: { logLevel: LogLevel.Error, piiLoggingEnabled: false, loggerCallback() {} } },
    cache: { cachePlugin: {
      async beforeCacheAccess(context) { const saved = store.secret(`msal:${account.id}`); if (saved) context.tokenCache.deserialize(saved); },
      async afterCacheAccess(context) { if (context.cacheHasChanged) store.secret(`msal:${account.id}`, context.tokenCache.serialize()); },
    } },
  });
  let owner;
  if (login) {
    const request = { scopes: MICROSOFT_SCOPES, timeout: 300, cancel: signal?.aborted || false,
      deviceCodeCallback: response => prompt(response.message, response) };
    const cancel = () => { request.cancel = true; };
    signal?.addEventListener('abort', cancel, { once: true });
    try { owner = (await msal.acquireTokenByDeviceCode(request))?.account; }
    finally { signal?.removeEventListener('abort', cancel); }
  } else {
    const homeId = store.get(`microsoft-owner:${account.id}`);
    owner = homeId ? await msal.getTokenCache().getAccountByHomeId(homeId) : null;
    if (!owner) throw new SetupError('Run login for this Microsoft account');
  }
  const api = Client.initWithMiddleware({ authProvider: { async getAccessToken() {
    const result = await msal.acquireTokenSilent({ account: owner, scopes: MICROSOFT_SCOPES });
    return result.accessToken;
  } } });
  const profile = await api.api('/me').select('id,mail,userPrincipalName').option('signal', AbortSignal.timeout(30000)).get();
  const addresses = [profile.mail, profile.userPrincipalName, owner.username].filter(Boolean).map(s => s.toLowerCase());
  if (!addresses.includes(account.address.toLowerCase())) throw new SetupError('Microsoft signed in to a different mailbox; configure its primary login address');
  if (login) store.set(`microsoft-owner:${account.id}`, owner.homeAccountId);
  return new MicrosoftInbox(account, api);
}

export function graphCursor(url) {
  const parsed = new URL(url);
  if (parsed.origin !== 'https://graph.microsoft.com' || !parsed.pathname.startsWith('/v1.0/') || parsed.username || parsed.password) {
    throw new SetupError('Microsoft returned an unexpected delta URL');
  }
  return url;
}

export class MicrosoftInbox {
  constructor(account, api) { this.account = account; this.api = api; }
  async poll(cursor, cutoff) {
    const query = new URLSearchParams({ changeType: 'created', '$select': 'id,internetMessageId,conversationId,from,toRecipients,subject,receivedDateTime,body,hasAttachments',
      '$filter': `receivedDateTime ge ${cutoff}`, '$top': '100' });
    const url = cursor?.url ? graphCursor(cursor.url) : `/me/mailFolders/inbox/messages/delta?${query}`;
    let response;
    try { response = await this.api.api(url).header('Prefer', 'IdType="ImmutableId", outlook.body-content-type="text"').option('signal', AbortSignal.timeout(30000)).get(); }
    catch (error) {
      if (![404, 410].includes(error.statusCode)) throw error;
      if (!cursor?.url) throw error;
      return this.poll(null, cutoff);
    }
    const messages = (response.value || []).filter(m => !m['@removed'] && m.id && Date.parse(m.receivedDateTime) >= Date.parse(cutoff)).map(m =>
      envelope(this.account, { id: m.id, internetId: m.internetMessageId, threadId: m.conversationId,
        from: m.from?.emailAddress?.address, to: (m.toRecipients || []).map(r => r.emailAddress?.address).join(', '),
        subject: m.subject, receivedAt: m.receivedDateTime,
        body: plainText(m.body?.content, m.body?.contentType?.toLowerCase() === 'html'), hasAttachments: m.hasAttachments }));
    const next = response['@odata.nextLink'] || response['@odata.deltaLink'];
    if (!next) throw new SetupError('Microsoft delta response has no continuation token');
    return { messages, cursor: { url: graphCursor(next) }, more: !!response['@odata.nextLink'] };
  }
}
