#!/bin/bash

# Copyright Solo.io, Inc
#
# Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

build_images_for_vm_test() {
  # Build just the images needed for VM tests
  targets="docker.pilot docker.proxyv2 docker.install-cni docker.ztunnel docker.istioctl"

  # Integration tests are always running on local architecture (no cross compiling), so find out what that is.
  arch="linux/amd64"
  if [[ "$(uname -m)" == "aarch64" ]]; then
      arch="linux/arm64"
  fi
  if [[ "${VARIANT:-default}" == "distroless" ]]; then
    DOCKER_ARCHITECTURES="${arch}" DOCKER_BUILD_VARIANTS="distroless" DOCKER_TARGETS="${targets}" make dockerx.pushx
  else
   DOCKER_ARCHITECTURES="${arch}"  DOCKER_BUILD_VARIANTS="${VARIANT:-default}" DOCKER_TARGETS="${targets}" make dockerx.pushx
  fi

  make deb ambient_deb spire-agent_deb spire-server_deb
}