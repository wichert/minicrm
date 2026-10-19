#!/bin/sh
# Generate a self-signed CA and a server certificate for the local Dovecot container.
# Idempotent: existing certificates are reused so the volume can be persisted.
set -eu

DIR="${CERT_DIR:-/certs}"
mkdir -p "$DIR"

if [ -s "$DIR/tls.crt" ] && [ -s "$DIR/tls.key" ] && [ -s "$DIR/ca.crt" ]; then
    echo "certificates already present in $DIR"
    exit 0
fi

echo "generating development certificates in $DIR"

openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
    -keyout "$DIR/ca.key" -out "$DIR/ca.crt" \
    -subj "/CN=MiniCRM Dev Mail CA" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign"

openssl req -newkey rsa:2048 -nodes \
    -keyout "$DIR/tls.key" -out "$DIR/tls.csr" \
    -subj "/CN=dovecot"

cat > "$DIR/san.cnf" <<'EOF'
subjectAltName=DNS:dovecot,DNS:mail,DNS:smtp,DNS:mailpit,DNS:localhost,IP:127.0.0.1
extendedKeyUsage=serverAuth
keyUsage=critical,digitalSignature,keyEncipherment
EOF

openssl x509 -req -in "$DIR/tls.csr" \
    -CA "$DIR/ca.crt" -CAkey "$DIR/ca.key" -CAcreateserial \
    -out "$DIR/tls.crt" -days 3650 -sha256 -extfile "$DIR/san.cnf"

rm -f "$DIR/tls.csr" "$DIR/san.cnf"
chmod 644 "$DIR/ca.crt" "$DIR/tls.crt" "$DIR/tls.key"
chmod 600 "$DIR/ca.key"

echo "certificates ready"
