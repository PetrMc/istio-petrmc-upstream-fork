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

req_with_retries_check_response() {
  cmd="$1"
  expectedCode="$2"
  expectedResponse="$3"
  local code
  local expectedSuccessCount=5
  local actualSuccessCount=0

  for _ in {1..5}; do
    mapfile -t response < <(eval "${cmd}")
    code=${response[-1]}                   # get last line
    body=${response[*]::${#response[*]}-1} # get all body lines except last

    if [ "${code}" = "${expectedCode}" ] && [[ "$body" =~ $expectedResponse ]]; then
      log_dbg "first req succeeded, got=$code, want=$expectedCode"
      log_dbg "first res body: $body"
      for ((i = 0; i < expectedSuccessCount; i++)); do
        unset response
        mapfile -t response < <(eval "${cmd}")
        code=${response[-1]}
        body=${response[*]::${#response[*]}-1}

        if [ "${code}" = "${expectedCode}" ] && [[ "$body" =~ $expectedResponse ]]; then
          ((actualSuccessCount++))
          log_dbg "repeated req success, code=${code}"
          log_dbg "repeated res body: $body"
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

test_traffic_resiliency_timeout() {
  local code
  local cmd

  url="vmsvc.cloud.svc.cluster.local:9000/delay/2"

  echo ""
  echo "--- Testing traffic without timeout: k8s:sleep -> VM ---"
  # Test traffic without timeout - expect 200 after 2s
  cmd="kubectl exec deploy/sleep -n test -- curl --write-out '%{http_code}' --silent --output /dev/null ${url}"
  code=$(req_with_retries "${cmd}" "200")
  if [ "$code" = "200" ]; then
    report_result "traffic resiliency" "timeout" "without timeout" "success"
    echo "SUCCESS: req sleep.test -> ${url} succeeded"
  else
    report_result "traffic resiliency" "timeout" "without timeout" "failure"
    error_may_pause "ERROR: req sleep.test -> ${url} failed, want code: 200, got: ${code}"
  fi

  kubectl apply -n "${VM_NAMESPACE}" -f - <<EOF
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: vmsvc
spec:
  hosts:
    - vmsvc.cloud.svc.cluster.local
  http:
  - route:
    - destination:
        host: vmsvc.cloud.svc.cluster.local
    timeout: 0.5s
EOF

  if [ "${VM_PROXY}" = "ztunnel" ]; then
    waypoint_name="${VM_SERVICE_ACCOUNT}-$(date +%s)"
    $ISTIOCTL waypoint apply -n "${VM_NAMESPACE}" --name "${waypoint_name}" --enroll-namespace --wait --overwrite
  fi

  echo ""
  echo "--- Testing traffic with timeout: k8s:sleep -> VM ---"
  # Test traffic with timeout - expect 504
  cmd="kubectl exec deploy/sleep -n test -- curl --write-out '%{http_code}' --silent --output /dev/null ${url}"
  code=$(req_with_retries "${cmd}" "504")
  if [ "$code" = "504" ]; then
    report_result "traffic resiliency" "timeout" "with timeout" "success"
    echo "SUCCESS: req sleep.test -> ${url} succeeded"
  else
    report_result "traffic resiliency" "timeout" "with timeout" "failure"
    error_may_pause "ERROR: req sleep.test -> ${url} failed, want code: 504, got: ${code}"
  fi

  kubectl delete -n "${VM_NAMESPACE}" virtualservice vmsvc
  if [ "${VM_PROXY}" = "ztunnel" ]; then
    $ISTIOCTL waypoint delete -n "${VM_NAMESPACE}" "${waypoint_name}"
  fi
}

test_fault_injection() {
  local code
  local cmd

  url="vmsvc.cloud.svc.cluster.local:9000"

  if [ "${VM_PROXY}" = "ztunnel" ]; then
    waypoint_name="${VM_SERVICE_ACCOUNT}-$(date +%s)"
    $ISTIOCTL waypoint apply -n "${VM_NAMESPACE}" --name "${waypoint_name}" --enroll-namespace --wait --overwrite
  fi

  echo ""
  echo "--- Testing traffic without fault injection: k8s:sleep -> VM ---"
  cmd="kubectl exec deploy/sleep -n test -- curl --write-out '%{http_code}' --silent --output /dev/null ${url}"
  code=$(req_with_retries "${cmd}" "200")
  if [ "$code" = "200" ]; then
    report_result "traffic resiliency" "fault injection" "without server error" "success"
    echo "SUCCESS: req sleep.test -> ${url} succeeded"
  else
    report_result "traffic resiliency" "fault injection" "without server error" "failure"
    error_may_pause "ERROR: req sleep.test -> ${url} failed, want code: 200, got: ${code}"
  fi

  kubectl apply -n "${VM_NAMESPACE}" -f - <<EOF
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: vmsvc
spec:
  hosts:
    - vmsvc.cloud.svc.cluster.local
  http:
  - fault:
      abort:
        httpStatus: 500
        percentage:
          value: 100
    match:
    - headers:
        fault:
          exact: abort
    route:
    - destination:
        host: vmsvc.cloud.svc.cluster.local
  - route:
    - destination:
        host: vmsvc.cloud.svc.cluster.local
EOF

  echo ""
  echo "--- Testing traffic with fault injection: k8s:sleep -> VM ---"
  cmd="kubectl exec deploy/sleep -n test -- curl -H 'fault: abort' --write-out '%{http_code}' --silent --output /dev/null ${url}"
  code=$(req_with_retries "${cmd}" "500")
  if [ "$code" = "500" ]; then
    report_result "traffic resiliency" "fault injection" "with server error" "success"
    echo "SUCCESS: req sleep.test -> ${url} succeeded"
  else
    report_result "traffic resiliency" "fault injection" "with server error" "failure"
    error_may_pause "ERROR: req sleep.test -> ${url} failed, want code: 500, got: ${code}"
  fi

  kubectl delete -n "${VM_NAMESPACE}" virtualservice vmsvc
  if [ "${VM_PROXY}" = "ztunnel" ]; then
    $ISTIOCTL waypoint delete -n "${VM_NAMESPACE}" "${waypoint_name}"
  fi
}

test_l7_authz_policy() {
  local code
  local cmd

  url="vmsvc.cloud.svc.cluster.local:9000/post"

  echo ""
  echo "--- Testing post without authz policy: k8s:sleep -> VM ---"
  cmd="kubectl exec deploy/sleep -n test -- curl -X POST --write-out '%{http_code}' --silent --output /dev/null ${url}"
  code=$(req_with_retries "${cmd}" "200")
  if [ "$code" = "200" ]; then
    report_result "l7_authz_policy" "deny post" "without authz policy" "success"
    echo "SUCCESS: req sleep.test -> ${url} succeeded"
  else
    report_result "l7_authz_policy" "deny post" "without authz policy" "failure"
    error_may_pause "ERROR: req sleep.test -> ${url} failed, want code: 200, got: ${code}"
  fi

  echo ""
  echo "--- Testing post with authz policy: k8s:sleep -> VM ---"
  if [ "${VM_PROXY}" = "ztunnel" ]; then
    waypoint_name="${VM_SERVICE_ACCOUNT}-$(date +%s)"
    $ISTIOCTL waypoint apply -n "${VM_NAMESPACE}" --name "${waypoint_name}" --enroll-namespace --wait --overwrite
    kubectl apply -n "${VM_NAMESPACE}" -f - <<EOF
apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: deny-post
spec:
  targetRefs:
  - kind: Gateway
    group: gateway.networking.k8s.io
    name: "${waypoint_name}"
  action: DENY
  rules:
  - from:
    - source:
        principals:
        - "cluster.local/ns/test/sa/sleep"
        - "cluster.local/ns/vm/sa/vmsa"
    to:
    - operation:
        ports: ["9000"]
        methods: ["POST"]
EOF
  else
    kubectl apply -n "${VM_NAMESPACE}" -f - <<EOF
apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: deny-post
spec:
  action: DENY
  rules:
  - from:
    - source:
        principals:
        - "cluster.local/ns/test/sa/sleep"
        - "cluster.local/ns/vm/sa/vmsa"
    to:
    - operation:
        ports: ["9000"]
        methods: ["POST"]
EOF
  fi

  cmd="kubectl exec deploy/sleep -n test -- curl -X POST --write-out '%{http_code}' --silent --output /dev/null ${url}"
  code=$(req_with_retries "${cmd}" "403")
  if [ "$code" = "403" ]; then
    report_result "l7_authz_policy" "deny post" "with authz policy" "success"
    echo "SUCCESS: req sleep.test -> ${url} succeeded"
  else
    report_result "l7_authz_policy" "deny post" "with authz policy" "failure"
    error_may_pause "ERROR: req sleep.test -> ${url} failed, want code: 403, got: ${code}"
  fi

  kubectl delete -n "${VM_NAMESPACE}" authorizationpolicy deny-post
  if [ "${VM_PROXY}" = "ztunnel" ]; then
    $ISTIOCTL waypoint delete -n "${VM_NAMESPACE}" "${waypoint_name}"
  fi
}

setup_traffic_mgmt_test() {
  echo "Setting up.."

  kubectl apply -n "${VM_NAMESPACE}" -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  labels:
    app: ${VM_APP}
    version: v2
  name: ${VM_SERVICE_ACCOUNT}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-v2
  labels:
    app: ${VM_APP}
    version: v2
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${VM_APP}
      version: v2
  template:
    metadata:
      labels:
        app: ${VM_APP}
        version: v2
    spec:
      serviceAccount: ${VM_SERVICE_ACCOUNT}
      containers:
      - name: helloworld
        image: docker.io/istio/examples-helloworld-v2
        resources:
          requests:
            cpu: "100m"
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 5000
EOF
  wait_for_pod_running "${VM_NAMESPACE}" "version=v2"

  if [ "${VM_PROXY}" = "ztunnel" ]; then
    waypoint_name="${VM_SERVICE_ACCOUNT}-$(date +%s)"
    $ISTIOCTL waypoint apply -n "${VM_NAMESPACE}" --name "${waypoint_name}" --enroll-namespace --wait --overwrite
  fi

  kubectl apply -n "${VM_NAMESPACE}" -f - <<EOF
apiVersion: networking.istio.io/v1alpha3
kind: DestinationRule
metadata:
  name: vmsvc
spec:
  host: vmsvc
  subsets:
  - name: v1
    labels:
      app: vm-svc
      version: v1
  - name: v2
    labels:
      app: vm-svc
      version: v2
---
apiVersion: networking.istio.io/v1beta1
kind: VirtualService
metadata:
  name: vmsvc
spec:
  hosts:
  - vmsvc
  http:
  - match:
      - headers:
          end-user:
            exact: jason
    route:
    - destination:
        host: vmsvc
        port:
          number: 5000
        subset: v2
  - route:
    - destination:
        host: vmsvc
        port:
          number: 5000
        subset: v1
EOF
}

cleanup_traffic_mgmt_test() {
  echo "Cleaning up.."
  kubectl delete -n "${VM_NAMESPACE}" deployment helloworld-v2
  kubectl delete -n "${VM_NAMESPACE}" serviceaccount "${VM_SERVICE_ACCOUNT}"

  kubectl delete -n "${VM_NAMESPACE}" destinationrule vmsvc
  kubectl delete -n "${VM_NAMESPACE}" virtualservice vmsvc
}

test_traffic_mgmt() {
  local code
  local cmd

  helloworld_url=vmsvc.cloud:5000/hello

  setup_traffic_mgmt_test

  echo ""
  echo "--- Testing traffic without user header: k8s:sleep -> VM (v1) ---"
  cmd="kubectl exec deploy/sleep -n test -- curl -s -w '\n%{http_code}' ${helloworld_url}"
  code=$(req_with_retries_check_response "${cmd}" "200" "v1")
  if [ "$code" = "200" ]; then
    report_result "traffic management" "header match subset" "from k8s without header (v1)" "success"
    echo "SUCCESS: req sleep.test -> ${helloworld_url} succeeded"
  else
    report_result "traffic management" "header match subset" "from k8s without header (v1)" "failure"
    error_may_pause "ERROR: req sleep.test -> ${helloworld_url} failed, want code: 200, got: ${code}"
  fi

  echo ""
  echo "--- Testing traffic with user header: k8s:sleep -> k8s:helloworld (v2) ---"
  cmd="kubectl exec deploy/sleep -n test -- curl -H 'end-user: jason' -s -w '\n%{http_code}' ${helloworld_url}"
  code=$(req_with_retries_check_response "${cmd}" "200" "v2")
  if [ "$code" = "200" ]; then
    report_result "traffic management" "header match subset" "from k8s with header (v2)" "success"
    echo "SUCCESS: req sleep.test -> ${helloworld_url} succeeded"
  else
    report_result "traffic management" "header match subset" "from k8s with header (v2)" "failure"
    error_may_pause "ERROR: req sleep.test -> ${helloworld_url} failed, want code: 200, got: ${code}"
  fi

  if [ "$PLATFORM" = "docker" ] && [ ${#DOCKER_VM_LIST[@]} -eq 1 ]; then
    vm_name="${DOCKER_VM_LIST[0]}"

    echo ""
    echo "--- Testing traffic without user header: VM -> VM (v1) ---"
    cmd="docker exec ${vm_name} curl -s -w '\n%{http_code}' ${helloworld_url}"
    code=$(req_with_retries_check_response "${cmd}" "200" "v1")
    if [ "$code" = "200" ]; then
      report_result "traffic management" "header match subset" "from VM without header (v1)" "success"
      echo "SUCCESS: req sleep.test -> ${helloworld_url} succeeded"
    else
      report_result "traffic management" "header match subset" "from VM without header (v1)" "failure"
      error_may_pause "ERROR: req sleep.test -> ${helloworld_url} failed, want code: 200, got: ${code}"
    fi

    echo ""
    echo "--- Testing traffic with user header: VM -> k8s:helloworld (v2) ---"
    cmd="docker exec ${vm_name} curl -H 'end-user: jason' -s -w '\n%{http_code}' ${helloworld_url}"
    code=$(req_with_retries_check_response "${cmd}" "200" "v2")
    if [ "$code" = "200" ]; then
      report_result "traffic management" "header match subset" "from VM with header (v2)" "success"
      echo "SUCCESS: req sleep.test -> ${helloworld_url} succeeded"
    else
      report_result "traffic management" "header match subset" "from VM with header (v2)" "failure"
      error_may_pause "ERROR: req sleep.test -> ${helloworld_url} failed, want code: 200, got: ${code}"
    fi

  fi

  cleanup_traffic_mgmt_test
}

extra_vm_tests() {
  test_traffic_resiliency_timeout
  test_fault_injection
  test_l7_authz_policy
  test_traffic_mgmt
}
