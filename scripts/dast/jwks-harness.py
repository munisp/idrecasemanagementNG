# Author: Patrick Munis at newwave
# JWKS harness for DAST runs: replaces Keycloak in environments where the
# full IdP cannot run. Mints RS256 tokens that case-api verifies through its
# REAL signature/issuer/tenant-claim path — only the token issuer is harnessed.
#
#   python3 jwks-harness.py gen                      # write key.json + jwks.json
#   python3 jwks-harness.py mint <user> <groups> <roles> [ttl_sec] [issuer]
#
# groups: comma-separated (e.g. /tenant/tx,/tenant/ny); roles: comma-separated.
# ISSUER default matches the dev realm; KID is fixed so JWKS rotation is simple.
import json
import os
import sys
import time
import uuid

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa
import jwt

ISSUER = "http://localhost/auth/realms/idre"
KID = "dast-key-1"


def gen(out_dir="."):
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    priv = key.private_bytes(
        serialization.Encoding.PEM,
        serialization.PrivateFormat.PKCS8,
        serialization.NoEncryption(),
    ).decode()
    pub = key.public_key().public_bytes(
        serialization.Encoding.PEM,
        serialization.PublicFormat.SubjectPublicKeyInfo,
    ).decode()
    with open(f"{out_dir}/key.json", "w") as f:
        json.dump({"private_pem": priv, "public_pem": pub}, f)
    import jwt as _jwt
    jwk = json.loads(_jwt.algorithms.RSAAlgorithm.to_jwk(key.public_key()))
    jwk.update({"kid": KID, "use": "sig", "alg": "RS256"})
    with open(f"{out_dir}/jwks.json", "w") as f:
        json.dump({"keys": [jwk]}, f)
    print(f"generated {out_dir}/key.json + {out_dir}/jwks.json (kid={KID})")


def mint(user, groups, roles, ttl=900, iss=ISSUER, key_dir=None):
    if key_dir is None:
        key_dir = os.path.dirname(os.path.abspath(__file__))
    with open(f"{key_dir}/key.json") as f:
        priv = json.load(f)["private_pem"]
    now = int(time.time())
    claims = {
        "iss": iss,
        "sub": str(uuid.uuid5(uuid.NAMESPACE_DNS, f"idre-dast:{user}")),
        "preferred_username": user,
        "iat": now,
        "exp": now + int(ttl),
        "groups": [g for g in groups.split(",") if g],
        "realm_access": {"roles": [r for r in roles.split(",") if r]},
        "typ": "Bearer",
    }
    return jwt.encode(claims, priv, algorithm="RS256", headers={"kid": KID})


if __name__ == "__main__":
    if len(sys.argv) >= 2 and sys.argv[1] == "gen":
        gen(sys.argv[2] if len(sys.argv) > 2 else ".")
    elif len(sys.argv) >= 5 and sys.argv[1] == "mint":
        ttl = int(sys.argv[5]) if len(sys.argv) > 5 else 900
        iss = sys.argv[6] if len(sys.argv) > 6 else ISSUER
        print(mint(sys.argv[2], sys.argv[3], sys.argv[4], ttl, iss))
    else:
        print(__doc__)
        sys.exit(2)
