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

WORK_DIR="${DOCKER_WORK_DIR}"

docker_cleanup() {
  # Clean up SPIRE server
  docker stop "${DOCKER_SPIRE_SERVER}" || echo "vm may have been already stopped"
  docker rm "${DOCKER_SPIRE_SERVER}" || echo "vm may  have been already removed"

  # Clean up VM agent
  for vm_name in "${DOCKER_VM_LIST[@]}"; do
    docker stop "${vm_name}" || echo "vm may have been already stopped"
    docker rm "${vm_name}" || echo "vm may  have been already removed"
  done
}

docker_bootstrap_ambient() {
  local vm_name
  vm_name="$1"

  docker exec "${vm_name}" mkdir -p /var/lib/istio/certs/
  docker exec "${vm_name}" cp /vm/root-cert.pem /var/lib/istio/certs/root-cert.pem
  docker exec "${vm_name}" mkdir -p /var/lib/istio/config/
  docker exec "${vm_name}" mkdir -p /var/lib/istio/pilot-agent/
  docker exec "${vm_name}" cp /vm/cluster.env /var/lib/istio/cluster.env
  docker exec "${vm_name}" cp /vm/mesh.yaml /var/lib/istio/config/mesh

  docker exec "${vm_name}" dpkg -i /vm/istio-ambient.deb
  docker exec "${vm_name}" bash -c "sed 's/logger -s/echo/g' /usr/local/bin/istio-start.sh > /vm/istio-start.sh"
  docker exec "${vm_name}" cp /vm/istio-start.sh /usr/local/bin/istio-start.sh
  docker exec "${vm_name}" bash -c "sed -i '1 i\export SHELL=/bin/bash' /usr/local/bin/istio-start.sh" # host zsh default can be picked up by docker and break the script
  docker exec -e RUST_LOG=trace -e RUST_BACKTRACE=1 -d "${vm_name}" bash /usr/local/bin/istio-start.sh
}

docker_bootstrap_envoy() {
  local vm_name
  vm_name="$1"

  docker exec "${vm_name}" dpkg -i /vm/istio-sidecar.deb

  docker exec "${vm_name}" mkdir -p /etc/certs
  docker exec "${vm_name}" cp /vm/root-cert.pem /etc/certs/root-cert.pem
  docker exec "${vm_name}" mkdir -p /var/lib/istio/envoy/
  docker exec "${vm_name}" cp /vm/cluster.env /var/lib/istio/envoy/cluster.env
  docker exec "${vm_name}" mkdir -p /etc/istio/config/
  docker exec "${vm_name}" cp /vm/mesh.yaml /etc/istio/config/mesh

  docker exec "${vm_name}" chown -R istio-proxy /var/lib/istio /etc/certs /etc/istio/config /var/run/secrets /etc/certs/root-cert.pem

  docker exec "${vm_name}" bash -c "sed -i '1 i\export SHELL=/bin/bash' /usr/local/bin/istio-start.sh" # host zsh default can be picked up by docker and break the script
  docker exec -d "${vm_name}" bash /usr/local/bin/istio-start.sh
}

docker_setup() {
  cp "${ISTIODIR}/out_spire/linux_$TARGET_ARCH/spire-server.deb" "$WORK_DIR/."
  cp "${ISTIODIR}/out_spire/linux_$TARGET_ARCH/spire-agent.deb" "$WORK_DIR/."

  # Set up SPIRE server
  kubectl get secret -n istio-system istio-ca-secret -o  jsonpath="{.data.ca-key\.pem}" | base64 -d > "${WORK_DIR}/key.pem"
  docker run -d --name "${DOCKER_SPIRE_SERVER}" --network kind --privileged -v "${DOCKER_WORK_DIR}":/vm ubuntu:22.04 bash -c 'sleep 360000'
  docker exec "${DOCKER_SPIRE_SERVER}" apt update -y
  docker exec "${DOCKER_SPIRE_SERVER}" apt-get install -y iputils-ping curl iproute2 python3 sudo dnsutils vim jq
  docker exec "${DOCKER_SPIRE_SERVER}" mkdir -p /var/lib/spire/certs/
  docker exec "${DOCKER_SPIRE_SERVER}" cp /vm/root-cert.pem /var/lib/spire/certs/root-cert.pem
  docker exec "${DOCKER_SPIRE_SERVER}" cp /vm/key.pem /var/lib/spire/certs/key.pem
  docker exec "${DOCKER_SPIRE_SERVER}" dpkg -i /vm/spire-server.deb
  docker exec "${DOCKER_SPIRE_SERVER}" cp /vm/server.conf /var/lib/spire/server.conf
  docker exec -d "${DOCKER_SPIRE_SERVER}" bash -c "/usr/local/bin/spire-server run -config /var/lib/spire/server.conf 2>> /var/log/spire/server.err.log >> /var/log/spire/server.log"
  sleep 5
  # Create the workload registration entries
  agent_join_token=$(docker exec "${DOCKER_SPIRE_SERVER}" /usr/local/bin/spire-server token generate -spiffeID spiffe://cluster.local/docker/workload/test/spire-agent | cut -d ':' -f2 | xargs)
  echo "SPIRE agent join token: ${agent_join_token}"
  if [ -z "${agent_join_token}" ]; then
    echo "SPIRE server error log"
    docker exec "${DOCKER_SPIRE_SERVER}" bash -c "cat /var/log/spire/server.err.log"
    echo "SPIRE server log"
    docker exec "${DOCKER_SPIRE_SERVER}" bash -c "cat /var/log/spire/server.log"
    exit_err "error generating SPIRE join token"
  fi
  docker exec "${DOCKER_SPIRE_SERVER}" /usr/local/bin/spire-server entry create -parentID spiffe://cluster.local/docker/workload/test/spire-agent -spiffeID spiffe://cluster.local/ns/"${VM_NAMESPACE}"/sa/"${VM_SERVICE_ACCOUNT}" -selector unix:user:istio-proxy

  # Set up VM agent
  echo "$(docker inspect -f '{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}}' spire-server) spire-server.spire" > "${WORK_DIR}"/spire_hosts
  for vm_name in "${DOCKER_VM_LIST[@]}"; do
    docker run -d --name "${vm_name}" --network kind --privileged -v "${DOCKER_WORK_DIR}":/vm ubuntu:22.04 bash -c 'sleep 360000'
    docker exec "${vm_name}" apt update -y
    docker exec "${vm_name}" apt-get install -y iputils-ping curl iproute2 iptables python3 sudo dnsutils vim jq python3-httpbin

    ISTIOD_ADDR="$(grep 'discoveryAddress:' "${DOCKER_WORK_DIR}"/mesh.yaml | cut -d':' -f2 | xargs)"
    kubectl get nodes -o=jsonpath='{range .items[*]}{"ip route add "}{.spec.podCIDR}{" via "}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' | xargs -L1 docker exec "${vm_name}"
    echo "$(kubectl get po -n istio-system -l app=istio-eastwestgateway -o jsonpath='{.items[*].status.podIP}') ${ISTIOD_ADDR}" > "${WORK_DIR}"/hosts
    docker exec "${vm_name}" bash -c 'cat /vm/hosts >> /etc/hosts'
    docker exec "${vm_name}" bash -c 'cat /vm/spire_hosts >> /etc/hosts'

    # Start spire-agent
    docker exec "${vm_name}" dpkg -i /vm/spire-agent.deb
    if [ "${VM_PROXY}" = "envoy" ]; then
      docker exec "${vm_name}" sed -i 's#/var/run/spire/api.sock#/var/run/secrets/workload-spiffe-uds/socket#' /vm/agent.conf
      docker exec "${vm_name}" mkdir -p /var/run/secrets/workload-spiffe-uds/
      docker exec "${vm_name}" chown -R istio-proxy /var/run/secrets/workload-spiffe-uds/
    fi
    docker exec "${vm_name}" cp /vm/agent.conf /var/lib/spire/agent.conf
    # TODO: -e SHELL=/bin/bash is needed here, investigate why later!
    docker exec -e SHELL=/bin/bash -d "${vm_name}" sudo -E -u istio-proxy -s /bin/bash -c "/usr/local/bin/spire-agent run -config /var/lib/spire/agent.conf -joinToken \"${agent_join_token}\" 2>> /var/log/spire/agent.err.log >> /var/log/spire/agent.log"

    # Update container (emulating VM) resolv.conf to use public DNS server
    docker exec "${vm_name}" bash -c "sed 's/127.0.0.11/8.8.8.8/' /etc/resolv.conf > /vm/resolv.conf"
    docker exec "${vm_name}" cp /vm/resolv.conf /etc/resolv.conf

    if [ "${VM_PROXY}" = "envoy" ]; then
      docker_bootstrap_envoy "${vm_name}"
    else
      docker_bootstrap_ambient "${vm_name}"
    fi


    docker exec "${vm_name}" python3 -m http.server 8080 &> /dev/null &

    # Run httpbin on port 9000
    vm_ip=$(docker inspect -f '{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${vm_name}")
    docker exec "${vm_name}" sh -c "python3 -m httpbin.core --port 9000 --host ${vm_ip} &> /dev/null &"

    # Run the helloworld v1 app (port 5000)
    docker exec "${vm_name}" sh -c "SERVICE_VERSION=v1 python3 /vm/app.py &> /dev/null &"
  done
}

docker_vm_bootstrap() {
  docker_cleanup
  docker_setup
}

docker_test_traffic_from_vm() {
  local code
  local cmd

  for vm_name in "${DOCKER_VM_LIST[@]}"; do
    echo ""
    echo "--- Testing traffic: VM:${vm_name} -> k8s:httpbin ---"
    # Test basic traffic - expect 200
    cmd="docker exec ${vm_name} curl --write-out '%{http_code}' --silent --output /dev/null ${HTTPBIN_URL}"
    code=$(req_with_retries "${cmd}" "200")
    if [ "$code" = "200" ]; then
      report_result "test_traffic_from_vm" "basic" "VM -> k8s:httpbin" "success"
      echo "SUCCESS: req ${vm_name} -> ${HTTPBIN_URL} succeeded"
    else
      report_result "test_traffic_from_vm" "basic" "VM -> k8s:httpbin" "failure"
      error_may_pause "ERROR: req ${vm_name} -> ${HTTPBIN_URL} failed, want code: 200, got: ${code}"
    fi

    # Test with L4 Authz - expect 0
    kubectl apply -f - <<EOF
apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
 name: httpbin
 namespace: test
spec:
 selector:
   matchLabels:
     app: httpbin
 action: DENY
 rules:
 - from:
   - source:
       principals: ["cluster.local/ns/${VM_NAMESPACE}/sa/${VM_SERVICE_ACCOUNT}"]
EOF
    sleep 5

    echo ""
    echo "--- Testing denied traffic with L4 policy: VM:${vm_name} -> k8s:httpbin ---"
    # Test with L4 Authz - expect 0
    cmd="docker exec ${vm_name} curl --write-out '%{http_code}' --silent --output /dev/null ${HTTPBIN_URL}"
    code=$(req_with_retries "${cmd}" "${L4_RBAC_RESP_CODE}")
    if [ "$code" = "${L4_RBAC_RESP_CODE}" ]; then
      report_result "test_traffic_from_vm" "L4 authz" "VM -> k8s:httpbin" "success"
      echo "SUCCESS: req ${vm_name} -> ${HTTPBIN_URL} failed as expected"
    else
      report_result "test_traffic_from_vm" "L4 authz" "VM -> k8s:httpbin" "failure"
      error_may_pause "ERROR L4 Authz: req ${vm_name} -> ${HTTPBIN_URL} did not fail as expected, want code: ${L4_RBAC_RESP_CODE}, got: ${code}"
    fi
    kubectl delete AuthorizationPolicy httpbin -n test

    echo ""
    echo "--- Testing denied traffic with L7 policy: VM:${vm_name} -> k8s:httpbin ---"
    # Test with L7 Authz - expect 403
    if [ "${VM_PROXY}" = "ztunnel" ]; then
      waypoint_name="waypoint-$(date +%s)"
      $ISTIOCTL waypoint apply -n test --enroll-namespace --name "${waypoint_name}" --wait
      kubectl apply -f - <<EOF
apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: deny-vm-sleep
  namespace: test
spec:
  targetRefs:
  - kind: Gateway
    group: gateway.networking.k8s.io
    name: "${waypoint_name}"
  action: DENY
  rules:
  - from:
    - source:
        principals: ["cluster.local/ns/${VM_NAMESPACE}/sa/${VM_SERVICE_ACCOUNT}"]
    to:
    - operation:
        methods: ["POST"]
EOF
    else
      kubectl apply -f - <<EOF
apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: deny-vm-sleep
  namespace: test
spec:
  action: DENY
  rules:
  - from:
    - source:
        principals: ["cluster.local/ns/${VM_NAMESPACE}/sa/${VM_SERVICE_ACCOUNT}"]
    to:
    - operation:
        methods: ["POST"]
EOF
    fi

    sleep 5

    cmd="docker exec ${vm_name} curl --write-out '%{http_code}' --silent --output /dev/null -X POST ${HTTPBIN_URL}/post"
    code=$(req_with_retries "${cmd}" "403")
    if [ "$code" = "403" ]; then
      report_result "test_traffic_from_vm" "L7 authz" "VM -> k8s:httpbin" "success"
      echo "SUCCESS: post req VM -> http://httpbin.test.svc:8000/post failed as expected"
    else
      report_result "test_traffic_from_vm" "L7 authz" "VM -> k8s:httpbin" "failure"
      error_may_pause "ERROR L7 Authz: post req VM -> http://httpbin.test.svc:8000/post did not fail as expected, want code: 403, got: ${code}"
    fi

    kubectl delete AuthorizationPolicy deny-vm-sleep -n test
    if [ "${VM_PROXY}" = "ztunnel" ]; then
      $ISTIOCTL waypoint delete -n test "${waypoint_name}"
    fi

  done
}
