import test from 'node:test';
import assert from 'node:assert/strict';
import { OAuth2Client } from 'google-auth-library';
import { authorizeGoogle } from '../src/google-login.mjs';

test('Google SDK browser authorization uses loopback, consent, PKCE and a validated one-time callback', { timeout: 5000 }, async () => {
  let exchange;
  const client = await authorizeGoogle({ client_id: 'desktop-fixture', client_secret: 'not-real' }, ['gmail.readonly'], {
    clientFactory(options) {
      const sdk = new OAuth2Client(options);
      sdk.getToken = async params => { exchange = params; return { tokens: { access_token: 'fixture', refresh_token: 'refresh-fixture' } }; };
      return sdk;
    },
    async openBrowser(url) {
      const request = new URL(url);
      assert.equal(request.origin, 'https://accounts.google.com');
      assert.equal(request.searchParams.get('code_challenge_method'), 'S256');
      assert.equal(request.searchParams.get('prompt'), 'consent');
      assert.equal(request.searchParams.get('access_type'), 'offline');
      assert.ok(request.searchParams.get('code_challenge'));
      const callback = new URL(request.searchParams.get('redirect_uri'));
      assert.equal(callback.hostname, '127.0.0.1');
      callback.searchParams.set('code', 'fixture-code');
      callback.searchParams.set('state', 'wrong-state');
      const invalid = await fetch(callback);
      assert.equal(invalid.status, 400);
      assert.equal(exchange, undefined);
      callback.searchParams.set('state', request.searchParams.get('state'));
      const valid = await fetch(callback);
      assert.equal(valid.status, 200);
      assert.match(await valid.text(), /complete/);
    },
  });
  assert.equal(exchange.code, 'fixture-code');
  assert.ok(exchange.codeVerifier);
  assert.equal(client.credentials.refresh_token, 'refresh-fixture');
});
