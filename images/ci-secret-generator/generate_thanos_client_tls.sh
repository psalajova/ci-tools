#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "Usage: $0 <output-dir> <kubeconfig-path> <cluster-name>"
  exit 1
fi

OUTPUT_DIR="$1"
KUBECONFIG_PATH="$2"
CLUSTER_NAME="$3"

mkdir -p "${OUTPUT_DIR}"

oc --kubeconfig "${KUBECONFIG_PATH}" -n ci-monitoring get secret thanos-ca \
  -o jsonpath='{.data.ca\.crt}' | base64 -d > "${OUTPUT_DIR}/ca.crt"
oc --kubeconfig "${KUBECONFIG_PATH}" -n ci-monitoring get secret thanos-ca \
  -o jsonpath='{.data.ca\.key}' | base64 -d > "${OUTPUT_DIR}/ca.key"

openssl genrsa -out "${OUTPUT_DIR}/client.key" 4096 2>/dev/null
openssl req -new -key "${OUTPUT_DIR}/client.key" \
  -subj "/CN=ci-monitoring-prometheus-${CLUSTER_NAME}" \
  -out "${OUTPUT_DIR}/client.csr" 2>/dev/null
openssl x509 -req -in "${OUTPUT_DIR}/client.csr" \
  -CA "${OUTPUT_DIR}/ca.crt" -CAkey "${OUTPUT_DIR}/ca.key" -CAcreateserial \
  -days 90 -sha256 \
  -out "${OUTPUT_DIR}/client.crt" 2>/dev/null

rm -f "${OUTPUT_DIR}/client.csr" "${OUTPUT_DIR}/ca.srl" "${OUTPUT_DIR}/ca.key"
