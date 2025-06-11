#!/usr/bin/env bash
# shellcheck disable=all
# Copyright Solo.io, Inc
#
# Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)

function load::deploy() {
  local name="${1:?name}"
  helm upgrade --install --namespace "${name}" --create-namespace istiod "${WD}" --wait=false
}

function load::kubectl() {
  local name="${1:?name}"
  shift
  kubectl port-forward-run svc/apiserver 6443 -n "${name}" -- env KUBECONFIG=/dev/null kubectl --username fake --server=https://{} --insecure-skip-tls-verify $@
}
function load::deploy-many() {
  for suffix in $@; do
    load::deploy load-"${suffix}" &
  done
  wait
}
function load::link() {
  local from=${1:?from}
  local to=${2:?to}
  local cip="$(kubectl -n "${from}" get svc istiod -ojsonpath={.spec.clusterIP})"
  cat <<EOF | load::kubectl $to apply -f - --validate=false --server-side
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: remote-$from
  namespace: istio-system
  annotations:
    gateway.istio.io/service-account: eastwest-istio-eastwest
  labels:
    topology.istio.io/network: "$from"
spec:
  gatewayClassName: istio-remote
  addresses:
  - value: ${cip}
  listeners:
  - name: cross-network
    port: 15008
    protocol: HBONE
    tls:
      mode: Passthrough
      options:
        gateway.istio.io/listener-protocol: auto-passthrough
EOF
}
function load::link-many() {
  for src in $@; do
    for dst in $@; do
      if [[ $src != $dst ]]; then
        load::link load-$src load-$dst &
      fi
    done
    wait
  done
}
# load::link-self 10
function load::link-self() {
  local count="${1:?count}"
  shift
  local gtw_config="$(for i in {1..$count}; do
    cat <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: remote-net-$i
  namespace: istio-system
  annotations:
    gateway.istio.io/service-account: eastwest-istio-eastwest
  labels:
    topology.istio.io/network: "net-$i"
spec:
  gatewayClassName: istio-remote
  addresses:
  - value: 127.0.0.1
  listeners:
  - name: cross-network
    port: 15008
    protocol: HBONE
    tls:
      mode: Passthrough
      options:
        gateway.istio.io/listener-protocol: auto-passthrough
---
EOF
  done)"
  echo "${gtw_config}" | kubectl apply -f - --validate=false --server-side
}

# load::create-services my-svc {0..2}
function load::create-service() {
  local name="${1:?name}"
  shift
  for suffix in $@; do
    cat <<EOF | load::kubectl load-"${suffix}" apply -f - --validate=false --server-side
apiVersion: v1
kind: Service
metadata:
  labels:
    solo.io/service-scope: "global"
  name: ${name}
  namespace: default
spec:
  ports:
  - name: http
    port: 8080
EOF
  done
}
# load::create-services 10 {0..2}
function load::create-services() {
  local count="${1:?count}"
  shift
  local service_config="$(for i in {1..$count}; do
    cat <<EOF
apiVersion: v1
kind: Service
metadata:
  labels:
    solo.io/service-scope: "global"
  name: svc-${i}
  namespace: default
spec:
  ports:
  - name: http
    port: 8080
---
EOF
  done)"
    for suffix in $@; do
      echo "${service_config}" | load::kubectl load-"${suffix}" apply -f - --validate=false --server-side
    done
  wait
}