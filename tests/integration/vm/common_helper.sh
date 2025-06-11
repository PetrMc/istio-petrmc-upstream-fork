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

error_may_pause() {
  echo "$1"
  copy_artifacts

  if [ "$PAUSE_ON_FAILURE" = "true" ]; then
    echo "Pausing due to failure. Select 'resume' to continue, or 'exit' to terminate execution"
    select opt in "resume" "exit"; do
        case $opt in
            resume ) return;;
            exit ) exit 1;;
        esac
    done
  elif [ "$EXIT_ON_FAILURE" = "true" ]; then
    echo "Exiting on failure"
    exit 1
  fi
}

copy_artifacts() {
  if [[ "${CI:-false}" == "true" ]]; then
    echo "Copying artifacts to ${ARTIFACTS}"
    mkdir -p "${ARTIFACTS}/vm1/var/log"
    docker cp vm1:/var/log/ "${ARTIFACTS}/vm1/var/"
    docker exec vm1 iptables-save > "${ARTIFACTS}/vm1/iptables-save"
    docker exec vm1 ip a > "${ARTIFACTS}/vm1/ip-a"
    docker exec vm1 ip route > "${ARTIFACTS}/vm1/ip-route"
    docker exec vm1 cat /etc/resolv.conf > "${ARTIFACTS}/vm1/resolv.conf"
  else
    echo "Not in CI, skipping artifact copy"
  fi
}

wait_for_pod_running() {
  local namespace label timeoutSeconds phase
  namespace=$1
  label="$2"
  timeoutSeconds="$3"
  phase=""

  SECONDS=0 # built-in bash variable
  while [ "$phase" != "Running" ]; do
    phase=$(kubectl get pod -n "${namespace}" -l "${label}" -o jsonpath="{.items[*].status.phase}")
    if [ "$phase" = "Running" ]; then
      break
    fi

    echo "pod with label ${label} in namespace ${namespace} is in phase=${phase}"
    # Dump the pod output for logging
    kubectl get pod -n "${namespace}" -l "${label}"
    if [ -n "$timeoutSeconds" ] && [ "$SECONDS" -gt "$timeoutSeconds" ]; then
      error_may_pause "ERROR: timed out after ${timeoutSeconds}s waiting for pod in namespace ${namespace} with label ${label} to be running"
      break
    fi
    sleep 5
  done
}

report_result() {
  local name scenario result
  name="$1"
  category="$2"
  scenario="$3"
  result="$4"

  if [ -z "${name}" ] || [ -z "${category}" ] || [ -z "${scenario}" ] || [ -z "$result" ]; then
    exit_err "invalid result format"
  fi

  echo "${name}, ${category}, ${scenario}, ${result}" >> "${TEST_RESULTS}"
}

log_dbg() {
  echo "$@" >&2
}

check_prereq() {
  if [ ! -d "${ISTIODIR}" ]; then
    exit_err "ISTIODIR unset. Set this to the absolute Istio repo path"
  fi
  if [ ! -f "${ISTIODIR}/out_spire/linux_$TARGET_ARCH/spire-server.deb" ]; then
    exit_err "missing spire-server.deb, run 'make spire-server_deb'"
  fi
  if [ ! -f "${ISTIODIR}/out_spire/linux_$TARGET_ARCH/spire-agent.deb" ]; then
    exit_err "missing spire-agent.deb, run 'make spire-agent_deb'"
  fi

  local deb
  local debPath
  if [ "${VM_PROXY}" = "ztunnel" ]; then
    deb="istio-ambient.deb"
  else
    deb="istio-sidecar.deb"
  fi
  debPath="${ISTIODIR}/out/linux_$TARGET_ARCH/release/${deb}"
  if [ ! -f "${debPath}" ]; then
    exit_err "missing ${debPath}, run 'make deb ambient_deb'"
  fi
  cp "${debPath}" "${WORK_DIR}/${deb}"
}

req_with_retries() {
  cmd="$1"
  expectedCode="$2"
  local code
  local expectedSuccessCount=5
  local actualSuccessCount=0

  for _ in {1..5}; do
    code=$(eval "${cmd}")
    if [ "${code}" = "${expectedCode}" ]; then
      log_dbg "first req succeeded, got=$code, want=$expectedCode"
      for ((i=0; i<expectedSuccessCount; i++)); do
        code=$(eval "${cmd}")
        if [ "${code}" = "${expectedCode}" ]; then
          ((actualSuccessCount++))
          log_dbg "repeated req success, code=${code}"
        else
          log_dbg "repeated req failed, code=${code}"
        fi
      done
      break
    fi
    log_dbg "request failed, retrying..."
    sleep 3
  done

  # Workaround to allow 1 request to fail. The second request always fails with RBAC in Envoy
  ((expectedSuccessCount--))
  if [ "${actualSuccessCount}" -ge "${expectedSuccessCount}" ]; then
    # Success threshold met, so return the expected code instead of the last code
    # as the test should succeed even if the last code failed. We expect >= 4 out
    # of 5 requests to return the expected code
    echo "${expectedCode}"
  else
    echo "code=${code}, actual=${actualSuccessCount}, expected=${expectedSuccessCount}"
  fi
}

test_traffic_to_vm() {
  local code
  local cmd
  echo ""
  echo "--- Testing traffic: k8s:sleep -> VM ---"
  # Test basic traffic - expect 200
  cmd="kubectl exec deploy/sleep -n test -- curl --write-out '%{http_code}' --silent --output /dev/null ${VM_SVC_URL}"
  code=$(req_with_retries "${cmd}" "200")
  if [ "$code" = "200" ]; then
    report_result "test_traffic_to_vm" "basic" "k8s:sleep -> VM" "success"
    echo "SUCCESS: req sleep.test -> ${VM_SVC_URL} succeeded"
  else
    report_result "test_traffic_to_vm" "basic" "k8s:sleep -> VM" "failure"
    error_may_pause "ERROR: req sleep.test -> ${VM_SVC_URL} failed, want code: 200, got: ${code}"
  fi

  echo ""
  echo "--- Testing denied traffic with L4 policy: k8s:sleep -> VM ---"
  # Test with L4 Authz - expect 0
  kubectl apply -f - <<EOF
apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: deny-sleep
  namespace: "${VM_NAMESPACE}"
spec:
  selector:
    matchLabels:
      app: "${VM_APP}"
  action: DENY
  rules:
  - from:
    - source:
        principals: ["cluster.local/ns/test/sa/sleep"]
EOF
  sleep 5

  cmd="kubectl exec deploy/sleep -n test -- curl --write-out '%{http_code}' --silent --output /dev/null ${VM_SVC_URL}"
  code=$(req_with_retries "${cmd}" "${L4_RBAC_RESP_CODE}")
  if [ "$code" = "${L4_RBAC_RESP_CODE}" ]; then
    report_result "test_traffic_to_vm" "L4 authz" "k8s:sleep -> VM" "success"
    echo "SUCCESS: req sleep.test -> ${VM_SVC_URL} failed as expected"
  else
    report_result "test_traffic_to_vm" "L4 authz" "k8s:sleep -> VM" "failure"
    error_may_pause "ERROR L4 Authz: req sleep.test -> ${VM_SVC_URL} did not fail as expected, want code: ${L4_RBAC_RESP_CODE}, got: ${code}"
  fi
  kubectl delete AuthorizationPolicy deny-sleep -n "${VM_NAMESPACE}"

  echo ""
  echo "--- Testing denied traffic with L7 policy: k8s:sleep -> VM ---"
  # Test with L7 Authz - expect 403
  if [ "${VM_PROXY}" = "ztunnel" ]; then
    waypoint_name="${VM_SERVICE_ACCOUNT}-$(date +%s)"
    $ISTIOCTL waypoint apply -n "${VM_NAMESPACE}" --enroll-namespace --name "${waypoint_name}" --wait
    kubectl apply -f - <<EOF
apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: deny-sleep
  namespace: "${VM_NAMESPACE}"
spec:
  targetRefs:
  - kind: Gateway
    group: gateway.networking.k8s.io
    name: "${waypoint_name}"
  action: DENY
  rules:
  - from:
    - source:
        principals: ["cluster.local/ns/test/sa/sleep"]
    to:
    - operation:
        ports: ["9000"]
        methods: ["GET", "HEAD"]
EOF
  else
    kubectl apply -f - <<EOF
apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: deny-sleep
  namespace: "${VM_NAMESPACE}"
spec:
  selector:
    matchLabels:
      app: "${VM_APP}"
  action: DENY
  rules:
  - from:
    - source:
        principals: ["cluster.local/ns/test/sa/sleep"]
    to:
    - operation:
        methods: ["GET", "HEAD"]
EOF
  fi
  sleep 5

  cmd="kubectl exec deploy/sleep -n test -- curl --write-out '%{http_code}' --silent --output /dev/null ${VM_SVC_URL}"
  code=$(req_with_retries "${cmd}" "403")
  if [ "$code" = "403" ]; then
    report_result "test_traffic_to_vm" "L7 authz" "k8s:sleep -> VM" "success"
    echo "SUCCESS: req sleep.test -> ${VM_SVC_URL} failed as expected"
  else
    report_result "test_traffic_to_vm" "L7 authz" "k8s:sleep -> VM" "failure"
    error_may_pause "ERROR L7 Authz: req sleep.test -> ${VM_SVC_URL} did not fail as expected, want code: 403, got: ${code}"
  fi
  kubectl delete AuthorizationPolicy deny-sleep -n "${VM_NAMESPACE}"
  if [ "${VM_PROXY}" = "ztunnel" ]; then
    $ISTIOCTL waypoint delete -n "${VM_NAMESPACE}" "${waypoint_name}"
  fi

  echo ""
  echo "--- Testing traffic: k8s:sleep -> VM ServiceEntry ---"
  # Test ServiceEntry selecting VM
  kubectl apply -f - <<EOF
apiVersion: networking.istio.io/v1beta1
kind: ServiceEntry
metadata:
  name: vm-service-entry
  namespace: "${VM_NAMESPACE}"
spec:
  addresses:
  - 240.240.23.45
  hosts:
  - vmtest.istio.io
  ports:
  - name: http
    number: 9000
    protocol: HTTP
    targetPort: 9000
  location: MESH_EXTERNAL
  resolution: STATIC # not honored for now; everything is static
  workloadSelector:
    labels:
      app: "${VM_APP}"
EOF
  sleep 5

  local req
  req="http://vmtest.istio.io:9000"
  cmd="kubectl exec deploy/sleep -n test -- curl --write-out '%{http_code}' --silent --output /dev/null ${req}"
  code=$(req_with_retries "${cmd}" "200")
  if [ "$code" = "200" ]; then
    report_result "test_traffic_to_vm" "ServiceEntry basic" "k8s:sleep -> VM" "success"
    echo "SUCCESS: req sleep.test -> ${req} succeeded"
  else
    report_result "test_traffic_to_vm" "ServiceEntry basic" "k8s:sleep -> VM" "failure"
    error_may_pause "ERROR: req sleep.test -> ${req} failed, want code: 200, got: ${code}"
  fi
  kubectl delete ServiceEntry vm-service-entry -n "${VM_NAMESPACE}"
}
