#!/bin/bash
#
# Copyright Solo.io, Inc
#
# Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant
#
################################################################################
#
# Script to configure and start the Istio sidecar.

set -e

ISTIO_BIN_BASE="/usr/local/bin"
ISTIO_LOG_DIR="/var/log/istio"
ISTIO_SYSTEM_NAMESPACE=${ISTIO_SYSTEM_NAMESPACE:-istio-system}
INIT_LOG_FILE="${ISTIO_LOG_DIR}/init.log"
ISTIO_CONFIG_DIR="/var/lib/istio"
ISTIO_CLUSTER_CONFIG="${ISTIO_CONFIG_DIR}/cluster.env"
MESH_CONFIG_PATH="${ISTIO_CONFIG_DIR}/config/mesh"
ROOT_CA_CERT_PATH="${ROOT_CA_CERT_PATH:-$ISTIO_CONFIG_DIR/certs/root-cert.pem}"
PROXY_MODE="dedicated"

log_info() {
  logger -s "INFO: $1" >> "${INIT_LOG_FILE}"
}

log_debug() {
  logger -s "DEBUG: $1" >> "${INIT_LOG_FILE}"
}

log_error() {
  logger -s "ERROR: $1" >> "${INIT_LOG_FILE}"
}

set -a
# Load config environment variables
set -o allexport
# shellcheck disable=SC1090
source "$ISTIO_CLUSTER_CONFIG"
set +o allexport

set +a

if [ -z "${ISTIO_SVC_IP:-}" ]; then
  ISTIO_SVC_IP=$(hostname --all-ip-addresses | cut -d ' ' -f 1)
fi

if [ -z "${POD_NAME:-}" ]; then
  POD_NAME=$(hostname -s)
fi

EXEC_USER=${EXEC_USER:-istio-proxy}
if [ "${ISTIO_INBOUND_INTERCEPTION_MODE}" = "TPROXY" ] ; then
  # In order to allow redirect inbound traffic using TPROXY, run envoy with the CAP_NET_ADMIN capability.
  # This allows configuring listeners with the "transparent" socket option set to true.
  EXEC_USER=root
fi

# su will mess with the limits set on the process we run. This may lead to quickly exhausting the file limits
# We will get the host limit and set it in the child as well.
# TODO(https://superuser.com/questions/1645513/why-does-executing-a-command-in-su-change-limits) can we do better?
currentLimit=$(ulimit -n)

start_ztunnel() {
  log_info "starting ztunnel"

  if [ "${EXEC_USER}" == "${USER:-}" ] ; then
    # if started as istio-proxy (or current user), do a normal start, without redirecting stderr
    CA_ROOT_CA="${ROOT_CA_CERT_PATH}" XDS_ROOT_CA="${ROOT_CA_CERT_PATH}"  \
      MESH_CONFIG_PATH="${MESH_CONFIG_PATH}" \
      PROXY_MODE="${PROXY_MODE}" ENABLE_INBOUND_PASSTHROUGH_BIND=true \
      INSTANCE_IP="${ISTIO_SVC_IP}" \
      POD_NAME="${POD_NAME}" POD_NAMESPACE="${POD_NAMESPACE}" \
      NODE_NAME="${ISTIO_SVC_IP}" \
      CA_ADDRESS="${WORKLOAD_IDENTITY_SOCKET_PATH}" \
      "${ISTIO_BIN_BASE}"/ztunnel proxy ztunnel
  else
    exec sudo -E -u "${EXEC_USER}" -s /bin/bash -c \
      "ulimit -n ${currentLimit}; \
      CA_ROOT_CA=${ROOT_CA_CERT_PATH} XDS_ROOT_CA=${ROOT_CA_CERT_PATH}  \
      MESH_CONFIG_PATH=${MESH_CONFIG_PATH} \
      PROXY_MODE=${PROXY_MODE} ENABLE_INBOUND_PASSTHROUGH_BIND=true \
      INSTANCE_IP=${ISTIO_SVC_IP} \
      POD_NAME=${POD_NAME} POD_NAMESPACE=${POD_NAMESPACE} \
      NODE_NAME=${ISTIO_SVC_IP} \
      CA_ADDRESS=${WORKLOAD_IDENTITY_SOCKET_PATH} \
      ${ISTIO_BIN_BASE}/ztunnel proxy ztunnel \
      2>> ${ISTIO_LOG_DIR}/ztunnel.err.log >> ${ISTIO_LOG_DIR}/ztunnel.log"
  fi
}

clean_iptables() {
  log_debug "cleaning iptables rules"
  "${ISTIO_BIN_BASE}"/pilot-agent istio-clean-iptables
}

init_iptables() {
  log_debug "setting up iptables rules"
  "${ISTIO_BIN_BASE}"/pilot-agent istio-iptables
}

case "$1" in
  "ztunnel")
    start_ztunnel
    exit 0
    ;;
  "clean")
    clean_iptables
    exit 0
    ;;
  "init")
    clean_iptables
    init_iptables
    exit 0
    ;;
  *)
    # Initialize and start everything
    clean_iptables
    init_iptables
    start_ztunnel
    ;;
esac
