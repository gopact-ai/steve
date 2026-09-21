"""Run the native preview regression against a disposable loopback fixture."""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import subprocess
import sys
import threading
import socket

HTML = Path(__file__).with_name("preview_fixture.html").read_bytes()
TOKEN = "isolated-preview-test-token-not-a-real-credential"


class Fixture(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.headers.get("Authorization") != f"Bearer {TOKEN}":
            self.send_error(401)
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(HTML)))
        self.end_headers()
        self.wfile.write(HTML)

    def log_message(self, *_):
        pass


class IPv6Server(ThreadingHTTPServer):
    address_family = socket.AF_INET6


for server_class, address, host in [
    (ThreadingHTTPServer, "127.0.0.1", "127.0.0.1"),
    (IPv6Server, "::1", "[::1]"),
]:
    with server_class((address, 0), Fixture) as server:
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            result = subprocess.run(
                [sys.argv[1], f"http://{host}:{server.server_port}/?token={TOKEN}"],
                timeout=60,
                check=False,
            )
        finally:
            server.shutdown()
            thread.join()
    if result.returncode:
        sys.exit(result.returncode)
