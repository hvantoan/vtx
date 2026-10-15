#!/usr/bin/env bash
# ==============================================================================
# Generate a self-signed TLS cert/key pair for the VTX webhook gateway.
# Output: infra/compose/nginx/certs/{gateway.crt,gateway.key}
# The certs/ directory is gitignored — private keys must never enter the tree.
# Local/dev use only; production should mount a CA-issued certificate.
# ==============================================================================
set -euo pipefail

CERT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/certs"
mkdir -p "$CERT_DIR"

openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "$CERT_DIR/gateway.key" \
    -out "$CERT_DIR/gateway.crt" \
    -days 825 \
    -subj "/CN=localhost/O=VTX Webhook Gateway (dev)" \
    -addext "subjectAltName=DNS:localhost,DNS:gateway,IP:127.0.0.1"

chmod 600 "$CERT_DIR/gateway.key"
chmod 644 "$CERT_DIR/gateway.crt"

echo "Generated dev TLS assets in $CERT_DIR (gitignored, not for production)."
