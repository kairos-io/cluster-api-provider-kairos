package envtest

import (
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
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
