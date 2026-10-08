#!/usr/bin/env python3
"""A tiny JSON REST API for testing examples/resources/scripted_resource/rest_api.py.

    POST   /widgets        -> 201 {id, ...fields, created_at}
    GET    /widgets/{id}   -> 200 object | 404
    PUT    /widgets/{id}   -> 200 object | 404
    DELETE /widgets/{id}   -> 204 | 404

Requires "Authorization: Bearer <token>" with the token given as argv[2];
argv[1] is the port. Prints "ready" on stdout once listening.
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT, TOKEN = int(sys.argv[1]), sys.argv[2]
objects = {}
counter = [0]


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def send(self, code, obj=None):
        raw = json.dumps(obj).encode() if obj is not None else b""
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def body(self):
        n = int(self.headers.get("Content-Length") or 0)
        return json.loads(self.rfile.read(n)) if n else {}

    def handle_any(self):
        if self.headers.get("Authorization") != "Bearer " + TOKEN:
            return self.send(401, {"error": "unauthorized"})
        parts = self.path.strip("/").split("/")
        if parts[0] != "widgets":
            return self.send(404, {"error": "no such route"})
        if len(parts) == 1 and self.command == "POST":
            counter[0] += 1
            obj = dict(self.body(), id="w%d" % counter[0], created_at="2026-10-08T00:00:00Z")
            objects[obj["id"]] = obj
            return self.send(201, obj)
        if len(parts) == 2:
            oid = parts[1]
            if oid not in objects:
                return self.send(404, {"error": "not found"})
            if self.command == "GET":
                return self.send(200, objects[oid])
            if self.command == "PUT":
                objects[oid] = dict(self.body(), id=oid, created_at=objects[oid]["created_at"])
                return self.send(200, objects[oid])
            if self.command == "DELETE":
                del objects[oid]
                return self.send(204)
        return self.send(405, {"error": "unsupported"})

    do_GET = do_POST = do_PUT = do_DELETE = handle_any


server = HTTPServer(("127.0.0.1", PORT), Handler)
print("ready", flush=True)
server.serve_forever()
