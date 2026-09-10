package envtest

import (
	"context"

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
func markClusterInfrastructureProvisioned(ctx context.Context, c client.Client, cluster *clusterv1.Cluster) error {
	live := &clusterv1.Cluster{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), live); err != nil {
		return err
	}
	live.Status.Initialization.InfrastructureProvisioned = ptr.To(true)
	return c.Status().Update(ctx, live)
}
