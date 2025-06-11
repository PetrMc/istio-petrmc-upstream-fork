#!/usr/bin/env bash
# shellcheck disable=all
# Copyright Solo.io, Inc
#
# Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

set -ex


function get_tag () {
  if [[ -n "${TAG:-""}" ]]; then
    echo ${TAG}
  else
    echo `date +%s`
  fi
}

export TAG=local-$(get_tag)
export HUB=localhost:5000
function build_all () {
  CGO_ENABLED=0 BUILD_WITH_CONTAINER=0 BUILD_ALL=false \
    DOCKER_TARGETS="docker.pilot docker.proxyv2 docker.app docker.ztunnel docker.install-cni" \
    make dockerx.pushx
}

function deploy_cluster() {
  name=${1:?name}
  index=${2:?index}
  if ! kind get clusters | grep -q ^$name$; then
     kindup2 --name $name --cluster-index=$index
  fi
  kubectl create namespace istio-system || true
  kubectl create secret generic cacerts -n istio-system \
        --from-file=$HOME/kube/ca/cluster1/ca-cert.pem \
        --from-file=$HOME/kube/ca/cluster1/ca-key.pem \
        --from-file=$HOME/kube/ca/cluster1/root-cert.pem \
        --from-file=$HOME/kube/ca/cluster1/cert-chain.pem || true
  cat <<EOF | istioctl install  -y -f - -d manifests/
apiVersion: install.istio.io/v1alpha1
kind: IstioOperator
metadata:
  name: install
spec:
  meshConfig:
    trustDomain: "$name.local"
  profile: ambient
  tag: "$TAG"
  hub: "$HUB"
  values:
    global:
      multiCluster:
        clusterName: "$name"
      variant: ""
      network: "$name"
    pilot:
      env:
        SKIP_LICENSE_CHECK: "true"
        PILOT_ENABLE_IP_AUTOALLOCATE: "true"
        PILOT_SKIP_VALIDATE_TRUST_DOMAIN: "true"
    ztunnel:
      env:
        SKIP_VALIDATE_TRUST_DOMAIN: "true"
    cni:
      ambient:
        dnsCapture: true
    platforms:
      peering:
        enabled: true
EOF
  kubectl label namespace istio-system topology.istio.io/network=$name --overwrite
  kubectl apply -f tests/integration/pilot/testdata/gateway-api-crd.yaml
  kubectl create namespace istio-gateways || true
  istioctl multicluster expose --wait -n istio-gateways
  kubectl label ns default istio.io/dataplane-mode=ambient istio-injection- --overwrite
  kubectl apply -f ~/kube/apps/echo.yaml
  kubectl apply -f ~/kube/shell/shell.yaml
  kubectl label svc echo solo.io/service-scope=global
}

build_all
deploy_cluster alpha 1
deploy_cluster beta 2
istioctl multicluster link --contexts=kind-alpha,kind-beta -n istio-gateways

# For AWS, you may need a internal LB as well if you don't have a NAT gateway..
#apiVersion: v1
#kind: Service
#metadata:
#  labels:
#    gateway.networking.k8s.io/gateway-name: eastwest
#    istio.io/dataplane-mode: none
#    istio.io/expose-istiod: "15012"
#    istio.io/expose-istiod-mode: "internal"
#    networking.istio.io/hboneGatewayPort: "15008"
#    topology.istio.io/network: beta
#    gateway.istio.io/service-account: eastwest-istio-eastwest
#  annotations:
#    service.beta.kubernetes.io/aws-load-balancer-type: "internal"
#  name: eastwest-istio-eastwest-internal
#  namespace: istio-system
#spec:
#  ports:
#  - name: status-port
#    port: 15021
#    protocol: TCP
#    targetPort: status-port
#  - name: tls-hbone
#    port: 15008
#    protocol: TCP
#    targetPort: hbone
#  - name: tcp-xds
#    port: 15010
#    protocol: TCP
#    targetPort: xds
#  - name: tls-xds
#    port: 15012
#    protocol: TCP
#    targetPort: xds-tls
#  selector:
#    hack: eastwest
#  type: LoadBalancer