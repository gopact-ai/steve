// Serve the built console against an existing hub without deploying or
// restarting it. Only reads reach the hub; the preview cannot submit work.
import http from "node:http";
import { createReadStream } from "node:fs";
import { stat } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const dist = path.resolve(fileURLToPath(new URL("../../internal/readmodel/web/dist/", import.meta.url)));
const mime = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2" };

export async function preview(port = 0) {
    const hub = new URL(process.env.HUB || "http://127.0.0.1:7710");
    const server = http.createServer(async (req, res) => {
        if (!["GET", "HEAD"].includes(req.method)) { res.writeHead(405).end("Read-only preview"); return; }
        const url = new URL(req.url, "http://localhost");
        if (/^\/(state|events|history|console)(\/|$)/.test(url.pathname)) {
            const upstream = http.request(new URL(req.url, hub), { method: req.method, headers: req.headers }, (reply) => {
                res.writeHead(reply.statusCode, reply.headers);
                reply.pipe(res);
            });
            upstream.on("error", () => { if (!res.headersSent) res.writeHead(502); res.end(); });
            res.on("close", () => upstream.destroy());
            upstream.end();
            return;
        }
        const file = path.resolve(dist, "." + (url.pathname === "/" ? "/index.html" : url.pathname));
        if (!file.startsWith(dist + path.sep)) { res.writeHead(403).end(); return; }
        try {
            if (!(await stat(file)).isFile()) throw new Error("Not a file");
            res.setHeader("Content-Type", mime[path.extname(file)] || "application/octet-stream");
            res.setHeader("Cache-Control", "no-store");
            if (req.method === "HEAD") res.end();
            else createReadStream(file).pipe(res);
        } catch { res.writeHead(404).end(); }
    });
    await new Promise((resolve) => server.listen(port, "127.0.0.1", resolve));
    return { url: `http://127.0.0.1:${server.address().port}`, close: () => { server.closeAllConnections(); server.close(); } };
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
    const app = await preview(Number(process.env.PORT || 17711));
    console.log(`Read-only console preview: ${app.url}/#/console (supply the hub token in the URL)`);
}
