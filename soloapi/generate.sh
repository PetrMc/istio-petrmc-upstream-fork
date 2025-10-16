#! /bin/bash
# Copyright Solo.io, Inc
#
# Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

set -x

set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR=$(git rev-parse --show-toplevel)

# Ensure we're in the module root for code generation tools
cd "${ROOT_DIR}"

# Override PATH to use our gofmt wrapper (needed for gengo v2 tools)
# gengo v2 silently creates empty files if gofmt fails, so we use a pass-through wrapper
export PATH="${ROOT_DIR}/soloapi/hack:${PATH}"

rm -f "${ROOT_DIR}/soloapi/v1alpha1/zz_generated.deepcopy.go"
rm -f "${ROOT_DIR}/soloapi/v1alpha1/zz_generated.register.go"

go tool controller-gen \
   object:headerFile="${ROOT_DIR}/soloapi/hack/boilerplate.go.txt" \
   paths="${ROOT_DIR}/soloapi/v1alpha1/..."

go run k8s.io/code-generator/cmd/register-gen \
   --go-header-file="${ROOT_DIR}/soloapi/hack/boilerplate.go.txt" \
   --output-file="zz_generated.register.go" \
   istio.io/istio/soloapi/v1alpha1

go run k8s.io/code-generator/cmd/client-gen \
	--clientset-name versioned \
	--input-base "istio.io/istio/" \
	--input "soloapi/v1alpha1" \
	--output-pkg "istio.io/istio/soloapi/client/clientset" \
	--output-dir "${ROOT_DIR}/soloapi/client/clientset" \
	--go-header-file "${ROOT_DIR}/soloapi/hack/boilerplate.go.txt"

# Generate listers
echo "Generating listers..."
go run k8s.io/code-generator/cmd/lister-gen \
   --go-header-file="${ROOT_DIR}/soloapi/hack/boilerplate.go.txt" \
   --output-pkg="istio.io/istio/soloapi/client/listers" \
   --output-dir="${ROOT_DIR}/soloapi/client/listers" \
   istio.io/istio/soloapi/v1alpha1

# Generate informers
echo "Generating informers..."
go run k8s.io/code-generator/cmd/informer-gen \
   --go-header-file="${ROOT_DIR}/soloapi/hack/boilerplate.go.txt" \
   --versioned-clientset-package="istio.io/istio/soloapi/client/clientset/versioned" \
   --listers-package="istio.io/istio/soloapi/client/listers" \
   --output-pkg="istio.io/istio/soloapi/client/informers" \
   --output-dir="${ROOT_DIR}/soloapi/client/informers" \
   istio.io/istio/soloapi/v1alpha1

# Generate CRD manifests
echo "Generating CRD manifests..."
mkdir -p "${ROOT_DIR}/soloapi/crd"
go tool controller-gen \
   crd paths="${ROOT_DIR}/soloapi/v1alpha1/..." \
   output:crd:artifacts:config="${ROOT_DIR}/soloapi/crd"

# Merge all CRDs into single file for Helm charts
echo "Merging CRDs for Helm charts..."
TARGET_DIR="${ROOT_DIR}/manifests/charts/base/files"
TARGET_FILE="${TARGET_DIR}/crd-solo.gen.yaml"
mkdir -p "${TARGET_DIR}"
: > "${TARGET_FILE}" # clear before appending
first=true
for crd_file in "${ROOT_DIR}/soloapi/crd"/*.yaml; do
    if [ -f "$crd_file" ]; then
        if [ "$first" = true ]; then
            first=false
        else
            echo "---" >> "${TARGET_FILE}"
        fi
        cat "$crd_file" >> "${TARGET_FILE}"
    fi
done

echo "CRDs merged into ${TARGET_FILE}"
