#!/bin/env bash

# Copyright Solo.io, Inc
#
# Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

set -euo pipefail
set -x

# use:
# ./generate-app-cert.sh --name <app-name> --out <out-dir>

# Parse flags
while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)
      OUT_DIR="$2"
      shift 2
      ;;
    --name)
      NAME="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done

WD=$(cd "$OUT_DIR"; pwd)
CA_DIR="$(dirname "$0")/../pilot"
CA_KEY_FILE="$CA_DIR/ca-key.pem"
CA_CERT_FILE="$CA_DIR/root-cert.pem"

if [[ ! -f "$CA_KEY_FILE" || ! -f "$CA_CERT_FILE" ]]; then
  echo "Error: CA key or cert file not found"
  exit 1
fi

# Create a config file for the app
CONF_FILE="$WD/${NAME}.conf"
if [[ ! -f "$CONF_FILE" ]]; then
  cat > "$CONF_FILE" <<EOF
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name
[req_distinguished_name]
[ v3_req ]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
extendedKeyUsage = clientAuth, serverAuth
subjectAltName = @alt_names
[alt_names]
URI = ${NAME}
EOF
  echo "Created config file $CONF_FILE"
fi

# Generate the client key and cert
CLIENT_KEY_FILE="$WD/${NAME}-client-key.pem"
CLIENT_CSR_FILE="$WD/${NAME}-client.csr"
CLIENT_CERT_FILE="$WD/${NAME}-client-cert.pem"
CLIENT_CA_CERT_FILE="$WD/${NAME}-client-ca-cert.pem"

openssl genrsa -out "$CLIENT_KEY_FILE" 2048
openssl req -new -key "$CLIENT_KEY_FILE" -out "$CLIENT_CSR_FILE" -subj "/CN=${NAME}" -config "$CONF_FILE"

# Sign the cert with the CA
openssl x509 -req -in "$CLIENT_CSR_FILE" -CA "$CA_CERT_FILE" -CAkey "$CA_KEY_FILE" -CAcreateserial -out "$CLIENT_CERT_FILE" -days 100000 -extensions v3_req -extfile "$CONF_FILE"

cp "$CA_CERT_FILE" "$CLIENT_CA_CERT_FILE"

# delete conf and csr
rm "$CONF_FILE" "$CLIENT_CSR_FILE"

