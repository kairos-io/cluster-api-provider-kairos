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
	"fmt"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// JoinMaterialSource resolves what one distribution needs to join a WORKER to an
// existing cluster (a token today; in P1 also a CA pin and a join configuration).
// The control-plane join stays outside this seam — it is coupled to the C5
// node-push rework (M2). Contract:
//
//   - It may read the management cluster, and a workload cluster if it was built
//     with a client for one. It MUST NOT mutate req.Config; the reconciler owns
//     status and conditions.
//   - "Not ready yet" is an error that wraps errTokenNotReady. The reconciler
//     then requeues after 10s and leaves the conditions alone
//     (kairosconfig_controller.go reconcileBootstrapData).
//   - ANY OTHER ERROR IS TERMINAL UNTIL A WATCH FIRES, because Reconcile marks
//     the conditions and returns ctrl.Result{}, nil. Transient failures (P1:
//     clustercache, workload API timeouts) must therefore wrap the not-ready type.
//   - Error text is user-visible. Neither the material nor req.Config is ever
//     logged (root rule 2).
type JoinMaterialSource interface {
	// WorkerJoin runs for role "worker" before any other render lookup; it
	// rejects incomplete material.
	WorkerJoin(ctx context.Context, req JoinRequest) (WorkerJoinMaterial, error)
}

// JoinRequest is the read-only input to a JoinMaterialSource.
type JoinRequest struct {
	Config        *bootstrapv1beta2.KairosConfig
	Machine       *clusterv1.Machine // P1: spec.version, status.nodeRef
	Cluster       *clusterv1.Cluster // legacy tokenSecretRef namespace; P1: controlPlaneRef, endpoint
	ServerAddress string             // spec.serverAddress, else https://<controlPlaneEndpoint>, else ""
}

// WorkerJoinMaterial is what a worker needs to join. For k0s/k3s, Token becomes
// the renderer's WorkerToken / K3sToken. For kubeadm, Kubeadm carries the minted
// join material. Every secret-bearing field is redacted in String() and GoString()
// so the material never leaks through a %v/%+v/%#v log (root rule 2).
type WorkerJoinMaterial struct {
	Token string
	// Kubeadm, when non-nil, carries the kubeadm worker-join material minted by the
	// kubeadm JoinMaterialSource after its owner-verified trust check (ADR 0010 P1).
	Kubeadm *KubeadmJoinMaterial
}

// KubeadmJoinMaterial is the kubeadm worker-join material: the marshalled
// JoinConfiguration (which embeds the minted bootstrap token in its discovery
// block — SECRET), the exact Kubernetes version the node must run, and the
// NON-SECRET token ID for status/audit. The JoinConfiguration is redacted by
// WorkerJoinMaterial's String/GoString; TokenID is safe to surface.
type KubeadmJoinMaterial struct {
	// JoinConfiguration is the version-marshalled kubeadm JoinConfiguration YAML.
	// It embeds the minted bootstrap token in discovery.bootstrapToken.token, so it
	// is secret material and is never logged.
	JoinConfiguration string
	// KubernetesVersion is the exact version the node must run (Machine.spec.version).
	KubernetesVersion string
	// TokenID is the non-secret ID half ("[a-z0-9]{6}") of the minted bootstrap
	// token, written to KairosConfig.status.bootstrapTokenID for audit. Never the
	// secret half.
	TokenID string
}

// String redacts the material for %v/%s/%+v. Only the non-secret kubeadm token ID
// is surfaced; the k0s/k3s token and the kubeadm JoinConfiguration (which embeds
// the bootstrap token) are always REDACTED.
func (m WorkerJoinMaterial) String() string {
	if m.Kubeadm != nil {
		return "WorkerJoinMaterial{Token:REDACTED, Kubeadm:{JoinConfiguration:REDACTED, TokenID:" + m.Kubeadm.TokenID + "}}"
	}
	return "WorkerJoinMaterial{Token:REDACTED}"
}

// GoString redacts the material for %#v.
func (m WorkerJoinMaterial) GoString() string { return m.String() }

// k0sJoinSource is the built-in k0s worker join source over a client.Reader.
type k0sJoinSource struct{ c client.Reader }

func (s k0sJoinSource) WorkerJoin(ctx context.Context, req JoinRequest) (WorkerJoinMaterial, error) {
	token, err := resolveWorkerToken(ctx, s.c, req.Config, req.Cluster)
	if err != nil {
		return WorkerJoinMaterial{}, err
	}
	if token == "" {
		return WorkerJoinMaterial{}, fmt.Errorf("worker token is required for worker nodes: either WorkerTokenSecretRef, WorkerToken, TokenSecretRef, or Token must be set")
	}
	return WorkerJoinMaterial{Token: token}, nil
}

// k3sJoinSource is the built-in k3s worker join source over a client.Reader. A
// k3s worker also requires a server address.
type k3sJoinSource struct{ c client.Reader }

func (s k3sJoinSource) WorkerJoin(ctx context.Context, req JoinRequest) (WorkerJoinMaterial, error) {
	token, err := resolveK3sWorkerToken(ctx, s.c, req.Config, req.Cluster)
	if err != nil {
		return WorkerJoinMaterial{}, err
	}
	if token == "" {
		return WorkerJoinMaterial{}, fmt.Errorf("k3s worker requires a join token: set k3sTokenSecretRef, k3sToken, workerTokenSecretRef, workerToken, tokenSecretRef, or token")
	}
	if req.ServerAddress == "" {
		return WorkerJoinMaterial{}, fmt.Errorf("k3s worker requires serverAddress or cluster controlPlaneEndpoint")
	}
	return WorkerJoinMaterial{Token: token}, nil
}

// joinSourceFor returns the JoinMaterialSource for distribution: the injected
// source in r.JoinSources if present, otherwise the row's built-in over r.Client.
//
// N2 (ADR 0010 P1): a distribution whose row has a nil builtinJoin (kubeadm) MUST
// have an injected JoinSources entry — main.go wires it with the dependencies only
// it has (clustercache, the uncached reader, the GroupKind allowlist). If both are
// absent, this HARD-FAILS with a clear error rather than silently falling back to
// the DefaultDistribution (k0s) built-in, which would resolve a k0s/k3s token and
// render the wrong bootstrap data onto a kubeadm node. A nil map or a missing key
// for a distribution that HAS a built-in (k0s/k3s) is still fine.
func (r *KairosConfigReconciler) joinSourceFor(distribution string) (JoinMaterialSource, error) {
	if r.JoinSources != nil {
		if src, ok := r.JoinSources[distribution]; ok && src != nil {
			return src, nil
		}
	}
	if row, ok := bootstrapDistributions[distribution]; ok && row.builtinJoin != nil {
		return row.builtinJoin(r.Client), nil
	}
	return nil, fmt.Errorf("no worker join source configured for distribution %q: it requires an injected JoinSources entry (check the manager wiring)", distribution)
}

// workerJoin resolves the worker join material for distribution through the
// selected source. It is the single entry point renderCloudConfig uses for the
// role == "worker" path.
func (r *KairosConfigReconciler) workerJoin(ctx context.Context, distribution string, kc *bootstrapv1beta2.KairosConfig, machine *clusterv1.Machine, cluster *clusterv1.Cluster, serverAddress string) (WorkerJoinMaterial, error) {
	src, err := r.joinSourceFor(distribution)
	if err != nil {
		return WorkerJoinMaterial{}, err
	}
	return src.WorkerJoin(ctx, JoinRequest{
		Config:        kc,
		Machine:       machine,
		Cluster:       cluster,
		ServerAddress: serverAddress,
	})
}
