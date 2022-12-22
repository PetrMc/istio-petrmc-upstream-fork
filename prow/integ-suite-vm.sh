#!/bin/bash

# Copyright Solo.io, Inc
#
# Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant


# Usage: ./integ-suite-vm.sh [options]

WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
ROOT=$(dirname "$WD")

# Exit immediately for non zero status
set -e
# Check unset variables
set -u
# Print commands
set -x

# shellcheck source=prow/lib.sh
source "${ROOT}/prow/lib.sh"
# shellcheck source=prow/lib_vm.sh
source "${ROOT}/prow/lib_vm.sh"
setup_and_export_git_sha

# shellcheck source=common/scripts/kind_provisioner.sh
source "${ROOT}/common/scripts/kind_provisioner.sh"

TOPOLOGY=SINGLE_CLUSTER
NODE_IMAGE="gcr.io/istio-testing/kind-node:v1.27.3"
KIND_CONFIG=""
CLUSTER_TOPOLOGY_CONFIG_FILE="${ROOT}/prow/config/topology/multicluster.json"
SKIP_CLEANUP=
SKIP_BUILD=
SKIP_SETUP=

export FAST_VM_BUILDS=true
export ISTIO_DOCKER_BUILDER=crane

PARAMS=()

while (( "$#" )); do
  case "$1" in
    # Node images can be found at https://github.com/kubernetes-sigs/kind/releases
    # For example, kindest/node:v1.14.0
    --node-image)
      NODE_IMAGE=$2
      shift 2
    ;;
    # Config for enabling different Kubernetes features in KinD (see prow/config{endpointslice.yaml,trustworthy-jwt.yaml}).
    --kind-config)
    KIND_CONFIG=$2
    shift 2
    ;;
    --skip-setup)
      SKIP_SETUP=true
      shift
    ;;
    --skip-cleanup)
      SKIP_CLEANUP=true
      shift
    ;;
    --skip-build)
      SKIP_BUILD=true
      shift
    ;;
    --topology)
      case $2 in
        # TODO(landow) get rid of MULTICLUSTER_SINGLE_NETWORK after updating Prow job
        SINGLE_CLUSTER | MULTICLUSTER_SINGLE_NETWORK | MULTICLUSTER )
          TOPOLOGY=$2
          echo "Running with topology ${TOPOLOGY}"
          ;;
        *)
          echo "Error: Unsupported topology ${TOPOLOGY}" >&2
          exit 1
          ;;
      esac
      shift 2
    ;;
    --topology-config)
      CLUSTER_TOPOLOGY_CONFIG_FILE="${ROOT}/${2}"
      shift 2
    ;;
    -*)
      echo "Error: Unsupported flag $1" >&2
      exit 1
      ;;
    *) # preserve positional arguments
      PARAMS+=("$1")
      shift
      ;;
  esac
done

if [ -f /proc/cpuinfo ]; then
  echo "Checking CPU..."
  grep 'model' /proc/cpuinfo || true
fi

# Default IP family of the cluster is IPv4
export IP_FAMILY="${IP_FAMILY:-ipv4}"

# LoadBalancer in Kind is supported using metallb
export TEST_ENV=kind-metallb

# See https://kind.sigs.k8s.io/docs/user/quick-start/#loading-an-image-into-your-cluster
export PULL_POLICY=IfNotPresent

# We run a local-registry in a docker container that KinD nodes pull from
# These values are must match what is in config/trustworthy-jwt.yaml
export KIND_REGISTRY_NAME="kind-registry"
export KIND_REGISTRY_PORT="5000"
export KIND_REGISTRY="localhost:${KIND_REGISTRY_PORT}"

export HUB=${HUB:-"istio-testing"}
export TAG="${TAG:-"istio-testing"}"

# If we're not intending to pull from an actual remote registry, use the local kind registry
if [[ -z "${SKIP_BUILD:-}" ]]; then
  HUB="${KIND_REGISTRY}"
  export HUB
fi

# Setup junit report and verbose logging
export T="${T:-"-v -count=1"}"
export CI="true"

export ARTIFACTS="${ARTIFACTS:-$(mktemp -d)}"
trace "init" make init

if [[ -z "${SKIP_SETUP:-}" ]]; then
  export DEFAULT_CLUSTER_YAML="./prow/config/default.yaml"
  export METRICS_SERVER_CONFIG_DIR='./prow/config/metrics'

  if [[ "${TOPOLOGY}" == "SINGLE_CLUSTER" ]]; then
    trace "setup kind cluster" setup_kind_cluster_retry "istio-testing" "${NODE_IMAGE}" "${KIND_CONFIG}"
  else
    trace "load cluster topology" load_cluster_topology "${CLUSTER_TOPOLOGY_CONFIG_FILE}"
    trace "setup kind clusters" setup_kind_clusters "${NODE_IMAGE}" "${IP_FAMILY}"

    TOPOLOGY_JSON=$(cat "${CLUSTER_TOPOLOGY_CONFIG_FILE}")
    for i in $(seq 0 $((${#CLUSTER_NAMES[@]} - 1))); do
      CLUSTER="${CLUSTER_NAMES[i]}"
      KCONFIG="${KUBECONFIGS[i]}"
      TOPOLOGY_JSON=$(set_topology_value "${TOPOLOGY_JSON}" "${CLUSTER}" "meta.kubeconfig" "${KCONFIG}")
    done
    RUNTIME_TOPOLOGY_CONFIG_FILE="${ARTIFACTS}/topology-config.json"
    echo "${TOPOLOGY_JSON}" > "${RUNTIME_TOPOLOGY_CONFIG_FILE}"

    export INTEGRATION_TEST_TOPOLOGY_FILE
    INTEGRATION_TEST_TOPOLOGY_FILE="${RUNTIME_TOPOLOGY_CONFIG_FILE}"

    export INTEGRATION_TEST_KUBECONFIG
    INTEGRATION_TEST_KUBECONFIG=NONE
  fi
fi

if [[ -z "${SKIP_BUILD:-}" ]]; then
  trace "setup kind registry" setup_kind_registry
  trace "build images" build_images_for_vm_test
fi

# If a variant is defined, update the tag accordingly
if [[ -n "${VARIANT:-}" ]]; then
  export TAG="${TAG}-${VARIANT}"
fi

# Fetch kubectl
arch="amd64"
if [[ "$(uname -m)" == "aarch64" ]]; then
    arch="arm64"
fi
(cd "${ROOT}/out" && curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/${arch}/kubectl")
export PATH="${PATH}:${ROOT}/out/"

# Run the VM test suite
uname -a
apt update
apt install bsdmainutils -y
istioctl_path="$(find "${ROOT}"/out -name istioctl | tail -n1)"

export ISTIODIR="${ROOT}/"
export ISTIO_VM_CONFIG="${ROOT}/tests/integration/vm/testdata/.istio_vm.env"
export ISTIOCTL="$istioctl_path"
export PLATFORM=docker
export SPIRE_AGENT_CONFIG="${ROOT}/tests/integration/vm/testdata/spire_agent_docker.conf"
export SPIRE_SERVER_CONFIG="${ROOT}/tests/integration/vm/testdata/spire_server_docker.conf"
export EXIT_ON_FAILURE=true

# Enable Ambient VM tests when we implement the next-gen VM integration with Ambient
# # Test Ambient VM integration
# export VM_PROXY=ztunnel
# trace "vm_test_ambient" bash "${ROOT}/tests/integration/vm/test_vm.sh"

# Test Envoy VM integration
export VM_PROXY=envoy
trace "vm_test_envoy" bash "${ROOT}/tests/integration/vm/test_vm.sh"

if [[ "${SKIP_CLEANUP}" != "true" ]]; then
  export CLEANUP=true
  trace "vm_test" bash "${ROOT}/tests/integration/vm/test_vm.sh"
fi
