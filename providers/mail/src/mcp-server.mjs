import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { CallToolRequestSchema, ListToolsRequestSchema } from '@modelcontextprotocol/sdk/types.js';
import { mailCapabilities } from './client-worker.mjs';
import { SetupError } from './errors.mjs';

export function createMailMCP(url, token, fetcher = fetch) {
  const origin = new URL(url);
  if (origin.protocol !== 'http:' || origin.hostname !== '127.0.0.1' || origin.pathname !== '/' || origin.search || origin.hash || origin.username || origin.password || typeof token !== 'string' || token.length < 32) throw new SetupError('Use a loopback mail administration URL and a private administration token');
  const tools = mailCapabilities.map(capability => ({ name: capability.name.replaceAll('.', '_'), description: capability.description,
    inputSchema: { ...capability.input_schema, properties: { ...capability.input_schema.properties,
      request_id: { type: 'string', description: 'A fresh UUID for this call. Retry with the same UUID and identical arguments after a lost response; use a new UUID when polling status.' } },
    required: [...capability.input_schema.required, 'request_id'] }, capability: capability.name }));
  const server = new Server({ name: 'steve-mail-1password', version: '0.1.0' }, { capabilities: { tools: {} },
    instructions: 'Use mail_1password_status, mail_1password_preview, mail_import_commit, mail_login_start and mail_login_status to connect requested Gmail accounts. Only authorized means success. Passwords and OTPs never enter this MCP connection. Use a fresh request_id for each status poll. The 1Password official Environments MCP does not expose vault Login items.' });
  server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: tools.map(({ capability, ...tool }) => tool) }));
  server.setRequestHandler(CallToolRequestSchema, async request => {
    try {
      const tool = tools.find(tool => tool.name === request.params.name);
      if (!tool) throw new SetupError('Unsupported mailbox MCP tool');
      const { request_id, ...args } = request.params.arguments || {};
      const response = await fetcher(origin.origin + '/agent/invoke', { method: 'POST', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
        redirect: 'error', signal: AbortSignal.timeout(150000), body: JSON.stringify({ request_id, capability: tool.capability, arguments: args }) });
      if (!response.ok) {
        const failure = await response.json().catch(() => null);
        throw new SetupError(typeof failure?.error === 'string' && failure.error.length <= 512 ? failure.error : `Mailbox operation returned HTTP ${response.status}; check the local provider configuration and retry`);
      }
      const result = await response.json();
      return { content: [{ type: 'text', text: JSON.stringify(result) }] };
    } catch (error) {
      return { isError: true, content: [{ type: 'text', text: error instanceof SetupError ? error.message : 'The local mailbox provider is unavailable; no login success has been confirmed' }] };
    }
  });
  return server;
}

export async function runMailMCP(url, token) {
  const server = createMailMCP(url, token);
  await server.connect(new StdioServerTransport());
}
