package envtest

import (
	"context"
	"time"

	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// testMachineInfraRef returns a syntactically valid infrastructureRef for a
// Machine created directly in a test.
//
// CAPI v1.13.3 (v1beta2 contract) makes Machine.spec.infrastructureRef a
// required field. The referenced object never needs to exist: the controllers
// under test resolve the KairosConfig / KairosControlPlane references, not the
// Machine's infrastructure reference, so a dangling ref is inert here and only
// exists to satisfy CRD-level required-field validation on Create.
func testMachineInfraRef(name string) clusterv1.ContractVersionedObjectReference {
	return clusterv1.ContractVersionedObjectReference{
		APIGroup: "infrastructure.cluster.x-k8s.io",
		Kind:     "DockerMachine",
		Name:     name,
	}
}

// testControlPlaneEndpoint is the control-plane endpoint stamped on Cluster
// fixtures whose test exercises a control-plane bootstrap render.
func testControlPlaneEndpoint() clusterv1.APIEndpoint {
	return clusterv1.APIEndpoint{Host: "172.16.56.45", Port: 6443}
}

// markClusterInfrastructureProvisioned flips
// Cluster.status.initialization.infrastructureProvisioned to true through the
// status subresource, standing in for CAPI's cluster controller.
//
// The bootstrap controller refuses to render until this is true (the endpoint
// is only guaranteed to be published by that same pass), so any test that
// expects a bootstrap Secret must call this after creating the Cluster.
// Callers pass the manager's cached client, and a read straight after a Create
// can miss while the informer catches up, so the read-modify-write is retried
// rather than failing the test on a cache that is merely a moment behind. A
// conflict is retried for the same reason.
func markClusterInfrastructureProvisioned(ctx context.Context, c client.Client, cluster *clusterv1.Cluster) error {
	var lastErr error
	for i := 0; i < 50; i++ {
		live := &clusterv1.Cluster{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), live); err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		live.Status.Initialization.InfrastructureProvisioned = ptr.To(true)
		if err := c.Status().Update(ctx, live); err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return nil
	}
	return lastErr
}
