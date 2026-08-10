#!/usr/bin/env python3
"""A stand-in for the Thunder Central API, for local testing only.

Serves the three read endpoints the operator needs to build its inventory, so
the real operator binary can publish real ResourceSlices without a Thunder
account. Inventory is passed in as JSON on the command line.

Usage: thunder-stub.py <port> <inventory-json>
"""

import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse

INVENTORY = json.loads(sys.argv[2]) if len(sys.argv) > 2 else {}
ROUTES = {
    "/api/v1/zones": lambda: {"zones": INVENTORY.get("zones", [])},
    "/api/v1/hosts": lambda: {"hosts": INVENTORY.get("hosts", [])},
    "/api/v1/clients": lambda: {"clients": INVENTORY.get("clients", [])},
}


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        route = ROUTES.get(urlparse(self.path).path)
        if route is None:
            self.send_response(404)
            self.end_headers()
            return
        payload = json.dumps(route()).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
