/*
Copyright 2024 The Kairos CAPI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing
permissions and limitations under the License.
*/

package bootstrap

import (
	"context"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// ManagementEndpoint carries the values a node needs to push its kubeconfig
// back to the management cluster without SSH. It is the controller-package
// twin of internal/bootstrap.ManagementEndpoint — the two structs are kept
// separate so the renderer stays free of any controller / Kubernetes-API
// concerns (cloudconfig-rendering-safety rule: the renderer does not talk to
// the API server). The call site in generate*CloudConfig performs a one-line
// struct-literal conversion.
//
// ClusterName is stamped into the pushed Secret as the
// `cluster.x-k8s.io/cluster-name` label so the controlplane controller's
// Secret-watch predicate (KD-15-compliant under KD-3b) can filter by label.
// ControlPlaneEndpointHost is the cluster's control-plane endpoint host that
// CAPV templates use to rewrite the kubeconfig `server:` URL before pushing.
type ManagementEndpoint struct {
	APIServer                 string
	Token                     string
	KubeconfigSecretName      string
	KubeconfigSecretNamespace string
	ClusterName               string
	ControlPlaneEndpointHost  string
}

// ManagementEndpointResolver materialises the management-cluster contact info
// the node needs at boot. Implementations are responsible for:
//
//   - Mint or look up an authenticated token whose RBAC reach is the bare
//     minimum required to push the kubeconfig Secret (and read VMI status,
//     under CAPK, for the SAN-detection script).
//   - Compute the management API URL the node should call back to.
//   - Pick the (namespace, name) of the kubeconfig Secret to write.
//
// Returning (nil, nil) is the documented "disabled" signal — used by the
// production resolver when the manager's REST config is unavailable (envtest,
// out-of-cluster `go run` flows). Callers MUST treat (nil, nil) as
// "render without push block", not as an error.
//
// The interface is the seam PR-6 introduces so PR-7 (KD-3b) can drop in
// non-CAPK resolvers (CAPV, CAPM3) without touching the bootstrap controller.
type ManagementEndpointResolver interface {
	Resolve(ctx context.Context, kc *bootstrapv1beta2.KairosConfig, cluster *clusterv1.Cluster) (*ManagementEndpoint, error)
}
