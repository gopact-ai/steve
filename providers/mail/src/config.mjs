import { SetupError } from './errors.mjs';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';

export function loadConfig(path, { managed = false } = {}) {
  const config = JSON.parse(readFileSync(path, 'utf8'));
  const ids = new Set();
  config.accounts ??= [];
  if (!Array.isArray(config.accounts) || (!managed && !config.accounts.length)) throw new SetupError('Configure at least one mail account');
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
  config.admin_token_env ||= 'STEVE_MAIL_ADMIN_TOKEN';
  config.admin_port ??= 8787;
  if (!Number.isInteger(config.admin_port) || config.admin_port < 0 || config.admin_port > 65535) throw new SetupError('Invalid mail administration port');
  if (config.onepassword !== undefined) {
    const value = config.onepassword;
    if (!value || !Array.isArray(value.vaults) || !value.vaults.length || value.vaults.length > 20 ||
        !value.vaults.every(vault => typeof vault === 'string' && vault.trim() === vault && vault.length > 0 && vault.length <= 80 && !/[\x00-\x1f]/.test(vault)) ||
        (value.command !== undefined && (typeof value.command !== 'string' || !value.command || /[\x00-\x1f]/.test(value.command))) ||
        (value.account !== undefined && (typeof value.account !== 'string' || !value.account || /[\x00-\x1f]/.test(value.account))) ||
        (value.auto_login !== undefined && typeof value.auto_login !== 'boolean') ||
        (value.approve_readonly_consent !== undefined && typeof value.approve_readonly_consent !== 'boolean') ||
        (value.channel !== undefined && !['chrome', 'chromium'].includes(value.channel))) throw new SetupError('Configure allowed 1Password vaults, CLI command and browser login options');
  }
  return config;
}

export function requiredEnv(name) {
  const value = process.env[name];
  if (!value) throw new SetupError(`Set ${name} in the provider environment`);
  return value;
}
