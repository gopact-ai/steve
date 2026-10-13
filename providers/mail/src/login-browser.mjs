import { chromium } from 'playwright';
import { setTimeout as delay } from 'node:timers/promises';
import { SetupError } from './errors.mjs';

export function googleAuthorizationURL(value) {
  try {
    const url = new URL(value); const redirect = new URL(url.searchParams.get('redirect_uri'));
    return url.origin === 'https://accounts.google.com' && url.pathname === '/o/oauth2/v2/auth' &&
      !url.username && !url.password && redirect.origin.startsWith('http://127.0.0.1:') && redirect.pathname === '/oauth2callback' && !redirect.username && !redirect.password && !redirect.search && !redirect.hash &&
      url.searchParams.get('response_type') === 'code' && url.searchParams.get('code_challenge_method') === 'S256' &&
      !!url.searchParams.get('state') && !!url.searchParams.get('code_challenge') &&
      url.searchParams.get('scope') === 'https://www.googleapis.com/auth/gmail.readonly';
  } catch { return false; }
}

export function isGooglePage(value) {
  try { return new URL(value).origin === 'https://accounts.google.com'; } catch { return false; }
}

// Fixed selectors and fixed origins: pages never become agent instructions.
// Each field is submitted once, so rejected credentials cannot cause retry loops.
export async function fillGoogleStep(page, account, credentials, otp, completed, approveConsent = false) {
  if (!isGooglePage(page.url())) return 'outside_google';
  if (await page.locator('input[aria-invalid="true"]').count()) return 'credentials_rejected';
  const stages = [
    { name: 'username', selector: '#identifierId', next: '#identifierNext', value: () => credentials.username },
    { name: 'password', selector: 'input[name="Passwd"]', next: '#passwordNext', value: () => credentials.password },
    { name: 'verification_code', selector: '#totpPin', next: '#totpNext', value: otp },
  ];
  for (const stage of stages) {
    const field = page.locator(stage.selector);
    if (!await field.isVisible()) continue;
    if (completed.has(stage.name)) return 'waiting_for_google';
    // Check again at the point of credential entry, rather than trusting an old URL.
    if (!isGooglePage(page.url())) return 'outside_google';
    const value = await stage.value();
    if (!isGooglePage(page.url())) return 'outside_google';
    await field.fill(value, { timeout: 5000 });
    completed.add(stage.name);
    await page.locator(stage.next).click({ timeout: 5000 });
    return stage.name;
  }
  if (new URL(page.url()).pathname.startsWith('/v3/signin/challenge/') || new URL(page.url()).pathname.startsWith('/signin/v2/challenge/')) {
    for (const [name, expression] of [
      ['password_option', /^(Use your password|Enter your password|使用密码|使用密碼|输入密码|輸入密碼)$/],
      ['another_method', /^(Try another way|尝试其他方式|嘗試其他方式)$/],
    ]) {
      const button = page.getByRole('button', { name: expression });
      const step = `${name}:${new URL(page.url()).pathname}`;
      if (!completed.has(step) && await button.count() === 1 && await button.isVisible()) {
        await button.click({ timeout: 5000 }); completed.add(step); return name;
      }
    }
  }
  const choice = page.locator('[data-identifier]');
  const count = await choice.count();
  if (!completed.has('account_choice')) for (let i = 0; i < count; i++) {
    const option = choice.nth(i);
    if ((await option.getAttribute('data-identifier'))?.toLowerCase() === account.address.toLowerCase() && await option.isVisible()) {
      await option.click({ timeout: 5000 }); completed.add('account_choice'); return 'account_choice';
    }
  }
  if (approveConsent && new URL(page.url()).pathname.startsWith('/signin/oauth/')) {
    const button = page.getByRole('button', { name: /^(Continue|Allow|继续|繼續|允许|允許)$/ });
    if (await button.count() === 1 && await button.isVisible()) {
      const step = `consent:${new URL(page.url()).pathname}`;
      if (!completed.has(step)) { await button.click({ timeout: 5000 }); completed.add(step); return 'google_consent'; }
    }
  }
  return 'owner_action_required';
}

export class LoginBrowser {
  constructor(onepassword, config = {}, launch = options => chromium.launch(options)) {
    this.onepassword = onepassword; this.config = config; this.launch = launch; this.tail = Promise.resolve(); this.sessions = new Set();
  }
  login(account, url, hooks) {
    if (!googleAuthorizationURL(url)) return Promise.reject(new SetupError('Automatic login requires the official Gmail read-only OAuth request'));
    return this.drive(account, url, hooks);
  }
  enqueue(operation) {
    // Queue the entire SDK flow, before its five-minute OAuth timer starts.
    const task = this.tail.catch(() => {}).then(operation);
    this.tail = task; return task;
  }
  async drive(account, url, { signal, progress }) {
    if (signal.aborted) throw new SetupError('Automatic login cancelled');
    let browser, context, credentials;
    const deadline = Date.now() + 270000; // Includes vault approval, inside the SDK's five-minute timeout.
    const abort = () => { void browser?.close().catch(() => {}); };
    signal.addEventListener('abort', abort, { once: true });
    try {
      progress('onepassword_approval');
      credentials = await this.onepassword.credentials(account, signal);
      progress('opening_browser');
      browser = await this.launch({ headless: false, ...(this.config.channel === 'chromium' ? {} : { channel: 'chrome' }) });
      this.sessions.add(browser);
      context = await browser.newContext({ acceptDownloads: false });
      // No tracing, HAR, video or shared user profile. Credentials stay in memory.
      const page = await context.newPage();
      await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 30000 });
      const redirect = new URL(url).searchParams.get('redirect_uri');
      const completed = new Set();
      while (!signal.aborted && Date.now() < deadline && !page.isClosed()) {
        if (page.url().startsWith(redirect + '?')) {
          await page.waitForLoadState('domcontentloaded', { timeout: 10000 });
          return; // The SDK still validates state, tokens and mailbox identity.
        }
        let stage, hint;
        try { stage = await fillGoogleStep(page, account, credentials, () => this.onepassword.otp(account, signal), completed, this.config.approve_readonly_consent === true); }
        catch (error) {
          stage = 'owner_action_required'; hint = error instanceof SetupError ? error.message : 'Complete the current Google step in the Steve login browser';
        }
        if (stage === 'credentials_rejected') throw new SetupError('Google rejected the saved credentials; update the 1Password item before retrying');
        if (stage === 'outside_google') throw new SetupError('The login browser left the official Google sign-in origin');
        if (stage === 'owner_action_required') progress(stage, hint || 'Complete Google consent, passkey or additional verification in the Steve login browser');
        else progress(stage);
        await delay(750, undefined, { signal });
      }
      throw new SetupError(signal.aborted ? 'Automatic login cancelled' : 'Automatic login timed out or the browser was closed; complete required Google verification before retrying');
    } catch (error) {
      if (error instanceof SetupError) throw error;
      throw new SetupError('The local login browser is unavailable; install Chrome and retry on the 1Password host');
    } finally {
      credentials = undefined;
      signal.removeEventListener('abort', abort);
      if (browser) { this.sessions.delete(browser); await browser.close().catch(() => {}); }
    }
  }
  async close() { await Promise.all([...this.sessions].map(browser => browser.close().catch(() => {}))); }
}
