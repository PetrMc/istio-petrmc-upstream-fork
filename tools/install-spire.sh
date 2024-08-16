#!/usr/bin/env sh

# Copyright Solo.io, Inc
#
# Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

SPIRE_HELM_OVERRIDE=$(
cat <<'END_HEREDOC'
global:
  spire:
    trustDomain: cluster.local
spire-agent:
    authorizedDelegates:
        - "spiffe://cluster.local/ns/istio-system/sa/ztunnel"
    sockets:
        admin:
            enabled: true
            mountOnHost: true
        hostBasePath: /run/spire/agent/sockets
    tolerations:
      - effect: NoSchedule
        operator: Exists
      - key: CriticalAddonsOnly
        operator: Exists
      - effect: NoExecute
        operator: Exists
spire-server:
    persistence:
        type: emptyDir

spiffe-csi-driver:
    tolerations:
      - effect: NoSchedule
        operator: Exists
      - key: CriticalAddonsOnly
        operator: Exists
      - effect: NoExecute
        operator: Exists
END_HEREDOC
)

echo "Installing SPIRE CRDS"
helm upgrade --install -n spire-server spire-crds spire-crds --repo https://spiffe.github.io/helm-charts-hardened/ --create-namespace

echo "Installing SPIRE and waiting for all pods to be ready before continuing..."
echo "$SPIRE_HELM_OVERRIDE" | helm upgrade --install -n spire-server spire spire --repo https://spiffe.github.io/helm-charts-hardened/ --wait -f -

echo "Applying ClusterSPIFFEID for ztunnel"

kubectl apply -f - <<EOF
apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata:
  name: istio-ztunnel-reg
spec:
  spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"
  podSelector:
    matchLabels:
      app: "ztunnel"
EOF

echo "Applying ClusterSPIFFEID for waypoints"

kubectl apply -f - <<EOF
apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata:
  name: istio-waypoint-reg
spec:
  spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"
  podSelector:
    matchLabels:
      istio.io/gateway-name: waypoint
EOF

echo "Applying ClusterSPIFFEID for workloads"
kubectl apply -f - <<EOF
apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata:
  name: istio-ambient-reg
spec:
  spiffeIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"
  podSelector:
    matchLabels:
      istio.io/dataplane-mode: ambient
EOF
