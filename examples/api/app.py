"""The api example (Python, Dockerfile): a tiny JSON API with the standard
library only, so the image builds in seconds. Shows the Dockerfile build path
and the non-root requirement (USER 1000)."""
import json
import os
import socket
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HOST = socket.gethostname()
STARTED = time.time()
VERSION = os.environ.get("API_VERSION", "1")


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz":
            return self.reply(200, {"status": "ok"})
        if self.path == "/time":
            return self.reply(200, {"time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())})
        if self.path == "/":
            return self.reply(200, {
                "service": "api", "version": VERSION, "instance": HOST,
                "uptime_s": int(time.time() - STARTED), "endpoints": ["/", "/healthz", "/time"],
            })
        self.reply(404, {"error": "not found"})

    def reply(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, fmt, *args):
        print(json.dumps({"level": "info", "msg": "request", "path": self.path, "status": args[1] if len(args) > 1 else "", "instance": HOST}), flush=True)


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "8080"))
    print(json.dumps({"level": "info", "msg": "api listening", "port": port, "version": VERSION}), flush=True)
    ThreadingHTTPServer(("", port), Handler).serve_forever()
