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

// WorkerJoinMaterial is what a worker needs to join. Token becomes the renderer's
// WorkerToken (k0s) or K3sToken (k3s). Every field is redacted in String() and
// GoString(), including fields P1 adds, so it never leaks through a %v/%+v/%#v
// log (root rule 2).
type WorkerJoinMaterial struct {
	Token string
}

// String redacts the material for %v/%s/%+v.
func (WorkerJoinMaterial) String() string { return "WorkerJoinMaterial{Token:REDACTED}" }

// GoString redacts the material for %#v.
func (WorkerJoinMaterial) GoString() string { return "WorkerJoinMaterial{Token:REDACTED}" }

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
// source in r.JoinSources if present, otherwise the built-in over r.Client. A nil
// map or missing key uses the built-in, so existing construction sites that set
// no JoinSources are unchanged.
func (r *KairosConfigReconciler) joinSourceFor(distribution string) JoinMaterialSource {
	if r.JoinSources != nil {
		if src, ok := r.JoinSources[distribution]; ok && src != nil {
			return src
		}
	}
	if row, ok := bootstrapDistributions[distribution]; ok && row.builtinJoin != nil {
		return row.builtinJoin(r.Client)
	}
	// Defensive: renderCloudConfig rejects an unknown distribution before the
	// worker path reaches here; fall back to the default distribution's built-in.
	return bootstrapDistributions[bootstrapv1beta2.DefaultDistribution].builtinJoin(r.Client)
}

// workerJoin resolves the worker join material for distribution through the
// selected source. It is the single entry point both generators use for the
// role == "worker" path.
func (r *KairosConfigReconciler) workerJoin(ctx context.Context, distribution string, kc *bootstrapv1beta2.KairosConfig, machine *clusterv1.Machine, cluster *clusterv1.Cluster, serverAddress string) (WorkerJoinMaterial, error) {
	return r.joinSourceFor(distribution).WorkerJoin(ctx, JoinRequest{
		Config:        kc,
		Machine:       machine,
		Cluster:       cluster,
		ServerAddress: serverAddress,
	})
}
