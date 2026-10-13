import { createServer } from 'node:http';
import { randomBytes, timingSafeEqual } from 'node:crypto';
import { OAuth2Client } from 'google-auth-library';
import open from 'open';
import { SetupError } from './errors.mjs';

// OAuth URL generation, PKCE and code/token exchange belong to Google's SDK.
// This is the local application's browser/callback glue only.
export async function authorizeGoogle(keys, scopes, { openBrowser = open, clientFactory = options => new OAuth2Client(options), signal } = {}) {
  const state = randomBytes(32).toString('base64url');
  let client;
  let verifier;
  let finish, fail;
  let accepted = false;
  const result = new Promise((resolve, reject) => { finish = resolve; fail = reject; });
  result.catch(() => {}); // The browser can cancel while its launch is still awaited.
  const abort = () => fail(new SetupError('Google authorization cancelled'));
  signal?.addEventListener('abort', abort, { once: true });
  const server = createServer(async (request, response) => {
    const url = new URL(request.url, 'http://127.0.0.1');
    const candidate = Buffer.from(url.searchParams.get('state') || '');
    const valid = candidate.length === Buffer.byteLength(state) && timingSafeEqual(candidate, Buffer.from(state));
    response.setHeader('Cache-Control', 'no-store');
    response.setHeader('Content-Type', 'text/plain; charset=utf-8');
    if (request.method !== 'GET' || url.pathname !== '/oauth2callback' || !valid || accepted) {
      response.writeHead(400); response.end('Invalid authorization callback'); return;
    }
    if (url.searchParams.has('error') || !url.searchParams.get('code')) {
      accepted = true;
      response.end('Authorization cancelled');
      fail(new SetupError('Google authorization was cancelled'));
      return;
    }
    accepted = true;
    try {
      const { tokens } = await client.getToken({ code: url.searchParams.get('code'), codeVerifier: verifier });
      client.setCredentials(tokens);
      response.once('finish', () => finish(client));
      response.end('Authorization complete. Return to Steve Mail.');
    } catch (error) { response.writeHead(400); response.end('Authorization failed'); fail(error); }
  });
  const timeout = setTimeout(() => fail(new SetupError('Google authorization timed out; run login again')), 300000);
  try {
    await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
    client = clientFactory({ clientId: keys.client_id, clientSecret: keys.client_secret,
      redirectUri: `http://127.0.0.1:${server.address().port}/oauth2callback` });
    const pkce = await client.generateCodeVerifierAsync();
    verifier = pkce.codeVerifier;
    const url = client.generateAuthUrl({ access_type: 'offline', prompt: 'consent', scope: scopes,
      state, code_challenge: pkce.codeChallenge, code_challenge_method: 'S256' });
    await openBrowser(url);
    if (signal?.aborted) abort();
    return await result;
  } finally {
    clearTimeout(timeout);
    signal?.removeEventListener('abort', abort);
    server.closeAllConnections();
    await new Promise(resolve => server.close(resolve));
  }
}
