import { createServer } from 'node:http';
import { timingSafeEqual } from 'node:crypto';
import { SetupError } from './errors.mjs';
import { errorCode } from './runner.mjs';

async function readBody(request) {
  const chunks = []; let length = 0;
  for await (const chunk of request) {
    length += chunk.length;
    if (length > 8 * 1024 * 1024) throw new SetupError('Request exceeds the mail import limit');
    chunks.push(chunk);
  }
  try {
    const value = JSON.parse(Buffer.concat(chunks).toString() || '{}');
    if (!value || Array.isArray(value) || typeof value !== 'object') throw new Error();
    return value;
  } catch { throw new SetupError('Expected a JSON object'); }
}

export async function adminServer(manager, token, port = 8787, worker) {
  if (typeof token !== 'string' || token.length < 32) throw new SetupError('Use a mail administration token of at least 32 characters');
  const server = createServer(async (request, response) => {
    response.setHeader('Cache-Control', 'no-store'); response.setHeader('Content-Type', 'application/json; charset=utf-8');
    const candidate = Buffer.from((request.headers.authorization || '').replace(/^Bearer /, ''));
    // Only Steve's owner-guarded proxy talks to this API. No browser CORS, cookies or redirect authentication.
    if (request.headers.origin || candidate.length !== Buffer.byteLength(token) || !timingSafeEqual(candidate, Buffer.from(token))) {
      response.writeHead(401); response.end(JSON.stringify({ error: 'Mail administration is unauthorized' })); return;
    }
    try {
      const url = new URL(request.url, 'http://127.0.0.1');
      const parts = url.pathname.split('/').filter(Boolean); const [resource, id, action] = parts;
      if (parts.length > 3 || url.search) throw new SetupError('Unknown mail administration route');
      const body = request.method === 'GET' ? {} : await readBody(request);
      let value;
      if (request.method === 'GET' && resource === 'accounts' && !id) value = manager.snapshot();
      else if (request.method === 'POST' && resource === 'accounts' && !id) value = manager.add(body);
      else if (request.method === 'POST' && resource === 'accounts' && id === 'batch' && !action) value = manager.batch(body);
      else if (request.method === 'PATCH' && resource === 'accounts' && id && !action) value = manager.update(id, body);
      else if (request.method === 'DELETE' && resource === 'accounts' && id && !action) value = manager.archive(id);
      else if (request.method === 'POST' && resource === 'accounts' && id && action === 'login') value = manager.jobs.start(id);
      else if (request.method === 'POST' && resource === 'accounts' && id && action === 'sync') {
        value = manager.startSync(id);
      } else if (request.method === 'GET' && resource === 'logins' && id && !action) value = manager.jobs.status(id);
      else if (request.method === 'POST' && resource === 'imports' && id === 'preview' && !action) value = manager.preview(body);
      else if (request.method === 'GET' && resource === 'imports' && id && !action) value = manager.draft(id);
      else if (request.method === 'POST' && resource === 'imports' && id === 'commit' && !action) value = manager.commit(body);
      else if (request.method === 'POST' && resource === 'exports' && !id) value = manager.export(body);
      else if (request.method === 'PUT' && resource === 'apps' && !id) value = manager.configureApps(body);
      else if (request.method === 'PUT' && resource === 'ai' && !id && worker) value = await worker.configure(body);
      else if (request.method === 'POST' && resource === 'agent' && id === 'invoke' && !action && worker) value = await worker.invoke(body);
      else throw new SetupError('Unknown mail administration route');
      response.end(JSON.stringify(value));
    } catch (error) {
      response.writeHead(error instanceof SetupError ? 400 : 503);
      response.end(JSON.stringify({ code: errorCode(error), error: error instanceof SetupError ? error.message : 'Mail operation failed; check the provider and retry' }));
    }
  });
  server.requestTimeout = 150000; server.headersTimeout = 10000;
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(port, '127.0.0.1', resolve); });
  return { url: `http://127.0.0.1:${server.address().port}`, close: () => new Promise(resolve => { server.closeIdleConnections(); server.close(resolve); }) };
}
