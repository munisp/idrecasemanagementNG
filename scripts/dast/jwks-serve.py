# Author: Patrick Munis at newwave
# Serves jwks.json at the Keycloak certs path shape so case-api's JWKS fetch
# (KEYCLOAK_JWKS_URL) works without running Keycloak:
#   http://127.0.0.1:8085/auth/realms/idre/protocol/openid-connect/certs
import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))


class JWKSHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.endswith("/protocol/openid-connect/certs"):
            with open(os.path.join(HERE, "jwks.json"), "rb") as f:
                body = f.read()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", 8085), JWKSHandler).serve_forever()
