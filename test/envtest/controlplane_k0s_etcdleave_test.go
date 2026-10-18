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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
// reaches the envtest apiserver, and every address that carries etcd-leave
// traffic is recorded. The KCP's read-only workload identity checks (the
// kube-system Namespace, ADR 0015 PR-2) also go to members, including members
// that are not leaving yet; they are not etcd-leave traffic and are not
// recorded.
type recordingMembers struct {
	mu     sync.Mutex
	dialed map[string]bool
}

// record marks ip as having carried etcd-leave traffic.
func (m *recordingMembers) record(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dialed[ip] = true
}

// client returns a client of the envtest apiserver for member ip that
// records every request except the workload identity read.
func (m *recordingMembers) client(t *testing.T, cfg *rest.Config, ip string) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	identityRead := func(obj client.Object) bool {
		_, ok := obj.(*corev1.Namespace)
		return ok
	}
	return interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if !identityRead(obj) {
				m.record(ip)
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			m.record(ip)
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			m.record(ip)
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			m.record(ip)
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
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

// k0sLeaveCluster is a three-member k0s control plane in envtest whose
// Machine <kcp>-0 (Node leaver-node, 10.11.0.10, holding the VIP) is being
// deleted. The envtest apiserver is also the workload cluster. The
// control-plane endpoint does not answer; every member address reaches the
// envtest apiserver, and each one asked for is recorded.
type k0sLeaveCluster struct {
	ctx       context.Context
	c         client.Client // management
	wc        client.Client // workload
	members   *recordingMembers
	nsName    string
	kcpName   string
	leaverKey types.NamespacedName
	// unregistered is k0sLeaveOptions.unregisteredLeaver.
	unregistered bool
}

const k0sLeaverIP = "10.11.0.10"

// startK0sLeaveCluster builds the cluster. leaverAnnotations are set on the
// leaving Machine next to the etcd-leave hook, before it is deleted.
func startK0sLeaveCluster(t *testing.T, name string, leaverAnnotations map[string]string) (*k0sLeaveCluster, func()) {
	t.Helper()
	return startK0sLeaveClusterWith(t, name, k0sLeaveOptions{leaverAnnotations: leaverAnnotations})
}

// k0sLeaveOptions vary the cluster startK0sLeaveClusterWith builds.
type k0sLeaveOptions struct {
	// leaverAnnotations are set on the leaving Machine next to the hook.
	leaverAnnotations map[string]string
	// unregisteredLeaver: the leaving Machine never got a NodeRef (ADR 0015
	// PR-3). Its node, and its EtcdMember, go by the Machine name, which is
	// what the renderer names a node with no spec.hostname. It never reported
	// into etcd-status.
	unregisteredLeaver bool
	// keepLeaver: the leaving Machine is created but not deleted, so the
	// control plane is in a steady state of three members.
	keepLeaver bool
	// extraMembers are EtcdMembers (name → peer address) with no Machine.
	extraMembers map[string]string
}

// leaverNode is the leaving node's name: its NodeRef name, or its Machine
// name when it has no NodeRef.
func (k *k0sLeaveCluster) leaverNode() string {
	if k.unregistered {
		return k.leaverKey.Name
	}
	return "leaver-node"
}

// startK0sLeaveClusterWith builds the cluster with opts.
func startK0sLeaveClusterWith(t *testing.T, name string, opts k0sLeaveOptions) (*k0sLeaveCluster, func()) {
	t.Helper()
	g := NewWithT(t)
	leaverAnnotations := opts.leaverAnnotations
	members := &recordingMembers{dialed: map[string]bool{}}
	ctx, c, cfg, _, teardown := startKCPEnvtest(t, func(r *controlplane.KairosControlPlaneReconciler, cfg *rest.Config) {
		r.WorkloadClientFactory = func(context.Context, *clusterv1.Cluster) (client.Client, error) {
			return nil, errors.New("the control-plane endpoint does not answer")
		}
		r.MemberClientFactory = func(_ context.Context, _ *clusterv1.Cluster, ip string) (client.Client, error) {
			return members.client(t, cfg, ip), nil
		}
	})
	installK0sEtcdMemberCRD(t, cfg)
	k := &k0sLeaveCluster{ctx: ctx, c: c, wc: envtestWorkloadClient(t, cfg), members: members, nsName: name, kcpName: name + "-kcp",
		unregistered: opts.unregisteredLeaver}
	k.leaverKey = types.NamespacedName{Name: k.kcpName + "-0", Namespace: k.nsName}
	clusterName := name + "-cluster"
	haFixture(t, ctx, c, k.nsName, clusterName, k.kcpName, "k0s")
	// The cluster is long past its first contact, so the KCP pinned its
	// identity then (ADR 0015 PR-2). The envtest apiserver is the workload.
	pinEnvtestWorkloadIdentity(t, ctx, c, k.wc, k.nsName, k.kcpName)
	endpointHost := testControlPlaneEndpoint().Host
	vip := getKCP(g, ctx, c, k.nsName, k.kcpName).Spec.HA.VIP.Address

	// Every node reported a healthy voting member at boot.
	etcdStatusName := bootstrapv1beta2.EtcdStatusSecretName(clusterName)
	g.Eventually(func() error {
		es := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Name: etcdStatusName, Namespace: k.nsName}, es); err != nil {
			return err
		}
		es.Data = map[string][]byte{}
		reporters := []string{"keeper-a", "keeper-b"}
		if !k.unregistered {
			reporters = append(reporters, "leaver-node")
		}
		for _, n := range reporters {
			es.Data[n] = []byte(`{"name":"` + n + `","healthy":true,"voting":true,"members":3,"reportedAt":"t"}`)
		}
		return c.Update(ctx, es)
	}, 15*time.Second, 500*time.Millisecond).Should(Succeed())

	// k0s's view: one EtcdMember per controller, all joined, plus any member
	// with no Machine.
	createK0sEtcdMember(t, ctx, k.wc, k.leaverNode(), k0sLeaverIP)
	createK0sEtcdMember(t, ctx, k.wc, "keeper-a", "10.11.0.11")
	createK0sEtcdMember(t, ctx, k.wc, "keeper-b", "10.11.0.12")
	for member, peer := range opts.extraMembers {
		createK0sEtcdMember(t, ctx, k.wc, member, peer)
	}

	// The Machines. The leaver holds the VIP; a keeper reports the endpoint
	// host too. Neither address may ever be dialled.
	kcp := getKCP(g, ctx, c, k.nsName, k.kcpName)
	mk := func(name, node string, annotations map[string]string, addrs ...string) *clusterv1.Machine {
		m := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: k.nsName,
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
		if node != "" {
			m.Status.NodeRef = clusterv1.MachineNodeReference{Name: node}
			m.Status.Phase = string(clusterv1.MachinePhaseRunning)
		} else {
			m.Status.Phase = string(clusterv1.MachinePhaseProvisioned)
		}
		for _, a := range addrs {
			m.Status.Addresses = append(m.Status.Addresses, clusterv1.MachineAddress{Type: clusterv1.MachineInternalIP, Address: a})
		}
		g.Expect(c.Status().Update(ctx, m)).To(Succeed())
		return m
	}
	annotations := map[string]string{etcdLeaveHookKey: ""}
	for key, v := range leaverAnnotations {
		annotations[key] = v
	}
	leaverNodeRef := "leaver-node"
	if k.unregistered {
		leaverNodeRef = ""
	}
	leaver := mk(k.leaverKey.Name, leaverNodeRef, annotations, k0sLeaverIP, vip)
	mk(k.kcpName+"-1", "keeper-a", nil, "10.11.0.11", endpointHost)
	mk(k.kcpName+"-2", "keeper-b", nil, "10.11.0.12")
	if !opts.keepLeaver {
		g.Expect(c.Delete(ctx, leaver)).To(Succeed())
	}
	return k, teardown
}

func (k *k0sLeaveCluster) leaver(g *WithT) *clusterv1.Machine {
	m := &clusterv1.Machine{}
	g.Expect(k.c.Get(k.ctx, k.leaverKey, m)).To(Succeed())
	return m
}

// setLeaverDeletingReason plays CAPI's Machine controller: it reports the
// stage of the leaver's deletion in the v1beta2 Deleting condition.
func (k *k0sLeaveCluster) setLeaverDeletingReason(g *WithT, reason string) {
	g.Eventually(func() error {
		m := &clusterv1.Machine{}
		if err := k.c.Get(k.ctx, k.leaverKey, m); err != nil {
			return err
		}
		apimeta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type: clusterv1.MachineDeletingCondition, Status: metav1.ConditionTrue, Reason: reason,
		})
		return k.c.Status().Update(k.ctx, m)
	}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
}

// leaveRequestFor returns the leave ConfigMap's value for node, "" when there
// is none.
func (k *k0sLeaveCluster) leaveRequestFor(node string) string {
	cm := &corev1.ConfigMap{}
	if err := k.wc.Get(k.ctx, types.NamespacedName{Namespace: "kube-system", Name: "kairos-etcd-leave"}, cm); err != nil {
		return ""
	}
	return cm.Data[node]
}

// TestHA_K0sEtcdLeave_StuckLeaveEndsOnlyWhenK0sReportsTheMemberGone is the
// 2026-10-16 lab case after a controller upgrade: a k0s control-plane Machine
// has been held at the etcd-leave hook for 20 minutes, its node never acked,
// and the control-plane endpoint does not answer. The KCP reaches a staying
// member directly, sets spec.leave on the leaving node's EtcdMember only, and
// removes the hook only after k0s reports that member Joined=False. CAPI's
// Deleting condition is not set at all here: a leave already requested
// carries on whatever stage CAPI reports.
func TestHA_K0sEtcdLeave_StuckLeaveEndsOnlyWhenK0sReportsTheMemberGone(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	stuckSince := time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)
	k, teardown := startK0sLeaveCluster(t, "ha-k0s-leave", map[string]string{etcdLeaveRequestedAtKey: stuckSince})
	defer teardown()
	ctx, c, wc := k.ctx, k.c, k.wc

	// The KCP hands the leave to k0s, through a member.
	g.Eventually(func() (bool, error) { return k0sEtcdMemberLeave(ctx, wc, "leaver-node") },
		30*time.Second, 500*time.Millisecond).Should(BeTrue(), "spec.leave must be set on the leaving node's EtcdMember")
	got := k.leaver(g)
	g.Expect(got.Annotations).To(HaveKey(etcdMemberLeaveRequestedAtKey))
	g.Expect(got.Annotations).To(HaveKey(etcdLeaveHookKey))

	// While k0s still reports the member joined, the hook stays.
	touchKCP(g, ctx, c, k.nsName, k.kcpName)
	g.Consistently(func() map[string]string { return k.leaver(g).Annotations },
		4*time.Second, 500*time.Millisecond).Should(HaveKey(etcdLeaveHookKey))

	// k0s removes the member and reports it.
	setK0sEtcdMemberJoined(t, ctx, wc, "leaver-node", k0sLeaverIP, "False")
	touchKCP(g, ctx, c, k.nsName, k.kcpName)
	g.Eventually(func() map[string]string { return k.leaver(g).Annotations },
		45*time.Second, 500*time.Millisecond).ShouldNot(HaveKey(etcdLeaveHookKey), "the hook goes once k0s reports Joined=False")

	// No other member was touched, and the leave never went to the leaving
	// member, the endpoint host or the VIP.
	for _, keeper := range []string{"keeper-a", "keeper-b"} {
		leave, err := k0sEtcdMemberLeave(ctx, wc, keeper)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(leave).To(BeFalse(), "EtcdMember %s must not be asked to leave", keeper)
	}
	g.Expect(k.members.addresses()).ToNot(BeEmpty())
	g.Expect(k.members.addresses()).To(HaveEach(BeElementOf("10.11.0.11", "10.11.0.12")))
}

// TestHA_K0sEtcdLeave_StartsOnlyAtPreTerminate: a hooked k0s Machine whose
// Node CAPI is still draining gets no leave request and no workload traffic;
// the hook stays. Once CAPI reports it is waiting on the pre-terminate hooks,
// the KCP requests the leave through a staying member and starts the
// timeout clock then.
func TestHA_K0sEtcdLeave_StartsOnlyAtPreTerminate(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	g := NewWithT(t)
	k, teardown := startK0sLeaveCluster(t, "ha-k0s-leave-drain", nil)
	defer teardown()
	k.setLeaverDeletingReason(g, clusterv1.MachineDeletingDrainingNodeReason)

	// While CAPI drains, nothing happens to etcd.
	touchKCP(g, k.ctx, k.c, k.nsName, k.kcpName)
	g.Consistently(func() string { return k.leaveRequestFor("leaver-node") },
		6*time.Second, 500*time.Millisecond).Should(BeEmpty(), "no leave is requested while CAPI drains the node")
	got := k.leaver(g)
	g.Expect(got.Annotations).ToNot(HaveKey(etcdLeaveRequestedAtKey))
	g.Expect(got.Annotations).To(HaveKey(etcdLeaveHookKey))
	g.Expect(k.members.addresses()).To(BeEmpty(), "no workload traffic while CAPI drains the node")

	// CAPI is done with the Node and waits on the pre-terminate hooks.
	k.setLeaverDeletingReason(g, clusterv1.MachineDeletingWaitingForPreTerminateHookReason)
	g.Eventually(func() string { return k.leaveRequestFor("leaver-node") },
		30*time.Second, 500*time.Millisecond).Should(Equal("leave-requested"))
	g.Eventually(func() map[string]string { return k.leaver(g).Annotations },
		10*time.Second, 500*time.Millisecond).Should(HaveKey(etcdLeaveRequestedAtKey))
	got = k.leaver(g)
	requestedAt, err := time.Parse(time.RFC3339, got.Annotations[etcdLeaveRequestedAtKey])
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(requestedAt).To(BeTemporally(">", got.DeletionTimestamp.Add(5*time.Second)),
		"the timeout clock starts at the request, not at the deletion")
	g.Expect(got.Annotations).To(HaveKey(etcdLeaveHookKey))
	g.Expect(k.members.addresses()).To(HaveEach(BeElementOf("10.11.0.11", "10.11.0.12")))
}
