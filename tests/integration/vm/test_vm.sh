#!/bin/bash
#
# Copyright 2017, 2018 Istio Authors. All Rights Reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
################################################################################

# shellcheck disable=all

exit_err() {
    echo "ERROR: $1"
    exit 1
}

trap 'exit_err $LINENO' ERR

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )

ISTIO_VM_CONFIG=${ISTIO_VM_CONFIG:-"${SCRIPT_DIR}/testdata/.istio_vm.env"}
if [ ! -f "${ISTIO_VM_CONFIG}" ]; then
    exit_err "${ISTIO_VM_CONFIG} file not found!"
fi

source "${ISTIO_VM_CONFIG}"
source "${SCRIPT_DIR}/common_helper.sh"
source "${SCRIPT_DIR}/extra_vm_tests.sh"
if [ "$PLATFORM" = "docker" ]; then
  source "${SCRIPT_DIR}/docker_helper.sh"
else
  exit_err "invalid platform: ${PLATFORM}"
fi

# Confirm the requested platform helpers implement the interface required by this scipt
fn_exists() { declare -F "$1" > /dev/null; }
declare -a required_func=("cleanup" "setup" "vm_bootstrap" "test_traffic_from_vm")
for func in "${required_func[@]}"; do
  fn_exists "${PLATFORM}_${func}" || exit_err "${PLATFORM}_${func} not implemented for platform $PLATFORM"
done

# Always cleanup the app namespace before starting the test
kubectl delete ns test "${VM_NAMESPACE}" || echo "ignoring app namespace deletion error"
if [ "${CLEANUP}" = "true" ]; then
  eval "${PLATFORM}_cleanup"
  $ISTIOCTL uninstall --purge -y || echo "istioctl uninstall failed"
  kubectl delete ns istio-system || echo "istio-system namespace deletion failed"
  exit 0
fi

if [ -z "${SPIRE_AGENT_CONFIG}" ]; then
  exit_err "SPIRE_AGENT_CONFIG unset"
fi
if [ -z "${SPIRE_SERVER_CONFIG}" ]; then
  exit_err "SPIRE_SERVER_CONFIG unset"
fi

mkdir -p "${WORK_DIR}"
rm -rf "${WORK_DIR}/*"
echo "tmp workdir: ${WORK_DIR}"
TEST_RESULTS="${WORK_DIR}/test_results"
rm -f "${TEST_RESULTS}"
echo "NAME, CATEGORY, SCENARIO, RESULT" > "${TEST_RESULTS}"

check_prereq

# Install Gateway API CRDs if not installed as per
# https://istio.io/latest/docs/tasks/traffic-management/ingress/gateway-api/#setup
kubectl get crd gateways.gateway.networking.k8s.io &> /dev/null || \
  { kubectl kustomize "github.com/kubernetes-sigs/gateway-api/config/crd?ref=v1.1.0" | kubectl apply -f -; }

if [ "$INSTALL_ISTIO" = "true" ]; then
# Uninstall Istio before attempting install to work around https://github.com/istio/istio/issues/45204
# istioctl install with --force flag does not work even though the issue above claims it does
$ISTIOCTL uninstall --purge -y || true
kubectl delete ns istio-system --ignore-not-found=true

cat <<< "apiVersion: install.istio.io/v1alpha1
kind: IstioOperator
metadata:
  name: istio
spec:
  components:
    ingressGateways:
      - name: istio-ingressgateway
        enabled: false
  values:
    cni:
      ambient:
        dnsCapture: true
    global:
      logging:
        level: "all:debug"
      meshID: mesh1
      multiCluster:
        clusterName: \"${CLUSTER}\"
      network: \"${CLUSTER_NETWORK}\"
      variant: \"\" # TODO: Remove this once/if we build distroless images
    ztunnel:
      variant: \"\" # TODO: Remove this once/if we build distroless images
      meshConfig:
        defaultConfig:
          proxyMetadata:
            ISTIO_META_DNS_CAPTURE: \"true\"
            ISTIO_META_DNS_AUTO_ALLOCATE: \"true\"
    pilot:
      variant: \"\" # TODO: Remove this once/if we build distroless images
  meshConfig:
    defaultConfig:
      proxyMetadata:
        ISTIO_META_DNS_CAPTURE: \"true\"
        ISTIO_META_DNS_AUTO_ALLOCATE: \"true\"" > istio-iop.yaml

cat istio-iop.yaml
$ISTIOCTL install --set hub="$HUB" --set tag="$TAG" --set profile=ambient \
  --set revision="$REVISION" \
  --set values.global.imagePullPolicy=Always \
  --set values.pilot.env.PILOT_ENABLE_WORKLOAD_ENTRY_AUTOREGISTRATION=true \
  --set values.pilot.env.PILOT_ENABLE_WORKLOAD_ENTRY_HEALTHCHECKS=true -y -f istio-iop.yaml
rm -f istio-iop.yaml

${ISTIODIR}/samples/multicluster/gen-eastwest-gateway.sh --single-cluster | $ISTIOCTL install -y -f -
kubectl apply -n istio-system -f ${ISTIODIR}/samples/multicluster/expose-istiod.yaml
fi

kubectl create namespace ${VM_NAMESPACE} || echo "namespace may already exist"

waitForIP() {
  svc=$1
  external_ip=""
  while [ -z $external_ip ]; do
    external_ip=$(kubectl get po -n istio-system -l app=istio-eastwestgateway -o jsonpath="{.items[*].status.podIP}")
    [ -z $external_ip ] && sleep 5
  done
  echo "LoadBalancer $svc ready"
}

echo "Waiting for east-west gateway to get external IP address"
waitForIP istio-eastwestgateway

echo "Creating VM WorkloadGroup"
cat <<EOF >${WORK_DIR}/workloadgroup.yaml
apiVersion: networking.istio.io/v1alpha3
kind: WorkloadGroup
metadata:
  name: "${VM_APP}"
  namespace: "${VM_NAMESPACE}"
spec:
  metadata:
    labels:
      app: "${VM_APP}"
    annotations:
      ambient.istio.io/redirection: enabled
  template:
    labels:
      version: v1
    serviceAccount: "${VM_SERVICE_ACCOUNT}"
    network: "${VM_NETWORK}"
    ports:
      http-9000: 9000
      http-8080: 8080
      http-5000: 5000
EOF
kubectl --namespace "${VM_NAMESPACE}" apply -f "${WORK_DIR}/workloadgroup.yaml"

$ISTIOCTL x workload entry configure -f $WORK_DIR/workloadgroup.yaml -o "${WORK_DIR}" \
  --clusterID "${CLUSTER}" --autoregister --tokenDuration 604800 --capture-dns="${DNS_CAPTURE}" --useServiceAccountToken=false
## --concurrency="1" is being added to ISTIO_AGENT_FLAGS to eliminate an integ test flake where tests might fail
#### when an envoy worker thread which has not yet received the latest config handles a connection.
echo "ISTIO_AGENT_FLAGS='--log_output_level=dns:debug,xdsproxy:debug,spire:debug --proxyLogLevel=debug --concurrency="1"'" >> "$WORK_DIR/cluster.env"

cp "${SPIRE_AGENT_CONFIG}" $WORK_DIR/agent.conf
cp "${SPIRE_SERVER_CONFIG}" $WORK_DIR/server.conf
curl -o $WORK_DIR/app.py https://raw.githubusercontent.com/istio/istio/1.18.0/samples/helloworld/src/app.py

kubectl create namespace test || echo "namespace may already exist"

if [ "${VM_PROXY}" = "ztunnel" ]; then
  kubectl label namespace test istio-injection- --overwrite=true
  kubectl label namespace test istio.io/dataplane-mode=ambient --overwrite=true
  kubectl label namespace "${VM_NAMESPACE}" istio.io/dataplane-mode=ambient --overwrite=true

  sed -i '/ISTIO_META_ENABLE_HBONE: "true"/a \ \ \ \ DNS_PROXY_ADDR: "127.0.0.1:15053"' "$WORK_DIR"/mesh.yaml
  # Set the WORKLOAD_IDENTITY_SOCKET_PATH to use SPIRE
  echo "WORKLOAD_IDENTITY_SOCKET_PATH='unix:///var/run/spire/api.sock'" >> "$WORK_DIR/cluster.env"
else
  kubectl label namespace test istio.io/dataplane-mode- --overwrite=true
  kubectl label namespace test istio-injection=enabled --overwrite=true

  # Set the WORKLOAD_IDENTITY_SOCKET_PATH to use SPIRE
  echo "WORKLOAD_IDENTITY_SOCKET_PATH='unix:///var/run/secrets/workload-spiffe-uds/socket'" >> "$WORK_DIR/cluster.env"
fi
sleep 5
kubectl apply -n test -f "${ISTIODIR}/samples/httpbin/httpbin.yaml"
kubectl rollout restart -n test deploy/httpbin
kubectl rollout status -n test deploy/httpbin
kubectl apply -n test -f "${ISTIODIR}/samples/sleep/sleep.yaml"
kubectl rollout restart -n test deploy/sleep
kubectl rollout status -n test deploy/sleep

kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: vmsvc
  namespace: "${VM_NAMESPACE}"
  labels:
    app: "${VM_APP}"
spec:
  ports:
  - name: http-9000
    port: 9000
    protocol: TCP
  - name: http-8080
    port: 8080
    protocol: TCP
  - name: http-5000
    port: 5000
    protocol: TCP
  selector:
    app: "$VM_APP"
  type: ClusterIP
EOF
sleep 5

# Bootstrap the VM
${PLATFORM}_vm_bootstrap
sleep 5

# Print the current state of the cluster
kubectl get po -n istio-system -o wide
kubectl get po -n test -o wide

# Test traffic from VM -> k8s:httpbin
${PLATFORM}_test_traffic_from_vm
# Test traffic from k8s:sleep -> VM
test_traffic_to_vm
# Extra non-core tests
extra_vm_tests

echo ""
echo "work dir: ${WORK_DIR}" >> /tmp/vmtest

result=
if grep -q failure "${TEST_RESULTS}"; then
  result="failure"
else
  result="success"
fi

echo "----------------------- TEST RESULTS -----------------------"
echo "Date: $(date)"
echo "VM proxy: ${VM_PROXY}"

echo ""
column -s, -t "${TEST_RESULTS}"
echo ""

if [ "$result" = "failure" ]; then
  exit_err "Test FAILED!"
fi
