import { SetupError } from './errors.mjs';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';

export function loadConfig(path) {
  const config = JSON.parse(readFileSync(path, 'utf8'));
  const ids = new Set();
  if (!Array.isArray(config.accounts) || !config.accounts.length) throw new SetupError('Configure at least one mail account');
  for (const account of config.accounts) {
    if (!/^[a-zA-Z0-9_-]{1,64}$/.test(account.id) || ids.has(account.id)) throw new SetupError('Account IDs must be unique stable names');
    ids.add(account.id);
    if (!['gmail', 'outlook', 'hotmail', 'icloud'].includes(account.provider)) throw new SetupError('Unsupported mail provider');
    if (typeof account.address !== 'string' || !account.address.includes('@')) throw new SetupError('Each account needs its login address');
    if (account.provider === 'gmail') {
      if (!account.credentials_file) throw new SetupError('Gmail needs a Google desktop OAuth credentials_file');
      account.credentials_file = resolve(dirname(path), account.credentials_file);
    }
    if (['outlook', 'hotmail'].includes(account.provider) && !account.client_id) throw new SetupError('Microsoft accounts need a public-client application client_id');
  }
  const url = new URL(config.steve_url);
  if (url.protocol !== 'https:' && !(url.protocol === 'http:' && ['127.0.0.1', 'localhost', '[::1]'].includes(url.hostname))) {
    throw new SetupError('Use HTTPS or a local SSH tunnel for the Steve connection');
  }
  if (url.username || url.password || url.search || url.hash || url.pathname !== '/') throw new SetupError('steve_url must be an origin');
  config.steve_url = url.origin;
  config.state_dir = resolve(dirname(path), config.state_dir || 'state');
  config.poll_seconds ??= 60;
  if (!Number.isFinite(config.poll_seconds) || config.poll_seconds < 15) throw new SetupError('poll_seconds must be at least 15');
  config.steve_token_env ||= 'STEVE_MAIL_CONNECTION_TOKEN';
  config.secrets_key_env ||= 'STEVE_MAIL_SECRETS_KEY';
  return config;
}

export function requiredEnv(name) {
  const value = process.env[name];
  if (!value) throw new SetupError(`Set ${name} in the provider environment`);
  return value;
}
