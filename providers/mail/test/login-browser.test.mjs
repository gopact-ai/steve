import test from 'node:test';
import assert from 'node:assert/strict';
import { chromium } from 'playwright';
import { authorizeGoogle } from '../src/google-login.mjs';
import { fillGoogleStep, googleAuthorizationURL, LoginBrowser } from '../src/login-browser.mjs';

const account = { address: 'owner@gmail.com' };
const credentials = { username: account.address, password: 'private-browser-fixture' };
const pageHTML = `<html><body><input id="identifierId"><button id="identifierNext">Next</button><script>
const values = {}; let submitted = 0;
document.querySelector('#identifierNext').onclick = () => {
 values.username = document.querySelector('#identifierId').value; submitted++;
 document.body.innerHTML = '<input name="Passwd" type="password"><button id="passwordNext">Next</button>';
 document.querySelector('#passwordNext').onclick = () => {
  values.password = document.querySelector('[name=Passwd]').value; submitted++;
  document.body.innerHTML = '<input id="totpPin"><button id="totpNext">Next</button>';
  document.querySelector('#totpNext').onclick = () => { values.otp = document.querySelector('#totpPin').value; submitted++; document.body.innerHTML = '<p>Consent required</p>'; };
 };
};
</script></body></html>`;
test('authorization allowlist rejects lookalike hosts, extra scopes and non-loopback callbacks', () => {
  const value = new URL('https://accounts.google.com/o/oauth2/v2/auth');
  for (const [name, data] of Object.entries({ redirect_uri: 'http://127.0.0.1:4567/oauth2callback', response_type: 'code', code_challenge_method: 'S256', state: 'fixture', code_challenge: 'fixture', scope: 'https://www.googleapis.com/auth/gmail.readonly' })) value.searchParams.set(name, data);
  assert.equal(googleAuthorizationURL(value.href), true);
  const badHost = new URL(value); badHost.hostname += '.evil.example'; assert.equal(googleAuthorizationURL(badHost.href), false);
  const badScope = new URL(value); badScope.searchParams.set('scope', 'https://mail.google.com/'); assert.equal(googleAuthorizationURL(badScope.href), false);
  const badRedirect = new URL(value); badRedirect.searchParams.set('redirect_uri', 'https://example.org/collect'); assert.equal(googleAuthorizationURL(badRedirect.href), false);
});
test('real isolated browser fills username, password and fresh OTP once and refuses credential entry on another origin', async t => {
  const browser = await chromium.launch({ channel: process.env.BROWSER_CHANNEL, headless: true }); t.after(() => browser.close());
  const context = await browser.newContext(); const page = await context.newPage();
  await context.route('https://accounts.google.com/**', route => route.fulfill({ contentType: 'text/html', body: pageHTML }));
  await context.route('https://example.org/**', route => route.fulfill({ contentType: 'text/html', body: pageHTML }));
  await page.goto('https://accounts.google.com/signin/fixture'); const completed = new Set(); let otpCalls = 0;
  const otp = async () => { otpCalls++; return '123456'; };
  assert.equal(await fillGoogleStep(page, account, credentials, otp, completed), 'username');
  assert.equal(await fillGoogleStep(page, account, credentials, otp, completed), 'password');
  assert.equal(await fillGoogleStep(page, account, credentials, otp, completed), 'verification_code');
  assert.deepEqual(await page.evaluate(() => values), { username: account.address, password: credentials.password, otp: '123456' });
  assert.equal(await page.evaluate(() => submitted), 3); assert.equal(otpCalls, 1);
  assert.equal(await fillGoogleStep(page, account, credentials, otp, completed), 'owner_action_required');
  await page.goto('https://example.org/signin');
  assert.equal(await fillGoogleStep(page, account, credentials, otp, new Set()), 'outside_google');
  assert.equal(await page.locator('#identifierId').inputValue(), '');
  await page.goto('https://accounts.google.com/signin/fixture');
  await page.evaluate(() => document.querySelector('#identifierId').setAttribute('aria-invalid', 'true'));
  assert.equal(await fillGoogleStep(page, account, credentials, otp, new Set()), 'credentials_rejected');
  await page.goto('https://accounts.google.com/signin/oauth/consent');
  await page.setContent('<button onclick="window.consentClicks = (window.consentClicks || 0) + 1">Allow</button>');
  assert.equal(await fillGoogleStep(page, account, credentials, otp, new Set(), false), 'owner_action_required');
  assert.equal(await page.evaluate(() => window.consentClicks || 0), 0);
  const consent = new Set();
  assert.equal(await fillGoogleStep(page, account, credentials, otp, consent, true), 'google_consent');
  assert.equal(await fillGoogleStep(page, account, credentials, otp, consent, true), 'owner_action_required');
  assert.equal(await page.evaluate(() => window.consentClicks), 1);
  await page.goto('https://accounts.google.com/v3/signin/challenge/pk');
  await page.setContent(`<button id="other">Try another way</button><script>document.querySelector('#other').onclick = () => {
    document.body.innerHTML = '<button id="password">Use your password</button>';
    document.querySelector('#password').onclick = () => { document.body.innerHTML = '<input name="Passwd" type="password"><button id="passwordNext">Next</button>'; };
  };</script>`);
  const fallback = new Set();
  assert.equal(await fillGoogleStep(page, account, credentials, otp, fallback), 'another_method');
  assert.equal(await fillGoogleStep(page, account, credentials, otp, fallback), 'password_option');
  assert.equal(await fillGoogleStep(page, account, credentials, otp, fallback), 'password');
  assert.equal(await page.locator('input[name="Passwd"]').inputValue(), credentials.password);
  await page.goto('https://accounts.google.com/v3/signin/challenge/selection');
  await page.setContent('<a href="#" onclick="event.preventDefault(); window.passwordChoice = true">输入您的密码</a>');
  const localized = new Set();
  assert.equal(await fillGoogleStep(page, account, credentials, otp, localized), 'password_option');
  assert.equal(await page.evaluate(() => window.passwordChoice), true);
  assert.equal(await fillGoogleStep(page, account, credentials, otp, localized), 'owner_action_required');
  await page.setContent(`<a href="#" id="otp">从 Google 身份验证器应用获取验证码</a><script>document.querySelector('#otp').onclick = event => {
    event.preventDefault(); document.body.innerHTML = '<input id="totpPin"><button id="totpNext">Next</button>';
  };</script>`);
  const totpChoice = new Set();
  assert.equal(await fillGoogleStep(page, account, credentials, otp, totpChoice), 'otp_option');
  assert.equal(await fillGoogleStep(page, account, credentials, otp, totpChoice), 'verification_code');
  assert.equal(await page.locator('#totpPin').inputValue(), '123456'); assert.equal(otpCalls, 2);
});
test('browser hands the real PKCE loopback callback to the Google SDK and serializes login windows', async t => {
  const starts = [], stages = []; let active = 0; let maximum = 0;
  const launcher = async options => {
    active++; maximum = Math.max(maximum, active);
    const browser = await chromium.launch({ ...options, channel: process.env.BROWSER_CHANNEL, headless: true });
    return { async newContext(opts) {
      const context = await browser.newContext(opts);
      await context.route('https://accounts.google.com/**', route => {
        const url = new URL(route.request().url()); starts.push(url.searchParams.get('state'));
        if (!url.searchParams.has('redirect_uri')) return route.fulfill({ status: 204 });
        const destination = new URL(url.searchParams.get('redirect_uri')); destination.searchParams.set('state', url.searchParams.get('state')); destination.searchParams.set('code', 'fixture-code');
        return route.fulfill({ contentType: 'text/html', body: `<script>setTimeout(() => location.assign(${JSON.stringify(destination.href)}), 50)</script>` });
      });
      return context;
    }, async close() { active--; await browser.close(); } };
  };
  const broker = { async credentials() { return credentials; } };
  const driver = new LoginBrowser(broker, {}, launcher); t.after(() => driver.close());
  const signal = new AbortController().signal;
  const flow = () => driver.enqueue(() => authorizeGoogle({ client_id: 'fixture', client_secret: 'fixture' }, ['https://www.googleapis.com/auth/gmail.readonly'], {
    signal,
    clientFactory: options => ({
      async generateCodeVerifierAsync() { return { codeVerifier: 'fixture-verifier', codeChallenge: 'fixture-challenge' }; },
      generateAuthUrl(params) { return 'https://accounts.google.com/o/oauth2/v2/auth?' + new URLSearchParams({ ...params, scope: params.scope.join(' '), redirect_uri: options.redirectUri, response_type: 'code' }); },
      async getToken(params) { assert.equal(params.codeVerifier, 'fixture-verifier'); assert.equal(params.code, 'fixture-code'); return { tokens: { refresh_token: 'fixture-refresh' } }; },
      setCredentials(value) { this.credentials = value; },
    }),
    openBrowser: url => driver.login(account, url, { signal, progress: stage => stages.push(stage) }),
  }));
  const results = await Promise.all([flow(), flow()]);
  assert.equal(maximum, 1); assert.equal(starts.length, 2); assert.notEqual(starts[0], starts[1]);
  assert.ok(stages.includes('opening_browser')); assert.ok(results.every(result => result.credentials.refresh_token === 'fixture-refresh'));
});
