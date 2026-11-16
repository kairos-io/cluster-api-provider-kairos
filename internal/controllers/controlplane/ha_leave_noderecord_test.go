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

// KD-91: the node name recorded on a control-plane Machine at creation, and
// the etcd leave of a Machine without a NodeRef that looks its member up by
// it instead of by the KairosConfig.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta2 "github.com/kairos-io/cluster-api-provider-kairos/api/bootstrap/v1beta2"
)

// withRecord sets m's recorded node name.
func withRecord(m *clusterv1.Machine, name string) *clusterv1.Machine {
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	m.Annotations[renderedNodeNameAnnotation] = name
	return m
}

// recordedLeaver is the unregistered leaver cp-0 with its KairosConfig
// reference and node name recorded as record.
func recordedLeaver(record string, annotations map[string]string) *clusterv1.Machine {
	all := map[string]string{renderedNodeNameAnnotation: record}
	for k, v := range annotations {
		all[k] = v
	}
	return withKairosConfigRef(unregisteredLeaver(all, "10.0.0.10"), "cp-0")
}

// refreshTarget reloads the leaver into the fixture after a pass.
func (f *leaveFixture) refreshTarget(g *WithT) {
	f.target = getMachine(g, f.mgmt, "cp-0")
	f.machines[0] = f.target
}

// TestNodeNameRecordFor: the record is the hostname the renderer gives the
// node, and the KCP refuses to write one that cannot name a node.
func TestNodeNameRecordFor(t *testing.T) {
	long := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, hostname, machine, want string
		wantErr                       bool
	}{
		{name: "no spec.hostname: the Machine name", machine: "kcp-abcde", want: "kcp-abcde"},
		{name: "spec.hostname wins", hostname: "cp-host", machine: "kcp-abcde", want: "cp-host"},
		{name: "uppercase hostname refused, not lowercased", hostname: "CP-Host", machine: "kcp-abcde", wantErr: true},
		{name: "template markup refused", hostname: "metal-{{ trunc 4 .MachineID }}", machine: "kcp-abcde", wantErr: true},
		{name: "dotted hostname refused", hostname: "cp.example.com", machine: "kcp-abcde", wantErr: true},
		{name: "a Machine name too long for a label refused", machine: long, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			got, err := nodeNameRecordFor(&bootstrapv1beta2.KairosConfigSpec{Hostname: tc.hostname}, tc.machine)
			if tc.wantErr {
				g.Expect(err).To(MatchError(ContainSubstring("lowercase DNS-1123 label")))
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

// TestLeaveNodeName_RecordNeverReadsTheKairosConfig: with a record, the name
// does not depend on the KairosConfig at all, so a KairosConfig that cannot
// be read is no error. (Without one it is: TestLeaveNodeName_ReadErrorIsNotAGuess.)
func TestLeaveNodeName_RecordNeverReadsTheKairosConfig(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*bootstrapv1beta2.KairosConfig); ok {
				return errors.New("etcdserver: request timed out")
			}
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}
	got, err := r.leaveNodeName(context.Background(), withRecord(withKairosConfigRef(noNodeRef(nodeMachine("kcp-1", true)), "kcp-1"), "cp-host"))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got).To(Equal("cp-host"))
}

// TestCreateControlPlaneMachine_RecordsTheNodeName: every control-plane
// Machine the KCP creates carries its node name, whatever its role and
// distribution, in the same Create that stamps the etcd-leave hook.
func TestCreateControlPlaneMachine_RecordsTheNodeName(t *testing.T) {
	for _, dist := range []string{"k0s", "k3s"} {
		for _, role := range []bootstrapv1beta2.ControlPlaneRole{bootstrapv1beta2.ControlPlaneRoleSingle, bootstrapv1beta2.ControlPlaneRoleInit, bootstrapv1beta2.ControlPlaneRoleJoin} {
			t.Run(dist+"/"+string(role), func(t *testing.T) {
				g := NewWithT(t)
				scheme := machineMetaScheme(g)
				kcp := machineMetaKCP(dist, nil)
				var createdMachines []*clusterv1.Machine
				c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineMetaInfraTemplate()).
					WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						if m, ok := obj.(*clusterv1.Machine); ok {
							createdMachines = append(createdMachines, m.DeepCopy())
						}
						return c.Create(ctx, obj, opts...)
					}}).Build()
				r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

				created, err := r.createControlPlaneMachine(context.Background(), log.Log, kcp, machineMetaCluster(), guardFor(kcp), role, "")
				g.Expect(err).ToNot(HaveOccurred())

				g.Expect(createdMachines).To(HaveLen(1), "one Machine Create")
				sent := createdMachines[0]
				g.Expect(sent.Annotations).To(HaveKeyWithValue(renderedNodeNameAnnotation, created.Name),
					"with no spec.hostname the node is named after the Machine")
				if shouldStampEtcdLeaveHook(dist, role) {
					g.Expect(sent.Annotations).To(HaveKey(etcdLeaveHookAnnotation()), "the hook and the record ride in the same Create")
				} else {
					g.Expect(sent.Annotations).ToNot(HaveKey(etcdLeaveHookAnnotation()))
				}
			})
		}
	}
}

// TestCreateControlPlaneMachine_RecordsTheTemplateHostname: a hostname from
// the KairosConfigTemplate is what the node renders, so it is the record.
func TestCreateControlPlaneMachine_RecordsTheTemplateHostname(t *testing.T) {
	g := NewWithT(t)
	r, kcp, _ := hostnameTemplateKCP(g, "cp-host")

	created, err := r.createControlPlaneMachine(context.Background(), log.Log, kcp, machineMetaCluster(), guardFor(kcp), bootstrapv1beta2.ControlPlaneRoleSingle, "")
	g.Expect(err).ToNot(HaveOccurred())

	machine := &clusterv1.Machine{}
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: created.Name, Namespace: "default"}, machine)).To(Succeed())
	g.Expect(machine.Annotations).To(HaveKeyWithValue(renderedNodeNameAnnotation, "cp-host"))
	kc := &bootstrapv1beta2.KairosConfig{}
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: created.Name, Namespace: "default"}, kc)).To(Succeed())
	g.Expect(bootstrapv1beta2.RenderedHostname(&kc.Spec, created.Name)).To(Equal("cp-host"), "the record is what the KairosConfig renders")
}

// TestCreateControlPlaneMachine_TemplateMetadataCannotSetTheRecord: a
// machineTemplate.metadata annotation of the same key reaches neither the
// Machine nor its infrastructure Machine.
func TestCreateControlPlaneMachine_TemplateMetadataCannotSetTheRecord(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := machineMetaKCP("k0s", &clusterv1.ObjectMeta{
		Annotations: map[string]string{renderedNodeNameAnnotation: "evil", "example.com/owner": "platform-team"},
	})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineMetaInfraTemplate()).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	created, err := r.createControlPlaneMachine(context.Background(), log.Log, kcp, machineMetaCluster(), guardFor(kcp), bootstrapv1beta2.ControlPlaneRoleInit, "")
	g.Expect(err).ToNot(HaveOccurred())

	machine := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: created.Name, Namespace: "default"}, machine)).To(Succeed())
	g.Expect(machine.Annotations).To(HaveKeyWithValue(renderedNodeNameAnnotation, created.Name))
	g.Expect(machine.Annotations).To(HaveKeyWithValue("example.com/owner", "platform-team"), "other template annotations still apply")

	infra := &unstructured.Unstructured{}
	infra.SetGroupVersionKind(machineMetaInfraTemplate().GroupVersionKind())
	infra.SetKind("DockerMachine")
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: created.Name, Namespace: "default"}, infra)).To(Succeed())
	g.Expect(infra.GetAnnotations()).ToNot(HaveKey(renderedNodeNameAnnotation))
}

// TestReconcileMachineMetadata_NeverCopiesTheRecord: one template value on
// every Machine would overwrite each Machine's own node name.
func TestReconcileMachineMetadata_NeverCopiesTheRecord(t *testing.T) {
	g := NewWithT(t)
	scheme := machineMetaScheme(g)
	kcp := machineMetaKCP("k0s", &clusterv1.ObjectMeta{
		Annotations: map[string]string{renderedNodeNameAnnotation: "evil", "example.com/owner": "platform-team"},
	})
	recorded := machineMetaMachine("test-kcp-0", nil, map[string]string{renderedNodeNameAnnotation: "mine"})
	unrecorded := machineMetaMachine("test-kcp-1", nil, nil)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(recorded, unrecorded).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	g.Expect(r.reconcileMachineMetadata(context.Background(), log.Log, kcp, machineMetaCluster(),
		[]*clusterv1.Machine{recorded.DeepCopy(), unrecorded.DeepCopy()})).To(Succeed())

	got0 := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "test-kcp-0", Namespace: "default"}, got0)).To(Succeed())
	g.Expect(got0.Annotations).To(HaveKeyWithValue(renderedNodeNameAnnotation, "mine"))
	g.Expect(got0.Annotations).To(HaveKeyWithValue("example.com/owner", "platform-team"))
	got1 := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "test-kcp-1", Namespace: "default"}, got1)).To(Succeed())
	g.Expect(got1.Annotations).ToNot(HaveKey(renderedNodeNameAnnotation))
	g.Expect(got1.Annotations).To(HaveKeyWithValue("example.com/owner", "platform-team"))
}

// TestReconcileRenderedNodeNames: Machines created before the record get
// one, from what their node goes by; nothing else is touched.
func TestReconcileRenderedNodeNames(t *testing.T) {
	registered := func(name, node string) *clusterv1.Machine {
		m := withKairosConfigRef(nodeMachine(name, false), name)
		m.Status.NodeRef.Name = node
		return m
	}
	unregistered := func(name string) *clusterv1.Machine {
		return withKairosConfigRef(noNodeRef(nodeMachine(name, false)), name)
	}
	for _, tc := range []struct {
		name    string
		machine *clusterv1.Machine
		config  *bootstrapv1beta2.KairosConfig
		want    string // "" = no record
	}{
		{name: "unregistered, no spec.hostname: the Machine name", machine: unregistered("m-a"), config: kairosConfigWithHostname("m-a", ""), want: "m-a"},
		{name: "unregistered, spec.hostname", machine: unregistered("m-b"), config: kairosConfigWithHostname("m-b", "cp-x"), want: "cp-x"},
		{name: "registered: the Node name, whatever the KairosConfig says", machine: registered("m-c", "node-c"), config: kairosConfigWithHostname("m-c", "other"), want: "node-c"},
		{name: "registered under a dotted Node name: none", machine: registered("m-d", "node.example.com"), config: kairosConfigWithHostname("m-d", "")},
		{name: "uppercase hostname: none, never lowercased", machine: unregistered("m-e"), config: kairosConfigWithHostname("m-e", "CP-Host")},
		{name: "template markup: none", machine: unregistered("m-f"), config: kairosConfigWithHostname("m-f", "metal-{{ trunc 4 .MachineID }}")},
		{name: "the KairosConfig is gone: none", machine: unregistered("m-g")},
		{name: "being deleted: never", machine: withKairosConfigRef(noNodeRef(nodeMachine("m-h", true)), "m-h"), config: kairosConfigWithHostname("m-h", "")},
		{name: "an existing record is never overwritten", machine: withRecord(registered("m-i", "node-i"), "kept"), config: kairosConfigWithHostname("m-i", "cp-y"), want: "kept"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := haTestScheme(g)
			objs := []client.Object{tc.machine}
			if tc.config != nil {
				objs = append(objs, tc.config)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

			r.reconcileRenderedNodeNames(context.Background(), log.Log, []*clusterv1.Machine{tc.machine.DeepCopy()})

			got := &clusterv1.Machine{}
			g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(tc.machine), got)).To(Succeed())
			if tc.want == "" {
				g.Expect(got.Annotations).ToNot(HaveKey(renderedNodeNameAnnotation))
				return
			}
			g.Expect(got.Annotations).To(HaveKeyWithValue(renderedNodeNameAnnotation, tc.want))
		})
	}
}

// TestReconcileRenderedNodeNames_ReadErrorIsSkipped: a KairosConfig that
// cannot be read gives no record and no failure; the next Machine is still
// recorded.
func TestReconcileRenderedNodeNames_ReadErrorIsSkipped(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	broken := withKairosConfigRef(noNodeRef(nodeMachine("m-a", false)), "m-a")
	fine := withKairosConfigRef(noNodeRef(nodeMachine("m-b", false)), "m-b")
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(broken, fine, kairosConfigWithHostname("m-a", ""), kairosConfigWithHostname("m-b", "")).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*bootstrapv1beta2.KairosConfig); ok && key.Name == "m-a" {
				return errors.New("etcdserver: request timed out")
			}
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	r.reconcileRenderedNodeNames(context.Background(), log.Log, []*clusterv1.Machine{broken.DeepCopy(), fine.DeepCopy()})

	got := &clusterv1.Machine{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(broken), got)).To(Succeed())
	g.Expect(got.Annotations).ToNot(HaveKey(renderedNodeNameAnnotation))
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(fine), got)).To(Succeed())
	g.Expect(got.Annotations).To(HaveKeyWithValue(renderedNodeNameAnnotation, "m-b"))
}

// TestUnregisteredLeave_RecordSurvivesKairosConfigDeletion is KD-91: the
// leaver's KairosConfig, which set spec.hostname, is gone by pre-terminate
// (a foreground delete removes it first). The member is still found under
// the recorded name, the leave is requested and the member pinned. The old
// lookup fell back to the Machine name, found no EtcdMember, and released
// the hook with no leave.
func TestUnregisteredLeave_RecordSurvivesKairosConfigDeletion(t *testing.T) {
	g := NewWithT(t)
	f := newLeaveFixtureFor(g, recordedLeaver("cp-host", nil), []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)

	done, err := f.reconcile()

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(leaveCM(g, f.member)).To(HaveKeyWithValue("cp-host", etcdLeaveRequestedValue))
	got := getMachine(g, f.mgmt, "cp-0")
	g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
	g.Expect(got.Annotations).To(HaveKey(etcdLeaveRequestedAtAnnotation))
	g.Expect(got.Annotations).To(HaveKeyWithValue(etcdMemberLeaveMemberIDAnnotation, testMemberID))
	g.Expect(f.events()).To(BeEmpty(), "a KairosConfig that is gone is no contradiction")
}

// TestUnregisteredLeave_LegacyMachineKeepsTheOldLookup: a Machine without a
// record (deleted before it could be backfilled) is looked up as before: the
// KairosConfig's hostname, else the Machine name.
func TestUnregisteredLeave_LegacyMachineKeepsTheOldLookup(t *testing.T) {
	g := NewWithT(t)
	target := withKairosConfigRef(unregisteredLeaver(nil, "10.0.0.10"), "cp-0")
	f := newLeaveFixtureFor(g, target, []client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "True")}, nil)

	done, err := f.reconcile()

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(getMachine(g, f.mgmt, "cp-0").Annotations).To(And(HaveKey(etcdLeaveRequestedAtAnnotation), HaveKeyWithValue(etcdMemberLeaveMemberIDAnnotation, testMemberID)))
}

// TestUnregisteredLeave_RecordBeatsAnEditedOrRecreatedHostname: a
// KairosConfig that now renders another name changes nothing about which
// member leaves; it gets one Warning event per eviction cycle.
func TestUnregisteredLeave_RecordBeatsAnEditedOrRecreatedHostname(t *testing.T) {
	g := NewWithT(t)
	f := newLeaveFixtureFor(g, recordedLeaver("cp-host", nil), []client.Object{
		k0sEtcdMember("cp-host", "10.0.0.10", false, "True"),
		k0sEtcdMember("elsewhere", "10.0.0.99", false, "True"),
	}, nil)
	g.Expect(f.mgmt.Create(context.Background(), kairosConfigWithHostname("cp-0", "elsewhere"))).To(Succeed())

	done, err := f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(leaveCM(g, f.member)).To(HaveKeyWithValue("cp-host", etcdLeaveRequestedValue))
	g.Expect(leaveCM(g, f.member)).ToNot(HaveKey("elsewhere"))
	g.Expect(f.events()).To(ConsistOf(And(
		ContainSubstring("Warning "+etcdLeaveNodeNameContradictedEventReason),
		ContainSubstring(`records node name "cp-host"`), ContainSubstring(`now renders "elsewhere"`))))

	// The next pass waits for the ack and does not warn again.
	f.refreshTarget(g)
	done, err = f.reconcile()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(done).To(BeFalse())
	g.Expect(f.events()).To(BeEmpty(), "once per eviction cycle")
	g.Expect(leaveRequested(getEtcdMember(g, f.member, "elsewhere"))).To(BeFalse())
}

// TestUnregisteredLeave_UnmatchableRecordIsNoEvidence: a record the KCP
// would never write (so changed by hand) is held like an unmatchable legacy
// hostname: nothing read or written, released with a warning past the bound.
// The KairosConfig is valid, so it is the record that is checked.
func TestUnregisteredLeave_UnmatchableRecordIsNoEvidence(t *testing.T) {
	for _, record := range []string{"CP-Host", "cp.example.com", "", "metal-{{ trunc 4 .MachineID }}"} {
		t.Run("'"+record+"': kept within the bound", func(t *testing.T) {
			g := NewWithT(t)
			f := newLeaveFixtureFor(g, recordedLeaver(record, nil), nil, nil)
			g.Expect(f.mgmt.Create(context.Background(), kairosConfigWithHostname("cp-0", ""))).To(Succeed())

			done, err := f.reconcile()

			g.Expect(err).To(MatchError(ContainSubstring("recorded node name of Machine cp-0 is not a lowercase DNS-1123 label")))
			g.Expect(done).To(BeFalse())
			got := getMachine(g, f.mgmt, "cp-0")
			g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
			g.Expect(got.Annotations).To(HaveKey(etcdLeaveUnreachableSinceAnnotation))
			g.Expect(got.Annotations).ToNot(HaveKey(etcdLeaveRequestedAtAnnotation))
			g.Expect(leaveCM(g, f.member)).To(Equal(map[string]string{"cp-0": etcdLeaveRequestedValue}), "nothing written")
		})
		t.Run("'"+record+"': released with a warning past the bound", func(t *testing.T) {
			g := NewWithT(t)
			f := newLeaveFixtureFor(g, recordedLeaver(record, map[string]string{etcdLeaveUnreachableSinceAnnotation: ago(leaveUnreachableBound + time.Minute)}), nil, nil)
			g.Expect(f.mgmt.Create(context.Background(), kairosConfigWithHostname("cp-0", ""))).To(Succeed())

			done, err := f.reconcile()

			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(done).To(BeTrue())
			assertHookGone(g, f.mgmt, "cp-0")
			g.Expect(f.events()).To(ConsistOf(And(ContainSubstring("Warning EtcdMemberLeaveTimedOut"), ContainSubstring("recorded node name is not a lowercase DNS-1123 label"))))
		})
	}
}

// TestUnregisteredLeave_CollisionUsesRecords: another Machine collides with
// the leaver by any name its node may go by: its NodeRef when registered;
// otherwise its record and the hostname its KairosConfig renders, since an
// edit made before its node was bootstrapped gave the node that name.
func TestUnregisteredLeave_CollisionUsesRecords(t *testing.T) {
	t.Run("another unregistered Machine's KairosConfig renders the leaver's name: abandoned", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, recordedLeaver("cp-host", nil), []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
		f.machines[2] = withRecord(withKairosConfigRef(noNodeRef(nodeMachine("cp-2", false, "10.0.0.12")), "cp-2"), "cp-2")
		g.Expect(f.mgmt.Create(context.Background(), kairosConfigWithHostname("cp-2", "cp-host"))).To(Succeed())

		done, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		assertHookGone(g, f.mgmt, "cp-0")
		g.Expect(leaveCM(g, f.member)).ToNot(HaveKey("cp-host"), "nothing is sent to a node that may be cp-2's")
		g.Expect(f.events()).To(ConsistOf(ContainSubstring(`Machine "cp-2" has the same node name`)))
	})
	t.Run("a registered Machine's KairosConfig renders the leaver's name: no collision", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, recordedLeaver("cp-host", nil), []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
		f.machines[2] = withRecord(withKairosConfigRef(nodeMachine("cp-2", false, "10.0.0.12"), "cp-2"), "cp-2")
		g.Expect(f.mgmt.Create(context.Background(), kairosConfigWithHostname("cp-2", "cp-host"))).To(Succeed())

		done, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeFalse())
		g.Expect(leaveCM(g, f.member)).To(HaveKeyWithValue("cp-host", etcdLeaveRequestedValue), "a registered node goes by its Node name")
		g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
		g.Expect(f.events()).To(BeEmpty())
	})
	t.Run("another unregistered Machine's KairosConfig cannot be read: fails closed", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, recordedLeaver("cp-host", nil), []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
		f.machines[2] = withRecord(withKairosConfigRef(noNodeRef(nodeMachine("cp-2", false, "10.0.0.12")), "cp-2"), "cp-2")
		f.r.Client = interceptor.NewClient(f.mgmt.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*bootstrapv1beta2.KairosConfig); ok && key.Name == "cp-2" {
				return errors.New("etcdserver: request timed out")
			}
			return c.Get(ctx, key, obj, opts...)
		}})

		done, err := f.reconcile()

		g.Expect(err).To(HaveOccurred())
		g.Expect(done).To(BeFalse())
		g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
		g.Expect(leaveCM(g, f.member)).ToNot(HaveKey("cp-host"))
	})
	t.Run("another Machine records the same name: abandoned", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, recordedLeaver("cp-host", nil), []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
		f.machines[2] = withRecord(noNodeRef(nodeMachine("cp-2", false, "10.0.0.12")), "cp-host")

		done, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		assertHookGone(g, f.mgmt, "cp-0")
		g.Expect(leaveCM(g, f.member)).ToNot(HaveKey("cp-host"))
		g.Expect(f.events()).To(ConsistOf(ContainSubstring(`Machine "cp-2" has the same node name`)))
	})
	t.Run("a registered Machine's Node has the leaver's name: abandoned", func(t *testing.T) {
		g := NewWithT(t)
		f := newLeaveFixtureFor(g, recordedLeaver("cp-host", nil), []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
		other := withRecord(nodeMachine("cp-2", false, "10.0.0.12"), "cp-2")
		other.Status.NodeRef.Name = "cp-host"
		f.machines[2] = other

		done, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue())
		assertHookGone(g, f.mgmt, "cp-0")
		g.Expect(f.events()).To(ConsistOf(ContainSubstring(`Machine "cp-2" has the same node name`)))
	})
}

// TestReconcileMemberLeave_NodeNameStableAcrossPasses: once a leave is
// requested, deleting or editing the KairosConfig does not move the ack key.
// The old lookup recomputed the name every pass, so the node's ack under its
// real name was never seen and the leave ended in a timeout.
func TestReconcileMemberLeave_NodeNameStableAcrossPasses(t *testing.T) {
	for _, change := range []string{"deleted", "edited"} {
		t.Run("KairosConfig "+change+" mid-leave", func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()
			f := newLeaveFixtureFor(g, recordedLeaver("cp-host", nil), []client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
			kc := kairosConfigWithHostname("cp-0", "cp-host")
			g.Expect(f.mgmt.Create(ctx, kc)).To(Succeed())

			done, err := f.reconcile()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(done).To(BeFalse())
			g.Expect(leaveCM(g, f.member)).To(HaveKeyWithValue("cp-host", etcdLeaveRequestedValue))

			if change == "deleted" {
				g.Expect(f.mgmt.Delete(ctx, kc)).To(Succeed())
			} else {
				g.Expect(f.mgmt.Get(ctx, client.ObjectKeyFromObject(kc), kc)).To(Succeed())
				kc.Spec.Hostname = "elsewhere"
				g.Expect(f.mgmt.Update(ctx, kc)).To(Succeed())
			}
			// The node acks under the name it goes by.
			cm := leaveConfigMap(map[string]string{"cp-host": etcdLeftValue})
			existing := leaveConfigMap(nil)
			g.Expect(f.member.Get(ctx, client.ObjectKeyFromObject(existing), existing)).To(Succeed())
			existing.Data = cm.Data
			g.Expect(f.member.Update(ctx, existing)).To(Succeed())

			f.refreshTarget(g)
			done, err = f.reconcile()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(done).To(BeTrue(), "the ack under the recorded name ends the leave")
			assertHookGone(g, f.mgmt, "cp-0")
		})
	}
}

// TestExpectedEtcdMembers_EveryNameAMachineMayGoByIsKept: the orphan check
// keeps a member named by any of a Machine's names: its record, the hostname
// its KairosConfig renders, or its Node name.
func TestExpectedEtcdMembers_EveryNameAMachineMayGoByIsKept(t *testing.T) {
	g := NewWithT(t)
	scheme := haTestScheme(g)
	recordOnly := withRecord(withKairosConfigRef(noNodeRef(nodeMachine("m-a", false)), "m-a"), "recorded-a")
	configDiffers := withRecord(withKairosConfigRef(nodeMachine("m-b", false), "m-b"), "recorded-b")
	configDiffers.Status.NodeRef.Name = "node-b"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kairosConfigWithHostname("m-b", "config-b")).Build()
	r := &KairosControlPlaneReconciler{Client: c, Scheme: scheme}

	e, err := r.expectedEtcdMembers(context.Background(), []*clusterv1.Machine{recordOnly, configDiffers})
	g.Expect(err).ToNot(HaveOccurred())

	member := func(name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetName(name)
		return u
	}
	for _, kept := range []string{"recorded-a", "m-a", "recorded-b", "config-b", "node-b"} {
		g.Expect(e.has(member(kept))).To(BeTrue(), kept)
	}
	g.Expect(e.has(member("stranger"))).To(BeFalse())
}

// TestReconcileMemberLeave_NameIsRecheckedAfterTheRequest: the name of a
// Machine without a NodeRef is checked again on every pass after the first
// request, before anything is read or written under it. A record changed to
// another Machine's node name, or to a name that cannot be matched, must not
// aim the sentinel or k0s's spec.leave at another member.
func TestReconcileMemberLeave_NameIsRecheckedAfterTheRequest(t *testing.T) {
	for _, tc := range []struct {
		name, record, why string
	}{
		{name: "changed to a registered Machine's Node name", record: "cp-1", why: `Machine "cp-1" has the same node name`},
		{name: "changed to a name that cannot be matched", record: "CP-1", why: "recorded node name is not a lowercase DNS-1123 label"},
	} {
		t.Run(tc.name+": held, nothing written", func(t *testing.T) {
			g := NewWithT(t)
			f := newLeaveFixtureFor(g, recordedLeaver(tc.record, map[string]string{etcdLeaveRequestedAtAnnotation: ago(time.Minute)}),
				[]client.Object{k0sEtcdMember("cp-1", "10.0.0.11", false, "True")}, nil)

			done, err := f.reconcile()

			g.Expect(err).To(MatchError(ContainSubstring(tc.why)))
			g.Expect(done).To(BeFalse())
			g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
			g.Expect(leaveCM(g, f.member)).To(Equal(map[string]string{"cp-0": etcdLeaveRequestedValue}), "no sentinel under the other name")
		})
		t.Run(tc.name+": past the ack timeout, still held and never delegated", func(t *testing.T) {
			g := NewWithT(t)
			// No member-ID pin and a peer address no Machine reports (a CAPK
			// bridge NIC): the delegation's own checks would not stop it.
			f := newLeaveFixtureFor(g, recordedLeaver(tc.record, map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)}),
				[]client.Object{k0sEtcdMember("cp-1", "192.168.50.11", false, "True")}, nil)

			done, err := f.reconcile()

			g.Expect(err).To(MatchError(ContainSubstring(tc.why)))
			g.Expect(done).To(BeFalse())
			got := getMachine(g, f.mgmt, "cp-0")
			g.Expect(hasEtcdLeaveHook(got)).To(BeTrue())
			g.Expect(got.Annotations).To(HaveKey(etcdMemberLeaveRequestedAtAnnotation), "the second phase of the bound starts")
			g.Expect(got.Annotations).ToNot(HaveKey(etcdMemberLeaveMemberIDAnnotation), "the other member's ID is never pinned")
			g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-1"))).To(BeFalse(), "spec.leave never set on the other member")
			g.Expect(f.events()).To(ConsistOf(And(ContainSubstring("Warning "+etcdLeaveNodeNameUnusableEventReason), ContainSubstring(tc.why))))
		})
		t.Run(tc.name+": past the whole bound, released with a warning and never delegated", func(t *testing.T) {
			g := NewWithT(t)
			f := newLeaveFixtureFor(g, recordedLeaver(tc.record, map[string]string{
				etcdLeaveRequestedAtAnnotation:       ago(memberLeaveTimeout + etcdMemberLeaveCap + 2*time.Minute),
				etcdMemberLeaveRequestedAtAnnotation: ago(etcdMemberLeaveCap + time.Minute),
			}), []client.Object{k0sEtcdMember("cp-1", "192.168.50.11", false, "True")}, nil)

			done, err := f.reconcile()

			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(done).To(BeTrue())
			assertHookGone(g, f.mgmt, "cp-0")
			g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-1"))).To(BeFalse(), "spec.leave never set on the other member")
			g.Expect(f.events()).To(ConsistOf(And(ContainSubstring("Warning EtcdMemberLeaveTimedOut"), ContainSubstring(tc.why))))
		})
	}
	t.Run("a change first seen after the delegation began keeps its stamp", func(t *testing.T) {
		g := NewWithT(t)
		stamp := ago(3 * time.Minute)
		f := newLeaveFixtureFor(g, recordedLeaver("cp-1", map[string]string{
			etcdLeaveRequestedAtAnnotation:       ago(memberLeaveTimeout + 3*time.Minute),
			etcdMemberLeaveRequestedAtAnnotation: stamp,
		}), []client.Object{k0sEtcdMember("cp-1", "192.168.50.11", false, "True")}, nil)

		done, err := f.reconcile()

		g.Expect(err).To(HaveOccurred())
		g.Expect(done).To(BeFalse(), "held for the rest of the cap, not abandoned at once")
		g.Expect(getMachine(g, f.mgmt, "cp-0").Annotations).To(HaveKeyWithValue(etcdMemberLeaveRequestedAtAnnotation, stamp))
		g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-1"))).To(BeFalse())
	})
	t.Run("a Machine that registers after the request is left under its Node name, without the re-check", func(t *testing.T) {
		g := NewWithT(t)
		// CAPI keeps reconciling the Node of a deleting Machine, so the
		// NodeRef can appear mid-leave. The name then is the Node name; the
		// record, here one that would not pass, is no longer used.
		target := withRecord(withKairosConfigRef(leavingMachine("cp-0", map[string]string{etcdLeaveRequestedAtAnnotation: ago(time.Minute)}, "10.0.0.10"), "cp-0"), "CP-HOST")
		f := newLeaveFixtureFor(g, target, []client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "True")}, nil)

		done, err := f.reconcile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeFalse())
		g.Expect(leaveCM(g, f.member)).To(Equal(map[string]string{"cp-0": etcdLeaveRequestedValue}), "the sentinel is kept under the Node name")
		g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
		g.Expect(f.events()).To(BeEmpty())
	})
	t.Run("a reverted record lets the leave finish", func(t *testing.T) {
		g := NewWithT(t)
		ctx := context.Background()
		f := newLeaveFixtureFor(g, recordedLeaver("cp-1", map[string]string{etcdLeaveRequestedAtAnnotation: ago(time.Minute)}),
			[]client.Object{k0sEtcdMember("cp-host", "10.0.0.10", false, "True")}, nil)
		existing := leaveConfigMap(nil)
		g.Expect(f.member.Get(ctx, client.ObjectKeyFromObject(existing), existing)).To(Succeed())
		existing.Data = map[string]string{"cp-host": etcdLeftValue}
		g.Expect(f.member.Update(ctx, existing)).To(Succeed())

		_, err := f.reconcile()
		g.Expect(err).To(HaveOccurred(), "held while the record names cp-1")
		g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())

		m := getMachine(g, f.mgmt, "cp-0")
		m.Annotations[renderedNodeNameAnnotation] = "cp-host"
		g.Expect(f.mgmt.Update(ctx, m)).To(Succeed())
		f.refreshTarget(g)
		done, err := f.reconcile()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(done).To(BeTrue(), "the ack under the restored name ends the leave")
		assertHookGone(g, f.mgmt, "cp-0")
	})
}
