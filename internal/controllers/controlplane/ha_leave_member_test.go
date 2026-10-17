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

package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	controlplanev1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/controlplane/v1beta2"
)

const (
	testEndpointHost = "10.0.0.200"
	testVIP          = "10.0.0.201"
)

// nodeMachine builds a control-plane Machine whose Node has the Machine's name,
// with the given addresses. A deleting Machine carries a finalizer so the fake
// client keeps it.
func nodeMachine(name string, deleting bool, addrs ...string) *clusterv1.Machine {
	m := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
	m.Status.NodeRef = clusterv1.MachineNodeReference{Name: name}
	for _, a := range addrs {
		m.Status.Addresses = append(m.Status.Addresses, clusterv1.MachineAddress{Type: clusterv1.MachineInternalIP, Address: a})
	}
	if deleting {
		now := metav1.Now()
		m.DeletionTimestamp = &now
		m.Finalizers = []string{"test.kairos.io/hold"}
	}
	return m
}

// leavingMachine is the Machine being removed: deleting, hooked, with a Node,
// at CAPI's pre-terminate stage.
func leavingMachine(name string, annotations map[string]string, addrs ...string) *clusterv1.Machine {
	m := nodeMachine(name, true, addrs...)
	m.Annotations = map[string]string{etcdLeaveHookAnnotation(): ""}
	for k, v := range annotations {
		m.Annotations[k] = v
	}
	return atPreTerminateHook(m)
}

func noNodeRef(m *clusterv1.Machine) *clusterv1.Machine {
	m.Status.NodeRef = clusterv1.MachineNodeReference{}
	return m
}

// haCluster is testCluster() with the control-plane endpoint set.
func haCluster() *clusterv1.Cluster {
	c := testCluster()
	c.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: testEndpointHost, Port: 6443}
	return c
}

// vipKCP is k0sKCP() with a kube-vip address.
func vipKCP() *controlplanev1beta2.KairosControlPlane {
	kcp := k0sKCP()
	kcp.Spec.HA = &controlplanev1beta2.HAConfig{VIP: &controlplanev1beta2.KubeVIPConfig{Address: testVIP}}
	return kcp
}

// fakeMembers stands in for MemberClientFactory: it hands out the client
// registered for an IP, fails like an unroutable address for any other, and
// records every IP it was asked for.
type fakeMembers struct {
	clients map[string]client.Client
	dialed  []string
}

func (f *fakeMembers) factory(_ context.Context, _ *clusterv1.Cluster, ip string) (client.Client, error) {
	f.dialed = append(f.dialed, ip)
	if c, ok := f.clients[ip]; ok {
		return c, nil
	}
	return nil, fmt.Errorf("dial tcp %s: connect: no route to host", ip)
}

// hungClient is a workload client whose ConfigMap reads never succeed.
func hungClient(scheme *runtime.Scheme) client.Client {
	return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(failConfigMapGets()).Build()
}

func leaveCM(g *WithT, wc client.Client) map[string]string {
	g.THelper()
	cm := &corev1.ConfigMap{}
	g.Expect(wc.Get(context.Background(), leaveConfigMapKey, cm)).To(Succeed())
	return cm.Data
}

func assertNoLeaveCM(g *WithT, wc client.Client) {
	g.THelper()
	cm := &corev1.ConfigMap{}
	err := wc.Get(context.Background(), leaveConfigMapKey, cm)
	g.Expect(err).To(HaveOccurred(), "nothing may be written through this apiserver")
}

func getMachine(g *WithT, c client.Client, name string) *clusterv1.Machine {
	g.THelper()
	m := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, m)).To(Succeed())
	return m
}

// TestLeaveCandidateIPs: the leave goes only to members that are staying,
// one address per member, never to the endpoint or the VIP.
func TestLeaveCandidateIPs(t *testing.T) {
	exclude := []string{testEndpointHost, testVIP}
	for _, tc := range []struct {
		name     string
		target   *clusterv1.Machine
		machines []*clusterv1.Machine
		want     []string
	}{
		{
			name:     "staying members, lowest address first",
			target:   nodeMachine("cp-0", true, "10.0.0.10"),
			machines: []*clusterv1.Machine{nodeMachine("cp-2", false, "10.0.0.12"), nodeMachine("cp-1", false, "10.0.0.11")},
			want:     []string{"10.0.0.11", "10.0.0.12"},
		},
		{
			name:     "another deleting Machine is skipped",
			target:   nodeMachine("cp-0", true, "10.0.0.10"),
			machines: []*clusterv1.Machine{nodeMachine("cp-1", false, "10.0.0.11"), nodeMachine("cp-3", true, "10.0.0.13")},
			want:     []string{"10.0.0.11"},
		},
		{
			name:     "an address the leaving Machine also reports is skipped",
			target:   nodeMachine("cp-0", true, "10.0.0.10"),
			machines: []*clusterv1.Machine{nodeMachine("cp-1", false, "10.0.0.10", "10.0.0.11")},
			want:     []string{"10.0.0.11"},
		},
		{
			name:     "the endpoint host and the VIP are skipped",
			target:   nodeMachine("cp-0", true, "10.0.0.10"),
			machines: []*clusterv1.Machine{nodeMachine("cp-1", false, testVIP, "10.0.0.11"), nodeMachine("cp-2", false, testEndpointHost, "10.0.0.12")},
			want:     []string{"10.0.0.11", "10.0.0.12"},
		},
		{
			name:     "Machines with a Node come first",
			target:   nodeMachine("cp-0", true, "10.0.0.10"),
			machines: []*clusterv1.Machine{noNodeRef(nodeMachine("cp-1", false, "10.0.0.1")), nodeMachine("cp-2", false, "10.0.0.12")},
			want:     []string{"10.0.0.12", "10.0.0.1"},
		},
		{
			name:     "names, loopback, link-local and unspecified addresses are dropped",
			target:   nodeMachine("cp-0", true, "10.0.0.10"),
			machines: []*clusterv1.Machine{nodeMachine("cp-1", false, "cp-1.example", "127.0.0.1", "::1", "fe80::1", "169.254.1.1", "0.0.0.0", "10.0.0.11")},
			want:     []string{"10.0.0.11"},
		},
		{
			name:   "an address two members report names neither",
			target: nodeMachine("cp-0", true, "10.0.0.10"),
			machines: []*clusterv1.Machine{
				nodeMachine("cp-1", false, "10.0.2.2", "10.0.0.11"),
				nodeMachine("cp-2", false, "10.0.2.2", "10.0.0.12"),
			},
			want: []string{"10.0.0.11", "10.0.0.12"},
		},
		{
			name:     "IPv6 addresses are canonical",
			target:   nodeMachine("cp-0", true, "fd00::10"),
			machines: []*clusterv1.Machine{nodeMachine("cp-1", false, "fd00:0:0:0:0:0:0:11")},
			want:     []string{"fd00::11"},
		},
		{
			name:     "the target is skipped even before it is marked deleting",
			target:   nodeMachine("cp-0", false, "10.0.0.10"),
			machines: []*clusterv1.Machine{nodeMachine("cp-1", false, "10.0.0.11")},
			want:     []string{"10.0.0.11"},
		},
		{
			name:     "no member left",
			target:   nodeMachine("cp-0", true, "10.0.0.10"),
			machines: nil,
			want:     []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			machines := append([]*clusterv1.Machine{tc.target}, tc.machines...)
			g.Expect(leaveCandidateIPs(machines, tc.target, exclude...)).To(Equal(tc.want))
		})
	}
}

// TestMemberRESTConfig: a member client dials the member's own address on the
// apiserver port, verifies the certificate for that address against the
// cluster CA, and gives up on any request after memberRequestTimeout.
func TestMemberRESTConfig(t *testing.T) {
	base := &rest.Config{
		Host:            "https://" + testEndpointHost + ":6443",
		BearerToken:     "kcp-token",
		TLSClientConfig: rest.TLSClientConfig{CAData: []byte(testWorkloadCA), ServerName: "endpoint.example", Insecure: true},
	}
	for _, tc := range []struct{ ip, host string }{
		{"10.0.0.11", "https://10.0.0.11:6443"},
		{"fd00::11", "https://[fd00::11]:6443"},
	} {
		t.Run(tc.ip, func(t *testing.T) {
			g := NewWithT(t)
			cfg, err := memberRESTConfig(base, tc.ip)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cfg.Host).To(Equal(tc.host))
			g.Expect(cfg.Timeout).To(Equal(5 * time.Second))
			g.Expect(cfg.CAData).To(Equal([]byte(testWorkloadCA)))
			g.Expect(cfg.BearerToken).To(Equal("kcp-token"))
			g.Expect(cfg.ServerName).To(BeEmpty(), "the member's own address is what the certificate is checked for")
			g.Expect(cfg.Insecure).To(BeFalse(), "verification is never skipped")
		})
	}
	g := NewWithT(t)
	g.Expect(base.Host).To(Equal("https://"+testEndpointHost+":6443"), "the endpoint config is left alone")
	g.Expect(base.ServerName).To(Equal("endpoint.example"))
	_, err := memberRESTConfig(base, "cp-1.example")
	g.Expect(err).To(HaveOccurred())
}

// TestReconcileMemberLeave_MemberAnswersWhileEndpointHangs: the leave
// progresses through a staying member while the endpoint hangs, which is the
// case where the leaving member holds the VIP.
func TestReconcileMemberLeave_MemberAnswersWhileEndpointHangs(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	target := leavingMachine("cp-0", nil, "10.0.0.10")
	machines := []*clusterv1.Machine{target, nodeMachine("cp-1", false, "10.0.0.11")}
	mgmt := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target, etcdStatusSecretForMembers("cp-0", "cp-1", "cp-2")).Build()
	member := fake.NewClientBuilder().WithScheme(scheme).Build()
	endpoint := hungClient(scheme)
	members := &fakeMembers{clients: map[string]client.Client{"10.0.0.11": member}}
	r := &KairosControlPlaneReconciler{Client: mgmt, Scheme: scheme,
		WorkloadClientFactory: staticWorkloadClient(endpoint), MemberClientFactory: members.factory}

	// First pass: the request is written through the member.
	done, err := r.reconcileMemberLeave(context.Background(), log.Log, vipKCP(), haCluster(), machines, target)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(leaveCM(g, member)).To(HaveKeyWithValue("cp-0", etcdLeaveRequestedValue))
	got := getMachine(g, mgmt, "cp-0")
	g.Expect(got.Annotations).To(HaveKey(etcdLeaveRequestedAtAnnotation))
	g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())

	// The node acks through the same member: the hook goes.
	cm := &corev1.ConfigMap{}
	g.Expect(member.Get(context.Background(), leaveConfigMapKey, cm)).To(Succeed())
	cm.Data["cp-0"] = etcdLeftValue
	g.Expect(member.Update(context.Background(), cm)).To(Succeed())
	done, err = r.reconcileMemberLeave(context.Background(), log.Log, vipKCP(), haCluster(), machines, got)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeTrue())
	assertHookGone(g, mgmt, "cp-0")
}

// TestReconcileMemberLeave_TriesTheNextMember: a member that does not answer
// is skipped, and the next one carries the whole reconcile.
func TestReconcileMemberLeave_TriesTheNextMember(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	target := leavingMachine("cp-0", nil, "10.0.0.10")
	machines := []*clusterv1.Machine{target, nodeMachine("cp-1", false, "10.0.0.11"), nodeMachine("cp-2", false, "10.0.0.12")}
	mgmt := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target).Build()
	first, second := hungClient(scheme), fake.NewClientBuilder().WithScheme(scheme).Build()
	members := &fakeMembers{clients: map[string]client.Client{"10.0.0.11": first, "10.0.0.12": second}}
	r := &KairosControlPlaneReconciler{Client: mgmt, Scheme: scheme,
		WorkloadClientFactory: staticWorkloadClient(hungClient(scheme)), MemberClientFactory: members.factory}

	done, err := r.reconcileMemberLeave(context.Background(), log.Log, vipKCP(), haCluster(), machines, target)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(members.dialed).To(Equal([]string{"10.0.0.11", "10.0.0.12"}))
	g.Expect(leaveCM(g, second)).To(HaveKeyWithValue("cp-0", etcdLeaveRequestedValue))
}

// TestReconcileMemberLeave_NoApiserverAnswersKeepsHook: when no member and
// not the endpoint answers, the hook stays and the error names every
// apiserver that was tried.
func TestReconcileMemberLeave_NoApiserverAnswersKeepsHook(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	target := leavingMachine("cp-0", map[string]string{etcdLeaveRequestedAtAnnotation: time.Now().UTC().Format(time.RFC3339)}, "10.0.0.10")
	machines := []*clusterv1.Machine{target, nodeMachine("cp-1", false, "10.0.0.11"), nodeMachine("cp-2", false, "10.0.0.12")}
	mgmt := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target, etcdStatusSecretForMembers("cp-0", "cp-1", "cp-2")).Build()
	members := &fakeMembers{clients: map[string]client.Client{"10.0.0.11": hungClient(scheme)}}
	r := &KairosControlPlaneReconciler{Client: mgmt, Scheme: scheme,
		WorkloadClientFactory: staticWorkloadClient(hungClient(scheme)), MemberClientFactory: members.factory}

	done, err := r.reconcileMemberLeave(context.Background(), log.Log, vipKCP(), haCluster(), machines, target)
	g.Expect(err).To(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(err.Error()).To(And(ContainSubstring("member 10.0.0.11"), ContainSubstring("member 10.0.0.12"), ContainSubstring("endpoint")))
	g.Expect(hasEtcdLeaveHook(getMachine(g, mgmt, "cp-0"))).To(BeTrue())
}

// TestReconcileMemberLeave_EndpointIsTheLastResort: when no member can be
// reached directly (on CAPK the Machines report pod IPs, which the
// apiserver's certificate does not cover), the leave still goes through the
// endpoint, as it did before.
func TestReconcileMemberLeave_EndpointIsTheLastResort(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	target := leavingMachine("cp-0", nil, "10.244.0.10")
	machines := []*clusterv1.Machine{target, nodeMachine("cp-1", false, "10.244.0.11")}
	mgmt := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target).Build()
	endpoint := fake.NewClientBuilder().WithScheme(scheme).Build()
	members := &fakeMembers{} // every member fails
	r := &KairosControlPlaneReconciler{Client: mgmt, Scheme: scheme,
		WorkloadClientFactory: staticWorkloadClient(endpoint), MemberClientFactory: members.factory}

	done, err := r.reconcileMemberLeave(context.Background(), log.Log, k0sKCP(), haCluster(), machines, target)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(members.dialed).To(Equal([]string{"10.244.0.11"}))
	g.Expect(leaveCM(g, endpoint)).To(HaveKeyWithValue("cp-0", etcdLeaveRequestedValue))
}

// TestReconcileMemberLeave_NeverDialsTheLeaverOrTheEndpointHost: even when
// every member fails, the KCP never addresses the leaving member, the
// endpoint host or the VIP directly.
func TestReconcileMemberLeave_NeverDialsTheLeaverOrTheEndpointHost(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	target := leavingMachine("cp-0", nil, "10.0.0.10", testVIP)
	machines := []*clusterv1.Machine{
		target,
		nodeMachine("cp-1", false, "10.0.0.11", testEndpointHost),
		nodeMachine("cp-2", false, "10.0.0.12"),
	}
	mgmt := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target).Build()
	leaver := fake.NewClientBuilder().WithScheme(scheme).Build()
	members := &fakeMembers{clients: map[string]client.Client{"10.0.0.10": leaver, testEndpointHost: leaver, testVIP: leaver}}
	r := &KairosControlPlaneReconciler{Client: mgmt, Scheme: scheme,
		WorkloadClientFactory: staticWorkloadClient(hungClient(scheme)), MemberClientFactory: members.factory}

	_, err := r.reconcileMemberLeave(context.Background(), log.Log, vipKCP(), haCluster(), machines, target)
	g.Expect(err).To(HaveOccurred())
	g.Expect(members.dialed).To(Equal([]string{"10.0.0.11", "10.0.0.12"}))
	assertNoLeaveCM(g, leaver)
}
