package envtest

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
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

// setMachineStatus writes m.Status onto the Machine m names, through the
// status subresource. The read-modify-write is retried: a control-plane
// Machine fixture is created and then given its status, and the KCP may write
// the Machine in between (it records the node name on every control-plane
// Machine without one, KD-91), so the object Create returned can be stale. m
// is updated to the stored object.
func setMachineStatus(g *WithT, ctx context.Context, c client.Client, m *clusterv1.Machine) {
	status := *m.Status.DeepCopy()
	g.Eventually(func() error {
		live := &clusterv1.Machine{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(m), live); err != nil {
			return err
		}
		live.Status = status
		if err := c.Status().Update(ctx, live); err != nil {
			return err
		}
		live.DeepCopyInto(m)
		return nil
	}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
}

// controlPlaneBootstrap creates the KairosConfig that a KCP-owned
// control-plane Machine fixture named name points at, recording distribution
// as createControlPlaneMachine stamps it, and returns the Bootstrap that
// names it. The KCP reads what each of its Machines runs from that
// KairosConfig (ADR 0016): a Machine with no readable one leaves the control
// plane's distribution unknown, and the KCP then holds every membership
// change, which would mask whatever gate a test means to exercise.
func controlPlaneBootstrap(t *testing.T, ctx context.Context, c client.Client, kcp *controlplanev1beta2.KairosControlPlane, clusterName, name, distribution string) clusterv1.Bootstrap {
	t.Helper()
	return controlPlaneBootstrapWithHostname(t, ctx, c, kcp, clusterName, name, distribution, "")
}

// controlPlaneBootstrapWithHostname is controlPlaneBootstrap with
// spec.hostname set on the KairosConfig.
func controlPlaneBootstrapWithHostname(t *testing.T, ctx context.Context, c client.Client, kcp *controlplanev1beta2.KairosControlPlane, clusterName, name, distribution, hostname string) clusterv1.Bootstrap {
	t.Helper()
	NewWithT(t).Expect(c.Create(ctx, &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: kcp.Namespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         clusterName,
				clusterv1.MachineControlPlaneLabel: "",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kcp, controlplanev1beta2.GroupVersion.WithKind("KairosControlPlane")),
			},
		},
		Spec: bootstrapv1beta2.KairosConfigSpec{
			Role:              "control-plane",
			Distribution:      distribution,
			KubernetesVersion: kcp.Spec.Version,
			Hostname:          hostname,
		},
	})).To(Succeed())
	return clusterv1.Bootstrap{ConfigRef: clusterv1.ContractVersionedObjectReference{
		APIGroup: bootstrapv1beta2.GroupVersion.Group,
		Kind:     "KairosConfig",
		Name:     name,
	}}
}

// recreateKairosConfig deletes the KairosConfig at key and creates it again
// under the same name, with its labels, annotations, owner references and
// spec, after mutate. The role, distribution and control-plane hostname of a
// stored KairosConfig cannot be updated (KD-88), so a test that needs a
// KairosConfig to say something else replaces it, as an operator would have
// to. It waits for the old object to be gone, which takes the bootstrap
// controller's finalizer removal where that controller runs.
func recreateKairosConfig(g *WithT, ctx context.Context, c client.Client, key types.NamespacedName, mutate func(*bootstrapv1beta2.KairosConfig)) {
	old := &bootstrapv1beta2.KairosConfig{}
	g.Expect(c.Get(ctx, key, old)).To(Succeed())
	fresh := &bootstrapv1beta2.KairosConfig{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       old.Namespace,
			Name:            old.Name,
			Labels:          old.Labels,
			Annotations:     old.Annotations,
			OwnerReferences: old.OwnerReferences,
		},
		Spec: *old.Spec.DeepCopy(),
	}
	mutate(fresh)
	g.Expect(c.Delete(ctx, old)).To(Succeed())
	g.Eventually(func() bool {
		return apierrors.IsNotFound(c.Get(ctx, key, &bootstrapv1beta2.KairosConfig{}))
	}, 30*time.Second, 200*time.Millisecond).Should(BeTrue())
	g.Expect(c.Create(ctx, fresh)).To(Succeed())
}
