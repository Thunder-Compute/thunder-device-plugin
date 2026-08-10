#!/usr/bin/env python3
"""A stand-in for the Thunder Central API, for local testing only.

Serves the three read endpoints the operator needs to build its inventory, so
the real operator binary can publish real ResourceSlices and DeviceClasses
without a Thunder account.

Inventory is read from a JSON file on every request rather than cached, so a
test can rewrite the file to simulate hardware being enrolled or removed and
watch the operator react.

Usage: thunder-stub.py <port> <inventory-file>
"""

import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse

INVENTORY_PATH = sys.argv[2]
KEYS = {
    "/api/v1/zones": "zones",
    "/api/v1/hosts": "hosts",
    "/api/v1/clients": "clients",
}


def read_inventory():
    try:
        with open(INVENTORY_PATH) as handle:
            return json.load(handle)
    except (OSError, ValueError):
        return {}


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        key = KEYS.get(urlparse(self.path).path)
        if key is None:
            self.send_response(404)
            self.end_headers()
            return
        payload = json.dumps({key: read_inventory().get(key, [])}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
