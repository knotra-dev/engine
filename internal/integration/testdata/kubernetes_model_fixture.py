"""Controllable Ollama wire fixture for isolated Kubernetes acceptance tests."""

import json
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    condition = threading.Condition()
    calls = 0
    waiting = 0
    hold = False

    def log_message(self, *_args):
        pass

    def reply(self, value, status=200):
        data = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()

        try:
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def do_GET(self):
        if self.path == "/control":
            with self.condition:
                self.reply(
                    {
                        "calls": Handler.calls,
                        "waiting": Handler.waiting,
                        "hold": Handler.hold,
                    }
                )
        elif self.path == "/api/tags":
            self.reply({"models": [{"name": "fixture", "digest": "fixture-digest"}]})
        else:
            self.reply({"error": "unknown fixture route"}, 404)

    def do_POST(self):
        data = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        if self.path == "/control":
            try:
                value = json.loads(data)
            except (ValueError, UnicodeDecodeError):
                self.reply({"error": "expected JSON hold boolean"}, 400)
                return
            if not isinstance(value, dict) or type(value.get("hold")) is not bool:
                self.reply({"error": "expected JSON hold boolean"}, 400)
                return

            with self.condition:
                Handler.hold = value["hold"]
                self.condition.notify_all()
            self.reply({"hold": value["hold"]})
        elif self.path == "/api/show":
            self.reply({"capabilities": ["completion"]})
        elif self.path == "/api/chat":
            with self.condition:
                Handler.calls += 1
                Handler.waiting += 1
                try:
                    self.condition.wait_for(lambda: not Handler.hold)
                finally:
                    Handler.waiting -= 1
            self.reply(
                {
                    "message": {
                        "role": "assistant",
                        "content": '{"answer":"shared remote result"}',
                    },
                    "done": True,
                    "done_reason": "stop",
                }
            )
        else:
            self.reply({"error": "unknown fixture route"}, 404)


if __name__ == "__main__":
    ThreadingHTTPServer(
        ("0.0.0.0", int(os.environ.get("PORT", "11434"))),
        Handler,
    ).serve_forever()
