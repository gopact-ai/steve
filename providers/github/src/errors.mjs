// Only errors written by this provider may expose a message in logs. SDK
// failures can contain request URLs, headers or upstream details.
export class SetupError extends Error {
  constructor(message) { super(message); this.name = 'SetupError'; }
}

export function errorCode(error) {
  const code = error.status || error.statusCode || error.code || error.name || 'error';
  return /^[a-zA-Z0-9_-]{1,80}$/.test(String(code)) ? String(code) : 'error';
}
