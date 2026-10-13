// Only errors written by this provider may expose a message in logs. SDK
// failures can contain request URLs, headers or upstream credential details.
export class SetupError extends Error {
  constructor(message) { super(message); this.name = 'SetupError'; }
}
