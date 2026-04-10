#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

CLUSTER_NAME="${CLUSTER_NAME:-kairos-capi-test}"
WORK_DIR="$ROOT_DIR/.work-kubevirt-${CLUSTER_NAME}"
KUBECONFIG_PATH="$WORK_DIR/kubeconfig"
KUBECTL_CONTEXT="kind-${CLUSTER_NAME}"

mkdir -p bin
if [[ ! -x bin/kubevirt-env ]]; then
  go build -o bin/kubevirt-env ./cmd/kubevirt-env
fi

bin/kubevirt-env

kubectl --kubeconfig "$KUBECONFIG_PATH" --context "$KUBECTL_CONTEXT" \
  apply -f config/samples/capk/kubevirt_cluster_k0s_single_node.yaml

echo "Waiting for control plane machine to be Ready..."
kubectl --kubeconfig "$KUBECONFIG_PATH" --context "$KUBECTL_CONTEXT" \
  wait --for=condition=Ready --timeout=600s machines \
  -l cluster.x-k8s.io/cluster-name=kairos-cluster -n default

echo "Checking providerID on control plane machine..."
provider_id=$(kubectl --kubeconfig "$KUBECONFIG_PATH" --context "$KUBECTL_CONTEXT" \
  get machines -l cluster.x-k8s.io/cluster-name=kairos-cluster -n default \
  -o jsonpath='{.items[0].spec.providerID}')
if [[ -z "${provider_id}" ]]; then
  echo "ERROR: providerID is empty on control plane machine"
  exit 1
fi

echo "KubeVirt local e2e flow completed."
