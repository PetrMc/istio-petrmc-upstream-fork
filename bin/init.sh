#!/bin/bash

# Copyright 2018 Istio Authors
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

# Init script downloads or updates envoy and the go dependencies. Called from Makefile, which sets
# the needed environment variables.

set -o errexit
set -o nounset
set -o pipefail

if [[ "${TARGET_OUT_LINUX:-}" == "" ]]; then
  echo "Environment variables not set. Make sure you run through the makefile (\`make init\`) rather than directly."
  exit 1
fi

# Setup arch suffix for envoy binary. For backwards compatibility, amd64 has no suffix.
if [[ "${TARGET_ARCH}" == "amd64" ]]; then
	ISTIO_ENVOY_ARCH_SUFFIX=""
else
	ISTIO_ENVOY_ARCH_SUFFIX="-${TARGET_ARCH}"
fi

# Populate the git version for istio/proxy (i.e. Envoy)
PROXY_REPO_SHA="${PROXY_REPO_SHA:-$(grep PROXY_REPO_SHA istio.deps  -A 4 | grep lastStableSHA | cut -f 4 -d '"')}"

# Envoy binary variables
ISTIO_ENVOY_BASE_URL="${ISTIO_ENVOY_BASE_URL:-https://storage.googleapis.com/istio-build/proxy}"

# If we are not using the default, assume its private and we need to authenticate
if [[ "${ISTIO_ENVOY_BASE_URL}" != "https://storage.googleapis.com/istio-build/proxy" ]]; then
  AUTH_HEADER="Authorization: Bearer $(gcloud auth print-access-token)"
  export AUTH_HEADER
fi

SIDECAR="${SIDECAR:-envoy}"

# OS-neutral vars. These currently only work for linux.
ISTIO_ENVOY_VERSION="${ISTIO_ENVOY_VERSION:-${PROXY_REPO_SHA}}"
ISTIO_ENVOY_DEBUG_URL="${ISTIO_ENVOY_DEBUG_URL:-${ISTIO_ENVOY_BASE_URL}/envoy-debug-${ISTIO_ENVOY_VERSION}${ISTIO_ENVOY_ARCH_SUFFIX}.tar.gz}"
ISTIO_ENVOY_RELEASE_URL="${ISTIO_ENVOY_RELEASE_URL:-${ISTIO_ENVOY_BASE_URL}/envoy-alpha-${ISTIO_ENVOY_VERSION}${ISTIO_ENVOY_ARCH_SUFFIX}.tar.gz}"

# Envoy Linux vars.
ISTIO_ENVOY_LINUX_VERSION="${ISTIO_ENVOY_LINUX_VERSION:-${ISTIO_ENVOY_VERSION}}"
ISTIO_ENVOY_LINUX_DEBUG_URL="${ISTIO_ENVOY_LINUX_DEBUG_URL:-${ISTIO_ENVOY_DEBUG_URL}}"
ISTIO_ENVOY_LINUX_RELEASE_URL="${ISTIO_ENVOY_LINUX_RELEASE_URL:-${ISTIO_ENVOY_RELEASE_URL}}"
# Variables for the extracted debug/release Envoy artifacts.
ISTIO_ENVOY_LINUX_DEBUG_DIR="${ISTIO_ENVOY_LINUX_DEBUG_DIR:-${TARGET_OUT_LINUX}/debug}"
ISTIO_ENVOY_LINUX_DEBUG_NAME="${ISTIO_ENVOY_LINUX_DEBUG_NAME:-envoy-debug-${ISTIO_ENVOY_LINUX_VERSION}}"
ISTIO_ENVOY_LINUX_DEBUG_PATH="${ISTIO_ENVOY_LINUX_DEBUG_PATH:-${ISTIO_ENVOY_LINUX_DEBUG_DIR}/${ISTIO_ENVOY_LINUX_DEBUG_NAME}}"

ISTIO_ENVOY_LINUX_RELEASE_DIR="${ISTIO_ENVOY_LINUX_RELEASE_DIR:-${TARGET_OUT_LINUX}/release}"
ISTIO_ENVOY_LINUX_RELEASE_NAME="${ISTIO_ENVOY_LINUX_RELEASE_NAME:-${SIDECAR}-${ISTIO_ENVOY_VERSION}}"
ISTIO_ENVOY_LINUX_RELEASE_PATH="${ISTIO_ENVOY_LINUX_RELEASE_PATH:-${ISTIO_ENVOY_LINUX_RELEASE_DIR}/${ISTIO_ENVOY_LINUX_RELEASE_NAME}}"

# There is no longer an Istio built Envoy binary available for the Mac. Copy the Linux binary as the Mac binary was
# very old and likely no one was really using it (at least temporarily).

# Download Envoy debug and release binaries for Linux x86_64. They will be included in the
# docker images created by Dockerfile.proxyv2.

# Gets the download command supported by the system (currently either curl or wget)
DOWNLOAD_COMMAND=""
function set_download_command () {
  # Try curl.
  if command -v curl > /dev/null; then
    if curl --version | grep Protocols  | grep https > /dev/null; then
      DOWNLOAD_COMMAND="curl -fLSs --retry 5 --retry-delay 1 --retry-connrefused"
      return
    fi
    echo curl does not support https, will try wget for downloading files.
  else
    echo curl is not installed, will try wget for downloading files.
  fi

  # Try wget.
  if command -v wget > /dev/null; then
    DOWNLOAD_COMMAND="wget -qO -"
    return
  fi
  echo wget is not installed.

  echo Error: curl is not installed or does not support https, wget is not installed. \
       Cannot download envoy. Please install wget or add support of https to curl.
  exit 1
}

# Downloads and extract an Envoy binary if the artifact doesn't already exist.
# Params:
#   $1: The URL of the Envoy tar.gz to be downloaded.
#   $2: The full path of the output binary.
#   $3: Non-versioned name to use
function download_envoy_if_necessary () {
  if [[ ! -f "$2" ]] ; then
    # Enter the output directory.
    mkdir -p "$(dirname "$2")"
    pushd "$(dirname "$2")"

    # Download and extract the binary to the output directory.
    echo "Downloading ${SIDECAR}: $1 to $2"
    time ${DOWNLOAD_COMMAND} --header "${AUTH_HEADER:-}" "$1" |\
      tar --extract --gzip --strip-components=3 --to-stdout > "$2"
    chmod +x "$2"

    # Make a copy named just "envoy" in the same directory (overwrite if necessary).
    echo "Copying $2 to $(dirname "$2")/${3}"
    cp -f "$2" "$(dirname "$2")/${3}"
    popd
  fi
}

# A core feature of modsecurity's WAF filters is a set of core rules that are usually included in every filter.
# The configuration for these rules is too large to hard-code in-line,
# so standard practice is to configure the filters to reference core rule files directly.
# To reference the core rules this way, we need to download them into our sidecar containers, since the proxies
# are where the filters are actually used.
# The following commands download the modsecurity core rules for use in the proxy container.
# Downloads core ruleset.
# Params:
#   $1: The full path to output files
CRS_COMMIT=v3.2.0
CRS_REPO=SpiderLabs/owasp-modsecurity-crs
function download_crs () {
  download_file_dir="$1"
  # Create the output directory.
  mkdir -p "${download_file_dir}"

  # Copy the crs files to the output directory.
  echo "Downloading crs files to ${download_file_dir}"
  repo_dir="modsecurity-repo"
  git clone https://github.com/"${CRS_REPO}".git "${repo_dir}"
  pushd "${repo_dir}"
  git checkout -qf "${CRS_COMMIT}"
  cp ./rules/*.conf "${download_file_dir}"
  cp ./rules/*.data "${download_file_dir}"
  popd

  rm -rf "${repo_dir}"
}

# Downloads libsaxon.
# Params:
#   $1: The URL of the libsaxon file to be downloaded.
#   $2: The full path of the output file.
function download_libsaxon () {
  download_file_dir="$(dirname "$2")"
  download_file_name="$(basename "$2")"
  download_file_path="${download_file_dir}/${download_file_name}"
  # Enter the output directory.
  mkdir -p "${download_file_dir}"
  pushd "${download_file_dir}"

  # Download the libsaxon plugin files to the output directory.
  echo "Downloading libsaxon file: $1 to ${download_file_path}"
  if [[ ${DOWNLOAD_COMMAND} == curl* ]]; then
    time ${DOWNLOAD_COMMAND} --header "${AUTH_HEADER:-}" "$1" -o "${download_file_name}"
  elif [[ ${DOWNLOAD_COMMAND} == wget* ]]; then
    time ${DOWNLOAD_COMMAND} --header "${AUTH_HEADER:-}" "$1" -O "${download_file_name}"
  fi

  popd
}


mkdir -p "${TARGET_OUT}"

# Set the value of DOWNLOAD_COMMAND (either curl or wget)
set_download_command

if [[ -n "${DEBUG_IMAGE:-}" ]]; then
  # Download and extract the Envoy linux debug binary.
  download_envoy_if_necessary "${ISTIO_ENVOY_LINUX_DEBUG_URL}" "$ISTIO_ENVOY_LINUX_DEBUG_PATH" "${SIDECAR}"
else
  echo "Skipping envoy debug. Set DEBUG_IMAGE to download."
fi

# Download and extract the Envoy linux release binary.
download_envoy_if_necessary "${ISTIO_ENVOY_LINUX_RELEASE_URL}" "$ISTIO_ENVOY_LINUX_RELEASE_PATH" "${SIDECAR}"
ISTIO_ENVOY_NATIVE_PATH=${ISTIO_ENVOY_LINUX_RELEASE_PATH}

# Dropped in favor of upstream Envoy, see https://solo-io-corp.slack.com/archives/C04T298GD28/p1689866392407009
# **SOLO** Add our libsaxon/modsecurity stuff
#
# @TODO: Uncomment this when 1.27 is branched
#
# SOLO_LIBSAXON_PATH="${ISTIO_ENVOY_LINUX_RELEASE_DIR}/libsaxon-solo.so"

# Download the Modsecurity core rule set to the envoy release dir
#
# @TODO: Uncomment this when 1.27 is branched
#
#SOLO_CRS_DIR="${ISTIO_ENVOY_LINUX_RELEASE_DIR}/solo-crs"
#echo "Downloading core rule set to ${SOLO_CRS_DIR}"
#download_crs "${SOLO_CRS_DIR}"

# Dropped in favor of upstream Envoy, see https://solo-io-corp.slack.com/archives/C04T298GD28/p1689866392407009
# Download the libsaxon shared object to the envoy release dir
#
# @TODO: Uncomment this when 1.27 is branched
#
#SOLO_LIBSAXON_BINARY_URL="${ISTIO_ENVOY_BASE_URL}/libsaxon-solo-${ISTIO_ENVOY_VERSION}${ISTIO_ENVOY_ARCH_SUFFIX}.so"
#echo "Downloading libsaxon: ${SOLO_LIBSAXON_BINARY_URL} to ${SOLO_LIBSAXON_PATH}"
#download_libsaxon "${SOLO_LIBSAXON_BINARY_URL}" "${SOLO_LIBSAXON_PATH}"

# Copy native envoy binary to TARGET_OUT
echo "Copying ${ISTIO_ENVOY_NATIVE_PATH} to ${TARGET_OUT}/${SIDECAR}"
cp -f "${ISTIO_ENVOY_NATIVE_PATH}" "${TARGET_OUT}/${SIDECAR}"

# Copy the envoy binary to TARGET_OUT_LINUX if the local OS is not Linux
if [[ "$GOOS_LOCAL" != "linux" ]]; then
  echo "Copying ${ISTIO_ENVOY_LINUX_RELEASE_PATH} to ${TARGET_OUT_LINUX}/${SIDECAR}"
  cp -f "${ISTIO_ENVOY_LINUX_RELEASE_PATH}" "${TARGET_OUT_LINUX}/${SIDECAR}"
fi
