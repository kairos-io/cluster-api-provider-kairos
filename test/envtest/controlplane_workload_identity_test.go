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

package envtest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/controllers/controlplane"
)

// otherClusterUID is the kube-system UID of the "other" cluster: the one a
// member that bootstrapped its own etcd serves, with the same CA.
const otherClusterUID = "99999999-9999-9999-9999-999999999999"

// recordingRecorder keeps every event, so a test can assert on them without
// a FakeRecorder's bounded channel ever blocking the reconciler.
type recordingRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recordingRecorder) Event(_ runtime.Object, eventtype, reason, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, eventtype+" "+reason+" "+message)
}

func (r *recordingRecorder) Eventf(o runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	r.Event(o, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (r *recordingRecorder) AnnotatedEventf(o runtime.Object, _ map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	r.Eventf(o, eventtype, reason, messageFmt, args...)
}

func (r *recordingRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// switchingWorkloadClient is a client of the envtest apiserver, which plays
// the workload cluster, that reports otherClusterUID as the kube-system UID
// once switched is set. The test cannot recreate kube-system itself: the
// NamespaceLifecycle admission plugin refuses to delete it, and envtest runs
// no namespace controller to finish a delete. Changing the UID the KCP reads
// is the same thing as far as the KCP can tell: an apiserver, trusted by the
// same CA, that serves a different cluster.
func switchingWorkloadClient(t *testing.T, cfg *rest.Config, switched *atomic.Bool) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if ns, ok := obj.(*corev1.Namespace); ok && ns.Name == metav1.NamespaceSystem && switched.Load() {
				ns.UID = types.UID(otherClusterUID)
			}
			return nil
		},
	})
}

// kcpCondition returns the KCP's condition of type ct, or nil.
func kcpCondition(kcp *controlplanev1beta2.KairosControlPlane, ct string) *clusterv1.Condition {
	for i := range kcp.Status.Conditions {
		if string(kcp.Status.Conditions[i].Type) == ct {
			return &kcp.Status.Conditions[i]
		}
	}
	return nil
}

// TestHA_WorkloadIdentityChange_FreezesMembership is ADR 0015 PR-2 end to
// end. A three-member k0s control plane has its identity pinned. Then the
// workload apiserver starts answering as another cluster, and in the same
// moment an operator scales the control plane down and deletes a member that
// is at its etcd-leave hook. The KCP must freeze: EtcdHealthy and Available
// False with reason WorkloadClusterIdentityChanged, a Warning event, no
// Machine created or deleted, the hook kept, and nothing written to the
// workload cluster. An operator re-pin then clears the freeze.
func TestHA_WorkloadIdentityChange_FreezesMembership(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	var switched atomic.Bool
	rec := &recordingRecorder{}
	ctx, c, cfg, _, teardown := startKCPEnvtest(t, func(r *controlplane.KairosControlPlaneReconciler, cfg *rest.Config) {
		r.Recorder = rec
		r.WorkloadClientFactory = func(context.Context, *clusterv1.Cluster) (client.Client, error) {
			return switchingWorkloadClient(t, cfg, &switched), nil
		}
		r.MemberClientFactory = func(context.Context, *clusterv1.Cluster, string) (client.Client, error) {
			return switchingWorkloadClient(t, cfg, &switched), nil
		}
	})
	defer teardown()
	wc := envtestWorkloadClient(t, cfg)

	const (
		nsName      = "ha-identity"
		clusterName = "ha-identity-cluster"
		kcpName     = "ha-identity-kcp"
	)
	haFixture(t, ctx, c, nsName, clusterName, kcpName, "k0s")
	pinnedUID := envtestKubeSystemUID(t, ctx, wc)
	pinEnvtestWorkloadIdentity(t, ctx, c, wc, nsName, kcpName)

	// Every member reported a healthy voting etcd member.
	etcdStatusName := bootstrapv1beta2.EtcdStatusSecretName(clusterName)
	g.Eventually(func() error {
		es := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Name: etcdStatusName, Namespace: nsName}, es); err != nil {
			return err
		}
		es.Data = map[string][]byte{}
		for i := 0; i < 3; i++ {
			n := fmt.Sprintf("node-%d", i)
			es.Data[n] = []byte(`{"name":"` + n + `","healthy":true,"voting":true,"members":3,"reportedAt":"t"}`)
		}
		return c.Update(ctx, es)
	}, 15*time.Second, 500*time.Millisecond).Should(Succeed())

	// Three joined, hooked k0s members.
	kcp := getKCP(g, ctx, c, nsName, kcpName)
	for i := 0; i < 3; i++ {
		m := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%d", kcpName, i),
				Namespace: nsName,
				Labels: map[string]string{
					clusterv1.ClusterNameLabel:         clusterName,
					clusterv1.MachineControlPlaneLabel: "",
				},
				Annotations:     map[string]string{etcdLeaveHookKey: ""},
				Finalizers:      []string{"test.kairos.io/hold"},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(kcp, controlplanev1beta2.GroupVersion.WithKind("KairosControlPlane"))},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName:       clusterName,
				Version:           kcp.Spec.Version,
				Bootstrap:         clusterv1.Bootstrap{DataSecretName: ptr.To("placeholder")},
				InfrastructureRef: testMachineInfraRef(fmt.Sprintf("infra-%d", i)),
			},
		}
		g.Expect(c.Create(ctx, m)).To(Succeed())
		m.Status.NodeRef = clusterv1.MachineNodeReference{Name: fmt.Sprintf("node-%d", i)}
		m.Status.Phase = string(clusterv1.MachinePhaseRunning)
		g.Expect(c.Status().Update(ctx, m)).To(Succeed())
	}
	touchKCP(g, ctx, c, nsName, kcpName)
	g.Consistently(func() string {
		return getKCP(g, ctx, c, nsName, kcpName).Annotations[controlplane.WorkloadClusterIdentityMismatchAnnotation]
	}, 3*time.Second, 500*time.Millisecond).Should(BeEmpty(), "the pinned cluster answers: no freeze")

	// The workload apiserver now serves another cluster.
	switched.Store(true)

	// The operator scales down, and deletes a member that CAPI holds at its
	// etcd-leave hook. Either would normally make the KCP act.
	g.Eventually(func() error {
		cur := getKCP(g, ctx, c, nsName, kcpName)
		cur.Spec.Replicas = ptr.To(int32(2))
		return c.Update(ctx, cur)
	}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
	leaverKey := types.NamespacedName{Name: kcpName + "-1", Namespace: nsName}
	leaver := &clusterv1.Machine{}
	g.Expect(c.Get(ctx, leaverKey, leaver)).To(Succeed())
	g.Expect(c.Delete(ctx, leaver)).To(Succeed())
	g.Eventually(func() error {
		m := &clusterv1.Machine{}
		if err := c.Get(ctx, leaverKey, m); err != nil {
			return err
		}
		apimeta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type: clusterv1.MachineDeletingCondition, Status: metav1.ConditionTrue, Reason: clusterv1.MachineDeletingWaitingForPreTerminateHookReason,
		})
		return c.Status().Update(ctx, m)
	}, 10*time.Second, 200*time.Millisecond).Should(Succeed())

	// The KCP freezes.
	g.Eventually(func() string {
		return getKCP(g, ctx, c, nsName, kcpName).Annotations[controlplane.WorkloadClusterIdentityMismatchAnnotation]
	}, 30*time.Second, 500*time.Millisecond).Should(And(
		HavePrefix(otherClusterUID+" via "),
		HaveSuffix("; pinned "+pinnedUID),
	))
	g.Eventually(func(g Gomega) {
		got := getKCP(NewWithT(t), ctx, c, nsName, kcpName)
		for _, ct := range []string{string(clusterv1.ReadyCondition), controlplanev1beta2.EtcdHealthyCondition, controlplanev1beta2.AvailableCondition} {
			cond := kcpCondition(got, ct)
			g.Expect(cond).ToNot(BeNil(), ct)
			g.Expect(cond.Status).To(Equal(corev1.ConditionFalse), ct)
			g.Expect(cond.Reason).To(Equal(controlplanev1beta2.WorkloadClusterIdentityChangedReason), ct)
			g.Expect(cond.Severity).To(Equal(clusterv1.ConditionSeverityError), ct)
		}
	}, 30*time.Second, 500*time.Millisecond).Should(Succeed())
	g.Expect(strings.Join(rec.all(), "\n")).To(And(
		ContainSubstring("Warning WorkloadClusterIdentityChanged"),
		ContainSubstring(otherClusterUID),
		ContainSubstring(pinnedUID),
	))

	// Nothing is created or deleted, the hook stays, and nothing is written
	// to the workload cluster.
	g.Consistently(func(g Gomega) {
		list := &clusterv1.MachineList{}
		g.Expect(c.List(ctx, list, client.InNamespace(nsName))).To(Succeed())
		g.Expect(list.Items).To(HaveLen(3), "no control-plane Machine may be created")
		for _, m := range list.Items {
			if m.Name == leaverKey.Name {
				g.Expect(m.Annotations).To(HaveKey(etcdLeaveHookKey), "the hook of the deleted member must stay")
				continue
			}
			g.Expect(m.DeletionTimestamp.IsZero()).To(BeTrue(), "the KCP must not delete %s", m.Name)
		}
		cm := &corev1.ConfigMap{}
		err := wc.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: "kairos-etcd-leave"}, cm)
		if err == nil {
			g.Expect(cm.Data).ToNot(HaveKey("node-1"), "no leave request may be written to the other cluster")
		}
	}, 6*time.Second, 500*time.Millisecond).Should(Succeed())

	// An operator who has verified that the cluster now answering is the one
	// to keep re-pins it. The freeze clears.
	g.Eventually(func() error {
		cur := getKCP(g, ctx, c, nsName, kcpName)
		cur.Annotations[controlplane.WorkloadClusterUIDAnnotation] = otherClusterUID
		return c.Update(ctx, cur)
	}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
	g.Eventually(func() map[string]string {
		return getKCP(g, ctx, c, nsName, kcpName).Annotations
	}, 30*time.Second, 500*time.Millisecond).ShouldNot(HaveKey(controlplane.WorkloadClusterIdentityMismatchAnnotation))
	g.Eventually(func() string {
		cond := kcpCondition(getKCP(g, ctx, c, nsName, kcpName), controlplanev1beta2.EtcdHealthyCondition)
		if cond == nil {
			return ""
		}
		return cond.Reason
	}, 30*time.Second, 500*time.Millisecond).ShouldNot(Equal(controlplanev1beta2.WorkloadClusterIdentityChangedReason))
	g.Expect(strings.Join(rec.all(), "\n")).To(ContainSubstring("Normal WorkloadClusterIdentityFreezeCleared"))
}
