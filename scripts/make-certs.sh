#!/usr/bin/env bash
# usage: scripts/make-certs.sh <seedctl-hostname>
# generates: certs/ca.{crt,key}, certs/server.{crt,key}, certs/client.{crt,key}
# the seedctl hostname becomes the server cert's CN and primary SAN.

set -euo pipefail

HOSTNAME="${1:-}"
if [[ -z "$HOSTNAME" ]]; then
  echo "usage: $0 <seedctl-hostname-or-ip>" >&2
  exit 1
fi

CERTS=certs
mkdir -p "$CERTS"

# CA — 10 years
if [[ ! -f "$CERTS/ca.key" ]]; then
  openssl ecparam -genkey -name prime256v1 -noout -out "$CERTS/ca.key"
  openssl req -x509 -new -nodes -key "$CERTS/ca.key" \
    -subj "/CN=torrent-manager-ca" \
    -days 3650 -out "$CERTS/ca.crt"
fi

# server cert
if [[ ! -f "$CERTS/server.key" ]]; then
  openssl ecparam -genkey -name prime256v1 -noout -out "$CERTS/server.key"
  openssl req -new -key "$CERTS/server.key" \
    -subj "/CN=${HOSTNAME}" -out "$CERTS/server.csr"

  # SAN config: support both hostname-as-DNS and hostname-as-IP
  SAN_KIND="DNS"
  if [[ "$HOSTNAME" =~ ^[0-9.]+$ ]] || [[ "$HOSTNAME" =~ : ]]; then
    SAN_KIND="IP"
  fi
  cat > "$CERTS/server.ext" <<EOF
subjectAltName = ${SAN_KIND}:${HOSTNAME}
extendedKeyUsage = serverAuth
EOF

  openssl x509 -req -in "$CERTS/server.csr" \
    -CA "$CERTS/ca.crt" -CAkey "$CERTS/ca.key" -CAcreateserial \
    -days 3650 -sha256 \
    -extfile "$CERTS/server.ext" \
    -out "$CERTS/server.crt"

  rm "$CERTS/server.csr" "$CERTS/server.ext"
fi

# client cert
if [[ ! -f "$CERTS/client.key" ]]; then
  openssl ecparam -genkey -name prime256v1 -noout -out "$CERTS/client.key"
  openssl req -new -key "$CERTS/client.key" \
    -subj "/CN=tmgr" -out "$CERTS/client.csr"

  cat > "$CERTS/client.ext" <<EOF
extendedKeyUsage = clientAuth
EOF

  openssl x509 -req -in "$CERTS/client.csr" \
    -CA "$CERTS/ca.crt" -CAkey "$CERTS/ca.key" -CAcreateserial \
    -days 3650 -sha256 \
    -extfile "$CERTS/client.ext" \
    -out "$CERTS/client.crt"

  rm "$CERTS/client.csr" "$CERTS/client.ext"
fi

chmod 600 "$CERTS"/*.key
echo "certs generated:"
ls -la "$CERTS"
