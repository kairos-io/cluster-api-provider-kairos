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
	"errors"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
	"github.com/kairos-io/cluster-api-provider-kairos/internal/controllers/controlplane"
)

const (
	etcdLeaveHookKey              = clusterv1.PreTerminateDeleteHookAnnotationPrefix + "/kairos-etcd-leave"
	etcdLeaveRequestedAtKey       = "controlplane.cluster.x-k8s.io/etcd-leave-requested-at"
	etcdMemberLeaveRequestedAtKey = "controlplane.cluster.x-k8s.io/etcd-member-leave-requested-at"
)

var k0sEtcdMemberGVK = schema.GroupVersionKind{Group: "etcd.k0sproject.io", Version: "v1beta1", Kind: "EtcdMember"}

// installK0sEtcdMemberCRD installs k0s's EtcdMember CRD in the envtest
// apiserver, which plays the workload cluster here: cluster-scoped, with a
// status subresource and spec.leave a boolean, as k0s defines it.
func installK0sEtcdMemberCRD(t *testing.T, cfg *rest.Config) {
	t.Helper()
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "etcdmembers.etcd.k0sproject.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: k0sEtcdMemberGVK.Group,
			Scope: apiextensionsv1.ClusterScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "etcdmembers", Singular: "etcdmember", Kind: "EtcdMember", ListKind: "EtcdMemberList",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: k0sEtcdMemberGVK.Version, Served: true, Storage: true,
				Subresources: &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"leave": {Type: "boolean"},
						}},
						"status": {Type: "object", XPreserveUnknownFields: ptr.To(true)},
					},
				}},
			}},
		},
	}
	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{CRDs: []*apiextensionsv1.CustomResourceDefinition{crd}}); err != nil {
		t.Fatal(err)
	}
}

// createK0sEtcdMember writes an EtcdMember the way k0s does: the object, then
// its status (peer address and Joined=True) through the status subresource.
func createK0sEtcdMember(t *testing.T, ctx context.Context, wc client.Client, name, peer string) {
	t.Helper()
	g := NewWithT(t)
	u := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{}}}
	u.SetGroupVersionKind(k0sEtcdMemberGVK)
	u.SetName(name)
	g.Expect(wc.Create(ctx, u)).To(Succeed())
	setK0sEtcdMemberJoined(t, ctx, wc, name, peer, "True")
}

func setK0sEtcdMemberJoined(t *testing.T, ctx context.Context, wc client.Client, name, peer, joined string) {
	t.Helper()
	NewWithT(t).Eventually(func() error {
		u := getK0sEtcdMember(ctx, wc, name)
		if u == nil {
			return errors.New("EtcdMember not found")
		}
		u.Object["status"] = map[string]interface{}{
			"peerAddress": peer,
			"memberID":    "8e9e05c52164694d",
			"conditions": []interface{}{map[string]interface{}{
				"type": "Joined", "status": joined, "lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
			}},
		}
		return wc.Status().Update(ctx, u)
	}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
}

func getK0sEtcdMember(ctx context.Context, wc client.Client, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(k0sEtcdMemberGVK)
	if err := wc.Get(ctx, client.ObjectKey{Name: name}, u); err != nil {
		return nil
	}
	return u
}

func k0sEtcdMemberLeave(ctx context.Context, wc client.Client, name string) (bool, error) {
	u := getK0sEtcdMember(ctx, wc, name)
	if u == nil {
		return false, errors.New("EtcdMember not found")
	}
	leave, _, err := unstructured.NestedBool(u.Object, "spec", "leave")
	return leave, err
}

// recordingMembers stands in for MemberClientFactory: every member address
// reaches the envtest apiserver, and every address asked for is recorded.
type recordingMembers struct {
	mu     sync.Mutex
	dialed map[string]bool
}

func (m *recordingMembers) addresses() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for ip := range m.dialed {
		out = append(out, ip)
	}
	return out
}

// TestHA_K0sEtcdLeave_StuckLeaveEndsOnlyWhenK0sReportsTheMemberGone is the
// 2026-10-16 lab case after a controller upgrade: a k0s control-plane Machine
// has been held at the etcd-leave hook for 20 minutes, its node never acked,
// and the control-plane endpoint does not answer. The KCP reaches a staying
// member directly, sets spec.leave on the leaving node's EtcdMember only, and
// removes the hook only after k0s reports that member Joined=False.
func TestHA_K0sEtcdLeave_StuckLeaveEndsOnlyWhenK0sReportsTheMemberGone(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	members := &recordingMembers{dialed: map[string]bool{}}
	ctx, c, cfg, _, teardown := startKCPEnvtest(t, func(r *controlplane.KairosControlPlaneReconciler, cfg *rest.Config) {
		r.WorkloadClientFactory = func(context.Context, *clusterv1.Cluster) (client.Client, error) {
			return nil, errors.New("the control-plane endpoint does not answer")
		}
		r.MemberClientFactory = func(_ context.Context, _ *clusterv1.Cluster, ip string) (client.Client, error) {
			members.mu.Lock()
			members.dialed[ip] = true
			members.mu.Unlock()
			return envtestWorkloadClient(t, cfg), nil
		}
	})
	defer teardown()
	installK0sEtcdMemberCRD(t, cfg)
	wc := envtestWorkloadClient(t, cfg)

	const (
		nsName      = "ha-k0s-leave"
		clusterName = "ha-k0s-leave-cluster"
		kcpName     = "ha-k0s-leave-kcp"
		leaverIP    = "10.11.0.10"
	)
	haFixture(t, ctx, c, nsName, clusterName, kcpName, "k0s")
	endpointHost := testControlPlaneEndpoint().Host
	vip := getKCP(g, ctx, c, nsName, kcpName).Spec.HA.VIP.Address

	// Every node reported a healthy voting member at boot.
	etcdStatusName := bootstrapv1beta2.EtcdStatusSecretName(clusterName)
	g.Eventually(func() error {
		es := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Name: etcdStatusName, Namespace: nsName}, es); err != nil {
			return err
		}
		es.Data = map[string][]byte{}
		for _, n := range []string{"leaver-node", "keeper-a", "keeper-b"} {
			es.Data[n] = []byte(`{"name":"` + n + `","healthy":true,"voting":true,"members":3,"reportedAt":"t"}`)
		}
		return c.Update(ctx, es)
	}, 15*time.Second, 500*time.Millisecond).Should(Succeed())

	// k0s's view: one EtcdMember per controller, all joined.
	createK0sEtcdMember(t, ctx, wc, "leaver-node", leaverIP)
	createK0sEtcdMember(t, ctx, wc, "keeper-a", "10.11.0.11")
	createK0sEtcdMember(t, ctx, wc, "keeper-b", "10.11.0.12")

	// The Machines. The leaver holds the VIP; a keeper reports the endpoint
	// host too. Neither address may ever be dialled.
	kcp := getKCP(g, ctx, c, nsName, kcpName)
	mk := func(name, node string, annotations map[string]string, addrs ...string) *clusterv1.Machine {
		m := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: nsName,
				Labels:          map[string]string{clusterv1.ClusterNameLabel: clusterName, clusterv1.MachineControlPlaneLabel: ""},
				Annotations:     annotations,
				Finalizers:      []string{"test.kairos.io/hold"},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(kcp, controlplanev1beta2.GroupVersion.WithKind("KairosControlPlane"))},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName:       clusterName,
				Version:           kcp.Spec.Version,
				Bootstrap:         clusterv1.Bootstrap{DataSecretName: ptr.To("placeholder")},
				InfrastructureRef: testMachineInfraRef("infra-" + name),
			},
		}
		g.Expect(c.Create(ctx, m)).To(Succeed())
		m.Status.NodeRef = clusterv1.MachineNodeReference{Name: node}
		m.Status.Phase = string(clusterv1.MachinePhaseRunning)
		for _, a := range addrs {
			m.Status.Addresses = append(m.Status.Addresses, clusterv1.MachineAddress{Type: clusterv1.MachineInternalIP, Address: a})
		}
		g.Expect(c.Status().Update(ctx, m)).To(Succeed())
		return m
	}
	stuckSince := time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)
	leaver := mk(kcpName+"-0", "leaver-node", map[string]string{etcdLeaveHookKey: "", etcdLeaveRequestedAtKey: stuckSince}, leaverIP, vip)
	mk(kcpName+"-1", "keeper-a", nil, "10.11.0.11", endpointHost)
	mk(kcpName+"-2", "keeper-b", nil, "10.11.0.12")
	g.Expect(c.Delete(ctx, leaver)).To(Succeed())

	leaverKey := types.NamespacedName{Name: leaver.Name, Namespace: nsName}
	getLeaver := func() *clusterv1.Machine {
		m := &clusterv1.Machine{}
		g.Expect(c.Get(ctx, leaverKey, m)).To(Succeed())
		return m
	}

	// The KCP hands the leave to k0s, through a member.
	g.Eventually(func() (bool, error) { return k0sEtcdMemberLeave(ctx, wc, "leaver-node") },
		30*time.Second, 500*time.Millisecond).Should(BeTrue(), "spec.leave must be set on the leaving node's EtcdMember")
	got := getLeaver()
	g.Expect(got.Annotations).To(HaveKey(etcdMemberLeaveRequestedAtKey))
	g.Expect(got.Annotations).To(HaveKey(etcdLeaveHookKey))

	// While k0s still reports the member joined, the hook stays.
	touchKCP(g, ctx, c, nsName, kcpName)
	g.Consistently(func() map[string]string { return getLeaver().Annotations },
		4*time.Second, 500*time.Millisecond).Should(HaveKey(etcdLeaveHookKey))

	// k0s removes the member and reports it.
	setK0sEtcdMemberJoined(t, ctx, wc, "leaver-node", leaverIP, "False")
	touchKCP(g, ctx, c, nsName, kcpName)
	g.Eventually(func() map[string]string { return getLeaver().Annotations },
		45*time.Second, 500*time.Millisecond).ShouldNot(HaveKey(etcdLeaveHookKey), "the hook goes once k0s reports Joined=False")

	// No other member was touched, and the leave never went to the leaving
	// member, the endpoint host or the VIP.
	for _, keeper := range []string{"keeper-a", "keeper-b"} {
		leave, err := k0sEtcdMemberLeave(ctx, wc, keeper)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(leave).To(BeFalse(), "EtcdMember %s must not be asked to leave", keeper)
	}
	g.Expect(members.addresses()).ToNot(BeEmpty())
	g.Expect(members.addresses()).To(HaveEach(BeElementOf("10.11.0.11", "10.11.0.12")))
}
