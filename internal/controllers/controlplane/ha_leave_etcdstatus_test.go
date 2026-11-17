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
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// etcdStatusCase is a state of the etcd-status Secret in which the leaving
// member cp-0 (NodeRef cp-0) has no entry under its Node name.
type etcdStatusCase struct {
	name string
	// secret is the etcd-status Secret, nil for none.
	secret *corev1.Secret
	// unreadable makes every read of the Secret fail.
	unreadable bool
}

func etcdStatusCases() []etcdStatusCase {
	return []etcdStatusCase{
		{name: "the member never reported", secret: etcdStatusSecretForMembers("cp-1", "cp-2")},
		{name: "the Secret is gone", secret: nil},
		{name: "the Secret was recreated empty", secret: etcdStatusSecretForMembers()},
		{name: "the node reported under its hostname, not lowercased", secret: etcdStatusSecretForMembers("CP-0", "cp-1", "cp-2")},
		{name: "the Secret cannot be read", secret: etcdStatusSecretForMembers("cp-1", "cp-2"), unreadable: true},
	}
}

// withEtcdStatus replaces the fixture's management client with one that holds
// the leaving Machine and tc's etcd-status Secret, and counts every read of
// that Secret.
func (f *leaveFixture) withEtcdStatus(tc etcdStatusCase) *int {
	reads := new(int)
	objs := []client.Object{f.target}
	if tc.secret != nil {
		objs = append(objs, tc.secret)
	}
	isEtcdStatus := func(key client.ObjectKey, obj client.Object) bool {
		_, ok := obj.(*corev1.Secret)
		return ok && key.Name == etcdStatusSecretName(testClusterName)
	}
	f.mgmt = fake.NewClientBuilder().WithScheme(f.scheme).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if isEtcdStatus(key, obj) {
				*reads++
				if tc.unreadable {
					return errors.New("etcdserver: request timed out")
				}
			}
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	f.r.Client = f.mgmt
	return reads
}

// TestReconcileMemberLeave_EtcdStatusAbsenceIsNotALeave is KD-94. Nothing
// ever removes a member's key from the etcd-status Secret (each node writes
// its own once, at boot), so a registered member with no key there may still
// be a voting member. In every such state the leave runs as it does for any
// member: it waits for the node's ack, hands the leave to k0s after
// memberLeaveTimeout and releases the hook only when k0s reports the member
// gone, or abandons it at the cap. It never reads the Secret.
func TestReconcileMemberLeave_EtcdStatusAbsenceIsNotALeave(t *testing.T) {
	for _, tc := range etcdStatusCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("before the timeout the hook stays and the request is kept", func(t *testing.T) {
				g := NewWithT(t)
				f := newLeaveFixture(g, map[string]string{etcdLeaveRequestedAtAnnotation: ago(time.Minute)},
					[]client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "True")}, nil)
				reads := f.withEtcdStatus(tc)

				done, err := f.reconcile()
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(done).To(BeFalse())
				g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())
				g.Expect(leaveCM(g, f.member)).To(HaveKeyWithValue("cp-0", etcdLeaveRequestedValue))
				g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeFalse())
				g.Expect(*reads).To(BeZero(), "the leave never reads the etcd-status Secret")
			})
			t.Run("past the timeout it is handed to k0s and ends on Joined=False", func(t *testing.T) {
				g := NewWithT(t)
				f := newLeaveFixture(g, map[string]string{etcdLeaveRequestedAtAnnotation: ago(2 * memberLeaveTimeout)},
					[]client.Object{k0sEtcdMember("cp-0", "10.0.0.10", false, "True")}, nil)
				reads := f.withEtcdStatus(tc)

				done, err := f.reconcile()
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(done).To(BeFalse(), "the hook stays while k0s reports the member joined")
				g.Expect(leaveRequested(getEtcdMember(g, f.member, "cp-0"))).To(BeTrue())
				g.Expect(hasEtcdLeaveHook(getMachine(g, f.mgmt, "cp-0"))).To(BeTrue())

				member := getEtcdMember(g, f.member, "cp-0")
				joinedFalse := k0sEtcdMember("cp-0", "10.0.0.10", true, "False")
				joinedFalse.SetResourceVersion(member.GetResourceVersion())
				g.Expect(f.member.Update(context.Background(), joinedFalse)).To(Succeed())
				f.refreshTarget(g)
				done, err = f.reconcile()
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(done).To(BeTrue())
				assertHookGone(g, f.mgmt, "cp-0")
				g.Expect(*reads).To(BeZero(), "the leave never reads the etcd-status Secret")
			})
			t.Run("past the cap it is abandoned with a warning", func(t *testing.T) {
				g := NewWithT(t)
				f := newLeaveFixture(g, map[string]string{
					etcdLeaveRequestedAtAnnotation:       ago(time.Hour),
					etcdMemberLeaveRequestedAtAnnotation: ago(etcdMemberLeaveCap + time.Minute),
				}, []client.Object{k0sEtcdMember("cp-0", "10.0.0.10", true, "True")}, nil)
				reads := f.withEtcdStatus(tc)

				done, err := f.reconcile()
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(done).To(BeTrue())
				assertHookGone(g, f.mgmt, "cp-0")
				g.Expect(f.events()).To(ConsistOf(ContainSubstring("Warning EtcdMemberLeaveTimedOut")))
				g.Expect(*reads).To(BeZero(), "the leave never reads the etcd-status Secret")
			})
		})
	}
}
